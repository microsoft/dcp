/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"archive/tar"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that OpenTarArchive verifies file contents before exposing them, supports hash prefixes,
// and rejects a source changed after its expected hash was calculated.
func TestTarArchiveSourceVerification(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	source := filepath.Join(t.TempDir(), "archive.tar")
	contents := []byte("archive contents")
	require.NoError(t, usvc_io.WriteFile(source, contents, 0600))
	hash := sha256.Sum256(contents)
	reader, openErr := usvc_io.OpenTarArchive(ctx, source, fmt.Sprintf("sha256:%x", hash), "")
	require.NoError(t, openErr)
	actual, readErr := io.ReadAll(reader)
	require.NoError(t, readErr)
	require.NoError(t, reader.Close())
	require.Equal(t, contents, actual)
	require.NoError(t, usvc_io.WriteFile(source, []byte("changed"), 0600))
	_, changedErr := usvc_io.OpenTarArchive(ctx, source, fmt.Sprintf("%x", hash), "")
	require.ErrorContains(t, changedErr, "SHA256 mismatch")
}

// Verifies that TarWriter.CopyDirectory streams nested files and empty directories with
// preserved permissions and caller-selected ownership.
func TestTarDirectoryContents(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin", "empty"), 0755))
	require.NoError(t, usvc_io.WriteFile(filepath.Join(root, "bin", "adapter"), []byte("binary"), 0755))
	require.NoError(t, usvc_io.WriteFile(filepath.Join(root, "dependency.dll"), []byte("dependency"), 0644))
	writer := usvc_io.NewTarWriter()
	require.NoError(t, writer.CopyDirectory(ctx, root, "opt/adapter", 1000, 1001))
	buffer, finalizeErr := writer.Buffer()
	require.NoError(t, finalizeErr)
	reader := tar.NewReader(buffer)
	headers := make(map[string]*tar.Header)
	contents := make(map[string]string)
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		require.NoError(t, nextErr)
		payload, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		headers[header.Name], contents[header.Name] = header, string(payload)
		require.Equal(t, 1000, header.Uid)
		require.Equal(t, 1001, header.Gid)
	}
	require.Equal(t, "binary", contents["opt/adapter/bin/adapter"])
	require.Equal(t, "dependency", contents["opt/adapter/dependency.dll"])
	require.Equal(t, byte(tar.TypeDir), headers["opt/adapter/bin/empty"].Typeflag)
	if !osutil.IsWindows() {
		require.EqualValues(t, 0755, headers["opt/adapter/bin/adapter"].Mode)
	}
}

// Verifies that TarWriter.CopyDirectory preserves internal relative symlinks and rejects
// absolute links, escaping links, and an internal link chain that escapes the directory.
func TestTarDirectorySymlinks(t *testing.T) {
	if osutil.IsWindows() {
		t.Skip("creating symlinks requires Windows developer mode or elevated privileges")
	}
	t.Parallel()
	for _, scenario := range []string{"internal", "absolute", "escaping", "chained-escape"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "bin"), 0755))
			require.NoError(t, usvc_io.WriteFile(filepath.Join(root, "dependency"), []byte("library"), 0644))
			target := "../dependency"
			switch scenario {
			case "absolute":
				target = filepath.Join(root, "dependency")
			case "escaping":
				target = "../../outside"
			case "chained-escape":
				require.NoError(t, os.Symlink("../outside", filepath.Join(root, "dependency-link")))
				target = "../dependency-link"
			}
			require.NoError(t, os.Symlink(target, filepath.Join(root, "bin", "link")))
			writer := usvc_io.NewTarWriter()
			copyErr := writer.CopyDirectory(ctx, root, "adapter", 0, 0)
			if scenario != "internal" {
				require.ErrorContains(t, copyErr, "must be relative and remain within")
				require.NoError(t, writer.Close())
				return
			}
			require.NoError(t, copyErr)
			buffer, finalizeErr := writer.Buffer()
			require.NoError(t, finalizeErr)
			reader := tar.NewReader(buffer)
			found := false
			for {
				header, nextErr := reader.Next()
				if errors.Is(nextErr, io.EOF) {
					break
				}
				require.NoError(t, nextErr)
				if header.Name == "adapter/bin/link" {
					require.Equal(t, byte(tar.TypeSymlink), header.Typeflag)
					require.Equal(t, "../dependency", header.Linkname)
					found = true
				}
			}
			require.True(t, found)
		})
	}
}
