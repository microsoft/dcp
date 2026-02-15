/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"github.com/microsoft/dcp/internal/flags"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

var (
	stopPid              process.Pid_t = process.UnknownPID
	stopProcessStartTime time.Time
)

func NewStopProcessTreeCommand(log logr.Logger) (*cobra.Command, error) {
	stopProcessTreeCmd := &cobra.Command{
		Use:          "stop-process-tree",
		Short:        "Stops a process tree identified by the root process ID.",
		RunE:         stopProcessTree(log),
		SilenceUsage: true,
		Args:         cobra.NoArgs,
	}

	stopProcessTreeCmd.Flags().Int64VarP((*int64)(&stopPid), "pid", "", int64(process.UnknownPID), "The PID of the process to stop (the root of the process tree).")
	flagErr := stopProcessTreeCmd.MarkFlagRequired("pid")
	if flagErr != nil {
		return nil, flagErr
	}

	stopProcessTreeCmd.Flags().Var(flags.NewTimeFlag(&stopProcessStartTime, osutil.RFC3339MiliTimestampFormat), "process-start-time", "If present, specifies the start time of the root process of the process tree to be stopped. This is used to ensure the correct process tree will be stopped. The time format is RFC3339 with millisecond precision, for example "+osutil.RFC3339MiliTimestampFormat)

	return stopProcessTreeCmd, nil
}

func stopProcessTree(log logr.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log = log.WithName("StopProcessTree").WithValues(
			"PID", stopPid,
			"ProcessStartTime", stopProcessStartTime,
		)

		_, procErr := process.FindWaitableProcess(process.NewProcessHandle(stopPid, stopProcessStartTime))
		if procErr != nil {
			log.Error(procErr, "Could not find the process to stop")
			return procErr
		}

		attachErr := attachToTargetProcessConsole(log, stopPid)
		if attachErr != nil {
			// Error already logged in attachToTargetProcessConsole
			return attachErr
		}

		pe := process.NewOSExecutor(log)
		stopErr := pe.StopProcess(process.NewProcessHandle(stopPid, stopProcessStartTime))
		if stopErr != nil {
			log.Error(stopErr, "Failed to stop process tree")
			return stopErr
		}

		return nil
	}
}
