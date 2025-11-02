// Copyright (c) Microsoft Corporation. All rights reserved.

package process_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"

	int_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

var log logr.Logger

func TestMain(m *testing.M) {
	log = testutil.NewLogForTesting("process-tests")
	os.Exit(m.Run())
}

func TestRunCompleted(t *testing.T) {
	t.Parallel()

	delayToolDir, err := getDelayToolDir()
	require.NoError(t, err)

	executor := process.NewOSExecutor(log)
	defer executor.Dispose()
	testCtx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	exitInfoChan := make(chan process.ProcessExitInfo, 1)

	go func() {
		// The command will wait for 300 ms and then exit with code 12.
		cmd := exec.Command("./delay", "-d", "300ms", "-e", "12")
		cmd.Dir = delayToolDir
		exitCode, runErr := process.RunToCompletion(testCtx, executor, cmd)
		exitInfoChan <- process.ProcessExitInfo{
			ExitCode: exitCode,
			Err:      runErr,
			PID:      process.UnknownPID,
		}
	}()

	select {
	case <-testCtx.Done():
		t.Fatal("test timed out")
	case ei := <-exitInfoChan:
		require.NoError(t, ei.Err, "Program execution failed unexpectedly")
		require.Equal(t, int32(12), ei.ExitCode, "Program exit code was not captured properly")
	}
}

// Tests that process is terminated when the context passed to RunToCompletion() expires.
func TestRunToCompletionDeadlineExceeded(t *testing.T) {
	t.Parallel()

	delayToolDir, err := getDelayToolDir()
	require.NoError(t, err)

	executor := process.NewOSExecutor(log)
	defer executor.Dispose()

	// Start the process, but use a context that expires within 2 seconds.
	// If it takes more than 10 seconds to receive a notification that the process has exited,
	// it means the process was not terminated properly upon context cancellation.
	// We do not care about the timing so much as we care about the process being terminated at all.

	// Command returns on its own after 15 seconds. This prevents the test from hanging.
	cmd := exec.Command("./delay", "-d", "15s")
	cmd.Dir = delayToolDir
	start := time.Now()
	ctx, cancelFn := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelFn()

	_, err = process.RunToCompletion(ctx, executor, cmd)

	elapsed := time.Since(start)
	elapsedStr := osutil.FormatDuration(elapsed)
	require.True(t, errors.Is(err, context.DeadlineExceeded))
	if elapsed > 10*time.Second {
		t.Fatal("Process was not terminated timely, elapsed time was ", elapsedStr)
	}
}

// Tests that RunWithTimeout() call returns almost immediately when the context is cancelled.
func TestRunWithTimeout(t *testing.T) {
	t.Parallel()

	delayToolDir, err := getDelayToolDir()
	require.NoError(t, err)

	executor := process.NewOSExecutor(log)
	defer executor.Dispose()

	// Command returns on its own after 5 seconds. This prevents the test from hanging
	// or leaving processes running after the test is done.
	cmd := exec.Command("./delay", "-d", "5s")
	cmd.Dir = delayToolDir
	ctx, cancelFn := context.WithCancel(context.Background())
	runCallEndedCh := make(chan struct{})

	go func() {
		_, _ = process.RunWithTimeout(ctx, executor, cmd)
		runCallEndedCh <- struct{}{}
	}()

	// Sleep for a bit to make sure the process is started.
	time.Sleep(1 * time.Second)

	start := time.Now()
	cancelFn()
	<-runCallEndedCh
	elapsed := time.Since(start)
	elapsedStr := osutil.FormatDuration(elapsed)

	// Normally we expect the RunWithTimeout() call to return much faster than 500 ms,
	// but we allow for some slack in case the test is running on a heavily loaded machine.
	if elapsed > 500*time.Millisecond {
		t.Fatal("RunWithTimeout() call did not return immediately after context cancellation, elapsed time was ", elapsedStr)
	}
}

