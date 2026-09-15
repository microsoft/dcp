/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
)

func TestCleanupProxyPairObservesPhysicalContainerDeletion(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1.AddToScheme(scheme))
	require.NoError(t, apiv2.AddToScheme(scheme))

	proxyObjectID := types.UID("test-proxy")
	containerName := tunnelProxyPhysicalContainerNameForUID(proxyObjectID)
	physicalContainer := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      containerName.Name,
			Namespace: containerName.Namespace,
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(physicalContainer).Build()
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		ReconcilerBase: NewReconcilerBase[apiv1.ContainerNetworkTunnelProxy](client, client, logr.Discard(), ctx),
	}
	proxyData := &containerNetworkTunnelProxyData{
		ContainerNetworkTunnelProxyStatus: apiv1.ContainerNetworkTunnelProxyStatus{
			ClientProxyContainerID: "runtime-container",
		},
		cleanupScheduled: true,
	}

	reconciler.cleanupProxyPair(ctx, proxyData, proxyObjectID, logr.Discard())

	require.False(t, proxyData.cleanupScheduled)
	require.False(t, proxyData.cleanupCompleted)
	require.Equal(t, "runtime-container", proxyData.ClientProxyContainerID)

	proxyData.cleanupScheduled = true
	reconciler.cleanupProxyPair(ctx, proxyData, proxyObjectID, logr.Discard())

	require.True(t, proxyData.cleanupScheduled)
	require.True(t, proxyData.cleanupCompleted)
	require.Empty(t, proxyData.ClientProxyContainerID)
}
