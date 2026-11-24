//go:build darwin || linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"fmt"
	"math"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that OSExecutor isolates ordinary child groups and preserves explicit Unix session setup.
func TestStartProcessIsolatesUnixProcessGroup(t *testing.T) {
	t.Parallel()

	for _, newSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("new-session=%t", newSession), func(t *testing.T) {
			t.Parallel()
			testCtx, cancelTest := testutil.GetTestContext(t, 0)
			t.Cleanup(cancelTest)
			executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
			t.Cleanup(executor.Dispose)

			cmd := delayCommandForTest(t, "--delay=120s")
			if newSession {
				ForkFromParent(cmd)
			}
			handle, _, _ := startDelayTreeForTest(t, testCtx, executor, cmd, 1)

			pid, pidErr := PidT_ToInt(handle.Pid)
			require.NoError(t, pidErr)
			groupID, groupErr := unix.Getpgid(pid)
			require.NoError(t, groupErr)
			require.Equal(t, pid, groupID)
			require.NotEqual(t, unix.Getpgrp(), groupID)
			sessionID, sessionErr := unix.Getsid(pid)
			require.NoError(t, sessionErr)
			if newSession {
				require.Equal(t, pid, sessionID)
			} else {
				parentSession, parentSessionErr := unix.Getsid(0)
				require.NoError(t, parentSessionErr)
				require.Equal(t, parentSession, sessionID)
			}
		})
	}
}

// Verifies that StopProcess kills group members created by SIGTERM handling after tree discovery.
func TestStopProcessKillsLateGroupMember(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	t.Cleanup(cancelTest)
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	t.Cleanup(executor.Dispose)
	output := testutil.NewBufferWriter()
	cmd := delayCommandForTest(t, "--delay=120s", "--child-spec=1", "--couple-children", "--children-on-exit")
	cmd.Stdout = output
	handle, _, _ := startDelayTreeForTest(t, testCtx, executor, cmd, 1)
	group, groupErr := FindProcessGroup(handle, false)
	require.NoError(t, groupErr)
	require.NotNil(t, group)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := WithDetachedStopTimeout(testCtx)
		defer cleanupCancel()
		require.NoError(t, group.signal(cleanupCtx, syscall.SIGKILL))
	})
	readyErr := wait.PollUntilContextCancel(testCtx, stopWaitPollInterval, true, func(context.Context) (bool, error) {
		return strings.Contains(string(output.Bytes()), "Ready to create children on exit\n"), nil
	})
	require.NoError(t, readyErr)

	tree, treeErr := GetProcessTree(testCtx, handle)
	require.NoError(t, treeErr)
	require.Len(t, tree, 1)
	require.NoError(t, executor.StopProcess(testCtx, handle))

	lines := output.Lines([]byte("\n"))
	require.Len(t, lines, 3)
	latePID, latePIDErr := StringToPidT(string(lines[1]))
	require.NoError(t, latePIDErr)
	require.NotEqual(t, handle.Pid, latePID)
	_, lateInspectionErr := FindProcessHandle(latePID)
	require.True(t, IsProcessGoneErr(lateInspectionErr), "late group member is still running: %v", lateInspectionErr)
}

// Verifies that StopProcess uses only tree cleanup for a root sharing the caller's Unix process group.
func TestStopProcessInSharedUnixGroupUsesTree(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	t.Cleanup(cancelTest)
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	t.Cleanup(executor.Dispose)
	cmd := delayCommandForTest(t, "--delay=120s", "--child-spec=1", "--couple-children")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: unix.Getpgrp()}
	handle, tree, _ := startDelayTreeForTest(t, testCtx, executor, cmd, 2)
	group, groupErr := FindProcessGroup(handle, false)
	require.NoError(t, groupErr)
	require.Nil(t, group)
	require.NoError(t, executor.StopProcess(testCtx, handle))
	for _, stopped := range tree {
		require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(stopped)))
	}
}

