/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"errors"
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

const childProcessGoneLogMessage = "Child process already exited or its PID was reused; skipping cleanup"

func NewProcessCommand(log logr.Logger) (*cobra.Command, error) {
	processCmd := &cobra.Command{
		Use:   "monitor-process",
		Short: "Ensures that child process is cleaned up when the monitored process exits",
		Long: `Monitors a child process and shuts it down when the monitored process exits.

This command is used to ensure that child processes are properly cleaned up when
DCP terminates unexpectedly. On Unix, an isolated child process group remains
monitored until it has no live members, even if its leader exits. Cleanup targets
both the group and any descendants discoverable through the child process tree.`,
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

	processCmd.Flags().Var(flags.NewTimeFlag(&childProcessStartTime, osutil.RFC3339MiliTimestampFormat), "child-identity-time", "Specifies the identity time of the child process. If omitted, identity is resolved once at command startup. The time format is RFC3339 with millisecond precision, for example "+osutil.RFC3339MiliTimestampFormat)

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

		childHandle, childIdentityErr := cmds.ResolveProcessHandle(childPid, childProcessStartTime)
		if childIdentityErr != nil {
			if process.IsProcessGoneErr(childIdentityErr) {
				log.Info(childProcessGoneLogMessage, "Error", childIdentityErr)
				return nil
			}
			log.Error(childIdentityErr, "Could not resolve child process identity")
			return childIdentityErr
		}

		childGroup, childGroupErr := process.FindProcessGroup(childHandle)
		if childGroupErr != nil {
			if process.IsProcessGoneErr(childGroupErr) {
				log.Info(childProcessGoneLogMessage, "Error", childGroupErr)
				return nil
			}
			log.Error(childGroupErr, "Child process group could not be monitored")
			return childGroupErr
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
				defer executor.Dispose()
				stopErr := runDetachedProcessCleanup(cmd.Context(), func(stopCtx context.Context) error {
					return process.StopViaConsole(stopCtx, log, executor, childHandle, process.StopWithProcessGroup(childGroup))
				})
				if stopErr != nil {
					if childGroup == nil && process.IsProcessGoneErr(stopErr) {
						log.V(1).Info("Child process exited before cleanup completed", "Error", stopErr)
						return nil
					}
					log.Error(stopErr, "Failed to stop child process")
					return stopErr
				}

				return nil
			} else {
				log.Error(monitorCtxErr, "Process could not be monitored")
				return monitorCtxErr
			}
		}

		var childProcess *process.WaitableProcess
		if childGroup == nil {
			var childMonitorErr error
			childProcess, childMonitorErr = process.FindWaitableProcess(childHandle)
			if childMonitorErr != nil {
				if process.IsProcessGoneErr(childMonitorErr) {
					log.Info(childProcessGoneLogMessage, "Error", childMonitorErr)
					return nil
				}
				log.Error(childMonitorErr, "Child process could not be monitored")
				return childMonitorErr
			}
			if monitorInterval > 0 {
				childProcess.WaitPollInterval = time.Second * time.Duration(monitorInterval)
			}
		} else if monitorInterval > 0 {
			childGroup.WaitPollInterval = time.Second * time.Duration(monitorInterval)
		}
		childProcessCtx, childProcessCtxCancel := context.WithCancel(cmd.Context())
		defer childProcessCtxCancel()
		childExited := make(chan error, 1)
		go func() {
			if childGroup != nil {
				childExited <- childGroup.Wait(childProcessCtx)
			} else {
				childExited <- childProcess.Wait(childProcessCtx)
			}
		}()
		if childGroup != nil {
			log.Info("Started monitoring process group", "PGID", childPid)
		} else {
			log.Info("Started monitoring process", "PID", childPid)
		}

		select {
		case <-monitorCtx.Done():
		case childWaitErr, received := <-childExited:
			if !received {
				return errors.New("child process monitoring ended without a result")
			}
			if childWaitErr != nil && !errors.Is(childWaitErr, context.Canceled) {
				log.Error(childWaitErr, "Error waiting for child process cleanup target")
				return childWaitErr
			}
			if cmd.Context().Err() == nil {
				log.V(1).Info("Child service process exited, DCPPROC is done")
				return nil
			}
		}

		if cmd.Context().Err() != nil {
			log.Info("Process monitor interrupted, shutting down child process")
		} else {
			log.Info("Monitored process exited, shutting down child process")
		}
		executor := process.NewOSExecutor(log)
		defer executor.Dispose()
		stopErr := runDetachedProcessCleanup(cmd.Context(), func(stopCtx context.Context) error {
			return process.StopViaConsole(stopCtx, log, executor, childHandle, process.StopWithProcessGroup(childGroup))
		})
		if stopErr != nil {
			if childGroup == nil && process.IsProcessGoneErr(stopErr) {
				log.V(1).Info("Child service process exited before cleanup completed", "Error", stopErr)
				return nil
			}
			log.Error(stopErr, "Failed to stop child service process")
		}
		return stopErr
	}
}

func runDetachedProcessCleanup(parent context.Context, cleanup func(context.Context) error) error {
	stopCtx, stopCancel := process.WithDetachedStopTimeout(parent)
	defer stopCancel()
	return cleanup(stopCtx)
}
