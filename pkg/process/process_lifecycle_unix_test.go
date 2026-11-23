//go:build darwin || linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/wait"
)

// Verifies that canceling a root-only stop attempt releases stop ownership,
// leaves the process running, and permits a later stop to complete without another waiter.
func TestCancelledStopCanBeRetriedWithoutAnotherWaiter(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	cmd := delayCommandForTest(t, "--ignore-sigterm", "--delay=60s")
	handle, _, startErr := executor.StartProcess(testCtx, cmd, nil, CreationFlagEnsureKillOnDispose, nil)
	require.NoError(t, startErr)
	// Allow delay to install its signal handlers before requesting a stop.
	time.Sleep(2 * time.Second)
	stopCtx, stopCancel := context.WithCancel(testCtx)
	defer stopCancel()
	stopped := make(chan error, 1)
	go func() { stopped <- executor.StopProcess(stopCtx, handle, StopRootOnly()) }()
	ownershipErr := wait.PollUntilContextCancel(testCtx, time.Millisecond, true, func(context.Context) (bool, error) {
		executor.acquireLock()
		defer executor.releaseLock()
		state := executor.procsWaiting[handle]
		return state != nil && state.reason&waitReasonStopping != 0, nil
	})
	require.NoError(t, ownershipErr)
	stopCancel()
	select {
	case stopErr, received := <-stopped:
		require.True(t, received)
		require.ErrorIs(t, stopErr, context.Canceled)
	case <-testCtx.Done():
		t.Fatal("cancelled stop did not return")
	}
	require.NoError(t, executor.CheckProcessRunning(handle))
	require.NoError(t, executor.StopProcess(testCtx, handle, StopRootOnly()))
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)))
}

// Verifies that stopping an already-exited executor child starts its pending wait
// and reports the exit code returned by delay.
func TestStopStartsTrackedWaitForExitedChild(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	handler := NewConcurrentProcessExitHandler()
	handle, _, startErr := executor.StartProcess(ctx, delayCommandForTest(t, "--delay=1ms", "--exit-code=17"), handler, CreationFlagEnsureKillOnDispose, nil)
	require.NoError(t, startErr)
	exitedErr := wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		return IsProcessGoneErr(executor.CheckProcessRunning(handle)), nil
	})
	require.NoError(t, exitedErr)

	stopErr := executor.StopProcess(ctx, handle)
	require.True(t, stopErr == nil || IsProcessGoneErr(stopErr), "unexpected stop error: %v", stopErr)
	select {
	case <-handler.Exited():
	case <-ctx.Done():
		t.Fatal("pending process wait did not report exit")
	}
	require.Equal(t, int32(17), handler.ExitInfo().ExitCode)
	require.NoError(t, handler.ExitInfo().Err)
}

// Verifies that stopping an orphaned delay child confirms exit below the monitoring poll interval.
func TestStopNonChildUsesShortPollingInterval(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	root, tree, _ := startDelayTreeForTest(t, ctx, executor, delayCommandForTest(t, "--delay=60s", "--child-spec=1"), 2)
	require.Len(t, tree, 2)
	child := tree[1]
	require.NoError(t, executor.StopProcess(ctx, root, StopRootOnly()))
	require.NoError(t, executor.CheckProcessRunning(child))

	startedAt := time.Now()
	require.NoError(t, executor.StopProcess(ctx, child, StopRootOnly()))
	require.Less(t, time.Since(startedAt), defaultWaitPollInterval/2)
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(child)))
}

// Verifies that stopping a real delay tree removes every verified process even when
// the injected enumeration result reports an incomplete snapshot.
func TestIncompleteTreeStillStopsVerifiedProcesses(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	root, tree, _ := startDelayTreeForTest(t, ctx, executor, delayCommandForTest(t, "--delay=60s", "--child-spec=1"), 2)

	stopErr := executor.stopProcessTreeInternal(ctx, root, processStopOptions{opts: optNone}, func(context.Context, ProcessHandle) ([]ProcessHandle, error) {
		return tree, ErrIncompleteProcessTree
	})
	require.ErrorIs(t, stopErr, ErrIncompleteProcessTree)
	for _, handle := range tree {
		require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)))
	}
}