// Verifies that StopRootOnly stops the root without signaling its coupled descendants.
func TestStopRootOnlySkipsUnixGroupCleanup(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	t.Cleanup(cancelTest)
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	t.Cleanup(executor.Dispose)
	handle, tree, _ := startDelayTreeForTest(t, testCtx, executor,
		delayCommandForTest(t, "--delay=120s", "--child-spec=1", "--couple-children"), 2)

	require.NoError(t, executor.StopProcess(testCtx, handle, StopRootOnly(), StopWithProcessGroup()))
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)))
	require.NoError(t, executor.CheckProcessRunning(tree[1]))
}

// Verifies that FindProcessGroup identifies a known group after its leader is reaped,
// ProcessGroup.Wait tracks surviving members, and StopWithProcessGroup stops them using the original handle.
func TestProcessGroupWaitTracksMembersAfterLeaderExit(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	t.Cleanup(cancelTest)
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	t.Cleanup(executor.Dispose)
	handle, tree, _ := startDelayTreeForTest(t, testCtx, executor,
		delayCommandForTest(t, "--delay=120s", "--child-spec=1", "--couple-children"), 2)
	require.NoError(t, executor.StopProcess(testCtx, handle, StopRootOnly()))
	_, discoveryErr := FindProcessGroup(handle, false)
	require.True(t, IsProcessGoneErr(discoveryErr))
	group, groupErr := FindProcessGroup(handle, true)
	require.NoError(t, groupErr)
	require.NotNil(t, group)

	// A deadline proves that Wait stays blocked while an orphaned group member is alive.
	waitCtx, cancelWait := context.WithTimeout(testCtx, time.Second)
	defer cancelWait()
	require.ErrorIs(t, group.Wait(waitCtx), context.DeadlineExceeded)
	require.NoError(t, executor.CheckProcessRunning(tree[1]))
	require.NoError(t, executor.StopProcess(testCtx, handle, StopWithProcessGroup()))
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(tree[1])))
	require.NoError(t, group.Wait(testCtx))
}

// Verifies that group cleanup stops further signaling on caller cancellation, leaving surviving members addressable.
func TestStopProcessGroupRespectsCallerCancellation(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	t.Cleanup(cancelTest)
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	t.Cleanup(executor.Dispose)
	handle, tree, _ := startDelayTreeForTest(t, testCtx, executor,
		delayCommandForTest(t, "--delay=120s", "--ignore-sigterm", "--child-spec=1", "--couple-children"), 2)

	stopCtx, cancelStop := context.WithCancel(testCtx)
	defer cancelStop()
	stopResult := make(chan error, 1)
	go func() { stopResult <- executor.StopProcess(stopCtx, handle) }()
	childExitErr := wait.PollUntilContextCancel(testCtx, stopWaitPollInterval, true, func(context.Context) (bool, error) {
		inspectionErr := executor.CheckProcessRunning(tree[1])
		if IsProcessGoneErr(inspectionErr) {
			return true, nil
		}
		return false, inspectionErr
	})
	require.NoError(t, childExitErr)
	cancelStop()
	select {
	case <-testCtx.Done():
		t.Fatal("group stopping did not end after caller cancellation")
	case stopErr, received := <-stopResult:
		require.True(t, received)
		require.ErrorIs(t, stopErr, context.Canceled)
	}
	require.NoError(t, executor.CheckProcessRunning(handle))
}

