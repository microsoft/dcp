/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/logs"
)

type physicalContainerImageDataStateKey string

type physicalContainerImageState int

const (
	physicalContainerImageStateBuild physicalContainerImageState = iota + 1
	physicalContainerImageStateBuildFailed
	physicalContainerImageStateBuildResultMissing
	physicalContainerImageStateBuilt
	physicalContainerImageStateInspectFailed
	physicalContainerImageStateInvalid
	physicalContainerImageStateMissing
	physicalContainerImageStatePull
	physicalContainerImageStatePullFailed
	physicalContainerImageStatePullRetry
	physicalContainerImageStatePulled
	physicalContainerImageStateReady
	physicalContainerImageStateResolve
	physicalContainerImageStateUnavailable
)

type physicalContainerImageOperation struct {
	cancel context.CancelFunc
}

type physicalContainerImageData struct {
	state                   physicalContainerImageState
	workInProgress          bool
	image                   string
	imageID                 string
	imageIDVerified         bool
	digest                  string
	tags                    []string
	failureMessage          string
	retryAfter              time.Time
	inspectionFailures      int
	inspectionFailureReason apiv2.ConditionReason

	// Cancels the queued pull or build operation. Set for as long as the operation may still be running.
	operation     *physicalContainerImageOperation
	startupStdout *logs.CaptureFile
	startupStderr *logs.CaptureFile
}

func (data *physicalContainerImageData) Clone() *physicalContainerImageData {
	return &physicalContainerImageData{
		state:                   data.state,
		workInProgress:          data.workInProgress,
		image:                   data.image,
		imageID:                 data.imageID,
		imageIDVerified:         data.imageIDVerified,
		digest:                  data.digest,
		tags:                    slices.Clone(data.tags),
		failureMessage:          data.failureMessage,
		retryAfter:              data.retryAfter,
		inspectionFailures:      data.inspectionFailures,
		inspectionFailureReason: data.inspectionFailureReason,
		operation:               data.operation,
		startupStdout:           data.startupStdout,
		startupStderr:           data.startupStderr,
	}
}

