// Copyright (c) Microsoft Corporation. All rights reserved.

package dcpproc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/dcppaths"
	int_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	"github.com/microsoft/usvc-apiserver/internal/testutil/ctrlutil"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func TestMonitorProcessExitsWithErrorForInvalidPid(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	// Set a reasonable timeout that gives the wait polling time to see the delay process exit
	testCtx, testCancel := context.WithTimeout(context.Background(), time.Second*30)
	defer testCancel()

	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"process",
		"--monitor", "aaa",
		"--child", "bbb",
	)
	err := dcpProcCmd.Run()
	require.Error(t, err)
}

func TestMonitorProcessTerminatesWatchedProcesses(t *testing.T) {
	type testcase struct {
		description        string
		prepareChildCmd    func(*exec.Cmd)
		expectedChildCount int
	}

	// One parent, one child, and one grandchild for a total of 3 processes.
	const childSpecFlag = "--child-spec=1,1"
	getExpectedChildCount := func(useForkFromParent bool) int {
		if useForkFromParent && osutil.IsWindows() {
			return 4 // Account for "conhost" process hosting separate console for the child process tree
		} else {
			return 3
		}
	}

	testCases := []testcase{
		{
			description:        "decouple from parent",
			prepareChildCmd:    process.DecoupleFromParent,
			expectedChildCount: getExpectedChildCount(false),
		},
		{
			description:        "fork from parent",
			prepareChildCmd:    process.ForkFromParent,
			expectedChildCount: getExpectedChildCount(true),
		},
	}

	t.Parallel()

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			dcpProc, dcpProcErr := getDcpProcExecutablePath()
			require.NoError(t, dcpProcErr)

			delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
			require.NoError(t, toolLaunchErr)

			// Set a reasonable timeout that ensures we can see all expected processes exit before their delay time
			const testTimeout = time.Second * 30
			testCtx, testCancel := context.WithTimeout(context.Background(), testTimeout)
			defer testCancel()

			// All commands will return on its own after 30 seconds.
			// This prevents the test from launching a bunch of processes that turn into zombies.
			delayFlag := fmt.Sprintf("--delay=%s", testTimeout.String())

			parentCmd := exec.CommandContext(testCtx, "./delay", delayFlag)
			parentCmd.Dir = delayToolDir
			process.DecoupleFromParent(parentCmd)
			parentCmdErr := parentCmd.Start()
			require.NoError(t, parentCmdErr, "command should start without error")

			parentPid := process.Uint32_ToPidT(uint32(parentCmd.Process.Pid))
			parentCreateTime := process.StartTimeForProcess(parentPid)
			require.False(t, parentCreateTime.IsZero(), "parent process start time should not be zero")
			int_testutil.EnsureProcessTree(t, process.ProcessTreeItem{Pid: parentPid, CreationTime: parentCreateTime}, 1, testTimeout/3)

			childrenCmd := exec.CommandContext(testCtx, "./delay", delayFlag, childSpecFlag)
			childrenCmd.Dir = delayToolDir
			tc.prepareChildCmd(childrenCmd)
			childrenCmdErr := childrenCmd.Start()
			require.NoError(t, childrenCmdErr, "command should start without error")

			pid := process.Uint32_ToPidT(uint32(childrenCmd.Process.Pid))
			childCreateTime := process.StartTimeForProcess(pid)
			require.False(t, childCreateTime.IsZero(), "child process start time should not be zero")
			int_testutil.EnsureProcessTree(
				t,
				process.ProcessTreeItem{Pid: pid, CreationTime: childCreateTime},
				tc.expectedChildCount,
				testTimeout/3,
			)

			dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
				"process",
				"--monitor", strconv.Itoa(parentCmd.Process.Pid),
				"--child", strconv.Itoa(childrenCmd.Process.Pid),
				"--monitor-start-time", parentCreateTime.Format(osutil.RFC3339MiliTimestampFormat),
				"--child-start-time", childCreateTime.Format(osutil.RFC3339MiliTimestampFormat),
			)
			dcpProcCmd.Stdout = os.Stdout
			dcpProcCmd.Stderr = os.Stderr
			process.DecoupleFromParent(dcpProcCmd)
			dcpProcCmdErr := dcpProcCmd.Start()
			require.NoError(t, dcpProcCmdErr, "command should start without error")

			// Give enough time for the monitor process to start before killing the parent process
			<-time.After(testTimeout / 4)

			killErr := parentCmd.Process.Kill()
			require.NoError(t, killErr)
			_ = parentCmd.Wait()

			childErr := childrenCmd.Wait()
			require.NoError(t, childErr) // dcpproc should terminate processes gracefully
			require.True(t, childrenCmd.ProcessState.Exited(), "child process should have exited")

			dcpWaitErr := dcpProcCmd.Wait()
			require.NoError(t, dcpWaitErr)
		})
	}
}

