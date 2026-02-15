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

// ProcessHandle is a compound type representing a reference to a process.
// It holds the process ID and its identity time (used to distinguish between
// different instances of processes with the same PID after PID reuse).
//
// The IdentityTime may not be a valid wall-clock time on all platforms; on Linux
// it is expressed as ticks since boot to avoid issues with system clock changes.
//
// ProcessHandle is a value type and is safe to use as a map key.
type ProcessHandle struct {
	Pid          Pid_t
	IdentityTime time.Time
}

// NewProcessHandle creates a ProcessHandle from a PID and an identity time.
func NewProcessHandle(pid Pid_t, identityTime time.Time) ProcessHandle {
	return ProcessHandle{
		Pid:          pid,
		IdentityTime: identityTime,
	}
}

// ProcessHandleFromCmd creates a ProcessHandle from a started exec.Cmd.
// The command must have been started (cmd.Process must be non-nil).
// The identity time is obtained via ProcessIdentityTime for stability across clock changes.
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
// The identity time is obtained via ProcessIdentityTime for stability across clock changes.
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