// Verifies that an injected enumeration deadline causes a real delay root to be force-stopped
// while the caller receives an incomplete-tree error.
func TestEnumerationDeadlineForceStopsRoot(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	root, _, handler := startDelayTreeForTest(t, ctx, executor, delayCommandForTest(t, "--delay=60s", "--ignore-sigterm"), 1)

	stopErr := executor.stopProcessTreeInternal(ctx, root, processStopOptions{opts: optNone}, func(context.Context, ProcessHandle) ([]ProcessHandle, error) {
		return nil, context.DeadlineExceeded
	})
	require.ErrorIs(t, stopErr, ErrIncompleteProcessTree)
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(root)))
	select {
	case <-handler.Exited():
	case <-ctx.Done():
		t.Fatal("force-stopped process did not report exit")
	}
	require.Equal(t, int32(-1), handler.ExitInfo().ExitCode)
	require.NoError(t, handler.ExitInfo().Err)
}

// Verifies that stopping a delay tree preserves the root's configured graceful exit code
// and removes its descendants without consuming the force-escalation window.
func TestStopProcessStopsDelayTreeGracefully(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	root, tree, handler := startDelayTreeForTest(t, ctx, executor, delayCommandForTest(t, "--delay=60s", "--child-spec=1", "--exit-code=17"), 2)

	startedAt := time.Now()
	require.NoError(t, executor.StopProcess(ctx, root))
	require.Less(t, time.Since(startedAt), signalAndWaitTimeout)
	for _, handle := range tree {
		require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)))
	}
	select {
	case <-handler.Exited():
	case <-ctx.Done():
		t.Fatal("stopped root did not report exit")
	}
	require.Equal(t, int32(17), handler.ExitInfo().ExitCode)
	require.NoError(t, handler.ExitInfo().Err)
}

// Verifies that disposal removes a delay tree and preserves the root's configured graceful exit code.
func TestDisposeStopsDelayTreeGracefully(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	_, tree, handler := startDelayTreeForTest(t, ctx, executor, delayCommandForTest(t, "--delay=60s", "--child-spec=1", "--exit-code=17"), 2)

	startedAt := time.Now()
	executor.Dispose()
	require.Less(t, time.Since(startedAt), signalAndWaitTimeout)
	for _, handle := range tree {
		require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)))
	}
	select {
	case <-handler.Exited():
	case <-ctx.Done():
		t.Fatal("disposed root did not report exit")
	}
	require.Equal(t, int32(17), handler.ExitInfo().ExitCode)
	require.NoError(t, handler.ExitInfo().Err)
}

// Verifies that disposal force-stops a SIGTERM-resistant delay root and removes its descendants
// within the complete stop budget.
func TestDisposeStopsDelayTreeWhenRootIgnoresSigterm(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 45*time.Second)
	defer cancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	_, tree, handler := startDelayTreeForTest(t, ctx, executor, delayCommandForTest(t, "--delay=60s", "--child-spec=1", "--ignore-sigterm"), 2)

	startedAt := time.Now()
	executor.Dispose()
	elapsed := time.Since(startedAt)
	require.GreaterOrEqual(t, elapsed, gracefulProcessStopTimeout-time.Second)
	require.Less(t, elapsed, processStopTimeout)
	for _, handle := range tree {
		require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)))
	}
	select {
	case <-handler.Exited():
	case <-ctx.Done():
		t.Fatal("force-stopped root did not report exit")
	}
	require.Equal(t, int32(-1), handler.ExitInfo().ExitCode)
}

