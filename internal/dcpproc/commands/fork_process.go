/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
)

func NewForkProcessCommand(log logr.Logger) (*cobra.Command, error) {
	forkProcessCmd := &cobra.Command{
		Use:                "fork-process command [args...]",
		Short:              "Starts a detached child process and prints its PID.",
		Long:               "Starts a child process outside of the parent process tree or process group and writes the child PID to stdout.",
		RunE:               forkProcess(log),
		SilenceUsage:       true,
		Args:               validateForkProcessArgs,
		DisableFlagParsing: true,
	}

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

		childCmd := exec.Command(args[0], args[1:]...)
		childCmd.Env = os.Environ()
		logger.WithSessionId(childCmd)
		process.ForkFromParent(childCmd)

		if startErr := childCmd.Start(); startErr != nil {
			log.Error(startErr, "Failed to start forked process", "Command", args[0], "Args", args[1:])
			return fmt.Errorf("could not start forked process: %w", startErr)
		}

		pid := childCmd.Process.Pid
		if releaseErr := childCmd.Process.Release(); releaseErr != nil {
			log.Error(releaseErr, "Failed to release forked process", "PID", pid)
			return fmt.Errorf("could not release forked process %d: %w", pid, releaseErr)
		}

		if _, writeErr := fmt.Fprintln(cmd.OutOrStdout(), pid); writeErr != nil {
			log.Error(writeErr, "Failed to write forked process PID", "PID", pid)
			return fmt.Errorf("could not write forked process pid: %w", writeErr)
		}

		return nil
	}
}

func trimForkProcessArgSeparator(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}

	return args
}
