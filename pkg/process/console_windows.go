//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"golang.org/x/sys/windows"
)

const attachParentProcess = uintptr(^uint32(0))

var (
	attachConsoleProc = kernel32.NewProc("AttachConsole")
	freeConsoleProc   = kernel32.NewProc("FreeConsole")
)

// StopViaConsole attaches to the target process's console, then stops the process tree.
// If attachment succeeds, it sends CTRL_C_EVENT to the entire console group and protects
// the caller from its own signal.
// If the target has no console or has already exited, it falls back to a regular StopProcess call.
func StopViaConsole(log logr.Logger, executor Executor, handle ProcessHandle, options ...ProcessStopOption) error {
	attached, attachErr := attachToTargetProcessConsole(log, handle.Pid)
	if attachErr != nil {
		// Error already logged in attachToTargetProcessConsole. Fall back to direct stop.
		stopErr := executor.StopProcess(handle, options...)
		if stopErr != nil {
			return errors.Join(attachErr, stopErr)
		}
		return nil
	}

	if !attached {
		return executor.StopProcess(handle, options...)
	}
	defer restoreParentConsole(log)

	handlerErr := installIgnoreConsoleCtrlEventHandler()
	if handlerErr != nil {
		return fmt.Errorf("could not install console ctrl handler: %w", handlerErr)
	}
	// No explicit removal: StopViaConsole detaches from the target console,
	// which resets the process control-handler table.

	consoleOptions := make([]ProcessStopOption, 0, len(options)+1)
	consoleOptions = append(consoleOptions, options...)
	consoleOptions = append(consoleOptions, stopConsoleGroup())
	return executor.StopProcess(handle, consoleOptions...)
}

func stopConsoleGroup() ProcessStopOption {
	return func(options *processStopOptions) {
		options.opts |= optSignalConsoleGroup
	}
}

// attachToTargetProcessConsole detaches from the current console and attaches to the console
// of the target process. Returns true if attachment was successful, or false if the target
// process has no console or has already exited.
// Returns a non-nil error only on unexpected failures.
func attachToTargetProcessConsole(log logr.Logger, targetPid Pid_t) (bool, error) {
	targetOSPid, pidErr := PidT_ToUint32(targetPid)
	if pidErr != nil {
		return false, pidErr
	}

	retval, _, win32err := freeConsoleProc.Call()
	if retval == 0 {
		// https://learn.microsoft.com/windows/console/freeconsole says
		// "If the calling process is not already attached to a console, the FreeConsole request still succeeds."
		// So we do not expect an error here.
		log.Error(win32err, "Could not detach the process from its current console")
		return false, win32err
	}

	attached := false
	defer func() {
		if !attached {
			reattachToParentConsole(log)
		}
	}()

	retval, _, win32err = attachConsoleProc.Call(uintptr(targetOSPid))
	if retval == 0 {
		errno, isErrno := win32err.(windows.Errno)
		switch {
		case isErrno && errno == windows.ERROR_INVALID_HANDLE:
			log.Info("The target process does not have a console. It will not be possible to stop it gracefully.")
			return false, nil
		case isErrno && errno == windows.ERROR_INVALID_PARAMETER:
			log.Info("The target process exited before we could attach to its console")
			return false, nil
		default:
			log.Error(win32err, "Could not attach to target process console")
			return false, win32err
		}
	}

	attached = true
	return true, nil
}

func detachFromConsole(log logr.Logger) {
	retval, _, win32err := freeConsoleProc.Call()
	if retval == 0 {
		log.Error(win32err, "Could not detach from console")
	}
}

func reattachToParentConsole(log logr.Logger) {
	retval, _, win32err := attachConsoleProc.Call(attachParentProcess)
	if retval == 0 {
		errno, isErrno := win32err.(windows.Errno)
		switch {
		case isErrno && (errno == windows.ERROR_INVALID_HANDLE || errno == windows.ERROR_INVALID_PARAMETER):
			log.V(1).Info("Parent console is not available; skipping console reattach", "Error", win32err)
		default:
			log.Error(win32err, "Could not reattach to parent console")
		}
	}
}

func restoreParentConsole(log logr.Logger) {
	detachFromConsole(log)
	reattachToParentConsole(log)
}
