//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/go-logr/logr"
)

const (
	// The timeout for sending a signal and waiting for the process to exit.
	signalAndWaitTimeout = 6 * time.Second

	// Stop confirmation should be responsive; polling falls back to the
	// long-lived monitoring interval after the bounded stop window.
	stopWaitPollInterval = 100 * time.Millisecond
)

type OSExecutor struct {
	*osExecutorBase
}

func NewOSExecutor(log logr.Logger) Executor {
	return &OSExecutor{
		osExecutorBase: newOSExecutorBase(log),
	}
}

func (e *OSExecutor) forceKillWasUsed(ws *waitState) bool {
	e.acquireLock()
	defer e.releaseLock()
	return ws.forceKillUsed
}

func (e *OSExecutor) stopSingleProcess(ctx context.Context, handle ProcessHandle, opts processStoppingOpts) (singleProcessStopResult, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return singleProcessStopResult{}, contextErr
	}
	// Console group signaling is Windows-specific; keep the shared option as an explicit no-op on Unix.
	opts &^= optSignalConsoleGroup

	proc, err := handle.OsProcess()
	if err != nil {
		if !IsProcessGoneErr(err) {
			return singleProcessStopResult{}, err
		}
		e.acquireLock()
		alreadyEnded := false
		ws, found := e.procsWaiting[handle]
		if found {
			alreadyEnded = !ws.waitEnded.IsZero()
		}
		e.releaseLock()

		if (opts&optNotFoundIsError) != 0 && !alreadyEnded {
			return singleProcessStopResult{}, &ErrProcessNotFound{Pid: handle.Pid, Inner: err}
		} else {
			return singleProcessStopResult{waitEndedCh: makeClosedChan()}, nil
		}
	}

	defer func() {
		if releaseErr := proc.Release(); releaseErr != nil {
			e.log.Error(releaseErr, "Could not release process reference", "PID", handle.Pid)
		}
	}()
	waitable := makeProcessWaitable(e.lifetimeCtx, handle, waitPollPolicy{
		initialInterval: stopWaitPollInterval,
		initialDuration: processStopTimeout,
		steadyInterval:  defaultWaitPollInterval,
	})
	ws, shouldStopProcess := e.tryStartWaiting(handle, waitable, waitReasonStopping)

	waitEndedCh := ws.waitEndedCh
	if opts&optWaitForStdio == 0 {
		waitEndedCh = makeClosedChan()
	}

	if !shouldStopProcess && (opts&optIsResponsibleForStopping) == 0 {
		waitErr := waitForTrackedProcessExit(ctx, handle.Pid, ws, 0)
		return singleProcessStopResult{
			waitEndedCh:   waitEndedCh,
			forceKillUsed: e.forceKillWasUsed(ws),
		}, waitErr
	}
	defer e.finishStopAttempt(ws)

	if (opts & optTrySignal) == optTrySignal {
		// Give the process a chance to gracefully exit.
		// There is no established standard for what signals are used for graceful shutdown,
		// but SIGTERM and SIGQUIT are commonly used.
		waitTimeout := signalAndWaitTimeout
		if (opts & (optGracefulOnly | optWaitForGracefulDeadline)) != 0 {
			waitTimeout = 0
		}
		err = e.signalAndWaitForExit(ctx, handle, proc, syscall.SIGTERM, ws, waitTimeout)
		switch {
		case err == nil:
			e.log.V(1).Info("Process stopped by SIGTERM", "PID", handle.Pid)
			return singleProcessStopResult{waitEndedCh: waitEndedCh}, nil
		case IsProcessGoneErr(e.CheckProcessRunning(handle)):
			e.log.V(1).Info("Process exited after SIGTERM while its wait operation was still completing", "PID", handle.Pid)
			return singleProcessStopResult{waitEndedCh: waitEndedCh}, nil
		case (opts & optGracefulOnly) != 0:
			return singleProcessStopResult{waitEndedCh: waitEndedCh}, err
		case !errors.Is(err, ErrTimedOutWaitingForProcessToStop):
			return singleProcessStopResult{}, err
		default:
			e.log.V(1).Info("Process did not stop upon SIGTERM", "PID", handle.Pid)
		}
	}

	e.log.V(1).Info("Sending SIGKILL to process...", "PID", handle.Pid)
	e.markForceKillUsed(ws)
	err = e.signalAndWaitForExit(ctx, handle, proc, syscall.SIGKILL, ws, signalAndWaitTimeout)
	if err != nil {
		return singleProcessStopResult{waitEndedCh: waitEndedCh, forceKillUsed: true}, err
	}

	e.log.V(1).Info("Process stopped by SIGKILL", "PID", handle.Pid)
	return singleProcessStopResult{waitEndedCh: waitEndedCh, forceKillUsed: true}, nil
}

