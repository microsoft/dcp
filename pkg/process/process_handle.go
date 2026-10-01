/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"fmt"
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

// NewHandle creates a value; operations require a positive PID and nonzero identity time.
func NewHandle(pid Pid_t, identityTime time.Time) ProcessHandle {
	return ProcessHandle{
		Pid:          pid,
		IdentityTime: identityTime,
	}
}

// Validate checks whether the handle contains a usable process identity.
func (handle ProcessHandle) Validate() error {
	if _, pidErr := PidT_ToUint32(handle.Pid); pidErr != nil || handle.Pid == 0 {
		return fmt.Errorf("%w: invalid pid %d", ErrInvalidProcessHandle, handle.Pid)
	}
	if handle.IdentityTime.IsZero() {
		return fmt.Errorf("%w: identity time is missing for pid %d", ErrInvalidProcessHandle, handle.Pid)
	}
	return nil
}

// ProcessHandleFromCmd creates a ProcessHandle from a started exec.Cmd.
func ProcessHandleFromCmd(cmd *exec.Cmd) (ProcessHandle, error) {
	if cmd == nil {
		return ProcessHandle{Pid: UnknownPID}, fmt.Errorf("%w: command is nil", ErrInvalidProcessHandle)
	}
	return ProcessHandleFromProcess(cmd.Process)
}

// ProcessHandleFromProcess captures identity before the owned process is waited on or released.
func ProcessHandleFromProcess(p *os.Process) (ProcessHandle, error) {
	if p == nil {
		return ProcessHandle{Pid: UnknownPID}, fmt.Errorf("%w: process is nil", ErrInvalidProcessHandle)
	}
	info, infoErr := readProcessInfoFromProcess(p, true)
	if infoErr != nil {
		return ProcessHandle{Pid: UnknownPID}, infoErr
	}
	return info.handle, info.handle.Validate()
}
