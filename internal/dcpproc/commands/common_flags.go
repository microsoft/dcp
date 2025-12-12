// Copyright (c) Microsoft Corporation. All rights reserved.

package commands

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/internal/flags"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

var (
	monitorPid              process.Pid_t = process.UnknownPID
	monitorProcessStartTime time.Time
	monitorInterval         uint8
)

func addMonitorFlags(cmd *cobra.Command) error {
	cmd.PersistentFlags().Int64VarP((*int64)(&monitorPid), "monitor", "m", int64(process.UnknownPID), "Tells DCPPROC to monitor the given PID. Will trigger cleanup of monitored resources if the watched process exits for any reason.")
	flagErr := cmd.MarkPersistentFlagRequired("monitor")
	if flagErr != nil {
		return flagErr // Should never happen--the only error would be if the flag was not found
	}

	cmd.PersistentFlags().Var(flags.NewTimeFlag(&monitorProcessStartTime, osutil.RFC3339MiliTimestampFormat), "monitor-identity-time", "If present, specifies the identity time of the process to monitor. This is used to ensure the correct process is being monitored. The time format is RFC3339 with millisecond precision, for example "+osutil.RFC3339MiliTimestampFormat)

	cmd.PersistentFlags().Uint8VarP(&monitorInterval, "monitor-interval", "i", 0, "If present, specifies the time in seconds between checks for the monitor process exit.")

	return nil
}
