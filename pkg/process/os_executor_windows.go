//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"github.com/go-logr/logr"
	"golang.org/x/sys/windows"

	"github.com/microsoft/dcp/pkg/osutil"
)

const (
	// The timeout for sending a signal and waiting for the process to exit.
	signalAndWaitTimeout = 6 * time.Second

	DCP_DISABLE_PROCESS_CLEANUP_JOB = "DCP_DISABLE_PROCESS_CLEANUP_JOB"

	cleanupJobProcessAccess = processInspectionAccess | windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE
	resumeThreadAccess      = windows.THREAD_SUSPEND_RESUME
)

var (
	cleanupJobDisabled = sync.OnceValue(func() bool { return osutil.EnvVarSwitchEnabled(DCP_DISABLE_PROCESS_CLEANUP_JOB) })

	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	kernel32SetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")

	ignoreConsoleCtrlEventsCallback = windows.NewCallback(ignoreConsoleCtrlEvent)
)

type OSExecutor struct {
	*osExecutorBase
	processCleanupJob         func() windows.Handle
	processCleanupJobCreated  bool
	processCleanupJobErr      error
	openProcessForCleanupJob  func(uint32, bool, uint32) (windows.Handle, error)
	assignProcessToCleanupJob func(windows.Handle, windows.Handle) error
}

func NewOSExecutor(log logr.Logger) Executor {
	e := &OSExecutor{
		osExecutorBase:            newOSExecutorBase(log),
		openProcessForCleanupJob:  windows.OpenProcess,
		assignProcessToCleanupJob: windows.AssignProcessToJobObject,
	}
	e.processCleanupJob = sync.OnceValue(func() windows.Handle {
		e.processCleanupJobCreated = true
		job, jobErr := e.createProcessCleanupJob()
		e.processCleanupJobErr = jobErr
		return job
	})
	return e
}

func (e *OSExecutor) getWindowsConsoleAvailability(ws *waitState) WindowsConsoleAvailability {
	e.acquireLock()
	defer e.releaseLock()
	return ws.winConsoleAvailability
}

