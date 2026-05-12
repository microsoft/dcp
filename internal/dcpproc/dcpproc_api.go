/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/microsoft/dcp/internal/dcppaths"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

const (
	DCP_DISABLE_MONITOR_PROCESS = "DCP_DISABLE_MONITOR_PROCESS"
)

func MonitorTargetFromFields(monitorPID *int64, monitorTimestamp metav1.MicroTime) (process.ProcessTreeItem, bool, error) {
	if monitorPID == nil {
		return process.ProcessTreeItem{}, false, nil
	}
	if monitorTimestamp.IsZero() {
		return process.ProcessTreeItem{}, false, fmt.Errorf("monitor timestamp must be set when monitor PID is set")
	}

	pid, pidErr := process.Int64_ToPidT(*monitorPID)
	if pidErr != nil {
		return process.ProcessTreeItem{}, false, fmt.Errorf("invalid monitor PID %d: %w", *monitorPID, pidErr)
	}

	return process.ProcessTreeItem{
		Pid:          pid,
		IdentityTime: monitorTimestamp.Time,
	}, true, nil
}

// Starts the process monitor for the given child process, using current process as the "monitored", or watched, process.
// The caller should ensure that the current process is the correct process to monitor.
// Errors are logged, but no error is returned if the process monitor fails to start--
// process monitor is considered a "best-effort reliability enhancement".
// Note: DCP process doesn't shut down if DCPCTRL goes away, but DCPCTRL will shut down if DCP process goes away,
// so monitoring DCPCTRL is a safe bet.
func RunProcessWatcher(
	pe process.Executor,
	childPid process.Pid_t,
	childStartTime time.Time,
	log logr.Logger,
) {
	monitorPid := process.Uint32_ToPidT(uint32(os.Getpid()))
	monitorIdentityTime := process.ProcessIdentityTime(monitorPid)
	RunProcessWatcherForMonitor(pe, process.ProcessTreeItem{Pid: monitorPid, IdentityTime: monitorIdentityTime}, childPid, childStartTime, log)
}

func RunProcessWatcherForMonitor(
	pe process.Executor,
	monitor process.ProcessTreeItem,
	childPid process.Pid_t,
	childStartTime time.Time,
	log logr.Logger,
) {
	if _, found := os.LookupEnv(DCP_DISABLE_MONITOR_PROCESS); found {
		return
	}

	log = log.WithValues("ChildPID", childPid)

	cmdArgs := []string{
		"monitor-process",
		"--child", strconv.FormatInt(int64(childPid), 10),
	}
	if !childStartTime.IsZero() {
		cmdArgs = append(cmdArgs, "--child-identity-time", childStartTime.Format(osutil.RFC3339MiliTimestampFormat))
	}
	cmdArgs = append(cmdArgs, getMonitorCmdArgs(monitor)...)

	startErr := startDcpProc(pe, cmdArgs)
	if startErr != nil {
		log.Error(startErr, "Failed to start process monitor")
	}
}

// Starts the container monitor for the given container ID, using current process as the "monitored", or watched, process.
// The caller should ensure that the current process is the correct process to monitor.
// Errors are logged, but no error is returned if the container monitor fails to start--
// container monitor is considered a "best-effort reliability enhancement".
func RunContainerWatcher(
	pe process.Executor,
	containerID string,
	log logr.Logger,
) {
	monitorPid := process.Uint32_ToPidT(uint32(os.Getpid()))
	monitorIdentityTime := process.ProcessIdentityTime(monitorPid)
	RunContainerWatcherForMonitor(pe, process.ProcessTreeItem{Pid: monitorPid, IdentityTime: monitorIdentityTime}, containerID, log)
}

func RunContainerWatcherForMonitor(
	pe process.Executor,
	monitor process.ProcessTreeItem,
	containerID string,
	log logr.Logger,
) {
	if _, found := os.LookupEnv(DCP_DISABLE_MONITOR_PROCESS); found {
		return
	}

	log = log.WithValues("ContainerID", containerID)

	cmdArgs := []string{
		"monitor-container",
		"--containerID", containerID,
	}
	cmdArgs = append(cmdArgs, getMonitorCmdArgs(monitor)...)

	startErr := startDcpProc(pe, cmdArgs)
	if startErr != nil {
		log.Error(startErr, "Failed to start container monitor")
	}
}

