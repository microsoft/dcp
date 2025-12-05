// Copyright (c) Microsoft Corporation. All rights reserved.

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

	"github.com/microsoft/usvc-apiserver/internal/dcppaths"
	internal_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func TestRunProcessWatcher(t *testing.T) {
	log := testutil.NewLogForTesting(t.Name())
	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	pe := internal_testutil.NewTestProcessExecutor(ctx)
	dcppaths.EnableTestPathProbing()
	dcpProcPath, dcpProcPathErr := geDcpProcPath()
	require.NoError(t, dcpProcPathErr, "Could not resolve path to dcpproc executable")

	testPid := process.Pid_t(28869)
	testStartTime := time.Now()

	RunProcessWatcher(pe, testPid, testStartTime, log)

	dcpProc, dcpProcErr := findRunningDcpProc(pe)
	require.NoError(t, dcpProcErr)

	require.True(t, len(dcpProc.Cmd.Args) >= 8, "Command should have at least 8 arguments")
	require.Equal(t, dcpProc.Cmd.Args[0], dcpProcPath, "Should execute dcpproc")
	require.Equal(t, "process", dcpProc.Cmd.Args[1], "Should use 'process' subcommand")

	require.Equal(t, dcpProc.Cmd.Args[2], "--child", "Should include --child flag")
	require.Equal(t, dcpProc.Cmd.Args[3], strconv.FormatInt(int64(testPid), 10), "Should include child PID")
	require.Equal(t, dcpProc.Cmd.Args[4], "--child-start-time", "Should include --child-start-time flag")
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
	dcpProcPath, dcpProcPathErr := geDcpProcPath()
	require.NoError(t, dcpProcPathErr, "Could not resolve path to dcpproc executable")

	testContainerID := "test-container-123"

	RunContainerWatcher(pe, testContainerID, log)

	dcpProc, dcpProcErr := findRunningDcpProc(pe)
	require.NoError(t, dcpProcErr)

	require.True(t, len(dcpProc.Cmd.Args) >= 5, "Command should have at least 5 arguments")
	require.Equal(t, dcpProc.Cmd.Args[0], dcpProcPath, "Should execute dcpproc")
	require.Equal(t, "container", dcpProc.Cmd.Args[1], "Should use 'container' subcommand")

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
	dcpProcPath, dcpProcPathErr := geDcpProcPath()
	require.NoError(t, dcpProcPathErr, "Could not resolve path to dcpproc executable")

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

	pid, startTime, startErr := pex.StartAndForget(testCmd, process.CreationFlagsNone)
	require.NoError(t, startErr, "Could not simulate starting test process")
	testProc, found := pex.FindByPid(pid)
	require.True(t, found, "Could not find the started process")

	var dcpProc *internal_testutil.ProcessExecution
	pex.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{dcpProcPath},
		},
		RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
			dcpProc = pe
			return SimulateStopProcessTreeCommand(pe)
		},
	})

	stopProcessTreeErr := StopProcessTree(ctx, pex, pid, startTime, log)
	require.NoError(t, stopProcessTreeErr, "Could not stop the process tree")
	require.True(t, testProc.Finished(), "The test processed should have been stopped")

	require.NotNil(t, dcpProc, "dcpproc stop-process-tree should have been invoked")
	require.True(t, len(dcpProc.Cmd.Args) >= 5, "Command should have at least 5 arguments")
	require.Equal(t, dcpProc.Cmd.Args[0], dcpProcPath, "Should execute dcpproc")
	require.Equal(t, "stop-process-tree", dcpProc.Cmd.Args[1], "Should use 'stop-process-tree' subcommand")

	require.Equal(t, dcpProc.Cmd.Args[2], "--pid", "Should include --pid flag")
	require.Equal(t, dcpProc.Cmd.Args[3], strconv.FormatInt(int64(pid), 10), "Should include test process ID")
	require.Equal(t, dcpProc.Cmd.Args[4], "--process-start-time", "Should include --process-start-time flag")
	require.Equal(t, dcpProc.Cmd.Args[5], startTime.Format(osutil.RFC3339MiliTimestampFormat), "Should include formatted process start time")
}

func findRunningDcpProc(pe *internal_testutil.TestProcessExecutor) (*internal_testutil.ProcessExecution, error) {
	dpProcPath, dcpProcPathErr := geDcpProcPath()
	if dcpProcPathErr != nil {
		return nil, dcpProcPathErr
	}

	candidates := pe.FindAll([]string{dpProcPath}, "", func(pe *internal_testutil.ProcessExecution) bool {
		return pe.Running()
	})
	if len(candidates) == 0 {
		return nil, errors.New("No running dcpproc process found")
	}
	if len(candidates) > 1 {
		return nil, errors.New("Multiple running dcpproc processes found")
	}
	return candidates[0], nil
}
