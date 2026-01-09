/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package lockfile

import (
	"math"
	"os"

	"golang.org/x/sys/windows"
)

func doLock(f *os.File) error {
	// This puts an exclusive lock on entire file (using maximum possible range).
	// If a process closes a file with outstanding locks, or terminates while holding a lock,
	// the lock is automatically released, BUT that happens asynchronously.
	// So it is always better to unlock files explicitly and timely.
	// For more info on LockFileEx() API see https://learn.microsoft.com/en-us/windows/win32/fileio/locking-and-unlocking-byte-ranges-in-files
	var overlapped windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,              // reserved, must be zero
		math.MaxUint32, // number of bytes to lock,
		math.MaxUint32, // number of bytes to lock, high-order DWORD
		&overlapped,
	)
}

func doUnlock(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,              // reserved, must be zero
		math.MaxUint32, // number of bytes to unlock
		math.MaxUint32, // number of bytes to unlock, high-order DWORD
		&overlapped,
	)
}

func isAlreadyLockedError(err error) bool {
	return err == windows.ERROR_LOCK_VIOLATION
}
