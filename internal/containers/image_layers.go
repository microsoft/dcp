/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

const (
	defaultApplyImageLayersTimeout = 10 * time.Minute
	maxImageIDFileSize             = 1024
)

// ImageLayer represents a tar file to be applied as an additional image layer when running the
// container. The layer is provided either as a path to a tar file (with a SHA256 hash for
// verification) or as base64-encoded tar contents.
type ImageLayer struct {
	// An opaque identifier for this layer, used to track whether a layer has meaningfully changed
	// independently of the raw binary content (which may vary due to timestamps or other
	// materially unimportant differences in the tar file).
	Digest string `json:"digest"`

	// Path to a tar file on the host filesystem. Mutually exclusive with RawContents.
	Source string `json:"source,omitempty"`

	// SHA256 hash of the tar file referenced by Source. Required when Source is set.
	SHA256 string `json:"sha256,omitempty"`

	// Base64-encoded tar file contents. Mutually exclusive with Source.
	RawContents string `json:"rawContents,omitempty"`
}

// ApplyImageLayersImpl builds a derived container image by applying additional tar layers
// on top of a base image. It streams a build context tar (containing a generated Dockerfile
// and the layer tars) to `docker build` via stdin, avoiding any temporary files on disk.
// Source-file layers are streamed directly from disk into the tar without buffering
// their full contents in memory.
func ApplyImageLayersImpl(
	ctx context.Context,
	log logr.Logger,
	options ApplyImageLayersOptions,
	runner CLICommandRunner,
) (string, error) {
	dockerfile, prepareErr := prepareImageLayerDockerfile(options)
	if prepareErr != nil {
		return "", prepareErr
	}

	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultApplyImageLayersTimeout
	}

	// Source layers are hash-verified here so the second pass can stream them
	// directly into the tar without buffering their full contents.
	for i := range options.Layers {
		layer := &options.Layers[i]

		if layer.Source != "" {
			if verifyErr := verifyLayerSourceHash(layer); verifyErr != nil {
				return "", fmt.Errorf("verifying image layer %d: %w", i, verifyErr)
			}
			log.V(1).Info("Layer source SHA256 verified", "Source", layer.Source, "Digest", layer.Digest)
		}
	}

	// Second pass: stream the tar archive directly to docker build via io.Pipe.
	// This avoids buffering the full build context in memory.
	pr, pw := io.Pipe()
	now := time.Now()

	// Write the tar in a goroutine while docker reads from the pipe.
	// If tar generation fails, CloseWithError unblocks the build command promptly.
	var tarErr error
	tarDone := make(chan struct{})
	go func() {
		defer close(tarDone)

		tw := usvc_io.NewTarWriterTo(pw)

		if writeErr := tw.WriteFile([]byte(dockerfile), "Dockerfile", 0, 0, 0644, now, now, now); writeErr != nil {
			tarErr = fmt.Errorf("writing Dockerfile to build context tar: %w", writeErr)
			pw.CloseWithError(tarErr)
			return
		}

		for i := range options.Layers {
			layer := &options.Layers[i]
			layerFileName := imageLayerFileName(i)

			if layer.Source != "" {
				if streamErr := streamLayerFromSource(tw, layer, layerFileName, now); streamErr != nil {
					tarErr = fmt.Errorf("streaming image layer %d to build context: %w", i, streamErr)
					pw.CloseWithError(tarErr)
					return
				}
			} else {
				decoded, decodeErr := base64.StdEncoding.DecodeString(layer.RawContents)
				if decodeErr != nil {
					tarErr = fmt.Errorf("decoding base64 rawContents for layer %d (%q): %w", i, layer.Digest, decodeErr)
					pw.CloseWithError(tarErr)
					return
				}
				if writeErr := tw.WriteFile(decoded, layerFileName, 0, 0, 0644, now, now, now); writeErr != nil {
					tarErr = fmt.Errorf("writing layer %d to build context tar: %w", i, writeErr)
					pw.CloseWithError(tarErr)
					return
				}
			}
		}

		if closeErr := tw.Close(); closeErr != nil {
			tarErr = fmt.Errorf("finalizing build context tar: %w", closeErr)
			pw.CloseWithError(tarErr)
			return
		}

		pw.Close()
	}()

	// Build the derived image, streaming the tar context via stdin.
	// The trailing "-" tells docker/podman build to read the build context from stdin.
	// --quiet suppresses build output and prints only the image ID on success.
	args := []string{"build", "--quiet"}
	if options.Tag != "" {
		args = append(args, "-t", options.Tag)
	}
	for _, label := range options.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", label.Key, label.Value))
	}
	args = append(args, "-")

	cmd := runner.MakeCommand(args...)
	cmd.Stdin = pr

	outBuf, errBuf, buildErr := runner.RunBufferedCommand(ctx, "ApplyImageLayers", cmd, nil, nil, timeout)

	// Close the read end of the pipe to unblock the tar writer goroutine if the build
	// command failed before consuming all input (e.g., binary not found, early exit).
	pr.Close()

	// Wait for the tar writer goroutine to finish
	<-tarDone

	// Prefer the build error (with actionable stderr) over tar errors, since a build
	// failure that closes stdin will cause a broken pipe in the tar writer goroutine.
	if buildErr != nil {
		errDetail := ""
		if errBuf != nil {
			errDetail = errBuf.String()
		}
		return "", fmt.Errorf("building derived image with image layers: %w: %s", buildErr, errDetail)
	}
	if tarErr != nil {
		return "", tarErr
	}

	// Return the tag if provided, otherwise the image ID from build output
	imageRef := options.Tag
	if imageRef == "" && outBuf != nil {
		imageRef = strings.TrimSpace(outBuf.String())
	}

	log.V(1).Info("Built derived image with image layers", "ImageRef", imageRef, "LayerCount", len(options.Layers))

	return imageRef, nil
}