func (e *OSExecutor) stopSingleProcess(ctx context.Context, handle ProcessHandle, opts processStoppingOpts) (singleProcessStopResult, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return singleProcessStopResult{}, contextErr
	}
	proc, err := handle.OsProcess()
	if err != nil {
		if !IsProcessGoneErr(err) {
			return singleProcessStopResult{}, err
		}
		e.acquireLock()
		alreadyEnded := false
		ws, found := e.procsWaiting[handle]
		if found {
			alreadyEnded = !ws.waitEnded.IsZero()
		}
		e.releaseLock()

		if (opts&optNotFoundIsError) != 0 && !alreadyEnded {
			return singleProcessStopResult{}, &ErrProcessNotFound{Pid: handle.Pid, Inner: err}
		} else {
			return singleProcessStopResult{waitEndedCh: makeClosedChan()}, nil
		}
	}

	defer func() {
		if releaseErr := proc.Release(); releaseErr != nil {
			e.log.Error(releaseErr, "Could not release process reference", "PID", handle.Pid)
		}
	}()
	waitable := makeProcessWaitable(e.lifetimeCtx, handle, fixedWaitPollPolicy(defaultWaitPollInterval))
	ws, shouldStopProcess := e.tryStartWaiting(handle, waitable, waitReasonStopping)

	waitEndedCh := ws.waitEndedCh
	if opts&optWaitForStdio == 0 {
		waitEndedCh = makeClosedChan()
	}

	if !shouldStopProcess && (opts&optIsResponsibleForStopping) == 0 {
		if (opts & optWaitForStdio) == 0 {
			// Another stop owns signaling, but descendants must still be confirmed exited.
			waitErr := waitForTrackedProcessExit(ctx, handle.Pid, ws, 0)
			return singleProcessStopResult{waitEndedCh: waitEndedCh}, waitErr
		}
		return singleProcessStopResult{waitEndedCh: waitEndedCh}, nil
	}
	defer e.finishStopAttempt(ws)

	if (opts & optTrySignal) != 0 {
		winConsoleAvailability := e.getWindowsConsoleAvailability(ws)
		consoleEvent, processGroupID, shouldSendEvent := consoleControlEventForStop(
			winConsoleAvailability,
			opts,
			proc.Pid,
		)
		deliveryConfirmed := false
		var dispatchErr error
		if shouldSendEvent {
			// StopViaConsole first attaches the helper to a foreign classic console and then sends
			// CTRL_C_EVENT to process group zero. A normal executor-owned process that inherited
			// this executor's classic console instead receives CTRL_BREAK_EVENT for the new process
			// group created at launch. A process in another console, a ConPTY process, a detached
			// process, or an adopted process with unknown launch topology may not receive either
			// event, so those scenarios do not attempt a direct dispatch here.
			dispatchErr = e.sendConsoleControlEvent(ctx, handle, proc, consoleEvent, processGroupID)
			if IsProcessGoneErr(dispatchErr) {
				return singleProcessStopResult{waitEndedCh: waitEndedCh}, nil
			}
			deliveryConfirmed = dispatchErr == nil
		}

		// A successful GenerateConsoleCtrlEvent call confirms dispatch of CTRL_C_EVENT or
		// CTRL_BREAK_EVENT, so the process may use the full shared 15-second graceful deadline.
		// Without that confirmation, wait passively for at most six seconds: the target may be
		// in a foreign/new console without an attach helper, attached to ConPTY, detached from
		// every console, or an unknown descendant that never received the root's console event.
		waitTimeout := consoleControlWaitTimeout(deliveryConfirmed, opts)
		gracefulWaitErr := waitForTrackedProcessExit(ctx, handle.Pid, ws, waitTimeout)
		if gracefulWaitErr == nil {
			if deliveryConfirmed {
				e.log.V(1).Info("Process stopped after confirmed console control event",
					"PID", handle.Pid, "Event", consoleControlEventName(consoleEvent))
			} else {
				e.log.V(1).Info("Process exited during passive console-control fallback", "PID", handle.Pid)
			}
			return singleProcessStopResult{waitEndedCh: waitEndedCh}, nil
		}
		if IsProcessGoneErr(e.CheckProcessRunning(handle)) {
			e.log.V(1).Info("Process exited while its wait operation was still completing", "PID", handle.Pid)
			return singleProcessStopResult{waitEndedCh: waitEndedCh}, nil
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return singleProcessStopResult{waitEndedCh: waitEndedCh}, errors.Join(dispatchErr, contextErr)
		}
		if (opts & optGracefulOnly) != 0 {
			return singleProcessStopResult{waitEndedCh: waitEndedCh}, errors.Join(dispatchErr, gracefulWaitErr)
		}

		if deliveryConfirmed {
			e.log.V(1).Info("Process did not exit after the confirmed console control event; force-killing",
				"PID", handle.Pid, "Event", consoleControlEventName(consoleEvent), "Error", gracefulWaitErr)
		} else {
			e.log.V(1).Info("Process did not exit during the six-second passive fallback; force-killing",
				"PID", handle.Pid, "DispatchError", dispatchErr, "WaitError", gracefulWaitErr)
		}
	} else if (opts & optGracefulOnly) != 0 {
		// No console control event was sent to this process. This is the descendant path when
		// receipt of the root's CTRL_C_EVENT or CTRL_BREAK_EVENT cannot be established, so wait
		// passively for six seconds rather than consuming the full 15-second graceful deadline.
		gracefulWaitErr := waitForTrackedProcessExit(ctx, handle.Pid, ws, signalAndWaitTimeout)
		if gracefulWaitErr == nil {
			e.log.V(1).Info("Process exited during passive console-control fallback", "PID", handle.Pid)
			return singleProcessStopResult{waitEndedCh: waitEndedCh}, nil
		}
		return singleProcessStopResult{waitEndedCh: waitEndedCh}, gracefulWaitErr
	}

	// Force escalation starts only after either (a) a confirmed CTRL_C_EVENT/CTRL_BREAK_EVENT
	// consumed the allowed graceful deadline, or (b) event delivery could not be confirmed and
	// the six-second passive fallback elapsed. This prevents foreign-console, ConPTY, detached,
	// no-console, and unknown-descendant cases from waiting the full 15 seconds without evidence
	// that a graceful console event reached the target.
	e.log.V(1).Info("Sending SIGKILL to process...", "PID", handle.Pid)
	e.markForceKillUsed(ws)
	err = signalProcess(ctx, handle, proc, os.Kill)
	if err != nil && !IsProcessGoneErr(err) {
		return singleProcessStopResult{waitEndedCh: waitEndedCh, forceKillUsed: true}, err
	}
	if (opts & optWaitForStdio) == 0 {
		waitErr := waitForTrackedProcessExit(ctx, handle.Pid, ws, 0)
		if waitErr != nil {
			return singleProcessStopResult{waitEndedCh: waitEndedCh, forceKillUsed: true}, waitErr
		}
	}

	e.log.V(1).Info("Process stopped by SIGKILL", "PID", handle.Pid)
	return singleProcessStopResult{waitEndedCh: waitEndedCh, forceKillUsed: true}, nil
}

