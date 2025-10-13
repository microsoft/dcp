// Copyright (c) Microsoft Corporation. All rights reserved.

package commands

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/flags"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

var (
	// Shared flag data for the process being monitored
	monitorPid              process.Pid_t = process.UnknownPID
	monitorProcessStartTime time.Time
	monitorInterval         uint8
	resourceId              string
)

func NewRootCmd(log *logger.Logger) (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		SilenceErrors: true,
		Use:           "dcpproc",
		Short:         "Monitors dcp and cleans up orphaned resources (processes or containers)",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you a development environment
	with minimum remote dependencies and maximum ease of use.

	dcpproc is a monitor tool responsible for cleaning up orphaned resources when DCP exits.`,
		SilenceUsage:     true,
		PersistentPreRun: cmds.LogVersion(log.Logger, "Starting DCPPROC..."),
		Hidden:           true,
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	log.AddLevelFlag(rootCmd.PersistentFlags())

	rootCmd.PersistentFlags().Int64VarP((*int64)(&monitorPid), "monitor", "m", int64(process.UnknownPID), "Tells DCPPROC to monitor the given PID. Will trigger cleanup of monitored resources if the watched process exits for any reason.")
	flagErr := rootCmd.MarkPersistentFlagRequired("monitor")
	if flagErr != nil {
		return nil, flagErr // Should never happen--the only error would be if the flag was not found
	}

	rootCmd.PersistentFlags().StringVarP(&resourceId, "resource-id", "r", "", "An optional identifier for the resource being monitored. This is used for logging purposes only.")

	rootCmd.PersistentFlags().Var(flags.NewTimeFlag(&monitorProcessStartTime, osutil.RFC3339MiliTimestampFormat), "monitor-start-time", "If present, specifies the start time of the process to monitor. This is used to ensure the correct process is being monitored. The time format is RFC3339 with millisecond precision, for example "+osutil.RFC3339MiliTimestampFormat)

	rootCmd.PersistentFlags().Uint8VarP(&monitorInterval, "monitor-interval", "i", 0, "If present, specifies the time in seconds between checks for the monitor process exit.")

	var err error
	var cmd *cobra.Command

	if cmd, err = NewProcessCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'process' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	if cmd, err = NewContainerCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'container' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	return rootCmd, nil
}
