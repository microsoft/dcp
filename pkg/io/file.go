/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"errors"
	"os"
)

var (
	// ErrRestrictedFilePolicy identifies an elevated Windows restricted-file policy rejection.
	ErrRestrictedFilePolicy = errors.New("restricted file policy")
	// ErrRestrictedFileUnsupportedPath identifies a path form unsupported by the restricted-file policy.
	ErrRestrictedFileUnsupportedPath = errors.New("unsupported restricted file path")
	// ErrRestrictedFileNonFixedDrive identifies storage that is not a fixed local drive.
	ErrRestrictedFileNonFixedDrive = errors.New("restricted file drive is not fixed local storage")
	// ErrRestrictedFileNoPersistentACLs identifies storage that cannot persist Windows ACLs.
	ErrRestrictedFileNoPersistentACLs = errors.New("restricted file system does not support persistent ACLs")
	// ErrRestrictedFileReparsePoint identifies a path that traverses or targets a reparse point.
	ErrRestrictedFileReparsePoint = errors.New("restricted file path contains a reparse point")
	// ErrRestrictedFileInvalidSecurity identifies an existing file with an unacceptable owner, DACL, or link count.
	ErrRestrictedFileInvalidSecurity = errors.New("restricted file security descriptor is invalid")
)

// OpenFileReadOnly opens an existing file for reading using standard operating-system path semantics.
func OpenFileReadOnly(name string) (*os.File, error) {
	return os.Open(name)
}

// CreateOrTruncateExportFile creates or truncates a user-selected export destination using
// standard operating-system path semantics. It does not provide the elevated Windows
// restricted-file guarantee and must not be used for DCP-managed sensitive files.
func CreateOrTruncateExportFile(name string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
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
