//go:build !windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package lockfile

import (
	"os"

	"golang.org/x/sys/unix"
)

func doLock(f *os.File) error {
	// This is advisory lock that is associated with a file descriptor.
	// If the file descriptor is closed, or if the process owning the file descriptor exist,
	// the lock is released automatically.
	// Do "man 2 flock" and "man 2 close" for more info.
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func doUnlock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

func isAlreadyLockedError(err error) bool {
	return err == unix.EWOULDBLOCK
}
