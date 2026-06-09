/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestMonitorProcessExitsWithErrorForInvalidPid(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	// Set a reasonable timeout that gives the wait polling time to see the delay process exit
	testCtx, testCancel := context.WithTimeout(context.Background(), time.Second*30)
	defer testCancel()

	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"monitor-process",
		"--monitor", "aaa",
		"--child", "bbb",
	)
	err := dcpProcCmd.Run()
	require.Error(t, err)
}

func TestMonitorProcessTerminatesWatchedProcesses(t *testing.T) {
	// One parent, one child, and one grandchild for a total of 3 processes.
	const childSpecFlag = "--child-spec=1,1"
	expectedChildCount := 3
	if osutil.IsWindows() {
		expectedChildCount = 4 // Account for "conhost" process hosting separate console for the child process tree
	}

	t.Parallel()

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
	parentIdentityTime := process.ProcessIdentityTime(parentPid)
	require.False(t, parentIdentityTime.IsZero(), "parent process start time should not be zero")
	int_testutil.EnsureProcessTree(t, process.ProcessTreeItem{Pid: parentPid, IdentityTime: parentIdentityTime}, 1, testTimeout/3)

	childrenCmd := exec.CommandContext(testCtx, "./delay", delayFlag, childSpecFlag)
	childrenCmd.Dir = delayToolDir
	process.ForkFromParent(childrenCmd)
	childrenCmdErr := childrenCmd.Start()
	require.NoError(t, childrenCmdErr, "command should start without error")

	pid := process.Uint32_ToPidT(uint32(childrenCmd.Process.Pid))
	childIdentityTime := process.ProcessIdentityTime(pid)
	require.False(t, childIdentityTime.IsZero(), "child process start time should not be zero")
	int_testutil.EnsureProcessTree(
		t,
		process.ProcessTreeItem{Pid: pid, IdentityTime: childIdentityTime},
		expectedChildCount,
		testTimeout/3,
	)

	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"monitor-process",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--child", strconv.Itoa(childrenCmd.Process.Pid),
		"--monitor-identity-time", parentIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--child-identity-time", childIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
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
}

