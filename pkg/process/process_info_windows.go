//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const processInspectionAccess = windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE

func nativeProcessNotFound(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
}

func readProcessInfo(pid Pid_t) (info processInfo, returnErr error) {
	nativeHandle, openErr := windows.OpenProcess(processInspectionAccess, false, uint32(pid))
	if openErr != nil {
		if errors.Is(openErr, windows.ERROR_INVALID_PARAMETER) {
			return processInfo{}, &ErrProcessNotFound{Pid: pid, Inner: openErr}
		}
		return processInfo{}, processLookupError(pid, openErr)
	}
	defer func() { returnErr = errors.Join(returnErr, windows.CloseHandle(nativeHandle)) }()
	return readWindowsProcessInfo(nativeHandle, false)
}

func readProcessInfoFromProcess(proc *os.Process, allowExited bool) (processInfo, error) {
	var info processInfo
	var infoErr error
	handleErr := proc.WithHandle(func(nativeHandle uintptr) {
		info, infoErr = readWindowsProcessInfo(windows.Handle(nativeHandle), allowExited)
	})
	return info, errors.Join(handleErr, infoErr)
}

// ProcessHandleFromNativeHandle captures identity while the caller still owns the Windows process handle.
func ProcessHandleFromNativeHandle(nativeHandle windows.Handle) (ProcessHandle, error) {
	info, infoErr := readWindowsProcessInfo(nativeHandle, true)
	if infoErr != nil {
		return ProcessHandle{Pid: UnknownPID}, infoErr
	}
	return info.handle, info.handle.Validate()
}

func readWindowsProcessInfo(nativeHandle windows.Handle, allowExited bool) (processInfo, error) {
	pid, pidErr := windows.GetProcessId(nativeHandle)
	if pidErr != nil {
		return processInfo{}, fmt.Errorf("could not read process id: %w", pidErr)
	}
	if !allowExited {
		state, waitErr := windows.WaitForSingleObject(nativeHandle, 0)
		if waitErr != nil {
			return processInfo{}, fmt.Errorf("could not inspect process %d state: %w", pid, waitErr)
		}
		if state == windows.WAIT_OBJECT_0 {
			return processInfo{}, &ErrProcessNotFound{Pid: Uint32_ToPidT(pid)}
		}
		if state != uint32(windows.WAIT_TIMEOUT) {
			return processInfo{}, fmt.Errorf("unexpected process %d wait state %d", pid, state)
		}
	}
	var creation, exit, kernel, user windows.Filetime
	timesErr := windows.GetProcessTimes(nativeHandle, &creation, &exit, &kernel, &user)
	if timesErr != nil {
		return processInfo{}, fmt.Errorf("could not read identity for pid %d: %w", pid, timesErr)
	}
	birth := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	if birth == 0 {
		return processInfo{}, fmt.Errorf("%w for pid %d", ErrProcessIdentityUnavailable, pid)
	}
	return processInfo{
		handle: NewHandle(Uint32_ToPidT(pid), time.UnixMilli(creation.Nanoseconds()/int64(time.Millisecond)).UTC()),
		birth:  birth,
	}, nil
}

func snapshotProcesses(ctx context.Context) ([]processInfo, error) {
	size := uint32(64 * 1024)
	for {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if uint64(size) > uint64(math.MaxInt) {
			return nil, fmt.Errorf("process snapshot exceeds addressable memory")
		}
		buffer := make([]byte, int(size))
		var returned uint32
		queryErr := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buffer[0]), size, &returned)
		if errors.Is(queryErr, windows.STATUS_INFO_LENGTH_MISMATCH) || errors.Is(queryErr, windows.STATUS_BUFFER_TOO_SMALL) {
			if size > math.MaxUint32/2 {
				return nil, fmt.Errorf("process snapshot size overflow")
			}
			size = max(size*2, returned)
			continue
		}
		if queryErr != nil {
			return nil, fmt.Errorf("could not read process table: %w", queryErr)
		}
		if returned == 0 || returned > size {
			return nil, fmt.Errorf("invalid process snapshot length %d", returned)
		}
		return decodeWindowsProcessSnapshot(ctx, buffer[:returned])
	}
}

