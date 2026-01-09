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

func OpenTempFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return OpenFile(filepath.Join(DcpTempDir(), name), flag, perm)
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
