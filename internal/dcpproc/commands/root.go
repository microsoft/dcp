package commands

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

var (
	dcpMonitorPid          process.Pid_t = process.UnknownPID
	dcpProcessStartTimeStr string

	childMonitorPid          process.Pid_t = process.UnknownPID
	childProcessStartTimeStr string

	monitorInterval uint8
)

func NewRootCmd(log logger.Logger) (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		SilenceErrors: true,
		Use:           "dcpproc",
		Short:         "Monitors dcp and service process PIDs and cleans up orphaned resources",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you a development environment
	with minimum remote dependencies and maximum ease of use.

	dcpproc is a monitor process responsible for cleaning up orphaned service processes.`,
		RunE:             monitorProcess(log),
		SilenceUsage:     true,
		PersistentPreRun: cmds.LogVersion(log, "Starting DCPPROC..."),
	}

	rootCmd.Flags().Int64VarP((*int64)(&dcpMonitorPid), "monitor", "m", int64(process.UnknownPID), "Tells DCPPROC to monitor the given PID (should be DCP). Will trigger cleanup of monitored service process if the watched process exits for any reason.")
	rootCmd.Flags().StringVar(&dcpProcessStartTimeStr, "monitor-start-time", "", "If present, specifies the start time of the DCP process to monitor. This is used to ensure the correct process is being monitored. The time format is RFC3339 with millisecond precsion, for example "+osutil.RFC3339MiliTimestampFormat)
	rootCmd.Flags().Int64VarP((*int64)(&childMonitorPid), "child", "p", int64(process.UnknownPID), "Tells DCPPROC to monitor the given child process PID and shut it down if the DCP process exits for any reason.")
	rootCmd.Flags().StringVar(&childProcessStartTimeStr, "child-start-time", "", "If present, specifies the start time of the child process to monitor. This is used to ensure the correct process is being monitored. The time format is RFC3339 with millisecond precsion, for example "+osutil.RFC3339MiliTimestampFormat)
	rootCmd.Flags().Uint8VarP(&monitorInterval, "monitor-interval", "i", 0, "If present, specifies the time in seconds between checks for the monitor PID.")

	return rootCmd, nil
}

func monitorProcess(log logger.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log := log.WithName("monitor")

		dcpProcessStartTime, dcpProcessStartTimeErr := cmds.ParseProcessStartTime(dcpProcessStartTimeStr)
		if dcpProcessStartTimeErr != nil {
			log.Error(dcpProcessStartTimeErr, "invalid DCP process start time, DCP process identity could not be verified, exiting...", "DCPPID", dcpMonitorPid)
			return dcpProcessStartTimeErr
		}

		childProcessStartTime, childProcessStartTimeErr := cmds.ParseProcessStartTime(childProcessStartTimeStr)
		if childProcessStartTimeErr != nil {
			log.Error(childProcessStartTimeErr, "invalid child service process start time, child service process identity could not be verified, exiting...", "PID", childMonitorPid)
			return childProcessStartTimeErr
		}

		monitorCtx, monitorCtxCancel, dcpMonitorErr := cmds.MonitorPid(cmd.Context(), dcpMonitorPid, dcpProcessStartTime, monitorInterval, log)
		defer monitorCtxCancel()
		if dcpMonitorErr != nil {
			if errors.Is(dcpMonitorErr, os.ErrProcessDone) {
				// If the monitor process is already terminated, stop the service immediately
				log.Info("DCP process already exited, shutting down child service process", "DCPPID", dcpMonitorPid, "PID", childMonitorPid)
				executor := process.NewOSExecutor(log)
				stopErr := executor.StopProcess(childMonitorPid, childProcessStartTime)
				if stopErr != nil {
					log.Error(stopErr, "failed to stop child service process", "PID", childMonitorPid)
					return stopErr
				}

				return nil
			} else {
				log.Error(dcpMonitorErr, "DCP process could not be monitored", "DCPPID", dcpMonitorPid)
				return dcpMonitorErr
			}
		}

		childProcessCtx, childProcessCtxCancel, childMonitorErr := cmds.MonitorPid(cmd.Context(), childMonitorPid, childProcessStartTime, monitorInterval, log)
		defer childProcessCtxCancel()
		if childMonitorErr != nil {
			log.Error(childMonitorErr, "child service process could not be monitored", "PID", childMonitorPid)
			return childMonitorErr
		}

		select {

		case <-monitorCtx.Done():
			if childProcessCtx.Err() == nil {
				// Unexpected DCP shutdown, force the watched process tree to exit
				log.Info("DCP process exited, shutting down child service process", "DCPPID", dcpMonitorPid, "PID", childMonitorPid)
				executor := process.NewOSExecutor(log)
				stopErr := executor.StopProcess(childMonitorPid, childProcessStartTime)
				if stopErr != nil {
					log.Error(stopErr, "failed to stop child service process", "PID", childMonitorPid)
					return stopErr
				}
			}

		case <-childProcessCtx.Done():
			// This is expected if the watched process exits
			log.V(1).Info("child service process exited", "PID", childMonitorPid)
		}

		return nil
	}
}
