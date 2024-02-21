package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type waitReason uint32

const (
	waitReasonMonitoring waitReason = 0x1
	waitReasonStopping   waitReason = 0x2
)

type waitState struct {
	waitErr     error         // The error returned by the wait function
	waitEndedCh chan struct{} // A channel that will be closed when the wait function ends
	waitEnded   time.Time     // The time when the wait function ended
	reason      waitReason    // The reason why are waiting on the process
}

type Waitable interface {
	Wait() error
}
type WaitFunc func() error

func (f WaitFunc) Wait() error {
	return f()
}

type OSExecutor struct {
	procsWaiting map[Pid_t]*waitState
	lock         sync.Locker
	log          logr.Logger
}

func NewOSExecutor(log logr.Logger) Executor {
	return &OSExecutor{
		procsWaiting: make(map[Pid_t]*waitState),
		lock:         &sync.Mutex{},
		log:          log.WithName("os-executor"),
	}
}

func (e *OSExecutor) StartProcess(ctx context.Context, cmd *exec.Cmd, handler ProcessExitHandler) (Pid_t, func(), error) {
	if err := cmd.Start(); err != nil {
		return UnknownPID, nil, err
	}

	// The caller might not call startWaitForProcessExit(), but if they do, we need to get a notification
	// when the process exits, so we can
	var processStopCh chan *waitState
	if handler != nil {
		processStopCh = make(chan *waitState, 1)
	}

	osPid := cmd.Process.Pid
	pid, err := IntToPidT(osPid)
	if err != nil {
		return UnknownPID, nil, err
	}

	go func() {
		select {

		case ws := <-processStopCh:
			// Do not report anything if the context expired, or nobody is listening
			if ctx.Err() != nil || handler == nil {
				return
			} else {
				exitCode, execError := getProcessExecResult(ws.waitErr, cmd)
				handler.OnProcessExited(pid, exitCode, execError)
			}

		case <-ctx.Done():
			e.acquireLock()
			needToStopProcess := false
			ws, found := e.procsWaiting[pid]
			if !found || (ws.reason&waitReasonStopping) == 0 {
				// The context associated with the process start has expired, but we haven't attempted to stop the process yet.
				needToStopProcess = true
			}
			e.releaseLock()

			var stopProcessErr error = nil
			if needToStopProcess {
				// CONSIDER: having an option to specify whether to shut down the process when the context expires.
				stopProcessErr = e.StopProcess(pid)
			}

			if handler != nil {
				exitCode, execError := getProcessExecResult(stopProcessErr, cmd)
				handler.OnProcessExited(pid, exitCode, errors.Join(ctx.Err(), execError))
			}
		}
	}()

	startWaitingForProcessExit := func() {
		ws := e.tryStartWaiting(pid, cmd, waitReasonMonitoring)

		if handler != nil {
			go func() {
				<-ws.waitEndedCh
				processStopCh <- ws
				close(processStopCh)
			}()
		}
	}

	return pid, startWaitingForProcessExit, nil
}

func (e *OSExecutor) tryStartWaiting(pid Pid_t, waitable Waitable, reason waitReason) *waitState {
	e.acquireLock()
	defer e.releaseLock()

	ws, found := e.procsWaiting[pid]
	if found {
		// We are already waiting, just update the reason
		ws.reason |= reason
	} else {
		ws = &waitState{
			waitEndedCh: make(chan struct{}),
			reason:      reason,
		}
		e.procsWaiting[pid] = ws

		go func() {
			err := waitable.Wait()

			e.acquireLock()
			defer e.releaseLock()
			endedWaitState, endedWaitStateFound := e.procsWaiting[pid]
			if !endedWaitStateFound {
				panic(fmt.Sprintf("process with pid %d was not found in the waiting list", pid))
			}
			endedWaitState.waitErr = err
			endedWaitState.waitEnded = time.Now()
			close(endedWaitState.waitEndedCh)
		}()
	}

	return ws
}

// Returns the process execution error and process exit code depending on the result of process wait call.
func getProcessExecResult(waitErr error, cmd *exec.Cmd) (int32, error) {
	var ee *exec.ExitError
	if waitErr == nil || errors.As(waitErr, &ee) {
		return int32(cmd.ProcessState.ExitCode()), nil
	} else {
		return UnknownExitCode, waitErr
	}
}

func (e *OSExecutor) acquireLock() {
	const maxCompletedDuration = 1 * time.Minute

	e.lock.Lock()

	// Only keep wait states that correspond to processes that are still running, or the ones that completed recently
	e.procsWaiting = maps.Select(e.procsWaiting, func(_ Pid_t, ws *waitState) bool {
		return ws.waitEnded.IsZero() || time.Since(ws.waitEnded) < maxCompletedDuration
	})
}

func (e *OSExecutor) releaseLock() {
	e.lock.Unlock()
}

func (e *OSExecutor) StopProcess(pid Pid_t) error {
	tree, err := GetProcessTree(pid)
	if err != nil {
		return fmt.Errorf("could not get process tree for process %d: %w", pid, err)
	}

	e.acquireLock()

	// If this is a process with a wait state, ensure we don't try to stop it twice
	ws, found := e.procsWaiting[pid]
	if found {
		// We are already waiting, just update the reason
		ws.reason |= waitReasonStopping
	}

	e.releaseLock()

	e.log.V(1).Info("stopping process tree", "root", pid, "tree", tree)

	// If the root process cannot be stopped, don't bother with the rest of the tree.
	err = e.stopSingleProcess(pid, optNotFoundIsError|optTrySignal)
	if err != nil {
		return err
	}

	tree = tree[1:] // We have processed the root
	if len(tree) == 0 {
		return nil
	}

	childStoppingErrors := slices.MapConcurrent[Pid_t, error](tree, func(id Pid_t) error {
		return e.stopSingleProcess(id, optNone)
	}, slices.MaxConcurrency)
	childStoppingErrors = slices.Select(childStoppingErrors, func(e error) bool { return e != nil })
	if len(childStoppingErrors) > 0 {
		return fmt.Errorf("some children processes could not be stopped: %w", errors.Join(childStoppingErrors...))
	}

	return nil
}

type processStoppingOpts uint16

const (
	optNone            processStoppingOpts = 0
	optNotFoundIsError processStoppingOpts = 0x1
	optTrySignal       processStoppingOpts = 0x2
)

var _ Executor = (*OSExecutor)(nil)
