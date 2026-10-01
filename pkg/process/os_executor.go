/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"fmt"
	stdlib_maps "maps"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/slices"
)

type waitReason uint32

const (
	waitReasonNone       waitReason = 0x0
	waitReasonMonitoring waitReason = 0x1
	waitReasonStopping   waitReason = 0x2
)

var (
	ErrDisposed = errors.New("the process executor has been disposed")
)

type waitState struct {
	waitable      Waitable      // The waitable that is being waited on
	waitEndedCh   chan struct{} // A channel that gets closed when the wait ends
	waitErr       error         // The result of the process wait. Not valid until waitEndedCh is closed.
	waitEnded     time.Time     // The time when the wait function ended. Zero if the wait is still in progress.
	reason        waitReason    // The reason why are waiting on the process
	waitStarted   bool
	forceKillUsed bool
}

type osExecutorBase struct {
	procsWaiting           map[ProcessHandle]*waitState
	disposed               bool
	lock                   sync.Locker
	log                    logr.Logger
	lifetimeCtx            context.Context
	lifetimeCtxCancel      context.CancelFunc
	startLifetimeCtx       context.Context
	startLifetimeCtxCancel context.CancelCauseFunc
	startsInFlight         sync.WaitGroup
}

func newOSExecutorBase(log logr.Logger) *osExecutorBase {
	lifetimeCtx, lifetimeCtxCancel := context.WithCancel(context.Background())
	startLifetimeCtx, startLifetimeCtxCancel := context.WithCancelCause(context.Background())
	return &osExecutorBase{
		procsWaiting:           make(map[ProcessHandle]*waitState),
		disposed:               false,
		lock:                   &sync.Mutex{},
		log:                    log.WithName("os-executor"),
		lifetimeCtx:            lifetimeCtx,
		lifetimeCtxCancel:      lifetimeCtxCancel,
		startLifetimeCtx:       startLifetimeCtx,
		startLifetimeCtxCancel: startLifetimeCtxCancel,
	}
}

func (e *OSExecutor) beginProcessStart(parent context.Context) (context.Context, func(), error) {
	e.acquireLock()
	if e.disposed {
		e.releaseLock()
		return nil, nil, ErrDisposed
	}
	e.startsInFlight.Add(1)
	startLifetimeCtx := e.startLifetimeCtx
	e.releaseLock()

	startCtx, cancelStart := context.WithCancelCause(parent)
	stopLifetimeCancellation := context.AfterFunc(startLifetimeCtx, func() {
		cancelStart(context.Cause(startLifetimeCtx))
	})
	finishStart := func() {
		stopLifetimeCancellation()
		cancelStart(nil)
		e.startsInFlight.Done()
	}
	return startCtx, finishStart, nil
}

