//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcpproc"
	int_testutil "github.com/microsoft/dcp/internal/testutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that monitor-process tracks groups even when started after leader exit, cleans up on owner exit or interruption,
// and reports persistent cleanup uncertainty or finishes when the group empties.
func TestMonitorProcessTracksGroupAfterLeaderExit(t *testing.T) {
	dcppaths.EnableTestPathProbing()
	t.Parallel()

	for _, testCase := range []struct {
		shutdown            string
		inspectionError     bool
		leaderAlreadyExited bool
	}{
		{"group-empty", false, false},
		{"owner-exit", false, false},
		{"monitor-interrupt", false, false},
		{"group-empty", false, true},
		{"owner-exit", false, true},
		{"monitor-interrupt", false, true},
		{"helper-stop", false, true},
		{"owner-exit", true, false},
		{"monitor-interrupt", true, false},
	} {
		if testCase.inspectionError && runtime.GOOS != "linux" {
			continue
		}
		t.Run(fmt.Sprintf("%s/inspection-error=%t/leader-exited=%t", testCase.shutdown, testCase.inspectionError, testCase.leaderAlreadyExited), func(t *testing.T) {
			t.Parallel()
			testCtx, cancelTest := testutil.GetTestContext(t, 0)
			t.Cleanup(cancelTest)
			executor := process.NewOSExecutor(testutil.NewLogForTesting(t.Name()))
			t.Cleanup(executor.Dispose)
			var cleanupHandles []process.ProcessHandle
			t.Cleanup(func() {
				for _, handle := range cleanupHandles {
					proc, findErr := handle.OsProcess()
					if process.IsProcessGoneErr(findErr) {
						continue
					}
					require.NoError(t, findErr)
					killErr := proc.Kill()
					releaseErr := proc.Release()
					require.True(t, killErr == nil || process.IsProcessGoneErr(killErr), "could not clean up delay: %v", killErr)
					require.NoError(t, releaseErr)
				}
			})
			delayDir, delayDirErr := int_testutil.GetTestToolDir("delay")
			require.NoError(t, delayDirErr)
			dcpPath, dcpPathErr := getDcpProcExecutablePath()
			require.NoError(t, dcpPathErr)

			ownerCmd := exec.Command("./delay", "--delay=120s")
			ownerCmd.Dir = delayDir
			ownerHandle, waitOwner, ownerStartErr := executor.StartProcess(testCtx, ownerCmd, nil, process.CreationFlagsNone, nil)
			require.NoError(t, ownerStartErr)
			waitOwner()
			cleanupHandles = append(cleanupHandles, ownerHandle)

			childCmd := exec.Command("./delay", "--delay=120s", "--child-spec=1", "--couple-children")
			childCmd.Dir = delayDir
			childHandle, waitChild, childStartErr := executor.StartProcess(testCtx, childCmd, nil, process.CreationFlagsNone, nil)
			require.NoError(t, childStartErr)
			waitChild()
			cleanupHandles = append(cleanupHandles, childHandle)
			testDeadline, haveTestDeadline := testCtx.Deadline()
			require.True(t, haveTestDeadline)
			int_testutil.EnsureProcessTree(t, childHandle, 2, time.Until(testDeadline))
			tree, treeErr := process.GetProcessTree(testCtx, childHandle)
			require.NoError(t, treeErr)
			require.Len(t, tree, 2)
			member := tree[1]
			cleanupHandles = append(cleanupHandles, member)
			if testCase.leaderAlreadyExited {
				require.NoError(t, executor.StopProcess(testCtx, childHandle, process.StopRootOnly()))
				require.NoError(t, executor.CheckProcessRunning(member))
			}

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
			if testCase.leaderAlreadyExited {
				monitorCmd.Args = append(monitorCmd.Args, "--child-process-group")
			}
			monitorCmd.Stdout = output.StdoutWriter()
			monitorCmd.Stderr = output.StderrWriter()
			if testCase.inspectionError {
				procRoot := t.TempDir()
				for _, mirroredHandle := range []process.ProcessHandle{ownerHandle, childHandle, member} {
					mirroredPID := fmt.Sprint(mirroredHandle.Pid)
					mirroredDirectory := filepath.Join(procRoot, mirroredPID)
					require.NoError(t, os.Mkdir(mirroredDirectory, 0700))
					sourceStat := filepath.Join(osutil.EnvVarStringWithDefault("HOST_PROC", "/proc"), mirroredPID, "stat")
					require.NoError(t, os.Symlink(sourceStat, filepath.Join(mirroredDirectory, "stat")))
				}
				unrelatedDirectory := filepath.Join(procRoot, "1")
				require.NoError(t, os.Mkdir(unrelatedDirectory, 0700))
				faultyStatPath := filepath.Join(unrelatedDirectory, "stat")
				require.NoError(t, usvc_io.WriteFile(faultyStatPath, []byte("malformed"), 0600))
				monitorCmd.Env = append(os.Environ(), "HOST_PROC="+procRoot)
			}
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

			if !testCase.leaderAlreadyExited {
				require.NoError(t, executor.StopProcess(testCtx, childHandle, process.StopRootOnly()))
				require.NoError(t, output.WaitForStderrSubstring(testCtx, "Monitor process exited, shutting down"))
			}
			require.NoError(t, executor.CheckProcessRunning(member))
			// Observe another monitoring interval before triggering shutdown of the live group.
			require.Never(t, func() bool {
				select {
				case monitorErr, received := <-monitorResult:
					t.Logf("monitor exited with a live member: received=%t, error=%v", received, monitorErr)
					return true
				default:
					return false
				}
			}, 2*time.Second, 50*time.Millisecond, "monitor must remain active while the owner and a group member are alive")
			require.NotContains(t, output.Stderr(), "monitoring remains active until cancellation", "readable live members must keep group inspection active")
			require.NoError(t, executor.CheckProcessRunning(ownerHandle))
			require.NoError(t, executor.CheckProcessRunning(member))
			switch testCase.shutdown {
			case "owner-exit":
				require.NoError(t, executor.StopProcess(testCtx, ownerHandle))
			case "group-empty":
				require.NoError(t, executor.StopProcess(testCtx, member, process.StopRootOnly()))
			case "monitor-interrupt":
				require.NoError(t, monitorCmd.Process.Signal(syscall.SIGTERM))
			case "helper-stop":
				require.NoError(t, dcpproc.StopProcessTree(testCtx, executor, childHandle, testutil.NewLogForTesting(t.Name())))
			}
			select {
			case <-testCtx.Done():
				t.Fatal("monitor-process did not finish before the test context expired")
			case monitorErr, received := <-monitorResult:
				require.True(t, received)
				if testCase.inspectionError {
					require.Error(t, monitorErr)
					require.Contains(t, output.Stderr(), "one or more descendant processes could not be confirmed stopped")
				} else {
					require.NoError(t, monitorErr)
				}
			}

			require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(member)), "monitor returned before its group member stopped")
			if testCase.shutdown != "owner-exit" {
				require.NoError(t, executor.CheckProcessRunning(ownerHandle))
			}
		})
	}
}