func consoleControlEventForStop(
	winConsoleAvailability WindowsConsoleAvailability,
	opts processStoppingOpts,
	pid int,
) (consoleEvent uint32, processGroupID uint32, shouldSend bool) {
	if (opts & optSignalConsoleGroup) != 0 {
		return windows.CTRL_C_EVENT, 0, true
	}
	if winConsoleAvailability == WindowsConsoleAvailabilityInherited {
		return windows.CTRL_BREAK_EVENT, uint32(pid), true
	}
	return 0, 0, false
}

func consoleControlWaitTimeout(deliveryConfirmed bool, opts processStoppingOpts) time.Duration {
	if deliveryConfirmed && (opts&(optGracefulOnly|optWaitForGracefulDeadline)) != 0 {
		return 0
	}
	return signalAndWaitTimeout
}

func consoleControlEventName(consoleEvent uint32) string {
	switch consoleEvent {
	case windows.CTRL_C_EVENT:
		return "CTRL_C_EVENT"
	case windows.CTRL_BREAK_EVENT:
		return "CTRL_BREAK_EVENT"
	default:
		return fmt.Sprintf("console event %d", consoleEvent)
	}
}

// sendConsoleControlEvent revalidates the process identity immediately before dispatch.
// processGroupID zero sends CTRL_C_EVENT to every process attached to the helper's current
// console; a nonzero ID sends CTRL_BREAK_EVENT only to that classic-console process group.
func (e *OSExecutor) sendConsoleControlEvent(
	ctx context.Context,
	handle ProcessHandle,
	proc *os.Process,
	consoleEvent uint32,
	processGroupID uint32,
) error {
	err := actOnProcess(ctx, handle, func() (ProcessHandle, error) {
		info, infoErr := readProcessInfoFromProcess(proc, false)
		return info.handle, infoErr
	}, func() error {
		return windows.GenerateConsoleCtrlEvent(consoleEvent, processGroupID)
	})
	if err != nil {
		return fmt.Errorf("could not send %s to process %d: %w",
			consoleControlEventName(consoleEvent), proc.Pid, err)
	}
	return nil
}
func (e *OSExecutor) completeDispose() {
	e.acquireLock()
	defer e.releaseLock()
	if !e.processCleanupJobCreated {
		return
	}

	pcj := e.processCleanupJob()
	if pcj != windows.InvalidHandle {
		// Close the job handle to ensure that all processes in the job are terminated.
		err := windows.CloseHandle(pcj)
		if err != nil {
			e.log.Error(err, "Could not close process cleanup job handle")
		} else {
			e.log.V(1).Info("Process cleanup job handle closed; all associated processes were terminated.")
		}
	}

	e.processCleanupJob = func() windows.Handle { return windows.InvalidHandle }
}

