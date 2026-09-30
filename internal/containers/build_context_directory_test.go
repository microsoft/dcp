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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

type buildArchiveTestEntry struct {
	name     string
	kind     byte
	contents string
	link     string
}

func buildArchiveTestContents(t *testing.T, entries ...buildArchiveTestEntry) []byte {
	t.Helper()
	var contents bytes.Buffer
	writer := tar.NewWriter(&contents)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: 0755, Linkname: entry.link}
		if entry.kind == tar.TypeReg || entry.kind == tar.TypeRegA {
			header.Size = int64(len(entry.contents))
		}
		require.NoError(t, writer.WriteHeader(header))
		if entry.contents != "" {
			_, writeErr := io.WriteString(writer, entry.contents)
			require.NoError(t, writeErr)
		}
	}
	require.NoError(t, writer.Close())
	return contents.Bytes()
}

// Verifies that source-file and raw archives stage correct contents with restricted permissions while preserving build options and caller data.
// The temporary workspace must be removed after the builder succeeds.
func TestBuildImageFromArchiveDirectoryPreservesBuildOptions(t *testing.T) {
	t.Parallel()

	contents := buildArchiveTestContents(t,
		buildArchiveTestEntry{name: "./", kind: tar.TypeDir},
		buildArchiveTestEntry{name: "nested/", kind: tar.TypeDir},
		buildArchiveTestEntry{name: "nested/Containerfile", kind: tar.TypeReg, contents: "FROM scratch\n"},
		buildArchiveTestEntry{name: "data/marker", kind: tar.TypeReg, contents: "archive marker"},
	)
	for _, representation := range []string{"source", "raw"} {
		t.Run(representation, func(t *testing.T) {
			t.Parallel()

			archive := &ContainerBuildContextArchive{}
			if representation == "raw" {
				archive.RawContents = base64.StdEncoding.EncodeToString(contents)
			} else {
				archive.Source = filepath.Join(t.TempDir(), "build context.tar")
				archive.SHA256 = fmt.Sprintf("sha256:%x", sha256.Sum256(contents))
				require.NoError(t, usvc_io.WriteFile(archive.Source, contents, osutil.PermissionOnlyOwnerReadWrite))
			}
			options := BuildImageOptions{
				IidFile: filepath.Join(t.TempDir(), "image.iid"),
				Pull:    true,
				ContainerBuildContext: &ContainerBuildContext{
					ContextArchive: archive,
					Dockerfile:     "nested/Containerfile",
					Digest:         "context-digest",
					Tags:           []string{"first:tag", "second:tag"},
					Args:           []EnvVar{{Name: "ARG", Value: "value"}},
					Secrets:        []ContainerBuildSecret{{ID: "secret", Type: EnvSecret, Value: "private-value"}},
					Stage:          "final",
					Labels:         []Label{{Key: "owner", Value: "test"}},
				},
				TimeoutOption: TimeoutOption{Timeout: time.Minute},
			}
			tempDirectory := t.TempDir()
			var workspace string
			builder := buildImageFunc(func(ctx context.Context, staged BuildImageOptions) error {
				require.NoError(t, ctx.Err())
				_, hasDeadline := ctx.Deadline()
				require.True(t, hasDeadline)
				workspace = staged.Context
				require.Equal(t, tempDirectory, filepath.Dir(workspace))
				require.NoError(t, usvc_io.ValidateRestrictedDirectory(workspace, osutil.PermissionOnlyOwnerReadWriteTraverse))
				require.Nil(t, staged.ContextArchive)
				require.Equal(t, filepath.Join(workspace, "nested", "Containerfile"), staged.Dockerfile)
				require.Equal(t, "FROM scratch\n", string(readImageLayerTestFile(t, staged.Dockerfile)))
				require.Equal(t, "archive marker", string(readImageLayerTestFile(t, filepath.Join(workspace, "data", "marker"))))
				require.Equal(t, options.IidFile, staged.IidFile)
				require.Equal(t, options.Pull, staged.Pull)
				require.Equal(t, options.Timeout, staged.Timeout)
				require.Equal(t, options.Digest, staged.Digest)
				require.Equal(t, options.Tags, staged.Tags)
				require.Equal(t, options.Args, staged.Args)
				require.Equal(t, options.Secrets, staged.Secrets)
				require.Equal(t, options.Stage, staged.Stage)
				require.Equal(t, options.Labels, staged.Labels)
				return nil
			})

			buildErr := buildImageFromArchiveDirectory(t.Context(), options, builder, tempDirectory)
			require.NoError(t, buildErr)
			require.NotEmpty(t, workspace)
			require.NoDirExists(t, workspace)
			require.Empty(t, options.Context)
			require.Same(t, archive, options.ContextArchive)
			require.Equal(t, "nested/Containerfile", options.Dockerfile)
			if archive.Source != "" {
				require.Equal(t, contents, readImageLayerTestFile(t, archive.Source))
			}
		})
	}
}

