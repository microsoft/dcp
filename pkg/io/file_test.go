/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

func TestOpenFileReadOnlyReadsFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "read.txt")
	require.NoError(t, os.WriteFile(path, []byte("content"), osutil.PermissionOnlyOwnerReadWrite))

	file, openErr := usvc_io.OpenFileReadOnly(path)
	require.NoError(t, openErr)
	contents, readErr := io.ReadAll(file)
	require.NoError(t, readErr)
	require.NoError(t, file.Close())
	require.Equal(t, "content", string(contents))
}

func TestCreateOrTruncateExportFileUsesStandardPathSemantics(t *testing.T) {
	t.Parallel()

	workingDirectory, workingDirectoryErr := os.Getwd()
	require.NoError(t, workingDirectoryErr)
	tempDirectory, tempDirectoryErr := os.MkdirTemp(workingDirectory, "export-file-test-*")
	require.NoError(t, tempDirectoryErr)
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(tempDirectory))
	})
	absolutePath := filepath.Join(tempDirectory, "export.txt")
	path, relativePathErr := filepath.Rel(workingDirectory, absolutePath)
	require.NoError(t, relativePathErr)

	file, createErr := usvc_io.CreateOrTruncateExportFile(path, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, createErr)
	_, writeErr := file.WriteString("existing")
	require.NoError(t, writeErr)
	require.NoError(t, file.Close())

	replacement, replacementErr := usvc_io.CreateOrTruncateExportFile(path, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, replacementErr)
	_, replacementWriteErr := replacement.WriteString("replacement")
	require.NoError(t, replacementWriteErr)
	require.NoError(t, replacement.Close())

	contents, readErr := os.ReadFile(absolutePath)
	require.NoError(t, readErr)
	require.Equal(t, "replacement", string(contents))
}

func TestOpenOrCreateFileForAppendingPreservesExistingContents(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "append.txt")
	require.NoError(t, usvc_io.WriteFile(path, []byte("first"), osutil.PermissionOnlyOwnerReadWrite))

	file, openErr := usvc_io.OpenOrCreateFileForAppending(path, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, openErr)
	_, writeErr := file.Write([]byte("-second"))
	require.NoError(t, writeErr)
	require.NoError(t, file.Close())

	contents, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "first-second", string(contents))
}

func TestOpenOrCreateFileForAppendingCreatesFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "append-create.txt")
	file, openErr := usvc_io.OpenOrCreateFileForAppending(path, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, openErr)
	_, writeErr := file.Write([]byte("content"))
	require.NoError(t, writeErr)
	require.NoError(t, file.Close())

	contents, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "content", string(contents))
}

func TestEnsureFileDoesNotTruncateExistingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "existing.txt")
	require.NoError(t, usvc_io.WriteFile(path, []byte("existing"), osutil.PermissionOnlyOwnerReadWrite))

	file, openErr := usvc_io.EnsureFile(path, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, openErr)
	require.NoError(t, file.Close())

	contents, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "existing", string(contents))
}

func TestEnsureEmptyFileTruncatesExistingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "truncate.txt")
	require.NoError(t, usvc_io.WriteFile(path, []byte("existing"), osutil.PermissionOnlyOwnerReadWrite))

	file, openErr := usvc_io.EnsureEmptyFile(path, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, openErr)
	require.NoError(t, file.Close())

	contents, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Empty(t, contents)
}

func TestCreateNewFileRejectsExistingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "exclusive.txt")
	require.NoError(t, usvc_io.WriteFile(path, []byte("existing"), osutil.PermissionOnlyOwnerReadWrite))

	file, openErr := usvc_io.CreateNewFile(path, osutil.PermissionOnlyOwnerReadWrite)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.ErrorIs(t, openErr, os.ErrExist)
}

func TestCreateNewFileDoesNotFollowDanglingSymlink(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	targetPath := filepath.Join(tempDir, "target.txt")
	linkPath := filepath.Join(tempDir, "link.txt")
	if symlinkErr := os.Symlink(targetPath, linkPath); symlinkErr != nil {
		t.Skipf("symlink creation is unavailable: %v", symlinkErr)
	}

	file, openErr := usvc_io.CreateNewFile(linkPath, osutil.PermissionOnlyOwnerReadWrite)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.Error(t, openErr)
	require.NoFileExists(t, targetPath)
}
