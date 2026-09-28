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
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
)

const (
	// The timeout for sending a signal and waiting for the process to exit.
	signalAndWaitTimeout = 6 * time.Second

	// Stop confirmation should be responsive; long-lived monitoring retains defaultWaitPollInterval.
	stopWaitPollInterval = 100 * time.Millisecond
)

type OSExecutor struct {
	procsWaiting           map[ProcessHandle]*waitState
	disposed               bool
	lock                   sync.Locker
	log                    logr.Logger
	lifetimeCtx            context.Context
	lifetimeCtxCancel      context.CancelFunc
	startLifetimeCtx       context.Context
	startLifetimeCtxCancel context.CancelCauseFunc
	startsInFlight         sync.WaitGroup
}

func NewOSExecutor(log logr.Logger) Executor {
	lifetimeCtx, lifetimeCtxCancel := context.WithCancel(context.Background())
	startLifetimeCtx, startLifetimeCtxCancel := context.WithCancelCause(context.Background())
	return &OSExecutor{
		procsWaiting:           make(map[ProcessHandle]*waitState),
		disposed:               false,
		lock:                   &sync.Mutex{},
		log:                    log.WithName("os-executor"),
		lifetimeCtx:            lifetimeCtx,
		lifetimeCtxCancel:      lifetimeCtxCancel,
		startLifetimeCtx:       startLifetimeCtx,
		startLifetimeCtxCancel: startLifetimeCtxCancel,
	}
}

func (e *OSExecutor) stopSingleProcess(ctx context.Context, handle ProcessHandle, opts processStoppingOpts) (singleProcessStopResult, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return singleProcessStopResult{}, contextErr
	}
	// Console group signaling is Windows-specific; keep the shared option as an explicit no-op on Unix.
	opts &^= optSignalConsoleGroup

	proc, err := FindProcess(handle)
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
	waitable := makeProcessWaitable(e.lifetimeCtx, handle, stopWaitPollInterval)
	ws, shouldStopProcess := e.tryStartWaiting(handle, waitable, waitReasonStopping)

	waitEndedCh := ws.waitEndedCh
	if opts&optWaitForStdio == 0 {
		waitEndedCh = makeClosedChan()
	}

	if !shouldStopProcess && (opts&optIsResponsibleForStopping) == 0 {
		return singleProcessStopResult{waitEndedCh: waitEndedCh}, nil
	}
	defer e.finishStopAttempt(ws)

	if (opts & optTrySignal) == optTrySignal {
		// Give the process a chance to gracefully exit.
		// There is no established standard for what signals are used for graceful shutdown,
		// but SIGTERM and SIGQUIT are commonly used.
		waitTimeout := signalAndWaitTimeout
		if (opts & optGracefulOnly) != 0 {
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

	var timeoutCh <-chan time.Time
	var timer *time.Timer
	if waitTimeout > 0 {
		timer = time.NewTimer(waitTimeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case <-ctx.Done():
		return ctx.Err()

	case <-ws.waitEndedCh:
		err = ws.waitErr
		if err == nil || IsEarlyProcessExitError(err) {
			return nil
		}

		return fmt.Errorf("could not wait for process %d to exit: %w", proc.Pid, err)

	case <-timeoutCh:
		return ErrTimedOutWaitingForProcessToStop
	}
}

func (e *OSExecutor) completeDispose() {
	// No additional cleanup needed for Unix-like systems.
}

func (e *OSExecutor) prepareProcessStart(_ *exec.Cmd, _ ProcessCreationFlag) {
	// No additional preparation needed for Unix-like systems.
}

func (e *OSExecutor) completeProcessStart(_ ProcessHandle, _ ProcessCreationFlag) error {
	// No additional actions needed on process start for Unix-like systems.
	return nil
}
