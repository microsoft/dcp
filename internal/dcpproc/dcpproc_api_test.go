/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/dcppaths"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestRunProcessWatcher(t *testing.T) {
	log := testutil.NewLogForTesting(t.Name())
	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	pe := internal_testutil.NewTestProcessExecutor(ctx)
	dcppaths.EnableTestPathProbing()
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr, "Could not determine DCP executable path")

	testPid := process.Pid_t(28869)
	testStartTime := time.Now()

	RunProcessWatcher(pe, process.NewProcessHandle(testPid, testStartTime), log)

	dcpProc, dcpProcErr := findRunningDcp(pe)
	require.NoError(t, dcpProcErr)

	require.True(t, len(dcpProc.Cmd.Args) >= 8, "Command should have at least 8 arguments")
	require.Equal(t, dcpProc.Cmd.Args[0], dcpPath, "Should execute dcp")
	require.Equal(t, "monitor-process", dcpProc.Cmd.Args[1], "Should use 'monitor-process' subcommand")

	require.Equal(t, dcpProc.Cmd.Args[2], "--child", "Should include --child flag")
	require.Equal(t, dcpProc.Cmd.Args[3], strconv.FormatInt(int64(testPid), 10), "Should include child PID")
	require.Equal(t, dcpProc.Cmd.Args[4], "--child-identity-time", "Should include --child-identity-time flag")
	require.Equal(t, dcpProc.Cmd.Args[5], testStartTime.Format(osutil.RFC3339MiliTimestampFormat), "Should include formatted child start time")
	require.Equal(t, dcpProc.Cmd.Args[6], "--monitor", "Should include --monitor flag")
	require.Equal(t, dcpProc.Cmd.Args[7], strconv.FormatInt(int64(os.Getpid()), 10), "Should include current process PID as monitored PID")
}

func TestRunContainerWatcher(t *testing.T) {
	log := testutil.NewLogForTesting(t.Name())
	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	pe := internal_testutil.NewTestProcessExecutor(ctx)
	dcppaths.EnableTestPathProbing()
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr, "Could not determine DCP executable path")

	testContainerID := "test-container-123"

	RunContainerWatcher(pe, testContainerID, log)

	dcpProc, dcpProcErr := findRunningDcp(pe)
	require.NoError(t, dcpProcErr)

	require.True(t, len(dcpProc.Cmd.Args) >= 5, "Command should have at least 5 arguments")
	require.Equal(t, dcpProc.Cmd.Args[0], dcpPath, "Should execute dcp")
	require.Equal(t, "monitor-container", dcpProc.Cmd.Args[1], "Should use 'monitor-container' subcommand")

	require.Equal(t, dcpProc.Cmd.Args[2], "--containerID", "Should include --containerID flag")
	require.Equal(t, dcpProc.Cmd.Args[3], testContainerID, "Should include container ID")
	require.Equal(t, dcpProc.Cmd.Args[4], "--monitor", "Should include --monitor flag")
	require.Equal(t, dcpProc.Cmd.Args[5], strconv.FormatInt(int64(os.Getpid()), 10), "Should include current process PID as monitored PID")
}

func TestStopProcessTree(t *testing.T) {
	log := testutil.NewLogForTesting(t.Name())
	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	pex := internal_testutil.NewTestProcessExecutor(ctx)
	dcppaths.EnableTestPathProbing()
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr, "Could not determine DCP executable path")

	testCmdPath := fmt.Sprintf("/usr/bin/%s", t.Name())
	testCmd := exec.Command(testCmdPath, "arg1", "arg2")
	pex.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{testCmdPath},
		},
		RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
			select {
			case <-ctx.Done():
				return 1 // Timeout -- the process should have been stopped instead -- report failure.
			case <-pe.Signal:
				return 0
			}
		},
	})

	handle, startErr := pex.StartAndForget(testCmd, process.CreationFlagsNone)
	require.NoError(t, startErr, "Could not simulate starting test process")
	testProc, found := pex.FindByPid(handle.Pid)
	require.True(t, found, "Could not find the started process")

	var dcpProc *internal_testutil.ProcessExecution
	pex.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{dcpPath},
		},
		RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
			dcpProc = pe
			return SimulateStopProcessTreeCommand(pe)
		},
	})

	stopProcessTreeErr := StopProcessTree(ctx, pex, handle, log)
	require.NoError(t, stopProcessTreeErr, "Could not stop the process tree")
	require.True(t, testProc.Finished(), "The test processed should have been stopped")

	require.NotNil(t, dcpProc, "dcp stop-process-tree should have been invoked")
	require.True(t, len(dcpProc.Cmd.Args) >= 5, "Command should have at least 5 arguments")
	require.Equal(t, dcpProc.Cmd.Args[0], dcpPath, "Should execute dcp")
	require.Equal(t, "stop-process-tree", dcpProc.Cmd.Args[1], "Should use 'stop-process-tree' subcommand")

	require.Equal(t, dcpProc.Cmd.Args[2], "--pid", "Should include --pid flag")
	require.Equal(t, dcpProc.Cmd.Args[3], strconv.FormatInt(int64(handle.Pid), 10), "Should include test process ID")
	require.Equal(t, dcpProc.Cmd.Args[4], "--process-start-time", "Should include --process-start-time flag")
	require.Equal(t, dcpProc.Cmd.Args[5], handle.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat), "Should include formatted process start time")
}

func findRunningDcp(pe *internal_testutil.TestProcessExecutor) (*internal_testutil.ProcessExecution, error) {
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	if dcpPathErr != nil {
		return nil, fmt.Errorf("could not determine DCP executable path: %w", dcpPathErr)
	}

	candidates := pe.FindAll([]string{dcpPath}, "", func(pe *internal_testutil.ProcessExecution) bool {
		return pe.Running()
	})
	if len(candidates) == 0 {
		return nil, errors.New("no running dcp process found")
	}
	if len(candidates) > 1 {
		return nil, errors.New("multiple running dcp processes found")
	}
	return candidates[0], nil
}
