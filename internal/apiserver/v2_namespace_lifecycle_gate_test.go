/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

const v2NamespaceLifecycleTestTimeout = 30 * time.Second

func TestV2NamespaceLifecycleGateWaitsForActiveCreates(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	release, allowed := gate.beginCreate("test")
	require.True(t, allowed)
	closeResult := make(chan error, 1)
	go func() {
		deleteLease, closeErr := gate.beginDelete(ctx, "test")
		if closeErr == nil {
			deleteLease.complete(v2NamespaceMutationAccepted)
		}
		closeResult <- closeErr
	}()
	waitV2NamespaceGateClosed(t, ctx, gate, "test")
	_, allowed = gate.beginCreate("test")
	require.False(t, allowed)
	select {
	case closeErr := <-closeResult:
		t.Fatalf("delete began before create completed: %v", closeErr)
	default:
	}

	release()
	require.NoError(t, waitForError(t, ctx, closeResult))
}

func TestV2NamespaceLifecycleGateRemovesAcceptedDeleteAfterStorageDeletion(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
	gate.observeNamespaces(map[string]v2NamespaceStorageState{}, gate.closedNamespaceStates())
	requireNoV2NamespaceGateState(t, gate, "test")
}

func TestV2NamespaceLifecycleGateWaitsForDeleteRequestAfterStorageDeletion(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	deleteLease, deleteErr := gate.beginDelete(ctx, "test")
	require.NoError(t, deleteErr)
	gate.observeNamespaces(map[string]v2NamespaceStorageState{}, gate.closedNamespaceStates())
	requireV2NamespaceGateState(t, gate, "test")
	deleteLease.complete(v2NamespaceMutationAccepted)
	requireNoV2NamespaceGateState(t, gate, "test")
}

func TestV2NamespaceLifecycleGateIgnoresDeletionObservedForReplacedState(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
	closedStates := gate.closedNamespaceStates()
	gate.open("test")
	secondDeleteLease, secondDeleteErr := gate.beginDelete(ctx, "test")
	require.NoError(t, secondDeleteErr)
	secondDeleteLease.complete(v2NamespaceMutationAccepted)
	gate.observeNamespaces(map[string]v2NamespaceStorageState{}, closedStates)
	requireV2NamespaceGateState(t, gate, "test")
}

func TestV2NamespaceLifecycleGateResolvesUncertainDelete(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "terminating"}[terminating], func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
			defer cancel()
			gate := newV2NamespaceLifecycleGate()
			deleteLease, deleteErr := gate.beginDelete(ctx, "test")
			require.NoError(t, deleteErr)
			deleteLease.complete(v2NamespaceMutationUncertain)
			gate.observeNamespaces(
				map[string]v2NamespaceStorageState{"test": {terminating: terminating}},
				gate.closedNamespaceStates(),
			)
			if !terminating {
				requireNoV2NamespaceGateState(t, gate, "test")
				return
			}
			gate.lock.Lock()
			defer gate.lock.Unlock()
			state := gate.namespaces["test"]
			require.NotNil(t, state)
			require.True(t, state.closed)
			require.True(t, state.deleteAccepted)
			require.Equal(t, v2NamespaceMutationNone, state.uncertainMutation)
		})
	}
}

func TestV2NamespaceLifecycleGateDoesNotResolveNewerUncertaintyFromOlderSnapshot(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()
	gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
	gate.markCreateUncertain("test")
	closedStates := gate.closedNamespaceStates()
	gate.markCreateUncertain("test")
	gate.observeNamespaces(map[string]v2NamespaceStorageState{"test": {}}, closedStates)
	requireV2NamespaceGateState(t, gate, "test")
}

func TestV2NamespaceLifecycleGateRemovesCancelledMutationWaiter(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()
	gate := newV2NamespaceLifecycleGate()
	firstLease, firstLeaseErr := gate.beginNamespaceMutation(ctx, "test")
	require.NoError(t, firstLeaseErr)
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	waitResult := make(chan error, 1)
	go func() {
		_, waitErr := gate.beginNamespaceMutation(waitCtx, "test")
		waitResult <- waitErr
	}()
	waitV2NamespaceMutationReferences(t, ctx, gate, "test", 2)
	cancelWait()
	require.ErrorIs(t, waitForError(t, ctx, waitResult), context.Canceled)
	firstLease.complete()
	requireNoV2NamespaceMutation(t, gate, "test")
	nextLease, nextLeaseErr := gate.beginNamespaceMutation(ctx, "test")
	require.NoError(t, nextLeaseErr)
	nextLease.complete()
	requireNoV2NamespaceMutation(t, gate, "test")
}

