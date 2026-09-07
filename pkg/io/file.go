/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"errors"
	"os"
)

// OpenExternalFileForReading opens a caller-supplied file using standard operating-system path semantics.
// Use this only for inputs that are intentionally allowed to be outside DCP-managed storage.
func OpenExternalFileForReading(name string) (*os.File, error) {
	return os.Open(name)
}

// OpenFileForReading opens an existing DCP-managed file for reading.
// Elevated Windows opens apply the same path, ownership, ACL, and hard-link validation as write operations.
func OpenFileForReading(name string, perm os.FileMode) (*os.File, error) {
	return openFileForReading(name, perm)
}

// CreateNewFile creates a new read-write file and fails if the path already exists.
// Elevated Windows creation accepts absolute paths on fixed local drives only, rejects reparse points, and applies the restricted file ACL atomically.
func CreateNewFile(name string, perm os.FileMode) (*os.File, error) {
	return createNewFile(name, perm)
}

// EnsureFile opens an existing read-write file or creates it when it does not exist.
// Elevated Windows opens accept absolute paths on fixed local drives only and reject reparse points, hard links, and files without the restricted file ACL.
func EnsureFile(name string, perm os.FileMode) (*os.File, error) {
	return ensureFile(name, perm)
}

// EnsureEmptyFile opens or creates a read-write file and ensures that its contents are empty.
// Elevated Windows validates an existing file before truncating it.
func EnsureEmptyFile(name string, perm os.FileMode) (*os.File, error) {
	return ensureEmptyFile(name, perm)
}

// OpenOrCreateFileForAppending opens or creates a file for sequential writes at the end of the file.
// The returned type intentionally excludes positional writes that are incompatible with append-only access.
func OpenOrCreateFileForAppending(name string, perm os.FileMode) (*AppendFile, error) {
	return openOrCreateFileForAppending(name, perm)
}

// WriteFile writes data to a file, creating it or replacing its existing contents.
func WriteFile(name string, data []byte, perm os.FileMode) error {
	file, openErr := ensureEmptyFileForWriting(name, perm)
	if openErr != nil {
		return openErr
	}

	_, writeErr := file.Write(data)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}
