/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	cmds "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

type forkProcessOptions struct {
	enableChildCleanup bool
	skipDescendants    bool
}

func NewForkProcessCommand(log logr.Logger) (*cobra.Command, error) {
	options := &forkProcessOptions{}
	forkProcessCmd := &cobra.Command{
		Use:          "fork-process [--monitor pid] [--monitor-identity-time time] [--enable-child-cleanup [--skip-descendants]] -- command [args...]",
		Short:        "Starts a detached child process.",
		Long:         "Starts a child process outside of the parent process tree or process group and writes the child PID to stdout. If a monitor process is provided, waits for either the child or monitor process to exit and exits with the child process exit code if the child exits first. If the monitor process exits first, fork-process exits and leaves the child process running unless --enable-child-cleanup is specified. Enabled cleanup also stops the child when fork-process receives a shutdown signal or PID output fails. Child stdout and stderr are not forwarded.",
		RunE:         forkProcess(log, options),
		Args:         validateForkProcessArgs,
		SilenceUsage: true,
	}

	cmds.AddMonitorFlags(forkProcessCmd)
	forkProcessCmd.Flags().BoolVar(&options.enableChildCleanup, "enable-child-cleanup", false, "Stops the child process tree when the monitored owner exits, fork-process receives a shutdown signal, or PID output fails. Requires --monitor.")
	forkProcessCmd.Flags().BoolVar(&options.skipDescendants, "skip-descendants", false, "Only applies when --enable-child-cleanup is used. Cleans up only the process started by this command, not its children. Shared-console signals may still reach its children.")

	return forkProcessCmd, nil
}

func validateForkProcessArgs(cmd *cobra.Command, args []string) error {
	if len(trimForkProcessArgSeparator(args)) == 0 {
		return fmt.Errorf("command is required")
	}

	cleanupEnabled, cleanupFlagErr := cmd.Flags().GetBool("enable-child-cleanup")
	if cleanupFlagErr != nil {
		return cleanupFlagErr
	}
	skipDescendants, skipFlagErr := cmd.Flags().GetBool("skip-descendants")
	if skipFlagErr != nil {
		return skipFlagErr
	}
	if skipDescendants && !cleanupEnabled {
		return fmt.Errorf("--skip-descendants requires --enable-child-cleanup")
	}
	if cleanupEnabled && !cmd.Flags().Changed("monitor") {
		return fmt.Errorf("--enable-child-cleanup requires --monitor")
	}

	return nil
}

func forkProcess(log logr.Logger, options *forkProcessOptions) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		args = trimForkProcessArgSeparator(args)

		log = log.WithName("ForkProcess").WithValues(
			"Command", args[0],
			"Args", args[1:],
		)

		childCmd := exec.Command(args[0], args[1:]...)
		childCmd.Env = os.Environ()
		logger.WithSessionId(childCmd)
		process.ForkFromParent(childCmd)

		monitorEnabled := cmd.Flags().Changed("monitor")
		var monitorCtx context.Context
		var monitorCtxCancel context.CancelFunc
		if monitorEnabled {
			if options.enableChildCleanup {
				var monitorErr error
				monitorCtx, monitorCtxCancel, monitorErr = monitorForkOwner(cmd, log)
				if monitorErr != nil && !process.IsProcessGoneErr(monitorErr) {
					monitorCtxCancel()
					return monitorErr
				}
			} else {
				monitorCtx, monitorCtxCancel = cmds.GetMonitorContextFromFlags(cmd.Context(), log)
			}
			defer monitorCtxCancel()
			select {
			case <-monitorCtx.Done():
				if options.enableChildCleanup && cmd.Context().Err() != nil {
					return cmd.Context().Err()
				}
				log.Info("Monitored process already exited; forked process will not be started")
				return nil
			default:
			}
		}

		executor := process.NewOSExecutor(log.WithName("ProcessExecutor"))
		defer executor.Dispose()
		return runForkedProcess(cmd, childCmd, monitorCtx, options, executor, log)
	}
}

