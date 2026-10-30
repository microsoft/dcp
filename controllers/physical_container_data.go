/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type physicalContainerDataStateKey string

type physicalContainerState int

const (
	physicalContainerStateCleanupFailed physicalContainerState = iota + 1
	physicalContainerStateCleanupRetry
	physicalContainerStateCopyFiles
	physicalContainerStateCreate
	physicalContainerStateCreateFailed
	physicalContainerStateCreateRetry
	physicalContainerStateCreated
	physicalContainerStateCreationSkipped
	physicalContainerStateDead
	physicalContainerStateExited
	physicalContainerStateFileCopyFailed
	physicalContainerStateFilesCopied
	physicalContainerStateImage
	physicalContainerStateImageFailed
	physicalContainerStateImageLookupFailed
	physicalContainerStateImageNotFound
	physicalContainerStateInspectFailed
	physicalContainerStateInvalid
	physicalContainerStateMissing
	physicalContainerStateNetworkFailed
	physicalContainerStateNetworkLookupFailed
	physicalContainerStateNetworkNotFound
	physicalContainerStateNetworks
	physicalContainerStateNotStarted
	physicalContainerStatePaused
	physicalContainerStateRemoveRetry
	physicalContainerStateRemoving
	physicalContainerStateReplaceRetry
	physicalContainerStateResolve
	physicalContainerStateResolveRetry
	physicalContainerStateRestarting
	physicalContainerStateRunning
	physicalContainerStateStart
	physicalContainerStateStartFailed
	physicalContainerStateStarted
	physicalContainerStateStopRetry
	physicalContainerStateStopped
	physicalContainerStateUnknown
	physicalContainerStateVolumeFailed
	physicalContainerStateVolumeLookupFailed
	physicalContainerStateVolumeNotFound
	physicalContainerStateVolumes
)

func (state physicalContainerState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalContainerStateCleanupFailed:
		return "CleanupFailed"
	case physicalContainerStateCleanupRetry:
		return "CleanupRetry"
	case physicalContainerStateCopyFiles:
		return "CopyFiles"
	case physicalContainerStateCreate:
		return "Create"
	case physicalContainerStateCreateFailed:
		return "CreateFailed"
	case physicalContainerStateCreateRetry:
		return "CreateRetry"
	case physicalContainerStateCreated:
		return "Created"
	case physicalContainerStateCreationSkipped:
		return "CreationSkipped"
	case physicalContainerStateDead:
		return "Dead"
	case physicalContainerStateExited:
		return "Exited"
	case physicalContainerStateFileCopyFailed:
		return "FileCopyFailed"
	case physicalContainerStateFilesCopied:
		return "FilesCopied"
	case physicalContainerStateImage:
		return "Image"
	case physicalContainerStateImageFailed:
		return "ImageFailed"
	case physicalContainerStateImageLookupFailed:
		return "ImageLookupFailed"
	case physicalContainerStateImageNotFound:
		return "ImageNotFound"
	case physicalContainerStateInspectFailed:
		return "InspectFailed"
	case physicalContainerStateInvalid:
		return "Invalid"
	case physicalContainerStateMissing:
		return "Missing"
	case physicalContainerStateNetworkFailed:
		return "NetworkFailed"
	case physicalContainerStateNetworkLookupFailed:
		return "NetworkLookupFailed"
	case physicalContainerStateNetworkNotFound:
		return "NetworkNotFound"
	case physicalContainerStateNetworks:
		return "Networks"
	case physicalContainerStateNotStarted:
		return "NotStarted"
	case physicalContainerStatePaused:
		return "Paused"
	case physicalContainerStateRemoveRetry:
		return "RemoveRetry"
	case physicalContainerStateRemoving:
		return "Removing"
	case physicalContainerStateReplaceRetry:
		return "ReplaceRetry"
	case physicalContainerStateResolve:
		return "Resolve"
	case physicalContainerStateResolveRetry:
		return "ResolveRetry"
	case physicalContainerStateRestarting:
		return "Restarting"
	case physicalContainerStateRunning:
		return "Running"
	case physicalContainerStateStart:
		return "Start"
	case physicalContainerStateStartFailed:
		return "StartFailed"
	case physicalContainerStateStarted:
		return "Started"
	case physicalContainerStateStopRetry:
		return "StopRetry"
	case physicalContainerStateStopped:
		return "Stopped"
	case physicalContainerStateUnknown:
		return "Unknown"
	case physicalContainerStateVolumeFailed:
		return "VolumeFailed"
	case physicalContainerStateVolumeLookupFailed:
		return "VolumeLookupFailed"
	case physicalContainerStateVolumeNotFound:
		return "VolumeNotFound"
	case physicalContainerStateVolumes:
		return "Volumes"
	default:
		return fmt.Sprintf("physicalContainerState(%d)", int(state))
	}
}

