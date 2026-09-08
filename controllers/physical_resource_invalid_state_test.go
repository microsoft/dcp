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

	apiv2 "github.com/microsoft/dcp/api/v2"
)

func TestUnknownPhysicalContainerStateBecomesTerminal(t *testing.T) {
	t.Parallel()

	data := &physicalContainerData{
		state:          physicalContainerStateStop,
		progress:       physicalResourceProgressCompleted,
		cleanupMessage: "old cleanup failure",
	}

	change := handleUnknownPhysicalContainerDataReason(
		t.Context(),
		nil,
		&apiv2.PhysicalContainer{},
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, noChange, change)
	require.Equal(t, physicalContainerStateInvalid, data.state)
	require.Equal(t, physicalResourceProgressFailed, data.progress)
	require.Equal(t, "Physical container reached invalid reconciliation state Stop with progress Completed.", data.failureMessage)
	require.Empty(t, data.cleanupMessage)
}

func TestUnknownPhysicalContainerImageStateBecomesTerminal(t *testing.T) {
	t.Parallel()

	data := &physicalContainerImageData{
		state:    physicalContainerImageStateDelete,
		progress: physicalResourceProgressCompleted,
	}

	change := handleUnknownPhysicalContainerImageState(
		t.Context(),
		nil,
		&apiv2.PhysicalContainerImage{},
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, noChange, change)
	require.Equal(t, physicalContainerImageStateInvalid, data.state)
	require.Equal(t, physicalResourceProgressFailed, data.progress)
	require.Equal(t, "PhysicalContainerImage reached invalid reconciliation state Delete with progress Completed.", data.failureMessage)
}

func TestUnknownPhysicalContainerNetworkStateBecomesTerminal(t *testing.T) {
	t.Parallel()

	data := &physicalContainerNetworkData{
		state:    physicalContainerNetworkStateRuntime,
		progress: physicalResourceProgressInProgress,
	}

	change := handleUnknownPhysicalContainerNetworkDataReason(
		t.Context(),
		nil,
		&apiv2.PhysicalContainerNetwork{},
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, noChange, change)
	require.Equal(t, physicalContainerNetworkStateInvalid, data.state)
	require.Equal(t, physicalResourceProgressFailed, data.progress)
	require.Equal(t, "Physical container network reached invalid reconciliation state Runtime with progress InProgress.", data.failureMessage)
}

func TestUnknownPhysicalContainerVolumeStateBecomesTerminal(t *testing.T) {
	t.Parallel()

	data := &physicalContainerVolumeData{
		state:    physicalContainerVolumeStateRemove,
		progress: physicalResourceProgressCompleted,
	}

	change := handleUnknownPhysicalContainerVolumeDataReason(
		t.Context(),
		nil,
		&apiv2.PhysicalContainerVolume{},
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, noChange, change)
	require.Equal(t, physicalContainerVolumeStateInvalid, data.state)
	require.Equal(t, physicalResourceProgressFailed, data.progress)
	require.Equal(t, "Physical container volume reached invalid reconciliation state Remove with progress Completed.", data.failureMessage)
}

func TestInvalidPhysicalContainerNetworkStillHandlesDeletion(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "network",
			Namespace:         "test",
			Finalizers:        []string{physicalContainerNetworkFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			Network: &apiv2.PhysicalContainerNetworkConfig{RetainRuntimeNetwork: true},
		},
	}
	data := &physicalContainerNetworkData{
		state:    physicalContainerNetworkStateInvalid,
		progress: physicalResourceProgressFailed,
	}
	reconciler := &PhysicalContainerNetworkReconciler{
		networkData: NewObjectStateMap[
			physicalContainerNetworkDataStateKey,
			physicalContainerNetworkData,
			*physicalContainerNetworkData,
			*apiv2.PhysicalContainerNetwork,
		](),
	}
	reconciler.networkData.Store(network.NamespacedName(), physicalContainerNetworkDataKey(network), data)

	change := handlePhysicalContainerNetworkTerminal(
		t.Context(),
		reconciler,
		network,
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, metadataChanged, change)
	require.False(t, hasFinalizer(network, physicalContainerNetworkFinalizer))
	_, storedData := reconciler.networkData.BorrowByNamespacedName(network.NamespacedName())
	require.Nil(t, storedData)
}

func TestInvalidPhysicalContainerVolumeStillHandlesDeletion(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "volume",
			Namespace:         "test",
			Finalizers:        []string{physicalContainerVolumeFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: apiv2.PhysicalContainerVolumeSpec{
			Volume: &apiv2.PhysicalContainerVolumeConfig{RetainRuntimeVolume: true},
		},
	}
	data := &physicalContainerVolumeData{
		state:    physicalContainerVolumeStateInvalid,
		progress: physicalResourceProgressFailed,
	}
	reconciler := &PhysicalContainerVolumeReconciler{
		volumeData: NewObjectStateMap[
			physicalContainerVolumeDataStateKey,
			physicalContainerVolumeData,
			*physicalContainerVolumeData,
			*apiv2.PhysicalContainerVolume,
		](),
	}
	reconciler.volumeData.Store(volume.NamespacedName(), physicalContainerVolumeDataKey(volume), data)

	change := handlePhysicalContainerVolumeTerminal(
		t.Context(),
		reconciler,
		volume,
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, metadataChanged, change)
	require.False(t, hasFinalizer(volume, physicalContainerVolumeFinalizer))
	_, storedData := reconciler.volumeData.BorrowByNamespacedName(volume.NamespacedName())
	require.Nil(t, storedData)
}