// Tests that process is terminated when the context that was used to start the process is manually cancelled.
func TestRunCancelled(t *testing.T) {
	t.Parallel()

	delayToolDir, err := getDelayToolDir()
	require.NoError(t, err)

	executor := process.NewOSExecutor(log)
	defer executor.Dispose()

	// Command returns on its own after 20 seconds. This prevents the test from hanging.
	exitInfoChan := make(chan process.ProcessExitInfo, 2)
	cmd := exec.Command("./delay", "-d", "20s")
	cmd.Dir = delayToolDir
	process.DecoupleFromParent(cmd)
	var onProcessExited process.ProcessExitHandlerFunc = func(pid process.Pid_t, exitCode int32, err error) {
		exitInfoChan <- process.ProcessExitInfo{
			ExitCode: exitCode,
			Err:      err,
			PID:      pid,
		}
	}

	ctx, cancelFn := context.WithCancel(context.Background())

	go func() {
		_, _, startWaitForExit, processStartErr := executor.StartProcess(ctx, cmd, onProcessExited, process.CreationFlagsNone)
		startupNotification := process.NewProcessExitInfo()
		if processStartErr != nil {
			startupNotification.Err = processStartErr
			exitInfoChan <- startupNotification
			return
		} else {
			startWaitForExit()
			exitInfoChan <- startupNotification
		}
	}()

	// Wait for the process to start.
	exitInfo := <-exitInfoChan
	require.NoError(t, exitInfo.Err, "Process failed to start")

	// Sleep for a bit to make sure the process is fully started and responsive to signals.
	time.Sleep(2 * time.Second)

	start := time.Now()
	cancelFn()
	exitInfo = <-exitInfoChan
	elapsed := time.Since(start)
	elapsedStr := osutil.FormatDuration(elapsed)

	require.True(t, errors.Is(exitInfo.Err, context.Canceled))
	if elapsed > 2*time.Second {
		t.Fatal("Process was not terminated timely, elapsed time was ", elapsedStr)
	}
}

// Tests that children of a process are terminated when the parent process is terminated.
func TestChildrenTerminated(t *testing.T) {
	type testcase struct {
		description    string
		processStartFn func(t *testing.T, cmd *exec.Cmd, e process.Executor) process.ProcessTreeItem
	}

	testcases := []testcase{
		{"external start", func(t *testing.T, cmd *exec.Cmd, _ process.Executor) process.ProcessTreeItem {
			err := cmd.Start()
			require.NoError(t, err, "could not start the 'delay' test program")
			pid := process.Uint32_ToPidT(uint32(cmd.Process.Pid))
			creationTime := process.StartTimeForProcess(pid)
			require.False(t, creationTime.IsZero(), "process start time should not be zero")
			return process.ProcessTreeItem{pid, creationTime}
		}},
		{"executor start, no wait", func(t *testing.T, cmd *exec.Cmd, e process.Executor) process.ProcessTreeItem {
			pid, _, _, err := e.StartProcess(context.Background(), cmd, nil, process.CreationFlagsNone)
			require.NoError(t, err, "could not start the 'delay' test program")
			creationTime := process.StartTimeForProcess(pid)
			require.False(t, creationTime.IsZero(), "process start time should not be zero")
			return process.ProcessTreeItem{pid, creationTime}
		}},
		{"executor start with wait", func(t *testing.T, cmd *exec.Cmd, e process.Executor) process.ProcessTreeItem {
			pid, _, startWaitForProcessExit, err := e.StartProcess(context.Background(), cmd, nil, process.CreationFlagsNone)
			require.NoError(t, err, "could not start the 'delay' test program")
			startWaitForProcessExit()
			creationTime := process.StartTimeForProcess(pid)
			require.False(t, creationTime.IsZero(), "process start time should not be zero")
			return process.ProcessTreeItem{pid, creationTime}
		}},
	}

	t.Parallel()

	delayToolDir, toolLaunchErr := getDelayToolDir()
	require.NoError(t, toolLaunchErr)

	// Using a shared executor to make sure that process stopping works no matter how it was started.
	executor := process.NewOSExecutor(log)

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			// All commands will return on its own after 20 seconds. This prevents the test from launching a bunch of processes
			// that turn into zombies.
			cmd := exec.Command("./delay", "--delay=20s", "--child-spec=2,1")
			cmd.Dir = delayToolDir

			process.DecoupleFromParent(cmd)

			rootP := tc.processStartFn(t, cmd, executor)

			// We ask for launching two children, with a single (grand)child each,
			// for a total of 4 child processes, so the expected tree size is 5.
			int_testutil.EnsureProcessTree(t, rootP, 5, 10*time.Second)

			processTree, err := process.GetProcessTree(rootP)
			require.NoError(t, err)

			err = executor.StopProcess(rootP.Pid, rootP.CreationTime)
			require.NoError(t, err)

			// Wait up to 10 seconds for all processes to exit. This guarantees that the test will only pass if StopProcess()
			// actually works, and not because 'delay' instances exited on their own (after 20 seconds).
			ensureAllStopped(t, processTree, 10*time.Second)
		})
	}
}

