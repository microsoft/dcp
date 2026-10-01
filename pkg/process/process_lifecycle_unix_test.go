//go:build darwin || linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	unixProcessLifecycleFixtureMode = "DCP_PROCESS_LIFECYCLE_FIXTURE_MODE"
	delayedRootExitDuration         = 4 * time.Second
)

// Verifies that the stop-context subprocess fixture reports readiness, ignores SIGTERM,
// and remains alive until its standard input is closed.
func TestProcessStopContextFixture(t *testing.T) {
	if os.Getenv("DCP_PROCESS_STOP_CONTEXT_FIXTURE") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	_, readyErr := fmt.Fprintln(os.Stdout, "ready")
	require.NoError(t, readyErr)
	_, readErr := io.Copy(io.Discard, os.Stdin)
	require.NoError(t, readErr)
}

// Verifies that canceling a stop attempt releases stop ownership,
// leaves the process running, and permits a later stop to complete without another waiter.
func TestCancelledStopCanBeRetriedWithoutAnotherWaiter(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessStopContextFixture$")
	cmd.Env = append(os.Environ(), "DCP_PROCESS_STOP_CONTEXT_FIXTURE=1")
	stdout, stdoutErr := cmd.StdoutPipe()
	require.NoError(t, stdoutErr)
	stdin, stdinErr := cmd.StdinPipe()
	require.NoError(t, stdinErr)
	defer func() { _ = stdin.Close() }()
	handle, _, startErr := executor.StartProcess(testCtx, cmd, nil, CreationFlagEnsureKillOnDispose, nil)
	require.NoError(t, startErr)
	ready, readyErr := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, readyErr)
	require.Equal(t, "ready\n", ready)

	stopCtx, stopCancel := context.WithCancel(testCtx)
	defer stopCancel()
	stopped := make(chan error, 1)
	go func() { stopped <- executor.StopProcess(stopCtx, handle) }()
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
	require.NoError(t, executor.StopProcess(testCtx, handle))
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)))
}

// Verifies that the Unix lifecycle subprocess fixture creates the requested orphan or process tree,
// reports child PIDs and signals, and models graceful and signal-resistant descendants.
func TestUnixProcessLifecycleFixture(t *testing.T) {
	mode := os.Getenv(unixProcessLifecycleFixtureMode)
	switch mode {
	case "":
		return

	case "orphan-parent":
		childCmd := exec.Command(os.Args[0], "-test.run=^TestUnixProcessLifecycleFixture$")
		childCmd.Env = append(os.Environ(), unixProcessLifecycleFixtureMode+"=orphan-child")
		DecoupleFromParent(childCmd)
		childStartErr := childCmd.Start()
		require.NoError(t, childStartErr)
		_, writeErr := fmt.Fprintln(os.Stdout, childCmd.Process.Pid)
		require.NoError(t, writeErr)

	case "orphan-child":
		time.Sleep(30 * time.Second)

	case "exit-immediately":
		return

	case "tree-root", "graceful-root", "forced-root", "deadline-root", "delayed-root", "concurrent-root", "concurrent-graceful-root", "pipe-held-root":
		childMode := "tree-child"
		if mode == "graceful-root" || mode == "concurrent-graceful-root" {
			childMode = "graceful-child"
		} else if mode == "forced-root" {
			signal.Ignore(syscall.SIGTERM)
			childMode = "observing-child"
		} else if mode == "delayed-root" {
			childMode = "observing-child"
		} else if mode == "pipe-held-root" {
			signal.Ignore(syscall.SIGTERM)
			childMode = "pipe-holder-child"
		} else if mode == "concurrent-root" {
			childMode = "observing-child"
		} else if mode == "deadline-root" {
			childMode = "observing-child"
		}
		var rootSignalCh chan os.Signal
		if mode == "concurrent-root" || mode == "concurrent-graceful-root" || mode == "delayed-root" {
			rootSignalCh = make(chan os.Signal, 1)
			signal.Notify(rootSignalCh, syscall.SIGTERM)
			defer signal.Stop(rootSignalCh)
		}
		childCmd := exec.Command(os.Args[0], "-test.run=^TestUnixProcessLifecycleFixture$")
		childCmd.Env = append(os.Environ(), unixProcessLifecycleFixtureMode+"="+childMode)
		var signalNoticeWriter *os.File
		if mode != "tree-root" && mode != "pipe-held-root" {
			signalNoticeWriter = os.NewFile(uintptr(4), "signal-notice")
			require.NotNil(t, signalNoticeWriter)
			childCmd.ExtraFiles = []*os.File{signalNoticeWriter}
		}
		if mode == "pipe-held-root" {
			childCmd.Stdout = os.Stdout
			childCmd.Stderr = os.Stderr
		}
		childStartErr := childCmd.Start()
		require.NoError(t, childStartErr)
		if signalNoticeWriter != nil && mode != "concurrent-root" && mode != "concurrent-graceful-root" {
			require.NoError(t, signalNoticeWriter.Close())
		}
		go func() {
			_ = childCmd.Wait()
		}()

		pidWriter := os.NewFile(uintptr(3), "child-pid")
		require.NotNil(t, pidWriter)
		_, writeErr := fmt.Fprintln(pidWriter, childCmd.Process.Pid)
		require.NoError(t, writeErr)
		require.NoError(t, pidWriter.Close())
		if mode == "concurrent-root" || mode == "concurrent-graceful-root" || mode == "delayed-root" {
			<-rootSignalCh
			if mode != "delayed-root" {
				_, signalWriteErr := fmt.Fprintln(signalNoticeWriter, "root-sigterm")
				require.NoError(t, signalWriteErr)
				require.NoError(t, signalNoticeWriter.Close())
			}
			if mode == "concurrent-graceful-root" {
				time.Sleep(250 * time.Millisecond)
				return
			}
			if mode == "delayed-root" {
				time.Sleep(delayedRootExitDuration)
				return
			}
		}
		time.Sleep(30 * time.Second)

	case "tree-child", "pipe-holder-child":
		time.Sleep(30 * time.Second)

	case "graceful-child", "observing-child":
		signalCh := make(chan os.Signal, 1)
		signal.Notify(signalCh, syscall.SIGTERM)
		defer signal.Stop(signalCh)
		signalNoticeWriter := os.NewFile(uintptr(3), "signal-notice")
		require.NotNil(t, signalNoticeWriter)
		_, readyWriteErr := fmt.Fprintln(signalNoticeWriter, "ready")
		require.NoError(t, readyWriteErr)
		<-signalCh
		_, signalWriteErr := fmt.Fprintln(signalNoticeWriter, "sigterm")
		require.NoError(t, signalWriteErr)
		require.NoError(t, signalNoticeWriter.Close())
		if mode == "observing-child" {
			time.Sleep(30 * time.Second)
		}

	default:
		t.Fatalf("unknown fixture mode %q", mode)
	}
}