// Verifies that an explicit stop force-stops a SIGTERM-resistant delay root and removes
// its descendants within the complete stop budget.
func TestStopProcessStopsDelayTreeWhenRootIgnoresSigterm(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 45*time.Second)
	defer cancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	cmd := delayCommandForTest(t, "--delay=60s", "--child-spec=1", "--ignore-sigterm")
	root, tree, handler := startDelayTreeForTest(t, ctx, executor, cmd, 2)

	startedAt := time.Now()
	require.NoError(t, executor.StopProcess(ctx, root))
	elapsed := time.Since(startedAt)
	require.GreaterOrEqual(t, elapsed, gracefulProcessStopTimeout-time.Second)
	require.Less(t, elapsed, processStopTimeout)
	for _, handle := range tree {
		require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)))
	}
	select {
	case <-handler.Exited():
	case <-ctx.Done():
		t.Fatal("force-stopped root did not report exit")
	}
	require.Equal(t, int32(-1), handler.ExitInfo().ExitCode)
}

// Verifies that group cleanup precedes tree cleanup, leaving escaped descendants a graceful-stop opportunity
// only when the group stops gracefully, and force-killing them when the group exhausts the graceful budget.
func TestStopProcessGroupOutcomeDeterminesTreeCleanup(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name         string
		ignoreTerm   bool
		expectedExit int32
	}{
		{"graceful", false, 0},
		{"forced", true, -1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			testCtx, cancelTest := testutil.GetTestContext(t, 0)
			t.Cleanup(cancelTest)
			output := testutil.NewBufferWriter()
			groupLog := funcr.New(func(_, message string) {
				_, _ = fmt.Fprintln(output, message)
			}, funcr.Options{Verbosity: 1})
			executor := NewOSExecutor(groupLog).(*OSExecutor)
			t.Cleanup(executor.Dispose)
			root, rootTree, _ := startDelayTreeForTest(t, testCtx, executor,
				delayCommandForTest(t, "--delay=120s", "--child-spec=1", "--fork-children"), 2)
			rootPID, rootPIDErr := PidT_ToInt(root.Pid)
			require.NoError(t, rootPIDErr)
			memberArgs := []string{"--delay=120s", "--child-spec=1", "--couple-children"}
			if testCase.ignoreTerm {
				memberArgs = append(memberArgs, "--ignore-sigterm")
			}
			memberCmd := delayCommandForTest(t, memberArgs...)
			memberCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: rootPID}
			member, memberTree, memberHandler := startDelayTreeForTest(t, testCtx, executor, memberCmd, 2)

			group, groupErr := FindProcessGroup(root, false)
			require.NoError(t, groupErr)
			require.NotNil(t, group)
			escaped := rootTree[1]
			escapedPID, escapedPIDErr := PidT_ToInt(escaped.Pid)
			require.NoError(t, escapedPIDErr)
			escapedGroup, escapedGroupErr := unix.Getpgid(escapedPID)
			require.NoError(t, escapedGroupErr)
			require.NotEqual(t, rootPID, escapedGroup)
			escapedSession, escapedSessionErr := unix.Getsid(escapedPID)
			require.NoError(t, escapedSessionErr)
			require.Equal(t, escapedPID, escapedSession)
			treeCleanupStarted := false
			options := newProcessStopOptions([]ProcessStopOption{StopWithProcessGroup()})
			if !testCase.ignoreTerm {
				options.afterRootExit = func() {
					treeCleanupStarted = true
					require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(member)))
					require.NoError(t, executor.CheckProcessRunning(escaped))
				}
			}
			startedAt := time.Now()
			require.NoError(t, executor.stopProcessInternal(testCtx, root, options))
			elapsed := time.Since(startedAt)
			if testCase.ignoreTerm {
				require.GreaterOrEqual(t, elapsed, gracefulProcessStopTimeout-time.Second)
				require.Less(t, elapsed, processStopTimeout)
			} else {
				require.Less(t, elapsed, signalAndWaitTimeout)
			}
			if !testCase.ignoreTerm {
				require.True(t, treeCleanupStarted)
			}
			expectedSignal := syscall.SIGTERM
			if testCase.ignoreTerm {
				expectedSignal = syscall.SIGKILL
			}
			messages := string(output.Bytes())
			signalIndex := strings.Index(messages, fmt.Sprintf(`"Signal"=%q`, expectedSignal.String()))
			treeIndex := strings.Index(messages, "Stopping process tree...")
			require.NotEqual(t, -1, signalIndex, "expected group signal was not sent: %s", messages)
			require.Greater(t, treeIndex, signalIndex, "tree cleanup must follow the group signal: %s", messages)
			escapedGracefulStop := fmt.Sprintf(`"msg"="Process stopped by SIGTERM" "PID"=%d`+"\n", escaped.Pid)
			escapedForceStop := fmt.Sprintf(`"msg"="Sending SIGKILL to process..." "PID"=%d`+"\n", escaped.Pid)
			if testCase.ignoreTerm {
				require.Contains(t, messages, escapedForceStop)
				require.NotContains(t, messages, escapedGracefulStop)
				require.NotContains(t, messages, "Giving child processes the remaining graceful-stop budget...")
			} else {
				require.Contains(t, messages, escapedGracefulStop)
				require.NotContains(t, messages, escapedForceStop)
				require.Contains(t, messages, "Giving child processes the remaining graceful-stop budget...")
			}
			for _, stopped := range append(rootTree, memberTree...) {
				require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(stopped)))
			}
			select {
			case <-memberHandler.Exited():
			case <-testCtx.Done():
				t.Fatal("stopped group member did not report exit")
			}
			require.Equal(t, testCase.expectedExit, memberHandler.ExitInfo().ExitCode)
			require.NoError(t, memberHandler.ExitInfo().Err)
		})
	}
}

