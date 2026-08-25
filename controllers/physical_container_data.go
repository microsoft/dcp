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

type physicalContainerData struct {
	resourceUID     types.UID
	conditionReason string
	containerID     string
	failureMessage  string
	retryAfter      time.Time
}

func newPhysicalContainerData(resourceUID types.UID) *physicalContainerData {
	return &physicalContainerData{
		conditionReason: apiv2.PhysicalContainerReasonCreating,
		resourceUID:     resourceUID,
	}
}

func (data *physicalContainerData) Clone() *physicalContainerData {
	return &physicalContainerData{
		resourceUID:     data.resourceUID,
		conditionReason: data.conditionReason,
		containerID:     data.containerID,
		failureMessage:  data.failureMessage,
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
	if data.containerID != other.containerID {
		data.containerID = other.containerID
		updated = true
	}
	if data.failureMessage != other.failureMessage {
		data.failureMessage = other.failureMessage
		updated = true
	}
	if !data.retryAfter.Equal(other.retryAfter) {
		data.retryAfter = other.retryAfter
		updated = true
	}

	return updated
}

func (data *physicalContainerData) operationInProgress() bool {
	return data.conditionReason == apiv2.PhysicalContainerReasonCreating ||
		data.conditionReason == apiv2.PhysicalContainerReasonCopyingFiles ||
		data.conditionReason == apiv2.PhysicalContainerReasonStarting
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
		apiv2.PhysicalContainerReasonFileCopyFailed,
		apiv2.PhysicalContainerReasonStartFailed,
		apiv2.PhysicalContainerReasonReconciliationFailed:
		change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
		change |= setCondition(&container.Status.Conditions, apiv2.ConditionReady, container.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
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
