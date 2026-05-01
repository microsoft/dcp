//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc_test

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	ps "github.com/shirou/gopsutil/v4/process"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
	"k8s.io/apimachinery/pkg/util/wait"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

const expectedDelayProcesses = 3

// TestStopProcessTreeDeliversSIGINT verifies that all processes in the target tree receive
// SIGINT (and therefore exit with code 0) rather than being force-killed (exit code 1) when
// stop-process-tree is invoked against a process that has its own console (ForkFromParent).
//
// The key behavior under test: stopViaConsole attaches to the target's console and sends
// CTRL_C_EVENT to process group 0, which Windows delivers as SIGINT to every process sharing
// that console, including the root's children and grandchildren.
func TestStopProcessTreeDeliversSIGINT(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
	require.NoError(t, toolLaunchErr)

	const testTimeout = time.Second * 30
	testCtx, testCancel := context.WithTimeout(context.Background(), testTimeout)
	defer testCancel()

	// ForkFromParent gives the delay tree its own isolated console so that:
	//   1. AttachConsole(rootPid) succeeds deterministically in stopViaConsole.
	//   2. CTRL_C_EVENT with PID 0 reaches exactly the delay tree, not the test process.
	// child-spec=1,1 -> root + 1 child + 1 grandchild; +1 for conhost = 4 total.
	const expectedCount = expectedDelayProcesses + 1
	delayFlag := fmt.Sprintf("--delay=%s", testTimeout.String())
	childrenCmd := exec.CommandContext(testCtx, "./delay", delayFlag, "--child-spec=1,1", "--couple-children")
	childrenCmd.Dir = delayToolDir
	var childrenStdout, childrenStderr bytes.Buffer
	childrenCmd.Stdout = &childrenStdout
	childrenCmd.Stderr = &childrenStderr
	defer logCommandOutput(t, "delay process tree", &childrenStdout, &childrenStderr)

	process.ForkFromParent(childrenCmd)
	require.NoError(t, childrenCmd.Start(), "delay tree should start without error")

	pid := process.Uint32_ToPidT(uint32(childrenCmd.Process.Pid))
	childIdentityTime := process.ProcessIdentityTime(pid)
	require.False(t, childIdentityTime.IsZero(), "process identity time should not be zero")

	int_testutil.EnsureProcessTree(
		t,
		process.ProcessTreeItem{Pid: pid, IdentityTime: childIdentityTime},
		expectedCount,
		testTimeout/3,
	)

	// Snapshot the full tree and open handles BEFORE stopping so that the process objects
	// remain queryable (via GetExitCodeProcess) even after the processes terminate.
	tree, treeErr := process.GetProcessTree(process.ProcessTreeItem{Pid: pid, IdentityTime: childIdentityTime})
	require.NoError(t, treeErr)

	handles := openProcessHandles(t, tree)
	defer closeHandles(handles)

	// Run stop-process-tree.
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"stop-process-tree",
		"--pid", strconv.Itoa(childrenCmd.Process.Pid),
		"--process-start-time", childIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
	)
	var dcpProcStdout, dcpProcStderr bytes.Buffer
	dcpProcCmd.Stdout = &dcpProcStdout
	dcpProcCmd.Stderr = &dcpProcStderr
	defer logCommandOutput(t, "dcpproc stop-process-tree", &dcpProcStdout, &dcpProcStderr)

	process.DecoupleFromParent(dcpProcCmd)
	require.NoError(t, dcpProcCmd.Start())
	require.NoError(t, dcpProcCmd.Wait(), "dcpproc stop-process-tree should exit cleanly (not killed by its own signal)")

	require.NoError(t, childrenCmd.Wait(), "root delay process should exit cleanly via SIGINT")

	// Wait for every delay process in the tree to exit, then assert all used exit code 0.
	// A non-zero code means the process was force-killed rather than interrupted gracefully.
	requireAllExitedWithCode(t, handles, 0, 10*time.Second)
}

