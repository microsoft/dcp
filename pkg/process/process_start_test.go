/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

type startupWaitable struct {
	aborted  bool
	abortErr error
}

func (w *startupWaitable) Wait() error                { return nil }
func (w *startupWaitable) Info() string               { return "startup fixture" }
func (w *startupWaitable) Flags() ProcessCreationFlag { return CreationFlagsNone }
func (w *startupWaitable) Abort(context.Context) error {
	w.aborted = true
	return w.abortErr
}

func TestCustomCreationIncompleteIdentityIsRolledBack(t *testing.T) {
	t.Parallel()
	rollbackErr := errors.New("rollback denied")
	for _, tc := range []struct {
		name      string
		abortErr  error
		uncertain bool
	}{
		{"confirmed cleanup", nil, false},
		{"failed cleanup", rollbackErr, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			executor := NewOSExecutor(logr.Discard())
			defer executor.Dispose()
			waitable := &startupWaitable{abortErr: tc.abortErr}
			handle, startWait, startErr := executor.StartProcess(context.Background(), exec.Command("unused"), nil, CreationFlagsNone,
				func(*exec.Cmd) (ProcessHandle, Waitable, error) {
					return NewHandle(1234, time.Time{}), waitable, nil
				})
			require.ErrorIs(t, startErr, ErrInvalidProcessHandle)
			require.Equal(t, tc.uncertain, errors.Is(startErr, ErrProcessStartUncertain))
			require.True(t, waitable.aborted)
			require.Equal(t, UnknownPID, handle.Pid)
			require.Nil(t, startWait)
		})
	}
}

func TestProcessActionCancellationDuringInspection(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle := treeProcess(10, 0, 10).handle
	actionCalled := false
	actionErr := actOnProcess(ctx, handle, func() (ProcessHandle, error) {
		cancel()
		return handle, nil
	}, func() error {
		actionCalled = true
		return nil
	})
	require.ErrorIs(t, actionErr, context.Canceled)
	require.False(t, actionCalled)
}