// Sends a given signal to a process and waits for it to exit.
// If waitTimeout is positive and the process does not exit within that duration, the function
// returns ErrTimedOutWaitingForProcessToStop. A zero timeout waits until the context expires.
func (e *OSExecutor) signalAndWaitForExit(
	ctx context.Context,
	handle ProcessHandle,
	proc *os.Process,
	sig syscall.Signal,
	ws *waitState,
	waitTimeout time.Duration,
) error {
	err := signalProcess(ctx, handle, proc, sig)
	switch {
	case IsProcessGoneErr(err):
		return nil
	case err != nil:
		return fmt.Errorf("could not send signal %s to process %d: %w", sig.String(), proc.Pid, err)
	}

	if sig == syscall.SIGKILL {
		// cmd.Wait can remain blocked by descendant-held pipes after the target process exits.
		return e.waitForTrackedProcessExitOrGone(ctx, handle, ws, waitTimeout)
	}
	return waitForTrackedProcessExit(ctx, handle.Pid, ws, waitTimeout)
}

func (e *OSExecutor) waitForTrackedProcessExitOrGone(
	ctx context.Context,
	handle ProcessHandle,
	ws *waitState,
	waitTimeout time.Duration,
) error {
	var timeoutCh <-chan time.Time
	var timeoutTimer *time.Timer
	if waitTimeout > 0 {
		timeoutTimer = time.NewTimer(waitTimeout)
		defer timeoutTimer.Stop()
		timeoutCh = timeoutTimer.C
	}

	pollTimer := time.NewTimer(0)
	defer pollTimer.Stop()
	var inspectionErr error
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ws.waitEndedCh:
			return trackedProcessWaitResult(handle.Pid, ws)
		case <-pollTimer.C:
			runningErr := e.CheckProcessRunning(handle)
			if IsProcessGoneErr(runningErr) {
				return nil
			}
			if runningErr != nil {
				inspectionErr = fmt.Errorf("could not confirm process %d exit: %w", handle.Pid, runningErr)
			} else {
				inspectionErr = nil
			}
			pollTimer.Reset(stopWaitPollInterval)
		case <-timeoutCh:
			if inspectionErr != nil {
				return errors.Join(ErrTimedOutWaitingForProcessToStop, inspectionErr)
			}
			return ErrTimedOutWaitingForProcessToStop
		}
	}
}

func (e *OSExecutor) completeDispose() {
	// No additional cleanup needed for Unix-like systems.
}

func (e *OSExecutor) prepareProcessStart(_ *exec.Cmd, _ ProcessCreationFlag) {
	// No additional preparation needed for Unix-like systems.
}

func windowsConsoleAvailabilityForCmd(_ *exec.Cmd) WindowsConsoleAvailability {
	return WindowsConsoleAvailabilityUnknown
}

func (e *OSExecutor) completeProcessStart(_ ProcessHandle, _ ProcessCreationFlag) error {
	// No additional actions needed on process start for Unix-like systems.
	return nil
}
