//go:build windows

package process

import (
	"errors"
	"fmt"
	"os"
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

	// Windows has no signals, and there is no universal way to "ask a process to stop",
	// so we just kill the process, but before that, we need to check if we are not already waiting for the process.
	var waitFunc WaitFunc = func() error {
		_, waitErr := proc.Wait()
		return waitErr
	}

	_, waitEndedCh, shouldStopProcess := e.tryStartWaiting(pid, waitFunc, waitReasonStopping)

	if opts&optWaitForStdio == 0 {
		waitEndedCh = makeClosedChan()
	}

	if shouldStopProcess || (opts&optIsResponsibleForStopping) != 0 {
		e.log.V(1).Info("sending SIGKILL to process", "pid", pid)
		err = proc.Kill()
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			return nil, err
		}
	}

	e.log.V(1).Info("waiting for process to stop", "pid", pid)

	return waitEndedCh, nil
}
