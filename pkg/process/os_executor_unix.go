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

func (e *OSExecutor) stopSingleProcess(pid int32, opts processStoppingOpts) error {
	proc, err := os.FindProcess(int(pid))
	if err != nil {
		if (opts & optNotFoundIsError) != 0 {
			return fmt.Errorf("could not find process %d: %w", pid, err)
		} else {
			return nil
		}
	}

	// Give the process a chance to gracefully exit.
	// There is no established standard for what signals are used for graceful shutdown,
	// but SIGTERM and SIGQUIT are commonly used.
	err = e.signalAndWaitForExit(proc, syscall.SIGTERM, opts)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, context.DeadlineExceeded):
		return err
	}

	// SIGTERM did not work, try SIGQUIT.
	err = e.signalAndWaitForExit(proc, syscall.SIGQUIT, opts)
	if err == nil {
		return nil
	}

	err = proc.Kill()
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		return nil
	} else {
		return err
	}
}

const signalAndWaitTimeout = 10 * time.Second

// Sends a given signal to a process and waits for it to exit.
// If the process does not exit within 10 seconds, the function returns context.DeadlineExceeded.
func (e *OSExecutor) signalAndWaitForExit(proc *os.Process, sig syscall.Signal, opts processStoppingOpts) error {
	err := proc.Signal(sig)
	switch {
	case errors.Is(err, os.ErrProcessDone):
		return nil
	case err != nil:
		return fmt.Errorf("could not send signal %s to process %d: %w", sig.String(), proc.Pid, err)
	}

	timeoutCtx, cancelTimeout := context.WithTimeout(context.Background(), signalAndWaitTimeout)
	defer cancelTimeout()

	var waitFunc WaitFunc = func() error {
		_, err := proc.Wait()
		return err
	}
	ws := e.tryStartWaiting(int32(proc.Pid), waitFunc, waitReasonStopping)
	select {
	case <-ws.waitEndedCh:
		err = ws.waitErr
		var ee *exec.ExitError
		if err == nil || errors.Is(err, os.ErrProcessDone) || errors.As(err, &ee) {
			// These are all expected errors, the process exited successfully.
			return nil
		}

		// Receiving ECHILD when calling wait() on the child process is expected,
		// (the parent process might have terminated them).
		var sysErr *os.SyscallError
		isEChildErr := err != nil && errors.As(err, &sysErr) && strings.Index(sysErr.Syscall, "wait") == 0 && errors.Is(sysErr.Err, syscall.ECHILD)
		if isEChildErr && (opts&optNotFoundIsError) == 0 {
			return nil
		}

		return fmt.Errorf("could not wait for process %d to exit: %w", proc.Pid, err)
	case <-timeoutCtx.Done():
		return context.DeadlineExceeded
	}
}