func TestMonitorProcessNotMonitoredIfStartTimeDoesNotMatch(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "linux" {
		t.Skip("Skipping test on Linux because process start time is not available")
	}

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
	require.NoError(t, toolLaunchErr)

	// Set a reasonable timeout that ensures we can see all expected processes exit before their delay time
	const testTimeout = time.Second * 30
	testCtx, testCancel := context.WithTimeout(context.Background(), testTimeout)
	defer testCancel()

	// All commands will return on its own after 30 seconds.
	// This prevents the test from launching a bunch of processes that turn into zombies.
	delayFlag := fmt.Sprintf("--delay=%s", testTimeout.String())

	parentCmd := exec.CommandContext(testCtx, "./delay", delayFlag)
	parentCmd.Dir = delayToolDir
	parentCmdErr := parentCmd.Start()
	require.NoError(t, parentCmdErr, "command should start without error")
	defer func() {
		_ = parentCmd.Process.Kill()
		_ = parentCmd.Wait()
	}()

	parentPid := process.Uint32_ToPidT(uint32(parentCmd.Process.Pid))
	parentCreateTime := process.StartTimeForProcess(parentPid)
	require.False(t, parentCreateTime.IsZero(), "parent process start time should not be zero")
	int_testutil.EnsureProcessTree(t, process.ProcessTreeItem{Pid: parentPid, CreationTime: parentCreateTime}, 1, testTimeout/3)

	childCmd := exec.CommandContext(testCtx, "./delay", delayFlag)
	childCmd.Dir = delayToolDir
	childCmdErr := childCmd.Start()
	require.NoError(t, childCmdErr, "command should start without error")
	defer func() {
		_ = childCmd.Process.Kill()
		_ = childCmd.Wait()
	}()

	childPid := process.Uint32_ToPidT(uint32(childCmd.Process.Pid))
	childCreateTime := process.StartTimeForProcess(childPid)
	require.False(t, childCreateTime.IsZero(), "child process start time should not be zero")
	int_testutil.EnsureProcessTree(t, process.ProcessTreeItem{Pid: childPid, CreationTime: childCreateTime}, 1, testTimeout/3)

	// Case 1: monitor start time does not match
	stdoutBuf := new(bytes.Buffer)
	stderrBuf := new(bytes.Buffer)
	monitorStartTime := parentCreateTime.Add(1 * time.Second)
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"process",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--child", strconv.Itoa(childCmd.Process.Pid),
		"--monitor-start-time", monitorStartTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--child-start-time", childCreateTime.Format(osutil.RFC3339MiliTimestampFormat),
	)
	dcpProcCmd.Stdout = stdoutBuf
	dcpProcCmd.Stderr = stderrBuf
	dcpProcCmdErr := dcpProcCmd.Start()
	require.NoError(t, dcpProcCmdErr, "command should start without error")

	dcpWaitErr := dcpProcCmd.Wait()
	require.Error(t, dcpWaitErr, "dcpproc should have exited with an error")
	require.True(t,
		strings.Contains(stderrBuf.String(), "process start time mismatch") && strings.Contains(stderrBuf.String(), "Process could not be monitored"),
		"dcpproc should have reported invalid DCP process start time",
	)

	// Case 2: child start time does not match
	stdoutBuf.Reset()
	stderrBuf.Reset()
	childStartTime := childCreateTime.Add(1 * time.Second)
	dcpProcCmd = exec.CommandContext(testCtx, dcpProc,
		"process",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--child", strconv.Itoa(childCmd.Process.Pid),
		"--monitor-start-time", parentCreateTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--child-start-time", childStartTime.Format(osutil.RFC3339MiliTimestampFormat),
	)
	dcpProcCmd.Stdout = stdoutBuf
	dcpProcCmd.Stderr = stderrBuf
	dcpProcCmdErr = dcpProcCmd.Start()
	require.NoError(t, dcpProcCmdErr, "command should start without error")

	dcpWaitErr = dcpProcCmd.Wait()
	require.NoError(t, dcpWaitErr, "dcpproc should have exited without an error")
	require.True(t,
		strings.Contains(stderrBuf.String(), "process start time mismatch") && strings.Contains(stderrBuf.String(), "Child process could not be monitored"),
		"dcpproc should have reported invalid child process start time")
}

