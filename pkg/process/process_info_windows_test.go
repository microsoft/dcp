//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"testing"
	"time"
	"unsafe"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func windowsRecord(pid, parent uintptr, birth int64, next uint32) []byte {
	native := windows.SYSTEM_PROCESS_INFORMATION{
		NextEntryOffset:              next,
		UniqueProcessID:              pid,
		InheritedFromUniqueProcessID: parent,
		CreateTime:                   birth,
	}
	size := int(unsafe.Sizeof(native))
	buffer := make([]byte, size)
	copy(buffer, unsafe.Slice((*byte)(unsafe.Pointer(&native)), size))
	return buffer
}

func TestWindowsSnapshotRecordBoundsAndPrecision(t *testing.T) {
	t.Parallel()
	creation := windows.NsecToFiletime(time.Unix(1000, 123456700).UnixNano())
	birth := int64(uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime))
	size := uint32(unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{}))
	buffer := append(windowsRecord(10, 0, birth, size), windowsRecord(11, 10, birth+1, 0)...)
	records, decodeErr := decodeWindowsProcessSnapshot(context.Background(), buffer)
	require.NoError(t, decodeErr)
	require.Len(t, records, 2)
	require.Equal(t, NewHandle(10, time.UnixMilli(1000123).UTC()), records[0].handle)
	require.Equal(t, uint64(birth+1), records[1].birth)
	require.Equal(t, Pid_t(10), records[1].parentPID)

	for _, malformed := range [][]byte{
		nil,
		make([]byte, size-1),
		windowsRecord(10, 0, birth, 1),
		windowsRecord(10, 0, birth, size),
		windowsRecord(10, 0, birth, ^uint32(0)),
	} {
		_, malformedErr := decodeWindowsProcessSnapshot(context.Background(), malformed)
		require.Error(t, malformedErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, cancelledErr := decodeWindowsProcessSnapshot(ctx, buffer)
	require.ErrorIs(t, cancelledErr, context.Canceled)
}

func TestDisposeClosesUntrackedProcessCleanupJob(t *testing.T) {
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	executor.acquireLock()
	job := executor.processCleanupJob()
	executor.releaseLock()
	require.NotEqual(t, windows.InvalidHandle, job)
	executor.Dispose()
	var information windows.JOBOBJECT_BASIC_LIMIT_INFORMATION
	queryErr := windows.QueryInformationJobObject(job, windows.JobObjectBasicLimitInformation,
		uintptr(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)), nil)
	require.ErrorIs(t, queryErr, windows.ERROR_INVALID_HANDLE)
}