// Runs stop-process-tree command to stop the process tree rooted at the given process.
func StopProcessTree(
	ctx context.Context,
	pe process.Executor,
	rootPid process.Pid_t,
	rootProcessStartTime time.Time,
	log logr.Logger,
) error {
	log = log.WithValues("RootPID", rootPid)

	cmdArgs := []string{
		"stop-process-tree",
		"--pid", strconv.FormatInt(int64(rootPid), 10),
	}
	if !rootProcessStartTime.IsZero() {
		cmdArgs = append(cmdArgs, "--process-start-time", rootProcessStartTime.Format(osutil.RFC3339MiliTimestampFormat))
	}

	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	if dcpPathErr != nil {
		log.Error(dcpPathErr, "DCP executable path could not be determined")
		return dcpPathErr
	}
	stopProcessTreeCmd := exec.Command(dcpPath, cmdArgs...)
	stopProcessTreeCmd.Env = os.Environ()    // Use DCP CLI environment
	logger.WithSessionId(stopProcessTreeCmd) // Ensure the session ID is passed to the monitor command

	exitCode, err := process.RunWithTimeout(ctx, pe, stopProcessTreeCmd)
	if err != nil {
		log.Error(err, "Failed to stop process tree", "ExitCode", exitCode)
		return err
	} else if exitCode != 0 {
		err = fmt.Errorf("'dcp stop-process-tree --pid %d' command returned non-zero exit code: %d", rootPid, exitCode)
		log.Error(err, "Failed to stop process tree", "ExitCode", exitCode)
		return err
	}

	return nil
}

func getMonitorCmdArgs(monitor process.ProcessTreeItem) []string {
	// Add monitor PID to the command args
	cmdArgs := []string{"--monitor", strconv.FormatInt(int64(monitor.Pid), 10)}

	// Add monitor start time if available
	if !monitor.IdentityTime.IsZero() {
		cmdArgs = append(cmdArgs, "--monitor-identity-time", monitor.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat))
	}

	return cmdArgs
}

func startDcpProc(pe process.Executor, cmdArgs []string) error {
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	if dcpPathErr != nil {
		return fmt.Errorf("DCP executable path could not be determined: %w", dcpPathErr)
	}
	dcpProcCmd := exec.Command(dcpPath, cmdArgs...)
	dcpProcCmd.Env = os.Environ()    // Use DCP CLI environment
	logger.WithSessionId(dcpProcCmd) // Ensure the session ID is passed to the monitor command
	_, _, monitorErr := pe.StartAndForget(dcpProcCmd, process.CreationFlagsNone)
	return monitorErr
}

func SimulateStopProcessTreeCommand(pe *internal_testutil.ProcessExecution) int32 {
	i := slices.Index(pe.Cmd.Args, "--pid")
	if i < 0 {
		return 1 // The command does not specify the PID to stop.
	}
	if len(pe.Cmd.Args) <= i+2 {
		return 2 // The --pid flag should be followed by the PID of the process to stop.
	}
	pid, pidErr := process.StringToPidT(pe.Cmd.Args[i+1])
	if pidErr != nil {
		return 3 // Invalid PID
	}
	var startTime time.Time
	i = slices.Index(pe.Cmd.Args, "--process-start-time")
	if i >= 0 && len(pe.Cmd.Args) > i+1 {
		var startTimeErr error
		startTime, startTimeErr = time.Parse(osutil.RFC3339MiliTimestampFormat, pe.Cmd.Args[i+1])
		if startTimeErr != nil {
			return 4 // Invalid start time
		}
	}

	// We do not simulate stopping the whole process tree (or process parent-child relationships, for that matter).
	// We can consider adding it if we have tests that require it (currently none).

	stopErr := pe.Executor.StopProcess(pid, startTime)
	if stopErr != nil {
		return 5 // Failed to stop the process
	}

	return 0 // Success
}
