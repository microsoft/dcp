//go:build windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package commands

import (
	"github.com/go-logr/logr"
	"golang.org/x/sys/windows"

	"github.com/microsoft/dcp/pkg/process"
)

func attachToTargetProcessConsole(log logr.Logger, targetPid process.Pid_t) error {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	freeConsoleProc := kernel32.NewProc("FreeConsole")
	retval, _, win32err := freeConsoleProc.Call(0)
	if retval == 0 {
		// https://learn.microsoft.com/windows/console/freeconsole says
		// "If the calling process is not already attached to a console, the FreeConsole request still succeeds."
		// So we do not expect an error here.
		log.Error(win32err, "Could not detach the process from its current console")
		return win32err
	}

	attachConsoleProc := kernel32.NewProc("AttachConsole")
	retval, _, win32err = attachConsoleProc.Call(uintptr(targetPid))
	if retval == 0 {
		errno, isErrno := win32err.(windows.Errno)
		switch {
		case isErrno && errno == windows.ERROR_INVALID_HANDLE:
			log.Info("The target process does not have a console. It will not be possible to stop it gracefully.")
			return nil
		case isErrno && errno == windows.ERROR_INVALID_PARAMETER:
			log.Info("The target process exited before we could attach to its console")
			return nil
		default:
			log.Error(win32err, "Could not attach to target process console")
			return win32err
		}
	}

	return nil
}
