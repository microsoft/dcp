/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	cmds "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/pkg/process"
)

const (
	// The name of the command, also used when 'fork-process' builds an invocation of it.
	ForkProcessExecCmdName = "fork-process-exec"

	// The flag carrying the resolved path of the image to execute. It is passed separately from
	// the arguments so that the child keeps the argv[0] the caller asked for.
	execPathFlagName = "exec-path"

	// The hidden flag carrying the SIGUSR1 disposition captured before this Go process started.
	callerSIGUSR1IgnoredFlagName = "caller-sigusr1-ignored"

	// The descriptor 'fork-process' passes as the only extra file, on which this command reports
	// whether the exec succeeded. It is the first descriptor after the standard streams.
	execStatusFd = 3

	// Reported when the image cannot be executed, matching the shell convention for a command
	// that could not be run. 'fork-process' reports the underlying errno itself, so this is only
	// a fallback for anything that inspects the shim's own exit code.
	execFailedExitCode = 127
)

var (
	execPath             string
	callerSIGUSR1Ignored bool
)

// NewForkProcessExecCommand creates the 'fork-process-exec' command, which installs the clean
// SIGUSR1 disposition requested by 'fork-process' and replaces itself with the requested image.
// It is an implementation detail and is not meant to be invoked directly.
func NewForkProcessExecCommand(log logr.Logger) (*cobra.Command, error) {
	forkProcessExecCmd := &cobra.Command{
		Use:   ForkProcessExecCmdName + " --" + execPathFlagName + " path -- command [args...]",
		Short: "Replaces this process with another program.",
		Long:  "Installs a clean SIGUSR1 disposition captured by 'fork-process' and then replaces this process with the requested program, keeping the same process ID. This prevents children from inheriting signal handler flags that confuse other language runtimes.",
		RunE:  forkProcessExec(log),
		Args:  validateForkProcessExecArgs,

		Hidden:       true,
		SilenceUsage: true,
	}

	forkProcessExecCmd.Flags().StringVar(&execPath, execPathFlagName, "", "Resolved path of the program to execute")
	forkProcessExecCmd.Flags().BoolVar(
		&callerSIGUSR1Ignored,
		callerSIGUSR1IgnoredFlagName,
		false,
		"Whether the caller ignored SIGUSR1 before starting the exec shim",
	)
	hideDispositionFlagErr := forkProcessExecCmd.Flags().MarkHidden(callerSIGUSR1IgnoredFlagName)
	if hideDispositionFlagErr != nil {
		return nil, fmt.Errorf("could not hide --%s: %w", callerSIGUSR1IgnoredFlagName, hideDispositionFlagErr)
	}

	return forkProcessExecCmd, nil
}

func validateForkProcessExecArgs(_ *cobra.Command, args []string) error {
	if len(trimForkProcessArgSeparator(args)) == 0 {
		return fmt.Errorf("command is required")
	}

	return nil
}

func forkProcessExec(log logr.Logger) func(cmd *cobra.Command, args []string) error {
	return func(_ *cobra.Command, args []string) error {
		args = trimForkProcessArgSeparator(args)

		if execPath == "" {
			return fmt.Errorf("--%s is required", execPathFlagName)
		}

		log = log.WithName("ForkProcessExec").WithValues(
			"Path", execPath,
			"Args", args[1:],
		)

		// 'fork-process' waits for this descriptor to close, which is how a successful execve is
		// reported, so it must not survive into the new program. It is always supplied, because
		// this command is only ever started by 'fork-process'.
		statusFile := os.NewFile(execStatusFd, "exec-status")
		syscall.CloseOnExec(execStatusFd)

		targetEnv := os.Environ()

		prepareErr := process.PrepareSIGUSR1ForExec(callerSIGUSR1Ignored)
		if prepareErr != nil {
			writeForkProcessExecFailure(statusFile, prepareErr)
			log.Error(prepareErr, "Could not prepare SIGUSR1 disposition for executed program")
			return cmds.NewExitCodeError(
				fmt.Errorf("could not prepare SIGUSR1 disposition for %q: %w", execPath, prepareErr),
				execFailedExitCode,
			)
		}

		// Exec must immediately follow the signal change. It only returns when it fails; on
		// success this process becomes the requested program.
		execErr := syscall.Exec(execPath, args, targetEnv)

		writeForkProcessExecFailure(statusFile, execErr)

		log.Error(execErr, "Could not execute the requested program")
		return cmds.NewExitCodeError(fmt.Errorf("could not execute %q: %w", execPath, execErr), execFailedExitCode)
	}
}

func writeForkProcessExecFailure(statusFile *os.File, failureErr error) {
	var failureErrno syscall.Errno
	if !errors.As(failureErr, &failureErrno) {
		failureErrno = syscall.EINVAL
	}

	_, _ = fmt.Fprintf(statusFile, "%d", int(failureErrno))
	_ = statusFile.Close()
}
