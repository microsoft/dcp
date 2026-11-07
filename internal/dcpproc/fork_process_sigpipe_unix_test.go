//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
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
	if childHandleErr == nil {
		t.Cleanup(func() { stopCleanupProcess(testCtx, childHandle) })
	}
	require.True(t, process.IsProcessGoneErr(childHandleErr), "child remained alive after fork-process exited: %v", childHandleErr)
}

// Verifies that cleanup-enabled fork-process exits and stops its child when PID output blocks on a full pipe.
func TestForkProcessCleanupHandlesFullPidPipe(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()
	ownerCmd, owner := startCleanupOwner(t, testCtx)
	dcpPath, dcpPathErr := getDcpProcExecutablePath()
	require.NoError(t, dcpPathErr)
	delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	stdoutRead, stdoutWrite, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	defer stdoutRead.Close()
	fillForkProcessPipe(t, stdoutWrite)
	readyPath := filepath.Join(t.TempDir(), "child-ready")
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
		"--ready-file="+readyPath,
	)
	process.ForkFromParent(command)
	command.Stdout = stdoutWrite
	var stderr bytes.Buffer
	command.Stderr = &stderr
	require.NoError(t, command.Start())
	commandHandle, commandHandleErr := process.ProcessHandleFromCmd(command)
	require.NoError(t, commandHandleErr)
	require.NoError(t, stdoutWrite.Close())
	waitCleanupFile(t, testCtx, readyPath)
	tree, treeErr := process.GetProcessTree(testCtx, commandHandle)
	require.NoError(t, treeErr)
	require.Len(t, tree, 2)
	childHandle := tree[1]
	t.Cleanup(func() { stopCleanupProcess(testCtx, childHandle) })
	require.NoError(t, ownerCmd.Process.Kill())
	require.NoError(t, command.Wait(), stderr.String())
	executor := process.NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(childHandle)))
}

// Verifies that a full stderr pipe cannot delay owner-exit cleanup after PID handoff.
func TestForkProcessCleanupHandlesFullLoggingPipe(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()
	ownerCmd, owner := startCleanupOwner(t, testCtx)
	dcpPath, dcpPathErr := getDcpProcExecutablePath()
	require.NoError(t, dcpPathErr)
	delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	stderrRead, stderrWrite, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	t.Cleanup(func() {
		_ = stderrRead.Close()
		_ = stderrWrite.Close()
	})
	command := exec.CommandContext(
		testCtx,
		dcpPath,
		"-v=debug",
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
	require.NoError(t, command.Start())
	stdoutLine, stdoutReadErr := bufio.NewReader(stdoutPipe).ReadString('\n')
	require.NoError(t, stdoutReadErr)
	childPID := parseForkedPid(t, stdoutLine)
	childHandle, childHandleErr := process.FindProcessHandle(childPID)
	require.NoError(t, childHandleErr)
	t.Cleanup(func() { stopCleanupProcess(testCtx, childHandle) })
	fillResultCh := startForkProcessPipeFill(stderrWrite)
	require.NoError(t, ownerCmd.Process.Kill())
	waitCleanupProcessGone(t, testCtx, childHandle)
	require.NoError(t, stderrRead.Close())
	select {
	case fillErr := <-fillResultCh:
		require.Error(t, fillErr)
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	require.NoError(t, stderrWrite.Close())
	require.NoError(t, command.Wait())
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
			executor := process.NewOSExecutor(logr.Discard())
			defer executor.Dispose()
			require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(childHandle)))
		})
	}
}

// Verifies that cleanup-enabled fork-process preserves a natural nonzero child exit code when final stderr logging hits a closed pipe.
func TestForkProcessCleanupPreservesExitCodeWithClosedLoggingPipe(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()
	_, owner := startCleanupOwner(t, testCtx)
	dcpPath, dcpPathErr := getDcpProcExecutablePath()
	require.NoError(t, dcpPathErr)
	delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	stderrRead, stderrWrite, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	require.NoError(t, stderrRead.Close())
	command := exec.CommandContext(
		testCtx,
		dcpPath,
		"fork-process",
		"--monitor", strconv.FormatInt(int64(owner.Pid), 10),
		"--monitor-identity-time", owner.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--enable-child-cleanup",
		"--",
		delayPath,
		"--delay=10ms",
		"--exit-code=7",
	)
	process.ForkFromParent(command)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = stderrWrite
	require.NoError(t, command.Start())
	require.NoError(t, stderrWrite.Close())
	runErr := command.Wait()
	var exitErr *exec.ExitError
	require.ErrorAs(t, runErr, &exitErr)
	require.Equal(t, 7, exitErr.ExitCode())
	waitStatus, hasWaitStatus := exitErr.Sys().(syscall.WaitStatus)
	require.True(t, hasWaitStatus)
	require.False(t, waitStatus.Signaled())
	_ = parseForkedPid(t, stdout.String())
}

// Verifies that process-lifetime SIGPIPE protection preserves natural successful child exits
// when debug monitor logging races with a closed stderr pipe.
func TestForkProcessCleanupPreservesSuccessfulExitWithClosedLoggingPipe(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()
	_, owner := startCleanupOwner(t, testCtx)
	dcpPath, dcpPathErr := getDcpProcExecutablePath()
	require.NoError(t, dcpPathErr)
	delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	for range 32 {
		stderrRead, stderrWrite, pipeErr := os.Pipe()
		require.NoError(t, pipeErr)
		command := exec.CommandContext(
			testCtx,
			dcpPath,
			"-v=debug",
			"fork-process",
			"--monitor", strconv.FormatInt(int64(owner.Pid), 10),
			"--monitor-identity-time", owner.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
			"--enable-child-cleanup",
			"--",
			delayPath,
			"--delay=10ms",
		)
		process.ForkFromParent(command)
		stdoutPipe, stdoutPipeErr := command.StdoutPipe()
		require.NoError(t, stdoutPipeErr)
		command.Stderr = stderrWrite
		require.NoError(t, command.Start())
		require.NoError(t, stderrWrite.Close())
		stdoutLine, stdoutReadErr := bufio.NewReader(stdoutPipe).ReadString('\n')
		require.NoError(t, stdoutReadErr)
		_ = parseForkedPid(t, stdoutLine)
		require.NoError(t, stderrRead.Close())
		require.NoError(t, command.Wait())
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

func fillForkProcessPipe(t *testing.T, pipe *os.File) {
	t.Helper()
	require.NoError(t, pipe.SetWriteDeadline(time.Now().Add(time.Second)))
	payload := make([]byte, 4096)
	for {
		_, fillErr := pipe.Write(payload)
		if errors.Is(fillErr, os.ErrDeadlineExceeded) {
			break
		}
		require.NoError(t, fillErr)
	}
	require.NoError(t, pipe.SetWriteDeadline(time.Time{}))
}

func startForkProcessPipeFill(pipe *os.File) <-chan error {
	startedCh := make(chan struct{})
	resultCh := make(chan error, 1)
	go func() {
		close(startedCh)
		_, writeErr := pipe.Write(make([]byte, 16*1024*1024))
		resultCh <- writeErr
	}()
	<-startedCh
	return resultCh
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