func (e *OSExecutor) StartProcess(
	ctx context.Context,
	cmd *exec.Cmd,
	handler ProcessExitHandler,
	flags ProcessCreationFlag,
	sysCreateProcess SysCreateProcessFunc,
) (ProcessHandle, func(), error) {
	startCtx, finishStart, admissionErr := e.beginProcessStart(ctx)
	if admissionErr != nil {
		return ProcessHandle{Pid: UnknownPID}, nil, admissionErr
	}
	defer finishStart()

	handle, waitable, startProcessErr := e.startProcess(startCtx, cmd, flags, sysCreateProcess)
	if startProcessErr != nil {
		return ProcessHandle{Pid: UnknownPID}, nil, startProcessErr
	}

	pid := handle.Pid

	// Get the wait result channel, but do not actually start waiting
	// This also has the effect of tying the wait for this process to the command that started it.
	ws, _ := e.tryStartWaiting(handle, waitable, waitReasonNone)

	// Start the goroutine that waits for the context to expire.
	go func() {

		select {

		case <-ws.waitEndedCh:
			// The process exited before the context expired.
			if handler != nil {
				exitCode, execError := getProcessExecResult(ws.waitErr, ws.waitable, cmd)
				handler.OnProcessExited(pid, exitCode, errors.Join(ctx.Err(), execError))
			}

		case <-ctx.Done():
			cleanupCtx, cleanupCancel := WithDetachedStopTimeout(ctx)
			defer cleanupCancel()
			_, _ = e.tryStartWaiting(handle, waitable, waitReasonMonitoring)
			cleanupLog := e.log.WithValues("PID", pid, "Command", cmd.Path, "Args", cmd.Args[1:])
			cleanupLog.Info("Context expired, stopping process...")
			stopProcessErr := e.stopProcessInternal(cleanupCtx, handle, processStopOptions{opts: optNone})
			if IsProcessGoneErr(stopProcessErr) {
				stopProcessErr = nil
			}
			if stopProcessErr != nil {
				cleanupLog.Error(stopProcessErr, "Could not stop process upon context expiration")
				if handler != nil {
					handler.OnProcessExited(pid, UnknownExitCode, errors.Join(stopProcessErr, ctx.Err()))
				}
				break
			}

			select {
			case <-ws.waitEndedCh:
			case <-cleanupCtx.Done():
				if handler != nil {
					handler.OnProcessExited(pid, UnknownExitCode, cleanupCtx.Err())
				}
				return
			}

			if handler != nil {
				exitCode, execError := getProcessExecResult(ws.waitErr, ws.waitable, cmd)
				handler.OnProcessExited(pid, exitCode, errors.Join(stopProcessErr, execError, ctx.Err()))
			}
		}
	}()

	startWaitingForProcessExit := func() {
		_, _ = e.tryStartWaiting(handle, waitable, waitReasonMonitoring)
	}

	return handle, startWaitingForProcessExit, nil
}

func (e *OSExecutor) StartAndForget(cmd *exec.Cmd, flags ProcessCreationFlag) (ProcessHandle, error) {
	startCtx, finishStart, admissionErr := e.beginProcessStart(context.Background())
	if admissionErr != nil {
		return ProcessHandle{Pid: UnknownPID}, admissionErr
	}
	defer finishStart()

	handle, waitable, startProcessErr := e.startProcess(startCtx, cmd, flags, nil)
	if startProcessErr != nil {
		return ProcessHandle{Pid: UnknownPID}, startProcessErr
	}

	// We have to wait (not cmd.Process.Release()) because if we don't, then if the child process exits
	// before the parent process exist, the child becomes a zombie (on non-Windows platforms).
	if waitable != nil {
		go func() {
			_ = waitable.Wait()
		}()
	}

	return handle, nil
}

func (e *OSExecutor) StopProcess(ctx context.Context, handle ProcessHandle, options ...ProcessStopOption) error {
	e.acquireLock()
	if e.disposed {
		e.releaseLock()
		return ErrDisposed
	}
	e.releaseLock()

	stopOptions := newProcessStopOptions(options)
	return e.stopProcessInternal(ctx, handle, stopOptions)
}

