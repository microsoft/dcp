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

	// The descriptor 'fork-process' passes as the only extra file, on which this command reports
	// whether the exec succeeded. It is the first descriptor after the standard streams.
	execStatusFd = 3

	// Reported when the image cannot be executed, matching the shell convention for a command
	// that could not be run. 'fork-process' reports the underlying errno itself, so this is only
	// a fallback for anything that inspects the shim's own exit code.
	execFailedExitCode = 127
)

var execPath string

// NewForkProcessExecCommand creates the 'fork-process-exec' command, which replaces itself with
// the requested image after clearing the signal dispositions inherited from the Go runtime.
// It is an implementation detail of 'fork-process' and is not meant to be invoked directly.
func NewForkProcessExecCommand(log logr.Logger) (*cobra.Command, error) {
	forkProcessExecCmd := &cobra.Command{
		Use:   ForkProcessExecCmdName + " --" + execPathFlagName + " path -- command [args...]",
		Short: "Replaces this process with another program.",
		Long:  "Clears the signal dispositions this process inherited from the Go runtime and then replaces it with the requested program, keeping the same process ID. Used internally by 'fork-process' so that children do not inherit signal handler flags that confuse other language runtimes.",
		RunE:  forkProcessExec(log),
		Args:  validateForkProcessExecArgs,

		Hidden:       true,
		SilenceUsage: true,
	}

	forkProcessExecCmd.Flags().StringVar(&execPath, execPathFlagName, "", "Resolved path of the program to execute")

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

		// From this point on the process must not rely on the Go runtime's signal handling,
		// which the reset disables. The only remaining step is the exec.
		process.ResetSignalDispositions()

		// Exec only returns when it fails; on success this process becomes the requested program.
		execErr := syscall.Exec(execPath, args, os.Environ())

		var execErrno syscall.Errno
		if !errors.As(execErr, &execErrno) {
			// Report something the parent can still parse; the message below stays accurate.
			execErrno = syscall.EINVAL
		}

		_, _ = fmt.Fprintf(statusFile, "%d", int(execErrno))
		_ = statusFile.Close()

		exitCode := execFailedExitCode

		log.Error(execErr, "Could not execute the requested program")
		return cmds.NewExitCodeError(fmt.Errorf("could not execute %q: %w", execPath, execErr), exitCode)
	}
}
