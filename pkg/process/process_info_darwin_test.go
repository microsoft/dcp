//go:build darwin

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

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func darwinRecord(pid, parent int32, seconds int64, microseconds int32) []byte {
	var native unix.KinfoProc
	native.Proc.P_pid = pid
	native.Eproc.Ppid = parent
	native.Proc.P_starttime.Sec = seconds
	native.Proc.P_starttime.Usec = microseconds
	buffer := make([]byte, unix.SizeofKinfoProc)
	copy(buffer, unsafe.Slice((*byte)(unsafe.Pointer(&native)), unix.SizeofKinfoProc))
	return buffer
}

func TestDarwinProcessRecordPrecision(t *testing.T) {
	t.Parallel()
	info, infoErr := decodeDarwinProcessInfo(darwinRecord(12, 10, 1000, 123456))
	require.NoError(t, infoErr)
	require.Equal(t, NewHandle(12, time.UnixMilli(1000123).UTC()), info.handle)
	require.Equal(t, uint64(1000123456), info.birth)
	require.Equal(t, Pid_t(10), info.parentPID)
	_, shortErr := decodeDarwinProcessInfo(make([]byte, unix.SizeofKinfoProc-1))
	require.Error(t, shortErr)
	_, invalidTimeErr := decodeDarwinProcessInfo(darwinRecord(12, 10, 1000, 1000000))
	require.Error(t, invalidTimeErr)
}

func TestDarwinSnapshotResizeAndCancellation(t *testing.T) {
	t.Parallel()
	calls := 0
	records, snapshotErr := snapshotDarwinProcesses(context.Background(), func() ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, unix.ENOMEM
		}
		return append(darwinRecord(10, 0, 1000, 0), darwinRecord(11, 10, 1000, 1)...), nil
	})
	require.NoError(t, snapshotErr)
	require.Len(t, records, 2)
	require.Equal(t, 2, calls)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, cancelledErr := snapshotDarwinProcesses(ctx, func() ([]byte, error) {
		cancel()
		return nil, unix.ENOMEM
	})
	require.ErrorIs(t, cancelledErr, context.Canceled)
	_, malformedErr := snapshotDarwinProcesses(context.Background(), func() ([]byte, error) {
		return []byte{1}, nil
	})
	require.Error(t, malformedErr)
	_, deniedErr := snapshotDarwinProcesses(context.Background(), func() ([]byte, error) {
		return nil, unix.EPERM
	})
	require.ErrorIs(t, deniedErr, unix.EPERM)
	require.False(t, IsProcessGoneErr(deniedErr))
}
