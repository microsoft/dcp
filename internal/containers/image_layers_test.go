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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
)

// fakeCLICommandRunner implements CLICommandRunner for testing.
type fakeCLICommandRunner struct {
	result  *fakeBuildResult
	outText string
	errText string
	retErr  error
}

// fakeBuildResult captures the stdin tar and args for inspection by tests.
type fakeBuildResult struct {
	args     []string
	stdinTar []byte
}

func (f *fakeCLICommandRunner) MakeCommand(args ...string) *exec.Cmd {
	return exec.Command("echo", args...)
}

func (f *fakeCLICommandRunner) RunBufferedCommand(ctx context.Context, opName string, cmd *exec.Cmd, stdout io.WriteCloser, stderr io.WriteCloser, timeout time.Duration) (*bytes.Buffer, *bytes.Buffer, error) {
	f.result.args = cmd.Args

	if cmd.Stdin != nil {
		data, readErr := io.ReadAll(cmd.Stdin)
		if readErr != nil {
			return nil, nil, readErr
		}
		f.result.stdinTar = data
	}

	outBuf := bytes.NewBufferString(f.outText)
	errBuf := bytes.NewBufferString(f.errText)
	return outBuf, errBuf, f.retErr
}

func newFakeRunner(result *fakeBuildResult, outText string, errText string, retErr error) *fakeCLICommandRunner {
	return &fakeCLICommandRunner{result: result, outText: outText, errText: errText, retErr: retErr}
}

// parseTarEntries extracts file names and contents from a tar archive.
func parseTarEntries(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	entries := make(map[string][]byte)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		header, headerErr := tr.Next()
		if headerErr == io.EOF {
			break
		}
		require.NoError(t, headerErr)

		content, readErr := io.ReadAll(tr)
		require.NoError(t, readErr)
		entries[header.Name] = content
	}
	return entries
}

func TestApplyImageLayersImpl_DockerfileGeneration(t *testing.T) {
	t.Parallel()

	layerContent := []byte("fake tar content")
	rawContents := base64.StdEncoding.EncodeToString(layerContent)

	result := &fakeBuildResult{}
	options := ApplyImageLayersOptions{
		BaseImage: InspectedImage{
			Id:   "sha256:baseimage123",
			Tags: []string{"myrepo/myimage:v1"},
		},
		Layers: []apiv1.ImageLayer{
			{Digest: "layer-a", RawContents: rawContents},
			{Digest: "layer-b", RawContents: rawContents},
		},
		Tag: "myrepo/myimage:dcp-abc123",
	}

	imageRef, applyErr := ApplyImageLayersImpl(
		context.Background(),
		logr.Discard(),
		options,
		newFakeRunner(result, "", "", nil),
	)

	require.NoError(t, applyErr)
	assert.Equal(t, "myrepo/myimage:dcp-abc123", imageRef)

	// Verify the tar contains the expected entries
	entries := parseTarEntries(t, result.stdinTar)

	dockerfileContent, hasDockerfile := entries["Dockerfile"]
	require.True(t, hasDockerfile, "tar should contain Dockerfile")
	assert.Contains(t, string(dockerfileContent), "FROM myrepo/myimage:v1\n")
	assert.Contains(t, string(dockerfileContent), "ADD layer0.tar /\n")
	assert.Contains(t, string(dockerfileContent), "ADD layer1.tar /\n")

	_, hasLayer0 := entries["layer0.tar"]
	assert.True(t, hasLayer0, "tar should contain layer0.tar")
	_, hasLayer1 := entries["layer1.tar"]
	assert.True(t, hasLayer1, "tar should contain layer1.tar")
	assert.Equal(t, layerContent, entries["layer0.tar"])
	assert.Equal(t, layerContent, entries["layer1.tar"])
}

