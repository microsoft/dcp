//go:build darwin

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
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func readProcessInfo(pid Pid_t) (processInfo, error) {
	if pid <= 0 || pid > math.MaxInt32 {
		return processInfo{}, &ErrProcessNotFound{Pid: pid}
	}
	contents, readErr := unix.SysctlRaw("kern.proc.pid", int(pid))
	if readErr != nil {
		return processInfo{}, processLookupError(pid, readErr)
	}
	if len(contents) == 0 {
		return processInfo{}, &ErrProcessNotFound{Pid: pid}
	}
	if len(contents) != unix.SizeofKinfoProc {
		return processInfo{}, fmt.Errorf("unexpected process record size %d for pid %d", len(contents), pid)
	}
	info, infoErr := decodeDarwinProcessInfo(contents)
	if infoErr != nil {
		return processInfo{}, infoErr
	}
	if info.handle.Pid != pid {
		return processInfo{}, fmt.Errorf("process record pid %d does not match requested pid %d", info.handle.Pid, pid)
	}
	return info, nil
}

func snapshotProcesses(ctx context.Context) ([]processInfo, error) {
	return snapshotDarwinProcesses(ctx, func() ([]byte, error) {
		return unix.SysctlRaw("kern.proc.all")
	})
}

func snapshotDarwinProcesses(ctx context.Context, query func() ([]byte, error)) ([]processInfo, error) {
	for {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		contents, queryErr := query()
		if errors.Is(queryErr, unix.ENOMEM) {
			continue
		}
		if queryErr != nil {
			return nil, fmt.Errorf("could not read process table: %w", queryErr)
		}
		if len(contents)%unix.SizeofKinfoProc != 0 {
			return nil, fmt.Errorf("process table has an incomplete record")
		}
		processes := make([]processInfo, 0, len(contents)/unix.SizeofKinfoProc)
		var inspectionErrors []error
		for offset := 0; offset < len(contents); offset += unix.SizeofKinfoProc {
			if contextErr := ctx.Err(); contextErr != nil {
				return processes, errors.Join(append(inspectionErrors, contextErr)...)
			}
			info, infoErr := decodeDarwinProcessInfo(contents[offset : offset+unix.SizeofKinfoProc])
			if infoErr != nil {
				inspectionErrors = append(inspectionErrors, infoErr)
			} else if info.handle.Pid > 0 {
				processes = append(processes, info)
			}
		}
		return processes, errors.Join(inspectionErrors...)
	}
}

func decodeDarwinProcessInfo(contents []byte) (processInfo, error) {
	const processStateZombie = 5 // SZOMB from sys/proc.h.
	if len(contents) != unix.SizeofKinfoProc {
		return processInfo{}, fmt.Errorf("invalid Darwin process record size %d", len(contents))
	}
	var native unix.KinfoProc
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&native)), unix.SizeofKinfoProc), contents)
	if native.Proc.P_pid == 0 {
		return processInfo{}, nil
	}
	start := native.Proc.P_starttime
	if native.Proc.P_pid < 0 || native.Eproc.Ppid < 0 || start.Sec < 0 ||
		start.Usec < 0 || start.Usec >= 1000000 || uint64(start.Sec) > (math.MaxUint64-999999)/1000000 {
		return processInfo{}, fmt.Errorf("invalid Darwin process information for pid %d", native.Proc.P_pid)
	}
	birth := uint64(start.Sec)*1000000 + uint64(start.Usec)
	identityTime := time.Time{}
	if birth != 0 {
		identityTime = time.Unix(start.Sec, int64(start.Usec)*1000).UTC().Truncate(time.Millisecond)
	}
	return processInfo{
		handle:    NewHandle(Pid_t(native.Proc.P_pid), identityTime),
		parentPID: Pid_t(native.Eproc.Ppid),
		birth:     birth,
		exited:    native.Proc.P_stat == processStateZombie,
	}, nil
}