func TestMonitorContainerTerminatesWatchedContainer(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr, "DCPPROC path should not be found")

	delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
	require.NoError(t, toolLaunchErr, "'delay' tool directory could not be found")

	const testTimeout = time.Second * 30
	testCtx, testCancel := context.WithTimeout(context.Background(), testTimeout)
	defer testCancel()

	log := testutil.NewLogForTesting(t.Name())
	tco, tcoErr := ctrlutil.NewTestContainerOrchestrator(testCtx, log, ctrlutil.TcoOptionEnableSocketListener)
	require.NoError(t, tcoErr)
	defer func() {
		closeErr := tco.Close()
		require.NoError(t, closeErr)
	}()

	const containerName = "test-monitor-terminates-watched-container"
	containerID, createErr := tco.CreateContainer(testCtx, containers.CreateContainerOptions{
		Name: containerName,
		ContainerSpec: apiv1.ContainerSpec{
			Image: containerName + "-image",
		},
	})
	require.NoError(t, createErr, "Test container could not be created")

	_, startErr := tco.StartContainers(testCtx, containers.StartContainersOptions{
		Containers: []string{containerID},
	})
	require.NoError(t, startErr, "Test container could not be started")

	// Just in case, ensure the container can be inspected (we use that as existence check)
	_, inspectErr := tco.InspectContainers(testCtx, containers.InspectContainersOptions{
		Containers: []string{containerID},
	})
	require.NoError(t, inspectErr, "Test container could not be inspected")

	delayTimeout := testTimeout / 6
	parentCmd := exec.CommandContext(testCtx, "./delay", fmt.Sprintf("--delay=%s", delayTimeout.String()))
	parentCmd.Dir = delayToolDir
	process.DecoupleFromParent(parentCmd)
	parentCmdErr := parentCmd.Start()
	require.NoError(t, parentCmdErr, "Monitored process should start without error")

	parentPid := process.Uint32_ToPidT(uint32(parentCmd.Process.Pid))
	parentCreateTime := process.StartTimeForProcess(parentPid)
	require.False(t, parentCreateTime.IsZero(), "Monitored process start time should not be zero")

	// Start dcpproc to monitor the parent process and clean up the container when it exits
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"container",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--containerID", containerID,
		"--monitor-start-time", parentCreateTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--test-container-orchestrator-socket", tco.GetSocketFilePath(),
	)
	process.DecoupleFromParent(dcpProcCmd)
	dcpProcCmdErr := dcpProcCmd.Start()
	require.NoError(t, dcpProcCmdErr, "dcpproc command should start without error")

	// Wait for the monitored process to exit
	parentProcResultCh := make(chan error, 1)
	go func() { parentProcResultCh <- parentCmd.Wait() }()

	select {
	case parentProcErr := <-parentProcResultCh:
		require.NoError(t, parentProcErr, "Monitored process should exit without error")
	case <-testCtx.Done():
		require.Fail(t, "Monitored process did not exit in time")
	}

	// Now ensure that dcpproc has cleaned up the container
	waitErr := wait.PollUntilContextCancel(testCtx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		_, inspectDeletedErr := tco.InspectContainers(testCtx, containers.InspectContainersOptions{
			Containers: []string{containerID},
		})
		if errors.Is(inspectDeletedErr, containers.ErrNotFound) {
			// Container was cleaned up as expected
			return true, nil
		}
		return false, inspectDeletedErr
	})
	require.NoError(t, waitErr, "Container cleanup did not complete in time")
}

