/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestPersistentNetworkFailsToStartWithoutStateStore(t *testing.T) {
	t.Parallel()

	network := persistentTestNetwork()
	reconciler := newTestNetworkReconciler(t, NetworkReconcilerConfig{})

	ctx, cancel := testutil.GetTestContext(t, controllerLeaseTestTimeout)
	defer cancel()

	change := reconciler.ensureNetwork(ctx, network, logr.Discard())

	require.Equal(t, statusChanged, change)
	require.Equal(t, apiv1.ContainerNetworkStateFailedToStart, network.Status.State)
	require.Equal(t, "state store is not configured", network.Status.Message)

	_, state := reconciler.existingNetworks.BorrowByNamespacedName(network.NamespacedName())
	require.NotNil(t, state)
	require.Equal(t, apiv1.ContainerNetworkStateFailedToStart, state.state)
	require.Equal(t, "state store is not configured", state.message)
}

func TestPersistentNetworkFailsToStartWithInvalidLeaseOwner(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, controllerLeaseTestTimeout)
	defer cancel()
	store := openNetworkTestStore(t, ctx)
	network := persistentTestNetwork()
	reconciler := newTestNetworkReconciler(t, NetworkReconcilerConfig{
		StateStore: store,
	})

	change := reconciler.ensureNetwork(ctx, network, logr.Discard())

	require.Equal(t, statusChanged, change)
	require.Equal(t, apiv1.ContainerNetworkStateFailedToStart, network.Status.State)
	require.Contains(t, network.Status.Message, "resource lease owner")
}

func TestPersistentNetworkLeaseHeldRequeues(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, controllerLeaseTestTimeout)
	defer cancel()
	store := openNetworkTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	otherOwner := leaseOwner
	otherOwner.IdentityTime = otherOwner.IdentityTime.Add(-time.Hour)

	network := persistentTestNetwork()
	_, acquireErr := store.AcquireResourceLease(ctx, network, otherOwner, time.Minute)
	require.NoError(t, acquireErr)

	reconciler := newTestNetworkReconciler(t, NetworkReconcilerConfig{
		StateStore:         store,
		ResourceLeaseOwner: leaseOwner,
	})

	change := reconciler.ensureNetwork(ctx, network, logr.Discard())

	require.Equal(t, additionalReconciliationNeeded, change)
	require.Empty(t, network.Status.State)
	require.Empty(t, network.Status.Message)
}

func TestSetPersistentNetworkStableStateReleasesLease(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		state apiv1.ContainerNetworkState
	}{
		{name: "running", state: apiv1.ContainerNetworkStateRunning},
		{name: "failed to start", state: apiv1.ContainerNetworkStateFailedToStart},
		{name: "removed", state: apiv1.ContainerNetworkStateRemoved},
		{name: "not found", state: apiv1.ContainerNetworkStateNotFound},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, controllerLeaseTestTimeout)
			defer cancel()
			store := openNetworkTestStore(t, ctx)
			leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
			require.NoError(t, leaseOwnerErr)
			network := persistentTestNetwork()
			network.Name = testCase.name
			reconciler := newTestNetworkReconciler(t, NetworkReconcilerConfig{
				StateStore:         store,
				ResourceLeaseOwner: leaseOwner,
			})

			_, acquireErr := store.AcquireResourceLease(ctx, network, leaseOwner, time.Minute)
			require.NoError(t, acquireErr)

			reconciler.setNetworkState(network, testCase.state, "")

			verifyErr := store.VerifyResourceLeaseHeld(ctx, network, leaseOwner)
			require.ErrorIs(t, verifyErr, statestore.ErrResourceLeaseNotHeld)
		})
	}
}

func TestSetPersistentNetworkTransientStateKeepsLease(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, controllerLeaseTestTimeout)
	defer cancel()
	store := openNetworkTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	network := persistentTestNetwork()
	reconciler := newTestNetworkReconciler(t, NetworkReconcilerConfig{
		StateStore:         store,
		ResourceLeaseOwner: leaseOwner,
	})

	_, acquireErr := store.AcquireResourceLease(ctx, network, leaseOwner, time.Minute)
	require.NoError(t, acquireErr)

	reconciler.setNetworkState(network, apiv1.ContainerNetworkStatePending, "")

	require.NoError(t, store.VerifyResourceLeaseHeld(ctx, network, leaseOwner))
}

func persistentTestNetwork() *apiv1.ContainerNetwork {
	return &apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app",
			Namespace: "default",
		},
		Spec: apiv1.ContainerNetworkSpec{
			NetworkName: "app",
			Persistent:  true,
		},
	}
}

func newTestNetworkReconciler(t *testing.T, config NetworkReconcilerConfig) *NetworkReconciler {
	t.Helper()

	lifetimeCtx, cancel := testutil.GetTestContext(t, controllerLeaseTestTimeout)
	cancel()
	reconciler := NewNetworkReconcilerWithConfig(lifetimeCtx, nil, nil, logr.Discard(), nil, nil, config)
	return reconciler
}

func openNetworkTestStore(t *testing.T, ctx context.Context) *statestore.Store {
	t.Helper()

	store, storeErr := statestore.Open(ctx, statestore.Options{
		Path:        filepath.Join(t.TempDir(), "state.sqlite3"),
		BusyTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, storeErr)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})
	return store
}
