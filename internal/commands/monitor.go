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
	"github.com/microsoft/dcp/internal/flags"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/spf13/cobra"
)

var (
	monitorPid              process.Pid_t = process.UnknownPID
	monitorProcessStartTime time.Time
	monitorInterval         uint8
)

func AddMonitorFlags(cmd *cobra.Command) {
	cmd.Flags().Int64VarP((*int64)(&monitorPid), "monitor", "m", int64(process.UnknownPID), "If present, tells DCP to monitor a given process ID (PID) and gracefully shutdown if the monitored process exits for any reason.")
	cmd.Flags().Var(flags.NewTimeFlag(&monitorProcessStartTime, osutil.RFC3339MiliTimestampFormat), "monitor-identity-time", "Specifies the identity time of the process to monitor. If omitted, identity is resolved once at command startup. The time format is RFC3339 with millisecond precision, for example "+osutil.RFC3339MiliTimestampFormat)
	cmd.Flags().Uint8VarP(&monitorInterval, "monitor-interval", "i", 0, "If present, specifies the time in seconds between checks for the monitor PID.")
}

// Starts monitoring a process identified by the given handle.
// Returns a context that will be cancelled when the monitored process exits, or if the returned cancellation function is called.
// The cancellation cause preserves parent or monitoring errors; a normal monitored-process exit uses context.Canceled.
// The returned context (and the cancellation function) is valid even if an error occurs (e.g. the process cannot be found),
// but it will be already cancelled in that case.
func MonitorPid(
	ctx context.Context,
	handle process.ProcessHandle,
	pollInterval uint8,
	logger logr.Logger,
) (context.Context, context.CancelFunc, error) {
	monitorCtx, monitorCtxCancelCause := context.WithCancelCause(ctx)
	monitorCtxCancel := func() {
		monitorCtxCancelCause(context.Canceled)
	}
	resolved, resolveErr := process.ResolveProcessHandle(handle.Pid, handle.IdentityTime)
	if resolveErr != nil {
		logger.Info("Could not resolve monitored process", "PID", handle.Pid, "Error", resolveErr)
		monitorCtxCancelCause(resolveErr)
		return monitorCtx, monitorCtxCancel, resolveErr
	}
	handle = resolved

	monitorProc, monitorProcErr := process.FindWaitableProcess(handle)
	if monitorProcErr != nil {
		logger.Info("Error finding process", "PID", handle.Pid, "Error", monitorProcErr)
		monitorCtxCancelCause(monitorProcErr)
		return monitorCtx, monitorCtxCancel, monitorProcErr
	}

	if pollInterval > 0 {
		monitorProc.WaitPollInterval = time.Second * time.Duration(pollInterval)
	}

	logger.Info("Started monitoring process", "PID", handle.Pid)

	go func() {
		waitErr := monitorProc.Wait(monitorCtx)
		monitorCtxCancelCause(waitErr)
		if waitErr != nil {
			if errors.Is(waitErr, context.Canceled) {
				logger.V(1).Info("Monitoring cancelled by context", "PID", handle.Pid)
			} else {
				logger.Error(waitErr, "Error waiting for process", "PID", handle.Pid)
			}
		} else {
			logger.Info("Monitor process exited, shutting down", "PID", handle.Pid)
		}
	}()

	return monitorCtx, monitorCtxCancel, nil
}

// Starts monitoring the process specified by the monitor flags (--monitor, --monitor-identity-time, and --monitor-interval).
// Returns the same results as MonitorPid. An absent or unknown (-1) --monitor value is reported as an invalid process handle.
func MonitorPidFromFlags(ctx context.Context, logger logr.Logger) (context.Context, context.CancelFunc, error) {
	return MonitorPid(ctx, process.NewHandle(monitorPid, monitorProcessStartTime), monitorInterval, logger)
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
	monitorCtx, monitorCtxCancel, _ := MonitorPidFromFlags(ctx, logger)
	return monitorCtx, monitorCtxCancel
}
