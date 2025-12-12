// Copyright (c) Microsoft Corporation. All rights reserved.

package commands

import (
	"context"
	"errors"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/internal/flags"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/spf13/cobra"
)

var (
	monitorPid              process.Pid_t = process.UnknownPID
	monitorProcessStartTime time.Time
	monitorInterval         uint8
)

func AddMonitorFlags(cmd *cobra.Command) {
	cmd.Flags().Int64VarP((*int64)(&monitorPid), "monitor", "m", int64(process.UnknownPID), "If present, tells DCP to monitor a given process ID (PID) and gracefully shutdown if the monitored process exits for any reason.")
	cmd.Flags().Var(flags.NewTimeFlag(&monitorProcessStartTime, osutil.RFC3339MiliTimestampFormat), "monitor-identity-time", "If present, specifies the identity time of the process to monitor. This is used to ensure the correct process is being monitored. The time format is RFC3339 with millisecond precision, for example "+osutil.RFC3339MiliTimestampFormat)
	cmd.Flags().Uint8VarP(&monitorInterval, "monitor-interval", "i", 0, "If present, specifies the time in seconds between checks for the monitor PID.")
}

// Starts monitoring a process with a given PID and (optional) start time.
// Returns a context that will be cancelled when the monitored process exits, or if the returned cancellation function is called.
// The returned context (and the cancellation function) is valid even if an error occurs (e.g. the process cannot be found),
// but it will be already cancelled in that case.
func MonitorPid(
	ctx context.Context,
	pid process.Pid_t,
	expectedProcessStartTime time.Time,
	pollInterval uint8,
	logger logr.Logger,
) (context.Context, context.CancelFunc, error) {
	monitorCtx, monitorCtxCancel := context.WithCancel(ctx)

	monitorProc, monitorProcErr := process.FindWaitableProcess(pid, expectedProcessStartTime)
	if monitorProcErr != nil {
		logger.Info("Error finding process", "PID", pid)
		monitorCtxCancel()
		return monitorCtx, monitorCtxCancel, monitorProcErr
	}

	if pollInterval > 0 {
		monitorProc.WaitPollInterval = time.Second * time.Duration(pollInterval)
	}

	go func() {
		defer monitorCtxCancel()
		if waitErr := monitorProc.Wait(monitorCtx); waitErr != nil {
			if errors.Is(waitErr, context.Canceled) {
				logger.V(1).Info("Monitoring cancelled by context", "PID", pid)
			} else {
				logger.Error(waitErr, "Error waiting for process", "PID", pid)
			}
		} else {
			logger.Info("Monitor process exited, shutting down", "PID", pid)
		}
	}()

	return monitorCtx, monitorCtxCancel, nil
}

// Returns a context (derived from the passed parent context) that will be cancelled when
// the process specified by the monitor flags (--monitor and --monitor-identity-time) exits .
// If the flags are not present at startup, the returned context is just a regular cancellable context
// with no additional semantics.
// If an error occurs (e.g. the process cannot be found, or the monitor flag values are invalid),
// the context is still returned, but it will be cancelled immediately.
func GetMonitorContextFromFlags(ctx context.Context, logger logr.Logger) (context.Context, context.CancelFunc) {
	if monitorPid == process.UnknownPID {
		// Suppress linter complaining about cancellation function not being used
		// nolint:govet
		return context.WithCancel(ctx) // No process to monitor
	}

	// Ignore errors as they're logged by MonitorPid and we always return a valid context
	monitorCtx, monitorCtxCancel, _ := MonitorPid(ctx, monitorPid, monitorProcessStartTime, monitorInterval, logger)
	return monitorCtx, monitorCtxCancel
}
