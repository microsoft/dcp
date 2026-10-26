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
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/pkg/osutil"
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

// Verifies that StartProcess rejects an injected incomplete identity, invokes the fake waitable's
// Abort method, and marks an Abort error as uncertain without returning a usable handle or wait callback.
func TestCustomCreationRejectsIncompleteIdentityAndInvokesAbort(t *testing.T) {
	t.Parallel()
	rollbackErr := errors.New("rollback denied")
	for _, tc := range []struct {
		name      string
		abortErr  error
		uncertain bool
	}{
		{"abort returns nil", nil, false},
		{"abort returns error", rollbackErr, true},
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

// Verifies that actOnProcess returns a cancellation error without invoking its action callback
// when the injected inspection callback cancels the context.
func TestProcessActionSkipsDispatchWhenInspectionCancelsContext(t *testing.T) {
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

// Verifies that rollbackProcessStart returns nil for injected nil or os.ErrProcessDone wait results
// despite a kill error, and otherwise preserves both injected errors with ErrProcessStartUncertain.
func TestRollbackProcessStartClassifiesInjectedKillAndWaitErrors(t *testing.T) {
	t.Parallel()

	killErr := errors.New("kill failed")
	waitErr := errors.New("wait failed")
	tests := []struct {
		name      string
		waitErr   error
		uncertain bool
	}{
		{"wait returns nil", nil, false},
		{"wait returns os.ErrProcessDone", os.ErrProcessDone, false},
		{"wait returns another error", waitErr, true},
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

// Verifies that rollbackProcessStart returns an uncertain-start error on cancellation while
// its blocked, injected wait callback remains able to finish after being released.
func TestRollbackProcessStartAllowsWaitCallbackToFinishAfterCancellation(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	rollbackCtx, rollbackCancel := context.WithCancel(testCtx)
	defer rollbackCancel()

	waitStarted := make(chan struct{})
	releaseWait := make(chan struct{})
	waitFinished := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseWait)
		})
	}
	t.Cleanup(release)

	killErr := errors.New("kill failed")
	rollbackResult := make(chan error, 1)
	go func() {
		rollbackResult <- rollbackProcessStart(
			rollbackCtx,
			func() error { return killErr },
			func() error {
				close(waitStarted)
				<-releaseWait
				close(waitFinished)
				return nil
			},
		)
	}()

	select {
	case <-waitStarted:
	case <-testCtx.Done():
		t.Fatal("rollback waiter did not start")
	}

	rollbackCancel()
	select {
	case rollbackErr := <-rollbackResult:
		require.ErrorIs(t, rollbackErr, ErrProcessStartUncertain)
		require.ErrorIs(t, rollbackErr, killErr)
		require.ErrorIs(t, rollbackErr, context.Canceled)
	case <-testCtx.Done():
		t.Fatal("rollback did not return after cancellation")
	}

	release()
	select {
	case <-waitFinished:
	case <-testCtx.Done():
		t.Fatal("retained rollback waiter did not finish")
	}
}

// Verifies that executor disposal cancels an in-flight process creation,
// waits for it to return, and rejects later process starts.
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

// Verifies that disposal signals cancellation to a noncooperative process creator,
// remains blocked until creation returns, and preserves both disposal and creation errors.
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

// Verifies that custom creation captures an owned identity without starting the original exec.Cmd
// and reports the exit code supplied by its Waitable.
func TestSysCreateProcess(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	cmd := delayCommandForTest(t, "--delay=60s")
	const overrideExitCode int32 = 77
	var capturedWaitable *customCreationWaitable
	creator := func(creationCtx context.Context, command *exec.Cmd) (ProcessHandle, Waitable, error) {
		createdProcess, createErr := os.StartProcess(command.Path, command.Args, &os.ProcAttr{
			Env:   command.Env,
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
			Sys:   command.SysProcAttr,
		})
		if createErr != nil {
			return ProcessHandle{Pid: UnknownPID}, nil, createErr
		}
		capturedWaitable = &customCreationWaitable{
			process:       createdProcess,
			overrideCode:  overrideExitCode,
			captureDoneCh: make(chan struct{}),
		}
		handle, handleErr := ProcessHandleFromProcess(createdProcess)
		if handleErr != nil {
			abortErr := capturedWaitable.Abort(context.WithoutCancel(creationCtx))
			return ProcessHandle{Pid: UnknownPID}, nil, errors.Join(handleErr, abortErr)
		}
		return handle, capturedWaitable, nil
	}
	exitResults := make(chan ProcessExitInfo, 1)
	handler := ProcessExitHandlerFunc(func(pid Pid_t, exitCode int32, exitErr error) {
		exitResults <- ProcessExitInfo{PID: pid, ExitCode: exitCode, Err: exitErr}
	})
	handle, startWaiting, startErr := executor.StartProcess(testCtx, cmd, handler, CreationFlagsNone, creator)
	require.NoError(t, startErr)
	require.NotNil(t, capturedWaitable)
	require.NoError(t, handle.Validate())
	require.Nil(t, cmd.Process, "custom creation must not start the original exec.Cmd")
	require.NoError(t, capturedWaitable.process.Kill())
	startWaiting()
	select {
	case exitResult, received := <-exitResults:
		require.True(t, received)
		require.Equal(t, handle.Pid, exitResult.PID)
		require.NoError(t, exitResult.Err)
		require.Equal(t, overrideExitCode, exitResult.ExitCode)
	case <-testCtx.Done():
		t.Fatal("custom process exit was not reported")
	}
}

type customCreationWaitable struct {
	process       *os.Process
	overrideCode  int32
	captured      int32
	captureDoneCh chan struct{}
}

func (waitable *customCreationWaitable) Wait() error {
	defer close(waitable.captureDoneCh)
	_, waitErr := waitable.process.Wait()
	if waitErr != nil {
		waitable.captured = UnknownExitCode
		return waitErr
	}
	waitable.captured = waitable.overrideCode
	return nil
}

func (*customCreationWaitable) Info() string               { return "custom creation fixture" }
func (*customCreationWaitable) Flags() ProcessCreationFlag { return CreationFlagsNone }
func (waitable *customCreationWaitable) Abort(ctx context.Context) error {
	return rollbackProcessStart(ctx, waitable.process.Kill, func() error {
		_, waitErr := waitable.process.Wait()
		return waitErr
	})
}

func (waitable *customCreationWaitable) ExitCode() int32 {
	<-waitable.captureDoneCh
	return waitable.captured
}

var _ Waitable = (*customCreationWaitable)(nil)
var _ ExitCodeSource = (*customCreationWaitable)(nil)

func delayCommandForTest(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	executableName := "delay"
	if runtime.GOOS == "windows" {
		executableName += ".exe"
	}
	toolRoot, toolRootErr := osutil.FindRootFor(osutil.FileTarget, ".toolbin", executableName)
	require.NoError(t, toolRootErr, "could not locate delay (did you run make test-prereqs?)")
	return exec.Command(filepath.Join(toolRoot, ".toolbin", executableName), args...)
}
