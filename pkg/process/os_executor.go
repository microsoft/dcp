package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/tklauser/ps"

	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type waitReason uint32

const (
	waitReasonNone       waitReason = 0x0
	waitReasonMonitoring waitReason = 0x1
	waitReasonStopping   waitReason = 0x2
)

type waitResult struct {
	waitErr error // The error returned by the wait function, if any
}

type waitState struct {
	waitable     Waitable        // The waitable that is being waited on
	waitEndedCh  chan struct{}   // A channel that gets closed when the wait ends
	waitResultCh chan waitResult // A channel that delivers the result of the wait
	waitEnded    time.Time       // The time when the wait function ended
	reason       waitReason      // The reason why are waiting on the process
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

func (e *OSExecutor) StartProcess(ctx context.Context, cmd *exec.Cmd, handler ProcessExitHandler) (Pid_t, time.Time, func(), error) {
	if err := cmd.Start(); err != nil {
		return UnknownPID, time.Time{}, nil, err
	}
	processStartTime := time.Now()

	osPid := cmd.Process.Pid
	pid, err := IntToPidT(osPid)
	if err != nil {
		return UnknownPID, time.Time{}, nil, err
	}

	psProcess, psProcessErr := ps.FindProcess(osPid)
	if psProcessErr != nil {
		e.log.Error(psProcessErr, "could not find process startup time", "PID", osPid)
	} else {
		// This is what the OS process startup timestamp is, so it is the most accurate value we can get.
		processStartTime = psProcess.CreationTime()
	}

	// Get the wait result channel, but do not actually start waiting
	// This also has the effect of tying the wait for this process to the command that started it.
	waitResultCh, _, _ := e.tryStartWaiting(pid, cmd, waitReasonNone)

	// Start the goroutine that waits for the context to expire.
	go func() {

		select {

		case wr := <-waitResultCh:
			// The process exited before the context expired.
			if handler != nil {
				exitCode, execError := getProcessExecResult(wr.waitErr, cmd)
				handler.OnProcessExited(pid, exitCode, errors.Join(ctx.Err(), execError))
			}

		case <-ctx.Done():
			_, _, shouldStopProcess := e.tryStartWaiting(pid, cmd, waitReasonStopping)
			var stopProcessErr error = nil

			if shouldStopProcess {
				// CONSIDER: having an option to specify whether to shut down the process when the context expires.
				stopProcessErr = e.stopProcessInternal(pid, processStartTime, optIsResponsibleForStopping)
				if stopProcessErr != nil {
					if handler != nil {
						// Let the caller know that the process did not stopp upon context expiration
						handler.OnProcessExited(pid, UnknownExitCode, errors.Join(stopProcessErr, ctx.Err()))
					}

					// There is no point waiting for the result if the process could not be stopped and we reported the error.
					break
				}
			}

			wr := <-waitResultCh

			if handler != nil {
				exitCode, execError := getProcessExecResult(wr.waitErr, cmd)
				handler.OnProcessExited(pid, exitCode, errors.Join(stopProcessErr, execError, ctx.Err()))
			}
		}
	}()

	startWaitingForProcessExit := func() {
		_, _, _ = e.tryStartWaiting(pid, cmd, waitReasonMonitoring)
	}

	return pid, processStartTime, startWaitingForProcessExit, nil
}

// Atomically starts waiting on the passed waitable if noting is already waiting in association with the process
// identified by PID. If the process is already being waited on, the reason is updated.
//
// Returns the channel that can be used to retrieve the wait result, the channel that signals the wait ended,
// and a boolean indicating whether the caller is the first one to indicate that the reason for the wait
// is "stopping the process", and thus IT is the caller that must stop the process.
func (e *OSExecutor) tryStartWaiting(pid Pid_t, waitable Waitable, reason waitReason) (<-chan waitResult, chan struct{}, bool) {
	doWait := func(ws *waitState) {
		err := ws.waitable.Wait()

		e.acquireLock()
		defer e.releaseLock()

		ws.waitEnded = time.Now()

		// There might be up to two different goroutines reading from the wait result channel:
		// the one that was started by StartProcess() and the one that was started by StopProcess().
		// We need to ensure that both of them are able get the result.
		ws.waitResultCh <- waitResult{waitErr: err}
		ws.waitResultCh <- waitResult{waitErr: err}
		close(ws.waitResultCh)
		close(ws.waitEndedCh)
	}

	e.acquireLock()
	defer e.releaseLock()

	ws, found := e.procsWaiting[pid]
	callerShouldStopProcess := false

	if found {
		if !ws.waitEnded.IsZero() {
			// The process has already exited, and we captured the wait result, there is no need to start waiting again,
			// or update anything.
			return ws.waitResultCh, ws.waitEndedCh, false
		}

		callerShouldStopProcess = (reason&waitReasonStopping) != 0 && (ws.reason&waitReasonStopping) == 0
		if ws.reason == waitReasonNone && reason != waitReasonNone {
			go doWait(ws)
		}
		ws.reason |= reason
	} else {
		callerShouldStopProcess = (reason & waitReasonStopping) != 0
		ws = &waitState{
			waitable:     waitable,
			waitResultCh: make(chan waitResult, 2),
			waitEndedCh:  make(chan struct{}),
			reason:       reason,
		}
		e.procsWaiting[pid] = ws
		if reason != waitReasonNone {
			go doWait(ws)
		}
	}

	return ws.waitResultCh, ws.waitEndedCh, callerShouldStopProcess
}

// Returns the process execution error and process exit code depending on the result of command wait call.
func getProcessExecResult(waitErr error, cmd *exec.Cmd) (int32, error) {
	var ee *exec.ExitError
	if waitErr == nil {
		return int32(cmd.ProcessState.ExitCode()), nil
	} else if errors.As(waitErr, &ee) {
		return int32(ee.ExitCode()), nil
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

func (e *OSExecutor) StopProcess(pid Pid_t, processStartTime time.Time) error {
	return e.stopProcessInternal(pid, processStartTime, optNone)
}

func (e *OSExecutor) stopProcessInternal(pid Pid_t, processStartTime time.Time, opts processStoppingOpts) error {
	tree, err := GetProcessTree(pid)
	if err != nil {
		return fmt.Errorf("could not get process tree for process %d: %w", pid, err)
	}

	e.log.V(1).Info("stopping process tree", "root", pid, "tree", tree)

	// If the root process cannot be stopped, don't bother with the rest of the tree.
	procEndedCh, stopErr := e.stopSingleProcess(pid, processStartTime, opts|optNotFoundIsError|optTrySignal|optWaitForStdio)
	if stopErr != nil {
		e.log.Error(stopErr, "could not stop root process", "root", pid)
		return stopErr
	}

	tree = tree[1:] // We have processed the root
	if len(tree) == 0 {
		return nil
	}

	childStoppingErrors := slices.MapConcurrent[Pid_t, error](tree, func(id Pid_t) error {
		// Retry stopping the child process as we occasionally see transient "Access Denied" errors.
		const childStopTimeout = 2 * time.Second
		return resiliency.RetryExponentialWithTimeout(context.Background(), childStopTimeout, func() error {
			_, childStopErr := e.stopSingleProcess(id, time.Time{}, opts)
			return childStopErr
		})
	}, slices.MaxConcurrency)
	childStoppingErrors = slices.Select(childStoppingErrors, func(e error) bool { return e != nil })
	if len(childStoppingErrors) > 0 {
		return fmt.Errorf("some children processes could not be stopped: %w", errors.Join(childStoppingErrors...))
	}

	<-procEndedCh

	return nil
}

type processStoppingOpts uint16

const (
	optNone            processStoppingOpts = 0
	optNotFoundIsError processStoppingOpts = 0x1
	optTrySignal       processStoppingOpts = 0x2
	optWaitForStdio    processStoppingOpts = 0x4

	// The caller is responsible for stopping the process, disregard "shouldStopProcess" value returned by tryStartWaiting().
	optIsResponsibleForStopping processStoppingOpts = 0x8
)

func makeClosedChan() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

var _ Executor = (*OSExecutor)(nil)