// TestMonitorProcessExitsCleanlyIfChildStartTimeDoesNotMatch verifies that when dcpproc
// cannot positively identify the child process (its PID is alive but the identity time
// does not match), it exits cleanly without trying to kill the unrelated process that
// currently owns that PID. The monitored process remains alive throughout, so dcpproc
// is expected to log a warning about the child and return without doing anything else.
func TestMonitorProcessExitsCleanlyIfChildStartTimeDoesNotMatch(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "linux" {
		t.Skip("Skipping test on Linux because process start time is not available")
	}

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
	require.NoError(t, toolLaunchErr)

	const testTimeout = time.Second * 30
	testCtx, testCancel := context.WithTimeout(context.Background(), testTimeout)
	defer testCancel()

	delayFlag := fmt.Sprintf("--delay=%s", testTimeout.String())

	parentCmd := exec.CommandContext(testCtx, "./delay", delayFlag)
	parentCmd.Dir = delayToolDir
	require.NoError(t, parentCmd.Start(), "parent command should start without error")
	defer func() {
		_ = parentCmd.Process.Kill()
		_ = parentCmd.Wait()
	}()

	parentPid := process.Uint32_ToPidT(uint32(parentCmd.Process.Pid))
	parentIdentityTime := process.ProcessIdentityTime(parentPid)
	require.False(t, parentIdentityTime.IsZero(), "parent process start time should not be zero")
	int_testutil.EnsureProcessTree(t, process.ProcessTreeItem{Pid: parentPid, IdentityTime: parentIdentityTime}, 1, testTimeout/3)

	childCmd := exec.CommandContext(testCtx, "./delay", delayFlag)
	childCmd.Dir = delayToolDir
	require.NoError(t, childCmd.Start(), "child command should start without error")
	defer func() {
		_ = childCmd.Process.Kill()
		_ = childCmd.Wait()
	}()

	childPid := process.Uint32_ToPidT(uint32(childCmd.Process.Pid))
	childIdentityTime := process.ProcessIdentityTime(childPid)
	require.False(t, childIdentityTime.IsZero(), "child process start time should not be zero")
	int_testutil.EnsureProcessTree(t, process.ProcessTreeItem{Pid: childPid, IdentityTime: childIdentityTime}, 1, testTimeout/3)

	bogusChildIdentityTime := childIdentityTime.Add(1 * time.Second)

	dcpProcOut := &dcpProcOutput{}
	defer dcpProcOut.DumpOnFailure(t, "dcpproc (TestMonitorProcessExitsCleanlyIfChildStartTimeDoesNotMatch)")
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"monitor-process",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--child", strconv.Itoa(childCmd.Process.Pid),
		"--monitor-identity-time", parentIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--child-identity-time", bogusChildIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
	)
	dcpProcCmd.Stdout = dcpProcOut.StdoutWriter()
	dcpProcCmd.Stderr = dcpProcOut.StderrWriter()
	require.NoError(t, dcpProcCmd.Start(), "dcpproc command should start without error")

	require.NoError(t, dcpProcCmd.Wait(), "dcpproc should have exited without an error")
	stderr := dcpProcOut.Stderr()
	require.True(t,
		strings.Contains(stderr, "process start time mismatch") && strings.Contains(stderr, "Child process could not be monitored"),
		"dcpproc should have reported invalid child process start time; stderr: %s", stderr)

	// The child process must still be alive: dcpproc must NOT kill a process it could not
	// positively identify.
	childStillAlive := process.ProcessIdentityTime(childPid)
	require.False(t, childStillAlive.IsZero(), "child process should still be running")
	require.True(t, childStillAlive.Equal(childIdentityTime), "child process should still be the same instance")
}

