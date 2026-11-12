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

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	cmds "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
)

type forkProcessOptions struct {
	enableChildCleanup bool
}

func NewForkProcessCommand(log logr.Logger) (*cobra.Command, error) {
	options := &forkProcessOptions{}
	forkProcessCmd := &cobra.Command{
		Use:          "fork-process [--monitor pid] [--monitor-identity-time time] [--enable-child-cleanup] -- command [args...]",
		Short:        "Starts a detached child process.",
		Long:         "Starts a child process outside of the parent process tree or process group and writes the child PID to stdout on a best-effort basis. PID delivery can race with cleanup and does not guarantee that the process remains alive; consumers must validate the process identity and observe fork-process completion. If a monitor process is provided, the child process is not started if the monitor process has already exited, and fork-process fails without starting it if the monitor process cannot be monitored. Otherwise fork-process waits for either the child or monitor process to exit and exits with the child process exit code if the child exits first. If the monitor process exits first, fork-process exits and leaves the child process running unless --enable-child-cleanup is specified. Enabled cleanup stops only the launched process when the monitored owner exits, fork-process receives a shutdown signal, or PID output fails; descendants are not enumerated or stopped, although shared-console signals may still reach them. Child stdout and stderr are not forwarded.",
		RunE:         forkProcess(log, options),
		Args:         validateForkProcessArgs,
		SilenceUsage: true,
	}

	cmds.AddMonitorFlags(forkProcessCmd)
	forkProcessCmd.Flags().BoolVar(&options.enableChildCleanup, "enable-child-cleanup", false, "Stops only the launched process when the monitored owner exits, fork-process receives a shutdown signal, or PID output fails. Descendants are not enumerated or stopped. Requires --monitor.")

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
	if cleanupEnabled && !cmd.Flags().Changed("monitor") {
		return fmt.Errorf("--enable-child-cleanup requires --monitor")
	}

	return nil
}

// Starts the child process outside of the fork-process process tree and reports its PID. Without --monitor,
// fork-process exits right away. With --monitor, it waits for the child or the monitored owner process to exit,
// and the child is left running if the owner exits first, unless --enable-child-cleanup is specified.
func forkProcess(log logr.Logger, options *forkProcessOptions) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		args = trimForkProcessArgSeparator(args)

		forkLog := log.WithName("ForkProcess").WithValues(
			"Command", args[0],
			"Args", args[1:],
		)

		childCmd := exec.Command(args[0], args[1:]...)
		childCmd.Env = os.Environ()
		logger.WithSessionId(childCmd)
		process.ForkFromParent(childCmd)

		if !cmd.Flags().Changed("monitor") {
			return startUnmonitoredForkedProcess(cmd, childCmd, forkLog)
		}

		if options.enableChildCleanup {
			// Owner monitoring writes diagnostics, so a closed stdout or stderr pipe must already produce
			// write errors, instead of terminating fork-process before the child can be stopped.
			enableForkProcessSIGPIPEErrorsForProcessLifetime()
		}

		ownerCtx, ownerCtxCancel, monitorErr := cmds.MonitorPidFromFlags(cmd.Context(), forkLog)
		defer ownerCtxCancel()
		if monitorErr != nil && !process.IsProcessGoneErr(monitorErr) {
			return monitorErr
		}
		if ownerCtx.Err() != nil {
			if ownerErr := forkOwnerMonitorResult(cmd.Context(), ownerCtx); ownerErr != nil {
				return ownerErr
			}
			forkLog.Info("Monitored process already exited; forked process will not be started")
			return nil
		}

		if options.enableChildCleanup {
			// Stopping the child must not depend on diagnostics that can block, so the executor does not log.
			executor := process.NewOSExecutor(logr.Discard())
			defer executor.Dispose()
			return superviseForkedProcessWithCleanup(cmd, childCmd, ownerCtx, executor, forkLog)
		}

		executor := process.NewOSExecutor(forkLog.WithName("ProcessExecutor"))
		defer executor.Dispose()
		return superviseForkedProcess(cmd, childCmd, ownerCtx, executor, forkLog)
	}
}

// Starts the child process without observing it afterwards, and reports its PID.
func startUnmonitoredForkedProcess(cmd *cobra.Command, childCmd *exec.Cmd, log logr.Logger) error {
	executor := process.NewOSExecutor(log.WithName("ProcessExecutor"))
	defer executor.Dispose()

	child, startErr := executor.StartAndForget(childCmd, process.CreationFlagsNone)
	if startErr != nil {
		return reportForkedProcessStartFailure(childCmd, startErr, log)
	}
	return writeForkedProcessPid(cmd, child.Pid, log)
}