func TestV2NamespaceLifecycleGateRejectsSnapshotsAcrossNewerMutation(t *testing.T) {
	testCases := []struct {
		name       string
		namespaces map[string]v2NamespaceStorageState
	}{
		{name: "active", namespaces: map[string]v2NamespaceStorageState{"test": {}}},
		{name: "absent", namespaces: map[string]v2NamespaceStorageState{}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
			defer cancel()
			gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
			gate.markCreateUncertain("test")
			beforeMutation := gate.closedNamespaceStates()
			mutationLease, mutationErr := gate.beginNamespaceMutation(ctx, "test")
			require.NoError(t, mutationErr)
			require.Empty(t, gate.closedNamespaceStates())
			deleteLease, deleteErr := gate.beginDelete(ctx, "test")
			require.NoError(t, deleteErr)
			gate.observeNamespaces(testCase.namespaces, beforeMutation)
			_, allowed := gate.beginCreate("test")
			require.False(t, allowed)
			deleteLease.complete(v2NamespaceMutationRejected)
			mutationLease.complete()

			gate.observeNamespaces(testCase.namespaces, beforeMutation)
			requireV2NamespaceGateState(t, gate, "test")
			gate.observeNamespaces(testCase.namespaces, gate.closedNamespaceStates())
			requireNoV2NamespaceGateState(t, gate, "test")
		})
	}
}

func TestV2NamespaceLifecycleGateDoesNotReopenWhileDeleteActive(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()
	gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
	gate.markCreateUncertain("test")
	deleteLease, deleteErr := gate.beginDelete(ctx, "test")
	require.NoError(t, deleteErr)
	gate.observeNamespaces(map[string]v2NamespaceStorageState{"test": {}}, gate.closedNamespaceStates())
	_, allowed := gate.beginCreate("test")
	require.False(t, allowed)
	deleteLease.complete(v2NamespaceMutationRejected)
}

func TestV2NamespaceLifecycleGateRefreshesAfterLastMutationReference(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()
	gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
	mutationLease, mutationErr := gate.beginNamespaceMutation(ctx, "test")
	require.NoError(t, mutationErr)
	gate.markCreateUncertain("test")
	waitForSignal(t, ctx, gate.refreshRequested)
	require.Empty(t, gate.closedNamespaceStates())
	mutationLease.complete()
	waitForSignal(t, ctx, gate.refreshRequested)
	require.Contains(t, gate.closedNamespaceStates(), "test")
}

func waitForSignal(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func waitForError(t *testing.T, ctx context.Context, result <-chan error) error {
	t.Helper()
	select {
	case resultErr := <-result:
		return resultErr
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return nil
	}
}

func requireNoV2NamespaceMutation(t *testing.T, gate *v2NamespaceLifecycleGate, namespace string) {
	t.Helper()
	gate.lock.Lock()
	defer gate.lock.Unlock()
	require.NotContains(t, gate.namespaceMutations, namespace)
}

func requireV2NamespaceGateState(t *testing.T, gate *v2NamespaceLifecycleGate, namespace string) {
	t.Helper()
	gate.lock.Lock()
	defer gate.lock.Unlock()
	require.Contains(t, gate.namespaces, namespace)
}

func requireNoV2NamespaceGateState(t *testing.T, gate *v2NamespaceLifecycleGate, namespace string) {
	t.Helper()
	gate.lock.Lock()
	defer gate.lock.Unlock()
	require.NotContains(t, gate.namespaces, namespace)
}

func waitV2NamespaceMutationReferences(
	t *testing.T,
	ctx context.Context,
	gate *v2NamespaceLifecycleGate,
	namespace string,
	references int,
) {
	t.Helper()
	for {
		gate.lock.Lock()
		state := gate.namespaceMutations[namespace]
		referenceCount := 0
		if state != nil {
			referenceCount = state.references
		}
		gate.lock.Unlock()
		if referenceCount == references {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			goruntime.Gosched()
		}
	}
}

func waitV2NamespaceGateClosed(t *testing.T, ctx context.Context, gate *v2NamespaceLifecycleGate, namespace string) {
	t.Helper()
	for {
		gate.lock.Lock()
		state := gate.namespaces[namespace]
		closed := state != nil && state.closed
		gate.lock.Unlock()
		if closed {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			goruntime.Gosched()
		}
	}
}
