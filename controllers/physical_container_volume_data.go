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

type physicalContainerVolumeData struct {
	conditionReason string
	volumeID        string
	failureMessage  string
	retryAfter      time.Time
}

func (data *physicalContainerVolumeData) Clone() *physicalContainerVolumeData {
	return &physicalContainerVolumeData{
		conditionReason: data.conditionReason,
		volumeID:        data.volumeID,
		failureMessage:  data.failureMessage,
		retryAfter:      data.retryAfter,
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

	return updated
}

func (data *physicalContainerVolumeData) operationInProgress() bool {
	return data.conditionReason == apiv2.PhysicalContainerVolumeReasonCreating
}

func (data *physicalContainerVolumeData) applyTo(volume *apiv2.PhysicalContainerVolume) objectChange {
	change := noChange
	if data.volumeID != "" {
		change |= setValue(&volume.Status.VolumeID, data.volumeID)
	}

	switch data.conditionReason {
	case apiv2.PhysicalContainerVolumeReasonCreating:
		change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhasePending)
		change |= setReadyCondition(&volume.Status.Conditions, volume.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonCreating, "Runtime volume creation is in progress.")
	case apiv2.PhysicalContainerVolumeReasonCreateFailed,
		apiv2.PhysicalContainerVolumeReasonReconciliationFailed:
		change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhaseFailed)
		change |= setReadyCondition(&volume.Status.Conditions, volume.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
	}

	return change
}

func physicalContainerVolumeDataKey(volume *apiv2.PhysicalContainerVolume) physicalContainerVolumeDataStateKey {
	if volume.UID != "" {
		return physicalContainerVolumeDataStateKey(volume.UID)
	}
	return physicalContainerVolumeDataStateKey(volume.NamespacedName().String())
}