// TestMonitorProcessCleansUpChildIfMonitorStartTimeDoesNotMatch covers the case where
// dcpproc cannot positively identify the monitored (parent) process because its identity
// time does not match what was captured by the caller. The original monitored process is
// by definition gone in that case (the PID was reused by an unrelated process), so dcpproc
// must shut down the child process -- the entire reason it was started -- rather than
// bailing out with an error and leaving the child running.
func TestMonitorProcessCleansUpChildIfMonitorStartTimeDoesNotMatch(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "linux" {
		t.Skip("Skipping on Linux because process identity time is computed from boot ticks and may not differ for a long-running process")
	}

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
	require.NoError(t, toolLaunchErr)

	const testTimeout = time.Second * 30
	testCtx, testCancel := context.WithTimeout(context.Background(), testTimeout)
	defer testCancel()

	delayFlag := fmt.Sprintf("--delay=%s", testTimeout.String())

	parentCmd := exec.CommandContext(testCtx, "./delay", delayFlag)
	parentCmd.Dir = delayToolDir
	require.NoError(t, parentCmd.Start(), "parent command should start without error")
	defer func() {
		_ = parentCmd.Process.Kill()
		_ = parentCmd.Wait()
	}()

	parentPid := process.Uint32_ToPidT(uint32(parentCmd.Process.Pid))
	parentIdentityTime := process.ProcessIdentityTime(parentPid)
	require.False(t, parentIdentityTime.IsZero(), "parent process start time should not be zero")
	int_testutil.EnsureProcessTree(t, process.ProcessTreeItem{Pid: parentPid, IdentityTime: parentIdentityTime}, 1, testTimeout/3)

	childCmd := exec.CommandContext(testCtx, "./delay", delayFlag)
	childCmd.Dir = delayToolDir
	process.ForkFromParent(childCmd)
	require.NoError(t, childCmd.Start(), "child command should start without error")
	defer func() {
		_ = childCmd.Process.Kill()
		_ = childCmd.Wait()
	}()

	childPid := process.Uint32_ToPidT(uint32(childCmd.Process.Pid))
	childIdentityTime := process.ProcessIdentityTime(childPid)
	require.False(t, childIdentityTime.IsZero(), "child process start time should not be zero")
	expectedChildTreeSize := 1
	if osutil.IsWindows() {
		expectedChildTreeSize = 2 // Account for conhost.exe hosting separate console for the child
	}
	int_testutil.EnsureProcessTree(t, process.ProcessTreeItem{Pid: childPid, IdentityTime: childIdentityTime}, expectedChildTreeSize, testTimeout/3)

	// Pass an identity time that does not match the actual one to simulate a reused PID.
	bogusMonitorIdentityTime := parentIdentityTime.Add(1 * time.Hour)

	dcpProcOut := &dcpProcOutput{}
	defer dcpProcOut.DumpOnFailure(t, "dcpproc (TestMonitorProcessCleansUpChildIfMonitorStartTimeDoesNotMatch)")
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"monitor-process",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--child", strconv.Itoa(childCmd.Process.Pid),
		"--monitor-identity-time", bogusMonitorIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--child-identity-time", childIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
	)
	dcpProcCmd.Stdout = dcpProcOut.StdoutWriter()
	dcpProcCmd.Stderr = dcpProcOut.StderrWriter()
	process.DecoupleFromParent(dcpProcCmd)
	require.NoError(t, dcpProcCmd.Start(), "dcpproc command should start without error")

	dcpProcResultCh := make(chan error, 1)
	go func() { dcpProcResultCh <- dcpProcCmd.Wait() }()

	select {
	case dcpProcResult := <-dcpProcResultCh:
		require.NoError(t, dcpProcResult, "dcpproc should exit cleanly when the monitored PID has been reused")
	case <-testCtx.Done():
		require.Fail(t, "dcpproc did not exit in time")
	}

	stderr := dcpProcOut.Stderr()
	require.Contains(t, stderr, "process start time mismatch",
		"dcpproc should have reported invalid monitor process start time; stderr: %s", stderr)
	require.Contains(t, stderr, "Monitored process already exited, shutting down child process",
		"dcpproc should have reported the child cleanup decision; stderr: %s", stderr)

	// The child process must have been shut down by dcpproc, well before the test context
	// would expire and force-kill it.
	childWaitCh := make(chan error, 1)
	go func() { childWaitCh <- childCmd.Wait() }()
	select {
	case <-childWaitCh:
		// dcpproc successfully terminated the child (delay exits cleanly on CTRL+BREAK / SIGTERM)
	case <-time.After(testTimeout / 2):
		require.Fail(t, "child process was not terminated by dcpproc in time")
	}
}

