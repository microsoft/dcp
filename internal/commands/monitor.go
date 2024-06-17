package commands

import (
	"context"
	"errors"
	"fmt"
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

func GetMonitorPid() int64 {
	return monitorPidInt64
}

func MonitorPid(ctx context.Context, pid int64, pollInterval uint8, logger logr.Logger) (context.Context, error) {
	if pid != int64(process.UnknownPID) {
		monitorCtx, monitorCtxCancel := context.WithCancel(ctx)

		monitorPid, err := process.Int64ToPidT(pid)
		if err != nil {
			logger.Error(err, "error converting PID", "pid", pid)
			monitorCtxCancel()
			return monitorCtx, err
		}

		monitorProc, err := process.FindWaitableProcess(monitorPid)
		if err != nil {
			logger.Error(err, "error finding process", "pid", monitorPid)
			monitorCtxCancel()
			return monitorCtx, err
		}

		if pollInterval > 0 {
			monitorProc.WaitPollInterval = time.Second * time.Duration(pollInterval)
		}

		go func() {
			defer monitorCtxCancel()
			if waitErr := monitorProc.Wait(monitorCtx); waitErr != nil {
				if errors.Is(waitErr, context.Canceled) {
					logger.V(1).Info("monitoring cancelled by context", "pid", monitorPid)
				} else {
					logger.Error(waitErr, "error waiting for process", "pid", monitorPid)
				}
			} else {
				logger.Info("monitor process exited, shutting down", "pid", monitorPid)
			}
		}()

		return monitorCtx, nil
	}

	return ctx, fmt.Errorf("no PID to monitor")
}

func Monitor(ctx context.Context, logger logr.Logger) context.Context {
	// Ignore errors as they're logged by MonitorPid and we always return a valid context
	monitorCtx, _ := MonitorPid(ctx, monitorPidInt64, monitorInterval, logger)
	return monitorCtx
}