// Returns the process handle, waitable process, and error.
func (e *OSExecutor) startProcess(
	ctx context.Context,
	cmd *exec.Cmd,
	flags ProcessCreationFlag,
	sysCreateProcess SysCreateProcessFunc,
) (ProcessHandle, Waitable, error) {
	e.prepareProcessStart(cmd, flags)
	if cancellationErr := context.Cause(ctx); cancellationErr != nil {
		return ProcessHandle{Pid: UnknownPID}, nil, cancellationErr
	}

	var handle ProcessHandle
	var waitable Waitable

	if sysCreateProcess != nil {
		var sysCreateErr error
		handle, waitable, sysCreateErr = sysCreateProcess(ctx, cmd)
		if sysCreateErr != nil {
			return ProcessHandle{Pid: UnknownPID}, nil, errors.Join(context.Cause(ctx), sysCreateErr)
		}
		if waitable == nil {
			return ProcessHandle{Pid: UnknownPID}, nil, fmt.Errorf("%w: sysCreateProcess returned nil waitable", ErrProcessStartUncertain)
		}
		if handleErr := handle.Validate(); handleErr != nil {
			return ProcessHandle{Pid: UnknownPID}, nil, errors.Join(
				fmt.Errorf("sysCreateProcess returned an incomplete identity: %w", handleErr),
				abortStartedProcess(waitable))
		}
	} else {
		if cmdStartErr := cmd.Start(); cmdStartErr != nil {
			return ProcessHandle{Pid: UnknownPID}, nil, errors.Join(context.Cause(ctx), cmdStartErr)
		}
		waitable = &waitableCmd{cmd, flags}
		var handleErr error
		handle, handleErr = ProcessHandleFromCmd(cmd)
		if handleErr != nil {
			return ProcessHandle{Pid: UnknownPID}, nil, errors.Join(handleErr, abortStartedProcess(waitable))
		}
	}

	pid := handle.Pid
	startLog := e.log.WithValues(
		"PID", pid,
		"Command", cmd.Path,
		"Args", cmd.Args[1:],
		"CreationFlags", flags,
	)

	abortForCancellation := func(cancellationErr error) (ProcessHandle, Waitable, error) {
		abortErr := abortStartedProcess(waitable)
		if abortErr != nil {
			startLog.Error(abortErr, "Could not roll back process after start cancellation")
		}
		return ProcessHandle{Pid: UnknownPID}, nil, errors.Join(cancellationErr, abortErr)
	}

	if cancellationErr := context.Cause(ctx); cancellationErr != nil {
		return abortForCancellation(cancellationErr)
	}

	startCompletionErr := e.completeProcessStart(handle, flags)
	if startCompletionErr != nil {
		startLog.Error(startCompletionErr, "Could not complete process start")

		abortErr := abortStartedProcess(waitable)
		if abortErr != nil {
			startLog.Error(abortErr, "Could not roll back process after failed start")
		}
		return ProcessHandle{Pid: UnknownPID}, nil, errors.Join(
			fmt.Errorf("could not complete process start: %w", startCompletionErr), abortErr)
	}

	if cancellationErr := context.Cause(ctx); cancellationErr != nil {
		return abortForCancellation(cancellationErr)
	}

	startLog.V(1).Info("Process started successfully", "PID", pid)
	return handle, waitable, nil
}

// Atomically starts waiting on the passed waitable if noting is already waiting in association with the process
// identified by PID. If the process is already being waited on, the reason is updated.
//
// Returns the waitState object associated with the process, and a boolean indicating whether the caller
// is the first one to indicate that the reason for the wait is "stopping the process",
// and thus IT is the caller that must stop the process.
func (e *OSExecutor) tryStartWaiting(handle ProcessHandle, waitable Waitable, reason waitReason) (*waitState, bool) {
	e.acquireLock()
	defer e.releaseLock()

	ws, found := e.procsWaiting[handle]
	callerShouldStopProcess := false

	if found {
		if !ws.waitEnded.IsZero() {
			// The process has already exited, and we captured the wait result, there is no need to start waiting again,
			// or update anything.
			return ws, false
		}

		callerShouldStopProcess = (reason&waitReasonStopping) != 0 && (ws.reason&waitReasonStopping) == 0
		if callerShouldStopProcess {
			ws.forceKillUsed = false
		}

		if ws.waitable == nil {
			ws.waitable = waitable
		}
		mustStartWaiting := !ws.waitStarted && reason != waitReasonNone
		ws.reason |= reason

		if mustStartWaiting {
			ws.waitStarted = true
			go e.doWait(ws, ws.waitable, handle.Pid)
		}
	} else {
		callerShouldStopProcess = (reason & waitReasonStopping) != 0
		ws = &waitState{
			waitable:    waitable,
			waitEndedCh: make(chan struct{}),
			reason:      reason,
			waitStarted: reason != waitReasonNone,
		}
		e.procsWaiting[handle] = ws
		if reason != waitReasonNone {
			go e.doWait(ws, waitable, handle.Pid)
		}
	}

	return ws, callerShouldStopProcess
}

