//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"os"
)

func openFileForReading(name string, _ os.FileMode) (*os.File, error) {
	return os.Open(name)
}

func createNewFile(name string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
}

func ensureFile(name string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, os.O_RDWR|os.O_CREATE, perm)
}

func ensureEmptyFile(name string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
}

func ensureEmptyFileForWriting(name string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
}

func openOrCreateFileForAppending(name string, perm os.FileMode) (*AppendFile, error) {
	file, openErr := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, perm)
	if openErr != nil {
		return nil, openErr
	}
	return newAppendFile(file), nil
}