// Verifies that both overlapping stops return only after every process in a delay tree has exited.
func TestConcurrentStopsConfirmDelayTreeExit(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 45*time.Second)
	defer cancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	root, tree, _ := startDelayTreeForTest(t, ctx, executor, delayCommandForTest(t, "--delay=60s", "--child-spec=1", "--ignore-sigterm"), 2)

	results := make(chan error, 2)
	for range 2 {
		go func() { results <- executor.StopProcess(ctx, root) }()
	}
	for range 2 {
		select {
		case stopErr, open := <-results:
			require.True(t, open)
			require.True(t, stopErr == nil || IsProcessGoneErr(stopErr) || errors.Is(stopErr, ErrIncompleteProcessTree), "unexpected stop error: %v", stopErr)
			for _, handle := range tree {
				require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)))
			}
		case <-ctx.Done():
			t.Fatal("concurrent stop did not finish")
		}
	}
}

func startDelayTreeForTest(
	t *testing.T,
	ctx context.Context,
	executor *OSExecutor,
	cmd *exec.Cmd,
	delayProcessCount int,
) (ProcessHandle, []ProcessHandle, *ConcurrentProcessExitHandler) {
	t.Helper()
	handler := NewConcurrentProcessExitHandler()
	handle, startWaiting, startErr := executor.StartProcess(ctx, cmd, handler, CreationFlagEnsureKillOnDispose, nil)
	require.NoError(t, startErr)
	startWaiting()
	var tree []ProcessHandle
	treeErr := wait.PollUntilContextCancel(ctx, 25*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		var snapshotErr error
		tree, snapshotErr = GetProcessTree(pollCtx, handle)
		return len(tree) >= delayProcessCount, snapshotErr
	})
	require.NoError(t, treeErr)
	t.Cleanup(func() {
		for _, treeHandle := range tree {
			proc, findErr := treeHandle.OsProcess()
			if IsProcessGoneErr(findErr) {
				continue
			}
			require.NoError(t, findErr)
			killErr := proc.Kill()
			releaseErr := proc.Release()
			require.True(t, killErr == nil || IsProcessGoneErr(killErr), "could not clean up delay: %v", killErr)
			require.NoError(t, releaseErr)
		}
	})
	return handle, tree, handler
}