// TestMonitorProcessCleansUpChildWhenMonitoredProcessAlreadyExited covers the case where
// dcpproc is invoked with a monitored PID that has already exited. This can happen under
// load when dcpproc startup latency is comparable to the lifetime of the (short-lived)
// process being monitored. In that case dcpproc must still shut down the child process --
// the entire reason it was started -- rather than bailing out with an error.
func TestMonitorProcessCleansUpChildWhenMonitoredProcessAlreadyExited(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
	require.NoError(t, toolLaunchErr)

	const testTimeout = time.Second * 30
	testCtx, testCancel := context.WithTimeout(context.Background(), testTimeout)
	defer testCancel()

	// Run the parent process to completion BEFORE starting dcpproc. The captured PID and
	// identity time are still passed to dcpproc, simulating the race where dcpproc startup
	// is slow enough that the monitored process exits first.
	parentCmd := exec.Command(filepath.Join(delayToolDir, "delay"+exeSuffix()), "--delay=10ms")
	parentCmd.Dir = delayToolDir
	process.DecoupleFromParent(parentCmd)
	require.NoError(t, parentCmd.Start(), "Monitored process should start without error")

	parentPid := process.Uint32_ToPidT(uint32(parentCmd.Process.Pid))
	parentIdentityTime := process.ProcessIdentityTime(parentPid)
	require.False(t, parentIdentityTime.IsZero(), "Monitored process start time should not be zero")

	require.NoError(t, parentCmd.Wait(), "Monitored process should exit without error")

	// Start a long-running child process that dcpproc should clean up.
	childCmd := exec.CommandContext(testCtx, "./delay", fmt.Sprintf("--delay=%s", testTimeout.String()))
	childCmd.Dir = delayToolDir
	process.ForkFromParent(childCmd)
	require.NoError(t, childCmd.Start(), "child command should start without error")
	defer func() {
		_ = childCmd.Process.Kill()
		_ = childCmd.Wait()
	}()

	childPid := process.Uint32_ToPidT(uint32(childCmd.Process.Pid))
	childIdentityTime := process.ProcessIdentityTime(childPid)
	require.False(t, childIdentityTime.IsZero(), "child process start time should not be zero")
	expectedChildTreeSize := 1
	if osutil.IsWindows() {
		expectedChildTreeSize = 2 // Account for conhost.exe hosting separate console for the child
	}
	int_testutil.EnsureProcessTree(t, process.ProcessTreeItem{Pid: childPid, IdentityTime: childIdentityTime}, expectedChildTreeSize, testTimeout/3)

	dcpProcOut := &dcpProcOutput{}
	defer dcpProcOut.DumpOnFailure(t, "dcpproc (TestMonitorProcessCleansUpChildWhenMonitoredProcessAlreadyExited)")
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"monitor-process",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--child", strconv.Itoa(childCmd.Process.Pid),
		"--monitor-identity-time", parentIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--child-identity-time", childIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
	)
	dcpProcCmd.Stdout = dcpProcOut.StdoutWriter()
	dcpProcCmd.Stderr = dcpProcOut.StderrWriter()
	process.DecoupleFromParent(dcpProcCmd)
	require.NoError(t, dcpProcCmd.Start(), "dcpproc command should start without error")

	// dcpproc should exit cleanly because it noticed the monitored process is gone and
	// performed child cleanup.
	dcpProcResultCh := make(chan error, 1)
	go func() { dcpProcResultCh <- dcpProcCmd.Wait() }()

	select {
	case dcpProcResult := <-dcpProcResultCh:
		require.NoError(t, dcpProcResult, "dcpproc should exit cleanly when monitored process is already gone")
	case <-testCtx.Done():
		require.Fail(t, "dcpproc did not exit in time")
	}

	stderr := dcpProcOut.Stderr()
	require.Contains(t, stderr, "Monitored process already exited, shutting down child process",
		"dcpproc should have reported the child cleanup decision; stderr: %s", stderr)

	// The child process must have been shut down by dcpproc, well before the test context
	// would expire and force-kill it.
	childWaitCh := make(chan error, 1)
	go func() { childWaitCh <- childCmd.Wait() }()
	select {
	case <-childWaitCh:
		// dcpproc successfully terminated the child (delay exits cleanly on CTRL+BREAK / SIGTERM)
	case <-time.After(testTimeout / 2):
		require.Fail(t, "child process was not terminated by dcpproc in time")
	}
}

