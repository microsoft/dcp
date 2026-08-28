/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type physicalContainerVolumeDataStateKey string

type physicalContainerVolumeOperationProgress int

const (
	physicalContainerVolumeOperationInProgress physicalContainerVolumeOperationProgress = iota + 1
	physicalContainerVolumeOperationCompleted
	physicalContainerVolumeOperationRetryPending
	physicalContainerVolumeOperationFailed
)

type physicalContainerVolumeData struct {
	conditionReason apiv2.ConditionReason
	progress        physicalContainerVolumeOperationProgress
	volumeID        string
	failureMessage  string
	retryAfter      time.Time
	resolveByName   bool
}

func (data *physicalContainerVolumeData) Clone() *physicalContainerVolumeData {
	return &physicalContainerVolumeData{
		conditionReason: data.conditionReason,
		progress:        data.progress,
		volumeID:        data.volumeID,
		failureMessage:  data.failureMessage,
		retryAfter:      data.retryAfter,
		resolveByName:   data.resolveByName,
	}
}

func (data *physicalContainerVolumeData) UpdateFrom(other *physicalContainerVolumeData) bool {
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
	if data.volumeID != other.volumeID {
		data.volumeID = other.volumeID
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
	if data.resolveByName != other.resolveByName {
		data.resolveByName = other.resolveByName
		updated = true
	}

	return updated
}

func (data *physicalContainerVolumeData) applyTo(volume *apiv2.PhysicalContainerVolume) objectChange {
	change := noChange
	if data.volumeID != "" {
		change |= setValue(&volume.Status.VolumeID, data.volumeID)
	}

	switch data.conditionReason {
	case apiv2.PhysicalContainerVolumeReasonCreating:
		change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhasePending)
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonCreating, "Runtime volume creation is in progress.")
	case apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoving:
		change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhasePending)
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoving, "Runtime volume removal is in progress.")
	case apiv2.PhysicalContainerVolumeReasonCreateFailed,
		apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed:
		if data.progress == physicalContainerVolumeOperationRetryPending {
			change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhasePending)
		} else {
			change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhaseFailed)
		}
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
	case apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoveFailed:
		change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhasePending)
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
	case apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoved:
		change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhasePending)
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, data.conditionReason, "Runtime volume removal completed.")
	case apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemovalAbandoned:
		change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhasePending)
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
	}

	return change
}

func physicalContainerVolumeDataKey(volume *apiv2.PhysicalContainerVolume) physicalContainerVolumeDataStateKey {
	if volume.UID != "" {
		return physicalContainerVolumeDataStateKey(volume.UID)
	}
	return physicalContainerVolumeDataStateKey(volume.NamespacedName().String())
}
