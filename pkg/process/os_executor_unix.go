//go:build !windows

package process

import (
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
)

type OSExecutor struct {
	procsWaiting map[WaitKey]*waitState
	disposed     bool
	lock         sync.Locker
	log          logr.Logger
}

func NewOSExecutor(log logr.Logger) Executor {
	return &OSExecutor{
		procsWaiting: make(map[WaitKey]*waitState),
		disposed:     false,
		lock:         &sync.Mutex{},
		log:          log.WithName("os-executor"),
	}
}

func (e *OSExecutor) stopSingleProcess(pid Pid_t, processStartTime time.Time, opts processStoppingOpts) (<-chan struct{}, error) {
	proc, err := FindProcess(pid, processStartTime)
	if err != nil {
		e.acquireLock()
		alreadyEnded := false
		ws, found := e.procsWaiting[WaitKey{pid, processStartTime}]
		if found {
			alreadyEnded = !ws.waitEnded.IsZero()
		}
		e.releaseLock()

		if (opts&optNotFoundIsError) != 0 && !alreadyEnded {
			return nil, ErrProcessNotFound{Pid: pid, Inner: err}
		} else {
			return makeClosedChan(), nil
		}
	}

	waitable := makeWaitable(pid, proc)
	ws, shouldStopProcess := e.tryStartWaiting(pid, processStartTime, waitable, waitReasonStopping)

	waitEndedCh := ws.waitEndedCh
	if opts&optWaitForStdio == 0 {
		waitEndedCh = makeClosedChan()
	}

	if !shouldStopProcess && (opts&optIsResponsibleForStopping) == 0 {
		return waitEndedCh, nil
	}

	if (opts & optTrySignal) == optTrySignal {
		// Give the process a chance to gracefully exit.
		// There is no established standard for what signals are used for graceful shutdown,
		// but SIGTERM and SIGQUIT are commonly used.
		err = e.signalAndWaitForExit(proc, syscall.SIGTERM, ws)
		switch {
		case err == nil:
			e.log.V(1).Info("process stopped by SIGTERM", "pid", pid)
			return waitEndedCh, nil
		case !errors.Is(err, ErrTimedOutWaitingForProcessToStop):
			return nil, err
		default:
			e.log.V(1).Info("process did not stop upon SIGTERM", "pid", pid)
		}
	}

	e.log.V(1).Info("sending SIGKILL to process...", "pid", pid)
	err = e.signalAndWaitForExit(proc, syscall.SIGKILL, ws)
	if err != nil {
		return nil, err
	}

	e.log.V(1).Info("process stopped by SIGKILL", "pid", pid)
	return waitEndedCh, nil
}

// Sends a given signal to a process and waits for it to exit.
// If the process does not exit within 6 seconds, the function returns context.DeadlineExceeded.
func (e *OSExecutor) signalAndWaitForExit(proc *os.Process, sig syscall.Signal, ws *waitState) error {
	err := proc.Signal(sig)
	switch {
	case errors.Is(err, os.ErrProcessDone):
		return nil
	case err != nil:
		return fmt.Errorf("could not send signal %s to process %d: %w", sig.String(), proc.Pid, err)
	}

	select {

	case <-ws.waitEndedCh:
		err = ws.waitErr
		if err == nil || IsEarlyProcessExitError(err) {
			return nil
		}

		return fmt.Errorf("could not wait for process %d to exit: %w", proc.Pid, err)

	case <-time.After(signalAndWaitTimeout):
		return ErrTimedOutWaitingForProcessToStop
	}
}

func (e *OSExecutor) completeDispose() {
	// No additional cleanup needed for Unix-like systems.
}

func (e *OSExecutor) prepareProcessStart(_ *exec.Cmd, _ ProcessCreationFlag) {
	// No additional preparation needed for Unix-like systems.
}

func (e *OSExecutor) completeProcessStart(_ *exec.Cmd, pid Pid_t, _ time.Time, _ ProcessCreationFlag) error {
	// No additional actions needed on process start for Unix-like systems.
	return nil
}
