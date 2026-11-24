//go:build linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that ProcessGroup checks only its live leader and enumerates members after the leader exits.
func TestProcessGroupLiveLeaderSkipsEnumeration(t *testing.T) {
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	t.Cleanup(cancelTest)
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	t.Cleanup(executor.Dispose)
	handle, _, _ := startDelayTreeForTest(t, testCtx, executor,
		delayCommandForTest(t, "--delay=120s", "--child-spec=1", "--couple-children"), 2)
	group, groupErr := FindProcessGroup(handle, false)
	require.NoError(t, groupErr)
	require.NotNil(t, group)

	procRoot := t.TempDir()
	leaderPID := fmt.Sprint(handle.Pid)
	leaderDirectory := filepath.Join(procRoot, leaderPID)
	require.NoError(t, os.Mkdir(leaderDirectory, 0700))
	require.NoError(t, os.Symlink(filepath.Join(processProcRoot(), leaderPID, "stat"), filepath.Join(leaderDirectory, "stat")))
	unrelatedDirectory := filepath.Join(procRoot, "1")
	require.NoError(t, os.Mkdir(unrelatedDirectory, 0700))
	require.NoError(t, usvc_io.WriteFile(filepath.Join(unrelatedDirectory, "stat"), []byte("malformed"), 0600))
	t.Setenv("HOST_PROC", procRoot)

	_, snapshotErr := snapshotProcesses(testCtx)
	require.Error(t, snapshotErr)
	running, runningErr := group.isRunning(testCtx)
	require.NoError(t, runningErr)
	require.True(t, running)

	require.NoError(t, executor.StopProcess(testCtx, handle, StopRootOnly()))
	_, afterExitErr := group.isRunning(testCtx)
	require.ErrorIs(t, afterExitErr, ErrIncompleteProcessTree)
	require.Contains(t, afterExitErr.Error(), "process enumeration was incomplete")
}

// Verifies that StopProcess stops verified escaped descendants and force-kills resistant group members
// while reporting a persistent group-inspection failure as incomplete cleanup.
func TestStopProcessCleansGroupDespiteInspectionFailure(t *testing.T) {
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	t.Cleanup(cancelTest)
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	t.Cleanup(executor.Dispose)
	root, rootTree, _ := startDelayTreeForTest(t, testCtx, executor,
		delayCommandForTest(t, "--delay=120s", "--child-spec=1", "--fork-children"), 2)
	rootPID, rootPIDErr := PidT_ToInt(root.Pid)
	require.NoError(t, rootPIDErr)
	memberCmd := delayCommandForTest(t, "--delay=120s", "--ignore-sigterm", "--child-spec=1", "--couple-children")
	memberCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: rootPID}
	_, memberTree, memberHandler := startDelayTreeForTest(t, testCtx, executor, memberCmd, 2)

	procRoot := t.TempDir()
	tree := append(rootTree, memberTree...)
	for _, mirrored := range tree {
		mirroredPID := fmt.Sprint(mirrored.Pid)
		mirroredDirectory := filepath.Join(procRoot, mirroredPID)
		require.NoError(t, os.Mkdir(mirroredDirectory, 0700))
		require.NoError(t, os.Symlink(filepath.Join(processProcRoot(), mirroredPID, "stat"), filepath.Join(mirroredDirectory, "stat")))
	}
	unrelatedDirectory := filepath.Join(procRoot, "1")
	require.NoError(t, os.Mkdir(unrelatedDirectory, 0700))
	require.NoError(t, usvc_io.WriteFile(filepath.Join(unrelatedDirectory, "stat"), []byte("malformed"), 0600))
	t.Setenv("HOST_PROC", procRoot)

	stopErr := executor.StopProcess(testCtx, root)
	require.ErrorIs(t, stopErr, ErrIncompleteProcessTree)
	require.Contains(t, stopErr.Error(), "process enumeration was incomplete")
	for _, stopped := range tree {
		require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(stopped)))
	}
	select {
	case <-memberHandler.Exited():
	case <-testCtx.Done():
		t.Fatal("force-stopped group member did not report exit")
	}
	require.Equal(t, int32(-1), memberHandler.ExitInfo().ExitCode)
	require.NoError(t, memberHandler.ExitInfo().Err)
}
