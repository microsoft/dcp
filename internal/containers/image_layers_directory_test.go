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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

type buildImageFunc func(context.Context, BuildImageOptions) error

func (f buildImageFunc) BuildImage(ctx context.Context, options BuildImageOptions) error {
	return f(ctx, options)
}

func TestApplyImageLayersFromDirectoryStagesRawAndSourceLayers(t *testing.T) {
	t.Parallel()

	rawLayer := []byte("raw layer contents")
	sourceLayer := []byte("source layer contents")
	sourceHash := sha256.Sum256(sourceLayer)
	sourceDirectory := t.TempDir()
	sourcePath := filepath.Join(sourceDirectory, "source-layer.tar")
	require.NoError(t, usvc_io.WriteFile(sourcePath, sourceLayer, osutil.PermissionOnlyOwnerReadWrite))

	labels := []Label{
		{Key: "first.label", Value: "first value"},
		{Key: "second.label", Value: "second value"},
	}
	options := ApplyImageLayersOptions{
		BaseImage: InspectedImage{
			Id:   "sha256:base-id",
			Tags: []string{"example.test/base:tag"},
		},
		Layers: []ImageLayer{
			{Digest: "raw", RawContents: base64.StdEncoding.EncodeToString(rawLayer)},
			{
				Digest: "source",
				Source: sourcePath,
				SHA256: "SHA256:" + strings.ToUpper(hex.EncodeToString(sourceHash[:])),
			},
		},
		Labels: labels,
		Tag:    "example.test/derived:tag",
		TimeoutOption: TimeoutOption{
			Timeout: 37 * time.Second,
		},
	}

	var workspace string
	builder := buildImageFunc(func(ctx context.Context, buildOptions BuildImageOptions) error {
		require.NoError(t, ctx.Err())
		workspace = filepath.Dir(buildOptions.Context)
		require.NoError(t, usvc_io.ValidateRestrictedDirectory(workspace, osutil.PermissionOnlyOwnerReadWriteTraverse))
		require.NoError(t, usvc_io.ValidateRestrictedDirectory(buildOptions.Context, osutil.PermissionOnlyOwnerReadWriteTraverse))

		assert.Equal(t, filepath.Join(buildOptions.Context, "Dockerfile"), buildOptions.Dockerfile)
		assert.Equal(t, []string{options.Tag}, buildOptions.Tags)
		assert.Equal(t, labels, buildOptions.Labels)
		assert.Equal(t, options.Timeout, buildOptions.Timeout)
		assert.Empty(t, buildOptions.IidFile)

		dockerfile := readImageLayerTestFile(t, buildOptions.Dockerfile)
		assert.Equal(t, "FROM example.test/base:tag\nADD layer0.tar /\nADD layer1.tar /\n", string(dockerfile))
		assert.Equal(t, rawLayer, readImageLayerTestFile(t, filepath.Join(buildOptions.Context, "layer0.tar")))
		assert.Equal(t, sourceLayer, readImageLayerTestFile(t, filepath.Join(buildOptions.Context, "layer1.tar")))

		entries, readDirectoryErr := os.ReadDir(buildOptions.Context)
		require.NoError(t, readDirectoryErr)
		require.Len(t, entries, 3)
		assert.Equal(t, "Dockerfile", entries[0].Name())
		assert.Equal(t, "layer0.tar", entries[1].Name())
		assert.Equal(t, "layer1.tar", entries[2].Name())
		return nil
	})

	imageRef, applyErr := ApplyImageLayersFromDirectory(context.Background(), logr.Discard(), options, builder)

	require.NoError(t, applyErr)
	assert.Equal(t, options.Tag, imageRef)
	require.NotEmpty(t, workspace)
	assertPathRemoved(t, workspace)
}

func TestApplyImageLayersFromDirectoryRejectsStagedSourceHashMismatch(t *testing.T) {
	t.Parallel()

	sourceDirectory := t.TempDir()
	sourcePath := filepath.Join(sourceDirectory, "source-layer.tar")
	require.NoError(t, usvc_io.WriteFile(sourcePath, []byte("source layer contents"), osutil.PermissionOnlyOwnerReadWrite))

	builderCalled := false
	tempDirectory := t.TempDir()
	imageRef, applyErr := applyImageLayersFromDirectory(
		context.Background(),
		logr.Discard(),
		ApplyImageLayersOptions{
			BaseImage: InspectedImage{Id: "sha256:base"},
			Layers: []ImageLayer{{
				Digest: "source",
				Source: sourcePath,
				SHA256: strings.Repeat("0", sha256.Size*2),
			}},
			Tag: "derived:tag",
		},
		buildImageFunc(func(context.Context, BuildImageOptions) error {
			builderCalled = true
			return nil
		}),
		tempDirectory,
	)

	require.Error(t, applyErr)
	assert.Contains(t, applyErr.Error(), "SHA256 mismatch")
	assert.Empty(t, imageRef)
	assert.False(t, builderCalled)
	assertDirectoryEmpty(t, tempDirectory)
}

