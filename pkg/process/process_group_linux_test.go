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
	group, groupErr := FindProcessGroup(handle)
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
	require.Error(t, afterExitErr)
	require.Contains(t, afterExitErr.Error(), "could not enumerate process group")
}