func TestStopProcessTreeSkipDescendantsLeavesForkedChildRunning(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
	require.NoError(t, toolLaunchErr)

	const testTimeout = time.Second * 30
	testCtx, testCancel := context.WithTimeout(context.Background(), testTimeout)
	defer testCancel()

	delayFlag := fmt.Sprintf("--delay=%s", testTimeout.String())
	rootCmd := exec.CommandContext(testCtx, "./delay", delayFlag, "--child-spec=1", "--fork-children")
	rootCmd.Dir = delayToolDir
	var rootStdout, rootStderr bytes.Buffer
	rootCmd.Stdout = &rootStdout
	rootCmd.Stderr = &rootStderr
	defer logCommandOutput(t, "delay root process", &rootStdout, &rootStderr)

	process.ForkFromParent(rootCmd)
	require.NoError(t, rootCmd.Start(), "delay root process should start without error")

	rootPid := process.Uint32_ToPidT(uint32(rootCmd.Process.Pid))
	rootIdentityTime := process.ProcessIdentityTime(rootPid)
	require.False(t, rootIdentityTime.IsZero(), "root process identity time should not be zero")
	rootItem := process.ProcessTreeItem{Pid: rootPid, IdentityTime: rootIdentityTime}

	forkedChild := requireDelayDescendant(t, rootItem, testTimeout/3)
	cleanupExecutor := process.NewOSExecutor(logr.Discard())
	defer func() {
		_ = cleanupExecutor.StopProcess(forkedChild.Pid, forkedChild.IdentityTime)
		_ = cleanupExecutor.StopProcess(rootItem.Pid, rootItem.IdentityTime)
		_ = rootCmd.Wait()
	}()

	childHandle := openProcessHandle(t, forkedChild)
	defer func() { _ = syscall.CloseHandle(childHandle) }()

	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"stop-process-tree",
		"--pid", strconv.Itoa(rootCmd.Process.Pid),
		"--process-start-time", rootIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--skip-descendants",
	)
	var dcpProcStdout, dcpProcStderr bytes.Buffer
	dcpProcCmd.Stdout = &dcpProcStdout
	dcpProcCmd.Stderr = &dcpProcStderr
	defer logCommandOutput(t, "dcpproc stop-process-tree --skip-descendants", &dcpProcStdout, &dcpProcStderr)

	process.DecoupleFromParent(dcpProcCmd)
	require.NoError(t, dcpProcCmd.Start())
	require.NoError(t, dcpProcCmd.Wait(), "dcpproc stop-process-tree should exit cleanly")

	requireProcessStillActive(t, childHandle, forkedChild.Pid)
}

func logCommandOutput(t *testing.T, name string, stdout *bytes.Buffer, stderr *bytes.Buffer) {
	t.Helper()

	if !t.Failed() && !testing.Verbose() {
		return
	}

	if stdout.Len() > 0 {
		t.Logf("%s stdout:\n%s", name, stdout.String())
	}
	if stderr.Len() > 0 {
		t.Logf("%s stderr:\n%s", name, stderr.String())
	}
}

// openProcessHandles opens a PROCESS_QUERY_LIMITED_INFORMATION handle for each delay process
// in the tree. Holding these handles prevents Windows from releasing the process objects before
// we can query their exit codes. The console host process is intentionally skipped.
func openProcessHandles(t *testing.T, tree []process.ProcessTreeItem) map[process.Pid_t]syscall.Handle {
	t.Helper()
	handles := make(map[process.Pid_t]syscall.Handle, len(tree))
	for _, item := range tree {
		osPid, processName := getProcessInfo(t, item)
		if strings.EqualFold(processName, "conhost.exe") {
			t.Logf("Skipping console host PID %d", osPid)
			continue
		}

		require.True(t, strings.EqualFold(processName, "delay.exe") || strings.EqualFold(processName, "delay"),
			"unexpected process %q with PID %d in delay process tree", processName, osPid)

		handles[item.Pid] = openProcessHandle(t, item)
	}

	require.Len(t, handles, expectedDelayProcesses, "expected handles for every delay process in the tree")
	return handles
}