// Verifies that group discovery rejects mismatched identities, known groups recognize PID reuse as completion,
// and invalid or missing identities remain errors.
func TestProcessGroupValidatesIdentity(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	t.Cleanup(cancelTest)
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	t.Cleanup(executor.Dispose)
	handle, _, _ := startDelayTreeForTest(t, testCtx, executor, delayCommandForTest(t, "--delay=120s"), 1)
	group, groupErr := FindProcessGroup(handle, false)
	require.NoError(t, groupErr)
	require.NotNil(t, group)
	wrongIdentity := NewHandle(handle.Pid, handle.IdentityTime.Add(-time.Minute))
	for _, knownLeader := range []bool{false, true} {
		wrongGroup, wrongIdentityErr := FindProcessGroup(wrongIdentity, knownLeader)
		if knownLeader {
			require.NoError(t, wrongIdentityErr)
			require.NoError(t, wrongGroup.Wait(testCtx))
		} else {
			require.ErrorIs(t, wrongIdentityErr, ErrProcessIdentityMismatch)
		}
		_, missingIdentityErr := FindProcessGroup(NewHandle(handle.Pid, time.Time{}), knownLeader)
		require.ErrorIs(t, missingIdentityErr, ErrInvalidProcessHandle)
		for _, invalidPID := range []Pid_t{UnknownPID, 0, 1, math.MaxInt32 + 1, math.MaxUint32 + 1} {
			_, invalidErr := FindProcessGroup(NewHandle(invalidPID, handle.IdentityTime), knownLeader)
			require.Error(t, invalidErr)
		}
	}
	require.ErrorIs(t, executor.StopProcess(testCtx, wrongIdentity, StopWithProcessGroup()), ErrProcessIdentityMismatch)
	require.NoError(t, executor.CheckProcessRunning(handle))
	emptyGroup := &ProcessGroup{}
	require.Error(t, emptyGroup.Wait(testCtx))
}

// Verifies that group waiting and signaling treat a reused leader PID as completion without targeting its replacement,
// while process-tree stopping retains its original gone-error classification.
func TestProcessGroupPIDReuseLeavesReplacementUntouched(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	t.Cleanup(cancelTest)
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	t.Cleanup(executor.Dispose)
	handle, tree, _ := startDelayTreeForTest(t, testCtx, executor,
		delayCommandForTest(t, "--delay=120s", "--child-spec=1", "--couple-children"), 2)
	// A stale identity simulates PID reuse without depending on the kernel recycling a PID during the test.
	original := NewHandle(handle.Pid, handle.IdentityTime.Add(-time.Minute))
	group, groupErr := FindProcessGroup(original, true)
	require.NoError(t, groupErr)
	require.NotNil(t, group)
	_, leaderErr := group.hasLiveLeader()
	require.ErrorIs(t, leaderErr, ErrProcessIdentityMismatch)
	running, inspectionErr := group.isRunning(testCtx)
	require.NoError(t, inspectionErr)
	require.False(t, running)
	require.NoError(t, group.Wait(testCtx))
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		require.NoError(t, group.signal(testCtx, signal))
		for _, replacement := range tree {
			require.NoError(t, executor.CheckProcessRunning(replacement))
		}
	}
	stopErr := executor.StopProcess(testCtx, original, StopWithProcessGroup())
	require.ErrorIs(t, stopErr, ErrProcessIdentityMismatch)
	require.True(t, IsProcessGoneErr(stopErr))
	for _, replacement := range tree {
		require.NoError(t, executor.CheckProcessRunning(replacement))
	}

	cancelledCtx, cancel := context.WithCancel(testCtx)
	cancel()
	require.ErrorIs(t, group.Wait(cancelledCtx), context.Canceled)
	require.ErrorIs(t, group.signal(cancelledCtx, syscall.SIGKILL), context.Canceled)
}

