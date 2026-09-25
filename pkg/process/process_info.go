/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/microsoft/dcp/pkg/osutil"
)

type processInfo struct {
	handle    ProcessHandle
	parentPID Pid_t
	birth     uint64
	exited    bool
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

// ProcessIdentityTime returns the stable identity timestamp, not a display timestamp.
func ProcessIdentityTime(pid Pid_t) (time.Time, error) {
	handle, handleErr := FindProcessHandle(pid)
	if handleErr != nil {
		return time.Time{}, handleErr
	}
	return handle.IdentityTime, nil
}

// StartTimeForProcess converts a captured process identity to a wall-clock display time.
// It does not require the process to still be running.
func StartTimeForProcess(handle ProcessHandle) (time.Time, error) {
	if handleErr := handle.Validate(); handleErr != nil {
		return time.Time{}, handleErr
	}
	return processDisplayTime(processInfo{handle: handle})
}

// FindProcess acquires and validates a process reference. The caller must wait on it or release it.
func FindProcess(handle ProcessHandle) (*os.Process, error) {
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

func findProcessInfo(handle ProcessHandle) (processInfo, error) {
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

func validateProcessID(pid Pid_t) error {
	if _, pidErr := PidT_ToUint32(pid); pidErr != nil || pid == 0 {
		return fmt.Errorf("%w: invalid pid %d", ErrInvalidProcessHandle, pid)
	}
	return nil
}

func processLookupError(pid Pid_t, lookupErr error) error {
	if errors.Is(lookupErr, os.ErrNotExist) || nativeProcessNotFound(lookupErr) {
		return &ErrProcessNotFound{Pid: pid, Inner: lookupErr}
	}
	return fmt.Errorf("could not inspect process %d: %w", pid, lookupErr)
}