// ApplyImageLayersFromDirectory builds a derived image from a disk-backed build context.
func ApplyImageLayersFromDirectory(
	ctx context.Context,
	log logr.Logger,
	options ApplyImageLayersOptions,
	builder BuildImage,
) (string, error) {
	return applyImageLayersFromDirectory(ctx, log, options, builder, usvc_io.DcpTempDir())
}

func applyImageLayersFromDirectory(
	ctx context.Context,
	log logr.Logger,
	options ApplyImageLayersOptions,
	builder BuildImage,
	tempDirectory string,
) (imageRef string, returnErr error) {
	if cancellationErr := ctx.Err(); cancellationErr != nil {
		return "", cancellationErr
	}

	dockerfile, prepareErr := prepareImageLayerDockerfile(options)
	if prepareErr != nil {
		return "", prepareErr
	}

	workspace, workspaceErr := createImageLayerWorkspace(tempDirectory)
	if workspaceErr != nil {
		return "", workspaceErr
	}
	defer func() {
		if cleanupErr := os.RemoveAll(workspace); cleanupErr != nil {
			imageRef = ""
			returnErr = errors.Join(returnErr, fmt.Errorf("removing image layer build workspace %q: %w", workspace, cleanupErr))
		}
	}()

	contextDirectory := filepath.Join(workspace, "context")
	if contextErr := usvc_io.EnsureRestrictedDirectory(contextDirectory, osutil.PermissionOnlyOwnerReadWriteTraverse); contextErr != nil {
		return "", fmt.Errorf("creating image layer build context: %w", contextErr)
	}

	dockerfilePath := filepath.Join(contextDirectory, "Dockerfile")
	if dockerfileErr := writeImageLayerBuildFile(ctx, dockerfilePath, strings.NewReader(dockerfile), nil); dockerfileErr != nil {
		return "", fmt.Errorf("writing Dockerfile to image layer build context: %w", dockerfileErr)
	}

	for layerIndex := range options.Layers {
		if cancellationErr := ctx.Err(); cancellationErr != nil {
			return "", cancellationErr
		}

		layer := &options.Layers[layerIndex]
		layerPath := filepath.Join(contextDirectory, imageLayerFileName(layerIndex))
		if layer.Source != "" {
			if stageErr := stageImageLayerSource(ctx, layerPath, layer); stageErr != nil {
				return "", fmt.Errorf("staging image layer %d: %w", layerIndex, stageErr)
			}
			log.V(1).Info("Layer source SHA256 verified", "Source", layer.Source, "Digest", layer.Digest)
		} else {
			decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(layer.RawContents))
			if stageErr := writeImageLayerBuildFile(ctx, layerPath, decoder, nil); stageErr != nil {
				return "", fmt.Errorf("staging base64 rawContents for layer %d (%q): %w", layerIndex, layer.Digest, stageErr)
			}
		}
	}

	if cancellationErr := ctx.Err(); cancellationErr != nil {
		return "", cancellationErr
	}

	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultApplyImageLayersTimeout
	}

	tags := []string(nil)
	iidFilePath := ""
	if options.Tag != "" {
		tags = []string{options.Tag}
	} else {
		iidFilePath = filepath.Join(workspace, "image.iid")
		iidFile, createIidErr := usvc_io.CreateNewFile(iidFilePath, osutil.PermissionOnlyOwnerReadWrite)
		if createIidErr != nil {
			return "", fmt.Errorf("creating image ID file: %w", createIidErr)
		}
		if closeIidErr := iidFile.Close(); closeIidErr != nil {
			return "", fmt.Errorf("closing image ID file before build: %w", closeIidErr)
		}
	}

	if cancellationErr := ctx.Err(); cancellationErr != nil {
		return "", cancellationErr
	}

	buildErr := builder.BuildImage(ctx, BuildImageOptions{
		IidFile: iidFilePath,
		ContainerBuildContext: &ContainerBuildContext{
			Context:    contextDirectory,
			Dockerfile: dockerfilePath,
			Tags:       tags,
			Labels:     options.Labels,
		},
		TimeoutOption: TimeoutOption{Timeout: timeout},
	})
	if buildErr != nil {
		return "", fmt.Errorf("building derived image with image layers: %w", buildErr)
	}
	if cancellationErr := ctx.Err(); cancellationErr != nil {
		return "", fmt.Errorf("building derived image with image layers: %w", cancellationErr)
	}

	imageRef = options.Tag
	if imageRef == "" {
		builtImageID, readIidErr := ReadImageIDFile(iidFilePath)
		if readIidErr != nil {
			return "", fmt.Errorf("reading derived image ID: %w", readIidErr)
		}
		imageRef = builtImageID
	}

	log.V(1).Info("Built derived image with image layers", "ImageRef", imageRef, "LayerCount", len(options.Layers))
	return imageRef, nil
}

