/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/microsoft/dcp/pkg/osutil"
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

// ResolveProcessHandle preserves and validates a supplied identity, or captures the current identity when omitted.
func ResolveProcessHandle(pid Pid_t, identityTime time.Time) (ProcessHandle, error) {
	if identityTime.IsZero() {
		return FindProcessHandle(pid)
	}
	handle := NewHandle(pid, identityTime)
	return handle, handle.Validate()
}

// FindProcessHandle resolves a PID once into the identity of the process currently using it.
func FindProcessHandle(pid Pid_t) (ProcessHandle, error) {
	if pidErr := validateProcessID(pid); pidErr != nil {
		return ProcessHandle{Pid: UnknownPID}, pidErr
	}
	info, infoErr := readProcessInfo(pid)
	if infoErr != nil {
		return ProcessHandle{Pid: UnknownPID}, infoErr
	}
	if info.exited {
		return ProcessHandle{Pid: UnknownPID}, &ErrProcessNotFound{Pid: pid}
	}
	if identityErr := validateIdentity(info.handle, info.handle); identityErr != nil {
		return ProcessHandle{Pid: UnknownPID}, identityErr
	}
	return info.handle, nil
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

// WallClockStartTime converts the captured process identity to a wall-clock start time.
// It does not require the process to still be running.
// On Linux, the boot-time offset is captured once and reused by subsequent conversions.
func (handle ProcessHandle) WallClockStartTime() (time.Time, error) {
	if handleErr := handle.Validate(); handleErr != nil {
		return time.Time{}, handleErr
	}
	return processDisplayTime(handle)
}

// OsProcess acquires and validates a process reference. The caller must wait on it or release it.
func (handle ProcessHandle) OsProcess() (*os.Process, error) {
	if handleErr := handle.Validate(); handleErr != nil {
		return nil, handleErr
	}
	pid, pidErr := PidT_ToInt(handle.Pid)
	if pidErr != nil {
		return nil, pidErr
	}
	proc, findErr := os.FindProcess(pid)
	if findErr != nil {
		return nil, processLookupError(handle.Pid, findErr)
	}
	if identityErr := checkProcessIdentity(handle, proc); identityErr != nil {
		return nil, errors.Join(identityErr, proc.Release())
	}
	return proc, nil
}

func (handle ProcessHandle) findProcessInfo() (processInfo, error) {
	if handleErr := handle.Validate(); handleErr != nil {
		return processInfo{}, handleErr
	}
	info, infoErr := readProcessInfo(handle.Pid)
	if infoErr != nil {
		return processInfo{}, infoErr
	}
	if identityErr := validateIdentity(handle, info.handle); identityErr != nil {
		return processInfo{}, identityErr
	}
	if info.exited {
		return processInfo{}, &ErrProcessNotFound{Pid: handle.Pid}
	}
	return info, nil
}

func checkProcessIdentity(handle ProcessHandle, proc *os.Process) error {
	if handleErr := handle.Validate(); handleErr != nil {
		return handleErr
	}
	info, infoErr := readProcessInfoFromProcess(proc, false)
	if infoErr != nil {
		return infoErr
	}
	return validateIdentity(handle, info.handle)
}

func validateIdentity(expected ProcessHandle, actual ProcessHandle) error {
	if actual.IdentityTime.IsZero() {
		return fmt.Errorf("%w for pid %d", ErrProcessIdentityUnavailable, actual.Pid)
	}
	if handleErr := expected.Validate(); handleErr != nil {
		return handleErr
	}
	if expected.Pid != actual.Pid || !osutil.Within(expected.IdentityTime, actual.IdentityTime, ProcessIdentityTimeMaximumDifference) {
		return fmt.Errorf("%w: pid %d, expected %s, actual %s",
			ErrProcessIdentityMismatch, expected.Pid,
			FormatIdentityTime(expected.IdentityTime), FormatIdentityTime(actual.IdentityTime))
	}
	return nil
}
