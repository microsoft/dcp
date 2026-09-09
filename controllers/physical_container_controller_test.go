/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stretchr/testify/require"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestPhysicalContainerCreateResultDoesNotReplaceExistingOwner(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	containerID := "shared-container"
	existingOwner := types.NamespacedName{Namespace: "test", Name: "existing"}
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      "contender",
			UID:       types.UID("contender"),
		},
	}
	reconciler := NewPhysicalContainerReconciler(ctx, nil, nil, logr.Discard(), nil)
	reconciler.containerData.Store(existingOwner, physicalContainerDataContainerIDKey(containerID), &physicalContainerData{
		resourceUID: types.UID("existing"),
		state:       physicalContainerStateRuntime,
		progress:    physicalResourceProgressRunning,
		containerID: containerID,
	})
	createStateKey := physicalContainerDataKey(container)
	reconciler.containerData.Store(container.NamespacedName(), createStateKey, &physicalContainerData{
		resourceUID: container.UID,
		state:       physicalContainerStateCreate,
		progress:    physicalResourceProgressInProgress,
	})

	reconciler.queuePhysicalContainerDataResult(container, createStateKey, &physicalContainerData{
		resourceUID: container.UID,
		state:       physicalContainerStateCreate,
		progress:    physicalResourceProgressCompleted,
		containerID: containerID,
	})
	reconciler.containerData.RunDeferredOps(container.NamespacedName(), container)

	owner, ownedData := reconciler.containerData.BorrowByStateKey(physicalContainerDataContainerIDKey(containerID))
	require.Equal(t, existingOwner, owner)
	require.NotNil(t, ownedData)
	require.Equal(t, types.UID("existing"), ownedData.resourceUID)

	currentStateKey, currentData := reconciler.containerData.BorrowByNamespacedName(container.NamespacedName())
	require.Equal(t, createStateKey, currentStateKey)
	require.NotNil(t, currentData)
	require.Equal(t, physicalContainerStateResolve, currentData.state)
	require.Equal(t, physicalResourceProgressRetryPending, currentData.progress)
	require.Equal(t, containerID, currentData.containerID)
	require.Contains(t, currentData.failureMessage, existingOwner.Name)
}

func TestPhysicalContainerPortMappingFailurePreservesRuntimePhase(t *testing.T) {
	t.Parallel()

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Generation: 1,
		},
	}
	data := &physicalContainerData{}
	reconciler := &PhysicalContainerReconciler{}
	inspectedContainer := &containers.InspectedContainer{
		Id:     "container-id",
		Status: containers.ContainerStatusRunning,
		Ports: containers.InspectedContainerPortMapping{
			"invalid-port": nil,
		},
	}

	reconciler.applyInspectedPhysicalContainerStatus(container, data, inspectedContainer, logr.Discard())
	change, delay, valid := data.applyTo(container)

	require.True(t, valid)
	require.Equal(t, apiv2.PhysicalContainerPhaseRunning, container.Status.Phase)
	readyCondition := apimeta.FindStatusCondition(container.Status.Conditions, string(apiv2.ConditionReady))
	require.NotNil(t, readyCondition)
	require.Equal(t, metav1.ConditionFalse, readyCondition.Status)
	require.Equal(t, string(apiv2.PhysicalContainerReasonPortMappingResolutionFailed), readyCondition.Reason)
	require.Equal(t, LongDelay, delay)
	require.NotEqual(t, noChange, change&additionalReconciliationNeeded)
}
