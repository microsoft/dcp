package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tklauser/ps"
	wait "k8s.io/apimachinery/pkg/util/wait"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/usvc-apiserver/internal/dcp/dcppaths"
	int_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

var (
	log = logger.New("process-tests").Logger
)

func TestRunCompleted(t *testing.T) {
	t.Parallel()

	delayToolDir, err := getDelayToolDir()
	require.NoError(t, err)

	executor := NewOSExecutor(log)
	testCtx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	exitInfoChan := make(chan ProcessExitInfo, 1)

	go func() {
		// The command will wait for 300 ms and then exit with code 12.
		cmd := exec.Command("./delay", "-d", "300ms", "-e", "12")
		cmd.Dir = delayToolDir
		exitCode, runErr := RunToCompletion(testCtx, executor, cmd)
		exitInfoChan <- ProcessExitInfo{
			ExitCode: exitCode,
			Err:      runErr,
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

// Tests that process is terminated when the context passed to RunToCompletion() expires.
func TestRunToCompletionDeadlineExceeded(t *testing.T) {
	t.Parallel()

	delayToolDir, err := getDelayToolDir()
	require.NoError(t, err)

	executor := NewOSExecutor(log)

	// Start the process, but use a context that expires within 500 ms.
	// If it takes more than 2 seconds to receive a notification that the process has exited,
	// it means the process was not terminated properly upon context cancellation.

	// Command returns on its own after 5 seconds. This prevents the test from hanging.
	cmd := exec.Command("./delay", "-d", "5s")
	cmd.Dir = delayToolDir
	start := time.Now()
	ctx, cancelFn := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelFn()

	_, err = RunToCompletion(ctx, executor, cmd)

	elapsed := time.Since(start)
	elapsedStr := osutil.FormatDuration(elapsed)
	require.True(t, errors.Is(err, context.DeadlineExceeded))
	if elapsed > 2*time.Second {
		t.Fatal("Process was not terminated timely, elapsed time was ", elapsedStr)
	}
}

// Tests that RunWithTimeout() call returns almost immediately when the context is cancelled.
func TestRunWithTimeout(t *testing.T) {
	t.Parallel()

	delayToolDir, err := getDelayToolDir()
	require.NoError(t, err)

	executor := NewOSExecutor(log)

	// Command returns on its own after 5 seconds. This prevents the test from hanging
	// or leaving processes running after the test is done.
	cmd := exec.Command("./delay", "-d", "5s")
	cmd.Dir = delayToolDir
	ctx, cancelFn := context.WithCancel(context.Background())
	runCallEndedCh := make(chan struct{})

	go func() {
		_, _ = RunWithTimeout(ctx, executor, cmd)
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

	executor := NewOSExecutor(log)

	// Command returns on its own after 20 seconds. This prevents the test from hanging.
	exitInfoChan := make(chan ProcessExitInfo, 2)
	cmd := exec.Command("./delay", "-d", "20s")
	cmd.Dir = delayToolDir
	var onProcessExited ProcessExitHandlerFunc = func(pid Pid_t, exitCode int32, err error) {
		exitInfoChan <- ProcessExitInfo{
			ExitCode: exitCode,
			Err:      err,
			PID:      pid,
		}
	}

	ctx, cancelFn := context.WithCancel(context.Background())

	go func() {
		_, _, startWaitForExit, processStartErr := executor.StartProcess(ctx, cmd, onProcessExited)
		startupNotification := NewProcessExitInfo()
		if processStartErr != nil {
			startupNotification.Err = processStartErr
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
		processStartFn func(t *testing.T, cmd *exec.Cmd, e Executor) ProcessTreeItem
	}

	testcases := []testcase{
		{"external start", func(t *testing.T, cmd *exec.Cmd, _ Executor) ProcessTreeItem {
			err := cmd.Start()
			require.NoError(t, err, "could not start the 'delay' test program")
			pid, err := IntToPidT(cmd.Process.Pid)
			require.NoError(t, err)
			pp, ppErr := ps.FindProcess(cmd.Process.Pid)
			require.NoError(t, ppErr)
			return ProcessTreeItem{pid, pp.CreationTime()}
		}},
		{"executor start, no wait", func(t *testing.T, cmd *exec.Cmd, e Executor) ProcessTreeItem {
			pid, _, _, err := e.StartProcess(context.Background(), cmd, nil)
			require.NoError(t, err, "could not start the 'delay' test program")
			intPid, intPidErr := PidT_ToInt(pid)
			require.NoError(t, intPidErr)
			pp, ppErr := ps.FindProcess(intPid)
			require.NoError(t, ppErr)
			return ProcessTreeItem{pid, pp.CreationTime()}
		}},
		{"executor start with wait", func(t *testing.T, cmd *exec.Cmd, e Executor) ProcessTreeItem {
			pid, _, startWaitForProcessExit, err := e.StartProcess(context.Background(), cmd, nil)
			require.NoError(t, err, "could not start the 'delay' test program")
			startWaitForProcessExit()
			intPid, intPidErr := PidT_ToInt(pid)
			require.NoError(t, intPidErr)
			pp, ppErr := ps.FindProcess(intPid)
			require.NoError(t, ppErr)
			return ProcessTreeItem{pid, pp.CreationTime()}
		}},
	}

	t.Parallel()

	delayToolDir, toolLaunchErr := getDelayToolDir()
	require.NoError(t, toolLaunchErr)

	// Using a shared executor to make sure that process stopping works no matter how it was started.
	executor := NewOSExecutor(log)

	for _, tc := range testcases {
		tc := tc // capture range variable for use in a goroutine

		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			// All commands will return on its own after 20 seconds. This prevents the test from launching a bunch of processes
			// that turn into zombies.
			cmd := exec.Command("./delay", "--delay=20s", "--child-spec=2,1")
			cmd.Dir = delayToolDir

			rootP := tc.processStartFn(t, cmd, executor)

			// We ask for launching two children, with a single (grand)child each,
			// for a total of 4 child processes, so the expected tree size is 5.
			expectedProcessTreeSize := 5

			ensureProcessTree(t, rootP, expectedProcessTreeSize, 10*time.Second)

			processTree, err := GetProcessTree(rootP)
			require.NoError(t, err)

			err = executor.StopProcess(rootP.Pid, rootP.CreationTime)
			require.NoError(t, err)

			// Wait up to 10 seconds for all processes to exit. This guarantees that the test will only pass if StopProcess()
			// actually works, and not because 'delay' instances exited on their own (after 20 seconds).
			ensureAllStopped(t, processTree, 10*time.Second)
		})
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
	DecoupleFromParent(cmd)
	err := cmd.Start()
	require.NoError(t, err)

	pid, err := IntToPidT(cmd.Process.Pid)
	require.NoError(t, err)

	delayProc, err := FindWaitableProcess(pid, time.Time{})
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
	DecoupleFromParent(cmd)
	err := cmd.Start()

	require.NoError(t, err, "command should start without error")

	pid, err := IntToPidT(cmd.Process.Pid)
	require.NoError(t, err)
	delayProc, err := FindWaitableProcess(pid, time.Time{})
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

func TestMonitorProcessExitsWithErrorForInvalidPid(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	// Set a reasonable timeout that gives the wait polling time to see the delay process exit
	testCtx, testCancel := context.WithTimeout(context.Background(), time.Second*30)
	defer testCancel()

	dcpProcCmd := exec.CommandContext(testCtx, dcpProc)
	err := dcpProcCmd.Run()
	require.Error(t, err)
}

func TestMonitorProcessTerminatesWatchedProcesses(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolDir, toolLaunchErr := getDelayToolDir()
	require.NoError(t, toolLaunchErr)

	// Set a reasonable timeout that ensures we can see all expected processes exit before their delay time
	testCtx, testCancel := context.WithTimeout(context.Background(), time.Second*30)
	defer testCancel()

	// All commands will return on its own after 30 seconds. This prevents the test from launching a bunch of processes
	// that turn into zombies.
	parentCmd := exec.CommandContext(testCtx, "./delay", "--delay=30s")
	parentCmd.Dir = delayToolDir
	parentCmdErr := parentCmd.Start()
	require.NoError(t, parentCmdErr, "command should start without error")

	parentPid, parentPidErr := IntToPidT(parentCmd.Process.Pid)
	require.NoError(t, parentPidErr)
	parentPp, parentPpErr := ps.FindProcess(int(parentPid))
	require.NoError(t, parentPpErr)
	ensureProcessTree(t, ProcessTreeItem{parentPid, parentPp.CreationTime()}, 1, 5*time.Second)
	parentProcInfo, parentProcInfoErr := ps.FindProcess(int(parentPid))
	require.NoError(t, parentProcInfoErr)

	childrenCmd := exec.CommandContext(testCtx, "./delay", "--delay=30s", "--child-spec=1,1")
	childrenCmd.Dir = delayToolDir
	childrenCmdErr := childrenCmd.Start()
	require.NoError(t, childrenCmdErr, "command should start without error")

	pid, pidErr := IntToPidT(childrenCmd.Process.Pid)
	require.NoError(t, pidErr)
	childPp, childPpErr := ps.FindProcess(int(pid))
	require.NoError(t, childPpErr)
	ensureProcessTree(t, ProcessTreeItem{pid, childPp.CreationTime()}, 3, 10*time.Second)
	childProcInfo, childProcInfoErr := ps.FindProcess(int(pid))
	require.NoError(t, childProcInfoErr)

	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--child", strconv.Itoa(childrenCmd.Process.Pid),
		"--monitor-start-time", parentProcInfo.CreationTime().Format(osutil.RFC3339MiliTimestampFormat),
		"--child-start-time", childProcInfo.CreationTime().Format(osutil.RFC3339MiliTimestampFormat),
	)
	dcpProcCmd.Stdout = os.Stdout
	dcpProcCmd.Stderr = os.Stderr
	dcpProcCmdErr := dcpProcCmd.Start()
	require.NoError(t, dcpProcCmdErr, "command should start without error")

	// Give enough time for the monitor process to start before killing the parent process
	<-time.After(1 * time.Second)

	killErr := parentCmd.Process.Kill()
	require.NoError(t, killErr)
	_ = parentCmd.Wait()

	_ = childrenCmd.Wait()
	require.True(t, childrenCmd.ProcessState.Exited(), "child process should have exited")

	dcpWaitErr := dcpProcCmd.Wait()
	require.NoError(t, dcpWaitErr)
}

func TestMonitorProcessNotMonitoredIfStartTimeDoesNotMatch(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolDir, toolLaunchErr := getDelayToolDir()
	require.NoError(t, toolLaunchErr)

	// Set a reasonable timeout that ensures we can see all expected processes exit before their delay time
	testCtx, testCancel := context.WithTimeout(context.Background(), time.Second*30)
	defer testCancel()

	// All commands will return on its own after 30 seconds. This prevents the test from launching a bunch of processes
	// that turn into zombies.
	parentCmd := exec.CommandContext(testCtx, "./delay", "--delay=30s")
	parentCmd.Dir = delayToolDir
	parentCmdErr := parentCmd.Start()
	require.NoError(t, parentCmdErr, "command should start without error")
	defer func() {
		_ = parentCmd.Process.Kill()
		_ = parentCmd.Wait()
	}()

	parentPid, parentPidErr := IntToPidT(parentCmd.Process.Pid)
	require.NoError(t, parentPidErr)
	parentPp, parentPpErr := ps.FindProcess(int(parentPid))
	require.NoError(t, parentPpErr)
	ensureProcessTree(t, ProcessTreeItem{parentPid, parentPp.CreationTime()}, 1, 5*time.Second)
	parentProcInfo, parentProcInfoErr := ps.FindProcess(int(parentPid))
	require.NoError(t, parentProcInfoErr)

	childCmd := exec.CommandContext(testCtx, "./delay", "--delay=30s")
	childCmd.Dir = delayToolDir
	childCmdErr := childCmd.Start()
	require.NoError(t, childCmdErr, "command should start without error")
	defer func() {
		_ = childCmd.Process.Kill()
		_ = childCmd.Wait()
	}()

	childPid, childPidErr := IntToPidT(childCmd.Process.Pid)
	require.NoError(t, childPidErr)
	childPp, childPpErr := ps.FindProcess(childCmd.Process.Pid)
	require.NoError(t, childPpErr)
	ensureProcessTree(t, ProcessTreeItem{childPid, childPp.CreationTime()}, 1, 5*time.Second)
	childProcInfo, childProcInfoErr := ps.FindProcess(int(childPid))
	require.NoError(t, childProcInfoErr)

	// Case 1: monitor start time does not match
	stdoutBuf := new(bytes.Buffer)
	stderrBuf := new(bytes.Buffer)
	monitorStartTime := parentProcInfo.CreationTime().Add(1 * time.Second)
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--child", strconv.Itoa(childCmd.Process.Pid),
		"--monitor-start-time", monitorStartTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--child-start-time", childProcInfo.CreationTime().Format(osutil.RFC3339MiliTimestampFormat),
	)
	dcpProcCmd.Stdout = stdoutBuf
	dcpProcCmd.Stderr = stderrBuf
	dcpProcCmdErr := dcpProcCmd.Start()
	require.NoError(t, dcpProcCmdErr, "command should start without error")

	dcpWaitErr := dcpProcCmd.Wait()
	require.Error(t, dcpWaitErr, "dcpproc should have exited with an error")
	require.True(t,
		strings.Contains(stderrBuf.String(), "process start time mismatch") && strings.Contains(stderrBuf.String(), "DCP process could not be monitored"),
		"dcpproc should have reported invalid DCP process start time",
	)

	// Case 2: child start time does not match
	stdoutBuf.Reset()
	stderrBuf.Reset()
	childStartTime := childProcInfo.CreationTime().Add(1 * time.Second)
	dcpProcCmd = exec.CommandContext(testCtx, dcpProc,
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--child", strconv.Itoa(childCmd.Process.Pid),
		"--monitor-start-time", parentProcInfo.CreationTime().Format(osutil.RFC3339MiliTimestampFormat),
		"--child-start-time", childStartTime.Format(osutil.RFC3339MiliTimestampFormat),
	)
	dcpProcCmd.Stdout = stdoutBuf
	dcpProcCmd.Stderr = stderrBuf
	dcpProcCmdErr = dcpProcCmd.Start()
	require.NoError(t, dcpProcCmdErr, "command should start without error")

	dcpWaitErr = dcpProcCmd.Wait()
	require.Error(t, dcpWaitErr, "dcpproc should have exited with an error")
	require.True(t,
		strings.Contains(stderrBuf.String(), "process start time mismatch") && strings.Contains(stderrBuf.String(), "child service process could not be monitored"),
		"dcpproc should have reported invalid child process start time")
}

func getDelayToolDir() (string, error) {
	return int_testutil.GetTestToolDir("delay")
}

func getDcpProcExecutablePath() (string, error) {
	dcpExeName := "dcpproc"
	if runtime.GOOS == "windows" {
		dcpExeName += ".exe"
	}

	outputBin, found := os.LookupEnv("OUTPUT_BIN")
	if found {
		dcpPath := filepath.Join(outputBin, dcpExeName)
		file, err := os.Stat(dcpPath)
		if err != nil {
			return "", fmt.Errorf("failed to find the DCP executable: %w", err)
		}
		if file.IsDir() {
			return "", fmt.Errorf("the expected path to DCP executable is a directory: %s", dcpPath)
		}
		return dcpPath, nil
	}

	tail := []string{dcppaths.DcpBinDir, dcppaths.DcpExtensionsDir, dcppaths.DcpBinDir, dcpExeName}
	rootFolder, err := testutil.FindRootFor(testutil.FileTarget, tail...)
	if err != nil {
		return "", err
	}

	return filepath.Join(append([]string{rootFolder}, tail...)...), nil
}

func ensureProcessTree(t *testing.T, rootP ProcessTreeItem, expectedSize int, timeout time.Duration) {
	processesStartedCtx, processesStartedCancelFn := context.WithTimeout(context.Background(), timeout)
	defer processesStartedCancelFn()

	var processTreeLen int
	err := wait.PollUntilContextCancel(
		processesStartedCtx,
		100*time.Millisecond,
		true, // Don't wait before polling for the first time
		func(_ context.Context) (bool, error) {
			processTree, err := GetProcessTree(rootP)
			if err != nil {
				return false, err
			}
			processTreeLen = len(processTree)
			return processTreeLen == expectedSize, nil
		},
	)

	require.NoError(t, err, "expected number of 'delay' program instances not found (%d/%d)", processTreeLen, expectedSize)
}
