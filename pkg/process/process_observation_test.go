/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process_test

import (
	"context"
	"os/exec"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	internaltest "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that concurrent exit observers share a started process's wait and preserve
// the owner's nonzero exit-code callback.
func TestExecutorExitObserversPreserveExitCode(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	tool, toolErr := internaltest.GetTestToolPath("delay")
	require.NoError(t, toolErr)
	executor := process.NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	exited := make(chan int32, 1)
	handle, startWait, startErr := executor.StartProcess(ctx, exec.Command(tool, "--delay=100ms", "--exit-code=7"),
		process.ProcessExitHandlerFunc(func(_ process.Pid_t, code int32, _ error) { exited <- code }),
		process.CreationFlagsNone, nil)
	require.NoError(t, startErr)
	startWait()
	var observers sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		observers.Add(1)
		go func() {
			defer observers.Done()
			results <- executor.WaitForProcessExit(ctx, handle)
		}()
	}
	observers.Wait()
	close(results)
	for waitErr := range results {
		require.NoError(t, waitErr)
	}
	select {
	case code := <-exited:
		require.EqualValues(t, 7, code)
	case <-ctx.Done():
		t.Fatal("exit observers prevented the owner's exit-code callback")
	}
}

// Verifies that exit observation handles owned and adopted processes without competing with
// the owner's wait or exit callback, and cancellation never stops the observed process.
func TestExecutorWaitForProcessExit(t *testing.T) {
	t.Parallel()
	for _, adopted := range []bool{false, true} {
		name := "started"
		if adopted {
			name = "adopted"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			tool, toolErr := internaltest.GetTestToolPath("delay")
			require.NoError(t, toolErr)
			owner := process.NewOSExecutor(logr.Discard())
			defer owner.Dispose()
			executor := owner
			if adopted {
				executor = process.NewOSExecutor(logr.Discard())
				defer executor.Dispose()
			}
			exited := make(chan int32, 1)
			handle, startWait, startErr := owner.StartProcess(ctx, exec.Command(tool, "--delay=30s"),
				process.ProcessExitHandlerFunc(func(_ process.Pid_t, code int32, _ error) { exited <- code }),
				process.CreationFlagsNone, nil)
			require.NoError(t, startErr)
			startWait()
			watchCtx, cancelWatch := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- executor.WaitForProcessExit(watchCtx, handle) }()
			cancelWatch()
			select {
			case waitErr := <-done:
				require.ErrorIs(t, waitErr, context.Canceled)
			case <-ctx.Done():
				t.Fatal("cancellation did not release process observation")
			}
			require.NoError(t, owner.CheckProcessRunning(handle))
			mismatched := handle
			mismatched.IdentityTime = handle.IdentityTime.Add(-process.ProcessIdentityTimeMaximumDifference * 2)
			require.NoError(t, executor.WaitForProcessExit(ctx, mismatched))
			require.NoError(t, owner.CheckProcessRunning(handle))
			go func() { done <- executor.WaitForProcessExit(ctx, handle) }()
			require.NoError(t, owner.StopProcess(ctx, handle))
			select {
			case waitErr := <-done:
				require.NoError(t, waitErr)
			case <-ctx.Done():
				t.Fatal("process exit did not complete observation")
			}
			select {
			case <-exited:
			case <-ctx.Done():
				t.Fatal("observation prevented the owner's exit callback")
			}
		})
	}
}
