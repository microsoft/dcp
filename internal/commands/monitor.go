package commands

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/spf13/cobra"
)

var (
	monitorPidInt64 int64 = int64(process.UnknownPID)
	monitorInterval uint8
)

func AddMonitorFlags(cmd *cobra.Command) {
	cmd.Flags().Int64VarP(&monitorPidInt64, "monitor", "m", int64(process.UnknownPID), "If present, tells DCP to monitor a given process ID (PID) and gracefully shutdown if the monitored process exits for any reason.")
	cmd.Flags().Uint8VarP(&monitorInterval, "monitor-interval", "i", 0, "If present, specifies the time in seconds between checks for the monitor PID.")
}

func Monitor(ctx context.Context, logger logr.Logger) context.Context {
	if monitorPidInt64 != int64(process.UnknownPID) {
		monitorCtx, monitorCtxCancel := context.WithCancel(ctx)

		monitorPid, err := process.Int64ToPidT(monitorPidInt64)
		if err != nil {
			logger.Error(err, "error converting PID", "pid", monitorPidInt64)
			monitorCtxCancel()
			return monitorCtx
		}

		monitorProc, err := process.FindWaitableProcess(monitorPid)
		if err != nil {
			logger.Error(err, "error finding process", "pid", monitorPid)
			monitorCtxCancel()
			return monitorCtx
		}

		if monitorInterval > 0 {
			monitorProc.WaitPollInterval = time.Second * time.Duration(monitorInterval)
		}

		go func() {
			defer monitorCtxCancel()
			if waitErr := monitorProc.Wait(monitorCtx); waitErr != nil {
				logger.Error(waitErr, "error waiting for process", "pid", monitorPid)
			} else {
				logger.Info("monitor process exited, shutting down", "pid", monitorPid)
			}
		}()

		return monitorCtx
	}

	return ctx
}
