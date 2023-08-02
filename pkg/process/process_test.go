package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	wait "k8s.io/apimachinery/pkg/util/wait"

	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func TestRunCompleted(t *testing.T) {
	t.Parallel()

	delayToolDir, err := getDelayToolDir()
	require.NoError(t, err)

	executor := NewOSExecutor()
	testCtx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	exitInfoChan := make(chan ProcessExitInfo, 1)

	go func() {
		// The command will wait for 300 ms and then exit with code 12.
		cmd := exec.Command("./delay", "-d", "300ms", "-e", "12")
		cmd.Dir = delayToolDir
		exitCode, err := Run(testCtx, executor, cmd)
		exitInfoChan <- ProcessExitInfo{
			ExitCode: exitCode,
			Err:      err,
			PID:      UnknownPID,
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

// Tests that process is terminated when the context expires.
func TestRunDeadlineExceeded(t *testing.T) {
	t.Parallel()

	delayToolDir, err := getDelayToolDir()
	require.NoError(t, err)

	executor := NewOSExecutor()

	// Start the process, but use a context that expires within 500 ms.
	// If it takes more than 2 seconds to receive a notification that the process has exited,
	// it means the process was not terminated properly upon context cancellation.

	// Command returns on its own after 5 seconds. This prevents the test from hanging.
	cmd := exec.Command("./delay", "-d", "5s")
	cmd.Dir = delayToolDir
	start := time.Now()
	ctx, cancelFn := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelFn()

	_, err = Run(ctx, executor, cmd)

	elapsed := time.Since(start)
	require.True(t, errors.Is(err, context.DeadlineExceeded))
	if elapsed > 2*time.Second {
		t.Fatal("Process was not terminated timely")
	}
}

// Tests that process is terminated when the context that was used to start the process is manually cancelled.
func TestRunCancelled(t *testing.T) {
	t.Parallel()

	delayToolDir, err := getDelayToolDir()
	require.NoError(t, err)

	executor := NewOSExecutor()

	// Command returns on its own after 5 seconds. This prevents the test from hanging.
	exitInfoChan := make(chan ProcessExitInfo, 2)
	cmd := exec.Command("./delay", "-d", "5s")
	cmd.Dir = delayToolDir
	var onProcessExited ProcessExitHandlerFunc = func(pid int32, exitCode int32, err error) {
		exitInfoChan <- ProcessExitInfo{
			ExitCode: exitCode,
			Err:      err,
			PID:      pid,
		}
	}

	ctx, cancelFn := context.WithCancel(context.Background())

	go func() {
		_, startWaitForExit, err := executor.StartProcess(ctx, cmd, onProcessExited)
		startupNotification := NewProcessExitInfo()
		if err != nil {
			startupNotification.Err = err
			exitInfoChan <- startupNotification
			return
		} else {
			exitInfoChan <- startupNotification
			startWaitForExit()
		}
	}()

	// Wait for the process to start.
	exitInfo := <-exitInfoChan
	require.NoError(t, exitInfo.Err, "Process failed to start")

	start := time.Now()
	cancelFn()
	exitInfo = <-exitInfoChan
	elapsed := time.Since(start)

	require.True(t, errors.Is(exitInfo.Err, context.Canceled))
	if elapsed > 2*time.Second {
		t.Fatal("Process was not terminated timely")
	}
}

// Tests that children of a process are terminated when the parent process is terminated.
func TestChildrenTerminated(t *testing.T) {
	type testcase struct {
		description    string
		processStartFn func(t *testing.T, cmd *exec.Cmd, e Executor)
	}

	testcases := []testcase{
		{"external start", func(t *testing.T, cmd *exec.Cmd, _ Executor) {
			err := cmd.Start()
			require.NoError(t, err, "could not start the 'delay' test program")
		}},
		{"executor start, no wait", func(t *testing.T, cmd *exec.Cmd, e Executor) {
			_, _, err := e.StartProcess(context.Background(), cmd, nil)
			require.NoError(t, err, "could not start the 'delay' test program")
		}},
		{"executor start with wait", func(t *testing.T, cmd *exec.Cmd, e Executor) {
			_, startWaitForProcessExit, err := e.StartProcess(context.Background(), cmd, nil)
			require.NoError(t, err, "could not start the 'delay' test program")
			startWaitForProcessExit()
		}},
	}

	t.Parallel()

	delayToolDir, toolLaunchErr := getDelayToolDir()
	require.NoError(t, toolLaunchErr)

	// Using a shared executor to make sure that process stopping works no matter how it was started.
	executor := NewOSExecutor()

	for _, tc := range testcases {
		tc := tc // capture range variable for use in a goroutine

		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			// All commands will return on its own after 20 seconds. This prevents the test from launching a bunch of processes
			// that turn into zombies.
			cmd := exec.Command("./delay", "--delay=20s", "--child-spec=2,1")
			cmd.Dir = delayToolDir

			tc.processStartFn(t, cmd, executor)

			// We ask for launching two children, with a single (grand)child each,
			// for a total of 4 child processes, so the expected tree size is 5.
			const expectedProcessTreeSize = 5
			ensureProcessTree(t, int32(cmd.Process.Pid), expectedProcessTreeSize, 5*time.Second)

			processTree, err := GetProcessTree(int32(cmd.Process.Pid))
			require.NoError(t, err)

			err = executor.StopProcess(int32(cmd.Process.Pid))
			require.NoError(t, err)

			// Wait up to 5 seconds for all processes to exit. This guarantees that the test will only pass if StopProcess()
			// actually works, and not because 'delay' instances exited on their own (after 20 seconds).
			ensureAllStopped(t, processTree, 5*time.Second)
		})
	}
}

func getDelayToolDir() (string, error) {
	delayExeName := "delay"
	if runtime.GOOS == "windows" {
		delayExeName += ".exe"
	}

	rootDir, err := testutil.FindRootFor(testutil.FileTarget, ".toolbin", delayExeName)
	if err == nil {
		return filepath.Join(rootDir, ".toolbin"), nil
	} else {
		return "", fmt.Errorf("could not find 'delay' test tool: %w", err)
	}
}

func ensureProcessTree(t *testing.T, rootPid int32, expectedSize int, timeout time.Duration) {
	processesStartedCtx, processesStartedCancelFn := context.WithTimeout(context.Background(), timeout)
	defer processesStartedCancelFn()

	err := wait.PollUntilContextCancel(
		processesStartedCtx,
		100*time.Millisecond,
		true, // Don't wait before polling for the first time
		func(_ context.Context) (bool, error) {
			processTree, err := GetProcessTree(int32(rootPid))
			if err != nil {
				return false, err
			}
			return len(processTree) == expectedSize, nil
		},
	)

	require.NoError(t, err, "expected number of 'delay' program instances (%d) not found", expectedSize)
}
