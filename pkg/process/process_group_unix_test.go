//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/wait"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that OSExecutor isolates ordinary child groups and preserves explicit Unix session setup.
func TestStartProcessIsolatesUnixProcessGroup(t *testing.T) {
	t.Parallel()

	for _, newSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("new-session=%t", newSession), func(t *testing.T) {
			t.Parallel()
			testCtx, cancelTest := testutil.GetTestContext(t, 0)
			defer cancelTest()
			executor := process.NewOSExecutor(log)
			t.Cleanup(executor.Dispose)

			cmd := exec.Command("sh", "-c", "read command")
			if newSession {
				process.ForkFromParent(cmd)
			}
			stdin, stdinErr := cmd.StdinPipe()
			require.NoError(t, stdinErr)
			t.Cleanup(func() { _ = stdin.Close() })
			handle, startWait, startErr := executor.StartProcess(testCtx, cmd, nil, process.CreationFlagsNone, nil)
			require.NoError(t, startErr)
			startWait()
			t.Cleanup(func() { stopGroupTestProcess(t, testCtx, executor, handle) })

			groupID, groupErr := unix.Getpgid(int(handle.Pid))
			require.NoError(t, groupErr)
			require.Equal(t, int(handle.Pid), groupID)
			require.NotEqual(t, unix.Getpgrp(), groupID)
			sessionID, sessionErr := unix.Getsid(int(handle.Pid))
			require.NoError(t, sessionErr)
			if newSession {
				require.Equal(t, int(handle.Pid), sessionID)
			} else {
				parentSession, parentSessionErr := unix.Getsid(0)
				require.NoError(t, parentSessionErr)
				require.Equal(t, parentSession, sessionID)
			}
		})
	}
}

// Verifies that StopProcess kills group members created by SIGTERM handling after tree discovery.
func TestStopProcessKillsLateGroupMember(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)
	handle, group, output, _ := startProcessGroupShell(t, testCtx, executor,
		`trap 'sleep 120 </dev/null & echo $!; exit 0' TERM; echo ready; read command`)
	require.Equal(t, "ready", readProcessGroupLine(t, testCtx, output))

	tree, treeErr := process.GetProcessTree(testCtx, handle)
	require.NoError(t, treeErr)
	require.Len(t, tree, 1)
	require.NoError(t, executor.StopProcess(testCtx, handle))

	latePID, latePIDErr := process.StringToPidT(readProcessGroupLine(t, testCtx, output))
	require.NoError(t, latePIDErr)
	require.NotEqual(t, handle.Pid, latePID)
	require.NoError(t, group.Wait(testCtx))
	requireUnixProcessesExited(t, testCtx, []process.ProcessHandle{{Pid: latePID}})
}

// Verifies that StopProcess escalates to group-wide SIGKILL when the leader exits but a member ignores SIGTERM.
func TestStopProcessKillsIgnoringGroupMemberAfterLeaderExit(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)
	handle, group, output, _ := startProcessGroupShell(t, testCtx, executor,
		`(trap '' TERM; echo ready; exec sleep 120) & echo $!; read command`)
	lines := []string{readProcessGroupLine(t, testCtx, output), readProcessGroupLine(t, testCtx, output)}
	require.Contains(t, lines, "ready")
	childPIDLine := lines[0]
	if childPIDLine == "ready" {
		childPIDLine = lines[1]
	}
	childPID, childPIDErr := process.StringToPidT(childPIDLine)
	require.NoError(t, childPIDErr)
	require.NoError(t, executor.StopProcess(testCtx, handle))
	require.NoError(t, group.Wait(testCtx))
	requireUnixProcessesExited(t, testCtx, []process.ProcessHandle{{Pid: childPID}})
}

// Verifies that StopProcess uses only tree cleanup for a root sharing the caller's Unix process group.
func TestStopProcessInSharedUnixGroupUsesTree(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)
	delayDir, delayDirErr := getDelayToolDir()
	require.NoError(t, delayDirErr)
	cmd := exec.Command("./delay", "--delay=120s", "--child-spec=1", "--couple-children")
	cmd.Dir = delayDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: unix.Getpgrp()}
	handle, startWait, startErr := executor.StartProcess(testCtx, cmd, nil, process.CreationFlagsNone, nil)
	require.NoError(t, startErr)
	startWait()
	t.Cleanup(func() { stopGroupTestProcess(t, testCtx, executor, handle) })
	group, groupErr := process.FindProcessGroup(handle)
	require.NoError(t, groupErr)
	require.Nil(t, group)
	testDeadline, haveTestDeadline := testCtx.Deadline()
	require.True(t, haveTestDeadline)
	int_testutil.EnsureProcessTree(t, handle, 2, time.Until(testDeadline))
	tree, treeErr := process.GetProcessTree(testCtx, handle)
	require.NoError(t, treeErr)
	require.NoError(t, executor.StopProcess(testCtx, handle))
	requireUnixProcessesExited(t, testCtx, tree)
}