func TestApplyImageLayersImpl_UsesImageIdWhenNoTags(t *testing.T) {
	t.Parallel()

	rawContents := base64.StdEncoding.EncodeToString([]byte("data"))

	result := &fakeBuildResult{}
	options := ApplyImageLayersOptions{
		BaseImage: InspectedImage{Id: "sha256:abc123"},
		Layers:    []apiv1.ImageLayer{{Digest: "d1", RawContents: rawContents}},
		Tag:       "test:tag",
	}

	_, applyErr := ApplyImageLayersImpl(
		context.Background(), logr.Discard(), options,
		newFakeRunner(result, "", "", nil),
	)
	require.NoError(t, applyErr)

	entries := parseTarEntries(t, result.stdinTar)
	assert.Contains(t, string(entries["Dockerfile"]), "FROM sha256:abc123\n")
}

func TestApplyImageLayersImpl_ReturnsImageIdWhenNoTag(t *testing.T) {
	t.Parallel()

	rawContents := base64.StdEncoding.EncodeToString([]byte("data"))

	options := ApplyImageLayersOptions{
		BaseImage: InspectedImage{Id: "sha256:base", Tags: []string{"img:v1"}},
		Layers:    []apiv1.ImageLayer{{Digest: "d1", RawContents: rawContents}},
		Tag:       "",
	}

	imageRef, applyErr := ApplyImageLayersImpl(
		context.Background(), logr.Discard(), options,
		newFakeRunner(&fakeBuildResult{}, "sha256:newimage123\n", "", nil),
	)

	require.NoError(t, applyErr)
	assert.Equal(t, "sha256:newimage123", imageRef)
}

func TestApplyImageLayersImpl_EmptyLayersReturnsError(t *testing.T) {
	t.Parallel()

	options := ApplyImageLayersOptions{
		BaseImage: InspectedImage{Id: "sha256:base"},
		Layers:    []apiv1.ImageLayer{},
		Tag:       "test:tag",
	}

	_, applyErr := ApplyImageLayersImpl(
		context.Background(), logr.Discard(), options,
		newFakeRunner(&fakeBuildResult{}, "", "", nil),
	)

	require.Error(t, applyErr)
	assert.Contains(t, applyErr.Error(), "at least one image layer")
}

func TestApplyImageLayersImpl_BuildErrorTakesPrecedenceOverTarError(t *testing.T) {
	t.Parallel()

	rawContents := base64.StdEncoding.EncodeToString([]byte("data"))

	options := ApplyImageLayersOptions{
		BaseImage: InspectedImage{Id: "sha256:base", Tags: []string{"img:v1"}},
		Layers:    []apiv1.ImageLayer{{Digest: "d1", RawContents: rawContents}},
		Tag:       "test:tag",
	}

	buildError := fmt.Errorf("docker command 'ApplyImageLayers' returned with non-zero exit code 1")
	_, applyErr := ApplyImageLayersImpl(
		context.Background(), logr.Discard(), options,
		newFakeRunner(&fakeBuildResult{}, "", "error: Dockerfile parse error", buildError),
	)

	require.Error(t, applyErr)
	assert.Contains(t, applyErr.Error(), "building derived image")
	assert.Contains(t, applyErr.Error(), "Dockerfile parse error")
}

func TestApplyImageLayersImpl_SourceLayerWithSHA256Prefix(t *testing.T) {
	t.Parallel()

	// Create a temp file with known content
	layerContent := []byte("test layer data for sha256 prefix test")
	hash := sha256.Sum256(layerContent)
	hashHex := hex.EncodeToString(hash[:])

	tempDir := t.TempDir()
	layerPath := filepath.Join(tempDir, "layer.tar")
	require.NoError(t, os.WriteFile(layerPath, layerContent, 0644))

	result := &fakeBuildResult{}
	options := ApplyImageLayersOptions{
		BaseImage: InspectedImage{Id: "sha256:base", Tags: []string{"img:v1"}},
		Layers: []apiv1.ImageLayer{{
			Digest: "d1",
			Source: layerPath,
			SHA256: "sha256:" + hashHex,
		}},
		Tag: "test:tag",
	}

	imageRef, applyErr := ApplyImageLayersImpl(
		context.Background(), logr.Discard(), options,
		newFakeRunner(result, "", "", nil),
	)

	require.NoError(t, applyErr)
	assert.Equal(t, "test:tag", imageRef)

	entries := parseTarEntries(t, result.stdinTar)
	assert.Equal(t, layerContent, entries["layer0.tar"])
}

