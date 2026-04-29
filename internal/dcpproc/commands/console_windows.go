/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package commands

import (
	"time"
	"unsafe"

	"github.com/go-logr/logr"
	"golang.org/x/sys/windows"

	"github.com/microsoft/dcp/pkg/process"
)

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	attachConsoleProc         = kernel32.NewProc("AttachConsole")
	freeConsoleProc           = kernel32.NewProc("FreeConsole")
	getConsoleProcessListProc = kernel32.NewProc("GetConsoleProcessList")
)

// attachToTargetProcessConsole detaches from the current console and attaches to the console
// of the target process. Returns true if attachment was successful, or false if the target
// process already shares the current console, has no console, or has already exited.
// Returns a non-nil error only on unexpected failures.
func attachToTargetProcessConsole(log logr.Logger, targetPid process.Pid_t) (attached bool, err error) {
	if currentConsoleContainsProcess(targetPid) {
		log.Info("Target process is already attached to the current console; using direct process stop to avoid signaling unrelated console processes")
		return false, nil
	}

	retval, _, win32err := freeConsoleProc.Call(0)
	if retval == 0 {
		// https://learn.microsoft.com/windows/console/freeconsole says
		// "If the calling process is not already attached to a console, the FreeConsole request still succeeds."
		// So we do not expect an error here.
		log.Error(win32err, "Could not detach the process from its current console")
		return false, win32err
	}

	retval, _, win32err = attachConsoleProc.Call(uintptr(targetPid))
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

	return true, nil
}

func currentConsoleContainsProcess(targetPid process.Pid_t) bool {
	targetOSPid, pidErr := process.PidT_ToUint32(targetPid)
	if pidErr != nil {
		return false
	}

	processIDs := make([]uint32, 16)
	count, _, _ := getConsoleProcessListProc.Call(
		uintptr(unsafe.Pointer(&processIDs[0])),
		uintptr(len(processIDs)),
	)
	if count == 0 {
		return false
	}
	if count > uintptr(len(processIDs)) {
		processIDs = make([]uint32, int(count))
		count, _, _ = getConsoleProcessListProc.Call(
			uintptr(unsafe.Pointer(&processIDs[0])),
			uintptr(len(processIDs)),
		)
		if count == 0 {
			return false
		}
	}

	for _, consolePID := range processIDs[:int(count)] {
		if consolePID == targetOSPid {
			return true
		}
	}
	return false
}

// stopViaConsole attaches to the target process's console, then stops the process tree.
// If attachment succeeds, it sends CTRL_C_EVENT to the entire console group and protects
// DCP from its own signal.
// If the target already shares the current console, has no console, or has already exited,
// it falls back to a regular StopProcess call.
func stopViaConsole(log logr.Logger, pe process.Executor, pid process.Pid_t, startTime time.Time) error {
	attached, attachErr := attachToTargetProcessConsole(log, pid)
	if attachErr != nil {
		// Error already logged in attachToTargetProcessConsole.
		return attachErr
	}

	if !attached {
		return pe.StopProcess(pid, startTime)
	}

	cgs, ok := pe.(process.ConsoleGroupStopper)
	if !ok {
		// Should not happen in production; fall back gracefully.
		log.Info("Process executor does not support console group stopping; falling back to regular stop")
		return pe.StopProcess(pid, startTime)
	}

	return cgs.StopProcessInConsoleGroup(pid, startTime)
}
