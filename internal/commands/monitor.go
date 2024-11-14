package commands

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/spf13/cobra"
)

var (
	monitorPid                process.Pid_t = process.UnknownPID
	monitorPrcessStartTimeStr string
	monitorInterval           uint8
)

func AddMonitorFlags(cmd *cobra.Command) {
	cmd.Flags().Int64VarP((*int64)(&monitorPid), "monitor", "m", int64(process.UnknownPID), "If present, tells DCP to monitor a given process ID (PID) and gracefully shutdown if the monitored process exits for any reason.")
	cmd.Flags().StringVar(&monitorPrcessStartTimeStr, "monitor-start-time", "", "If present, specifies the start time of the process to monitor. This is used to ensure the correct process is being monitored. The time format is RFC3339 with millisecond precision, for example "+osutil.RFC3339MiliTimestampFormat)
	cmd.Flags().Uint8VarP(&monitorInterval, "monitor-interval", "i", 0, "If present, specifies the time in seconds between checks for the monitor PID.")
}

// Returns a context that will be cancelled when the monitored process exits.
func MonitorPid(
	ctx context.Context,
	pid process.Pid_t,
	expectedProcessStartTime time.Time,
	pollInterval uint8,
	logger logr.Logger,
) (context.Context, error) {
	if pid == process.UnknownPID {
		return ctx, fmt.Errorf("no PID to monitor")
	}

	monitorCtx, monitorCtxCancel := context.WithCancel(ctx)

	monitorProc, monitorProcErr := process.FindWaitableProcess(pid, expectedProcessStartTime)
	if monitorProcErr != nil {
		logger.Error(monitorProcErr, "error finding process", "pid", pid)
		monitorCtxCancel()
		return monitorCtx, monitorProcErr
	}

	if pollInterval > 0 {
		monitorProc.WaitPollInterval = time.Second * time.Duration(pollInterval)
	}

	go func() {
		defer monitorCtxCancel()
		if waitErr := monitorProc.Wait(monitorCtx); waitErr != nil {
			if errors.Is(waitErr, context.Canceled) {
				logger.V(1).Info("monitoring cancelled by context", "pid", pid)
			} else {
				logger.Error(waitErr, "error waiting for process", "pid", pid)
			}
		} else {
			logger.Info("monitor process exited, shutting down", "pid", pid)
		}
	}()

	return monitorCtx, nil
}

func TryGetMonitorContext(ctx context.Context, logger logr.Logger) context.Context {
	processStartTime, startTimeErr := ParseProcessStartTime(monitorPrcessStartTimeStr)
	if startTimeErr != nil {
		logger.Error(startTimeErr, "error parsing monitor start time, process to monitor could not be verified")
		monitorCtx, monitorCtxCancel := context.WithCancel(ctx)
		monitorCtxCancel()
		return monitorCtx
	}

	// Ignore errors as they're logged by MonitorPid and we always return a valid context
	monitorCtx, _ := MonitorPid(ctx, monitorPid, processStartTime, monitorInterval, logger)
	return monitorCtx
}

func ParseProcessStartTime(timeStr string) (time.Time, error) {
	if timeStr == "" {
		// Not specified, return zero-time to indicate process start time should be disregarded
		return time.Time{}, nil
	}

	return time.Parse(osutil.RFC3339MiliTimestampFormat, timeStr)
}
