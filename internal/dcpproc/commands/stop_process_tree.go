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
	"github.com/microsoft/dcp/internal/dcpproc/protocol"
	"github.com/microsoft/dcp/internal/flags"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

var (
	stopPid              process.Pid_t = process.UnknownPID
	stopProcessStartTime time.Time
	stopSkipDescendants  bool
)

func NewStopProcessTreeCommand(log logr.Logger) (*cobra.Command, error) {
	stopProcessTreeCmd := &cobra.Command{
		Use:          "stop-process-tree",
		Short:        "Stops a process tree identified by the root process ID.",
		Long:         "Stops a process tree identified by the root process ID. On Unix, if the root leads an isolated process group, cleanup also signals the whole group. --skip-descendants disables both tree and Unix group cleanup.",
		RunE:         stopProcessTree(log),
		SilenceUsage: true,
		Args:         cobra.NoArgs,
	}

	stopProcessTreeCmd.Flags().Int64VarP((*int64)(&stopPid), "pid", "", int64(process.UnknownPID), "The PID of the process to stop (the root of the process tree).")
	flagErr := stopProcessTreeCmd.MarkFlagRequired("pid")
	if flagErr != nil {
		return nil, flagErr
	}

	stopProcessTreeCmd.Flags().Var(flags.NewTimeFlag(&stopProcessStartTime, osutil.RFC3339MiliTimestampFormat), "process-start-time", "Specifies the identity time of the root process. If omitted, identity is resolved once at command startup. The time format is RFC3339 with millisecond precision, for example "+osutil.RFC3339MiliTimestampFormat)
	stopProcessTreeCmd.Flags().BoolVar(&stopSkipDescendants, "skip-descendants", false, "If specified, stops only the root process and skips force-killing descendants.")

	return stopProcessTreeCmd, nil
}

func stopProcessTree(log logr.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log = log.WithName("StopProcessTree").WithValues(
			"PID", stopPid,
			"ProcessStartTime", stopProcessStartTime,
			"SkipDescendants", stopSkipDescendants,
		)

		handle, handleErr := cmds.ResolveProcessHandle(stopPid, stopProcessStartTime)
		if handleErr != nil {
			logProcessStopFailure(log, handleErr, "Process to stop already exited", "Could not resolve the process to stop")
			return stopProcessTreeCommandError(handleErr)
		}

		pe := process.NewOSExecutor(log)
		defer pe.Dispose()
		var stopOptions []process.ProcessStopOption
		if stopSkipDescendants {
			stopOptions = append(stopOptions, process.StopRootOnly())
		}

		stopErr := runDetachedProcessCleanup(cmd.Context(), func(stopCtx context.Context) error {
			return process.StopViaConsole(stopCtx, log, pe, handle, stopOptions...)
		})
		if stopErr != nil {
			logProcessStopFailure(log, stopErr, "Process tree already stopped", "Failed to stop process tree")
			return stopProcessTreeCommandError(stopErr)
		}

		return nil
	}
}

func logProcessStopFailure(log logr.Logger, err error, goneMessage string, failureMessage string) {
	if process.IsProcessGoneErr(err) {
		log.V(1).Info(goneMessage, "Error", err)
		return
	}
	log.Error(err, failureMessage)
}

func stopProcessTreeCommandError(err error) error {
	switch {
	case errors.Is(err, process.ErrIncompleteProcessTree):
		return cmds.NewExitCodeError(err, protocol.StopProcessTreeIncompleteExitCode)
	case process.IsProcessGoneErr(err):
		return cmds.NewSilentExitCodeError(err, protocol.StopProcessTreeProcessGoneExitCode)
	default:
		return err
	}
}
