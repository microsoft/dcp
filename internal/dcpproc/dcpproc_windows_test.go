/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build windows

package dcpproc_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

const (
	processQueryLimitedInfo = 0x1000
	stillActive             = 259
)

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
	const expectedCount = 4
	delayFlag := fmt.Sprintf("--delay=%s", testTimeout.String())
	childrenCmd := exec.CommandContext(testCtx, "./delay", delayFlag, "--child-spec=1,1", "--couple-children")
	childrenCmd.Dir = delayToolDir
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
	dcpProcCmd.Stdout = os.Stdout
	dcpProcCmd.Stderr = os.Stderr
	process.DecoupleFromParent(dcpProcCmd)
	require.NoError(t, dcpProcCmd.Start())
	require.NoError(t, dcpProcCmd.Wait(), "dcpproc stop-process-tree should exit cleanly (not killed by its own signal)")

	require.NoError(t, childrenCmd.Wait(), "root delay process should exit cleanly via SIGINT")

	// Wait for every process in the tree to exit, then assert all used exit code 0.
	// A non-zero code means the process was force-killed rather than interrupted gracefully.
	requireAllExitedWithCode(t, handles, 0, 10*time.Second)
}

// openProcessHandles opens a PROCESS_QUERY_LIMITED_INFORMATION handle for each process in
// the tree. Holding these handles prevents Windows from releasing the process objects before
// we can query their exit codes.
func openProcessHandles(t *testing.T, tree []process.ProcessTreeItem) map[process.Pid_t]syscall.Handle {
	t.Helper()
	handles := make(map[process.Pid_t]syscall.Handle, len(tree))
	for _, item := range tree {
		osPid, err := process.PidT_ToUint32(item.Pid)
		if err != nil {
			continue
		}
		handle, err := syscall.OpenProcess(processQueryLimitedInfo, false, osPid)
		if err != nil {
			// The process may have already exited by the time we try to open it.
			t.Logf("Could not open handle for PID %d (may have already exited): %v", osPid, err)
			continue
		}
		handles[item.Pid] = handle
	}
	return handles
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
			for _, h := range handles {
				var code uint32
				if err := syscall.GetExitCodeProcess(h, &code); err != nil || code == stillActive {
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
