package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/tklauser/ps"

	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
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

type waitState struct {
	waitable    Waitable      // The waitable that is being waited on
	waitEndedCh chan struct{} // A channel that gets closed when the wait ends
	waitErr     error         // The result of the process wait. Not valid until waitEndedCh is closed.
	waitEnded   time.Time     // The time when the wait function ended. Zero if the wait is still in progress.
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

func (e *OSExecutor) StartProcess(ctx context.Context, cmd *exec.Cmd, handler ProcessExitHandler) (Pid_t, time.Time, func(), error) {
	pid, processStartTime, err := e.startProcess(cmd)
	if err != nil {
		return UnknownPID, time.Time{}, nil, err
	}

	// Get the wait result channel, but do not actually start waiting
	// This also has the effect of tying the wait for this process to the command that started it.
	ws, _ := e.tryStartWaiting(pid, cmd, waitReasonNone)

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
			_, shouldStopProcess := e.tryStartWaiting(pid, cmd, waitReasonStopping)
			var stopProcessErr error = nil

			if shouldStopProcess {
				// CONSIDER: having an option to specify whether to shut down the process when the context expires.
				stopProcessErr = e.stopProcessInternal(pid, processStartTime, optIsResponsibleForStopping)
				if stopProcessErr != nil {
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
		_, _ = e.tryStartWaiting(pid, cmd, waitReasonMonitoring)
	}

	return pid, processStartTime, startWaitingForProcessExit, nil
}

func (e *OSExecutor) StartAndForget(cmd *exec.Cmd) (Pid_t, time.Time, error) {
	pid, processStartTime, err := e.startProcess(cmd)
	if err != nil {
		return UnknownPID, time.Time{}, err
	}

	if cmd.Process == nil {
		e.log.V(1).Info("process info is not available after successful start???",
			"PID", pid,
			"Command", cmd.Path,
			"Args", cmd.Args,
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

func (e *OSExecutor) startProcess(cmd *exec.Cmd) (Pid_t, time.Time, error) {
	if err := cmd.Start(); err != nil {
		return UnknownPID, time.Time{}, err
	}
	processStartTime := time.Now()

	osPid := cmd.Process.Pid
	pid, err := IntToPidT(osPid)
	if err != nil {
		return UnknownPID, time.Time{}, err
	}

	psProcess, psProcessErr := ps.FindProcess(osPid)
	if psProcessErr != nil {
		e.log.Error(psProcessErr, "could not find process startup time", "PID", osPid)
	} else {
		// This is what the OS process startup timestamp is, so it is the most accurate value we can get.
		processStartTime = psProcess.CreationTime()
	}

	return pid, processStartTime, nil
}

// Atomically starts waiting on the passed waitable if noting is already waiting in association with the process
// identified by PID. If the process is already being waited on, the reason is updated.
//
// Returns the waitState object associated with the process, and a boolean indicating whether the caller
// is the first one to indicate that the reason for the wait is "stopping the process",
// and thus IT is the caller that must stop the process.
func (e *OSExecutor) tryStartWaiting(pid Pid_t, waitable Waitable, reason waitReason) (*waitState, bool) {
	doWait := func(ws *waitState) {
		e.log.V(1).Info("starting waiting for process to exit", "pid", pid)
		err := ws.waitable.Wait()
		e.log.V(1).Info("process wait ended", "pid", pid, "Error", logger.FriendlyErrorString(err))

		e.acquireLock()
		defer e.releaseLock()

		ws.waitEnded = time.Now()
		ws.waitErr = err
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
			return ws, false
		}

		callerShouldStopProcess = (reason&waitReasonStopping) != 0 && (ws.reason&waitReasonStopping) == 0
		if ws.reason == waitReasonNone && reason != waitReasonNone {
			go doWait(ws)
		}
		ws.reason |= reason
	} else {
		callerShouldStopProcess = (reason & waitReasonStopping) != 0
		ws = &waitState{
			waitable:    waitable,
			waitEndedCh: make(chan struct{}),
			reason:      reason,
		}
		e.procsWaiting[pid] = ws
		if reason != waitReasonNone {
			go doWait(ws)
		}
	}

	return ws, callerShouldStopProcess
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
	tree, err := GetProcessTree(ProcessTreeItem{pid, processStartTime})
	if err != nil {
		return fmt.Errorf("could not get process tree for process %d: %w", pid, err)
	}

	e.log.V(1).Info("stopping process tree", "root", pid, "tree", getIDs(tree))

	procEndedCh, stopErr := e.stopSingleProcess(pid, processStartTime, opts|optNotFoundIsError|optTrySignal|optWaitForStdio)
	if stopErr != nil && !errors.Is(stopErr, ErrTimedOutWaitingForProcessToStop) {
		// If the root process cannot be stopped (and it is not just a timeout error), don't bother with the rest of the tree.
		e.log.Error(stopErr, "could not stop root process", "root", pid)
		return stopErr
	}

	waitForRootProcessToEnd := func() error {
		if errors.Is(stopErr, ErrTimedOutWaitingForProcessToStop) {
			// Do not bother waiting for the confirmation of root process exit, it probably is not going to happen
			// if a timeout occurred already...
			e.log.V(1).Info("timed out waiting for root process to stop", "root", pid)
			return ErrTimedOutWaitingForProcessToStop
		}

		select {
		case <-procEndedCh:
			e.log.V(1).Info("root process has stopped", "root", pid)
			return nil

		case <-time.After(waitForProcessExitTimeout):
			e.log.Error(ErrTimedOutWaitingForProcessToStop, "did not get confirmation that the root process has stopped before timeout elapsed", "root", pid)
			return ErrTimedOutWaitingForProcessToStop
		}
	}

	tree = tree[1:] // We have processed the root
	if len(tree) == 0 {
		e.log.V(1).Info("the root process has no children", "root", pid)
		return waitForRootProcessToEnd()
	}

	e.log.V(1).Info("make sure children of the root processes are gone...", "root", pid, "children", getIDs(tree))
	childStoppingErrors := slices.MapConcurrent[ProcessTreeItem, error](tree, func(p ProcessTreeItem) error {
		// Retry stopping the child process as we occasionally see transient "Access Denied" errors.
		const childStopTimeout = 2 * time.Second

		retryErr := resiliency.RetryExponentialWithTimeout(context.Background(), childStopTimeout, func() error {
			e.log.V(1).Info("stopping child process...", "child", p.Pid, "root", pid)

			_, childStopErr := e.stopSingleProcess(p.Pid, p.CreationTime, opts&^optNotFoundIsError)
			if childStopErr != nil {
				e.log.V(1).Info("error stopping child process", "child", p.Pid, "root", pid, "error", childStopErr.Error())
			} else {
				e.log.V(1).Info("child process has stopped (or is gone)", "child", p.Pid, "root", pid)
			}

			return childStopErr
		})

		if retryErr != nil {
			e.log.Error(err, "could not stop child process", "child", p.Pid, "root", pid)
		}
		return retryErr

	}, slices.MaxConcurrency)

	childStoppingErrors = slices.Select(childStoppingErrors, func(e error) bool { return e != nil })
	if len(childStoppingErrors) > 0 {
		e.log.V(1).Error(errors.Join(childStoppingErrors...), "some child processes could not be stopped", "root", pid)
	} else {
		e.log.V(1).Info("all child processes have stopped", "root", pid)
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
