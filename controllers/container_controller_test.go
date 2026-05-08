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

func TestPersistentContainerLeaseHeldSkipsResourceUpdate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, storeErr := statestore.Open(ctx, statestore.Options{
		Path:        filepath.Join(t.TempDir(), "state.sqlite3"),
		BusyTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, storeErr)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	otherOwner := leaseOwner
	otherOwner.IdentityTime = otherOwner.IdentityTime.Add(-time.Hour)

	container := &apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api",
			Namespace: "default",
		},
		Spec: apiv1.ContainerSpec{
			ContainerName: "api",
			Persistent:    true,
		},
	}

	_, acquireErr := store.AcquireResourceLease(ctx, container, leaseOwner, time.Minute)
	require.NoError(t, acquireErr)

	reconciler := &ContainerReconciler{
		config: ContainerReconcilerConfig{
			StateStore:         store,
			ResourceLeaseOwner: otherOwner,
		},
	}

	updateCalled := false
	leaseErr := reconciler.withPersistentContainerResourceLease(ctx, container, logr.Discard(), func(context.Context) error {
		updateCalled = true
		return nil
	})

	require.ErrorIs(t, leaseErr, statestore.ErrResourceLeaseHeld)
	require.False(t, updateCalled)
}
