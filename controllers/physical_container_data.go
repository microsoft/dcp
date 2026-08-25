/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type physicalContainerDataStateKey string

type physicalContainerOperationProgress int

const (
	physicalContainerOperationInProgress physicalContainerOperationProgress = iota + 1
	physicalContainerOperationCompleted
	physicalContainerOperationRetryPending
	physicalContainerOperationFailed
)

type physicalContainerData struct {
	// UID of the PhysicalContainer that owns this data.
	resourceUID types.UID

	// Current condition reason used to dispatch reconciliation behavior.
	conditionReason apiv2.ConditionReason

	// Progress of the current runtime operation.
	progress physicalContainerOperationProgress

	// ID of the associated runtime container, including a partially created container.
	containerID string

	// Diagnostic message from the current failed runtime operation.
	failureMessage string

	// Diagnostic message from the latest partial-container cleanup failure.
	cleanupMessage string

	// Earliest time at which a failed operation should be retried.
	retryAfter time.Time
}

func newPhysicalContainerData(resourceUID types.UID) *physicalContainerData {
	return &physicalContainerData{
		conditionReason: apiv2.PhysicalContainerReasonCreating,
		progress:        physicalContainerOperationInProgress,
		resourceUID:     resourceUID,
	}
}

func (data *physicalContainerData) Clone() *physicalContainerData {
	return &physicalContainerData{
		resourceUID:     data.resourceUID,
		conditionReason: data.conditionReason,
		progress:        data.progress,
		containerID:     data.containerID,
		failureMessage:  data.failureMessage,
		cleanupMessage:  data.cleanupMessage,
		retryAfter:      data.retryAfter,
	}
}

func (data *physicalContainerData) UpdateFrom(other *physicalContainerData) bool {
	if other == nil {
		return false
	}

	updated := false
	if data.conditionReason != other.conditionReason {
		data.conditionReason = other.conditionReason
		updated = true
	}
	if data.progress != other.progress {
		data.progress = other.progress
		updated = true
	}
	if data.containerID != other.containerID {
		data.containerID = other.containerID
		updated = true
	}
	if data.failureMessage != other.failureMessage {
		data.failureMessage = other.failureMessage
		updated = true
	}
	if data.cleanupMessage != other.cleanupMessage {
		data.cleanupMessage = other.cleanupMessage
		updated = true
	}
	if !data.retryAfter.Equal(other.retryAfter) {
		data.retryAfter = other.retryAfter
		updated = true
	}

	return updated
}

func (data *physicalContainerData) operationInProgress() bool {
	return data.progress == physicalContainerOperationInProgress
}

func (data *physicalContainerData) applyTo(container *apiv2.PhysicalContainer) objectChange {
	change := noChange
	if data.containerID != "" {
		change |= setValue(&container.Status.ContainerID, data.containerID)
	}

	switch data.conditionReason {
	case apiv2.PhysicalContainerReasonCreating:
		change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhasePending)
		change |= setCondition(&container.Status.Conditions, apiv2.ConditionReady, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonCreating, "Physical container creation is in progress.")
		return change
	case apiv2.PhysicalContainerReasonCopyingFiles:
		change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhasePending)
		change |= setCondition(&container.Status.Conditions, apiv2.ConditionReady, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonCopyingFiles, "Physical container file copy is in progress.")
		return change
	case apiv2.PhysicalContainerReasonStarting:
		change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhasePending)
		change |= setCondition(&container.Status.Conditions, apiv2.ConditionReady, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonStarting, "Physical container start is in progress.")
		return change
	case apiv2.PhysicalContainerReasonCreateFailed,
		apiv2.PhysicalContainerReasonExistingContainerReplacementFailed:
		if data.progress == physicalContainerOperationRetryPending {
			change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhasePending)
		} else {
			change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
		}
		change |= setCondition(&container.Status.Conditions, apiv2.ConditionReady, container.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
		return change
	case apiv2.PhysicalContainerReasonPartialContainerCleanupFailed:
		if data.progress == physicalContainerOperationRetryPending {
			change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhasePending)
		} else {
			change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
		}
		change |= setCondition(&container.Status.Conditions, apiv2.ConditionReady, container.Generation, metav1.ConditionFalse, data.conditionReason, data.cleanupMessage)
		return change
	case apiv2.PhysicalContainerReasonFileCopyFailed,
		apiv2.PhysicalContainerReasonStartFailed:
		change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
		message := data.failureMessage
		if data.cleanupMessage != "" {
			message = data.cleanupMessage
		}
		change |= setCondition(&container.Status.Conditions, apiv2.ConditionReady, container.Generation, metav1.ConditionFalse, data.conditionReason, message)
		return change
	default:
		return change
	}
}

func storeStartedPhysicalContainerData(
	containerData *ObjectStateMap[physicalContainerDataStateKey, physicalContainerData, *physicalContainerData, *apiv2.PhysicalContainer],
	container *apiv2.PhysicalContainer,
	containerID string,
) {
	startedData := &physicalContainerData{
		resourceUID:     container.UID,
		conditionReason: apiv2.PhysicalContainerReasonStarted,
		progress:        physicalContainerOperationCompleted,
		containerID:     containerID,
	}
	updated := containerData.UpdateChangingStateKey(
		container.NamespacedName(),
		physicalContainerDataKey(container),
		physicalContainerDataContainerIDKey(containerID),
		startedData,
	)
	if !updated {
		containerData.Store(container.NamespacedName(), physicalContainerDataContainerIDKey(containerID), startedData)
	}
}

func physicalContainerDataKey(container *apiv2.PhysicalContainer) physicalContainerDataStateKey {
	if container.UID != "" {
		return physicalContainerDataStateKey(container.UID)
	}
	return physicalContainerDataStateKey(container.NamespacedName().String())
}

func physicalContainerDataContainerIDKey(containerID string) physicalContainerDataStateKey {
	return physicalContainerDataStateKey(containerID)
}
