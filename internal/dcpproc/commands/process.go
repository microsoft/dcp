/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	cmds "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/internal/flags"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

var (
	childPid              process.Pid_t = process.UnknownPID
	childProcessStartTime time.Time
)

func NewProcessCommand(log logr.Logger) (*cobra.Command, error) {
	processCmd := &cobra.Command{
		Use:   "monitor-process",
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

	processCmd.Flags().Var(flags.NewTimeFlag(&childProcessStartTime, osutil.RFC3339MiliTimestampFormat), "child-identity-time", "If present, specifies the identity time of the child process. This is used to ensure the correct process will be shut down. The time format is RFC3339 with millisecond precision, for example "+osutil.RFC3339MiliTimestampFormat)

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

		monitorCtx, monitorCtxCancel, monitorCtxErr := cmds.MonitorPid(cmd.Context(), process.NewHandle(monitorPid, monitorProcessStartTime), monitorInterval, log)
		defer monitorCtxCancel()
		if monitorCtxErr != nil {
			if isMonitorProcessGoneErr(monitorCtxErr) {
				// If the monitor process is already gone (either exited cleanly, no longer exists, or its PID
				// has been reused by an unrelated process), shut down the child process immediately. Even though
				// we cannot positively identify the original monitor process, the child PID itself is protected
				// by an identity-time check inside StopViaConsole/StopProcess, so we will not accidentally kill
				// an unrelated process even if the child PID has been reused as well.
				log.Info("Monitored process already exited, shutting down child process", "Reason", monitorCtxErr)
				executor := process.NewOSExecutor(log)
				stopErr := process.StopViaConsole(log, executor, process.NewHandle(childPid, childProcessStartTime))
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

		childProcessCtx, childProcessCtxCancel, childMonitorErr := cmds.MonitorPid(cmd.Context(), process.NewHandle(childPid, childProcessStartTime), monitorInterval, log)
		defer childProcessCtxCancel()
		if childMonitorErr != nil {
			// Log as Info--we might leak the child process if regular cleanup fails, but this should be rare.
			log.Info("Child process could not be monitored", "Error", childMonitorErr)
			return nil
		}

		select {

		case <-monitorCtx.Done():
			if childProcessCtx.Err() == nil {
				log.Info("Monitored process exited, shutting down child process")
				executor := process.NewOSExecutor(log)
				stopErr := process.StopViaConsole(log, executor, process.NewHandle(childPid, childProcessStartTime))
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
