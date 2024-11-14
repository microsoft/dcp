//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func (e *OSExecutor) stopSingleProcess(pid Pid_t, processStartTime time.Time, opts processStoppingOpts) (<-chan struct{}, error) {
	proc, err := FindProcess(pid, processStartTime)
	if err != nil {
		if (opts & optNotFoundIsError) != 0 {
			return nil, fmt.Errorf("could not find process %d: %w", pid, err)
		} else {
			return makeClosedChan(), nil
		}
	}

	var waitFunc WaitFunc = func() error {
		_, waitErr := proc.Wait()
		return waitErr
	}

	waitResultCh, waitEndedCh, shouldStopProcess := e.tryStartWaiting(pid, waitFunc, waitReasonStopping)

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
		err = e.signalAndWaitForExit(proc, syscall.SIGTERM, waitResultCh)
		switch {
		case err == nil:
			e.log.V(1).Info("process stopped by SIGTERM", "pid", pid)
			return waitEndedCh, nil
		case !errors.Is(err, context.DeadlineExceeded):
			return nil, err
		}
	}

	err = e.signalAndWaitForExit(proc, syscall.SIGKILL, waitResultCh)
	if err != nil {
		return nil, err
	}

	e.log.V(1).Info("process stopped by SIGKILL", "pid", pid)
	return waitEndedCh, nil
}

const signalAndWaitTimeout = 10 * time.Second

// Sends a given signal to a process and waits for it to exit.
// If the process does not exit within 10 seconds, the function returns context.DeadlineExceeded.
func (e *OSExecutor) signalAndWaitForExit(proc *os.Process, sig syscall.Signal, waitResultCh <-chan waitResult) error {
	err := proc.Signal(sig)
	switch {
	case errors.Is(err, os.ErrProcessDone):
		return nil
	case err != nil:
		return fmt.Errorf("could not send signal %s to process %d: %w", sig.String(), proc.Pid, err)
	}

	timeoutCtx, cancelTimeout := context.WithTimeout(context.Background(), signalAndWaitTimeout)
	defer cancelTimeout()

	select {

	case wr := <-waitResultCh:
		err = wr.waitErr
		var ee *exec.ExitError
		if err == nil || errors.Is(err, os.ErrProcessDone) || errors.As(err, &ee) {
			// These are all expected errors, the process exited successfully.
			return nil
		}

		// Receiving ECHILD when calling wait() on the child process is expected,
		// (the parent process might have terminated them).
		var sysErr *os.SyscallError
		isEChildErr := errors.As(err, &sysErr) && strings.Index(sysErr.Syscall, "wait") == 0 && errors.Is(sysErr.Err, syscall.ECHILD)
		if isEChildErr {
			return nil
		}

		return fmt.Errorf("could not wait for process %d to exit: %w", proc.Pid, err)

	case <-timeoutCtx.Done():
		return context.DeadlineExceeded
	}
}
