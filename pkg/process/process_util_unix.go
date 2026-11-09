//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"os/exec"
	"syscall"
)

// Use separate process group so this process exit will not affect the children.
func DecoupleFromParent(cmd *exec.Cmd) {
	sysProcAttr := cmd.SysProcAttr
	if sysProcAttr == nil {
		sysProcAttr = &syscall.SysProcAttr{}
	}
	sysProcAttr.Setpgid = true

	cmd.SysProcAttr = sysProcAttr
}

// Use a separate session so this process exit and terminal signals will not affect the children.
func ForkFromParent(cmd *exec.Cmd) {
	sysProcAttr := cmd.SysProcAttr
	if sysProcAttr == nil {
		sysProcAttr = &syscall.SysProcAttr{}
	}
	sysProcAttr.Setsid = true

	cmd.SysProcAttr = sysProcAttr
}