func TestMonitorContainerTerminatesWatchedContainer(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr, "DCPPROC path should not be found")

	delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
	require.NoError(t, toolLaunchErr, "'delay' tool directory could not be found")

	const testTimeout = time.Second * 60
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
	parentIdentityTime := process.ProcessIdentityTime(parentPid)
	require.False(t, parentIdentityTime.IsZero(), "Monitored process start time should not be zero")

	// Start dcpproc to monitor the parent process and clean up the container when it exits
	dcpProcOut := &dcpProcOutput{}
	defer dcpProcOut.DumpOnFailure(t, "dcpproc (TestMonitorContainerTerminatesWatchedContainer)")
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"monitor-container",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--containerID", containerID,
		"--monitor-identity-time", parentIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--test-container-orchestrator-socket", tco.GetSocketFilePath(),
	)
	dcpProcCmd.Stdout = dcpProcOut.StdoutWriter()
	dcpProcCmd.Stderr = dcpProcOut.StderrWriter()
	process.DecoupleFromParent(dcpProcCmd)
	dcpProcCmdErr := dcpProcCmd.Start()
	require.NoError(t, dcpProcCmdErr, "dcpproc command should start without error")

	// Wait for dcpproc to fully start monitoring before letting the parent exit.
	// Without this, a slow dcpproc startup could race with the (short-lived) parent and end up
	// attempting to monitor a process that has already exited. While our error handling still
	// triggers container cleanup in that case, syncing here makes the test exercise the normal
	// monitor-then-detect-exit path deterministically.
	require.NoError(t,
		dcpProcOut.WaitForStderrSubstring(testCtx, dcpprocMonitoringStartedLogMessage),
		"dcpproc did not start monitoring in time")

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

func TestMonitorContainerStopsWatchedContainerWithoutRemoving(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr, "DCPPROC path should not be found")

	delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
	require.NoError(t, toolLaunchErr, "'delay' tool directory could not be found")

	const testTimeout = time.Second * 60
	testCtx, testCancel := context.WithTimeout(context.Background(), testTimeout)
	defer testCancel()

	log := testutil.NewLogForTesting(t.Name())
	tco, tcoErr := ctrlutil.NewTestContainerOrchestrator(testCtx, log, ctrlutil.TcoOptionEnableSocketListener)
	require.NoError(t, tcoErr)
	defer func() {
		closeErr := tco.Close()
		require.NoError(t, closeErr)
	}()

	const containerName = "test-monitor-stops-watched-container-without-removing"
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

	delayTimeout := testTimeout / 6
	parentCmd := exec.CommandContext(testCtx, "./delay", fmt.Sprintf("--delay=%s", delayTimeout.String()))
	parentCmd.Dir = delayToolDir
	process.DecoupleFromParent(parentCmd)
	parentCmdErr := parentCmd.Start()
	require.NoError(t, parentCmdErr, "Monitored process should start without error")

	parentPid := process.Uint32_ToPidT(uint32(parentCmd.Process.Pid))
	parentIdentityTime := process.ProcessIdentityTime(parentPid)
	require.False(t, parentIdentityTime.IsZero(), "Monitored process start time should not be zero")

	dcpProcOut := &dcpProcOutput{}
	defer dcpProcOut.DumpOnFailure(t, "dcpproc (TestMonitorContainerStopsWatchedContainerWithoutRemoving)")
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"monitor-container",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--containerID", containerID,
		"--monitor-identity-time", parentIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--test-container-orchestrator-socket", tco.GetSocketFilePath(),
		"--stop-only",
	)
	dcpProcCmd.Stdout = dcpProcOut.StdoutWriter()
	dcpProcCmd.Stderr = dcpProcOut.StderrWriter()
	process.DecoupleFromParent(dcpProcCmd)
	dcpProcCmdErr := dcpProcCmd.Start()
	require.NoError(t, dcpProcCmdErr, "dcpproc command should start without error")

	// Wait for dcpproc to fully start monitoring before letting the parent exit (see the
	// equivalent comment in TestMonitorContainerTerminatesWatchedContainer for rationale).
	require.NoError(t,
		dcpProcOut.WaitForStderrSubstring(testCtx, dcpprocMonitoringStartedLogMessage),
		"dcpproc did not start monitoring in time")

	parentProcResultCh := make(chan error, 1)
	go func() { parentProcResultCh <- parentCmd.Wait() }()

	select {
	case parentProcErr := <-parentProcResultCh:
		require.NoError(t, parentProcErr, "Monitored process should exit without error")
	case <-testCtx.Done():
		require.Fail(t, "Monitored process did not exit in time")
	}

	waitErr := wait.PollUntilContextCancel(testCtx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		inspected, inspectErr := tco.InspectContainers(ctx, containers.InspectContainersOptions{
			Containers: []string{containerID},
		})
		if inspectErr != nil {
			return false, inspectErr
		}
		if len(inspected) != 1 {
			return false, fmt.Errorf("expected one inspected container, got %d", len(inspected))
		}
		return inspected[0].Status == containers.ContainerStatusExited, nil
	})
	require.NoError(t, waitErr, "Container stop did not complete in time")

	dcpProcResultCh := make(chan error, 1)
	go func() { dcpProcResultCh <- dcpProcCmd.Wait() }()

	select {
	case dcpProcResult := <-dcpProcResultCh:
		require.NoError(t, dcpProcResult, "dcpproc should exit cleanly after stopping the container")
	case <-testCtx.Done():
		require.Fail(t, "dcpproc did not exit after stopping container")
	}
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
	parentIdentityTime := process.ProcessIdentityTime(parentPid)
	require.False(t, parentIdentityTime.IsZero(), "Monitored process start time should not be zero")

	// Start dcpproc to monitor the parent process and container, with a short poll interval for the test
	dcpProcOut := &dcpProcOutput{}
	defer dcpProcOut.DumpOnFailure(t, "dcpproc (TestMonitorContainerExitWhenContainerRemoved)")
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"monitor-container",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--containerID", containerID,
		"--monitor-identity-time", parentIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--test-container-orchestrator-socket", tco.GetSocketFilePath(),
		"--containerPollInterval", "2s",
	)
	dcpProcCmd.Stdout = dcpProcOut.StdoutWriter()
	dcpProcCmd.Stderr = dcpProcOut.StderrWriter()
	process.DecoupleFromParent(dcpProcCmd)
	dcpProcCmdErr := dcpProcCmd.Start()
	require.NoError(t, dcpProcCmdErr, "dcpproc command should start without error")

	// Wait for dcpproc to fully start monitoring before removing the container. This guarantees
	// the container removal happens after dcpproc has installed its container-removed poller.
	require.NoError(t,
		dcpProcOut.WaitForStderrSubstring(testCtx, dcpprocMonitoringStartedLogMessage),
		"dcpproc did not start monitoring in time")

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