func (e *OSExecutor) prepareProcessStart(cmd *exec.Cmd, flags ProcessCreationFlag) {
	// On Windows, we need to decouple the process from the parent to ensure we can
	// send CTRL_BREAK_EVENT to the child without impacting the parent.
	DecoupleFromParent(cmd)

	if !cleanupJobDisabled() && (flags&CreationFlagEnsureKillOnDispose) == CreationFlagEnsureKillOnDispose {
		// cmd.SysProcAttr is allocated already because we called DecoupleFromParent()
		cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	}
}

func windowsConsoleAvailabilityForCmd(cmd *exec.Cmd) WindowsConsoleAvailability {
	if cmd == nil || cmd.SysProcAttr == nil {
		return WindowsConsoleAvailabilityUnknown
	}

	creationFlags := cmd.SysProcAttr.CreationFlags
	switch {
	case creationFlags&windows.CREATE_NEW_CONSOLE != 0:
		// CREATE_NEW_CONSOLE places the process in a foreign console. The long-lived
		// executor cannot send CTRL_BREAK_EVENT across that console boundary; an isolated
		// helper must AttachConsole and then send CTRL_C_EVENT to process group zero.
		return WindowsConsoleAvailabilityRequiresAttach
	case creationFlags&(windows.DETACHED_PROCESS|windows.CREATE_NO_WINDOW) != 0:
		// Detached and CREATE_NO_WINDOW processes have no classic console. CTRL_C_EVENT
		// and CTRL_BREAK_EVENT therefore cannot reach them; ConPTY creators report the
		// same availability through WindowsConsoleAvailabilitySource.
		return WindowsConsoleAvailabilityUnavailable
	case creationFlags&windows.CREATE_NEW_PROCESS_GROUP != 0:
		// The process inherited this executor's classic console and its PID names the new
		// process group, so a successful CTRL_BREAK_EVENT dispatch confirms delivery.
		return WindowsConsoleAvailabilityInherited
	default:
		return WindowsConsoleAvailabilityUnknown
	}
}

func (e *OSExecutor) completeProcessStart(handle ProcessHandle, flags ProcessCreationFlag) error {
	if cleanupJobDisabled() || (flags&CreationFlagEnsureKillOnDispose) == 0 {
		return nil
	}

	e.acquireLock()
	defer e.releaseLock()

	pcj := e.processCleanupJob()
	if pcj == windows.InvalidHandle {
		if e.processCleanupJobErr != nil {
			return fmt.Errorf("could not establish process cleanup job for pid %d: %w",
				handle.Pid, e.processCleanupJobErr)
		}
		return fmt.Errorf("process cleanup job is unavailable for pid %d", handle.Pid)
	}

	// CreationFlagEnsureKillOnDispose starts the process suspended. Do not resume it unless
	// identity-validated job assignment succeeds: resuming after OpenProcess or
	// AssignProcessToJobObject fails would leave a running process outside cleanup ownership.
	// AssignProcessToJobObject requires PROCESS_SET_QUOTA and PROCESS_TERMINATE; inspection
	// rights are also needed to verify that PID reuse did not replace the suspended process.
	processHandle, processHandleErr := e.openProcessForCleanupJob(
		cleanupJobProcessAccess,
		false,
		uint32(handle.Pid),
	)
	if processHandleErr != nil {
		return fmt.Errorf("could not open process %d for cleanup job assignment: %w",
			handle.Pid, processHandleErr)
	}
	defer tryCloseHandle(processHandle)

	info, infoErr := readWindowsProcessInfo(processHandle, false)
	if infoErr != nil {
		return fmt.Errorf("could not inspect process %d for cleanup job assignment: %w",
			handle.Pid, infoErr)
	}
	if identityErr := validateIdentity(handle, info.handle); identityErr != nil {
		return fmt.Errorf("could not validate process %d for cleanup job assignment: %w",
			handle.Pid, identityErr)
	}

	jobAssignmentErr := e.assignProcessToCleanupJob(pcj, processHandle)
	if jobAssignmentErr != nil {
		return fmt.Errorf("could not assign process %d to cleanup job: %w",
			handle.Pid, jobAssignmentErr)
	}

	resumptionErr := resumeNewSuspendedProcess(handle)
	if resumptionErr != nil {
		e.log.Error(resumptionErr, "Could not resume new suspended process", "PID", handle.Pid)
		return fmt.Errorf("could not resume new suspended process with pid %d: %w", handle.Pid, resumptionErr)
	}

	return nil
}

