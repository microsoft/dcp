/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

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

		execShim, shimErr := useExecShim(childCmd)
		if shimErr != nil {
			return shimErr
		}
		if execShim != nil {
			defer execShim.close()
		}

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

		pid, childExitInfoCh, disposeChildExecutor, startErr := startForkedProcess(cmd, childCmd, execShim, monitorEnabled, log)
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
	execShim *execShimHandshake,
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

	// Starting the shim only means dcp itself started. The PID must not be reported before the
	// requested program is known to be running, so that a program which cannot be executed is
	// still reported as a start failure.
	if execShim != nil {
		if execErr := execShim.wait(); execErr != nil {
			// The logger already carries the command and arguments.
			log.Error(execErr, "Failed to execute forked process")
			executor.Dispose()
			return process.UnknownPID, nil, nil, fmt.Errorf("could not start forked process: %w", execErr)
		}
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

// Redirects the child through the 'fork-process-exec' command on platforms where the child would
// otherwise inherit the Go runtime's signal handler flags. The shim clears those flags and then
// execs the original program, which keeps the process ID, session, standard streams, and exit
// code that the caller of 'fork-process' expects.
//
// The reset cannot be done here: the Go runtime restores its own signal dispositions in the
// forked child before it reaches execve, so it has to happen in the process that calls exec.
//
// Returns the handshake that reports whether the shim reached the requested program, or nil when
// the child is started directly. The caller owns the returned handshake and must close it.
func useExecShim(childCmd *exec.Cmd) (*execShimHandshake, error) {
	if !process.SignalDispositionsLeakToChildren() {
		return nil, nil
	}

	if childCmd.Err != nil {
		// The program could not be located. Leave the command untouched so that starting it
		// reports that original failure rather than one from the shim.
		return nil, nil
	}

	dcpPath, dcpPathErr := os.Executable()
	if dcpPathErr != nil {
		return nil, fmt.Errorf("could not determine the path of the current executable: %w", dcpPathErr)
	}

	statusR, statusW, pipeErr := os.Pipe()
	if pipeErr != nil {
		return nil, fmt.Errorf("could not create the exec status pipe: %w", pipeErr)
	}

	shimArgs := []string{dcpPath, ForkProcessExecCmdName, "--" + execPathFlagName, childCmd.Path, "--"}
	childCmd.Args = append(shimArgs, childCmd.Args...)
	childCmd.Path = dcpPath

	// The shim reports the outcome of the exec on this descriptor. It is the only extra file, so
	// the shim sees it as execStatusFd.
	childCmd.ExtraFiles = append(childCmd.ExtraFiles, statusW)

	return &execShimHandshake{statusR: statusR, statusW: statusW}, nil
}

// execShimHandshake reports whether the shim managed to exec the requested program. Starting the
// shim only proves that dcp itself could be started, so without this the caller would be told
// that a program which never ran had started successfully.
//
// The shim inherits the write end. A successful execve closes it and the read end reports EOF,
// while a failure sends the errno before the shim exits.
type execShimHandshake struct {
	statusR *os.File
	statusW *os.File
}

// wait blocks until the shim either replaces itself with the requested program or reports why it
// could not. It returns the failure that a direct start would have reported.
func (h *execShimHandshake) wait() error {
	// The write end is now owned by the shim. The parent's copy has to go, because the read below
	// only reports EOF once every writer is closed.
	h.closeWriteEnd()

	status, readErr := io.ReadAll(h.statusR)
	if readErr != nil {
		return fmt.Errorf("could not read the exec status: %w", readErr)
	}

	if len(status) == 0 {
		// EOF with nothing written: the descriptor was closed by a successful execve.
		return nil
	}

	errnoValue, parseErr := strconv.Atoi(strings.TrimSpace(string(status)))
	if parseErr != nil {
		return fmt.Errorf("the exec status %q could not be parsed: %w", status, parseErr)
	}

	return syscall.Errno(errnoValue)
}

func (h *execShimHandshake) closeWriteEnd() {
	if h.statusW != nil {
		_ = h.statusW.Close()
		h.statusW = nil
	}
}

func (h *execShimHandshake) close() {
	h.closeWriteEnd()

	if h.statusR != nil {
		_ = h.statusR.Close()
		h.statusR = nil
	}
}
