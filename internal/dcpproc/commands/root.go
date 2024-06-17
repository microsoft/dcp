package commands

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

var (
	dcpMonitorPidInt64  int64 = int64(process.UnknownPID)
	procMonitorPidInt64 int64 = int64(process.UnknownPID)
	monitorInterval     uint8
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
		RunE:         monitorProcess(log),
		SilenceUsage: true,
	}

	rootCmd.Flags().Int64VarP(&dcpMonitorPidInt64, "monitor", "m", int64(process.UnknownPID), "Tells DCPPROC to monitor the given PID (should be DCP). Will trigger cleanup of monitored service process if the watched process exits for any reason.")
	rootCmd.Flags().Int64VarP(&procMonitorPidInt64, "proc", "p", int64(process.UnknownPID), "Tells DCPPROC to monitor the given service PID and shut it down if the DCP process exits for any reason.")
	rootCmd.Flags().Uint8VarP(&monitorInterval, "monitor-interval", "i", 0, "If present, specifies the time in seconds between checks for the monitor PID.")

	return rootCmd, nil
}

func monitorProcess(log logger.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log := log.WithName("monitor")

		pidT, pidErr := process.Int64ToPidT(procMonitorPidInt64)
		if pidErr != nil {
			log.Error(pidErr, "could not convert PID to PIDT", "PID", procMonitorPidInt64)
			return pidErr
		}

		monitorCtx, dcpMonitorErr := cmds.MonitorPid(cmd.Context(), dcpMonitorPidInt64, monitorInterval, log)
		if dcpMonitorErr != nil {
			if errors.Is(dcpMonitorErr, os.ErrProcessDone) {
				// If the monitor process is already terminated, stop the service immediately
				log.Info("DCP process already exited, shutting down service process", "DCPPID", dcpMonitorPidInt64, "PID", procMonitorPidInt64)
				executor := process.NewOSExecutor(log)
				stopErr := executor.StopProcess(pidT)
				if stopErr != nil {
					log.Error(stopErr, "failed to stop service process", "PID", procMonitorPidInt64)
					return stopErr
				}

				return nil
			} else {
				log.Error(dcpMonitorErr, "invalid DCP monitor PID", "DCPPID", dcpMonitorPidInt64)
				return dcpMonitorErr
			}
		}

		processCtx, procMonitorErr := cmds.MonitorPid(cmd.Context(), procMonitorPidInt64, monitorInterval, log)
		if procMonitorErr != nil {
			log.Error(procMonitorErr, "invalid service monitor PID", "PID", procMonitorPidInt64)
			return procMonitorErr
		}

		select {
		case <-monitorCtx.Done():
			if processCtx.Err() == nil {
				// Unexpected DCP shutdown, force the watched process tree to exit
				log.Info("DCP process exited, shutting down service process", "DCPPID", dcpMonitorPidInt64, "PID", procMonitorPidInt64)

				executor := process.NewOSExecutor(log)
				stopErr := executor.StopProcess(pidT)
				if stopErr != nil {
					log.Error(stopErr, "failed to stop service process", "PID", procMonitorPidInt64)
					return stopErr
				}
			}
		case <-processCtx.Done():
			// This is expected if the watched process exits
			log.V(1).Info("service process exited", "PID", procMonitorPidInt64)
		}

		return nil
	}
}