func prepareImageLayerDockerfile(options ApplyImageLayersOptions) (string, error) {
	if len(options.Layers) == 0 {
		return "", fmt.Errorf("at least one image layer must be specified")
	}

	baseImage := options.BaseImage.Id
	if len(options.BaseImage.Tags) > 0 {
		baseImage = options.BaseImage.Tags[0]
	}

	var dockerfile strings.Builder
	dockerfile.WriteString("FROM ")
	dockerfile.WriteString(baseImage)
	dockerfile.WriteByte('\n')
	for layerIndex := range options.Layers {
		dockerfile.WriteString("ADD ")
		dockerfile.WriteString(imageLayerFileName(layerIndex))
		dockerfile.WriteString(" /\n")
	}
	return dockerfile.String(), nil
}

func imageLayerFileName(layerIndex int) string {
	return fmt.Sprintf("layer%d.tar", layerIndex)
}

func createImageLayerWorkspace(tempDirectory string) (string, error) {
	workspace, createErr := os.MkdirTemp(tempDirectory, "dcp-image-layers-")
	if createErr != nil {
		return "", fmt.Errorf("creating image layer build workspace: %w", createErr)
	}

	if restrictErr := usvc_io.EnsureRestrictedDirectory(workspace, osutil.PermissionOnlyOwnerReadWriteTraverse); restrictErr != nil {
		cleanupErr := os.RemoveAll(workspace)
		return "", errors.Join(
			fmt.Errorf("restricting image layer build workspace %q: %w", workspace, restrictErr),
			wrapImageLayerCleanupError(workspace, cleanupErr),
		)
	}
	return workspace, nil
}