func (e *OSExecutor) createProcessCleanupJob() (windows.Handle, error) {
	if cleanupJobDisabled() {
		return windows.InvalidHandle, nil
	}

	job, jobCreationErr := windows.CreateJobObject(nil, nil)
	if jobCreationErr != nil {
		e.log.Error(jobCreationErr, "Could not create process cleanup job")
		return windows.InvalidHandle, fmt.Errorf("could not create process cleanup job: %w", jobCreationErr)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}

	_, setJobInfoErr := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if setJobInfoErr != nil {
		e.log.Error(setJobInfoErr, "Could not set process cleanup job information")
		tryCloseHandle(job)
		return windows.InvalidHandle, fmt.Errorf("could not configure process cleanup job: %w", setJobInfoErr)
	}

	return job, nil
}

func resumeNewSuspendedProcess(handle ProcessHandle) error {
	proc, findErr := handle.OsProcess()
	if findErr != nil {
		return findErr
	}
	defer func() { _ = proc.Release() }()
	pid := uint32(handle.Pid)
	snapshot, snapshotErr := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, pid)
	if snapshotErr != nil {
		return fmt.Errorf("could not create thread snapshot for pid %d: %w", pid, snapshotErr)
	}
	defer tryCloseHandle(snapshot)

	var threadEntry windows.ThreadEntry32
	threadEntry.Size = uint32(unsafe.Sizeof(threadEntry))

	enumErr := windows.Thread32First(snapshot, &threadEntry)
	for enumErr == nil {
		if threadEntry.OwnerProcessID == pid {
			// Found the primary (only) thread of the new, suspended process.
			break
		}
		enumErr = windows.Thread32Next(snapshot, &threadEntry)
	}
	if enumErr != nil {
		return fmt.Errorf("could not enumerate threads for pid %d: %w", pid, enumErr)
	}

	primaryThreadId := threadEntry.ThreadID
	hThread, threadOpenErr := windows.OpenThread(resumeThreadAccess, false, primaryThreadId)
	if threadOpenErr != nil {
		return fmt.Errorf("could not open primary thread for pid %d: %w", pid, threadOpenErr)
	}
	defer tryCloseHandle(hThread)

	if identityErr := checkProcessIdentity(handle, proc); identityErr != nil {
		return identityErr
	}
	_, resumeErr := windows.ResumeThread(hThread)
	if resumeErr != nil {
		return fmt.Errorf("could not resume primary thread for pid %d: %w", pid, resumeErr)
	}

	return nil
}

func tryCloseHandle(handle windows.Handle) {
	if handle != windows.InvalidHandle {
		_ = windows.CloseHandle(handle)
	}
}

func ignoreConsoleCtrlEvent(ctrlType uint32) uintptr {
	switch ctrlType {
	case windows.CTRL_C_EVENT, windows.CTRL_BREAK_EVENT:
		return 1
	default:
		return 0
	}
}

func installIgnoreConsoleCtrlEventHandler() error {
	retval, _, win32err := kernel32SetConsoleCtrlHandler.Call(ignoreConsoleCtrlEventsCallback, 1)
	if retval == 0 {
		return win32err
	}
	return nil
}