// Starts an existing executor-owned wait without claiming stop ownership.
func (e *OSExecutor) ensureTrackedWaitStarted(handle ProcessHandle) {
	e.acquireLock()
	defer e.releaseLock()

	ws, found := e.procsWaiting[handle]
	if !found || ws.waitStarted || ws.waitable == nil || !ws.waitEnded.IsZero() {
		return
	}

	ws.waitStarted = true
	ws.reason |= waitReasonMonitoring
	go e.doWait(ws, ws.waitable, handle.Pid)
}

func (e *OSExecutor) finishStopAttempt(ws *waitState) {
	e.acquireLock()
	defer e.releaseLock()
	ws.reason &^= waitReasonStopping
}

func (e *OSExecutor) markForceKillUsed(ws *waitState) {
	e.acquireLock()
	defer e.releaseLock()
	ws.forceKillUsed = true
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

func waitForTrackedProcessExit(ctx context.Context, pid Pid_t, ws *waitState, waitTimeout time.Duration) error {
	var timeoutCh <-chan time.Time
	var timer *time.Timer
	if waitTimeout > 0 {
		timer = time.NewTimer(waitTimeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case <-ctx.Done():
		return ctx.Err()

	case <-ws.waitEndedCh:
		return trackedProcessWaitResult(pid, ws)

	case <-timeoutCh:
		return ErrTimedOutWaitingForProcessToStop
	}
}

func trackedProcessWaitResult(pid Pid_t, ws *waitState) error {
	if ws.waitErr == nil || IsEarlyProcessExitError(ws.waitErr) {
		return nil
	}

	return fmt.Errorf("could not wait for process %d to exit: %w", pid, ws.waitErr)
}

func waitForProcessStopConfirmation(
	ctx context.Context,
	processEndedCh <-chan struct{},
	stopErr error,
	waitTimeout time.Duration,
) error {
	if errors.Is(stopErr, ErrTimedOutWaitingForProcessToStop) {
		return ErrTimedOutWaitingForProcessToStop
	}
	if processEndedCh == nil {
		return nil
	}

	var timeoutCh <-chan time.Time
	var timer *time.Timer
	if waitTimeout > 0 {
		timer = time.NewTimer(waitTimeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-processEndedCh:
		return nil
	case <-timeoutCh:
		return ErrTimedOutWaitingForProcessToStop
	}
}

// Returns the process execution error and process exit code depending on the result
// of a process wait operation.
func getProcessExecResult(waitErr error, w Waitable, cmd *exec.Cmd) (int32, error) {
	var ee *exec.ExitError
	ecs, haveEcs := w.(ExitCodeSource)

	switch {
	case waitErr == nil && haveEcs:
		return ecs.ExitCode(), nil
	case waitErr == nil && cmd.ProcessState != nil:
		return int32(cmd.ProcessState.ExitCode()), nil
	case errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil:
		return int32(cmd.ProcessState.ExitCode()), nil
	case waitErr != nil && errors.As(waitErr, &ee):
		return int32(ee.ExitCode()), nil
	default:
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
	e.procsWaiting = maps.Select(e.procsWaiting, func(_ ProcessHandle, ws *waitState) bool {
		return ws.waitEnded.IsZero() || time.Since(ws.waitEnded) < maxCompletedDuration
	})
}

func (e *OSExecutor) releaseLock() {
	e.lock.Unlock()
}

func (e *OSExecutor) stopProcessInternal(ctx context.Context, handle ProcessHandle, options processStopOptions) error {
	return e.stopProcessTreeInternal(ctx, handle, options, GetProcessTree)
}

// stopProcessTreeInternal stops a verified process tree root-first.
//
// The root process gets the first graceful-stop opportunity and may escalate to a force kill.
// If the root exits gracefully, verified descendants are given the remainder of the shared
// graceful-stop budget to stop concurrently. If the root required a force kill, or the graceful
// budget expires, remaining descendants are force-killed concurrently during the final bounded
// cleanup phase. Caller cancellation stops further work. Every signal and kill revalidates the
// target process identity before acting.
func (e *OSExecutor) stopProcessTreeInternal(
	ctx context.Context,
	handle ProcessHandle,
	options processStopOptions,
	resolveProcessTree func(context.Context, ProcessHandle) ([]ProcessHandle, error),
) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if handleErr := handle.Validate(); handleErr != nil {
		return handleErr
	}
	opts := options.opts
	e.ensureTrackedWaitStarted(handle)

	graceCtx, graceCancel := context.WithTimeout(ctx, gracefulProcessStopTimeout)
	defer graceCancel()

	procTreeLog := e.log.WithValues("Root", handle.Pid)
	notifyRootExit := func() {
		if options.afterRootExit == nil || !IsProcessGoneErr(e.CheckProcessRunning(handle)) {
			return
		}
		options.afterRootExit()
		options.afterRootExit = nil
	}
	rootWasVerified := false

	stopRootProcess := func(stopCtx context.Context, rootOpts processStoppingOpts) (singleProcessStopResult, error, error) {
		stopResult, stopErr := e.stopSingleProcess(stopCtx, handle, rootOpts|optNotFoundIsError|optWaitForStdio)
		if rootWasVerified && IsProcessGoneErr(stopErr) {
			return singleProcessStopResult{waitEndedCh: makeClosedChan()}, nil, nil
		}
		if stopErr != nil &&
			!errors.Is(stopErr, ErrTimedOutWaitingForProcessToStop) &&
			!errors.Is(stopErr, context.DeadlineExceeded) {
			// If the root process cannot be stopped (and it is not just a timeout error), don't bother with the rest of the tree.
			procTreeLog.Error(stopErr, "Could not stop root process")
			return singleProcessStopResult{}, stopErr, stopErr
		}

		return stopResult, stopErr, nil
	}

	waitForRootProcessToEnd := func(waitCtx context.Context, procEndedCh <-chan struct{}, stopErr error) error {
		waitErr := waitForProcessStopConfirmation(waitCtx, procEndedCh, stopErr, processStopTimeout)
		switch {
		case errors.Is(stopErr, ErrTimedOutWaitingForProcessToStop):
			procTreeLog.V(1).Info("Timed out waiting for root process to stop")
		case procEndedCh == nil:
			procTreeLog.V(1).Info("Skipping root process exit confirmation because no wait channel is available")
		case waitErr == nil:
			procTreeLog.Info("Root process has stopped")
		case errors.Is(waitErr, ErrTimedOutWaitingForProcessToStop):
			procTreeLog.Error(ErrTimedOutWaitingForProcessToStop, "Did not get confirmation that the root process has stopped before timeout elapsed")
		}
		return waitErr
	}

	gracefulRootOpts := opts | optTrySignal | optWaitForGracefulDeadline
	forceProcessOpts := opts &^ (optNotFoundIsError | optTrySignal | optSignalConsoleGroup | optGracefulOnly | optWaitForGracefulDeadline)

	forceRootAfterIncompleteEnumeration := func(treeErr error) error {
		if contextErr := ctx.Err(); contextErr != nil {
			return errors.Join(treeErr, contextErr)
		}

		forceCtx, forceCancel := context.WithTimeout(ctx, signalAndWaitTimeout)
		defer forceCancel()
		rootResult, rootStopErr := e.stopSingleProcess(
			forceCtx,
			handle,
			forceProcessOpts|optWaitForStdio,
		)
		if rootStopErr != nil &&
			!errors.Is(rootStopErr, ErrTimedOutWaitingForProcessToStop) &&
			!errors.Is(rootStopErr, context.DeadlineExceeded) {
			return errors.Join(
				fmt.Errorf("%w: process tree enumeration did not complete before the graceful-stop deadline", ErrIncompleteProcessTree),
				treeErr,
				rootStopErr,
			)
		}
		rootWaitErr := waitForRootProcessToEnd(forceCtx, rootResult.waitEndedCh, rootStopErr)
		incompleteErr := fmt.Errorf(
			"%w: process tree enumeration did not complete before the graceful-stop deadline",
			ErrIncompleteProcessTree,
		)
		return errors.Join(incompleteErr, treeErr, rootStopErr, rootWaitErr)
	}

	if (opts & optSkipDescendants) != 0 {
		procTreeLog.V(1).Info("Stopping root process without enumerating descendants")
		rootResult, stopErr, rootStopErr := stopRootProcess(graceCtx, gracefulRootOpts)
		if rootStopErr != nil {
			return rootStopErr
		}
		if stopErr == nil && !rootResult.forceKillUsed {
			notifyRootExit()
			return waitForRootProcessToEnd(ctx, rootResult.waitEndedCh, nil)
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return errors.Join(stopErr, contextErr)
		}

		forceCtx, forceCancel := context.WithTimeout(ctx, signalAndWaitTimeout)
		defer forceCancel()
		if stopErr != nil {
			var fatalForceErr error
			rootResult, stopErr, fatalForceErr = stopRootProcess(forceCtx, forceProcessOpts)
			if fatalForceErr != nil {
				return errors.Join(stopErr, fatalForceErr)
			}
		}

		return waitForRootProcessToEnd(forceCtx, rootResult.waitEndedCh, stopErr)
	}

	tree, treeErr := resolveProcessTree(graceCtx, handle)
	if treeErr != nil && !errors.Is(treeErr, ErrIncompleteProcessTree) {
		if errors.Is(treeErr, context.DeadlineExceeded) && ctx.Err() == nil {
			procTreeLog.Error(treeErr, "Process tree enumeration exceeded the graceful-stop deadline; force-stopping only the root")
			return forceRootAfterIncompleteEnumeration(treeErr)
		}
		return fmt.Errorf("could not get process tree for process %d: %w", handle.Pid, treeErr)
	}
	if errors.Is(treeErr, ErrIncompleteProcessTree) {
		procTreeLog.Error(
			treeErr,
			"Process tree enumeration was incomplete; stopping verified processes, but descendant cleanup remains uncertain",
		)
	}
	if len(tree) == 0 {
		return fmt.Errorf("could not get a verified root for process %d: %w", handle.Pid, treeErr)
	}
	handle = tree[0]
	rootWasVerified = true

	procTreeLog.V(1).Info("Stopping process tree...", "Root", handle.Pid, "Tree", getIDs(tree))

	rootResult, rootStopErr, fatalRootStopErr := stopRootProcess(graceCtx, gracefulRootOpts)
	if fatalRootStopErr != nil {
		return errors.Join(treeErr, fatalRootStopErr)
	}
	if rootStopErr == nil && !rootResult.forceKillUsed {
		notifyRootExit()
	}

	tree = tree[1:] // We have processed the root

	stopChildren := func(
		stopCtx context.Context,
		childOpts processStoppingOpts,
		retry bool,
		action string,
	) []error {
		procTreeLog.V(1).Info(action)
		childStoppingErrors := slices.MapConcurrent[error](tree, func(childHandle ProcessHandle) error {
			childLog := procTreeLog.WithValues("Child", childHandle.Pid)
			stopChild := func() error {
				childLog.V(1).Info("Stopping child process...")
				_, childStopErr := e.stopSingleProcess(stopCtx, childHandle, childOpts)
				if childStopErr != nil {
					childLog.V(1).Info("Error stopping child process", "Error", childStopErr.Error())
				} else {
					childLog.V(1).Info("Child process has been stopped (or is gone)")
				}
				return childStopErr
			}

			var childStopErr error
			if retry {
				// Retry force-killing the child process as we occasionally see transient "Access Denied" errors.
				const childStopTimeout = 2 * time.Second
				childStopErr = resiliency.RetryExponentialWithTimeout(stopCtx, childStopTimeout, stopChild)
			} else {
				childStopErr = stopChild()
			}
			if childStopErr != nil {
				childLog.V(1).Info("Could not stop child process", "Error", childStopErr.Error())
			}
			return childStopErr
		}, slices.MaxConcurrency)

		return slices.Select(childStoppingErrors, func(stopErr error) bool { return stopErr != nil })
	}

	if len(tree) == 0 && rootStopErr == nil && !rootResult.forceKillUsed {
		procTreeLog.V(1).Info("The root process has no children")
		return errors.Join(treeErr, waitForRootProcessToEnd(ctx, rootResult.waitEndedCh, nil))
	}

	forceDescendants := rootResult.forceKillUsed || rootStopErr != nil || graceCtx.Err() != nil

	if len(tree) > 0 && !forceDescendants {
		gracefulChildOpts := opts &^ optNotFoundIsError
		gracefulChildOpts |= optGracefulOnly
		if runtime.GOOS == "windows" || (opts&optSignalConsoleGroup) != 0 {
			// Windows descendants may share the root's group; their PIDs are not necessarily group IDs.
			gracefulChildOpts &^= optTrySignal
		} else {
			gracefulChildOpts |= optTrySignal
		}

		gracefulChildErrors := stopChildren(
			graceCtx,
			gracefulChildOpts,
			false,
			"Giving child processes the remaining graceful-stop budget...",
		)
		forceDescendants = len(gracefulChildErrors) > 0 || graceCtx.Err() != nil
		if !forceDescendants {
			procTreeLog.V(1).Info("All child processes stopped gracefully")
			rootWaitErr := waitForRootProcessToEnd(ctx, rootResult.waitEndedCh, rootStopErr)
			return errors.Join(treeErr, rootWaitErr)
		}

		procTreeLog.V(1).Info("The graceful-stop budget expired before all child processes stopped")
	}

	if contextErr := ctx.Err(); contextErr != nil {
		return errors.Join(treeErr, rootStopErr, contextErr)
	}

	if rootResult.forceKillUsed {
		procTreeLog.V(1).Info("The root process required a force kill; force-killing remaining child processes")
	}

	forceCtx, forceCancel := context.WithTimeout(ctx, signalAndWaitTimeout)
	defer forceCancel()

	if rootStopErr != nil {
		var fatalRootForceErr error
		var forceRootResult singleProcessStopResult
		forceRootResult, rootStopErr, fatalRootForceErr = stopRootProcess(forceCtx, forceProcessOpts)
		if forceRootResult.waitEndedCh != nil {
			rootResult = forceRootResult
		}
		if fatalRootForceErr != nil {
			procTreeLog.Error(fatalRootForceErr, "Could not force-kill root process")
		}
	}

	var childStoppingErrors []error
	if len(tree) > 0 && forceDescendants {
		childStoppingErrors = stopChildren(
			forceCtx,
			forceProcessOpts,
			true,
			"Force-killing remaining child processes...",
		)
	}
	if len(childStoppingErrors) > 0 {
		procTreeLog.V(1).Error(summarizeProcessErrors(childStoppingErrors), "Some child processes could not be stopped")
	} else if len(tree) > 0 {
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
	rootWaitErr := waitForRootProcessToEnd(forceCtx, rootResult.waitEndedCh, rootStopErr)

	return joinProcessTreeStopErrors(treeErr, rootStopErr, rootWaitErr, childStoppingErrors)
}

func joinProcessTreeStopErrors(
	treeErr error,
	rootStopErr error,
	rootWaitErr error,
	childStoppingErrors []error,
) error {
	var descendantCleanupErr error
	if len(childStoppingErrors) > 0 {
		descendantCleanupErr = fmt.Errorf(
			"%w: one or more descendant processes could not be confirmed stopped",
			ErrIncompleteProcessTree,
		)
	}

	return errors.Join(
		treeErr,
		rootStopErr,
		rootWaitErr,
		descendantCleanupErr,
		summarizeProcessErrors(childStoppingErrors),
	)
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
	e.releaseLock()

	e.startLifetimeCtxCancel(ErrDisposed)
	e.startsInFlight.Wait()
	defer e.lifetimeCtxCancel()

	// Make a shallow copy of the waiting processes map so we can safely iterate over it while stopping processes.
	e.acquireLock()
	currentProcs := stdlib_maps.Clone(e.procsWaiting)
	e.releaseLock()

	if len(currentProcs) == 0 {
		e.log.V(1).Info("No processes to stop during executor disposal")
		e.completeDispose()
		return
	} else {
		e.log.V(1).Info("Stopping processes during executor disposal...", "Count", len(currentProcs))
	}

	wg := &sync.WaitGroup{}
	wg.Add(len(currentProcs))
	sem := concurrency.NewSemaphoreWithCount(uint(maxConcurrentProcessStops))

	for handle, ws := range currentProcs {
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
				e.log.V(1).Info("Stopping process during executor disposal...", "PID", handle.Pid, "Command", waitable.Info())
				// One 15-second graceful-stop budget covers the whole process tree. The root is
				// handled first, and descendants receive the remaining budget only if the root
				// exits gracefully.
				cleanupCtx, cleanupCancel := WithStopTimeout(context.Background())
				defer cleanupCancel()
				stopErr := e.stopProcessInternal(
					cleanupCtx,
					handle,
					processStopOptions{opts: optIsResponsibleForStopping},
				)
				if stopErr != nil {
					e.log.Error(stopErr, "Could not stop process during executor disposal", "PID", handle.Pid, "Command", waitable.Info())
				}
			} else {
				// Just make sure we called wait() so the process does not become a zombie.
				_, _ = e.tryStartWaiting(handle, waitable, waitReasonMonitoring)
			}
		}()
	}

	wg.Wait()

	e.completeDispose()
	e.log.V(1).Info("Process executor disposed")
}

func (e *OSExecutor) CheckProcessRunning(handle ProcessHandle) error {
	proc, err := FindProcess(handle)
	if err != nil {
		return err
	}
	if releaseErr := proc.Release(); releaseErr != nil {
		e.log.Error(releaseErr, "Failed to release process handle", "PID", handle.Pid)
	}
	return nil
}

func (e *OSExecutor) FindProcessHandle(pid Pid_t) (ProcessHandle, error) {
	return FindProcessHandle(pid)
}

type processStoppingOpts uint16

type singleProcessStopResult struct {
	waitEndedCh   <-chan struct{}
	forceKillUsed bool
}

const (
	optNone            processStoppingOpts = 0
	optNotFoundIsError processStoppingOpts = 0x1
	optTrySignal       processStoppingOpts = 0x2
	optWaitForStdio    processStoppingOpts = 0x4

	// The caller is responsible for stopping the process, disregard "shouldStopProcess" value returned by tryStartWaiting().
	optIsResponsibleForStopping processStoppingOpts = 0x8

	// When combined with optTrySignal on Windows, sends CTRL_C_EVENT to process group 0
	// (all processes sharing the current console) instead of CTRL_BREAK_EVENT to the specific PID.
	// Only meaningful when the caller has already attached to the target's console
	// via AttachConsole. Has no effect on non-Windows platforms.
	optSignalConsoleGroup processStoppingOpts = 0x10

	// Skips descendant enumeration and cleanup after stopping the root process.
	// Descendants may still receive signals sent to a shared console or process group.
	optSkipDescendants processStoppingOpts = 0x20

	// Attempts graceful stopping without escalating to a force kill. The caller is responsible
	// for force-killing the process later if the graceful-stop context expires.
	optGracefulOnly processStoppingOpts = 0x40

	// Uses the caller's context deadline instead of the per-signal timeout for graceful waiting.
	// Unlike optGracefulOnly, a graceful signal dispatch failure may still fall back to a force kill.
	optWaitForGracefulDeadline processStoppingOpts = 0x80
)

func makeClosedChan() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

var _ Executor = (*OSExecutor)(nil)
