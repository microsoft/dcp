/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func treeProcess(pid, parent Pid_t, birth uint64) processInfo {
	return processInfo{
		handle:    NewHandle(pid, time.UnixMilli(1000+int64(birth)).UTC()),
		parentPID: parent,
		birth:     birth,
	}
}

// Verifies that process-tree construction is breadth-first, rejects reused or unverifiable identities,
// handles cycles and ambiguous records, and excludes stale or disconnected ancestry.
func TestBuildProcessTree(t *testing.T) {
	t.Parallel()
	root := treeProcess(10, 0, 10)
	child := treeProcess(11, 10, 11)
	equalChild := treeProcess(11, 10, 10)
	newRoot := treeProcess(10, 20, 20)
	cyclicRoot := treeProcess(10, 20, 10)
	unknownChild := treeProcess(11, 10, 11)
	unknownChild.handle.IdentityTime = time.Time{}
	tests := []struct {
		name     string
		root     processInfo
		snapshot []processInfo
		pids     []Pid_t
		err      error
	}{
		{"breadth first", root, []processInfo{treeProcess(13, 11, 13), root, child, treeProcess(12, 10, 12)}, []Pid_t{10, 11, 12, 13}, nil},
		{"stale Windows parent cycle", newRoot, []processInfo{newRoot, treeProcess(20, 10, 10), treeProcess(30, 10, 21)}, []Pid_t{10, 30}, nil},
		{"equal birth times", root, []processInfo{root, equalChild}, []Pid_t{10, 11}, nil},
		{"equal time cycle", cyclicRoot, []processInfo{cyclicRoot, treeProcess(20, 10, 10), treeProcess(30, 10, 11)}, []Pid_t{10, 30}, ErrIncompleteProcessTree},
		{"self parent", treeProcess(10, 10, 10), []processInfo{treeProcess(10, 10, 10), child}, []Pid_t{10, 11}, ErrIncompleteProcessTree},
		{"duplicate record", root, []processInfo{root, child, child}, []Pid_t{10, 11}, nil},
		{"conflicting identity", root, []processInfo{root, child, treeProcess(11, 10, 12), treeProcess(13, 11, 13)}, []Pid_t{10}, ErrIncompleteProcessTree},
		{"unreadable descendant identity", root, []processInfo{root, unknownChild, treeProcess(13, 11, 13)}, []Pid_t{10}, ErrIncompleteProcessTree},
		{"disconnected and orphaned", root, []processInfo{root, treeProcess(40, 99, 40), treeProcess(50, 51, 50), treeProcess(51, 50, 50)}, []Pid_t{10}, nil},
		{"root absent", root, []processInfo{child}, []Pid_t{10}, ErrIncompleteProcessTree},
		{"root reused within compatibility tolerance", root, []processInfo{treeProcess(10, 0, 11)}, nil, ErrProcessIdentityMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree, treeErr := buildProcessTree(context.Background(), tc.root, tc.snapshot)
			require.ErrorIs(t, treeErr, tc.err)
			require.Equal(t, tc.pids, getIDs(tree))
		})
	}
}

// Verifies that a root which exits or is replaced after the snapshot does not discard
// descendants whose identities were already verified from that snapshot. It also ensures
// that a replacement process using the root PID never enters the usable tree.
func TestGetProcessTreePreservesVerifiedDescendantsWhenRootChangesAfterSnapshot(t *testing.T) {
	t.Parallel()

	root := treeProcess(10, 0, 10)
	child := treeProcess(11, 10, 11)
	replacement := treeProcess(10, 0, 20)
	tests := []struct {
		name      string
		after     processInfo
		afterErr  error
		changeErr error
	}{
		{
			name:      "root exited",
			afterErr:  &ErrProcessNotFound{Pid: root.handle.Pid, Inner: ErrorProcessNotFound},
			changeErr: ErrorProcessNotFound,
		},
		{
			name:      "root pid reused",
			afterErr:  fmt.Errorf("%w during revalidation", ErrProcessIdentityMismatch),
			changeErr: ErrProcessIdentityMismatch,
		},
		{
			name:      "native identity changed within timestamp tolerance",
			after:     replacement,
			changeErr: ErrProcessIdentityMismatch,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			lookupCount := 0
			tree, treeErr := getProcessTree(
				context.Background(),
				root.handle,
				func(ProcessHandle) (processInfo, error) {
					lookupCount++
					if lookupCount == 1 {
						return root, nil
					}
					return testCase.after, testCase.afterErr
				},
				func(context.Context) ([]processInfo, error) {
					return []processInfo{root, child}, nil
				},
			)

			require.ErrorIs(t, treeErr, ErrIncompleteProcessTree)
			require.ErrorIs(t, treeErr, testCase.changeErr)
			require.Equal(t, []Pid_t{root.handle.Pid, child.handle.Pid}, getIDs(tree))
			require.Equal(t, root.handle, tree[0], "a replacement using the root PID must never enter the usable tree")
		})
	}
}

// Verifies that cancellation and non-lifecycle revalidation failures remain fatal
// even when a snapshot had enough information to construct a process tree.
func TestGetProcessTreePreservesFatalRevalidationBehavior(t *testing.T) {
	t.Parallel()

	root := treeProcess(10, 0, 10)
	child := treeProcess(11, 10, 11)
	fatalErr := errors.New("process metadata access denied")
	lookupCount := 0
	tree, treeErr := getProcessTree(
		context.Background(),
		root.handle,
		func(ProcessHandle) (processInfo, error) {
			lookupCount++
			if lookupCount == 1 {
				return root, nil
			}
			return processInfo{}, fatalErr
		},
		func(context.Context) ([]processInfo, error) {
			return []processInfo{root, child}, nil
		},
	)
	require.Nil(t, tree)
	require.ErrorIs(t, treeErr, fatalErr)

	ctx, cancel := context.WithCancel(context.Background())
	cancelledTree, cancelledErr := getProcessTree(
		ctx,
		root.handle,
		func(ProcessHandle) (processInfo, error) {
			return root, nil
		},
		func(context.Context) ([]processInfo, error) {
			cancel()
			return []processInfo{root, child}, nil
		},
	)
	require.Nil(t, cancelledTree)
	require.ErrorIs(t, cancelledErr, context.Canceled)
}