// Verifies that stopping an executor-owned child starts its tracked wait even if
// the child already exited before exit monitoring was enabled.
func TestStopStartsTrackedWaitForExitedChild(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()

	exitResults := make(chan ProcessExitInfo, 1)
	cmd := exec.Command(os.Args[0], "-test.run=^TestUnixProcessLifecycleFixture$")
	cmd.Env = append(os.Environ(), unixProcessLifecycleFixtureMode+"=exit-immediately")
	handle, _, startErr := executor.StartProcess(
		context.Background(),
		cmd,
		ProcessExitHandlerFunc(func(pid Pid_t, exitCode int32, exitErr error) {
			exitResults <- ProcessExitInfo{PID: pid, ExitCode: exitCode, Err: exitErr}
		}),
		CreationFlagEnsureKillOnDispose,
		nil,
	)
	require.NoError(t, startErr)

	exitObservedErr := wait.PollUntilContextCancel(testCtx, time.Millisecond, true, func(context.Context) (bool, error) {
		return IsProcessGoneErr(executor.CheckProcessRunning(handle)), nil
	})
	require.NoError(t, exitObservedErr)

	stopErr := executor.StopProcess(testCtx, handle)
	require.True(t, stopErr == nil || IsProcessGoneErr(stopErr), "unexpected stop error: %v", stopErr)

	select {
	case exitResult := <-exitResults:
		require.Equal(t, handle.Pid, exitResult.PID)
		require.Equal(t, int32(0), exitResult.ExitCode)
		require.NoError(t, exitResult.Err)
	case <-testCtx.Done():
		t.Fatal("timed out waiting for tracked process exit notification")
	}
}

// Verifies that stopping a non-child process uses the short stop polling interval,
// completes below the monitoring poll floor, and confirms the process is gone.
func TestStopNonChildUsesShortPollingInterval(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	handle := startOrphanProcessForTest(t, testCtx)
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()

	startedAt := time.Now()
	stopErr := executor.StopProcess(testCtx, handle, StopRootOnly())
	elapsed := time.Since(startedAt)

	require.NoError(t, stopErr)
	// This is the stop-latency contract: adopted processes must not inherit the two-second monitor poll floor.
	require.Less(t, elapsed, defaultWaitPollInterval/2)
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)))
}

