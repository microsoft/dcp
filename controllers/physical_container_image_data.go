/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type physicalContainerImageDataStateKey string

type physicalContainerImageState int

const (
	physicalContainerImageStateNamespace physicalContainerImageState = iota + 1
	physicalContainerImageStateResolve
	physicalContainerImageStatePull
	physicalContainerImageStateBuild
	physicalContainerImageStateRuntime
	physicalContainerImageStateDelete
	physicalContainerImageStateInvalid
)

type physicalContainerImageData struct {
	state          physicalContainerImageState
	progress       physicalResourceProgress
	image          string
	imageID        string
	failureMessage string
	retryAfter     time.Time

	// Cancels the queued pull or build operation. Set for as long as the operation may still be running.
	cancelOperation context.CancelFunc
}

func (data *physicalContainerImageData) Clone() *physicalContainerImageData {
	return &physicalContainerImageData{
		state:           data.state,
		progress:        data.progress,
		image:           data.image,
		imageID:         data.imageID,
		failureMessage:  data.failureMessage,
		retryAfter:      data.retryAfter,
		cancelOperation: data.cancelOperation,
	}
}

func (data *physicalContainerImageData) UpdateFrom(other *physicalContainerImageData) bool {
	if other == nil {
		return false
	}

	updated := false
	if data.state != other.state {
		data.state = other.state
		updated = true
	}
	if data.progress != other.progress {
		data.progress = other.progress
		updated = true
	}
	if data.imageID != other.imageID {
		data.imageID = other.imageID
		updated = true
	}
	if data.image != other.image {
		data.image = other.image
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

func (data *physicalContainerImageData) operationInProgress() bool {
	return data.progress == physicalResourceProgressInProgress
}

func (data *physicalContainerImageData) applyTo(
	image *apiv2.PhysicalContainerImage,
) (objectChange, AdditionalReconciliationDelay, bool) {
	change := noChange
	if data.imageID != "" {
		change |= setValue(&image.Status.ImageID, data.imageID)
	}
	if data.image != "" {
		change |= setValue(&image.Status.Image, data.image)
	}

	stateChange, delay, valid := physicalContainerImageProjections.apply(
		data.state,
		data.progress,
		data.failureMessage,
		&image.Status.Phase,
		&image.Status.Conditions,
		image.Generation,
	)
	return change | stateChange, delay, valid
}

var physicalContainerImageProjections = physicalResourceProjectionTable[physicalContainerImageState, apiv2.PhysicalContainerImagePhase]{
	invalidPhase: apiv2.PhysicalContainerImagePhaseUnknown,
	projections: map[physicalResourceProjectionKey[physicalContainerImageState]]physicalResourceProjection[apiv2.PhysicalContainerImagePhase]{
		{state: physicalContainerImageStateNamespace, progress: physicalResourceProgressNotFound}: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotFound,
		},
		{state: physicalContainerImageStateNamespace, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotReady,
		},
		{state: physicalContainerImageStateNamespace, progress: physicalResourceProgressTerminating}: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceTerminating,
		},
		{state: physicalContainerImageStateNamespace, progress: physicalResourceProgressNotActive}: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotActive,
		},
		{state: physicalContainerImageStateNamespace, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerImagePhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceLookupFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerImageStatePull, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonPulling,
			message:         "Image pull is in progress.",
		},
		{state: physicalContainerImageStatePull, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonPulled,
			message:         "Image pull completed.",
		},
		{state: physicalContainerImageStatePull, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalContainerImagePhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonPullFailed,
		},
		{state: physicalContainerImageStateBuild, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonBuilding,
			message:         "Image build is in progress.",
		},
		{state: physicalContainerImageStateBuild, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonBuilt,
			message:         "Image build completed.",
		},
		{state: physicalContainerImageStateBuild, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalContainerImagePhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonBuildFailed,
		},
		{state: physicalContainerImageStateRuntime, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.PhysicalContainerImagePhaseReady, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.PhysicalContainerImageReasonImageAvailable,
			message:         "Image is available to the container runtime.",
		},
		{state: physicalContainerImageStateRuntime, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerImagePhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonRuntimeImageInspectFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerImageStateRuntime, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalContainerImagePhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonLocalImageNotFound,
		},
		{state: physicalContainerImageStatePull, progress: physicalResourceProgressResultMissing}: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonPullResultMissingImageID,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerImageStateBuild, progress: physicalResourceProgressResultMissing}: {
			phase: apiv2.PhysicalContainerImagePhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonBuildResultMissingImageID,
		},
		{state: physicalContainerImageStateInvalid, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalContainerImagePhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
			requeue:         true, requeueDelay: LongDelay,
		},
	},
}

func physicalContainerImageDataKey(image *apiv2.PhysicalContainerImage) physicalContainerImageDataStateKey {
	if image.UID != "" {
		return physicalContainerImageDataStateKey(image.UID)
	}
	return physicalContainerImageDataStateKey(image.NamespacedName().String())
}