func wrapImageLayerCleanupError(workspace string, cleanupErr error) error {
	if cleanupErr == nil {
		return nil
	}
	return fmt.Errorf("removing image layer build workspace %q: %w", workspace, cleanupErr)
}

func writeImageLayerBuildFile(ctx context.Context, name string, source io.Reader, observer io.Writer) error {
	file, createErr := usvc_io.CreateNewFile(name, osutil.PermissionOnlyOwnerReadWrite)
	if createErr != nil {
		return fmt.Errorf("creating build context file %q: %w", name, createErr)
	}

	destination := io.Writer(file)
	if observer != nil {
		destination = io.MultiWriter(file, observer)
	}

	_, copyErr := copyImageLayerContents(ctx, destination, source)
	closeErr := file.Close()
	if copyErr != nil {
		return errors.Join(
			fmt.Errorf("writing build context file %q: %w", name, copyErr),
			wrapImageLayerFileCloseError(name, closeErr),
		)
	}
	if closeErr != nil {
		return fmt.Errorf("closing build context file %q: %w", name, closeErr)
	}
	return nil
}

func wrapImageLayerFileCloseError(name string, closeErr error) error {
	if closeErr == nil {
		return nil
	}
	return fmt.Errorf("closing build context file %q: %w", name, closeErr)
}

