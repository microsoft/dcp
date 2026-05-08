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
)

func TestPersistentNetworkFailsToStartWithoutStateStore(t *testing.T) {
	t.Parallel()

	network := persistentTestNetwork()
	reconciler := newTestNetworkReconciler(t, NetworkReconcilerConfig{})

	change := reconciler.ensureNetwork(context.Background(), network, logr.Discard())

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

	ctx := context.Background()
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

	ctx := context.Background()
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

	lifetimeCtx, cancel := context.WithCancel(context.Background())
	reconciler := NewNetworkReconcilerWithConfig(lifetimeCtx, nil, nil, logr.Discard(), nil, nil, config)
	cancel()
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
