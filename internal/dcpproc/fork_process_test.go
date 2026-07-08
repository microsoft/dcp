/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestForkProcessStartsDetachedCommand(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	t.Cleanup(testCancel)

	forkedProcess := startForkedDelay(t, testCtx)

	require.False(t, forkedProcess.IdentityTime.IsZero(), "forked process start time should not be zero")
	checkExecutor := process.NewOSExecutor(testutil.NewLogForTesting(t.Name()))
	defer checkExecutor.Dispose()
	require.NoError(t, checkExecutor.CheckProcessRunning(forkedProcess))
}

func TestForkProcessExitsWithChildExitCode(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	t.Cleanup(testCancel)

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolPath, toolPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, toolPathErr)

	cmdArgs := forkProcessArgsForCurrentProcess(t, delayToolPath, "--delay=100ms", "--exit-code=7")
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc, cmdArgs...)
	var stdout, stderr bytes.Buffer
	dcpProcCmd.Stdout = &stdout
	dcpProcCmd.Stderr = &stderr

	runErr := dcpProcCmd.Run()
	var exitErr *exec.ExitError
	require.True(t, errors.As(runErr, &exitErr), "dcp fork-process should exit with the child exit code; stderr: %s", stderr.String())
	require.Equal(t, 7, exitErr.ExitCode(), "dcp fork-process should exit with the child exit code; stderr: %s", stderr.String())

	stdoutText := strings.TrimSpace(stdout.String())
	_ = parseForkedPid(t, stdoutText)
}

func TestForkProcessExitsWhenMonitoredParentExits(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	t.Cleanup(testCancel)

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolPath, toolPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, toolPathErr)

	parentCmd := exec.CommandContext(testCtx, delayToolPath, "--delay=30s")
	require.NoError(t, parentCmd.Start(), "parent command should start without error")
	t.Cleanup(func() {
		_ = parentCmd.Process.Kill()
		_ = parentCmd.Wait()
	})

	parentHandle := process.ProcessHandleFromCmd(parentCmd)
	require.False(t, parentHandle.IdentityTime.IsZero(), "parent process start time should not be zero")

	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"fork-process",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--monitor-identity-time", parentHandle.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--monitor-interval", "1",
		"--",
		delayToolPath,
		"--delay=30s",
	)
	stdoutPipe, pipeErr := dcpProcCmd.StdoutPipe()
	require.NoError(t, pipeErr)
	var stderr bytes.Buffer
	dcpProcCmd.Stderr = &stderr

	require.NoError(t, dcpProcCmd.Start(), "dcp fork-process should start without error")

	stdoutText, readErr := bufio.NewReader(stdoutPipe).ReadString('\n')
	require.NoError(t, readErr, "dcp fork-process should print a child PID; stderr: %s", stderr.String())
	childPid := parseForkedPid(t, stdoutText)
	childIdentityTime := process.ProcessIdentityTime(childPid)
	require.False(t, childIdentityTime.IsZero(), "forked process %d should be running", childPid)

	cleanupExecutor := process.NewOSExecutor(testutil.NewLogForTesting(t.Name()))
	t.Cleanup(func() {
		_ = cleanupExecutor.StopProcess(process.NewHandle(childPid, childIdentityTime))
		cleanupExecutor.Dispose()
	})

	require.NoError(t, parentCmd.Process.Kill(), "parent command should stop without error")
	_ = parentCmd.Wait()

	require.NoError(t, dcpProcCmd.Wait(), "dcp fork-process should exit cleanly when the monitored parent exits; stderr: %s", stderr.String())
	require.NoError(t, cleanupExecutor.CheckProcessRunning(process.NewHandle(childPid, childIdentityTime)), "fork-process should not stop the detached child")
}

func startForkedDelay(t *testing.T, testCtx context.Context) process.ProcessHandle {
	t.Helper()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolPath, toolPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, toolPathErr)

	cmdArgs := append([]string{"fork-process", "--", delayToolPath}, "--delay=30s")
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc, cmdArgs...)
	var stdout, stderr bytes.Buffer
	dcpProcCmd.Stdout = &stdout
	dcpProcCmd.Stderr = &stderr

	runErr := dcpProcCmd.Run()
	require.NoError(t, runErr, "dcp fork-process should exit cleanly; stderr: %s", stderr.String())

	pid := parseForkedPid(t, stdout.String())
	var identityTime time.Time
	cleanupExecutor := process.NewOSExecutor(testutil.NewLogForTesting(t.Name()))
	t.Cleanup(func() {
		_ = cleanupExecutor.StopProcess(process.NewHandle(pid, identityTime))
		cleanupExecutor.Dispose()
	})

	identityTime = process.ProcessIdentityTime(pid)
	require.False(t, identityTime.IsZero(), "forked process %d should still be running", pid)

	return process.NewHandle(pid, identityTime)
}

func forkProcessArgsForCurrentProcess(t *testing.T, childArgs ...string) []string {
	t.Helper()

	currentPid := process.Pid_t(os.Getpid())
	currentIdentityTime := process.ProcessIdentityTime(currentPid)
	require.False(t, currentIdentityTime.IsZero(), "current process start time should not be zero")

	args := []string{
		"fork-process",
		"--monitor", fmt.Sprint(os.Getpid()),
		"--monitor-identity-time", currentIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--",
	}
	args = append(args, childArgs...)
	return args
}

func parseForkedPid(t *testing.T, stdoutText string) process.Pid_t {
	t.Helper()

	childPid, parseErr := strconv.ParseInt(strings.TrimSpace(stdoutText), 10, 64)
	require.NoError(t, parseErr, "dcp fork-process should print only a child PID, got %q", stdoutText)

	return process.Pid_t(childPid)
}
