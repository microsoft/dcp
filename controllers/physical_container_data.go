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

type physicalContainerState int

const (
	physicalContainerStateNamespace physicalContainerState = iota + 1
	physicalContainerStateResolve
	physicalContainerStateImage
	physicalContainerStateCreate
	physicalContainerStateReplace
	physicalContainerStateCopyFiles
	physicalContainerStateStart
	physicalContainerStateCleanup
	physicalContainerStateRuntime
	physicalContainerStateStop
	physicalContainerStateRemove
	physicalContainerStatePortMapping
	physicalContainerStateInvalid
)

const (
	physicalContainerOperationInProgress   = physicalResourceProgressInProgress
	physicalContainerOperationCompleted    = physicalResourceProgressCompleted
	physicalContainerOperationRetryPending = physicalResourceProgressRetryPending
	physicalContainerOperationFailed       = physicalResourceProgressFailed
)

type physicalContainerData struct {
	// UID of the PhysicalContainer that owns this data.
	resourceUID types.UID

	// Current reconciliation concern.
	state physicalContainerState

	// Progress of the current runtime operation.
	progress physicalResourceProgress

	// ID of the associated runtime container, including a partially created container.
	containerID string

	// Image name resolved from the referenced PhysicalContainerImage.
	image string

	// Diagnostic message from the current failed runtime operation.
	failureMessage string

	// Diagnostic message from the latest partial-container cleanup failure.
	cleanupMessage string

	// Earliest time at which a failed operation should be retried.
	retryAfter time.Time
}

func (data *physicalContainerData) Clone() *physicalContainerData {
	return &physicalContainerData{
		resourceUID:    data.resourceUID,
		state:          data.state,
		progress:       data.progress,
		containerID:    data.containerID,
		image:          data.image,
		failureMessage: data.failureMessage,
		cleanupMessage: data.cleanupMessage,
		retryAfter:     data.retryAfter,
	}
}