// Ensures that children using CreationFlagEnsureKillOnDispose are terminated when the executor is disposed.
func TestChildrenTerminatedOnDispose(t *testing.T) {
	t.Parallel()

	delayToolDir, toolLaunchErr := getDelayToolDir()
	require.NoError(t, toolLaunchErr)

	executor := process.NewOSExecutor(log)

	// Command return on its own after 20 seconds, so that it does not end up as a zombie process.
	cmd := exec.Command("./delay", "--delay=20s")
	cmd.Dir = delayToolDir
	processExited := make(chan struct{})

	_, _, startWaitForProcessExit, startErr := executor.StartProcess(
		context.Background(),
		cmd,
		process.ProcessExitHandlerFunc(func(_ process.Pid_t, _ int32, err error) {
			require.True(t, err == nil || process.IsEarlyProcessExitError(err), "The process could not be tracked: %v", err)
			close(processExited)
		}),
		process.CreationFlagEnsureKillOnDispose,
	)
	require.NoError(t, startErr)
	startWaitForProcessExit()

	executor.Dispose()

	select {
	case <-processExited:
		// Process exited as expected, so the test passes.
	case <-time.After(10 * time.Second):
		require.Fail(t, "Process did not exit within 10 seconds after Dispose() was called")
	}
}

func TestWatchCatchesProcessExit(t *testing.T) {
	t.Parallel()

	delayToolDir, toolLaunchErr := getDelayToolDir()
	require.NoError(t, toolLaunchErr)

	// Set a reasonable timeout that gives the wait polling time to see the delay process exit
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	// All commands will return on its own after 10 seconds. This prevents the test from launching a bunch of processes
	// that turn into zombies.
	cmd := exec.CommandContext(ctx, "./delay", "--delay=10s", "--child-spec=2,1")
	cmd.Dir = delayToolDir
	process.DecoupleFromParent(cmd)
	err := cmd.Start()
	require.NoError(t, err)

	pid := process.Uint32_ToPidT(uint32(cmd.Process.Pid))
	delayProc, err := process.FindWaitableProcess(pid, time.Time{})
	require.NoError(t, err)

	err = delayProc.Wait(ctx)
	require.NoError(t, err)
}

func TestContextCancelsWatch(t *testing.T) {
	t.Parallel()

	delayToolDir, toolLaunchErr := getDelayToolDir()
	require.NoError(t, toolLaunchErr)

	// Set a reasonable timeout that gives the wait polling time to see the delay process exit
	testCtx, testCancel := context.WithTimeout(context.Background(), time.Second*30)
	defer testCancel()

	// All commands will return on its own after 20 seconds. This prevents the test from launching a bunch of processes
	// that turn into zombies.
	cmd := exec.CommandContext(testCtx, "./delay", "--delay=20s", "--child-spec=2,1")
	cmd.Dir = delayToolDir
	process.DecoupleFromParent(cmd)
	err := cmd.Start()

	require.NoError(t, err, "command should start without error")

	pid := process.Uint32_ToPidT(uint32(cmd.Process.Pid))
	delayProc, err := process.FindWaitableProcess(pid, time.Time{})
	require.NoError(t, err, "find process should succeed without error")

	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second*5)
	defer waitCancel()

	err = delayProc.Wait(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "call to wait should return a context cancelation error")

	select {
	case <-testCtx.Done():
		require.Fail(t, "test timed out")
	default:
	}
}

func getDelayToolDir() (string, error) {
	return int_testutil.GetTestToolDir("delay")
}