// Verifies that StopProcess retains tree cleanup for descendants in separate groups and sessions.
func TestStopProcessCleansDescendantsOutsideGroup(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)
	delayDir, delayDirErr := getDelayToolDir()
	require.NoError(t, delayDirErr)
	cmd := exec.Command("./delay", "--delay=120s", "--child-spec=2,1", "--fork-children")
	cmd.Dir = delayDir
	handle, startWait, startErr := executor.StartProcess(testCtx, cmd, nil, process.CreationFlagsNone, nil)
	require.NoError(t, startErr)
	startWait()
	t.Cleanup(func() { stopGroupTestProcess(t, testCtx, executor, handle) })
	testDeadline, haveTestDeadline := testCtx.Deadline()
	require.True(t, haveTestDeadline)
	int_testutil.EnsureProcessTree(t, handle, 5, time.Until(testDeadline))

	tree, treeErr := process.GetProcessTree(testCtx, handle)
	require.NoError(t, treeErr)
	for _, child := range tree[1:] {
		childGroup, childGroupErr := unix.Getpgid(int(child.Pid))
		require.NoError(t, childGroupErr)
		require.NotEqual(t, int(handle.Pid), childGroup)
		childSession, childSessionErr := unix.Getsid(int(child.Pid))
		require.NoError(t, childSessionErr)
		require.Equal(t, int(child.Pid), childSession)
	}
	require.NoError(t, executor.StopProcess(testCtx, handle))
	requireUnixProcessesExited(t, testCtx, tree)
}

// Verifies that StopRootOnly leaves coupled descendants running and captured group cleanup still stops them.
func TestStopRootOnlySkipsUnixGroupCleanup(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)
	handle, group, output, _ := startProcessGroupShell(t, testCtx, executor,
		"sleep 120 </dev/null & echo $!; read command")
	childPID, childPIDErr := process.StringToPidT(readProcessGroupLine(t, testCtx, output))
	require.NoError(t, childPIDErr)
	childHandle, childHandleErr := executor.FindProcessHandle(childPID)
	require.NoError(t, childHandleErr)

	require.NoError(t, executor.StopProcess(testCtx, handle, process.StopRootOnly()))
	require.NoError(t, executor.CheckProcessRunning(childHandle))
	require.NoError(t, executor.StopProcess(testCtx, handle, process.StopWithProcessGroup(group)))
	requireUnixProcessesExited(t, testCtx, []process.ProcessHandle{childHandle})
}

// Verifies that a captured ProcessGroup survives leader exit and respects canceled waits.
func TestProcessGroupOutlivesLeader(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)
	handle, group, output, input := startProcessGroupShell(t, testCtx, executor,
		"sleep 120 </dev/null & echo $!; read command; exit 0")
	childPID, childPIDErr := process.StringToPidT(readProcessGroupLine(t, testCtx, output))
	require.NoError(t, childPIDErr)
	childHandle, childHandleErr := executor.FindProcessHandle(childPID)
	require.NoError(t, childHandleErr)
	_, writeErr := fmt.Fprintln(input, "exit")
	require.NoError(t, writeErr)
	requireUnixProcessesExited(t, testCtx, []process.ProcessHandle{handle})
	require.NoError(t, executor.CheckProcessRunning(childHandle))

	waitCtx, cancelWait := context.WithCancel(testCtx)
	cancelWait()
	require.ErrorIs(t, group.Wait(waitCtx), context.Canceled)
	require.NoError(t, executor.StopProcess(testCtx, handle, process.StopWithProcessGroup(group)))
	require.NoError(t, group.Wait(testCtx))
	requireUnixProcessesExited(t, testCtx, []process.ProcessHandle{childHandle})
}

// Verifies that group cleanup stops further signaling on caller cancellation, leaving surviving members addressable.
func TestStopProcessGroupRespectsCallerCancellation(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)
	handle, group, output, _ := startProcessGroupShell(t, testCtx, executor,
		`(trap '' TERM; echo ready; exec sleep 120) & echo $!; read command`)
	lines := []string{readProcessGroupLine(t, testCtx, output), readProcessGroupLine(t, testCtx, output)}
	require.Contains(t, lines, "ready")
	childPIDLine := lines[0]
	if childPIDLine == "ready" {
		childPIDLine = lines[1]
	}
	childPID, childPIDErr := process.StringToPidT(childPIDLine)
	require.NoError(t, childPIDErr)
	childHandle, childHandleErr := process.FindProcessHandle(childPID)
	require.NoError(t, childHandleErr)

	stopCtx, cancelStop := context.WithCancel(testCtx)
	defer cancelStop()
	stopResult := make(chan error, 1)
	go func() { stopResult <- executor.StopProcess(stopCtx, handle, process.StopWithProcessGroup(group)) }()
	requireUnixProcessesExited(t, testCtx, []process.ProcessHandle{handle})
	cancelStop()
	select {
	case <-testCtx.Done():
		t.Fatal("group stopping did not end after caller cancellation")
	case stopErr, received := <-stopResult:
		require.True(t, received)
		require.ErrorIs(t, stopErr, context.Canceled)
	}
	require.NoError(t, executor.CheckProcessRunning(childHandle))
	childProcess, childProcessErr := process.FindProcess(childHandle)
	require.NoError(t, childProcessErr)
	require.NoError(t, childProcess.Kill())
	require.NoError(t, childProcess.Release())
	require.NoError(t, group.Wait(testCtx))
}