func (data *physicalContainerData) UpdateFrom(other *physicalContainerData) bool {
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
	if data.containerID != other.containerID {
		data.containerID = other.containerID
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
	if data.image != "" {
		change |= setValue(&container.Status.Image, data.image)
	}

	message := data.failureMessage
	if data.state == physicalContainerStateCleanup || data.cleanupMessage != "" {
		message = data.cleanupMessage
	}
	stateChange, _, _ := physicalContainerProjections.apply(
		data.state,
		data.progress,
		message,
		&container.Status.Phase,
		&container.Status.Conditions,
		container.Generation,
	)
	return change | stateChange
}

var physicalContainerProjections = physicalResourceProjectionTable[physicalContainerState, apiv2.PhysicalContainerPhase]{
	invalidPhase: apiv2.PhysicalContainerPhaseUnknown,
	projections: map[physicalResourceProjectionKey[physicalContainerState]]physicalResourceProjection[apiv2.PhysicalContainerPhase]{
		{state: physicalContainerStateNamespace, progress: physicalResourceProgressNotFound}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalResourceReasonNamespaceNotFound,
		},
		{state: physicalContainerStateNamespace, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalResourceReasonNamespaceNotReady,
		},
		{state: physicalContainerStateNamespace, progress: physicalResourceProgressTerminating}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalResourceReasonNamespaceTerminating,
		},
		{state: physicalContainerStateNamespace, progress: physicalResourceProgressNotActive}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalResourceReasonNamespaceNotActive,
		},
		{state: physicalContainerStateNamespace, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalResourceReasonNamespaceLookupFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerStateImage, progress: physicalResourceProgressNotFound}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonImageNotFound,
		},
		{state: physicalContainerStateImage, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonImageNotReady,
		},
		{state: physicalContainerStateImage, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonImageLookupFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerStateCreate, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonCreating,
			message: "Physical container creation is in progress.",
		},
		{state: physicalContainerStateCreate, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonCreated,
			message: "Physical container creation completed.",
		},
		{state: physicalContainerStateCreate, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonCreateFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerStateCreate, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonCreateFailed,
		},
		{state: physicalContainerStateReplace, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonExistingContainerReplacementFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerStateCopyFiles, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonCopyingFiles,
			message: "Physical container file copy is in progress.",
		},
		{state: physicalContainerStateCopyFiles, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonFilesCreated,
			message: "Physical container file copy completed.",
		},
		{state: physicalContainerStateCopyFiles, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonFileCopyFailed,
		},
		{state: physicalContainerStateStart, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonStarting,
			message: "Physical container start is in progress.",
		},
		{state: physicalContainerStateStart, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonStarted,
			message: "Physical container start completed.",
		},
		{state: physicalContainerStateStart, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonStartFailed,
		},
		{state: physicalContainerStateCleanup, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonPartialContainerCleanupFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerStateCleanup, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonPartialContainerCleanupFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerStateRuntime, progress: physicalResourceProgressRunning}: {
			phase: apiv2.PhysicalContainerPhaseRunning, conditionStatus: metav1.ConditionTrue, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerRunning,
			message: "Runtime container is running.", requeue: true, requeueDelay: MonitoringDelay,
		},
		{state: physicalContainerStateRuntime, progress: physicalResourceProgressPaused}: {
			phase: apiv2.PhysicalContainerPhasePaused, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerPaused,
			message: "Runtime container is paused.", requeue: true, requeueDelay: MonitoringDelay,
		},
		{state: physicalContainerStateRuntime, progress: physicalResourceProgressRestarting}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerRestarting,
			message: "Runtime container is restarting.", requeue: true,
		},
		{state: physicalContainerStateRuntime, progress: physicalResourceProgressCreated}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerCreated,
			message: "Runtime container has been created but is not running.", requeue: true, requeueDelay: MonitoringDelay,
		},
		{state: physicalContainerStateRuntime, progress: physicalResourceProgressRemoving}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerRemoving,
			message: "Runtime container is being removed.", requeue: true, requeueDelay: MonitoringDelay,
		},
		{state: physicalContainerStateRuntime, progress: physicalResourceProgressExited}: {
			phase: apiv2.PhysicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerExited,
			message: "Runtime container has exited.",
		},
		{state: physicalContainerStateRuntime, progress: physicalResourceProgressDead}: {
			phase: apiv2.PhysicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerDead,
			message: "Runtime container is dead.",
		},
		{state: physicalContainerStateRuntime, progress: physicalResourceProgressUnknown}: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerStatusUnknown,
			requeue: true,
		},
		{state: physicalContainerStateRuntime, progress: physicalResourceProgressMissing}: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerMissing,
			message: "Runtime container was not found.",
		},
		{state: physicalContainerStateRuntime, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerInspectFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerStateStop, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerStopFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerStateRemove, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerRemoveFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerStateResolve, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerAlreadyTracked,
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerStatePortMapping, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonPortMappingResolutionFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerStateInvalid, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
			requeue: true, requeueDelay: LongDelay,
		},
	},
}

// Claims the runtime container identity for the resource. On success the caller's reconciliation
// state is replaced with the newly recorded state.
func storeStartedPhysicalContainerData(
	containerData *ObjectStateMap[physicalContainerDataStateKey, physicalContainerData, *physicalContainerData, *apiv2.PhysicalContainer],
	container *apiv2.PhysicalContainer,
	containerID string,
	data *physicalContainerData,
) (types.NamespacedName, bool) {
	startedData := &physicalContainerData{
		resourceUID: container.UID,
		state:       physicalContainerStateRuntime,
		progress:    physicalResourceProgressCreated,
		containerID: containerID,
	}
	owner, stored := containerData.StoreIfStateKeyUnclaimed(container.NamespacedName(), physicalContainerDataContainerIDKey(containerID), startedData)
	if stored {
		*data = *startedData.Clone()
	}
	return owner, stored
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