func TestApplyImageLayersFromDirectoryReturnsValidatedImageID(t *testing.T) {
	t.Parallel()

	expectedImageID := "SHA256:" + strings.Repeat("A", sha256.Size*2)
	tempDirectory := t.TempDir()
	var workspace string
	builder := buildImageFunc(func(_ context.Context, buildOptions BuildImageOptions) error {
		workspace = filepath.Dir(buildOptions.Context)
		require.Empty(t, buildOptions.Tags)
		require.Equal(t, defaultApplyImageLayersTimeout, buildOptions.Timeout)
		require.Equal(t, filepath.Join(workspace, "image.iid"), buildOptions.IidFile)
		require.Equal(t, "FROM sha256:base-id\nADD layer0.tar /\n", string(readImageLayerTestFile(t, buildOptions.Dockerfile)))
		return usvc_io.WriteFile(buildOptions.IidFile, []byte(expectedImageID+"\n"), osutil.PermissionOnlyOwnerReadWrite)
	})

	imageRef, applyErr := applyImageLayersFromDirectory(
		context.Background(),
		logr.Discard(),
		ApplyImageLayersOptions{
			BaseImage: InspectedImage{Id: "sha256:base-id"},
			Layers: []ImageLayer{{
				Digest:      "raw",
				RawContents: base64.StdEncoding.EncodeToString([]byte("layer")),
			}},
		},
		builder,
		tempDirectory,
	)

	require.NoError(t, applyErr)
	assert.Equal(t, expectedImageID, imageRef)
	require.NotEmpty(t, workspace)
	assertPathRemoved(t, workspace)
	assertDirectoryEmpty(t, tempDirectory)
}

func TestApplyImageLayersFromDirectoryRejectsMissingOrInvalidImageID(t *testing.T) {
	testCases := []struct {
		name          string
		writeImageID  func(t *testing.T, path string)
		errorContains string
	}{
		{
			name: "missing",
			writeImageID: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Remove(path))
			},
			errorContains: "inspecting image ID file",
		},
		{
			name: "invalid",
			writeImageID: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, usvc_io.WriteFile(path, []byte("not-an-image-id"), osutil.PermissionOnlyOwnerReadWrite))
			},
			errorContains: "invalid image ID",
		},
		{
			name: "oversized",
			writeImageID: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, usvc_io.WriteFile(path, []byte(strings.Repeat("a", maxImageIDFileSize+1)), osutil.PermissionOnlyOwnerReadWrite))
			},
			errorContains: "exceeds 1024 bytes",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			tempDirectory := t.TempDir()
			imageRef, applyErr := applyImageLayersFromDirectory(
				context.Background(),
				logr.Discard(),
				ApplyImageLayersOptions{
					BaseImage: InspectedImage{Id: "sha256:base"},
					Layers: []ImageLayer{{
						Digest:      "raw",
						RawContents: base64.StdEncoding.EncodeToString([]byte("layer")),
					}},
				},
				buildImageFunc(func(_ context.Context, buildOptions BuildImageOptions) error {
					testCase.writeImageID(t, buildOptions.IidFile)
					return nil
				}),
				tempDirectory,
			)

			require.Error(t, applyErr)
			assert.Contains(t, applyErr.Error(), testCase.errorContains)
			assert.Empty(t, imageRef)
			assertDirectoryEmpty(t, tempDirectory)
		})
	}
}

func TestApplyImageLayersFromDirectoryReturnsBuilderFailureAndCleansUp(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("builder failed")
	tempDirectory := t.TempDir()
	var workspace string
	imageRef, applyErr := applyImageLayersFromDirectory(
		context.Background(),
		logr.Discard(),
		ApplyImageLayersOptions{
			BaseImage: InspectedImage{Id: "sha256:base"},
			Layers: []ImageLayer{{
				Digest:      "raw",
				RawContents: base64.StdEncoding.EncodeToString([]byte("layer")),
			}},
			Tag: "derived:tag",
		},
		buildImageFunc(func(_ context.Context, buildOptions BuildImageOptions) error {
			workspace = filepath.Dir(buildOptions.Context)
			return expectedErr
		}),
		tempDirectory,
	)

	require.ErrorIs(t, applyErr, expectedErr)
	assert.Empty(t, imageRef)
	require.NotEmpty(t, workspace)
	assertPathRemoved(t, workspace)
	assertDirectoryEmpty(t, tempDirectory)
}

func TestApplyImageLayersFromDirectoryHonorsBuilderCancellationAndCleansUp(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tempDirectory := t.TempDir()
	var workspace string
	imageRef, applyErr := applyImageLayersFromDirectory(
		ctx,
		logr.Discard(),
		ApplyImageLayersOptions{
			BaseImage: InspectedImage{Id: "sha256:base"},
			Layers: []ImageLayer{{
				Digest:      "raw",
				RawContents: base64.StdEncoding.EncodeToString([]byte("layer")),
			}},
			Tag: "derived:tag",
		},
		buildImageFunc(func(_ context.Context, buildOptions BuildImageOptions) error {
			workspace = filepath.Dir(buildOptions.Context)
			cancel()
			return nil
		}),
		tempDirectory,
	)

	require.ErrorIs(t, applyErr, context.Canceled)
	assert.Empty(t, imageRef)
	require.NotEmpty(t, workspace)
	assertPathRemoved(t, workspace)
	assertDirectoryEmpty(t, tempDirectory)
}

func readImageLayerTestFile(t *testing.T, path string) []byte {
	t.Helper()

	file, openErr := usvc_io.OpenFileReadOnly(path)
	require.NoError(t, openErr)
	contents, readErr := io.ReadAll(file)
	closeErr := file.Close()
	require.NoError(t, readErr)
	require.NoError(t, closeErr)
	return contents
}

func assertPathRemoved(t *testing.T, path string) {
	t.Helper()

	_, statErr := os.Lstat(path)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()

	entries, readErr := os.ReadDir(path)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}