// Verifies rejection of unsafe paths, links, special files, duplicate/case-colliding entries, and incompatible file/directory layouts.
// Rejected archives must not invoke the builder, alter an outside file, or leave a workspace behind.
func TestBuildImageFromArchiveDirectoryRejectsUnsafeEntries(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		entries []buildArchiveTestEntry
		want    string
	}{
		{"parent traversal", []buildArchiveTestEntry{{name: "../outside", kind: tar.TypeReg, contents: "bad"}}, "unsafe"},
		{"nested traversal", []buildArchiveTestEntry{{name: "inside/../outside", kind: tar.TypeReg, contents: "bad"}}, "unsafe"},
		{"absolute", []buildArchiveTestEntry{{name: "/outside", kind: tar.TypeReg, contents: "bad"}}, "unsafe"},
		{"drive path", []buildArchiveTestEntry{{name: "C:/outside", kind: tar.TypeReg, contents: "bad"}}, "unsafe"},
		{"backslashes", []buildArchiveTestEntry{{name: `inside\outside`, kind: tar.TypeReg, contents: "bad"}}, "unsafe"},
		{"alternate stream", []buildArchiveTestEntry{{name: "file:stream", kind: tar.TypeReg, contents: "bad"}}, "unsafe"},
		{"trailing dot", []buildArchiveTestEntry{{name: "file.", kind: tar.TypeReg, contents: "bad"}}, "unsafe"},
		{"trailing space", []buildArchiveTestEntry{{name: "file ", kind: tar.TypeReg, contents: "bad"}}, "unsafe"},
		{"symlink", []buildArchiveTestEntry{{name: "link", kind: tar.TypeSymlink, link: "../outside"}}, "unsupported"},
		{"hard link", []buildArchiveTestEntry{{name: "link", kind: tar.TypeLink, link: "Dockerfile"}}, "unsupported"},
		{"named pipe", []buildArchiveTestEntry{{name: "pipe", kind: tar.TypeFifo}}, "unsupported"},
		{"root replacement", []buildArchiveTestEntry{{name: ".", kind: tar.TypeReg, contents: "bad"}}, "workspace root"},
		{"duplicate", []buildArchiveTestEntry{{name: "Dockerfile", kind: tar.TypeReg, contents: "replace"}}, "duplicate"},
		{"case collision", []buildArchiveTestEntry{{name: "dockerfile", kind: tar.TypeReg, contents: "replace"}}, "duplicate"},
		{"implicit parent case collision", []buildArchiveTestEntry{
			{name: "directory/first", kind: tar.TypeReg, contents: "first"},
			{name: "DIRECTORY/second", kind: tar.TypeReg, contents: "second"},
		}, "case-colliding"},
		{"explicit parent case collision", []buildArchiveTestEntry{
			{name: "directory", kind: tar.TypeDir},
			{name: "DIRECTORY/child", kind: tar.TypeReg, contents: "child"},
		}, "case-colliding"},
		{"file replaces parent", []buildArchiveTestEntry{
			{name: "directory/child", kind: tar.TypeReg, contents: "child"},
			{name: "directory", kind: tar.TypeReg, contents: "replace"},
		}, "stage build context file"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			tempDirectory := t.TempDir()
			outsidePath := filepath.Join(tempDirectory, "outside")
			require.NoError(t, usvc_io.WriteFile(outsidePath, []byte("untouched"), osutil.PermissionOnlyOwnerReadWrite))
			entries := append([]buildArchiveTestEntry{{name: "Dockerfile", kind: tar.TypeReg, contents: "FROM scratch\n"}}, testCase.entries...)
			contents := buildArchiveTestContents(t, entries...)
			called := false
			buildErr := buildImageFromArchiveDirectory(t.Context(), BuildImageOptions{
				ContainerBuildContext: &ContainerBuildContext{ContextArchive: &ContainerBuildContextArchive{
					RawContents: base64.StdEncoding.EncodeToString(contents),
				}},
			}, buildImageFunc(func(context.Context, BuildImageOptions) error {
				called = true
				return nil
			}), tempDirectory)

			require.ErrorContains(t, buildErr, testCase.want)
			require.False(t, called)
			require.Equal(t, "untouched", string(readImageLayerTestFile(t, outsidePath)))
			remaining, listErr := os.ReadDir(tempDirectory)
			require.NoError(t, listErr)
			require.Len(t, remaining, 1)
			require.Equal(t, "outside", remaining[0].Name())
		})
	}
}

