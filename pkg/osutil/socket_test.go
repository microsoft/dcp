/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package osutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// shortTempDir creates a temp directory with a short path (independent of the
// test name) so that generated socket paths stay within MaxUnixSocketPathLen,
// which matters on Windows where t.TempDir() embeds the long test name.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "st")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestProgramSubfolderName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		exePath  string
		expected string
	}{
		{"bare name", "dcp", "dcp"},
		{"name with exe extension", "dcp.exe", "dcp"},
		{"unix-style path", filepath.Join(string(filepath.Separator), "usr", "bin", "dcp"), "dcp"},
		{"path with exe extension", filepath.Join("some", "dir", "dcp.exe"), "dcp"},
		{"multiple dots keeps all but last segment", "dcp.test.exe", "dcp.test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, programSubfolderName(tt.exePath))
		})
	}
}

func TestCreateRandomSocketPath_CreatesDirAndUniquePaths(t *testing.T) {
	t.Parallel()

	rootDir := shortTempDir(t)
	const prefix = "sock-"

	exe, exeErr := os.Executable()
	require.NoError(t, exeErr)
	expectedDir := filepath.Join(rootDir, programSubfolderName(exe))

	first, firstErr := CreateRandomSocketPath(rootDir, prefix)
	require.NoError(t, firstErr)
	require.Equal(t, expectedDir, filepath.Dir(first), "socket should live under <rootDir>/<programName>")
	require.True(t, strings.HasPrefix(filepath.Base(first), prefix), "socket name %q should start with prefix %q", filepath.Base(first), prefix)

	info, statErr := os.Stat(expectedDir)
	require.NoError(t, statErr, "the program subfolder should have been created")
	require.True(t, info.IsDir())

	second, secondErr := CreateRandomSocketPath(rootDir, prefix)
	require.NoError(t, secondErr)
	require.NotEqual(t, first, second, "successive calls must yield distinct paths")
}

func TestCreateRandomSocketPath_EmptyRootResolvesToUserCacheDir(t *testing.T) {
	t.Parallel()

	cacheDir, cacheErr := os.UserCacheDir()
	require.NoError(t, cacheErr)

	socketPath, err := CreateRandomSocketPath("", "sock-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	require.Truef(t, strings.HasPrefix(socketPath, cacheDir),
		"expected %q to be rooted under the user cache dir %q", socketPath, cacheDir)
}

func TestCreateRandomSocketPath_FailsWhenPathTooLong(t *testing.T) {
	t.Parallel()

	// A long root dir pushes the resulting socket path beyond MaxUnixSocketPathLen, while staying
	// short enough that the directory itself can still be created.
	rootDir := filepath.Join(t.TempDir(), strings.Repeat("a", 80))

	_, err := CreateRandomSocketPath(rootDir, "sock-")
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds the maximum")
}

func TestCreateRandomSocketPath_DirIsPrivateOnUnix(t *testing.T) {
	t.Parallel()

	if IsWindows() {
		t.Skip("directory permissions are not enforced on Windows")
	}

	rootDir := shortTempDir(t)
	socketPath, err := CreateRandomSocketPath(rootDir, "sock-")
	require.NoError(t, err)

	info, statErr := os.Stat(filepath.Dir(socketPath))
	require.NoError(t, statErr)
	require.Equal(t, PermissionOnlyOwnerReadWriteTraverse, info.Mode().Perm())
}