func monitorForkOwner(cmd *cobra.Command, log logr.Logger) (context.Context, context.CancelFunc, error) {
	ownerPID, pidFlagErr := cmd.Flags().GetInt64("monitor")
	if pidFlagErr != nil {
		return cmd.Context(), func() {}, pidFlagErr
	}
	ownerIdentity, identityFlagErr := time.Parse(osutil.RFC3339MiliTimestampFormat, cmd.Flags().Lookup("monitor-identity-time").Value.String())
	if identityFlagErr != nil {
		return cmd.Context(), func() {}, identityFlagErr
	}
	pollInterval, intervalFlagErr := cmd.Flags().GetUint8("monitor-interval")
	if intervalFlagErr != nil {
		return cmd.Context(), func() {}, intervalFlagErr
	}
	return cmds.MonitorPid(cmd.Context(), process.NewHandle(process.Pid_t(ownerPID), ownerIdentity), pollInterval, log)
}

func runForkedProcess(
	cmd *cobra.Command,
	childCmd *exec.Cmd,
	monitorCtx context.Context,
	options *forkProcessOptions,
	executor process.Executor,
	log logr.Logger,
) (returnErr error) {
	var handle process.ProcessHandle
	var childExitInfoCh chan process.ProcessExitInfo
	var startErr error
	if monitorCtx != nil {
		childExitInfoCh = make(chan process.ProcessExitInfo, 1)
		childExitHandler := process.NewChannelProcessExitHandler(childExitInfoCh)
		var startWaitForProcessExit func()
		// Cleanup is owned here so executor context cancellation cannot override root-only policy.
		handle, startWaitForProcessExit, startErr = executor.StartProcess(context.Background(), childCmd, childExitHandler, process.CreationFlagsNone, nil)
		if startErr == nil {
			startWaitForProcessExit()
		}
	} else {
		handle, startErr = executor.StartAndForget(childCmd, process.CreationFlagsNone)
	}
	if startErr != nil {
		log.Error(startErr, "Failed to start forked process", "Command", childCmd.Path, "Args", childCmd.Args[1:])
		return fmt.Errorf("could not start forked process: %w", startErr)
	}

	cleanupChild := options.enableChildCleanup
	defer func() {
		if !cleanupChild {
			return
		}
		var stopOptions []process.ProcessStopOption
		if options.skipDescendants {
			stopOptions = append(stopOptions, process.StopRootOnly())
		}
		stopErr := runDetachedProcessCleanup(cmd.Context(), func(stopCtx context.Context) error {
			return process.StopViaConsole(stopCtx, log, executor, handle, stopOptions...)
		})
		if process.IsProcessGoneErr(stopErr) {
			log.V(1).Info("Forked process already exited before cleanup", "PID", handle.Pid)
		} else if stopErr != nil {
			log.Error(stopErr, "Failed to stop forked process", "PID", handle.Pid)
			returnErr = errors.Join(returnErr, fmt.Errorf("could not stop forked process: %w", stopErr))
		}
	}()

	if options.enableChildCleanup && monitorCtx.Err() != nil {
		log.Info("Monitored process exited during startup; stopping forked process", "PID", handle.Pid)
		return cmd.Context().Err()
	}
	pid := handle.Pid
	if _, writeErr := fmt.Fprintln(cmd.OutOrStdout(), pid); writeErr != nil {
		log.Error(writeErr, "Failed to write forked process PID", "PID", pid)
		return fmt.Errorf("could not write forked process pid: %w", writeErr)
	}

	if monitorCtx == nil {
		return nil
	}

	select {
	case childExitInfo, ok := <-childExitInfoCh:
		if !ok {
			return fmt.Errorf("forked process exit channel closed without a result")
		}
		if childExitInfo.Err != nil {
			log.Error(childExitInfo.Err, "Error waiting for forked process", "PID", pid)
			return childExitInfo.Err
		}
		if childExitInfo.ExitCode >= 0 {
			cleanupChild = false
		}
		if childExitInfo.ExitCode != 0 {
			exitCode := int(childExitInfo.ExitCode)
			log.Info("Forked process exited with a non-zero exit code", "PID", pid, "ExitCode", exitCode)
			return cmds.NewExitCodeError(fmt.Errorf("forked process exited with code %d", exitCode), exitCode)
		}
		log.V(1).Info("Forked process exited", "PID", pid)
		return nil
	case <-monitorCtx.Done():
		if cmd.Context().Err() != nil {
			return cmd.Context().Err()
		}
		log.Info("Monitored process exited; fork-process is exiting", "PID", pid)
		return nil
	}
}

func trimForkProcessArgSeparator(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}

	return args
}