type physicalContainerData struct {
	// UID of the PhysicalContainer that owns this data.
	resourceUID types.UID

	// Current reconciliation concern.
	state physicalContainerState

	// Whether the current asynchronous operation is still outstanding.
	workInProgress bool

	// ID of the associated runtime container, including a partially created container.
	containerID string

	// Image name resolved from the referenced PhysicalContainerImage.
	image string

	// Diagnostic message from the current failed runtime operation.
	failureMessage string

	// Diagnostic from the latest port-mapping projection failure.
	portMappingFailureMessage string

	// Diagnostic message from the latest partial-container cleanup failure.
	cleanupMessage string

	// Earliest time at which a failed operation should be retried.
	retryAfter time.Time
}

func (data *physicalContainerData) Clone() *physicalContainerData {
	return &physicalContainerData{
		resourceUID:               data.resourceUID,
		state:                     data.state,
		workInProgress:            data.workInProgress,
		containerID:               data.containerID,
		image:                     data.image,
		failureMessage:            data.failureMessage,
		portMappingFailureMessage: data.portMappingFailureMessage,
		cleanupMessage:            data.cleanupMessage,
		retryAfter:                data.retryAfter,
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
	if data.workInProgress != other.workInProgress {
		data.workInProgress = other.workInProgress
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
	if data.portMappingFailureMessage != other.portMappingFailureMessage {
		data.portMappingFailureMessage = other.portMappingFailureMessage
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
	return data != nil && data.workInProgress
}

func (data *physicalContainerData) applyTo(
	container *apiv2.PhysicalContainer,
) (objectChange, AdditionalReconciliationDelay, bool) {
	change := setValue(&container.Status.ContainerID, data.containerID)
	if data.containerID != "" {
		if container.Spec.Container != nil {
			change |= setValue(&container.Status.ContainerName, container.Spec.Container.ContainerName)
		}
	}
	if data.image != "" {
		change |= setValue(&container.Status.Image, data.image)
	}

	message := data.failureMessage
	if data.state == physicalContainerStateCleanupFailed || data.state == physicalContainerStateCleanupRetry || data.cleanupMessage != "" {
		message = data.cleanupMessage
	}
	if data.portMappingFailureMessage != "" {
		projection, valid := physicalContainerProjections.project(data.state)
		if !valid {
			stateChange, delay, _ := physicalContainerProjections.apply(
				data.state,

				message,
				&container.Status.Phase,
				&container.Status.Conditions,
				container.Generation,
			)
			return change | stateChange, delay, false
		}

		stateChange := setValue(&container.Status.Phase, projection.phase)
		stateChange |= setCondition(
			&container.Status.Conditions,
			apiv2.ConditionReady,
			container.Generation,
			metav1.ConditionFalse,
			apiv2.PhysicalContainerReasonPortMappingResolutionFailed,
			data.portMappingFailureMessage,
		)
		stateChange |= additionalReconciliationNeeded
		return change | stateChange, LongDelay, true
	}

	stateChange, delay, valid := physicalContainerProjections.apply(
		data.state,

		message,
		&container.Status.Phase,
		&container.Status.Conditions,
		container.Generation,
	)
	return change | stateChange, delay, valid
}

var physicalContainerProjections = physicalResourceProjectionTable[physicalContainerState, apiv2.PhysicalContainerPhase]{
	invalidPhase: apiv2.PhysicalContainerPhaseUnknown,
	projections: map[physicalContainerState]physicalResourceProjection[apiv2.PhysicalContainerPhase]{
		physicalContainerStateImageNotFound: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonImageNotFound,
		},
		physicalContainerStateImage: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonImageNotReady,
		},
		physicalContainerStateImageLookupFailed: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonImageLookupFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerStateImageFailed: {
			phase: apiv2.PhysicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonImageFailed,
		},
		physicalContainerStateVolumeNotFound: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonVolumeNotFound,
		},
		physicalContainerStateVolumes: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonVolumeNotReady,
		},
		physicalContainerStateVolumeLookupFailed: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonVolumeLookupFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerStateVolumeFailed: {
			phase: apiv2.PhysicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonVolumeFailed,
		},
		physicalContainerStateNetworkNotFound: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonNetworkNotFound,
		},
		physicalContainerStateNetworks: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonNetworkNotReady,
		},
		physicalContainerStateNetworkLookupFailed: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonNetworkLookupFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerStateNetworkFailed: {
			phase: apiv2.PhysicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonNetworkFailed,
		},
		physicalContainerStateCreate: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonCreating,
			message: "Physical container creation is in progress.",
		},
		physicalContainerStateCreated: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonCreated,
			message: "Physical container creation completed.",
		},
		physicalContainerStateCreationSkipped: {
			phase: apiv2.PhysicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonStopRequested,
			message: "Runtime container was not created because stop was requested.",
		},
		physicalContainerStateCreateRetry: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonCreateFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerStateCreateFailed: {
			phase: apiv2.PhysicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonCreateFailed,
		},
		physicalContainerStateReplaceRetry: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonExistingContainerReplacementFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerStateCopyFiles: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonCopyingFiles,
			message: "Physical container file copy is in progress.",
		},
		physicalContainerStateFilesCopied: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonFilesCreated,
			message: "Physical container file copy completed.",
		},
		physicalContainerStateFileCopyFailed: {
			phase: apiv2.PhysicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonFileCopyFailed,
		},
		physicalContainerStateStart: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonStarting,
			message: "Physical container start is in progress.",
		},
		physicalContainerStateStarted: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonStarted,
			message: "Physical container start completed.",
		},
		physicalContainerStateStartFailed: {
			phase: apiv2.PhysicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonStartFailed,
		},
		physicalContainerStateCleanupRetry: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonPartialContainerCleanupFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerStateCleanupFailed: {
			phase: apiv2.PhysicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonPartialContainerCleanupFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerStateRunning: {
			phase: apiv2.PhysicalContainerPhaseRunning, conditionStatus: metav1.ConditionTrue, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerRunning,
			message: "Runtime container is running.", requeue: true, requeueDelay: MonitoringDelay,
		},
		physicalContainerStatePaused: {
			phase: apiv2.PhysicalContainerPhasePaused, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerPaused,
			message: "Runtime container is paused.", requeue: true, requeueDelay: MonitoringDelay,
		},
		physicalContainerStateRestarting: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerRestarting,
			message: "Runtime container is restarting.", requeue: true,
		},
		physicalContainerStateNotStarted: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerCreated,
			message: "Runtime container has been created but is not running.", requeue: true, requeueDelay: MonitoringDelay,
		},
		physicalContainerStateRemoving: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerRemoving,
			message: "Runtime container is being removed.", requeue: true, requeueDelay: MonitoringDelay,
		},
		physicalContainerStateExited: {
			phase: apiv2.PhysicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerExited,
			message: "Runtime container has exited.",
		},
		physicalContainerStateDead: {
			phase: apiv2.PhysicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerDead,
			message: "Runtime container is dead.",
		},
		physicalContainerStateUnknown: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerStatusUnknown,
			requeue: true,
		},
		physicalContainerStateMissing: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerMissing,
			message: "Runtime container was not found.",
		},
		physicalContainerStateInspectFailed: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerInspectFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerStateStopRetry: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerStopFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerStateStopped: {
			phase: apiv2.PhysicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonStopRequested,
			message: "Runtime container is already stopped or was never started.",
		},
		physicalContainerStateRemoveRetry: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerRemoveFailed,
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerStateResolveRetry: {
			phase: apiv2.PhysicalContainerPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalContainerReasonRuntimeContainerAlreadyTracked,
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerStateInvalid: {
			phase: apiv2.PhysicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}

// Claims the runtime container identity for the resource. On success the caller's reconciliation
// state is replaced with the newly recorded state.
func storeStartedPhysicalContainerData(
	containerData *ObjectStateMap[physicalContainerDataStateKey, physicalContainerData, *physicalContainerData, *apiv2.PhysicalContainer],
	container *apiv2.PhysicalContainer,
	oldStateKey physicalContainerDataStateKey,
	containerID string,
	data *physicalContainerData,
) (types.NamespacedName, bool) {
	startedData := &physicalContainerData{
		resourceUID: container.UID,
		state:       physicalContainerStateNotStarted,

		containerID: containerID,
	}
	owner, updated := containerData.UpdateChangingStateKeyIfUnclaimed(
		container.NamespacedName(),
		oldStateKey,
		physicalContainerDataContainerIDKey(containerID),
		startedData,
	)
	if updated {
		*data = *startedData.Clone()
	}
	return owner, updated
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
