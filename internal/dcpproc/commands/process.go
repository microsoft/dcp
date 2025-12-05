// Copyright (c) Microsoft Corporation. All rights reserved.

package commands

import (
	"errors"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/flags"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

var (
	childPid              process.Pid_t = process.UnknownPID
	childProcessStartTime time.Time
)

func NewProcessCommand(log logr.Logger) (*cobra.Command, error) {
	processCmd := &cobra.Command{
		Use:   "process",
		Short: "Ensures that child process is cleaned up when the monitored process exits",
		Long: `Monitors a child process and shuts it down when the monitored process exits.

This command is used to ensure that child processes are properly cleaned up when
DCP terminates unexpectedly.`,
		RunE:         monitorProcess(log),
		SilenceUsage: true,
		Args:         cobra.NoArgs,
	}

	flagErr := addMonitorFlags(processCmd)
	if flagErr != nil {
		return nil, flagErr
	}

	processCmd.Flags().Int64VarP((*int64)(&childPid), "child", "p", int64(process.UnknownPID), "Tells DCPPROC the PID for the process that needs to be shut down (child process) when the monitored process exits for any reason.")
	flagErr = processCmd.MarkFlagRequired("child")
	if flagErr != nil {
		return nil, flagErr
	}

	processCmd.Flags().Var(flags.NewTimeFlag(&childProcessStartTime, osutil.RFC3339MiliTimestampFormat), "child-start-time", "If present, specifies the start time of the child process. This is used to ensure the correct process will be shut down. The time format is RFC3339 with millisecond precision, for example "+osutil.RFC3339MiliTimestampFormat)

	return processCmd, nil
}

func monitorProcess(log logr.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log = log.WithName("ProcessMonitor").WithValues(
			"MonitorPID", monitorPid,
			"ChildPID", childPid,
		)

		if resourceId != "" {
			log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, resourceId)
		}

		monitorCtx, monitorCtxCancel, monitorCtxErr := cmds.MonitorPid(cmd.Context(), monitorPid, monitorProcessStartTime, monitorInterval, log)
		defer monitorCtxCancel()
		if monitorCtxErr != nil {
			if errors.Is(monitorCtxErr, os.ErrProcessDone) {
				// If the monitor process is already terminated, stop the service immediately
				log.Info("Monitored process already exited, shutting down child process...")
				executor := process.NewOSExecutor(log)
				stopErr := executor.StopProcess(childPid, childProcessStartTime)
				if stopErr != nil {
					log.Error(stopErr, "Failed to stop child process")
					return stopErr
				}

				return nil
			} else {
				log.Error(monitorCtxErr, "Process could not be monitored")
				return monitorCtxErr
			}
		}

		childProcessCtx, childProcessCtxCancel, childMonitorErr := cmds.MonitorPid(cmd.Context(), childPid, childProcessStartTime, monitorInterval, log)
		defer childProcessCtxCancel()
		if childMonitorErr != nil {
			// Log as Info--we might leak the child process if regular cleanup fails, but this should be rare.
			log.Info("Child process could not be monitored", "Error", childMonitorErr)
			return nil
		}

		attachErr := attachToTargetProcessConsole(log, childPid)
		if attachErr != nil {
			// Error already logged in attachToTargetProcessConsole
			return attachErr
		}

		select {

		case <-monitorCtx.Done():
			if childProcessCtx.Err() == nil {
				log.Info("Monitored process exited, shutting down child process")
				executor := process.NewOSExecutor(log)
				stopErr := executor.StopProcess(childPid, childProcessStartTime)
				if stopErr != nil {
					log.Error(stopErr, "Failed to stop child service process")
					return stopErr
				}
			}

		case <-childProcessCtx.Done():
			// This is what we expect most of the time
			log.V(1).Info("Child service process exited, DCPPROC is done")
		}

		return nil
	}
}