// Verifies that an incomplete process-tree result is returned to the caller
// while every verified process in the partial tree is still stopped.
func TestIncompleteTreeStillStopsVerifiedProcesses(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	rootHandle, childHandle := startProcessTreeForTest(t, testCtx, executor, "tree-root", nil, nil)
	injectedSnapshotErr := fmt.Errorf(
		"%w: process enumeration was incomplete: unrelated process metadata could not be read",
		ErrIncompleteProcessTree,
	)

	stopErr := executor.stopProcessTreeInternal(
		testCtx,
		rootHandle,
		processStopOptions{opts: optNone},
		func(context.Context, ProcessHandle) ([]ProcessHandle, error) {
			return []ProcessHandle{rootHandle, childHandle}, injectedSnapshotErr
		},
	)

	require.ErrorIs(t, stopErr, ErrIncompleteProcessTree)
	requireProcessGone(t, testCtx, executor, rootHandle)
	requireProcessGone(t, testCtx, executor, childHandle)
}

// Verifies that expiration of the internal graceful enumeration budget still
// force-stops the root while reporting descendant cleanup as incomplete.
func TestEnumerationDeadlineForceStopsRoot(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	rootCmd := exec.Command(os.Args[0], "-test.run=^TestUnixProcessLifecycleFixture$")
	rootCmd.Env = append(os.Environ(), unixProcessLifecycleFixtureMode+"=tree-child")
	rootHandle, startWaitForExit, startErr := executor.StartProcess(
		testCtx,
		rootCmd,
		nil,
		CreationFlagEnsureKillOnDispose,
		nil,
	)
	require.NoError(t, startErr)
	startWaitForExit()

	stopErr := executor.stopProcessTreeInternal(
		testCtx,
		rootHandle,
		processStopOptions{opts: optNone},
		func(context.Context, ProcessHandle) ([]ProcessHandle, error) {
			return nil, context.DeadlineExceeded
		},
	)

	require.ErrorIs(t, stopErr, ErrIncompleteProcessTree)
	requireProcessGone(t, testCtx, executor, rootHandle)
}

// Verifies that executor disposal gives descendants the root process's remaining graceful-stop budget,
// observes their SIGTERM handling, and stops the complete tree without force-kill delay.
func TestDisposeGivesDescendantsRemainingGracefulBudget(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 45*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	rootHandle, childHandle, signalNotices := startProcessTreeWithSignalNoticeForTest(t, testCtx, executor, "graceful-root")

	startedAt := time.Now()
	executor.Dispose()
	elapsed := time.Since(startedAt)
	remainingNotices, readErr := io.ReadAll(signalNotices)

	require.NoError(t, readErr)
	require.Equal(t, "sigterm\n", string(remainingNotices))
	require.Less(t, elapsed, signalAndWaitTimeout)
	requireProcessGone(t, testCtx, executor, rootHandle)
	requireProcessGone(t, testCtx, executor, childHandle)
}

// Verifies that an explicit stop gives descendants the same remaining graceful budget as disposal.
func TestStopProcessGivesDescendantsRemainingGracefulBudget(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 45*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	rootHandle, childHandle, signalNotices := startProcessTreeWithSignalNoticeForTest(t, testCtx, executor, "graceful-root")

	startedAt := time.Now()
	stopErr := executor.StopProcess(testCtx, rootHandle)
	elapsed := time.Since(startedAt)
	remainingNotices, readErr := io.ReadAll(signalNotices)

	require.NoError(t, stopErr)
	require.NoError(t, readErr)
	require.Equal(t, "sigterm\n", string(remainingNotices))
	require.Less(t, elapsed, signalAndWaitTimeout)
	requireProcessGone(t, testCtx, executor, rootHandle)
	requireProcessGone(t, testCtx, executor, childHandle)
}

// Verifies that time spent gracefully stopping the root is deducted from the
// descendants' graceful-stop budget instead of starting a fresh deadline.
func TestStopProcessSharesGracefulBudgetAfterDelayedRootExit(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	rootHandle, childHandle, signalNotices := startProcessTreeWithSignalNoticeForTest(t, testCtx, executor, "delayed-root")

	startedAt := time.Now()
	stopErr := executor.StopProcess(testCtx, rootHandle)
	elapsed := time.Since(startedAt)
	remainingNotices, readErr := io.ReadAll(signalNotices)

	require.NoError(t, stopErr)
	require.NoError(t, readErr)
	require.Equal(t, "sigterm\n", string(remainingNotices))
	require.GreaterOrEqual(t, elapsed, gracefulProcessStopTimeout-time.Second)
	require.Less(t, elapsed, gracefulProcessStopTimeout+delayedRootExitDuration/2)
	requireProcessGone(t, testCtx, executor, rootHandle)
	requireProcessGone(t, testCtx, executor, childHandle)
}

