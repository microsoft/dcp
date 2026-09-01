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
	physicalContainerVolumeStateNamespace physicalContainerVolumeState = iota + 1
	physicalContainerVolumeStateResolve
	physicalContainerVolumeStateCreate
	physicalContainerVolumeStateReplace
	physicalContainerVolumeStateRuntime
	physicalContainerVolumeStateRemove
)

const (
	physicalContainerVolumeOperationInProgress   = physicalResourceProgressInProgress
	physicalContainerVolumeOperationCompleted    = physicalResourceProgressCompleted
	physicalContainerVolumeOperationRetryPending = physicalResourceProgressRetryPending
	physicalContainerVolumeOperationFailed       = physicalResourceProgressFailed
)

type physicalContainerVolumeData struct {
	state          physicalContainerVolumeState
	progress       physicalResourceProgress
	volumeID       string
	failureMessage string
	retryAfter     time.Time
	resolveByName  bool
}

func (data *physicalContainerVolumeData) Clone() *physicalContainerVolumeData {
	return &physicalContainerVolumeData{
		state:          data.state,
		progress:       data.progress,
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

	stateChange, _, _ := physicalContainerVolumeProjections.apply(
		data.state,
		data.progress,
		data.failureMessage,
		&volume.Status.Phase,
		&volume.Status.Conditions,
		volume.Generation,
	)
	return change | stateChange
}

var physicalContainerVolumeProjections = physicalResourceProjectionTable[physicalContainerVolumeState, apiv2.PhysicalContainerVolumePhase]{
	invalidPhase: apiv2.PhysicalContainerVolumePhaseUnknown,
	projections: map[physicalResourceProjectionKey[physicalContainerVolumeState]]physicalResourceProjection[apiv2.PhysicalContainerVolumePhase]{
		{state: physicalContainerVolumeStateNamespace, progress: physicalResourceProgressNotFound}: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotFound,
		},
		{state: physicalContainerVolumeStateNamespace, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotReady,
		},
		{state: physicalContainerVolumeStateNamespace, progress: physicalResourceProgressTerminating}: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceTerminating,
		},
		{state: physicalContainerVolumeStateNamespace, progress: physicalResourceProgressNotActive}: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotActive,
		},
		{state: physicalContainerVolumeStateNamespace, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerVolumePhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceLookupFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerVolumeStateCreate, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonCreating,
			message:         "Runtime volume creation is in progress.",
		},
		{state: physicalContainerVolumeStateCreate, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonCreated,
			message:         "Runtime volume creation completed.",
		},
		{state: physicalContainerVolumeStateCreate, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonCreateFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerVolumeStateCreate, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalContainerVolumePhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonCreateFailed,
		},
		{state: physicalContainerVolumeStateReplace, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerVolumeStateRuntime, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.PhysicalContainerVolumePhaseReady, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.PhysicalContainerVolumeReasonVolumeAvailable,
			message:         "Runtime volume is available.",
			requeue:         true, requeueDelay: MonitoringDelay,
		},
		{state: physicalContainerVolumeStateRuntime, progress: physicalResourceProgressMissing}: {
			phase: apiv2.PhysicalContainerVolumePhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeMissing,
			message:         "Runtime volume was not found.",
			requeue:         true, requeueDelay: MonitoringDelay,
		},
		{state: physicalContainerVolumeStateRuntime, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerVolumePhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeInspectFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerVolumeStateRemove, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoving,
			message:         "Runtime volume removal is in progress.",
		},
		{state: physicalContainerVolumeStateRemove, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoveFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerVolumeStateRemove, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoved,
			message:         "Runtime volume removal completed.",
		},
		{state: physicalContainerVolumeStateRemove, progress: physicalResourceProgressAbandoned}: {
			phase: apiv2.PhysicalContainerVolumePhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemovalAbandoned,
		},
	},
}

func physicalContainerVolumeDataKey(volume *apiv2.PhysicalContainerVolume) physicalContainerVolumeDataStateKey {
	if volume.UID != "" {
		return physicalContainerVolumeDataStateKey(volume.UID)
	}
	return physicalContainerVolumeDataStateKey(volume.NamespacedName().String())
}
