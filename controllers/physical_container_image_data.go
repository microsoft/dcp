/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type physicalContainerImageDataStateKey string

type physicalContainerImageData struct {
	conditionReason string
	imageID         string
	failureMessage  string
}

func (data *physicalContainerImageData) Clone() *physicalContainerImageData {
	return &physicalContainerImageData{
		conditionReason: data.conditionReason,
		imageID:         data.imageID,
		failureMessage:  data.failureMessage,
	}
}

func (data *physicalContainerImageData) UpdateFrom(other *physicalContainerImageData) bool {
	if other == nil {
		return false
	}

	updated := false
	if data.conditionReason != other.conditionReason {
		data.conditionReason = other.conditionReason
		updated = true
	}
	if data.imageID != other.imageID {
		data.imageID = other.imageID
		updated = true
	}
	if data.failureMessage != other.failureMessage {
		data.failureMessage = other.failureMessage
		updated = true
	}

	return updated
}

func (data *physicalContainerImageData) applyTo(image *apiv2.PhysicalContainerImage) objectChange {
	change := noChange
	if data.imageID != "" {
		change |= setValue(&image.Status.ImageID, data.imageID)
	}

	switch data.conditionReason {
	case apiv2.PhysicalContainerImageReasonPulling:
		change |= setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhasePending)
		change |= setReadyCondition(&image.Status.Conditions, image.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonPulling, "Image pull is in progress.")
		return change
	case apiv2.PhysicalContainerImageReasonBuilding:
		change |= setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhasePending)
		change |= setReadyCondition(&image.Status.Conditions, image.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonBuilding, "Image build is in progress.")
		return change
	case apiv2.PhysicalContainerImageReasonPullFailed, apiv2.PhysicalContainerImageReasonBuildFailed:
		change |= setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhaseFailed)
		change |= setReadyCondition(&image.Status.Conditions, image.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
		return change
	default:
		return change
	}
}

func physicalContainerImageDataKey(image *apiv2.PhysicalContainerImage) physicalContainerImageDataStateKey {
	if image.UID != "" {
		return physicalContainerImageDataStateKey(image.UID)
	}
	return physicalContainerImageDataStateKey(image.NamespacedName().String())
}