// Reports the child PID, then waits for the child or the monitored owner process to exit.
// If the owner exits first, the child is left running.
func superviseForkedProcess(
	cmd *cobra.Command,
	childCmd *exec.Cmd,
	ownerCtx context.Context,
	executor process.Executor,
	log logr.Logger,
) error {
	child, childExitCh, startErr := startObservedForkedProcess(executor, childCmd, log)
	if startErr != nil {
		return startErr
	}
	if writeErr := writeForkedProcessPid(cmd, child.Pid, log); writeErr != nil {
		return writeErr
	}

	select {
	case exitInfo, received := <-childExitCh:
		outcome := childExitOutcome(exitInfo, received)
		outcome.logDiagnostic(log)
		return outcome.err
	case <-ownerCtx.Done():
		if commandErr := cmd.Context().Err(); commandErr != nil {
			return commandErr
		}
		log.Info("Monitored process exited; fork-process is exiting", "PID", child.Pid)
		return nil
	}
}

// Reports the child PID, then waits for the child or the monitored owner process to exit.
// Unless the child is observed to exit on its own, it is stopped before returning, even if supervision panics.
// Only the child is stopped; its descendants are not enumerated or stopped.
// Diagnostics are logged after the child is stopped, so that a blocked stderr cannot delay the cleanup.
func superviseForkedProcessWithCleanup(
	cmd *cobra.Command,
	childCmd *exec.Cmd,
	ownerCtx context.Context,
	executor process.Executor,
	log logr.Logger,
) (returnErr error) {
	child, childExitCh, startErr := startObservedForkedProcess(executor, childCmd, log)
	if startErr != nil {
		return startErr
	}

	var outcome forkedProcessOutcome // The zero value means that the child must be stopped.
	defer func() {
		if !outcome.childExited {
			if stopErr := stopForkedProcess(cmd.Context(), executor, child, log); stopErr != nil {
				returnErr = errors.Join(returnErr, stopErr)
			}
		}
		outcome.logDiagnostic(log)
	}()

	outcome = awaitForkedProcessOutcome(cmd, ownerCtx, child.Pid, childExitCh)
	return outcome.err
}

// Reports the child PID unless the monitored owner process has already exited, then waits for the child
// or the owner to exit. It does not stop the child or log anything; the returned outcome describes the result.
// The owner context must be derived from the command context, so that cancelling fork-process ends the wait.
func awaitForkedProcessOutcome(
	cmd *cobra.Command,
	ownerCtx context.Context,
	pid process.Pid_t,
	childExitCh <-chan process.ProcessExitInfo,
) forkedProcessOutcome {
	if ownerCtx.Err() != nil {
		// The child is about to be stopped, so it must not be handed off.
		return ownerExitedOutcome(cmd.Context(), ownerCtx, pid, "Monitored process exited during startup; fork-process is exiting")
	}

	pidWriteCh := make(chan error, 1)
	go func() {
		// PID output is best effort. If the owner exits first, this goroutine can remain blocked in the write,
		// and the write may complete while the child is being stopped.
		_, writeErr := fmt.Fprintln(cmd.OutOrStdout(), pid)
		pidWriteCh <- writeErr
	}()
	select {
	case writeErr := <-pidWriteCh:
		if writeErr != nil {
			return pidWriteFailedOutcome(pid, writeErr)
		}
	case <-ownerCtx.Done():
		return ownerExitedOutcome(cmd.Context(), ownerCtx, pid, "Monitored process exited before forked process PID was reported; fork-process is exiting")
	}

	select {
	case exitInfo, received := <-childExitCh:
		return childExitOutcome(exitInfo, received)
	case <-ownerCtx.Done():
		return ownerExitedOutcome(cmd.Context(), ownerCtx, pid, "Monitored process exited; fork-process is exiting")
	}
}

// Describes how supervision of a started child process ended.
type forkedProcessOutcome struct {
	// The fork-process result.
	err error

	// True if the child was observed to exit on its own, so there is nothing to stop.
	childExited bool

	// Logs the outcome, if there is anything to log.
	diagnostic func(log logr.Logger)
}

func (outcome forkedProcessOutcome) logDiagnostic(log logr.Logger) {
	if outcome.diagnostic != nil {
		outcome.diagnostic(log)
	}
}

// Describes the end of supervision caused by the monitored owner process exit, an owner monitoring failure,
// or fork-process cancellation. Only the owner exit is logged; the other causes are reported as errors.
func ownerExitedOutcome(commandCtx context.Context, ownerCtx context.Context, pid process.Pid_t, message string) forkedProcessOutcome {
	if ownerErr := forkOwnerMonitorResult(commandCtx, ownerCtx); ownerErr != nil {
		return forkedProcessOutcome{err: ownerErr}
	}
	return forkedProcessOutcome{
		diagnostic: func(log logr.Logger) { log.Info(message, "PID", pid) },
	}
}

