//go:build windows

package process

import (
	"errors"
	"fmt"
	"os"
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

	// Windows has no signals, and there is no universal way to "ask a process to stop",
	// so we just kill the process, but before that, if we are already waiting for the process to exit,
	// we need to update the "reason" field, to indicate that we attempted to kill the process,
	// so that we do not try to stop it again in case the context used for StartProcess() expires.
	e.acquireLock()
	ws, found := e.procsWaiting[pid]
	if found {
		ws.reason |= waitReasonStopping
	}
	e.releaseLock()

	err = proc.Kill()
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		return nil
	} else {
		return err
	}
}