func (data *physicalContainerImageData) UpdateFrom(other *physicalContainerImageData) bool {
	if other == nil {
		return false
	}

	updated := false
	if data.inspectionFailures != other.inspectionFailures || data.inspectionFailureReason != other.inspectionFailureReason {
		data.inspectionFailures, data.inspectionFailureReason = other.inspectionFailures, other.inspectionFailureReason
		updated = true
	}
	if data.startupStdout != other.startupStdout || data.startupStderr != other.startupStderr {
		data.startupStdout, data.startupStderr = other.startupStdout, other.startupStderr
		updated = true
	}
	if data.state != other.state {
		data.state = other.state
		updated = true
	}
	if data.workInProgress != other.workInProgress {
		data.workInProgress = other.workInProgress
		updated = true
	}
	if data.imageID != other.imageID {
		data.imageID = other.imageID
		updated = true
	}
	if data.imageIDVerified != other.imageIDVerified {
		data.imageIDVerified = other.imageIDVerified
		updated = true
	}
	if data.digest != other.digest {
		data.digest = other.digest
		updated = true
	}
	if !slices.Equal(data.tags, other.tags) {
		data.tags = slices.Clone(other.tags)
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
	if data.operation != other.operation {
		data.operation = other.operation
		updated = true
	}

	return updated
}

func (data *physicalContainerImageData) operationInProgress() bool {
	return data != nil && data.workInProgress
}

func (data *physicalContainerImageData) logsComplete() bool {
	if data.operationInProgress() {
		return false
	}
	if data.imageID != "" {
		return true
	}
	switch data.state {
	case physicalContainerImageStatePullFailed, physicalContainerImageStateBuildFailed,
		physicalContainerImageStateBuildResultMissing, physicalContainerImageStateUnavailable,
		physicalContainerImageStateInvalid:
		return true
	default:
		return false
	}
}

func (data *physicalContainerImageData) applyTo(
	image *apiv2.PhysicalContainerImage,
) (objectChange, AdditionalReconciliationDelay, bool) {
	change := noChange
	if data.startupStdout != nil {
		change |= setValue(&image.Status.StdoutFile, data.startupStdout.Path)
		change |= setValue(&image.Status.StderrFile, data.startupStderr.Path)
	}
	change |= setValue(&image.Status.LogsComplete, data.logsComplete() ||
		(!image.DeletionTimestamp.IsZero() && !data.operationInProgress()))
	if data.imageID != "" && data.imageIDVerified {
		change |= setValue(&image.Status.ImageID, data.imageID)
		change |= setValue(&image.Status.Digest, data.digest)
		change |= setPhysicalContainerImageTags(image, data.tags)
	} else {
		change |= setValue(&image.Status.ImageID, "")
		change |= setValue(&image.Status.Digest, "")
		change |= setPhysicalContainerImageTags(image, nil)
	}
	if data.image != "" {
		change |= setValue(&image.Status.Image, data.image)
	}

	reason := apiv2.ConditionReason("")
	if data.state == physicalContainerImageStateBuildFailed || data.state == physicalContainerImageStateInvalid || data.state == physicalContainerImageStatePullFailed || data.state == physicalContainerImageStateUnavailable {
		reason = data.inspectionFailureReason
	}
	stateChange, delay, valid := physicalContainerImageProjections.applyWithReason(
		data.state,
		reason,
		data.failureMessage,
		&image.Status.Phase,
		&image.Status.Conditions,
		image.Generation,
	)
	return change | stateChange, delay, valid
}

var physicalContainerImageProjections = physicalResourceProjectionTable[physicalContainerImageState, apiv2.PhysicalContainerImagePhase]{
	invalidPhase: apiv2.PhysicalContainerImagePhaseUnknown,
	projections: map[physicalContainerImageState]physicalResourceProjection[apiv2.PhysicalContainerImagePhase]{
		physicalContainerImageStateResolve: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonContainerRuntimeUnhealthy,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalContainerImageStatePull: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonPulling,
			message:         "Image pull is in progress.",
		},
		physicalContainerImageStatePulled: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonPulled,
			message:         "Image pull completed.",
		},
		physicalContainerImageStatePullFailed: {
			phase: apiv2.PhysicalContainerImagePhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonPullFailed,
		},
		physicalContainerImageStateBuild: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonBuilding,
			message:         "Image build is in progress.",
		},
		physicalContainerImageStateBuilt: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonBuilt,
			message:         "Image build completed.",
		},
		physicalContainerImageStateBuildFailed: {
			phase: apiv2.PhysicalContainerImagePhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonBuildFailed,
		},
		physicalContainerImageStateReady: {
			phase: apiv2.PhysicalContainerImagePhaseReady, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.PhysicalContainerImageReasonImageAvailable,
			message:         "Image is available to the container runtime.",
		},
		physicalContainerImageStateInspectFailed: {
			phase: apiv2.PhysicalContainerImagePhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonRuntimeImageInspectFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalContainerImageStateMissing: {
			phase: apiv2.PhysicalContainerImagePhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonLocalImageNotFound,
			requeue:         true, requeueDelay: MonitoringDelay,
		},
		physicalContainerImageStateUnavailable: {
			phase: apiv2.PhysicalContainerImagePhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonLocalImageNotFound,
		},
		physicalContainerImageStatePullRetry: {
			phase: apiv2.PhysicalContainerImagePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonPullResultMissingImageID,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalContainerImageStateBuildResultMissing: {
			phase: apiv2.PhysicalContainerImagePhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerImageReasonBuildResultMissingImageID,
		},
		physicalContainerImageStateInvalid: {
			phase: apiv2.PhysicalContainerImagePhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}

func physicalContainerImageDataKey(image *apiv2.PhysicalContainerImage) physicalContainerImageDataStateKey {
	if image.UID != "" {
		return physicalContainerImageDataStateKey(image.UID)
	}
	return physicalContainerImageDataStateKey(image.NamespacedName().String())
}
