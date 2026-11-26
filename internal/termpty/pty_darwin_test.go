//go:build darwin

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"context"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that openPTYMaster redrives a wrapped Darwin PTY allocation race and returns the successful file.
func TestOpenPTYMaster_RedrivesAllocationRace(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, internalTestTimeout)
	defer cancel()

	expected := &os.File{}
	attempts := 0
	file, openErr := openPTYMaster(ctx, func() (*os.File, error) {
		attempts++
		if attempts <= 2 {
			return nil, &os.PathError{Op: "open", Path: "/dev/ptmx", Err: ^syscall.Errno(5)}
		}
		return expected, nil
	})
	require.NoError(t, openErr)
	require.Same(t, expected, file)
	require.Equal(t, 3, attempts)
}

// Verifies that openPTYMaster promptly returns non-race errors, including PTY exhaustion and permissions failures.
func TestOpenPTYMaster_DoesNotRetryOtherErrors(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{syscall.ENXIO, syscall.EACCES, syscall.EMFILE, syscall.EINTR} {
		t.Run(cause.Error(), func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, internalTestTimeout)
			defer cancel()
			expectedErr := &os.PathError{Op: "open", Path: "/dev/ptmx", Err: cause}
			attempts := 0
			file, openErr := openPTYMaster(ctx, func() (*os.File, error) {
				attempts++
				return nil, expectedErr
			})
			require.Nil(t, file)
			require.ErrorIs(t, openErr, expectedErr)
			require.Equal(t, 1, attempts)
		})
	}
}

// Verifies that openPTYMaster bounds repeated Darwin redrive failures and preserves their cause.
func TestOpenPTYMaster_BoundsPersistentAllocationRace(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, internalTestTimeout)
	defer cancel()
	expectedErr := &os.PathError{Op: "open", Path: "/dev/ptmx", Err: ^syscall.Errno(5)}
	attempts := 0
	file, openErr := openPTYMaster(ctx, func() (*os.File, error) {
		attempts++
		return nil, expectedErr
	})
	require.Nil(t, file)
	require.ErrorIs(t, openErr, expectedErr)
	require.Greater(t, attempts, 1)
	require.LessOrEqual(t, attempts, 10)
	require.NoError(t, ctx.Err(), "retry budget should expire before the caller's lifetime")
}

// Verifies that openPTYMaster stops redriving when cancelled during an open and never opens for an already cancelled caller.
func TestOpenPTYMaster_RespectsCancellation(t *testing.T) {
	t.Parallel()
	for _, cancelBeforeOpen := range []bool{false, true} {
		t.Run(map[bool]string{false: "during open", true: "before open"}[cancelBeforeOpen], func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, internalTestTimeout)
			defer cancel()
			if cancelBeforeOpen {
				cancel()
			}
			attempts := 0
			file, openErr := openPTYMaster(ctx, func() (*os.File, error) {
				attempts++
				cancel()
				return nil, &os.PathError{Op: "open", Path: "/dev/ptmx", Err: ^syscall.Errno(5)}
			})
			require.Nil(t, file)
			require.ErrorIs(t, openErr, context.Canceled)
			if cancelBeforeOpen {
				require.Zero(t, attempts)
			} else {
				require.Equal(t, 1, attempts)
				require.ErrorIs(t, openErr, ^syscall.Errno(5))
			}
		})
	}
}

// Verifies that redrivePTYMasterOpen stops waiting when cancelled during a pending retry delay,
// reports both the cancellation and the failed open, and makes no further open attempts.
func TestRedrivePTYMasterOpen_CancellationDuringRetryDelay(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, internalTestTimeout)
	defer cancel()

	policy := &delayScheduledBackOff{scheduled: make(chan struct{})}
	go func() {
		select {
		case <-policy.scheduled:
			cancel()
		case <-ctx.Done():
		}
	}()

	expectedErr := &os.PathError{Op: "open", Path: "/dev/ptmx", Err: darwinRedriveOpen}
	attempts := 0
	file, openErr := redrivePTYMasterOpen(ctx, policy, func() (*os.File, error) {
		attempts++
		return nil, expectedErr
	})
	require.Nil(t, file)
	require.ErrorIs(t, openErr, context.Canceled)
	require.ErrorIs(t, openErr, expectedErr)
	require.Equal(t, 1, attempts)
}

// delayScheduledBackOff signals when a retry delay is scheduled and keeps that delay pending until cancellation.
type delayScheduledBackOff struct {
	scheduled chan struct{}
	once      sync.Once
}

func (b *delayScheduledBackOff) NextBackOff() time.Duration {
	b.once.Do(func() { close(b.scheduled) })
	return time.Hour
}

func (b *delayScheduledBackOff) Reset() {}