// Verifies that force escalation uses the full graceful budget, then confirms
// SIGKILL by identity instead of waiting for descendant-held stdio pipes.
func TestForceKillDoesNotWaitForDescendantHeldStdio(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	rootHandle, childHandle := startProcessTreeForTest(t, testCtx, executor, "pipe-held-root", io.Discard, nil)

	startedAt := time.Now()
	stopErr := executor.StopProcess(testCtx, rootHandle)
	elapsed := time.Since(startedAt)

	require.NoError(t, stopErr)
	require.GreaterOrEqual(t, elapsed, gracefulProcessStopTimeout)
	require.Less(t, elapsed, processStopTimeout+2*time.Second)
	requireProcessGone(t, testCtx, executor, rootHandle)
	requireProcessGone(t, testCtx, executor, childHandle)
}

// Verifies that overlapping Unix stops wait for the owning root stop before processing descendants,
// and that every caller confirms descendant exit before returning.
func TestConcurrentStopsPreserveRootFirstOrdering(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		mode            string
		expectedNotices string
	}{
		{name: "forced root", mode: "concurrent-root"},
		{name: "graceful root", mode: "concurrent-graceful-root", expectedNotices: "sigterm\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testCtx, testCancel := testutil.GetTestContext(t, 45*time.Second)
			defer testCancel()
			executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
			defer executor.Dispose()
			rootHandle, childHandle, signalNotices := startProcessTreeWithSignalNoticeForTest(t, testCtx, executor, testCase.mode)

			stopResults := make(chan error, 2)
			go func() {
				stopResults <- executor.StopProcess(testCtx, rootHandle)
			}()
			rootSignalNotice, rootSignalReadErr := signalNotices.ReadString('\n')
			require.NoError(t, rootSignalReadErr)
			require.Equal(t, "root-sigterm\n", rootSignalNotice)

			go func() {
				stopResults <- executor.StopProcess(testCtx, rootHandle)
			}()
			for range 2 {
				select {
				case stopErr := <-stopResults:
					require.NoError(t, stopErr)
					require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(childHandle)),
						"each concurrent stop must confirm descendant exit")
				case <-testCtx.Done():
					t.Fatal("concurrent process stop did not finish")
				}
			}

			remainingNotices, readErr := io.ReadAll(signalNotices)
			require.NoError(t, readErr)
			require.Equal(t, testCase.expectedNotices, string(remainingNotices))
			requireProcessGone(t, testCtx, executor, rootHandle)
			requireProcessGone(t, testCtx, executor, childHandle)
		})
	}
}

// Verifies that when disposal force-kills the root, descendants skip graceful signaling
// and are force-killed with the entire process tree confirmed gone.
func TestDisposeForceKillsDescendantsWhenRootWasForceKilled(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 45*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	rootHandle, childHandle, signalNotices := startProcessTreeWithSignalNoticeForTest(t, testCtx, executor, "forced-root")

	executor.Dispose()
	remainingNotices, readErr := io.ReadAll(signalNotices)

	require.NoError(t, readErr)
	require.Empty(t, remainingNotices, "descendants must not receive a graceful signal after the root required a force kill")
	requireProcessGone(t, testCtx, executor, rootHandle)
	requireProcessGone(t, testCtx, executor, childHandle)
}

// Verifies that disposal shares one graceful deadline across the process tree,
// then force-kills a surviving descendant within the bounded force-cleanup interval.
func TestDisposeForceKillsDescendantsAfterWholeTreeDeadline(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 45*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	rootHandle, childHandle, signalNotices := startProcessTreeWithSignalNoticeForTest(t, testCtx, executor, "deadline-root")

	startedAt := time.Now()
	// The root gets the first graceful-stop opportunity. Because it exits gracefully, the
	// descendant receives the remaining portion of the shared 15-second budget. The descendant
	// deliberately survives SIGTERM, so disposal force-kills it only after that budget expires.
	executor.Dispose()
	elapsed := time.Since(startedAt)
	remainingNotices, readErr := io.ReadAll(signalNotices)

	require.NoError(t, readErr)
	require.Equal(t, "sigterm\n", string(remainingNotices))
	require.GreaterOrEqual(t, elapsed, gracefulProcessStopTimeout-time.Second)
	require.Less(t, elapsed, processStopTimeout+2*time.Second)
	requireProcessGone(t, testCtx, executor, rootHandle)
	requireProcessGone(t, testCtx, executor, childHandle)
}

