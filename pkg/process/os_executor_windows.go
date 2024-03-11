//go:build windows

package process

import (
	"errors"
	"fmt"
	"os"
)

func (e *OSExecutor) stopSingleProcess(pid Pid_t, opts processStoppingOpts) error {
	osPid, err := PidT_ToInt(pid)
	if err != nil {
		return err
	}

	proc, err := os.FindProcess(osPid)
	if err != nil {
		if (opts & optNotFoundIsError) != 0 {
			return fmt.Errorf("could not find process %d: %w", pid, err)
		} else {
			return nil
		}
	}

	// Windows has no signals, and there is no universal way to "ask a process to stop",
	// so we just kill the process, but before that, we need to check if we are not already waiting for the process.
	var waitFunc WaitFunc = func() error {
		_, waitErr := proc.Wait()
		return waitErr
	}

	_, waitEnded, shouldStopProcess := e.tryStartWaiting(pid, waitFunc, waitReasonStopping)
	if shouldStopProcess || (opts&optIsResponsibleForStopping) != 0 {
		err = proc.Kill()
		if err == nil || errors.Is(err, os.ErrProcessDone) {
			return nil
		} else {
			return err
		}
	} else {
		// We already initiated the stop for this process, just wait for that to complete
		<-waitEnded
		return nil
	}
}
