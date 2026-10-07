//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc_test

import (
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that monitor-process survives group leader exit, cleans up on owner exit, and finishes when the group empties.
func TestMonitorProcessTracksGroupAfterLeaderExit(t *testing.T) {
	t.Parallel()

	for _, ownerExits := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner-exits=%t", ownerExits), func(t *testing.T) {
			t.Parallel()
			testCtx, cancelTest := testutil.GetTestContext(t, 0)
			defer cancelTest()
			executor := process.NewOSExecutor(testutil.NewLogForTesting(t.Name()))
			t.Cleanup(executor.Dispose)
			delayDir, delayDirErr := int_testutil.GetTestToolDir("delay")
			require.NoError(t, delayDirErr)
			dcpPath, dcpPathErr := getDcpProcExecutablePath()
			require.NoError(t, dcpPathErr)

			ownerCmd := exec.Command("./delay", "--delay=120s")
			ownerCmd.Dir = delayDir
			ownerHandle, waitOwner, ownerStartErr := executor.StartProcess(testCtx, ownerCmd, nil, process.CreationFlagsNone, nil)
			require.NoError(t, ownerStartErr)
			waitOwner()
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := process.WithDetachedStopTimeout(testCtx)
				defer cleanupCancel()
				stopErr := executor.StopProcess(cleanupCtx, ownerHandle)
				require.True(t, stopErr == nil || process.IsProcessGoneErr(stopErr), "owner cleanup failed: %v", stopErr)
			})

			childCmd := exec.Command("./delay", "--delay=120s", "--child-spec=1", "--couple-children")
			childCmd.Dir = delayDir
			childHandle, waitChild, childStartErr := executor.StartProcess(testCtx, childCmd, nil, process.CreationFlagsNone, nil)
			require.NoError(t, childStartErr)
			waitChild()
			group, groupErr := process.FindProcessGroup(childHandle)
			require.NoError(t, groupErr)
			require.NotNil(t, group)
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := process.WithDetachedStopTimeout(testCtx)
				defer cleanupCancel()
				require.NoError(t, executor.StopProcess(cleanupCtx, childHandle, process.StopWithProcessGroup(group)))
			})
			testDeadline, haveTestDeadline := testCtx.Deadline()
			require.True(t, haveTestDeadline)
			int_testutil.EnsureProcessTree(t, childHandle, 2, time.Until(testDeadline))
			tree, treeErr := process.GetProcessTree(testCtx, childHandle)
			require.NoError(t, treeErr)
			require.Len(t, tree, 2)
			member := tree[1]

			var output dcpProcOutput
			defer output.DumpOnFailure(t, "monitor-process")
			monitorCmd := exec.CommandContext(testCtx, dcpPath,
				"monitor-process",
				"--monitor", fmt.Sprint(ownerHandle.Pid),
				"--monitor-identity-time", ownerHandle.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
				"--child", fmt.Sprint(childHandle.Pid),
				"--child-identity-time", childHandle.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
				"--monitor-interval", "1",
			)
			monitorCmd.Stdout = output.StdoutWriter()
			monitorCmd.Stderr = output.StderrWriter()
			process.DecoupleFromParent(monitorCmd)
			require.NoError(t, monitorCmd.Start())
			monitorResult := make(chan error, 1)
			go func() {
				monitorResult <- monitorCmd.Wait()
				close(monitorResult)
			}()
			t.Cleanup(func() {
				_ = monitorCmd.Process.Kill()
				<-monitorResult
			})
			require.NoError(t, output.WaitForStderrSubstring(testCtx, "Started monitoring process group"))

			require.NoError(t, executor.StopProcess(testCtx, childHandle, process.StopRootOnly()))
			require.NoError(t, executor.CheckProcessRunning(member))
			if ownerExits {
				require.NoError(t, executor.StopProcess(testCtx, ownerHandle))
			} else {
				require.NoError(t, executor.StopProcess(testCtx, member, process.StopRootOnly()))
			}
			select {
			case <-testCtx.Done():
				t.Fatal("monitor-process did not finish before the test context expired")
			case monitorErr, received := <-monitorResult:
				require.True(t, received)
				require.NoError(t, monitorErr)
			}
			require.NoError(t, group.Wait(testCtx))
			if !ownerExits {
				require.NoError(t, executor.CheckProcessRunning(ownerHandle))
			}
		})
	}
}
