/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"os"
	"os/exec"
	"time"
)

// ProcessHandle identifies a process instance by PID and identity time.
//
// IdentityTime may not be a valid wall-clock time on all platforms. On Linux it
// is expressed as milliseconds since boot to avoid issues with system clock changes.
//
// ProcessHandle is comparable and can be used as a map key for handles created
// from the same identity time source.
type ProcessHandle struct {
	Pid          Pid_t
	IdentityTime time.Time
}

// NewHandle creates a ProcessHandle from a PID and identity time.
func NewHandle(pid Pid_t, identityTime time.Time) ProcessHandle {
	return ProcessHandle{
		Pid:          pid,
		IdentityTime: identityTime,
	}
}

// ProcessHandleFromCmd creates a ProcessHandle from a started exec.Cmd.
func ProcessHandleFromCmd(cmd *exec.Cmd) ProcessHandle {
	if cmd.Process == nil {
		return ProcessHandle{Pid: UnknownPID}
	}

	pid := Uint32_ToPidT(uint32(cmd.Process.Pid))
	return ProcessHandle{
		Pid:          pid,
		IdentityTime: ProcessIdentityTime(pid),
	}
}

// ProcessHandleFromProcess creates a ProcessHandle from a running os.Process.
func ProcessHandleFromProcess(p *os.Process) ProcessHandle {
	if p == nil {
		return ProcessHandle{Pid: UnknownPID}
	}

	pid := Uint32_ToPidT(uint32(p.Pid))
	return ProcessHandle{
		Pid:          pid,
		IdentityTime: ProcessIdentityTime(pid),
	}
}