// Ensures that dcpproc exits when the monitored container is removed externally.
func TestMonitorContainerExitWhenContainerRemoved(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr, "DCPPROC path should not be found")

	delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
	require.NoError(t, toolLaunchErr, "'delay' tool directory could not be found")

	const testTimeout = time.Second * 30
	testCtx, testCancel := context.WithTimeout(context.Background(), testTimeout)
	defer testCancel()

	log := testutil.NewLogForTesting(t.Name())
	tco, tcoErr := ctrlutil.NewTestContainerOrchestrator(testCtx, log, ctrlutil.TcoOptionEnableSocketListener)
	require.NoError(t, tcoErr)
	defer func() {
		closeErr := tco.Close()
		require.NoError(t, closeErr)
	}()

	const containerName = "test-monitor-exits-when-container-removed"
	containerID, createErr := tco.CreateContainer(testCtx, containers.CreateContainerOptions{
		Name: containerName,
		ContainerSpec: apiv1.ContainerSpec{
			Image: containerName + "-image",
		},
	})
	require.NoError(t, createErr, "Test container could not be created")

	_, startErr := tco.StartContainers(testCtx, containers.StartContainersOptions{
		Containers: []string{containerID},
	})
	require.NoError(t, startErr, "Test container could not be started")

	// Ensure the container exists
	_, inspectErr := tco.InspectContainers(testCtx, containers.InspectContainersOptions{
		Containers: []string{containerID},
	})
	require.NoError(t, inspectErr, "Test container could not be inspected")

	parentCmd := exec.CommandContext(testCtx, "./delay", fmt.Sprintf("--delay=%s", testTimeout.String()))
	parentCmd.Dir = delayToolDir
	process.DecoupleFromParent(parentCmd)
	parentCmdErr := parentCmd.Start()
	require.NoError(t, parentCmdErr, "Monitored process should start without error")

	parentPid := process.Uint32_ToPidT(uint32(parentCmd.Process.Pid))
	parentCreateTime := process.StartTimeForProcess(parentPid)
	require.False(t, parentCreateTime.IsZero(), "Monitored process start time should not be zero")

	// Start dcpproc to monitor the parent process and container, with a short poll interval for the test
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"container",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--containerID", containerID,
		"--monitor-start-time", parentCreateTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--test-container-orchestrator-socket", tco.GetSocketFilePath(),
		"--containerPollInterval", "2s",
	)
	process.DecoupleFromParent(dcpProcCmd)
	dcpProcCmdErr := dcpProcCmd.Start()
	require.NoError(t, dcpProcCmdErr, "dcpproc command should start without error")

	// Remove the container externally and ensure dcpproc exits
	_, removeErr := tco.RemoveContainers(testCtx, containers.RemoveContainersOptions{
		Containers: []string{containerID},
		Force:      true,
	})
	require.NoError(t, removeErr, "Test container could not be removed")

	// Wait for dcpproc to notice and exit
	dcpProcResultCh := make(chan error, 1)
	go func() { dcpProcResultCh <- dcpProcCmd.Wait() }()

	select {
	case dcpProcResult := <-dcpProcResultCh:
		require.NoError(t, dcpProcResult, "dcpproc should exit cleanly when container is removed")
	case <-testCtx.Done():
		require.Fail(t, "dcpproc did not exit after container removal")
	}
}

