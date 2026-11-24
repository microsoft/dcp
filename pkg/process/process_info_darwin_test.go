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

func darwinRecord(pid, parent, group int32, seconds int64, microseconds int32, state int8) []byte {
	var native unix.KinfoProc
	native.Proc.P_pid = pid
	native.Proc.P_stat = state
	native.Eproc.Ppid = parent
	native.Eproc.Pgid = group
	native.Proc.P_starttime.Sec = seconds
	native.Proc.P_starttime.Usec = microseconds
	buffer := make([]byte, unix.SizeofKinfoProc)
	copy(buffer, unsafe.Slice((*byte)(unsafe.Pointer(&native)), unix.SizeofKinfoProc))
	return buffer
}

// Verifies that Darwin process records preserve identity, ancestry, group membership, and zombie state,
// and reject malformed sizes, group IDs, and timestamps.
func TestDarwinProcessRecordPrecision(t *testing.T) {
	t.Parallel()
	for _, state := range []int8{1, 5} {
		info, infoErr := decodeDarwinProcessInfo(darwinRecord(12, 10, 9, 1000, 123456, state))
		require.NoError(t, infoErr)
		require.Equal(t, NewHandle(12, time.UnixMilli(1000123).UTC()), info.handle)
		require.Equal(t, uint64(1000123456), info.birth)
		require.Equal(t, Pid_t(10), info.parentPID)
		require.Equal(t, Pid_t(9), info.groupID)
		require.Equal(t, state == 5, info.exited)
	}
	_, shortErr := decodeDarwinProcessInfo(make([]byte, unix.SizeofKinfoProc-1))
	require.Error(t, shortErr)
	_, invalidTimeErr := decodeDarwinProcessInfo(darwinRecord(12, 10, 9, 1000, 1000000, 1))
	require.Error(t, invalidTimeErr)
	_, invalidGroupErr := decodeDarwinProcessInfo(darwinRecord(12, 10, -1, 1000, 0, 1))
	require.Error(t, invalidGroupErr)
}

// Verifies that Darwin process snapshots retry transient sizing failures, honor cancellation,
// preserve readable records alongside inspection errors, and propagate malformed-table and permission errors.
func TestDarwinSnapshotRetriesAndPropagatesErrors(t *testing.T) {
	t.Parallel()
	calls := 0
	records, snapshotErr := snapshotDarwinProcesses(context.Background(), func() ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, unix.ENOMEM
		}
		return append(darwinRecord(10, 0, 10, 1000, 0, 1), darwinRecord(11, 10, 10, 1000, 1, 1)...), nil
	})
	require.NoError(t, snapshotErr)
	require.Len(t, records, 2)
	require.Equal(t, 2, calls)
	require.Equal(t, Pid_t(10), records[1].groupID)
	partial, partialErr := snapshotDarwinProcesses(context.Background(), func() ([]byte, error) {
		return append(darwinRecord(10, 0, 10, 1000, 0, 1), darwinRecord(11, 10, -1, 1000, 1, 1)...), nil
	})
	require.Error(t, partialErr)
	require.Equal(t, records[:1], partial)

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
