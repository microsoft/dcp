/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

func TestEnsureRestrictedDirectoryRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional privileges on Windows")
	}

	targetDir := t.TempDir()
	linkPath := filepath.Join(t.TempDir(), "restricted-dir")
	require.NoError(t, os.Symlink(targetDir, linkPath))

	ensureErr := usvc_io.EnsureRestrictedDirectory(linkPath, osutil.PermissionOnlyOwnerReadWriteTraverse)

	require.Error(t, ensureErr)
	require.Contains(t, ensureErr.Error(), "is a symlink")
}

func TestEnsureRestrictedDirectoryRestrictsExistingDirectory(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "restricted-dir")
	require.NoError(t, os.Mkdir(outputDir, osutil.PermissionDirectoryOthersRead))

	require.NoError(t, usvc_io.EnsureRestrictedDirectory(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse))

	info, statErr := os.Lstat(outputDir)
	require.NoError(t, statErr)
	require.True(t, info.IsDir())
	if runtime.GOOS != "windows" {
		require.Equal(t, osutil.PermissionOnlyOwnerReadWriteTraverse, info.Mode().Perm())
	}
}

func TestValidateRestrictedDirectoryRejectsPermissiveDirectoryWithoutRestricting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory permissions are not represented by Unix mode bits")
	}

	outputDir := filepath.Join(t.TempDir(), "restricted-dir")
	require.NoError(t, os.Mkdir(outputDir, osutil.PermissionDirectoryOthersRead))
	require.NoError(t, os.Chmod(outputDir, osutil.PermissionDirectoryOthersRead))

	validateErr := usvc_io.ValidateRestrictedDirectory(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse)

	require.Error(t, validateErr)
	require.Contains(t, validateErr.Error(), "invalid permissions")
	info, statErr := os.Lstat(outputDir)
	require.NoError(t, statErr)
	require.Equal(t, osutil.PermissionDirectoryOthersRead, info.Mode().Perm())
}