// Verifies that process-tree ancestry uses native birth ordering
// rather than rounded identity timestamps when excluding an older child.
func TestProcessTreeUsesNativeBirthOrdering(t *testing.T) {
	t.Parallel()
	root := treeProcess(10, 0, 10000)
	older := treeProcess(11, 10, 9999)
	older.handle.IdentityTime = root.handle.IdentityTime
	tree, treeErr := buildProcessTree(context.Background(), root, []processInfo{root, older})
	require.NoError(t, treeErr)
	require.Equal(t, []Pid_t{10}, getIDs(tree))
}

// Verifies that process-tree construction handles deep ancestry iteratively,
// preserves breadth-first order, and exits promptly for a canceled context.
func TestProcessTreeDeepAndCancelled(t *testing.T) {
	t.Parallel()
	const count = 10000
	snapshot := make([]processInfo, count)
	for index := range snapshot {
		snapshot[index] = treeProcess(Pid_t(index+1), Pid_t(index), uint64(index+1))
	}
	tree, treeErr := buildProcessTree(context.Background(), snapshot[0], snapshot)
	require.NoError(t, treeErr)
	require.Len(t, tree, count)
	for index, handle := range tree {
		require.Equal(t, Pid_t(index+1), handle.Pid)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, cancelledErr := buildProcessTree(ctx, snapshot[0], nil)
	require.ErrorIs(t, cancelledErr, context.Canceled)
}

// Verifies that every process action re-inspects identity before dispatch,
// rejects PID reuse and inspection errors, and honors cancellation before inspection.
func TestProcessActionRevalidatesEveryDispatch(t *testing.T) {
	t.Parallel()
	expected := treeProcess(10, 0, 10).handle
	actual := expected
	inspections := 0
	signals := 0
	inspect := func() (ProcessHandle, error) {
		inspections++
		return actual, nil
	}
	send := func() error {
		signals++
		return nil
	}
	require.NoError(t, actOnProcess(context.Background(), expected, inspect, send))
	actual.IdentityTime = actual.IdentityTime.Add(time.Second)
	require.ErrorIs(t, actOnProcess(context.Background(), expected, inspect, send), ErrProcessIdentityMismatch)
	require.Equal(t, 2, inspections)
	require.Equal(t, 1, signals, "force-kill escalation must not target the replacement")

	inspectionErr := errors.New("metadata access denied")
	require.ErrorIs(t, actOnProcess(context.Background(), expected, func() (ProcessHandle, error) {
		return ProcessHandle{}, inspectionErr
	}, send), inspectionErr)
	require.Equal(t, 1, signals)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, actOnProcess(ctx, expected, inspect, send), context.Canceled)
	require.Equal(t, 2, inspections)
}

// Verifies that process lookup, stopping, waiting, and tree APIs reject incomplete identities,
// and that nil process and command inputs return invalid-handle errors.
func TestProcessAPIsRejectIncompleteIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	executor := NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	incomplete := NewHandle(Pid_t(os.Getpid()), time.Time{})
	_, findErr := incomplete.OsProcess()
	require.ErrorIs(t, findErr, ErrInvalidProcessHandle)
	require.ErrorIs(t, executor.StopProcess(ctx, incomplete), ErrInvalidProcessHandle)
	_, waitErr := FindWaitableProcess(incomplete)
	require.ErrorIs(t, waitErr, ErrInvalidProcessHandle)
	_, treeErr := GetProcessTree(ctx, incomplete)
	require.ErrorIs(t, treeErr, ErrInvalidProcessHandle)
	_, nilProcessErr := ProcessHandleFromProcess(nil)
	require.ErrorIs(t, nilProcessErr, ErrInvalidProcessHandle)
	_, nilCmdErr := ProcessHandleFromCmd(nil)
	require.ErrorIs(t, nilCmdErr, ErrInvalidProcessHandle)
}

// Verifies that process identity comparison accepts only the configured timestamp tolerance,
// distinguishes unavailable identity, and classifies joined gone errors only when every cause is gone.
func TestIdentityToleranceAndGoneClassification(t *testing.T) {
	t.Parallel()
	actual := treeProcess(10, 0, 10).handle
	expected := actual
	expected.IdentityTime = actual.IdentityTime.Add(ProcessIdentityTimeMaximumDifference)
	require.NoError(t, validateIdentity(expected, actual))
	expected.IdentityTime = expected.IdentityTime.Add(time.Nanosecond)
	require.ErrorIs(t, validateIdentity(expected, actual), ErrProcessIdentityMismatch)
	require.ErrorIs(t, validateIdentity(actual, NewHandle(actual.Pid, time.Time{})), ErrProcessIdentityUnavailable)
	require.False(t, IsProcessGoneErr(errors.Join(ErrorProcessNotFound, os.ErrPermission)))
	require.False(t, IsProcessGoneErr(fmt.Errorf("inspection: %w", errors.Join(ErrProcessIdentityMismatch, context.Canceled))))
	require.True(t, IsProcessGoneErr(errors.Join(ErrorProcessNotFound, ErrProcessIdentityMismatch)))
}
