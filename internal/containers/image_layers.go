/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/dcp/api/v1"
)

const defaultApplyImageLayersTimeout = 10 * time.Minute

type makeCommandFn func(args ...string) *exec.Cmd
type runBufferedCommandFn func(ctx context.Context, opName string, cmd *exec.Cmd, stdout io.WriteCloser, stderr io.WriteCloser, timeout time.Duration) (*bytes.Buffer, *bytes.Buffer, error)

// ApplyImageLayersImpl builds a derived container image by applying additional tar layers
// on top of a base image. It streams a build context tar (containing a generated Dockerfile
// and the layer tars) to `docker build` via stdin, avoiding any temporary files on disk.
func ApplyImageLayersImpl(
	ctx context.Context,
	log logr.Logger,
	options ApplyImageLayersOptions,
	makeCommand makeCommandFn,
	runBufferedCommand runBufferedCommandFn,
) (string, error) {
	if len(options.Layers) == 0 {
		return options.Tag, nil
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

	// Build the Dockerfile content and collect layer data
	dockerfile := fmt.Sprintf("FROM %s\n", baseImage)

	type layerEntry struct {
		fileName string
		data     []byte
	}
	layerEntries := make([]layerEntry, 0, len(options.Layers))

	for i := range options.Layers {
		layer := &options.Layers[i]
		layerFileName := fmt.Sprintf("layer%d.tar", i)

		layerData, readErr := readLayerData(layer, log)
		if readErr != nil {
			return "", fmt.Errorf("preparing image layer %d: %w", i, readErr)
		}

		layerEntries = append(layerEntries, layerEntry{fileName: layerFileName, data: layerData})
		dockerfile += fmt.Sprintf("ADD %s /\n", layerFileName)
	}

	// Build a tar archive containing the Dockerfile and all layer tars
	var contextBuf bytes.Buffer
	tw := tar.NewWriter(&contextBuf)

	if writeErr := writeTarEntry(tw, "Dockerfile", []byte(dockerfile)); writeErr != nil {
		return "", fmt.Errorf("writing Dockerfile to build context tar: %w", writeErr)
	}

	for i, entry := range layerEntries {
		if writeErr := writeTarEntry(tw, entry.fileName, entry.data); writeErr != nil {
			return "", fmt.Errorf("writing layer %d to build context tar: %w", i, writeErr)
		}
	}

	if closeErr := tw.Close(); closeErr != nil {
		return "", fmt.Errorf("closing build context tar: %w", closeErr)
	}

	// Build the derived image, streaming the tar context via stdin.
	// --no-cache prevents BuildKit from reusing potentially stale cache entries.
	// --load ensures the image is exported from the BuildKit cache into the
	// Docker daemon's image store, preventing manifest/rootfs mismatches.
	// The trailing "-" tells docker build to read the build context from stdin.
	args := []string{"build", "--no-cache", "--load"}
	if options.Tag != "" {
		args = append(args, "-t", options.Tag)
	}
	args = append(args, "-")

	cmd := makeCommand(args...)
	cmd.Stdin = &contextBuf

	_, errBuf, buildErr := runBufferedCommand(ctx, "ApplyImageLayers", cmd, nil, nil, timeout)
	if buildErr != nil {
		errDetail := ""
		if errBuf != nil {
			errDetail = errBuf.String()
		}
		return "", fmt.Errorf("building derived image with image layers: %w: %s", buildErr, errDetail)
	}

	log.V(1).Info("Built derived image with image layers", "Tag", options.Tag, "LayerCount", len(options.Layers))

	return options.Tag, nil
}

// readLayerData reads the raw tar data for a layer, from Source (with SHA256 verification) or RawContents.
func readLayerData(layer *apiv1.ImageLayer, log logr.Logger) ([]byte, error) {
	if layer.Source != "" {
		return readLayerFromSource(layer, log)
	}

	return readLayerFromRawContents(layer)
}

func readLayerFromSource(layer *apiv1.ImageLayer, log logr.Logger) ([]byte, error) {
	srcData, readErr := os.ReadFile(layer.Source)
	if readErr != nil {
		return nil, fmt.Errorf("reading layer source file %q: %w", layer.Source, readErr)
	}

	actualHash := sha256.Sum256(srcData)
	actualHashHex := hex.EncodeToString(actualHash[:])
	if !strings.EqualFold(actualHashHex, layer.SHA256) {
		return nil, fmt.Errorf("SHA256 mismatch for layer source %q: expected %s, got %s", layer.Source, layer.SHA256, actualHashHex)
	}

	log.V(1).Info("Layer source SHA256 verified", "Source", layer.Source, "Digest", layer.Digest)
	return srcData, nil
}

func readLayerFromRawContents(layer *apiv1.ImageLayer) ([]byte, error) {
	decoded, decodeErr := base64.StdEncoding.DecodeString(layer.RawContents)
	if decodeErr != nil {
		return nil, fmt.Errorf("decoding base64 rawContents for layer %q: %w", layer.Digest, decodeErr)
	}
	return decoded, nil
}

func writeTarEntry(tw *tar.Writer, name string, data []byte) error {
	header := &tar.Header{
		Name: name,
		Mode: 0644,
		Size: int64(len(data)),
	}
	if headerErr := tw.WriteHeader(header); headerErr != nil {
		return headerErr
	}
	_, writeErr := tw.Write(data)
	return writeErr
}