// Verifies that group inspection filters shared snapshot metadata, uses readable live members despite partial errors,
// and requires complete snapshots containing only previously observed zombies before confirming completion.
func TestIsProcessGroupRunningUsesPartialSnapshots(t *testing.T) {
	t.Parallel()
	member := treeProcess(11, 99, 11)
	member.groupID = 10
	zombie := member
	zombie.exited = true
	newZombie := treeProcess(12, 1, 12)
	newZombie.groupID = 10
	newZombie.exited = true
	unrelated := treeProcess(20, 10, 20)
	unrelated.groupID = 20
	snapshotFailure := fmt.Errorf("could not inspect unrelated process: %w", syscall.EACCES)
	type snapshotResult struct {
		processes []processInfo
		err       error
	}
	for _, testCase := range []struct {
		name      string
		snapshots []snapshotResult
		running   bool
		err       error
	}{
		{
			name:      "live member in partial snapshot",
			snapshots: []snapshotResult{{[]processInfo{unrelated, member}, snapshotFailure}},
			running:   true,
		},
		{
			name:      "live member in partial second snapshot",
			snapshots: []snapshotResult{{[]processInfo{zombie}, nil}, {[]processInfo{member}, snapshotFailure}},
			running:   true,
		},
		{
			name:      "unrelated group with matching parent",
			snapshots: []snapshotResult{{[]processInfo{unrelated}, nil}, {[]processInfo{unrelated}, nil}},
		},
		{
			name:      "unrelated live process cannot resolve partial snapshot",
			snapshots: []snapshotResult{{[]processInfo{unrelated}, snapshotFailure}},
			err:       ErrIncompleteProcessTree,
		},
		{
			name:      "empty partial snapshot",
			snapshots: []snapshotResult{{nil, snapshotFailure}},
			err:       ErrIncompleteProcessTree,
		},
		{
			name:      "zombie in partial snapshot",
			snapshots: []snapshotResult{{[]processInfo{zombie}, snapshotFailure}},
			err:       ErrIncompleteProcessTree,
		},
		{
			name:      "known zombie in complete snapshots",
			snapshots: []snapshotResult{{[]processInfo{zombie}, nil}, {[]processInfo{zombie}, nil}},
		},
		{
			name:      "zombie removed between snapshots",
			snapshots: []snapshotResult{{[]processInfo{zombie}, nil}, {nil, nil}},
		},
		{
			name:      "new zombie requires another census",
			snapshots: []snapshotResult{{[]processInfo{zombie}, nil}, {[]processInfo{newZombie}, nil}},
			running:   true,
		},
		{
			name:      "incomplete second snapshot",
			snapshots: []snapshotResult{{[]processInfo{zombie}, nil}, {[]processInfo{zombie}, snapshotFailure}},
			err:       ErrIncompleteProcessTree,
		},
		{
			name:      "new zombie cannot resolve partial second snapshot",
			snapshots: []snapshotResult{{[]processInfo{zombie}, nil}, {[]processInfo{newZombie}, snapshotFailure}},
			err:       ErrIncompleteProcessTree,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			t.Cleanup(cancel)
			snapshotsTaken := 0
			running, inspectionErr := isProcessGroupRunning(ctx, 10, func(context.Context) ([]processInfo, error) {
				require.Less(t, snapshotsTaken, len(testCase.snapshots))
				snapshot := testCase.snapshots[snapshotsTaken]
				snapshotsTaken++
				return snapshot.processes, snapshot.err
			})
			require.Equal(t, testCase.running, running)
			require.ErrorIs(t, inspectionErr, testCase.err)
			if testCase.err != nil {
				require.ErrorIs(t, inspectionErr, syscall.EACCES)
			}
			require.Equal(t, len(testCase.snapshots), snapshotsTaken)
		})
	}
}

// Verifies that group inspection honors cancellation before and during capture even when a partial snapshot has a live member.
func TestIsProcessGroupRunningHonorsSnapshotCancellation(t *testing.T) {
	t.Parallel()
	for _, cancelBeforeSnapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel-before-snapshot=%t", cancelBeforeSnapshot), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			t.Cleanup(cancel)
			if cancelBeforeSnapshot {
				cancel()
			}
			snapshotsTaken := 0
			running, inspectionErr := isProcessGroupRunning(ctx, 10, func(context.Context) ([]processInfo, error) {
				snapshotsTaken++
				cancel()
				return []processInfo{{handle: NewHandle(11, time.Now()), groupID: 10}}, syscall.EACCES
			})
			require.False(t, running)
			require.ErrorIs(t, inspectionErr, context.Canceled)
			if cancelBeforeSnapshot {
				require.Zero(t, snapshotsTaken)
			} else {
				require.Equal(t, 1, snapshotsTaken)
			}
		})
	}
}
