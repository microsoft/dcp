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

type physicalContainerVolumeState int

const (
	physicalContainerVolumeStateCreate physicalContainerVolumeState = iota + 1
	physicalContainerVolumeStateCreateFailed
	physicalContainerVolumeStateCreateRetry
	physicalContainerVolumeStateCreated
	physicalContainerVolumeStateInspectFailed
	physicalContainerVolumeStateInvalid
	physicalContainerVolumeStateMissing
	physicalContainerVolumeStateReady
	physicalContainerVolumeStateRemovalAbandoned
	physicalContainerVolumeStateRemove
	physicalContainerVolumeStateRemoveRetry
	physicalContainerVolumeStateRemoved
	physicalContainerVolumeStateReplaceRetry
	physicalContainerVolumeStateReplaced
	physicalContainerVolumeStateResolve
)

type physicalContainerVolumeData struct {
	state          physicalContainerVolumeState
	workInProgress bool
	volumeID       string
	failureMessage string
	retryAfter     time.Time
	resolveByName  bool
}

func (data *physicalContainerVolumeData) operationInProgress() bool {
	return data != nil && data.workInProgress
}

func (data *physicalContainerVolumeData) Clone() *physicalContainerVolumeData {
	return &physicalContainerVolumeData{
		state:          data.state,
		workInProgress: data.workInProgress,
		volumeID:       data.volumeID,
		failureMessage: data.failureMessage,
		retryAfter:     data.retryAfter,
		resolveByName:  data.resolveByName,
	}
}

func (data *physicalContainerVolumeData) UpdateFrom(other *physicalContainerVolumeData) bool {
	if other == nil {
		return false
	}

	updated := false
	if data.state != other.state {
		data.state = other.state
		updated = true
	}
	if data.workInProgress != other.workInProgress {
		data.workInProgress = other.workInProgress
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

func (data *physicalContainerVolumeData) applyTo(
	volume *apiv2.PhysicalContainerVolume,
) (objectChange, AdditionalReconciliationDelay, bool) {
	change := noChange
	if data.volumeID != "" {
		change |= setValue(&volume.Status.VolumeID, data.volumeID)
	}

	stateChange, delay, valid := physicalContainerVolumeProjections.apply(
		data.state,

		data.failureMessage,
		&volume.Status.Phase,
		&volume.Status.Conditions,
		volume.Generation,
	)
	return change | stateChange, delay, valid
}

var physicalContainerVolumeProjections = physicalResourceProjectionTable[physicalContainerVolumeState, apiv2.PhysicalContainerVolumePhase]{
	invalidPhase: apiv2.PhysicalContainerVolumePhaseUnknown,
	projections: map[physicalContainerVolumeState]physicalResourceProjection[apiv2.PhysicalContainerVolumePhase]{
		physicalContainerVolumeStateCreate: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonCreating,
			message:         "Runtime volume creation is in progress.",
		},
		physicalContainerVolumeStateCreated: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonCreated,
			message:         "Runtime volume creation completed.",
		},
		physicalContainerVolumeStateCreateRetry: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonCreateFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalContainerVolumeStateCreateFailed: {
			phase: apiv2.PhysicalContainerVolumePhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonCreateFailed,
		},
		physicalContainerVolumeStateReplaceRetry: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalContainerVolumeStateReady: {
			phase: apiv2.PhysicalContainerVolumePhaseReady, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.PhysicalContainerVolumeReasonVolumeAvailable,
			message:         "Runtime volume is available.",
			requeue:         true, requeueDelay: MonitoringDelay,
		},
		physicalContainerVolumeStateMissing: {
			phase: apiv2.PhysicalContainerVolumePhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeMissing,
			message:         "Runtime volume was not found.",
			requeue:         true, requeueDelay: MonitoringDelay,
		},
		physicalContainerVolumeStateInspectFailed: {
			phase: apiv2.PhysicalContainerVolumePhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeInspectFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalContainerVolumeStateRemove: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoving,
			message:         "Runtime volume removal is in progress.",
		},
		physicalContainerVolumeStateRemoveRetry: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoveFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalContainerVolumeStateRemoved: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoved,
			message:         "Runtime volume removal completed.",
		},
		physicalContainerVolumeStateRemovalAbandoned: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemovalAbandoned,
		},
		physicalContainerVolumeStateInvalid: {
			phase: apiv2.PhysicalContainerVolumePhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}

func physicalContainerVolumeDataKey(volume *apiv2.PhysicalContainerVolume) physicalContainerVolumeDataStateKey {
	if volume.UID != "" {
		return physicalContainerVolumeDataStateKey(volume.UID)
	}
	return physicalContainerVolumeDataStateKey(volume.NamespacedName().String())
}