// TestMonitorContainerCleansUpWhenMonitoredProcessAlreadyExited covers the case where dcpproc
// is invoked with a monitored PID that has already exited. This can happen under load when
// dcpproc startup latency is comparable to the lifetime of the (short-lived) process being
// monitored. In that case dcpproc must still clean up the container -- the original reason
// it was started -- rather than bailing out with an error.
func TestMonitorContainerCleansUpWhenMonitoredProcessAlreadyExited(t *testing.T) {
	t.Parallel()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
	require.NoError(t, toolLaunchErr)

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

	const containerName = "test-monitor-cleanup-when-already-exited"
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

	// Run the parent process to completion BEFORE starting dcpproc. The captured PID and
	// identity time are still passed to dcpproc, simulating the race where dcpproc startup
	// is slow enough that the monitored process exits first.
	parentCmd := exec.Command(filepath.Join(delayToolDir, "delay"+exeSuffix()), "--delay=10ms")
	parentCmd.Dir = delayToolDir
	process.DecoupleFromParent(parentCmd)
	require.NoError(t, parentCmd.Start(), "Monitored process should start without error")

	parentPid := process.Uint32_ToPidT(uint32(parentCmd.Process.Pid))
	parentIdentityTime := process.ProcessIdentityTime(parentPid)
	require.False(t, parentIdentityTime.IsZero(), "Monitored process start time should not be zero")

	require.NoError(t, parentCmd.Wait(), "Monitored process should exit without error")

	dcpProcOut := &dcpProcOutput{}
	defer dcpProcOut.DumpOnFailure(t, "dcpproc (TestMonitorContainerCleansUpWhenMonitoredProcessAlreadyExited)")
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"monitor-container",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--containerID", containerID,
		"--monitor-identity-time", parentIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--test-container-orchestrator-socket", tco.GetSocketFilePath(),
	)
	dcpProcCmd.Stdout = dcpProcOut.StdoutWriter()
	dcpProcCmd.Stderr = dcpProcOut.StderrWriter()
	process.DecoupleFromParent(dcpProcCmd)
	require.NoError(t, dcpProcCmd.Start(), "dcpproc command should start without error")

	// dcpproc should exit cleanly because it noticed the monitored process is gone and
	// performed container cleanup.
	dcpProcResultCh := make(chan error, 1)
	go func() { dcpProcResultCh <- dcpProcCmd.Wait() }()

	select {
	case dcpProcResult := <-dcpProcResultCh:
		require.NoError(t, dcpProcResult, "dcpproc should exit cleanly when monitored process is already gone")
	case <-testCtx.Done():
		require.Fail(t, "dcpproc did not exit in time")
	}

	// The container should have been cleaned up despite dcpproc never having attached to a live monitored process.
	_, inspectErr := tco.InspectContainers(testCtx, containers.InspectContainersOptions{
		Containers: []string{containerID},
	})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound, "Container should have been cleaned up by dcpproc")
}

