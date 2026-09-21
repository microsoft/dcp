/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	cmds "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
)

func NewForkProcessCommand(log logr.Logger) (*cobra.Command, error) {
	forkProcessCmd := &cobra.Command{
		Use:   "fork-process [--monitor pid] [--monitor-identity-time time] -- command [args...]",
		Short: "Starts a detached child process.",
		Long:  "Starts a child process outside of the parent process tree or process group and writes the child PID to stdout. If a monitor process is provided, waits for either the child or monitor process to exit and exits with the child process exit code if the child exits first. If the monitor process exits first, fork-process exits and leaves the child process running.",
		RunE:  forkProcess(log),
		Args:  validateForkProcessArgs,

		SilenceUsage: true,
	}

	cmds.AddMonitorFlags(forkProcessCmd)

	return forkProcessCmd, nil
}

func validateForkProcessArgs(_ *cobra.Command, args []string) error {
	if len(trimForkProcessArgSeparator(args)) == 0 {
		return fmt.Errorf("command is required")
	}

	return nil
}

func forkProcess(log logr.Logger) func(cmd *cobra.Command, args []string) error {
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
			monitorCtx, monitorCtxCancel = cmds.GetMonitorContextFromFlags(cmd.Context(), log)
			defer monitorCtxCancel()
			select {
			case <-monitorCtx.Done():
				log.Info("Monitored process already exited; forked process will not be started")
				return nil
			default:
			}
		}

		pid, childExitInfoCh, disposeChildExecutor, startErr := startForkedProcess(cmd, childCmd, monitorEnabled, log)
		if startErr != nil {
			return startErr
		}
		defer disposeChildExecutor()

		if !monitorEnabled {
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
}

func startForkedProcess(
	cmd *cobra.Command,
	childCmd *exec.Cmd,
	observeExit bool,
	log logr.Logger,
) (process.Pid_t, <-chan process.ProcessExitInfo, func(), error) {
	executor := process.NewOSExecutor(log.WithName("ProcessExecutor"))
	dispose := executor.Dispose

	var handle process.ProcessHandle
	var childExitInfoCh chan process.ProcessExitInfo
	var startErr error
	if observeExit {
		childExitInfoCh = make(chan process.ProcessExitInfo, 1)
		childExitHandler := process.NewChannelProcessExitHandler(childExitInfoCh)
		var startWaitForProcessExit func()
		handle, startWaitForProcessExit, startErr = executor.StartProcess(context.Background(), childCmd, childExitHandler, process.CreationFlagsNone, nil)
		if startErr == nil {
			startWaitForProcessExit()
		}
	} else {
		handle, startErr = executor.StartAndForget(childCmd, process.CreationFlagsNone)
	}
	if startErr != nil {
		log.Error(startErr, "Failed to start forked process", "Command", childCmd.Path, "Args", childCmd.Args[1:])
		executor.Dispose()
		return process.UnknownPID, nil, nil, fmt.Errorf("could not start forked process: %w", startErr)
	}

	pid := handle.Pid
	if _, writeErr := fmt.Fprintln(cmd.OutOrStdout(), pid); writeErr != nil {
		log.Error(writeErr, "Failed to write forked process PID", "PID", pid)
		executor.Dispose()
		return process.UnknownPID, nil, nil, fmt.Errorf("could not write forked process pid: %w", writeErr)
	}

	if !observeExit {
		executor.Dispose()
		dispose = func() {}
	}

	return pid, childExitInfoCh, dispose, nil
}

func trimForkProcessArgSeparator(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}

	return args
}