// Describes the end of supervision caused by a failure to report the child PID.
func pidWriteFailedOutcome(pid process.Pid_t, writeErr error) forkedProcessOutcome {
	return forkedProcessOutcome{
		err:        fmt.Errorf("could not write forked process pid: %w", writeErr),
		diagnostic: func(log logr.Logger) { log.Error(writeErr, "Failed to write forked process PID", "PID", pid) },
	}
}

// Describes the end of supervision caused by the child exit notification.
// If waiting for the child failed, the child might still be running.
func childExitOutcome(exitInfo process.ProcessExitInfo, received bool) forkedProcessOutcome {
	switch {
	case !received:
		return forkedProcessOutcome{err: fmt.Errorf("forked process exit channel closed without a result")}
	case exitInfo.Err != nil:
		return forkedProcessOutcome{
			err: exitInfo.Err,
			diagnostic: func(log logr.Logger) {
				log.Error(exitInfo.Err, "Error waiting for forked process", "PID", exitInfo.PID)
			},
		}
	case exitInfo.ExitCode != 0:
		exitCode := int(exitInfo.ExitCode)
		return forkedProcessOutcome{
			err:         cmds.NewExitCodeError(fmt.Errorf("forked process exited with code %d", exitCode), exitCode),
			childExited: true,
			diagnostic: func(log logr.Logger) {
				log.Info("Forked process exited with a non-zero exit code", "PID", exitInfo.PID, "ExitCode", exitCode)
			},
		}
	default:
		return forkedProcessOutcome{
			childExited: true,
			diagnostic:  func(log logr.Logger) { log.V(1).Info("Forked process exited", "PID", exitInfo.PID) },
		}
	}
}

// Starts the child process and returns a channel that receives the child exit notification.
func startObservedForkedProcess(
	executor process.Executor,
	childCmd *exec.Cmd,
	log logr.Logger,
) (process.ProcessHandle, <-chan process.ProcessExitInfo, error) {
	childExitCh := make(chan process.ProcessExitInfo, 1)
	// A cancellable start context would let the executor stop the child, and its descendants, on cancellation.
	// Whether and how the child is stopped is decided by fork-process alone.
	child, startWaitForExit, startErr := executor.StartProcess(
		context.Background(), childCmd, process.NewChannelProcessExitHandler(childExitCh), process.CreationFlagsNone, nil,
	)
	if startErr != nil {
		return child, nil, reportForkedProcessStartFailure(childCmd, startErr, log)
	}
	startWaitForExit()
	return child, childExitCh, nil
}

func reportForkedProcessStartFailure(childCmd *exec.Cmd, startErr error, log logr.Logger) error {
	log.Error(startErr, "Failed to start forked process", "Command", childCmd.Path, "Args", childCmd.Args[1:])
	return fmt.Errorf("could not start forked process: %w", startErr)
}

// Writes the child PID to stdout, and logs a write failure right away.
func writeForkedProcessPid(cmd *cobra.Command, pid process.Pid_t, log logr.Logger) error {
	_, writeErr := fmt.Fprintln(cmd.OutOrStdout(), pid)
	if writeErr == nil {
		return nil
	}
	outcome := pidWriteFailedOutcome(pid, writeErr)
	outcome.logDiagnostic(log)
	return outcome.err
}

// Stops the child process, but not its descendants. Stopping is bounded by the standard stop timeout,
// and is not interrupted by fork-process cancellation.
func stopForkedProcess(ctx context.Context, executor process.Executor, child process.ProcessHandle, log logr.Logger) error {
	stopErr := runDetachedProcessCleanup(ctx, func(stopCtx context.Context) error {
		// Stopping the child must not depend on diagnostics that can block.
		return process.StopViaConsole(stopCtx, logr.Discard(), executor, child, process.StopRootOnly())
	})
	if process.IsProcessGoneErr(stopErr) {
		log.V(1).Info("Forked process already exited before cleanup", "PID", child.Pid)
		return nil
	}
	if stopErr != nil {
		log.Error(stopErr, "Failed to stop forked process", "PID", child.Pid)
		return fmt.Errorf("could not stop forked process: %w", stopErr)
	}
	return nil
}

// Returns the fork-process result for a done owner monitoring context: the command cancellation error,
// the owner monitoring failure, or nil if the owner process exited (or its PID is gone or reused).
func forkOwnerMonitorResult(commandCtx context.Context, monitorCtx context.Context) error {
	if commandErr := commandCtx.Err(); commandErr != nil {
		return commandErr
	}
	monitorErr := context.Cause(monitorCtx)
	if monitorErr == nil || errors.Is(monitorErr, context.Canceled) || process.IsProcessGoneErr(monitorErr) {
		return nil
	}
	return fmt.Errorf("could not monitor fork owner: %w", monitorErr)
}

func trimForkProcessArgSeparator(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}

	return args
}