func startOrphanProcessForTest(t *testing.T, ctx context.Context) ProcessHandle {
	t.Helper()

	parentCmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUnixProcessLifecycleFixture$")
	parentCmd.Env = append(os.Environ(), unixProcessLifecycleFixtureMode+"=orphan-parent")
	output, runErr := parentCmd.Output()
	require.NoError(t, runErr)
	childPIDText, _, found := strings.Cut(string(output), "\n")
	require.True(t, found)
	childPID, parseErr := strconv.ParseInt(strings.TrimSpace(childPIDText), 10, 64)
	require.NoError(t, parseErr)
	handle, handleErr := FindProcessHandle(Pid_t(childPID))
	require.NoError(t, handleErr)
	t.Cleanup(func() {
		killProcessForTest(handle)
	})
	return handle
}

func startProcessTreeForTest(
	t *testing.T,
	ctx context.Context,
	executor *OSExecutor,
	mode string,
	output io.Writer,
	childSignalNoticeWriter *os.File,
) (ProcessHandle, ProcessHandle) {
	t.Helper()

	pidReader, pidWriter, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	defer func() {
		_ = pidReader.Close()
	}()
	rootCmd := exec.Command(os.Args[0], "-test.run=^TestUnixProcessLifecycleFixture$")
	rootCmd.Env = append(os.Environ(), unixProcessLifecycleFixtureMode+"="+mode)
	rootCmd.ExtraFiles = []*os.File{pidWriter}
	if childSignalNoticeWriter != nil {
		rootCmd.ExtraFiles = append(rootCmd.ExtraFiles, childSignalNoticeWriter)
	}
	rootCmd.Stdout = output
	rootCmd.Stderr = output
	rootHandle, startWaitForExit, startErr := executor.StartProcess(
		ctx,
		rootCmd,
		nil,
		CreationFlagEnsureKillOnDispose,
		nil,
	)
	require.NoError(t, startErr)
	require.NoError(t, pidWriter.Close())
	if childSignalNoticeWriter != nil {
		require.NoError(t, childSignalNoticeWriter.Close())
	}
	startWaitForExit()

	childPIDText, readErr := bufio.NewReader(pidReader).ReadString('\n')
	require.NoError(t, readErr)
	childPID, parseErr := strconv.ParseInt(strings.TrimSpace(childPIDText), 10, 64)
	require.NoError(t, parseErr)
	childHandle, childHandleErr := FindProcessHandle(Pid_t(childPID))
	require.NoError(t, childHandleErr)
	t.Cleanup(func() {
		killProcessForTest(childHandle)
		killProcessForTest(rootHandle)
	})
	return rootHandle, childHandle
}

func startProcessTreeWithSignalNoticeForTest(
	t *testing.T,
	ctx context.Context,
	executor *OSExecutor,
	mode string,
) (ProcessHandle, ProcessHandle, *bufio.Reader) {
	t.Helper()

	signalNoticeReader, signalNoticeWriter, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	t.Cleanup(func() {
		_ = signalNoticeReader.Close()
	})
	rootHandle, childHandle := startProcessTreeForTest(t, ctx, executor, mode, nil, signalNoticeWriter)
	signalNotices := bufio.NewReader(signalNoticeReader)
	readyNotice, readyReadErr := signalNotices.ReadString('\n')
	require.NoError(t, readyReadErr)
	require.Equal(t, "ready\n", readyNotice)
	return rootHandle, childHandle, signalNotices
}

func requireProcessGone(t *testing.T, ctx context.Context, executor *OSExecutor, handle ProcessHandle) {
	t.Helper()

	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Second)
	defer waitCancel()
	waitErr := wait.PollUntilContextCancel(waitCtx, 25*time.Millisecond, true, func(context.Context) (bool, error) {
		return IsProcessGoneErr(executor.CheckProcessRunning(handle)), nil
	})
	require.NoError(t, waitErr)
}

func killProcessForTest(handle ProcessHandle) {
	proc, findErr := FindProcess(handle)
	if findErr != nil {
		return
	}
	_ = proc.Kill()
	_ = proc.Release()
}
