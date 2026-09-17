/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

func TestPhysicalContainerDependenciesRejectTerminatingResources(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	deletionTimestamp := metav1.Now()
	finalizers := []string{"test-finalizer"}

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "image",
			Namespace:         "namespace",
			DeletionTimestamp: &deletionTimestamp,
			Finalizers:        finalizers,
		},
		Status: apiv2.PhysicalContainerImageStatus{
			Phase:   apiv2.PhysicalContainerImagePhaseReady,
			ImageID: "image-id",
		},
	}
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "volume",
			Namespace:         "namespace",
			DeletionTimestamp: &deletionTimestamp,
			Finalizers:        finalizers,
		},
		Status: apiv2.PhysicalContainerVolumeStatus{
			Phase:    apiv2.PhysicalContainerVolumePhaseReady,
			VolumeID: "volume-id",
		},
	}
	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "network",
			Namespace:         "namespace",
			DeletionTimestamp: &deletionTimestamp,
			Finalizers:        finalizers,
		},
		Status: apiv2.PhysicalContainerNetworkStatus{
			Phase:     apiv2.PhysicalContainerNetworkPhaseReady,
			NetworkID: "network-id",
		},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(image, volume, network).
		Build()
	reconciler := &PhysicalContainerReconciler{
		ReconcilerBase: NewReconcilerBase[apiv2.PhysicalContainer](client, client, logr.Discard(), ctx),
	}
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: "namespace"},
		Spec: apiv2.PhysicalContainerSpec{
			Container: &apiv2.PhysicalContainerConfig{
				ImageRef: image.Name,
				VolumeMounts: []apiv2.VolumeMount{{
					Type:      apiv2.NamedVolumeMount,
					VolumeRef: volume.Name,
					Target:    "/data",
				}},
				Networks: []apiv2.ContainerNetworkConnectionConfig{{
					Name: network.Name,
				}},
			},
		},
	}

	imageReady, _, imageProgress, imageMessage, _ := reconciler.resolvePhysicalContainerImage(ctx, container, logr.Discard())
	require.False(t, imageReady)
	require.Equal(t, physicalResourceProgressNotReady, imageProgress)
	require.Equal(t, `PhysicalContainerImage "image" is terminating.`, imageMessage)

	volumesReady, _, volumeProgress, volumeMessage := reconciler.resolvePhysicalContainerVolumes(ctx, container, logr.Discard())
	require.False(t, volumesReady)
	require.Equal(t, physicalResourceProgressNotReady, volumeProgress)
	require.Equal(t, `PhysicalContainerVolume "volume" is terminating.`, volumeMessage)

	networksReady, _, networkProgress, networkMessage := reconciler.resolvePhysicalContainerNetworks(ctx, container, logr.Discard())
	require.False(t, networksReady)
	require.Equal(t, physicalResourceProgressNotReady, networkProgress)
	require.Equal(t, `PhysicalContainerNetwork "network" is terminating.`, networkMessage)
}
