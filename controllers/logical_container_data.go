/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
)

type logicalContainerState int

const (
	logicalContainerStatePending logicalContainerState = iota + 1
	logicalContainerStatePreparationFailed
	logicalContainerStateDelete
	logicalContainerStateExited
	logicalContainerStateFailed
	logicalContainerStateInvalid
	logicalContainerStatePaused
	logicalContainerStateRunning
	logicalContainerStateStopFailed
	logicalContainerStateStopped
	logicalContainerStateStopping
	logicalContainerStateUnknown
	logicalContainerStateCreating
)

func (state logicalContainerState) String() string {
	switch state {
	case 0:
		return "Unset"
	case logicalContainerStatePending:
		return "Pending"
	case logicalContainerStatePreparationFailed:
		return "PreparationFailed"
	case logicalContainerStateDelete:
		return "Delete"
	case logicalContainerStateExited:
		return "Exited"
	case logicalContainerStateFailed:
		return "Failed"
	case logicalContainerStateInvalid:
		return "Invalid"
	case logicalContainerStatePaused:
		return "Paused"
	case logicalContainerStateRunning:
		return "Running"
	case logicalContainerStateStopFailed:
		return "StopFailed"
	case logicalContainerStateStopped:
		return "Stopped"
	case logicalContainerStateStopping:
		return "Stopping"
	case logicalContainerStateUnknown:
		return "Unknown"
	case logicalContainerStateCreating:
		return "Creating"
	default:
		return fmt.Sprintf("logicalContainerState(%d)", int(state))
	}
}

type logicalContainerData struct {
	state      logicalContainerState
	reason     apiv2.ConditionReason
	message    string
	retryAfter time.Time
	// Most recent coordination error, retained until the next attempt.
	reconcileErr error
	// Lifecycle key selected for creation or adopted from the inspected container.
	lifecycleKey         string
	physicalContainerUID types.UID
	physicalImageUID     types.UID
	lease                *statestore.ResourceLease
	// Lease settlement failures are independent of the original reconciliation failure.
	leaseFailureMessage string
}

func (data *logicalContainerData) Clone() *logicalContainerData {
	cloned := *data
	return &cloned
}

func (data *logicalContainerData) UpdateFrom(other *logicalContainerData) bool {
	if other == nil {
		return false
	}
	left, right := *data, *other
	left.lease, right.lease = nil, nil
	if data.lease == other.lease && reflect.DeepEqual(left, right) {
		return false
	}
	*data = *other.Clone()
	return true
}

func (data *logicalContainerData) applyTo(container *apiv2.LogicalContainer) (objectChange, AdditionalReconciliationDelay, bool) {
	change := noChange
	if data.physicalContainerUID == "" {
		change |= setValue(&container.Status.PhysicalContainerRef, "")
	}
	// After handoff, inline image references come from the backing physical creation spec.
	if container.Spec.Container == nil || container.Spec.Container.Image == nil || data.physicalContainerUID == "" {
		imageRef := ""
		if container.Spec.Container != nil && container.Spec.Container.ImageRef != "" {
			imageRef = commonapi.AsNamespacedName(container.Spec.Container.ImageRef, container.Namespace).Name
		}
		change |= setValue(&container.Status.ImageRef, imageRef)
	}
	change |= setValue(&container.Status.LifecycleKey, data.lifecycleKey)
	if data.physicalContainerUID == "" {
		change |= applyLogicalContainerBackingStatus(container, &apiv2.PhysicalContainer{}, nil, false)
	}
	if data.state == logicalContainerStateInvalid {
		for _, source := range []*apiv2.LogSource{container.Status.RuntimeLogs, container.Status.StartupLogs} {
			if source != nil {
				change |= setValue(&source.Complete, true)
			}
		}
	}
	if _, valid := logicalContainerProjections.project(data.state); valid &&
		(data.reconcileErr != nil || data.leaseFailureMessage != "") {
		phase := apiv2.LogicalContainerPhaseUnknown
		if data.state == logicalContainerStateStopping || data.state == logicalContainerStateStopped ||
			data.state == logicalContainerStateStopFailed {
			phase = apiv2.LogicalContainerPhaseStopping
		}
		reason, message := data.reason, data.message
		if reason == "" {
			switch data.state {
			case logicalContainerStatePending:
				reason = "PhysicalResourcePreparationFailed"
			case logicalContainerStateDelete:
				reason = "PhysicalResourceDeletionFailed"
			default:
				reason = "PhysicalContainerLookupFailed"
			}
		}
		if data.leaseFailureMessage != "" {
			reason, message = "PersistenceFailed", data.leaseFailureMessage
		}
		change |= setValue(&container.Status.Phase, phase)
		change |= setCondition(&container.Status.Conditions, apiv2.ConditionReady, container.Generation,
			metav1.ConditionFalse, reason, message)
		return change | additionalReconciliationNeeded, LongDelay, true
	}
	projectionChange, delay, valid := logicalContainerProjections.applyWithReason(
		data.state, data.reason, data.message,
		&container.Status.Phase, &container.Status.Conditions, container.Generation,
	)
	if data.state == logicalContainerStatePending && !data.retryAfter.IsZero() {
		projectionChange |= additionalReconciliationNeeded
		delay = MonitoringDelay
	}
	return change | projectionChange, delay, valid
}

var logicalContainerProjections = physicalResourceProjectionTable[logicalContainerState, apiv2.LogicalContainerPhase]{
	invalidPhase: apiv2.LogicalContainerPhaseUnknown,
	projections: map[logicalContainerState]physicalResourceProjection[apiv2.LogicalContainerPhase]{
		logicalContainerStatePending: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: logicalContainerPreparationPendingReason,
		},
		logicalContainerStatePreparationFailed: {
			phase: apiv2.LogicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalResourcePreparationFailed",
		},
		logicalContainerStateCreating: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalContainerNotReady",
		},
		logicalContainerStateRunning: {
			phase: apiv2.LogicalContainerPhaseRunning, conditionStatus: metav1.ConditionTrue,
			conditionReason: "ContainerRunning", message: "The logical container is running.",
		},
		logicalContainerStatePaused: {
			phase: apiv2.LogicalContainerPhasePaused, conditionStatus: metav1.ConditionFalse,
			conditionReason: "ContainerPaused", message: "The logical container is paused.",
		},
		logicalContainerStateExited: {
			phase: apiv2.LogicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: "ContainerExited", message: "The logical container has exited.",
		},
		logicalContainerStateFailed: {
			phase: apiv2.LogicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalContainerFailed",
		},
		logicalContainerStateUnknown: {
			phase: apiv2.LogicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalContainerStateUnknown",
		},
		logicalContainerStateStopping: {
			phase: apiv2.LogicalContainerPhaseStopping, conditionStatus: metav1.ConditionFalse,
			conditionReason: "ContainerStopping", message: "Waiting for the physical controller to stop the runtime container.",
		},
		logicalContainerStateStopped: {
			phase: apiv2.LogicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: "ContainerStopped", message: "The logical container has stopped.",
		},
		logicalContainerStateStopFailed: {
			phase: apiv2.LogicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalContainerFailed",
		},
		logicalContainerStateDelete: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "DeletingPhysicalResources", requeue: true,
		},
		logicalContainerStateInvalid: {
			phase: apiv2.LogicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}
