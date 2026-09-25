/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/pkg/testutil"
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
				func(context.Context, *exec.Cmd) (ProcessHandle, Waitable, error) {
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

func TestRollbackProcessStartUsesConfirmedExit(t *testing.T) {
	t.Parallel()

	killErr := errors.New("kill failed")
	waitErr := errors.New("wait failed")
	tests := []struct {
		name      string
		waitErr   error
		uncertain bool
	}{
		{"successful wait", nil, false},
		{"process already exited", os.ErrProcessDone, false},
		{"wait failed", waitErr, true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			rollbackErr := rollbackProcessStart(
				context.Background(),
				func() error { return killErr },
				func() error { return testCase.waitErr },
			)
			require.Equal(t, testCase.uncertain, errors.Is(rollbackErr, ErrProcessStartUncertain))
			if testCase.uncertain {
				require.ErrorIs(t, rollbackErr, killErr)
				require.ErrorIs(t, rollbackErr, waitErr)
			} else {
				require.NoError(t, rollbackErr)
			}
		})
	}
}

func TestDisposeCancelsInFlightProcessCreation(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	executor := NewOSExecutor(logr.Discard())
	creationStarted := make(chan struct{})
	startResult := make(chan error, 1)
	go func() {
		_, _, startErr := executor.StartProcess(
			testCtx,
			exec.Command("unused"),
			nil,
			CreationFlagsNone,
			func(startCtx context.Context, _ *exec.Cmd) (ProcessHandle, Waitable, error) {
				close(creationStarted)
				<-startCtx.Done()
				return ProcessHandle{Pid: UnknownPID}, nil, context.Cause(startCtx)
			},
		)
		startResult <- startErr
	}()

	select {
	case <-creationStarted:
	case <-testCtx.Done():
		t.Fatal("process creation did not start")
	}

	disposeDone := make(chan struct{})
	go func() {
		executor.Dispose()
		close(disposeDone)
	}()

	select {
	case startErr := <-startResult:
		require.ErrorIs(t, startErr, ErrDisposed)
	case <-testCtx.Done():
		t.Fatal("process creation was not canceled by disposal")
	}

	select {
	case <-disposeDone:
	case <-testCtx.Done():
		t.Fatal("executor disposal did not complete")
	}

	_, _, rejectedErr := executor.StartProcess(
		testCtx,
		exec.Command("unused"),
		nil,
		CreationFlagsNone,
		func(context.Context, *exec.Cmd) (ProcessHandle, Waitable, error) {
			require.Fail(t, "disposed executor invoked process creation")
			return ProcessHandle{Pid: UnknownPID}, nil, nil
		},
	)
	require.ErrorIs(t, rejectedErr, ErrDisposed)
}

func TestDisposeWaitsForNonCooperativeProcessCreation(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	executor := NewOSExecutor(logr.Discard())
	creationStarted := make(chan struct{})
	creationCanceled := make(chan struct{})
	releaseCreation := make(chan struct{})
	createErr := errors.New("process creation failed")
	startResult := make(chan error, 1)
	go func() {
		_, _, startErr := executor.StartProcess(
			testCtx,
			exec.Command("unused"),
			nil,
			CreationFlagsNone,
			func(startCtx context.Context, _ *exec.Cmd) (ProcessHandle, Waitable, error) {
				close(creationStarted)
				go func() {
					<-startCtx.Done()
					close(creationCanceled)
				}()
				<-releaseCreation
				return ProcessHandle{Pid: UnknownPID}, nil, createErr
			},
		)
		startResult <- startErr
	}()

	select {
	case <-creationStarted:
	case <-testCtx.Done():
		t.Fatal("process creation did not start")
	}

	disposeDone := make(chan struct{})
	go func() {
		executor.Dispose()
		close(disposeDone)
	}()

	select {
	case <-creationCanceled:
	case <-testCtx.Done():
		t.Fatal("disposal did not cancel the process creation context")
	}

	select {
	case <-disposeDone:
		t.Fatal("disposal completed before admitted process creation returned")
	default:
	}

	close(releaseCreation)

	select {
	case startErr := <-startResult:
		require.ErrorIs(t, startErr, ErrDisposed)
		require.ErrorIs(t, startErr, createErr)
	case <-testCtx.Done():
		t.Fatal("process creation did not return")
	}

	select {
	case <-disposeDone:
	case <-testCtx.Done():
		t.Fatal("executor disposal did not complete")
	}
}
