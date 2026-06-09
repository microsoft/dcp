/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package osutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/dcp/pkg/randdata"
)

const socketPathRandomSuffixLength = 8

// CreateRandomSocketPath builds a unique Unix domain socket path under a
// per-program subfolder, creating that subfolder (private to the current user)
// as necessary. The returned path is not bound; the caller is responsible for
// creating and removing the socket file.
//
// If rootDir is empty, the user's cache directory is used. The subfolder name
// is derived from the running executable's name with its extension stripped
// (so both "dcp" and "dcp.exe" map to "dcp"). socketNamePrefix is prepended to
// a random suffix to form the socket file name.
//
// The function fails if the resulting path would exceed MaxUnixSocketPathLen,
// since such a path cannot be used for an AF_UNIX socket.
func CreateRandomSocketPath(rootDir string, socketNamePrefix string) (string, error) {
	if rootDir == "" {
		cacheDir, cacheDirErr := os.UserCacheDir()
		if cacheDirErr != nil {
			return "", fmt.Errorf("failed to get user cache directory when creating a socket path: %w", cacheDirErr)
		}
		rootDir = cacheDir
	}

	exePath, exePathErr := os.Executable()
	if exePathErr != nil {
		return "", fmt.Errorf("failed to determine the current executable when creating a socket path: %w", exePathErr)
	}

	socketDir := filepath.Join(rootDir, programSubfolderName(exePath))
	if err := os.MkdirAll(socketDir, PermissionOnlyOwnerReadWriteTraverse); err != nil {
		return "", fmt.Errorf("failed to create directory for socket: %w", err)
	}

	// On Windows the user cache directory always exists and is always private to the user,
	// but on Unix-like systems, we need to ensure the directory is private.
	if !IsWindows() {
		info, infoErr := os.Stat(socketDir)
		if infoErr != nil {
			return "", fmt.Errorf("failed to check permissions on the socket directory: %w", infoErr)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("socket path %s is not a directory", socketDir)
		}
		if info.Mode().Perm() != PermissionOnlyOwnerReadWriteTraverse {
			return "", fmt.Errorf("socket directory %s is not private to the user", socketDir)
		}
	}

	suffix, suffixErr := randdata.MakeRandomString(socketPathRandomSuffixLength)
	if suffixErr != nil {
		return "", fmt.Errorf("failed to create random string for socket path suffix: %w", suffixErr)
	}

	socketPath := filepath.Join(socketDir, socketNamePrefix+string(suffix))
	if len(socketPath) > MaxUnixSocketPathLen {
		return "", fmt.Errorf("socket path %s is %d characters long, which exceeds the maximum of %d for a Unix domain socket", socketPath, len(socketPath), MaxUnixSocketPathLen)
	}

	return socketPath, nil
}

// programSubfolderName returns the base name of the executable at exePath with
// any file extension removed (e.g. "dcp.exe" and "/usr/bin/dcp" both yield "dcp").
func programSubfolderName(exePath string) string {
	base := filepath.Base(exePath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
