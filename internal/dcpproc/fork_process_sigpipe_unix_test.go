//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc_test

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

var forkProcessPidLogPattern = regexp.MustCompile(`"PID":\s*([0-9]+)`)

// Verifies that cleanup-enabled fork-process converts a real broken PID-output pipe into an
// ordinary write error, runs deferred cleanup, and does not leave the launched child alive.
func TestForkProcessCleanupHandlesClosedPidPipe(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()
	_, owner := startCleanupOwner(t, testCtx)
	dcpPath, dcpPathErr := getDcpProcExecutablePath()
	require.NoError(t, dcpPathErr)
	delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	readPipe, writePipe, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	require.NoError(t, readPipe.Close())
	command := exec.CommandContext(
		testCtx,
		dcpPath,
		"fork-process",
		"--monitor", strconv.FormatInt(int64(owner.Pid), 10),
		"--monitor-identity-time", owner.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--enable-child-cleanup",
		"--",
		delayPath,
		"--delay=2m",
	)
	process.ForkFromParent(command)
	command.Stdout = writePipe
	var stderr bytes.Buffer
	command.Stderr = &stderr
	require.NoError(t, command.Start())
	require.NoError(t, writePipe.Close())
	runErr := command.Wait()
	var exitErr *exec.ExitError
	require.ErrorAs(t, runErr, &exitErr, stderr.String())
	require.Equal(t, 1, exitErr.ExitCode(), stderr.String())
	waitStatus, hasWaitStatus := exitErr.Sys().(syscall.WaitStatus)
	require.True(t, hasWaitStatus)
	require.False(t, waitStatus.Signaled(), stderr.String())
	childPID := failedForkProcessPID(t, stderr.String())
	childHandle, childHandleErr := process.FindProcessHandle(childPID)
	if process.IsProcessGoneErr(childHandleErr) {
		return
	}
	require.NoError(t, childHandleErr)
	t.Cleanup(func() { stopCleanupProcess(testCtx, childHandle) })
	waitCleanupProcessGone(t, testCtx, childHandle)
}

// Verifies that cleanup-enabled fork-process survives a stderr pipe closed before monitoring or
// after PID handoff, then cleans the child when the monitored owner exits.
func TestForkProcessCleanupHandlesClosedLoggingPipe(t *testing.T) {
	for _, closeBeforeMonitor := range []bool{false, true} {
		t.Run(fmt.Sprintf("before-monitor=%t", closeBeforeMonitor), func(t *testing.T) {
			testCtx, testCancel := testutil.GetTestContext(t, 0)
			defer testCancel()
			ownerCmd, owner := startCleanupOwner(t, testCtx)
			dcpPath, dcpPathErr := getDcpProcExecutablePath()
			require.NoError(t, dcpPathErr)
			delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
			require.NoError(t, delayPathErr)
			stderrRead, stderrWrite, pipeErr := os.Pipe()
			require.NoError(t, pipeErr)
			command := exec.CommandContext(
				testCtx,
				dcpPath,
				"fork-process",
				"--monitor", strconv.FormatInt(int64(owner.Pid), 10),
				"--monitor-identity-time", owner.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
				"--monitor-interval", "1",
				"--enable-child-cleanup",
				"--",
				delayPath,
				"--delay=2m",
			)
			process.ForkFromParent(command)
			stdoutPipe, stdoutPipeErr := command.StdoutPipe()
			require.NoError(t, stdoutPipeErr)
			command.Stderr = stderrWrite
			if closeBeforeMonitor {
				require.NoError(t, stderrRead.Close())
			}
			require.NoError(t, command.Start())
			require.NoError(t, stderrWrite.Close())
			stdoutLine, stdoutReadErr := bufio.NewReader(stdoutPipe).ReadString('\n')
			require.NoError(t, stdoutReadErr)
			childPID := parseForkedPid(t, stdoutLine)
			childHandle, childHandleErr := process.FindProcessHandle(childPID)
			require.NoError(t, childHandleErr)
			t.Cleanup(func() { stopCleanupProcess(testCtx, childHandle) })
			if !closeBeforeMonitor {
				require.NoError(t, stderrRead.Close())
			}
			require.NoError(t, ownerCmd.Process.Kill())
			waitCleanupProcessGone(t, testCtx, owner)
			require.NoError(t, command.Wait())
			waitCleanupProcessGone(t, testCtx, childHandle)
		})
	}
}

// Verifies that cleanup-disabled fork-process retains standard Unix command-line SIGPIPE
// behavior rather than changing the existing default mode process-wide.
func TestForkProcessDefaultModeRetainsSIGPIPEBehavior(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()
	dcpPath, dcpPathErr := getDcpProcExecutablePath()
	require.NoError(t, dcpPathErr)
	delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	readPipe, writePipe, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	require.NoError(t, readPipe.Close())
	command := exec.CommandContext(testCtx, dcpPath, "fork-process", "--", delayPath, "--delay=10ms")
	process.ForkFromParent(command)
	command.Stdout = writePipe
	command.Stderr = &bytes.Buffer{}
	require.NoError(t, command.Start())
	require.NoError(t, writePipe.Close())
	runErr := command.Wait()
	var exitErr *exec.ExitError
	require.ErrorAs(t, runErr, &exitErr)
	waitStatus, hasWaitStatus := exitErr.Sys().(syscall.WaitStatus)
	require.True(t, hasWaitStatus)
	require.True(t, waitStatus.Signaled())
	require.Equal(t, syscall.SIGPIPE, waitStatus.Signal())
}

func failedForkProcessPID(t *testing.T, stderr string) process.Pid_t {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "Failed to write forked process PID") {
			continue
		}
		match := forkProcessPidLogPattern.FindStringSubmatch(line)
		require.Len(t, match, 2, "PID log was malformed: %s", line)
		pid, parseErr := strconv.ParseInt(match[1], 10, 64)
		require.NoError(t, parseErr)
		return process.Pid_t(pid)
	}
	require.FailNow(t, "PID output failure was not logged", stderr)
	return process.UnknownPID
}
