/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"os"
	"path/filepath"
	"sync"
)

var (
	DcpTempDir func() string
)

func CreateTempFolder(name string, perm os.FileMode) (string, error) {
	err := os.MkdirAll(filepath.Join(DcpTempDir(), name), perm)
	if err != nil {
		return "", err
	}

	return filepath.Join(DcpTempDir(), name), nil
}

// CreateNewTempFile creates a new read-write file in the DCP temporary directory.
func CreateNewTempFile(name string, perm os.FileMode) (*os.File, error) {
	return CreateNewFile(filepath.Join(DcpTempDir(), name), perm)
}

// EnsureEmptyTempFile opens or creates an empty read-write file in the DCP temporary directory.
func EnsureEmptyTempFile(name string, perm os.FileMode) (*os.File, error) {
	return EnsureEmptyFile(filepath.Join(DcpTempDir(), name), perm)
}

// OpenOrCreateTempFileForAppending opens or creates a file in the DCP temporary directory for appending.
func OpenOrCreateTempFileForAppending(name string, perm os.FileMode) (*AppendFile, error) {
	return OpenOrCreateFileForAppending(filepath.Join(DcpTempDir(), name), perm)
}

func init() {
	DcpTempDir = sync.OnceValue(func() string {
		sessionDir := DcpSessionDir()
		if sessionDir != "" {
			return sessionDir
		} else {
			return os.TempDir()
		}
	})
}
