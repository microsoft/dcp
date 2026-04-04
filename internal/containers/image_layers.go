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
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/dcp/api/v1"
	usvc_io "github.com/microsoft/dcp/pkg/io"
)

const defaultApplyImageLayersTimeout = 10 * time.Minute

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
	if len(options.Layers) == 0 {
		return "", fmt.Errorf("at least one image layer must be specified")
	}

	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultApplyImageLayersTimeout
	}

	// Use a tag if available, otherwise fall back to the image ID for the FROM directive
	baseImage := options.BaseImage.Id
	if len(options.BaseImage.Tags) > 0 {
		baseImage = options.BaseImage.Tags[0]
	}

	// First pass: verify source-file layers and build the Dockerfile content.
	// Source layers are hash-verified here (streaming, no full buffer) so that
	// the second pass can stream them directly into the tar.
	dockerfile := fmt.Sprintf("FROM %s\n", baseImage)
	for i := range options.Layers {
		layer := &options.Layers[i]

		if layer.Source != "" {
			if verifyErr := verifyLayerSourceHash(layer); verifyErr != nil {
				return "", fmt.Errorf("verifying image layer %d: %w", i, verifyErr)
			}
			log.V(1).Info("Layer source SHA256 verified", "Source", layer.Source, "Digest", layer.Digest)
		}

		dockerfile += fmt.Sprintf("ADD layer%d.tar /\n", i)
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
			layerFileName := fmt.Sprintf("layer%d.tar", i)

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
	args = append(args, "-")

	cmd := runner.MakeCommand(args...)
	cmd.Stdin = pr

	outBuf, errBuf, buildErr := runner.RunBufferedCommand(ctx, "ApplyImageLayers", cmd, nil, nil, timeout)

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

// verifyLayerSourceHash streams the source file through a SHA256 hasher
// and verifies the hash matches, without buffering the full file in memory.
func verifyLayerSourceHash(layer *apiv1.ImageLayer) error {
	f, openErr := os.Open(layer.Source)
	if openErr != nil {
		return fmt.Errorf("opening layer source file %q: %w", layer.Source, openErr)
	}
	defer f.Close()

	hasher := sha256.New()
	if _, copyErr := io.Copy(hasher, f); copyErr != nil {
		return fmt.Errorf("hashing layer source file %q: %w", layer.Source, copyErr)
	}

	actualHashHex := hex.EncodeToString(hasher.Sum(nil))
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
func streamLayerFromSource(tw *usvc_io.TarWriter, layer *apiv1.ImageLayer, tarName string, modTime time.Time) error {
	f, openErr := os.Open(layer.Source)
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