func TestApplyImageLayersImpl_SourceLayerWithUppercaseSHA256(t *testing.T) {
	t.Parallel()

	layerContent := []byte("test layer data for uppercase test")
	hash := sha256.Sum256(layerContent)
	hashHex := strings.ToUpper(hex.EncodeToString(hash[:]))

	tempDir := t.TempDir()
	layerPath := filepath.Join(tempDir, "layer.tar")
	require.NoError(t, os.WriteFile(layerPath, layerContent, 0644))

	result := &fakeBuildResult{}
	options := ApplyImageLayersOptions{
		BaseImage: InspectedImage{Id: "sha256:base", Tags: []string{"img:v1"}},
		Layers: []apiv1.ImageLayer{{
			Digest: "d1",
			Source: layerPath,
			SHA256: hashHex,
		}},
		Tag: "test:tag",
	}

	_, applyErr := ApplyImageLayersImpl(
		context.Background(), logr.Discard(), options,
		newFakeRunner(result, "", "", nil),
	)

	require.NoError(t, applyErr)
}

func TestApplyImageLayersImpl_SourceLayerHashMismatch(t *testing.T) {
	t.Parallel()

	layerContent := []byte("test layer data")

	tempDir := t.TempDir()
	layerPath := filepath.Join(tempDir, "layer.tar")
	require.NoError(t, os.WriteFile(layerPath, layerContent, 0644))

	options := ApplyImageLayersOptions{
		BaseImage: InspectedImage{Id: "sha256:base", Tags: []string{"img:v1"}},
		Layers: []apiv1.ImageLayer{{
			Digest: "d1",
			Source: layerPath,
			SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		}},
		Tag: "test:tag",
	}

	_, applyErr := ApplyImageLayersImpl(
		context.Background(), logr.Discard(), options,
		newFakeRunner(&fakeBuildResult{}, "", "", nil),
	)

	require.Error(t, applyErr)
	assert.Contains(t, applyErr.Error(), "SHA256 mismatch")
}

func TestApplyImageLayersImpl_BuildCommandArgs(t *testing.T) {
	t.Parallel()

	rawContents := base64.StdEncoding.EncodeToString([]byte("data"))

	t.Run("with tag", func(t *testing.T) {
		t.Parallel()
		result := &fakeBuildResult{}
		options := ApplyImageLayersOptions{
			BaseImage: InspectedImage{Id: "sha256:base", Tags: []string{"img:v1"}},
			Layers:    []apiv1.ImageLayer{{Digest: "d1", RawContents: rawContents}},
			Tag:       "myimage:dcp-abc",
		}

		_, applyErr := ApplyImageLayersImpl(
			context.Background(), logr.Discard(), options,
			newFakeRunner(result, "", "", nil),
		)
		require.NoError(t, applyErr)

		// Args[0] is "echo" from MakeCommand, rest are the build args
		assert.Contains(t, result.args, "--quiet")
		assert.Contains(t, result.args, "-t")
		assert.Contains(t, result.args, "myimage:dcp-abc")
		assert.Equal(t, "-", result.args[len(result.args)-1])
	})

	t.Run("without tag", func(t *testing.T) {
		t.Parallel()
		result := &fakeBuildResult{}
		options := ApplyImageLayersOptions{
			BaseImage: InspectedImage{Id: "sha256:base", Tags: []string{"img:v1"}},
			Layers:    []apiv1.ImageLayer{{Digest: "d1", RawContents: rawContents}},
			Tag:       "",
		}

		_, applyErr := ApplyImageLayersImpl(
			context.Background(), logr.Discard(), options,
			newFakeRunner(result, "sha256:built", "", nil),
		)
		require.NoError(t, applyErr)

		assert.NotContains(t, result.args, "-t")
	})
}
