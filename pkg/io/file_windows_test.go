//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/microsoft/dcp/pkg/osutil"
)

func TestRestrictedFileWritableAccessDoesNotRequestWriteDAC(t *testing.T) {
	t.Parallel()

	for _, mode := range []restrictedFileOpenMode{
		restrictedFileCreateNew,
		restrictedFileOpenOrCreate,
		restrictedFileCreateOrTruncate,
		restrictedFileWriteOrTruncate,
		restrictedFileAppend,
	} {
		require.Zero(t, restrictedFileAccess(mode)&uint32(windows.WRITE_DAC))
	}
}

func TestRestrictedFileRejectsUntrustedExistingFile(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	path := filepath.Join(t.TempDir(), "untrusted.txt")
	require.NoError(t, os.WriteFile(path, []byte("existing"), 0600))

	file, openErr := openRestrictedFile(
		path,
		restrictedFileCreateOrTruncate,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.Error(t, openErr)

	contents, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "existing", string(contents))
}

func TestRestrictedReadRejectsUntrustedExistingFile(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	path := filepath.Join(t.TempDir(), "untrusted-read.txt")
	require.NoError(t, os.WriteFile(path, []byte("existing"), 0600))

	file, openErr := openRestrictedFile(
		path,
		restrictedFileRead,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.Error(t, openErr)
}

func TestRestrictedReadOpensManagedFileWhileWriterIsOpen(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	path := filepath.Join(t.TempDir(), "managed-read.txt")
	writer, createErr := openRestrictedFile(
		path,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, createErr)
	defer writer.Close()
	_, writeErr := writer.WriteString("content")
	require.NoError(t, writeErr)
	require.NoError(t, writer.Sync())

	reader, openErr := openRestrictedFile(
		path,
		restrictedFileRead,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, openErr)
	contents, readErr := io.ReadAll(reader)
	require.NoError(t, readErr)
	require.NoError(t, reader.Close())
	require.Equal(t, "content", string(contents))
}

func TestRestrictedFileCreateRejectsExistingFile(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	path := filepath.Join(t.TempDir(), "existing.txt")
	file, createErr := openRestrictedFile(
		path,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, createErr)
	require.NoError(t, file.Close())

	secondFile, secondCreateErr := openRestrictedFile(
		path,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if secondFile != nil {
		require.NoError(t, secondFile.Close())
	}
	require.ErrorIs(t, secondCreateErr, os.ErrExist)
}

func TestRestrictedFileRejectsReparsePathBeforeTruncate(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	tempDir := t.TempDir()
	targetPath := filepath.Join(tempDir, "target.txt")
	linkPath := filepath.Join(tempDir, "link.txt")
	require.NoError(t, os.WriteFile(targetPath, []byte("existing"), 0600))
	if symlinkErr := os.Symlink(targetPath, linkPath); symlinkErr != nil {
		t.Skipf("symlink creation is unavailable: %v", symlinkErr)
	}

	file, openErr := openRestrictedFile(
		linkPath,
		restrictedFileCreateOrTruncate,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.Error(t, openErr)

	contents, readErr := os.ReadFile(targetPath)
	require.NoError(t, readErr)
	require.Equal(t, "existing", string(contents))
}

func TestRestrictedFileRejectsAncestorReparsePoint(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	tempDir := t.TempDir()
	targetDir := filepath.Join(tempDir, "target")
	require.NoError(t, os.Mkdir(targetDir, 0700))
	linkDir := filepath.Join(tempDir, "link")
	if symlinkErr := os.Symlink(targetDir, linkDir); symlinkErr != nil {
		t.Skipf("symlink creation is unavailable: %v", symlinkErr)
	}

	file, openErr := openRestrictedFile(
		filepath.Join(linkDir, "created.txt"),
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.Error(t, openErr)
	require.NoFileExists(t, filepath.Join(targetDir, "created.txt"))
}

func TestRestrictedFileRejectsHardLinks(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	targetPath := filepath.Join(t.TempDir(), "target.txt")
	target, targetErr := openRestrictedFile(
		targetPath,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, targetErr)
	require.NoError(t, target.Close())

	linkPath := filepath.Join(filepath.Dir(targetPath), "link.txt")
	require.NoError(t, os.Link(targetPath, linkPath))
	file, openErr := openRestrictedFile(
		linkPath,
		restrictedFileAppend,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.ErrorContains(t, openErr, "hard links")
}

func TestRestrictedFileRejectsUnsupportedPathForms(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	testCases := []struct {
		name string
		path string
	}{
		{name: "relative path", path: "file.txt"},
		{name: "UNC path", path: `\\server\share\file.txt`},
		{name: "extended path", path: `\\?\C:\file.txt`},
		{name: "alternate data stream", path: filepath.Join(t.TempDir(), "file.txt:stream")},
		{name: "trailing period", path: filepath.Join(t.TempDir(), "file.txt.")},
		{name: "trailing space", path: filepath.Join(t.TempDir(), "file.txt ")},
		{name: "reserved device", path: filepath.Join(t.TempDir(), "NUL.txt")},
		{name: "superscript reserved device", path: filepath.Join(t.TempDir(), "COM\u00B9.txt")},
		{name: "wildcard", path: filepath.Join(t.TempDir(), "file?.txt")},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			file, openErr := openRestrictedFile(
				testCase.path,
				restrictedFileCreateNew,
				osutil.PermissionOnlyOwnerReadWrite,
				principals,
			)
			if file != nil {
				require.NoError(t, file.Close())
			}
			require.Error(t, openErr)
		})
	}
}

func TestRestrictedFileAppendUsesNativeAppendAccess(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	path := filepath.Join(t.TempDir(), "append.txt")
	file, createErr := openRestrictedFile(
		path,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, createErr)
	_, writeErr := file.WriteString("first")
	require.NoError(t, writeErr)
	require.NoError(t, file.Close())

	appendFile, appendErr := openRestrictedFile(
		path,
		restrictedFileAppend,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, appendErr)
	_, seekErr := appendFile.Seek(0, io.SeekStart)
	require.NoError(t, seekErr)
	_, appendWriteErr := appendFile.WriteString("-second")
	require.NoError(t, appendWriteErr)
	require.NoError(t, appendFile.Close())

	contents, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "first-second", string(contents))
}

func TestRestrictedFileValidatesBeforeTruncate(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	path := filepath.Join(t.TempDir(), "truncate.txt")
	file, createErr := openRestrictedFile(
		path,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, createErr)
	_, writeErr := file.WriteString("existing")
	require.NoError(t, writeErr)
	require.NoError(t, file.Close())

	truncated, truncateErr := openRestrictedFile(
		path,
		restrictedFileCreateOrTruncate,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, truncateErr)
	require.NoError(t, truncated.Close())

	contents, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Empty(t, contents)
}

func restrictedTestPrincipals(t *testing.T) restrictedDirectoryPrincipals {
	t.Helper()

	principals, principalErr := currentRestrictedDirectoryPrincipals()
	require.NoError(t, principalErr)
	principals.admins = principals.tokenUser
	return principals
}
