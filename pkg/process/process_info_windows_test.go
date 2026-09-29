//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
	"unsafe"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// Verifies that cleanup-job process access rights can open the current process,
// inspect its PID, and read a nonzero process identity.
func TestCleanupJobProcessAccessSupportsInspection(t *testing.T) {
	t.Parallel()

	pid := uint32(os.Getpid())
	nativeHandle, openErr := windows.OpenProcess(cleanupJobProcessAccess, false, pid)
	require.NoError(t, openErr)
	t.Cleanup(func() {
		require.NoError(t, windows.CloseHandle(nativeHandle))
	})

	info, infoErr := readWindowsProcessInfo(nativeHandle, false)
	require.NoError(t, infoErr)
	require.Equal(t, Uint32_ToPidT(pid), info.handle.Pid)
	require.False(t, info.handle.IdentityTime.IsZero())
}

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

// Verifies that Windows snapshot decoding preserves identity precision and parent relationships,
// rejects invalid record bounds, and honors cancellation.
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

// Verifies that executor disposal closes a cleanup job even when no tracked process forced its creation.
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

// Verifies that native rollback result handling keeps close-only failures certain,
// while rollback failures remain uncertain and preserve all contributing errors.
func TestNativeProcessRollbackResultDistinguishesCloseErrors(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("close failed")
	confirmedResult := nativeProcessRollbackResult(nil, closeErr)
	require.ErrorIs(t, confirmedResult, closeErr)
	require.NotErrorIs(t, confirmedResult, ErrProcessStartUncertain)

	waitErr := errors.New("wait failed")
	uncertainResult := nativeProcessRollbackResult(waitErr, closeErr)
	require.ErrorIs(t, uncertainResult, waitErr)
	require.ErrorIs(t, uncertainResult, closeErr)
	require.ErrorIs(t, uncertainResult, ErrProcessStartUncertain)
}

// Verifies that disposal cancels an admitted process start, waits for that start to finish,
// and closes the cleanup job created while the start was in flight.
func TestDisposeClosesCleanupJobCreatedByAdmittedStart(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	startCtx, finishStart, startErr := executor.beginProcessStart(context.Background())
	require.NoError(t, startErr)

	disposeDone := make(chan struct{})
	go func() {
		executor.Dispose()
		close(disposeDone)
	}()

	select {
	case <-startCtx.Done():
		require.ErrorIs(t, context.Cause(startCtx), ErrDisposed)
	case <-testCtx.Done():
		t.Fatal("disposal did not cancel the admitted start")
	}

	executor.acquireLock()
	job := executor.processCleanupJob()
	executor.releaseLock()
	require.NotEqual(t, windows.InvalidHandle, job)

	finishStart()

	select {
	case <-disposeDone:
	case <-testCtx.Done():
		t.Fatal("executor disposal did not complete")
	}

	var information windows.JOBOBJECT_BASIC_LIMIT_INFORMATION
	queryErr := windows.QueryInformationJobObject(job, windows.JobObjectBasicLimitInformation,
		uintptr(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)), nil)
	require.ErrorIs(t, queryErr, windows.ERROR_INVALID_HANDLE)
}