func TestStopProcessTreeWorks(t *testing.T) {
	type testcase struct {
		description        string
		prepareChildCmd    func(*exec.Cmd)
		expectedChildCount int
	}

	// One parent, one child, and one grandchild for a total of 3 processes.
	const childSpecFlag = "--child-spec=1,1"
	getExpectedChildCount := func(useForkFromParent bool) int {
		if useForkFromParent && osutil.IsWindows() {
			return 4 // Account for "conhost" process hosting separate console for the child process tree
		} else {
			return 3
		}
	}

	testCases := []testcase{
		{
			description:        "decouple from parent",
			prepareChildCmd:    process.DecoupleFromParent,
			expectedChildCount: getExpectedChildCount(false),
		},
		{
			description:        "fork from parent",
			prepareChildCmd:    process.ForkFromParent,
			expectedChildCount: getExpectedChildCount(true),
		},
	}

	t.Parallel()

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			dcpProc, dcpProcErr := getDcpProcExecutablePath()
			require.NoError(t, dcpProcErr)

			delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
			require.NoError(t, toolLaunchErr)

			// Set a reasonable timeout that ensures we can see all expected processes exit before their delay time
			const testTimeout = time.Second * 30
			testCtx, testCancel := context.WithTimeout(context.Background(), testTimeout)
			defer testCancel()

			// All commands will return on its own after 30 seconds.
			// This prevents the test from launching a bunch of processes that turn into zombies.
			delayFlag := fmt.Sprintf("--delay=%s", testTimeout.String())

			childrenCmd := exec.CommandContext(testCtx, "./delay", delayFlag, childSpecFlag)
			childrenCmd.Dir = delayToolDir
			tc.prepareChildCmd(childrenCmd)
			childrenCmdErr := childrenCmd.Start()
			require.NoError(t, childrenCmdErr, "command should start without error")

			pid := process.Uint32_ToPidT(uint32(childrenCmd.Process.Pid))
			childCreateTime := process.StartTimeForProcess(pid)
			require.False(t, childCreateTime.IsZero(), "child process start time should not be zero")
			int_testutil.EnsureProcessTree(
				t,
				process.ProcessTreeItem{Pid: pid, CreationTime: childCreateTime},
				tc.expectedChildCount,
				testTimeout/3,
			)

			dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
				"stop-process-tree",
				"--pid", strconv.Itoa(childrenCmd.Process.Pid),
				"--process-start-time", childCreateTime.Format(osutil.RFC3339MiliTimestampFormat),
			)
			dcpProcCmd.Stdout = os.Stdout
			dcpProcCmd.Stderr = os.Stderr
			process.DecoupleFromParent(dcpProcCmd)
			dcpProcCmdErr := dcpProcCmd.Start()
			require.NoError(t, dcpProcCmdErr, "command should start without error")

			dcpWaitErr := dcpProcCmd.Wait()
			require.NoError(t, dcpWaitErr)

			childErr := childrenCmd.Wait()
			require.NoError(t, childErr) // dcpproc should terminate processes gracefully
			require.True(t, childrenCmd.ProcessState.Exited(), "child process should have exited")
		})
	}
}

func getDcpProcExecutablePath() (string, error) {
	dcpExeName := "dcpproc"
	if runtime.GOOS == "windows" {
		dcpExeName += ".exe"
	}

	outputBin, found := os.LookupEnv("OUTPUT_BIN")
	if found {
		dcpPath := filepath.Join(outputBin, dcpExeName)
		file, err := os.Stat(dcpPath)
		if err != nil {
			return "", fmt.Errorf("failed to find the DCP executable: %w", err)
		}
		if file.IsDir() {
			return "", fmt.Errorf("the expected path to DCP executable is a directory: %s", dcpPath)
		}
		return dcpPath, nil
	}

	tail := []string{dcppaths.DcpBinDir, dcppaths.DcpExtensionsDir, dcppaths.DcpBinDir, dcpExeName}
	rootFolder, err := osutil.FindRootFor(osutil.FileTarget, tail...)
	if err != nil {
		return "", err
	}

	return filepath.Join(append([]string{rootFolder}, tail...)...), nil
}
