//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureWslcCommand(cmd *exec.Cmd) {
	// WSLC handles console interrupts, including broadcasts intended for unrelated processes.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_CONSOLE,
		HideWindow:    true,
	}
}