func getProcessInfo(t *testing.T, item process.ProcessTreeItem) (uint32, string) {
	t.Helper()

	osPid, processName, processInfoErr := tryGetProcessInfo(item)
	require.NoError(t, processInfoErr, "could not inspect process PID %d", item.Pid)

	return osPid, processName
}

func tryGetProcessInfo(item process.ProcessTreeItem) (uint32, string, error) {
	osPid, pidErr := process.PidT_ToUint32(item.Pid)
	if pidErr != nil {
		return 0, "", fmt.Errorf("could not convert PID %d to Windows PID: %w", item.Pid, pidErr)
	}

	psProcess, processErr := ps.NewProcess(int32(osPid))
	if processErr != nil {
		return 0, "", fmt.Errorf("could not inspect process PID %d: %w", osPid, processErr)
	}

	processName, nameErr := psProcess.Name()
	if nameErr != nil {
		return 0, "", fmt.Errorf("could not get process name for PID %d: %w", osPid, nameErr)
	}

	return osPid, processName, nil
}

func openProcessHandle(t *testing.T, item process.ProcessTreeItem) syscall.Handle {
	t.Helper()

	osPid, pidErr := process.PidT_ToUint32(item.Pid)
	require.NoError(t, pidErr, "could not convert PID %d to Windows PID", item.Pid)

	handle, openErr := syscall.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, osPid)
	require.NoError(t, openErr, "could not open handle for delay process PID %d", osPid)
	return handle
}

func requireDelayDescendant(t *testing.T, root process.ProcessTreeItem, timeout time.Duration) process.ProcessTreeItem {
	t.Helper()

	waitCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var descendant process.ProcessTreeItem
	pollErr := wait.PollUntilContextCancel(waitCtx, 100*time.Millisecond, true,
		func(_ context.Context) (bool, error) {
			tree, treeErr := process.GetProcessTree(root)
			if treeErr != nil {
				return false, treeErr
			}

			for _, item := range tree[1:] {
				_, processName, processInfoErr := tryGetProcessInfo(item)
				if processInfoErr != nil {
					continue
				}
				if strings.EqualFold(processName, "delay.exe") || strings.EqualFold(processName, "delay") {
					descendant = item
					return true, nil
				}
			}

			return false, nil
		},
	)
	require.NoError(t, pollErr, "delay descendant was not found in the process tree")
	return descendant
}

func requireProcessStillActive(t *testing.T, handle syscall.Handle, pid process.Pid_t) {
	t.Helper()

	var code uint32
	require.NoError(t, syscall.GetExitCodeProcess(handle, &code), "could not get exit code for PID %d", pid)
	require.Equal(t, uint32(windows.STATUS_PENDING), code, "PID %d should still be running", pid)
}

func closeHandles(handles map[process.Pid_t]syscall.Handle) {
	for _, h := range handles {
		_ = syscall.CloseHandle(h)
	}
}

// requireAllExitedWithCode polls until every held process has exited (or timeout elapses),
// then asserts that each one's exit code equals expected.
func requireAllExitedWithCode(t *testing.T, handles map[process.Pid_t]syscall.Handle, expected uint32, timeout time.Duration) {
	t.Helper()

	waitCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pollErr := wait.PollUntilContextCancel(waitCtx, 100*time.Millisecond, true,
		func(_ context.Context) (bool, error) {
			for pid, h := range handles {
				var code uint32
				if err := syscall.GetExitCodeProcess(h, &code); err != nil {
					return false, fmt.Errorf("could not get exit code for PID %d: %w", pid, err)
				}
				if code == uint32(windows.STATUS_PENDING) {
					return false, nil
				}
			}
			return true, nil
		},
	)
	require.NoError(t, pollErr, "not all processes in the tree exited within the timeout")

	for pid, h := range handles {
		var code uint32
		require.NoError(t, syscall.GetExitCodeProcess(h, &code), "could not get exit code for PID %d", pid)

		require.Equal(t, expected, code,
			"PID %d should have exited with code %d (graceful SIGINT) but exited with code %d (force-killed)",
			pid, expected, code)
	}
}
