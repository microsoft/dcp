//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Copyright (c) Microsoft Corporation. All rights reserved.

package process

import (
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
)

var (
	cleanupJobDisabled = sync.OnceValue(func() bool { return osutil.EnvVarSwitchEnabled(DCP_DISABLE_PROCESS_CLEANUP_JOB) })
)

type OSExecutor struct {
	procsWaiting      map[ProcessHandle]*waitState
	lock              sync.Locker
	disposed          bool
	log               logr.Logger
	processCleanupJob func() windows.Handle
}

func NewOSExecutor(log logr.Logger) Executor {
	e := &OSExecutor{
		procsWaiting: make(map[ProcessHandle]*waitState),
		lock:         &sync.Mutex{},
		disposed:     false,
		log:          log.WithName("os-executor"),
	}
	e.processCleanupJob = sync.OnceValue(func() windows.Handle { return e.createProcessCleanupJob() })
	return e
}

func (e *OSExecutor) stopSingleProcess(handle ProcessHandle, opts processStoppingOpts) (<-chan struct{}, error) {
	proc, err := FindProcess(handle)
	if err != nil {
		e.acquireLock()
		alreadyEnded := false
		ws, found := e.procsWaiting[handle]
		if found {
			alreadyEnded = !ws.waitEnded.IsZero()
		}
		e.releaseLock()

		if (opts&optNotFoundIsError) != 0 && !alreadyEnded {
			return nil, ErrProcessNotFound{Pid: handle.Pid, Inner: err}
		} else {
			return makeClosedChan(), nil
		}
	}

	waitable := makeWaitable(handle.Pid, proc)
	ws, shouldStopProcess := e.tryStartWaiting(handle, waitable, waitReasonStopping)

	waitEndedCh := ws.waitEndedCh
	if opts&optWaitForStdio == 0 {
		waitEndedCh = makeClosedChan()
	}

	if !shouldStopProcess && (opts&optIsResponsibleForStopping) == 0 {
		return waitEndedCh, nil
	}

	if (opts & optTrySignal) == optTrySignal {
		// Give the process a chance to gracefully exit.
		// The only signal we can send on Windows is CTRL_BREAK_EVENT.
		err = e.signalAndWaitForExit(proc, windows.CTRL_BREAK_EVENT, ws)
		switch {
		case err == nil:
			e.log.V(1).Info("Process stopped by CTRL_BREAK_EVENT", "PID", handle.Pid)
			return waitEndedCh, nil
		case !errors.Is(err, ErrTimedOutWaitingForProcessToStop):
			return nil, err
		default:
			e.log.V(1).Info("Process did not stop upon CTRL_BREAK_EVENT", "PID", handle.Pid)
		}
	}

	e.log.V(1).Info("Sending SIGKILL to process...", "PID", handle.Pid)
	err = proc.Kill()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return nil, err
	}

	e.log.V(1).Info("Process stopped by SIGKILL", "PID", handle.Pid)
	return waitEndedCh, nil
}

// Sends a given signal to a process and waits for it to exit.
// If the process does not exit within 6 seconds, the function returns context.DeadlineExceeded.
func (e *OSExecutor) signalAndWaitForExit(proc *os.Process, sig uint32, ws *waitState) error {
	err := windows.GenerateConsoleCtrlEvent(sig, uint32(proc.Pid))
	if err != nil {
		return fmt.Errorf("could not send signal to process %d: %w", proc.Pid, err)
	}

	select {

	case <-ws.waitEndedCh:
		err = ws.waitErr
		if err == nil || IsEarlyProcessExitError(err) {
			// No error or the process exited successfully.
			return nil
		}

		return fmt.Errorf("could not wait for process %d to exit: %w", proc.Pid, err)

	case <-time.After(signalAndWaitTimeout):
		return ErrTimedOutWaitingForProcessToStop
	}
}

func (e *OSExecutor) completeDispose() {
	e.acquireLock()
	defer e.releaseLock()

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

func (e *OSExecutor) completeProcessStart(_ *exec.Cmd, handle ProcessHandle, flags ProcessCreationFlag) error {
	if cleanupJobDisabled() || (flags&CreationFlagEnsureKillOnDispose) == 0 {
		return nil
	}

	e.acquireLock()
	defer e.releaseLock()

	pcj := e.processCleanupJob()
	if pcj != windows.InvalidHandle {
		// We will try to assign the process to the job object. If we fail, this is not a fatal error,
		// The process or its children may not be cleaned up when the process executor is disposed, but it will run.

		// Unfortunately, even though internally the running process exec.Cmd.Process has the process handle,
		// it is not documented, nor publicly accessible.
		// The AssignProcessToJobObject docs say PROCESS_TERMINATE and PROCESS_SET_QUOTA are sufficient to assign a process to a job object,
		// but in practice we need PROCESS_ALL_ACCESS to make it work.
		const access = windows.PROCESS_ALL_ACCESS
		processHandle, processHandleErr := windows.OpenProcess(access, false, uint32(handle.Pid))
		if processHandleErr != nil {
			e.log.V(1).Info("Could not open new process handle", "PID", handle.Pid, "Error", processHandleErr)
		} else {
			defer tryCloseHandle(processHandle)

			// Ideally we would assign the process to the job on process start, but the required access to STARTUPINFOEX structure
			// is not available via the exec.Cmd interface as of Go 1.24.3. We would need to completely re-implement
			// the process start logic and given that we only use the job for process termination on dispose, it is simply not worth the effort.

			jobAssignmentErr := windows.AssignProcessToJobObject(pcj, processHandle)
			if jobAssignmentErr != nil {
				e.log.V(1).Info("Could not assign process to job object", "PID", handle.Pid, "Error", jobAssignmentErr)
			}
		}
	}

	resumptionErr := resumeNewSuspendedProcess(uint32(handle.Pid))
	if resumptionErr != nil {
		e.log.Error(resumptionErr, "Could not resume new suspended process", "PID", handle.Pid)
		return fmt.Errorf("could not resume new suspended process with pid %d: %w", handle.Pid, resumptionErr)
	}

	return nil
}

func (e *OSExecutor) createProcessCleanupJob() windows.Handle {
	if cleanupJobDisabled() {
		return windows.InvalidHandle
	}

	job, jobCreationErr := windows.CreateJobObject(nil, nil)
	if jobCreationErr != nil {
		e.log.Error(jobCreationErr, "Could not create process cleanup job")
		return windows.InvalidHandle
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
		return windows.InvalidHandle
	}

	return job
}

func resumeNewSuspendedProcess(pid uint32) error {
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
	hThread, theadErr := windows.OpenThread(windows.PROCESS_ALL_ACCESS, false, primaryThreadId)
	if theadErr != nil {
		return fmt.Errorf("could not open primary thread for pid %d: %w", pid, theadErr)
	}
	defer tryCloseHandle(hThread)

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
