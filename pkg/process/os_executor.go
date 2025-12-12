// Copyright (c) Microsoft Corporation. All rights reserved.

package process

import (
	"context"
	"errors"
	"fmt"
	stdlib_maps "maps"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type waitReason uint32

const (
	waitReasonNone       waitReason = 0x0
	waitReasonMonitoring waitReason = 0x1
	waitReasonStopping   waitReason = 0x2

	// Timeout for getting a confirmation that the child process has exited (return of a wait() call).
	// Must be greater that 2 * signalAndWaitTimeout, because in worst case we might send up to two signals
	// and then time out checking if the process exited.
	waitForProcessExitTimeout = 15 * time.Second
)

var (
	ErrDisposed = errors.New("the process executor has been disposed")
)

type waitState struct {
	waitable    Waitable      // The waitable that is being waited on
	waitEndedCh chan struct{} // A channel that gets closed when the wait ends
	waitErr     error         // The result of the process wait. Not valid until waitEndedCh is closed.
	waitEnded   time.Time     // The time when the wait function ended. Zero if the wait is still in progress.
	reason      waitReason    // The reason why are waiting on the process
}

type WaitKey struct {
	Pid       Pid_t
	StartedAt time.Time
}

func (e *OSExecutor) StartProcess(
	ctx context.Context,
	cmd *exec.Cmd,
	handler ProcessExitHandler,
	flags ProcessCreationFlag,
) (Pid_t, time.Time, func(), error) {
	e.acquireLock()
	if e.disposed {
		e.releaseLock()
		return UnknownPID, time.Time{}, nil, ErrDisposed
	}
	e.releaseLock()

	pid, processIdentityTime, err := e.startProcess(cmd, flags)
	if err != nil {
		return UnknownPID, time.Time{}, nil, err
	}

	// Get the wait result channel, but do not actually start waiting
	// This also has the effect of tying the wait for this process to the command that started it.
	ws, _ := e.tryStartWaiting(pid, processIdentityTime, waitableCmd{cmd, flags}, waitReasonNone)

	// Start the goroutine that waits for the context to expire.
	go func() {

		select {

		case <-ws.waitEndedCh:
			// The process exited before the context expired.
			if handler != nil {
				exitCode, execError := getProcessExecResult(ws.waitErr, cmd)
				handler.OnProcessExited(pid, exitCode, errors.Join(ctx.Err(), execError))
			}

		case <-ctx.Done():
			_, shouldStopProcess := e.tryStartWaiting(pid, processIdentityTime, waitableCmd{cmd, flags}, waitReasonStopping)
			var stopProcessErr error = nil

			if shouldStopProcess {
				// CONSIDER: having an option to specify whether to shut down the process when the context expires.
				log := e.log.WithValues(
					"PID", pid,
					"Command", cmd.Path,
					"Args", cmd.Args[1:],
				)
				log.Info("Context expired, stopping process...")
				stopProcessErr = e.stopProcessInternal(pid, processIdentityTime, optIsResponsibleForStopping)
				if stopProcessErr != nil {
					log.Error(stopProcessErr, "Could not stop process upon context expiration")
					if handler != nil {
						// Let the caller know that the process did not stop upon context expiration
						handler.OnProcessExited(pid, UnknownExitCode, errors.Join(stopProcessErr, ctx.Err()))
					}

					// There is no point waiting for the result if the process could not be stopped and we reported the error.
					break
				}
			}

			<-ws.waitEndedCh

			if handler != nil {
				exitCode, execError := getProcessExecResult(ws.waitErr, cmd)
				handler.OnProcessExited(pid, exitCode, errors.Join(stopProcessErr, execError, ctx.Err()))
			}
		}
	}()

	startWaitingForProcessExit := func() {
		_, _ = e.tryStartWaiting(pid, processIdentityTime, waitableCmd{cmd, flags}, waitReasonMonitoring)
	}

	return pid, processIdentityTime, startWaitingForProcessExit, nil
}

func (e *OSExecutor) StartAndForget(cmd *exec.Cmd, flags ProcessCreationFlag) (Pid_t, time.Time, error) {
	e.acquireLock()
	if e.disposed {
		e.releaseLock()
		return UnknownPID, time.Time{}, ErrDisposed
	}
	e.releaseLock()

	pid, processStartTime, err := e.startProcess(cmd, flags)
	if err != nil {
		return UnknownPID, time.Time{}, err
	}

	if cmd.Process == nil {
		e.log.V(1).Info("Process info is not available after successful start???",
			"PID", pid,
			"Command", cmd.Path,
			"Args", cmd.Args[1:],
		)
	} else {
		// We have to wait (not cmd.Process.Release()) because if we don't, then if the child process exits
		// before the parent process exist, the child becomes a zombie (on non-Windows platforms).
		go func(p *os.Process) {
			_, _ = p.Wait()
		}(cmd.Process)
	}

	return pid, processStartTime, nil
}

func (e *OSExecutor) StopProcess(pid Pid_t, processStartTime time.Time) error {
	e.acquireLock()
	if e.disposed {
		e.releaseLock()
		return ErrDisposed
	}
	e.releaseLock()

	return e.stopProcessInternal(pid, processStartTime, optNone)
}

// Returns the PID, process identity time (to distinguish between process instances with the same PID), and error.
func (e *OSExecutor) startProcess(cmd *exec.Cmd, flags ProcessCreationFlag) (Pid_t, time.Time, error) {
	e.prepareProcessStart(cmd, flags)

	if err := cmd.Start(); err != nil {
		return UnknownPID, time.Time{}, err
	}

	osPid := cmd.Process.Pid
	pid := Uint32_ToPidT(uint32(osPid))

	startLog := e.log.WithValues(
		"PID", pid,
		"Command", cmd.Path,
		"Args", cmd.Args[1:],
		"CreationFlags", flags,
	)

	processIdentityTime := ProcessIdentityTime(pid)

	startCompletionErr := e.completeProcessStart(cmd, pid, processIdentityTime, flags)
	if startCompletionErr != nil {
		startLog.Error(startCompletionErr, "Could not complete process start")

		// If we could not complete the process start, we need to stop the process.
		// Do not try graceful stop (no optTrySignal), just kill it immediately.
		if stopErr := e.stopProcessInternal(pid, processIdentityTime, optIsResponsibleForStopping); stopErr != nil {
			startLog.Error(stopErr, "Could not stop process after failed start")
		}

		return UnknownPID, time.Time{}, fmt.Errorf("could not complete process start: %w", startCompletionErr)
	}

	startLog.V(1).Info("Process started successfully", "PID", pid)
	return pid, processIdentityTime, nil
}

// Atomically starts waiting on the passed waitable if noting is already waiting in association with the process
// identified by PID. If the process is already being waited on, the reason is updated.
//
// Returns the waitState object associated with the process, and a boolean indicating whether the caller
// is the first one to indicate that the reason for the wait is "stopping the process",
// and thus IT is the caller that must stop the process.
func (e *OSExecutor) tryStartWaiting(pid Pid_t, startTime time.Time, waitable Waitable, reason waitReason) (*waitState, bool) {
	e.acquireLock()
	defer e.releaseLock()

	ws, found := e.procsWaiting[WaitKey{pid, startTime}]
	callerShouldStopProcess := false

	if found {
		if !ws.waitEnded.IsZero() {
			// The process has already exited, and we captured the wait result, there is no need to start waiting again,
			// or update anything.
			return ws, false
		}

		callerShouldStopProcess = (reason&waitReasonStopping) != 0 && (ws.reason&waitReasonStopping) == 0
		mustStartWaiting := ws.reason == waitReasonNone && reason != waitReasonNone
		ws.reason |= reason
		if mustStartWaiting {
			go e.doWait(ws, waitable, pid)
		}
	} else {
		callerShouldStopProcess = (reason & waitReasonStopping) != 0
		ws = &waitState{
			waitable:    waitable,
			waitEndedCh: make(chan struct{}),
			reason:      reason,
		}
		e.procsWaiting[WaitKey{pid, startTime}] = ws
		if reason != waitReasonNone {
			go e.doWait(ws, waitable, pid)
		}
	}

	return ws, callerShouldStopProcess
}

func (e *OSExecutor) doWait(ws *waitState, waitable Waitable, pid Pid_t) {
	e.log.V(1).Info("Starting waiting for process to exit", "PID", pid)
	err := waitable.Wait()
	e.log.V(1).Info("Process wait ended", "PID", pid, "Error", logger.FriendlyErrorString(err), "Command", waitable.Info())

	e.acquireLock()
	defer e.releaseLock()

	ws.waitEnded = time.Now()
	ws.waitErr = err
	close(ws.waitEndedCh)
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

	if e.disposed {
		// Do not forget any process state when we are disposing of the executor,
		// so that we have all the information needed to stop processes.
		return
	}

	// Only keep wait states that correspond to processes that are still running, or the ones that completed recently
	e.procsWaiting = maps.Select(e.procsWaiting, func(_ WaitKey, ws *waitState) bool {
		return ws.waitEnded.IsZero() || time.Since(ws.waitEnded) < maxCompletedDuration
	})
}

func (e *OSExecutor) releaseLock() {
	e.lock.Unlock()
}

func (e *OSExecutor) stopProcessInternal(pid Pid_t, processStartTime time.Time, opts processStoppingOpts) error {
	tree, err := GetProcessTree(ProcessTreeItem{pid, processStartTime})
	if err != nil {
		return fmt.Errorf("could not get process tree for process %d: %w", pid, err)
	}

	procTreeLog := e.log.WithValues("Root", pid)
	procTreeLog.V(1).Info("Stopping process tree...", "Root", pid, "Tree", getIDs(tree))

	procEndedCh, stopErr := e.stopSingleProcess(pid, processStartTime, opts|optNotFoundIsError|optTrySignal|optWaitForStdio)
	if stopErr != nil && !errors.Is(stopErr, ErrTimedOutWaitingForProcessToStop) {
		// If the root process cannot be stopped (and it is not just a timeout error), don't bother with the rest of the tree.
		procTreeLog.Error(stopErr, "Could not stop root process")
		return stopErr
	}

	waitForRootProcessToEnd := func() error {
		if errors.Is(stopErr, ErrTimedOutWaitingForProcessToStop) {
			// Do not bother waiting for the confirmation of root process exit, it probably is not going to happen
			// if a timeout occurred already...
			procTreeLog.V(1).Info("Timed out waiting for root process to stop")
			return ErrTimedOutWaitingForProcessToStop
		}

		select {
		case <-procEndedCh:
			procTreeLog.Info("Root process has stopped")
			return nil

		case <-time.After(waitForProcessExitTimeout):
			procTreeLog.Error(ErrTimedOutWaitingForProcessToStop, "Did not get confirmation that the root process has stopped before timeout elapsed")
			return ErrTimedOutWaitingForProcessToStop
		}
	}

	tree = tree[1:] // We have processed the root
	if len(tree) == 0 {
		procTreeLog.V(1).Info("The root process has no children")
		return waitForRootProcessToEnd()
	}

	procTreeLog.V(1).Info("Make sure children of the root processes are gone...")
	childStoppingErrors := slices.MapConcurrent[ProcessTreeItem, error](tree, func(p ProcessTreeItem) error {
		// Retry stopping the child process as we occasionally see transient "Access Denied" errors.
		const childStopTimeout = 2 * time.Second
		childLog := procTreeLog.WithValues("Child", p.Pid)

		retryErr := resiliency.RetryExponentialWithTimeout(context.Background(), childStopTimeout, func() error {
			childLog.V(1).Info("Stopping child process...")

			_, childStopErr := e.stopSingleProcess(p.Pid, p.IdentityTime, opts&^optNotFoundIsError)
			if childStopErr != nil {
				childLog.V(1).Info("Error stopping child process", "Error", childStopErr.Error())
			} else {
				childLog.V(1).Info("Child process has been stopped (or is gone)")
			}

			return childStopErr
		})

		if retryErr != nil {
			childLog.Error(err, "Could not stop child process")
		}
		return retryErr

	}, slices.MaxConcurrency)

	childStoppingErrors = slices.Select(childStoppingErrors, func(e error) bool { return e != nil })
	if len(childStoppingErrors) > 0 {
		procTreeLog.V(1).Error(errors.Join(childStoppingErrors...), "Some child processes could not be stopped")
	} else {
		procTreeLog.V(1).Info("All child processes have stopped")
	}

	// Depending on how (grand)children are launched, the os.exec.Cmd.Wait() API may not return until
	// all grandchildren have exited, so we want to try to kill all these grandchildren BEFORE waiting
	// for the root process to exit.
	// And even this is not 100% reliable because
	//     a. some grandchildren may exit, leaving the great-grandchildren orphaned
	//     b. we have a time-of-check vs time-of-use problem  with the process tree, which is a snapshot,
	//        and may be out-of-date for processes spawn children vigorously,
	// So that is why the following wait operation employs a timeout.
	rootWaitErr := waitForRootProcessToEnd()

	return errors.Join(append([]error{rootWaitErr}, childStoppingErrors...)...)
}

var maxConcurrentProcessStops = runtime.NumCPU() * 5

// Disposes the process executor.
func (e *OSExecutor) Dispose() {
	e.acquireLock()
	if e.disposed {
		e.releaseLock()
		return
	}
	e.disposed = true
	// Make a shallow copy of the waiting processes map so we can safely iterate over it while stopping processes.
	currentProcs := stdlib_maps.Clone(e.procsWaiting)
	e.releaseLock()

	if len(currentProcs) == 0 {
		e.log.V(1).Info("No processes to stop during executor disposal")
		return
	} else {
		e.log.V(1).Info("Stopping processes during executor disposal...", "Count", len(currentProcs))
	}

	wg := &sync.WaitGroup{}
	wg.Add(len(currentProcs))
	sem := concurrency.NewSemaphoreWithCount(uint(maxConcurrentProcessStops))

	for wk, ws := range currentProcs {
		<-sem.Wait().Chan

		go func() {
			defer wg.Done()
			defer sem.Signal()
			e.acquireLock()

			if ws != nil && !ws.waitEnded.IsZero() {
				// The process has already ended
				e.releaseLock()
				return
			}

			waitable := ws.waitable
			flags := waitable.Flags()
			e.releaseLock()

			if flags&CreationFlagEnsureKillOnDispose == CreationFlagEnsureKillOnDispose {
				// Best effort to stop the process.
				e.log.V(1).Info("Stopping process during executor disposal...", "PID", wk.Pid, "Command", waitable.Info())
				stopErr := e.stopProcessInternal(wk.Pid, wk.StartedAt, optIsResponsibleForStopping|optTrySignal)
				if stopErr != nil {
					e.log.Error(stopErr, "Could not stop process during executor disposal", "PID", wk.Pid, "Command", waitable.Info())
				}
			} else {
				// Just make sure we called wait() so the process does not become a zombie.
				_, _ = e.tryStartWaiting(wk.Pid, wk.StartedAt, waitable, waitReasonMonitoring)
			}
		}()
	}

	wg.Wait()

	e.completeDispose()
	e.log.V(1).Info("Process executor disposed")
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