// Verifies that group discovery and cleanup reject incorrect identities and mismatched cleanup handles.
func TestProcessGroupValidatesIdentity(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)
	handle, group, output, _ := startProcessGroupShell(t, testCtx, executor, "echo ready; read command")
	require.Equal(t, "ready", readProcessGroupLine(t, testCtx, output))
	wrongIdentity := process.NewHandle(handle.Pid, handle.IdentityTime.Add(-time.Minute))
	_, groupErr := process.FindProcessGroup(wrongIdentity)
	require.ErrorIs(t, groupErr, process.ErrProcessIdentityMismatch)
	require.Error(t, executor.StopProcess(testCtx, wrongIdentity, process.StopWithProcessGroup(group)))
	_, missingIdentityErr := process.FindProcessGroup(process.NewHandle(handle.Pid, time.Time{}))
	require.ErrorIs(t, missingIdentityErr, process.ErrInvalidProcessHandle)
	require.NoError(t, executor.CheckProcessRunning(handle))
	for _, invalidPID := range []process.Pid_t{process.UnknownPID, 0, 1} {
		_, invalidErr := process.FindProcessGroup(process.NewHandle(invalidPID, time.Time{}))
		require.Error(t, invalidErr)
	}
	emptyGroup := &process.ProcessGroup{}
	require.Error(t, emptyGroup.Wait(testCtx))
}

func startProcessGroupShell(
	t *testing.T,
	ctx context.Context,
	executor process.Executor,
	script string,
) (process.ProcessHandle, *process.ProcessGroup, *bufio.Reader, *os.File) {
	t.Helper()
	outputRead, outputWrite, outputErr := os.Pipe()
	require.NoError(t, outputErr)
	t.Cleanup(func() { _ = outputRead.Close(); _ = outputWrite.Close() })
	inputRead, inputWrite, inputErr := os.Pipe()
	require.NoError(t, inputErr)
	t.Cleanup(func() { _ = inputRead.Close(); _ = inputWrite.Close() })
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = inputRead
	cmd.Stdout = outputWrite
	cmd.Stderr = os.Stderr
	handle, startWait, startErr := executor.StartProcess(ctx, cmd, nil, process.CreationFlagsNone, nil)
	require.NoError(t, startErr)
	startWait()
	group, groupErr := process.FindProcessGroup(handle)
	require.NoError(t, groupErr)
	require.NotNil(t, group)
	t.Cleanup(func() { stopGroupTestProcess(t, ctx, executor, handle, process.StopWithProcessGroup(group)) })
	require.NoError(t, outputWrite.Close())
	require.NoError(t, inputRead.Close())
	return handle, group, bufio.NewReader(outputRead), inputWrite
}

func readProcessGroupLine(t *testing.T, ctx context.Context, reader *bufio.Reader) string {
	t.Helper()
	type lineResult struct {
		line string
		err  error
	}
	lines := make(chan lineResult, 1)
	go func() {
		line, readErr := reader.ReadString('\n')
		lines <- lineResult{line: line, err: readErr}
	}()
	select {
	case <-ctx.Done():
		t.Fatal("process output did not arrive before the test context expired")
		return ""
	case result, received := <-lines:
		require.True(t, received)
		require.NoError(t, result.err)
		return strings.TrimSpace(result.line)
	}
}

func requireUnixProcessesExited(t *testing.T, ctx context.Context, handles []process.ProcessHandle) {
	t.Helper()
	pollErr := wait.PollUntilContextCancel(ctx, 20*time.Millisecond, true, func(_ context.Context) (bool, error) {
		for _, handle := range handles {
			var inspectionErr error
			if handle.IdentityTime.IsZero() {
				_, inspectionErr = process.FindProcessHandle(handle.Pid)
			} else {
				proc, processErr := process.FindProcess(handle)
				inspectionErr = processErr
				if proc != nil {
					inspectionErr = errors.Join(inspectionErr, proc.Release())
				}
			}
			if process.IsProcessGoneErr(inspectionErr) {
				continue
			}
			return false, inspectionErr
		}
		return true, nil
	})
	require.NoError(t, pollErr, "processes should have exited")
}

func stopGroupTestProcess(t *testing.T, ctx context.Context, executor process.Executor, handle process.ProcessHandle, options ...process.ProcessStopOption) {
	t.Helper()
	cleanupCtx, cleanupCancel := process.WithDetachedStopTimeout(ctx)
	defer cleanupCancel()
	stopErr := executor.StopProcess(cleanupCtx, handle, options...)
	if stopErr != nil && !process.IsProcessGoneErr(stopErr) {
		t.Errorf("could not clean up process %d: %v", handle.Pid, stopErr)
	}
}