// Verifies rejection of invalid context combinations, Dockerfile paths, archive encodings/content, source types, and hashes.
// Input failures must not invoke the builder or leave staged files behind.
func TestBuildImageFromArchiveDirectoryRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	validContents := buildArchiveTestContents(t, buildArchiveTestEntry{
		name: "Dockerfile", kind: tar.TypeReg, contents: "FROM scratch\n",
	})
	validArchive := &ContainerBuildContextArchive{RawContents: base64.StdEncoding.EncodeToString(validContents)}
	sourcePath := filepath.Join(t.TempDir(), "source.tar")
	require.NoError(t, usvc_io.WriteFile(sourcePath, validContents, osutil.PermissionOnlyOwnerReadWrite))
	for _, testCase := range []struct {
		name    string
		options BuildImageOptions
		want    string
	}{
		{"missing context", BuildImageOptions{}, "archive is required"},
		{"missing archive", BuildImageOptions{ContainerBuildContext: &ContainerBuildContext{}}, "archive is required"},
		{"path and archive", BuildImageOptions{ContainerBuildContext: &ContainerBuildContext{Context: "directory", ContextArchive: validArchive}}, "mutually exclusive"},
		{"bad Dockerfile", BuildImageOptions{ContainerBuildContext: &ContainerBuildContext{ContextArchive: validArchive, Dockerfile: "../Dockerfile"}}, "invalid archive Dockerfile"},
		{"missing Dockerfile", BuildImageOptions{ContainerBuildContext: &ContainerBuildContext{ContextArchive: validArchive, Dockerfile: "missing"}}, "inspect staged Dockerfile"},
		{"Dockerfile directory", BuildImageOptions{ContainerBuildContext: &ContainerBuildContext{ContextArchive: validArchive, Dockerfile: "."}}, "not a regular file"},
		{"invalid base64", BuildImageOptions{ContainerBuildContext: &ContainerBuildContext{ContextArchive: &ContainerBuildContextArchive{RawContents: "not base64"}}}, "decode build context"},
		{"invalid tar", BuildImageOptions{ContainerBuildContext: &ContainerBuildContext{ContextArchive: &ContainerBuildContextArchive{RawContents: base64.StdEncoding.EncodeToString([]byte("not tar"))}}}, "read build context"},
		{"truncated file", BuildImageOptions{ContainerBuildContext: &ContainerBuildContext{ContextArchive: &ContainerBuildContextArchive{RawContents: base64.StdEncoding.EncodeToString(validContents[:514])}}}, "stage build context file"},
		{"bad hash", BuildImageOptions{ContainerBuildContext: &ContainerBuildContext{ContextArchive: &ContainerBuildContextArchive{Source: sourcePath, SHA256: "bad"}}}, "SHA256 mismatch"},
		{"source directory", BuildImageOptions{ContainerBuildContext: &ContainerBuildContext{ContextArchive: &ContainerBuildContextArchive{Source: t.TempDir(), SHA256: "bad"}}}, "not a regular file"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			tempDirectory := t.TempDir()
			called := false
			buildErr := buildImageFromArchiveDirectory(t.Context(), testCase.options,
				buildImageFunc(func(context.Context, BuildImageOptions) error {
					called = true
					return nil
				}), tempDirectory)
			require.ErrorContains(t, buildErr, testCase.want)
			require.False(t, called)
			entries, listErr := os.ReadDir(tempDirectory)
			require.NoError(t, listErr)
			require.Empty(t, entries)
		})
	}
}

// Verifies that builder failure and cancellation are propagated with workspace cleanup, including cancellation before staging begins.
func TestBuildImageFromArchiveDirectoryCleansFailureAndCancellation(t *testing.T) {
	t.Parallel()

	contents := buildArchiveTestContents(t, buildArchiveTestEntry{name: "Dockerfile", kind: tar.TypeReg, contents: "FROM scratch\n"})
	options := BuildImageOptions{ContainerBuildContext: &ContainerBuildContext{
		ContextArchive: &ContainerBuildContextArchive{RawContents: base64.StdEncoding.EncodeToString(contents)},
	}}
	for _, cancelBuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelBuild), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			expectedErr := errors.New("builder failed")
			var workspace string
			buildErr := buildImageFromArchiveDirectory(ctx, options, buildImageFunc(func(_ context.Context, staged BuildImageOptions) error {
				workspace = staged.Context
				if cancelBuild {
					cancel()
					return nil
				}
				return expectedErr
			}), t.TempDir())
			if cancelBuild {
				require.ErrorIs(t, buildErr, context.Canceled)
			} else {
				require.ErrorIs(t, buildErr, expectedErr)
			}
			require.NotEmpty(t, workspace)
			require.NoDirExists(t, workspace)
		})
	}

	canceledCtx, cancelBeforeBuild := context.WithCancel(t.Context())
	cancelBeforeBuild()
	tempDirectory := t.TempDir()
	buildErr := buildImageFromArchiveDirectory(canceledCtx, options, buildImageFunc(func(context.Context, BuildImageOptions) error {
		t.Error("builder must not run with a canceled context")
		return nil
	}), tempDirectory)
	require.ErrorIs(t, buildErr, context.Canceled)
	entries, listErr := os.ReadDir(tempDirectory)
	require.NoError(t, listErr)
	require.Empty(t, entries)
}
