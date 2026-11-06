/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"
	"reflect"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
)

type logicalContainerState int

func logicalContainerBackingState(physical *apiv2.PhysicalContainer) logicalContainerState {
	if !physical.DeletionTimestamp.IsZero() {
		return logicalContainerStateUnknown
	}
	switch physical.Status.Phase {
	case apiv2.PhysicalContainerPhaseRunning:
		ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
		if ready != nil && ready.Status == metav1.ConditionTrue {
			return logicalContainerStateRunning
		}
	case apiv2.PhysicalContainerPhasePaused:
		return logicalContainerStatePaused
	case apiv2.PhysicalContainerPhaseExited:
		return logicalContainerStateExited
	case apiv2.PhysicalContainerPhaseFailed:
		return logicalContainerStateFailed
	case apiv2.PhysicalContainerPhaseUnknown:
		return logicalContainerStateUnknown
	}
	return logicalContainerStateCreating
}

const (
	logicalContainerStatePending logicalContainerState = iota + 1
	logicalContainerStatePreparationFailed
	logicalContainerStateDelete
	logicalContainerStateExited
	logicalContainerStateFailed
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
	// Effective launch values and handed-off port selections.
	effectiveCommand []string
	effectiveEnv     []commonapi.EnvVar
	ports            []logicalPortSelection
	// Supplied or calculated lifecycle key for the desired container configuration.
	lifecycleKey         string
	physicalContainerUID types.UID
	physicalImageUID     types.UID
	lease                *statestore.ResourceLease
	// Lease settlement failures are independent of the original reconciliation failure.
	leaseFailureMessage string
}

func (data *logicalContainerData) Clone() *logicalContainerData {
	cloned := *data
	cloned.effectiveCommand = append([]string(nil), data.effectiveCommand...)
	cloned.effectiveEnv = append([]commonapi.EnvVar(nil), data.effectiveEnv...)
	cloned.ports = append([]logicalPortSelection(nil), data.ports...)
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
	var ports []logicalPortData
	if data.physicalContainerUID != "" {
		ports = data.portData(container)
	}
	change := projectLogicalPorts(ports, &container.Status.Ports)
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
	lifecycleKey := container.Spec.LifecycleKey
	if data.physicalContainerUID != "" {
		lifecycleKey = data.lifecycleKey
	}
	change |= setValue(&container.Status.LifecycleKey, lifecycleKey)
	if data.physicalContainerUID == "" {
		change |= applyLogicalContainerBackingStatus(container, &apiv2.PhysicalContainer{})
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
				reason = apiv2.LogicalContainerReasonPhysicalResourcePreparationFailed
			case logicalContainerStateDelete:
				reason = apiv2.LogicalContainerReasonPhysicalResourceDeletionFailed
			default:
				reason = apiv2.LogicalContainerReasonPhysicalContainerLookupFailed
			}
		}
		if data.leaseFailureMessage != "" {
			reason, message = apiv2.LogicalResourceReasonPersistenceFailed, data.leaseFailureMessage
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
			conditionReason: apiv2.LogicalContainerReasonPhysicalResourcePreparationPending,
		},
		logicalContainerStatePreparationFailed: {
			phase: apiv2.LogicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonPhysicalResourcePreparationFailed,
		},
		logicalContainerStateCreating: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonPhysicalContainerNotReady,
		},
		logicalContainerStateRunning: {
			phase: apiv2.LogicalContainerPhaseRunning, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.LogicalContainerReasonRunning, message: "The logical container is running.",
		},
		logicalContainerStatePaused: {
			phase: apiv2.LogicalContainerPhasePaused, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonPaused, message: "The logical container is paused.",
		},
		logicalContainerStateExited: {
			phase: apiv2.LogicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonExited, message: "The logical container has exited.",
		},
		logicalContainerStateFailed: {
			phase: apiv2.LogicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonPhysicalContainerFailed,
		},
		logicalContainerStateUnknown: {
			phase: apiv2.LogicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonPhysicalContainerStateUnknown,
		},
		logicalContainerStateStopping: {
			phase: apiv2.LogicalContainerPhaseStopping, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonStopping, message: "Waiting for the physical controller to stop the runtime container.",
		},
		logicalContainerStateStopped: {
			phase: apiv2.LogicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonStopped, message: "The logical container has stopped.",
		},
		logicalContainerStateStopFailed: {
			phase: apiv2.LogicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonPhysicalContainerFailed,
		},
		logicalContainerStateDelete: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonDeletingPhysicalResources, requeue: true,
		},
	},
}