func decodeWindowsProcessSnapshot(ctx context.Context, contents []byte) ([]processInfo, error) {
	var native windows.SYSTEM_PROCESS_INFORMATION
	recordSize := int(unsafe.Sizeof(native))
	var processes []processInfo
	for offset := 0; ; {
		if contextErr := ctx.Err(); contextErr != nil {
			return processes, contextErr
		}
		if offset > len(contents)-recordSize {
			return processes, fmt.Errorf("truncated Windows process record")
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(&native)), recordSize), contents[offset:offset+recordSize])
		if uint64(native.UniqueProcessID) > math.MaxUint32 || uint64(native.InheritedFromUniqueProcessID) > math.MaxUint32 {
			return processes, fmt.Errorf("invalid Windows process id")
		}
		if native.UniqueProcessID != 0 {
			identityTime := time.Time{}
			birth := uint64(0)
			if native.CreateTime > 0 {
				birth = uint64(native.CreateTime)
				creation := windows.Filetime{LowDateTime: uint32(birth), HighDateTime: uint32(birth >> 32)}
				identityTime = time.UnixMilli(creation.Nanoseconds() / int64(time.Millisecond)).UTC()
			}
			processes = append(processes, processInfo{
				handle:    NewHandle(Pid_t(native.UniqueProcessID), identityTime),
				parentPID: Pid_t(native.InheritedFromUniqueProcessID),
				birth:     birth,
			})
		}
		if native.NextEntryOffset == 0 {
			return processes, nil
		}
		next := uint64(native.NextEntryOffset)
		if next < uint64(recordSize) || next > uint64(len(contents)-offset) {
			return processes, fmt.Errorf("invalid Windows process record offset %d", next)
		}
		offset += int(next)
	}
}

// ProcessName returns the executable's base name after validating the process identity.
func ProcessName(handle ProcessHandle) (string, error) {
	proc, findErr := FindProcess(handle)
	if findErr != nil {
		return "", findErr
	}
	var name string
	var nameErr error
	handleErr := proc.WithHandle(func(nativeHandle uintptr) {
		buffer := make([]uint16, 32768)
		size := uint32(len(buffer))
		nameErr = windows.QueryFullProcessImageName(windows.Handle(nativeHandle), 0, &buffer[0], &size)
		if nameErr == nil {
			name = filepath.Base(windows.UTF16ToString(buffer[:size]))
		}
	})
	return name, errors.Join(handleErr, nameErr, proc.Release())
}

// RollbackNativeProcess terminates and closes an owned Windows process whose startup failed.
func RollbackNativeProcess(ctx context.Context, nativeHandle windows.Handle) (returnErr error) {
	cleanupCtx, cleanupCancel := WithStopTimeout(ctx)
	defer cleanupCancel()
	defer func() {
		returnErr = nativeProcessRollbackResult(returnErr, windows.CloseHandle(nativeHandle))
	}()
	terminateErr := windows.TerminateProcess(nativeHandle, 1)
	for {
		if contextErr := cleanupCtx.Err(); contextErr != nil {
			return errors.Join(terminateErr, contextErr)
		}
		state, waitErr := windows.WaitForSingleObject(nativeHandle, 100)
		if waitErr != nil {
			return errors.Join(terminateErr, waitErr)
		}
		if state == windows.WAIT_OBJECT_0 {
			return nil
		}
		if state != uint32(windows.WAIT_TIMEOUT) {
			return fmt.Errorf("unexpected rollback wait state %d", state)
		}
		if terminateErr != nil {
			return terminateErr
		}
	}
}

func nativeProcessRollbackResult(rollbackErr error, closeErr error) error {
	return errors.Join(uncertainProcessStart(rollbackErr), closeErr)
}