func copyImageLayerContents(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var total int64

	for {
		if cancellationErr := ctx.Err(); cancellationErr != nil {
			return total, cancellationErr
		}

		readCount, readErr := source.Read(buffer)
		if readCount > 0 {
			writeCount, writeErr := destination.Write(buffer[:readCount])
			total += int64(writeCount)
			if writeErr != nil {
				return total, writeErr
			}
			if writeCount != readCount {
				return total, io.ErrShortWrite
			}
		}

		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func stageImageLayerSource(ctx context.Context, destination string, layer *ImageLayer) error {
	sourceFile, openErr := usvc_io.OpenFileReadOnly(layer.Source)
	if openErr != nil {
		return fmt.Errorf("opening layer source file %q: %w", layer.Source, openErr)
	}

	hasher := sha256.New()
	stageErr := writeImageLayerBuildFile(ctx, destination, sourceFile, hasher)
	closeErr := sourceFile.Close()
	if stageErr != nil {
		return errors.Join(stageErr, wrapImageLayerSourceCloseError(layer.Source, closeErr))
	}
	if closeErr != nil {
		return fmt.Errorf("closing layer source file %q: %w", layer.Source, closeErr)
	}

	actualHashHex := hex.EncodeToString(hasher.Sum(nil))
	return verifyLayerHash(layer, actualHashHex)
}

func wrapImageLayerSourceCloseError(source string, closeErr error) error {
	if closeErr == nil {
		return nil
	}
	return fmt.Errorf("closing layer source file %q: %w", source, closeErr)
}

// ReadImageIDFile reads and validates a bounded SHA256 image ID from a regular file.
func ReadImageIDFile(name string) (string, error) {
	info, statErr := os.Lstat(name)
	if statErr != nil {
		return "", fmt.Errorf("inspecting image ID file %q: %w", name, statErr)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("image ID file %q is not a regular file", name)
	}

	file, openErr := usvc_io.EnsureFile(name, osutil.PermissionOnlyOwnerReadWrite)
	if openErr != nil {
		return "", fmt.Errorf("opening image ID file %q: %w", name, openErr)
	}

	contents, readErr := io.ReadAll(io.LimitReader(file, maxImageIDFileSize+1))
	closeErr := file.Close()
	if readErr != nil {
		return "", errors.Join(
			fmt.Errorf("reading image ID file %q: %w", name, readErr),
			wrapImageLayerFileCloseError(name, closeErr),
		)
	}
	if closeErr != nil {
		return "", fmt.Errorf("closing image ID file %q: %w", name, closeErr)
	}
	if len(contents) > maxImageIDFileSize {
		return "", fmt.Errorf("image ID file %q exceeds %d bytes", name, maxImageIDFileSize)
	}

	imageID := strings.TrimSpace(string(contents))
	if imageID == "" {
		return "", fmt.Errorf("image ID file is empty")
	}
	if validateErr := validateBuiltImageID(imageID); validateErr != nil {
		return "", fmt.Errorf("invalid image ID %q: %w", imageID, validateErr)
	}
	return imageID, nil
}

func validateBuiltImageID(imageID string) error {
	const sha256Prefix = "sha256:"
	const sha256HexLength = sha256.Size * 2

	if len(imageID) != len(sha256Prefix)+sha256HexLength || !strings.EqualFold(imageID[:len(sha256Prefix)], sha256Prefix) {
		return fmt.Errorf("expected %s followed by %d hexadecimal characters", sha256Prefix, sha256HexLength)
	}
	if _, decodeErr := hex.DecodeString(imageID[len(sha256Prefix):]); decodeErr != nil {
		return fmt.Errorf("decoding SHA256 value: %w", decodeErr)
	}
	return nil
}

// verifyLayerSourceHash streams the source file through a SHA256 hasher
// and verifies the hash matches, without buffering the full file in memory.
func verifyLayerSourceHash(layer *ImageLayer) error {
	f, openErr := usvc_io.OpenFileReadOnly(layer.Source)
	if openErr != nil {
		return fmt.Errorf("opening layer source file %q: %w", layer.Source, openErr)
	}
	defer f.Close()

	hasher := sha256.New()
	if _, copyErr := io.Copy(hasher, f); copyErr != nil {
		return fmt.Errorf("hashing layer source file %q: %w", layer.Source, copyErr)
	}

	actualHashHex := hex.EncodeToString(hasher.Sum(nil))
	return verifyLayerHash(layer, actualHashHex)
}

func verifyLayerHash(layer *ImageLayer, actualHashHex string) error {
	expectedHash := strings.TrimSpace(layer.SHA256)
	if strings.HasPrefix(strings.ToLower(expectedHash), "sha256:") {
		expectedHash = expectedHash[7:]
	}
	if !strings.EqualFold(actualHashHex, expectedHash) {
		return fmt.Errorf("SHA256 mismatch for layer source %q: expected %s, got %s", layer.Source, layer.SHA256, actualHashHex)
	}

	return nil
}

// streamLayerFromSource streams a source-file layer directly into the tar writer
// without buffering the full file contents in memory.
func streamLayerFromSource(tw *usvc_io.TarWriter, layer *ImageLayer, tarName string, modTime time.Time) error {
	f, openErr := usvc_io.OpenFileReadOnly(layer.Source)
	if openErr != nil {
		return fmt.Errorf("opening layer source file %q: %w", layer.Source, openErr)
	}
	defer f.Close()

	info, statErr := f.Stat()
	if statErr != nil {
		return fmt.Errorf("getting size of layer source file %q: %w", layer.Source, statErr)
	}

	return tw.CopyFile(f, info.Size(), tarName, 0, 0, 0644, modTime, modTime, modTime)
}