// Verifies that known-group command flags require the original leader identity rather than resolving a new identity.
func TestProcessGroupCommandsRequireOriginalIdentity(t *testing.T) {
	t.Parallel()

	dcpPath, dcpPathErr := getDcpProcExecutablePath()
	require.NoError(t, dcpPathErr)
	for _, testCase := range []struct {
		args    []string
		message string
	}{
		{[]string{"monitor-process", "--child", "1", "--monitor", "1", "--child-process-group"}, "--child-process-group requires --child-identity-time"},
		{[]string{"stop-process-tree", "--pid", "1", "--process-group"}, "--process-group requires --process-start-time"},
	} {
		t.Run(testCase.args[0], func(t *testing.T) {
			t.Parallel()
			testCtx, cancelTest := testutil.GetTestContext(t, 0)
			t.Cleanup(cancelTest)
			command := exec.CommandContext(testCtx, dcpPath, testCase.args...)
			output, commandErr := command.CombinedOutput()
			require.Error(t, commandErr)
			require.Contains(t, string(output), testCase.message)
		})
	}
}

// Verifies that monitor-process exits cleanly on a reused child-group leader PID with a live or exited owner,
// without waiting for the owner or signaling the replacement process and its descendants.
func TestMonitorProcessTreatsGroupPIDReuseAsCompletion(t *testing.T) {
	t.Parallel()

	for _, ownerExited := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner-exited=%t", ownerExited), func(t *testing.T) {
			t.Parallel()
			testCtx, cancelTest := testutil.GetTestContext(t, 0)
			t.Cleanup(cancelTest)
			executor := process.NewOSExecutor(testutil.NewLogForTesting(t.Name()))
			t.Cleanup(executor.Dispose)
			delayDir, delayDirErr := int_testutil.GetTestToolDir("delay")
			require.NoError(t, delayDirErr)
			dcpPath, dcpPathErr := getDcpProcExecutablePath()
			require.NoError(t, dcpPathErr)

			ownerCmd := exec.Command("./delay", "--delay=120s")
			ownerCmd.Dir = delayDir
			owner, waitOwner, ownerStartErr := executor.StartProcess(testCtx, ownerCmd, nil, process.CreationFlagsNone, nil)
			require.NoError(t, ownerStartErr)
			waitOwner()
			replacementCmd := exec.Command("./delay", "--delay=120s", "--child-spec=1", "--couple-children")
			replacementCmd.Dir = delayDir
			replacement, waitReplacement, replacementStartErr := executor.StartProcess(testCtx, replacementCmd, nil, process.CreationFlagsNone, nil)
			require.NoError(t, replacementStartErr)
			waitReplacement()
			testDeadline, haveTestDeadline := testCtx.Deadline()
			require.True(t, haveTestDeadline)
			int_testutil.EnsureProcessTree(t, replacement, 2, time.Until(testDeadline))
			tree, treeErr := process.GetProcessTree(testCtx, replacement)
			require.NoError(t, treeErr)
			require.Len(t, tree, 2)
			t.Cleanup(func() {
				for _, handle := range append(tree, owner) {
					cleanupCtx, cleanupCancel := process.WithDetachedStopTimeout(testCtx)
					cleanupErr := executor.StopProcess(cleanupCtx, handle, process.StopRootOnly())
					cleanupCancel()
					require.True(t, cleanupErr == nil || process.IsProcessGoneErr(cleanupErr), "could not clean up delay: %v", cleanupErr)
				}
			})
			if ownerExited {
				require.NoError(t, executor.StopProcess(testCtx, owner, process.StopRootOnly()))
			}

			originalIdentity := replacement.IdentityTime.Add(-time.Minute)
			var output dcpProcOutput
			defer output.DumpOnFailure(t, "monitor-process PID reuse")
			monitorCmd := exec.CommandContext(testCtx, dcpPath,
				"monitor-process",
				"--monitor", fmt.Sprint(owner.Pid),
				"--monitor-identity-time", owner.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
				"--child", fmt.Sprint(replacement.Pid),
				"--child-identity-time", originalIdentity.Format(osutil.RFC3339MiliTimestampFormat),
				"--child-process-group",
			)
			monitorCmd.Stdout = output.StdoutWriter()
			monitorCmd.Stderr = output.StderrWriter()
			require.NoError(t, monitorCmd.Run())
			require.NotContains(t, output.Stderr(), "Failed to stop child")
			require.NotContains(t, output.Stderr(), "monitoring remains active until cancellation")
			if ownerExited {
				require.Contains(t, output.Stderr(), "Monitored process already exited, shutting down child process")
			} else {
				require.Contains(t, output.Stderr(), "Started monitoring process group")
				require.NoError(t, executor.CheckProcessRunning(owner))
			}
			for _, untouched := range tree {
				require.NoError(t, executor.CheckProcessRunning(untouched))
			}
		})
	}
}
