/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// lockfile package provides an interface for working with files that can be locked and unlocked.
package lockfile

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

// Represents a file that can be locked and unlocked.
// I/O operations are not allowed on an unlocked Lockfile.
// Lockfile is NOT goroutine-safe.
type Lockfile struct {
	path   string
	file   *os.File
	locked bool
}

const (
	DefaultLockRetryInterval = 20 * time.Millisecond
)

var (
	ErrUnlocked    = errors.New("Lockfile has not been locked, I/O operations are not allowed")
	ErrNeedAbsPath = errors.New("Lockfiles must be created using absolute path")
)

// Creates a new Lockfile instance for given path. The actual file is not created or locked yet.
// The path must be an absolute path.
func NewLockfile(path string) (*Lockfile, error) {
	if len(path) == 0 || !filepath.IsAbs(path) {
		return nil, ErrNeedAbsPath
	}

	return &Lockfile{
		path: path,
	}, nil
}

func (l *Lockfile) Path() string {
	return l.path
}

func (l *Lockfile) Locked() bool {
	return l.locked
}

func (l *Lockfile) Close() error {
	unlockErr := l.Unlock()
	if l.file != nil {
		closeErr := l.file.Close()
		l.file = nil
		return errors.Join(unlockErr, closeErr)
	} else {
		return unlockErr
	}
}

func (l *Lockfile) TryLock(ctx context.Context, retryInterval time.Duration) error {
	if l.locked {
		return nil
	}

	if retryInterval <= 0 {
		retryInterval = DefaultLockRetryInterval
	}
	retryInterval = wait.Jitter(retryInterval, 0.1)

	return wait.PollUntilContextCancel(ctx, retryInterval, true /* poll immediately */, func(ctx context.Context) (bool, error) {
		file := l.file

		if file == nil {
			var openErr error
			file, openErr = usvc_io.OpenFile(l.path, os.O_CREATE|os.O_RDWR, osutil.PermissionOnlyOwnerReadWrite)
			if openErr != nil {
				return false, openErr
			}

			l.file = file
		}

		lockErr := doLock(l.file)
		if lockErr == nil {
			l.locked = true
			return true, nil
		}
		if isAlreadyLockedError(lockErr) {
			// Expected error if the file is already locked
			return false, nil
		} else {
			return false, lockErr
		}
	})
}

func (l *Lockfile) Unlock() error {
	if l.file == nil || !l.locked {
		return nil
	}

	// Clear the locked flag regardless of the result of unlocking.
	// This ensures that subsequent I/O operations will fail unless the client tries to lock the file again.
	l.locked = false

	unlockErr := doUnlock(l.file)
	return unlockErr
}

func (l *Lockfile) Read(p []byte) (int, error) {
	if l.file == nil || !l.locked {
		return 0, ErrUnlocked
	}
	return l.file.Read(p)
}

func (l *Lockfile) Write(p []byte) (int, error) {
	if l.file == nil || !l.locked {
		return 0, ErrUnlocked
	}
	return l.file.Write(p)
}

func (l *Lockfile) Seek(offset int64, whence int) (int64, error) {
	if l.file == nil || !l.locked {
		return 0, ErrUnlocked
	}
	return l.file.Seek(offset, whence)
}

func (l *Lockfile) Truncate(size int64) error {
	if l.file == nil || !l.locked {
		return ErrUnlocked
	}
	return l.file.Truncate(size)
}

var _ io.ReadWriteCloser = (*Lockfile)(nil)
var _ io.Seeker = (*Lockfile)(nil)