// TestMonitorContainerCleansUpOnIdentityTimeMismatch covers the case where the monitored PID
// has been reused by a different process between the caller capturing the identity time and
// dcpproc looking it up. The original monitored process is by definition gone in that case,
// so dcpproc must clean up the container associated with it.
func TestMonitorContainerCleansUpOnIdentityTimeMismatch(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "linux" {
		t.Skip("Skipping on Linux because process identity time is computed from boot ticks and may not differ for a long-running process")
	}

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolDir, toolLaunchErr := int_testutil.GetTestToolDir("delay")
	require.NoError(t, toolLaunchErr)

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

	const containerName = "test-monitor-cleanup-on-identity-mismatch"
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

	parentCmd := exec.CommandContext(testCtx, "./delay", fmt.Sprintf("--delay=%s", testTimeout.String()))
	parentCmd.Dir = delayToolDir
	process.DecoupleFromParent(parentCmd)
	require.NoError(t, parentCmd.Start(), "Monitored process should start without error")
	defer func() {
		_ = parentCmd.Process.Kill()
		_ = parentCmd.Wait()
	}()

	parentPid := process.Uint32_ToPidT(uint32(parentCmd.Process.Pid))
	parentIdentityTime := process.ProcessIdentityTime(parentPid)
	require.False(t, parentIdentityTime.IsZero(), "Monitored process start time should not be zero")

	// Pass an identity time that does not match the actual one to simulate a reused PID.
	bogusIdentityTime := parentIdentityTime.Add(1 * time.Hour)

	dcpProcOut := &dcpProcOutput{}
	defer dcpProcOut.DumpOnFailure(t, "dcpproc (TestMonitorContainerCleansUpOnIdentityTimeMismatch)")
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"monitor-container",
		"--monitor", strconv.Itoa(parentCmd.Process.Pid),
		"--containerID", containerID,
		"--monitor-identity-time", bogusIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--test-container-orchestrator-socket", tco.GetSocketFilePath(),
	)
	dcpProcCmd.Stdout = dcpProcOut.StdoutWriter()
	dcpProcCmd.Stderr = dcpProcOut.StderrWriter()
	process.DecoupleFromParent(dcpProcCmd)
	require.NoError(t, dcpProcCmd.Start(), "dcpproc command should start without error")

	dcpProcResultCh := make(chan error, 1)
	go func() { dcpProcResultCh <- dcpProcCmd.Wait() }()

	select {
	case dcpProcResult := <-dcpProcResultCh:
		require.NoError(t, dcpProcResult, "dcpproc should exit cleanly when the monitored PID has been reused")
	case <-testCtx.Done():
		require.Fail(t, "dcpproc did not exit in time")
	}

	_, inspectErr := tco.InspectContainers(testCtx, containers.InspectContainersOptions{
		Containers: []string{containerID},
	})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound, "Container should have been cleaned up by dcpproc")
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func TestStopProcessTreeWorks(t *testing.T) {
	// One parent, one child, and one grandchild for a total of 3 processes.
	const childSpecFlag = "--child-spec=1,1"
	expectedChildCount := 3
	if osutil.IsWindows() {
		expectedChildCount = 4 // Account for "conhost" process hosting separate console for the child process tree
	}

	t.Parallel()

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
	process.ForkFromParent(childrenCmd)
	childrenCmdErr := childrenCmd.Start()
	require.NoError(t, childrenCmdErr, "command should start without error")

	pid := process.Uint32_ToPidT(uint32(childrenCmd.Process.Pid))
	childIdentityTime := process.ProcessIdentityTime(pid)
	require.False(t, childIdentityTime.IsZero(), "child process start time should not be zero")
	int_testutil.EnsureProcessTree(
		t,
		process.ProcessTreeItem{Pid: pid, IdentityTime: childIdentityTime},
		expectedChildCount,
		testTimeout/3,
	)

	dcpProcCmd := exec.CommandContext(testCtx, dcpProc,
		"stop-process-tree",
		"--pid", strconv.Itoa(childrenCmd.Process.Pid),
		"--process-start-time", childIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
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
}

func getDcpProcExecutablePath() (string, error) {
	dcpExeName := "dcp"
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

	tail := []string{dcppaths.BuildOutputDir, dcpExeName}
	rootFolder, err := osutil.FindRootFor(osutil.FileTarget, tail...)
	if err != nil {
		return "", err
	}

	return filepath.Join(append([]string{rootFolder}, tail...)...), nil
}

// dcpProcOutput captures stdout and stderr of a dcpproc process in a thread-safe
// way and lets the test wait for specific substrings to appear in the captured output.
// It is intended to replace ad-hoc time-based delays for waiting on dcpproc startup,
// and to make test failures easier to debug by dumping captured output on failure.
type dcpProcOutput struct {
	mu     sync.Mutex
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func (o *dcpProcOutput) StdoutWriter() io.Writer {
	return &dcpProcOutputWriter{out: o, dest: &o.stdout}
}

func (o *dcpProcOutput) StderrWriter() io.Writer {
	return &dcpProcOutputWriter{out: o, dest: &o.stderr}
}

func (o *dcpProcOutput) Stderr() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stderr.String()
}

// WaitForStderrSubstring polls until either the substring appears in dcpproc's
// captured stderr, or the provided context is done.
func (o *dcpProcOutput) WaitForStderrSubstring(ctx context.Context, substring string) error {
	return wait.PollUntilContextCancel(ctx, 50*time.Millisecond, true, func(_ context.Context) (bool, error) {
		return strings.Contains(o.Stderr(), substring), nil
	})
}

// DumpOnFailure writes captured stdout/stderr to the test log when the test fails
// or when running with -v. This makes debugging flaky failures considerably easier.
func (o *dcpProcOutput) DumpOnFailure(t *testing.T, name string) {
	t.Helper()
	if !t.Failed() && !testing.Verbose() {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if o.stdout.Len() > 0 {
		t.Logf("%s stdout:\n%s", name, o.stdout.String())
	}
	if o.stderr.Len() > 0 {
		t.Logf("%s stderr:\n%s", name, o.stderr.String())
	}
}

type dcpProcOutputWriter struct {
	out  *dcpProcOutput
	dest *bytes.Buffer
}

func (w *dcpProcOutputWriter) Write(p []byte) (int, error) {
	w.out.mu.Lock()
	defer w.out.mu.Unlock()
	return w.dest.Write(p)
}

// dcpprocMonitoringStartedLogMessage is emitted by cmds.MonitorPid after it
// successfully attaches to the monitored process. Tests use it as a signal that
// dcpproc is fully initialized and actively watching the target PID.
const dcpprocMonitoringStartedLogMessage = "Started monitoring process"
