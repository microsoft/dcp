/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"
	"maps"
	"reflect"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
)

type logicalContainerState int

func logicalContainerBackingState(physical *apiv2.PhysicalContainer) logicalContainerState {
	ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	switch physical.Status.Phase {
	case apiv2.PhysicalContainerPhasePending:
		if ready != nil && ready.Reason == string(apiv2.PhysicalContainerReasonStopRequested) {
			return logicalContainerStateStopping
		}
	case apiv2.PhysicalContainerPhaseRunning:
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
	logicalContainerStateExited
	logicalContainerStateFailed
	logicalContainerStatePaused
	logicalContainerStateRunning
	logicalContainerStateStopped
	logicalContainerStateStopping
	logicalContainerStateUnknown
	logicalContainerStateCreating
	logicalContainerStateWaitingForExisting
)

func (state logicalContainerState) String() string {
	switch state {
	case 0:
		return "Unset"
	case logicalContainerStatePending:
		return "Pending"
	case logicalContainerStatePreparationFailed:
		return "PreparationFailed"
	case logicalContainerStateExited:
		return "Exited"
	case logicalContainerStateFailed:
		return "Failed"
	case logicalContainerStatePaused:
		return "Paused"
	case logicalContainerStateRunning:
		return "Running"
	case logicalContainerStateStopped:
		return "Stopped"
	case logicalContainerStateStopping:
		return "Stopping"
	case logicalContainerStateUnknown:
		return "Unknown"
	case logicalContainerStateCreating:
		return "Creating"
	case logicalContainerStateWaitingForExisting:
		return "WaitingForExisting"
	default:
		return fmt.Sprintf("logicalContainerState(%d)", int(state))
	}
}

type logicalContainerData struct {
	state   logicalContainerState
	reason  apiv2.ConditionReason
	message string
	// Records successful startup independently of temporary observation failures.
	started bool
	// Effective launch values and handed-off port selections.
	effectiveCommand []string
	effectiveEnv     []commonapi.EnvVar
	ports            []logicalPortSelection
	// Supplied or calculated lifecycle key for the desired container configuration.
	lifecycleKey         string
	physicalContainerUID types.UID
	physicalImageUID     types.UID
	unobservedChildren   unobservedLogicalChildren
	lease                *statestore.ResourceLease
	leaseFailureMessage  string
}

func (data *logicalContainerData) Clone() *logicalContainerData {
	cloned := *data
	cloned.unobservedChildren = maps.Clone(data.unobservedChildren)
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

func (data *logicalContainerData) terminal() bool {
	switch data.state {
	case logicalContainerStatePreparationFailed, logicalContainerStateExited, logicalContainerStateFailed,
		logicalContainerStateStopped:
		return true
	default:
		return false
	}
}

func (data *logicalContainerData) failMissing(message string) {
	data.state, data.reason, data.message = logicalContainerStateFailed,
		apiv2.LogicalContainerReasonFailedToStart, message
	if data.started {
		data.reason = apiv2.LogicalContainerReasonMissing
	}
}

func (data *logicalContainerData) updateLifecycleFromPhysicalContainer(physical *apiv2.PhysicalContainer) {
	data.state, data.reason, data.message = logicalContainerBackingState(physical), "", logicalBackingStatusMessage("PhysicalContainer", physical.Status.Conditions)
	switch data.state {
	case logicalContainerStateCreating:
		if data.started {
			data.state = logicalContainerStateUnknown
		}
	case logicalContainerStateRunning, logicalContainerStatePaused:
		data.started = true
	case logicalContainerStateStopping:
		if !data.started {
			data.state = logicalContainerStateCreating
		}
	case logicalContainerStateExited:
		data.reason = apiv2.LogicalContainerReasonCompleted
		if physical.Spec.Stop {
			data.state, data.reason = logicalContainerStateStopped, apiv2.LogicalContainerReasonStopped
		}
		if physical.Status.ExitCode != nil {
			data.message = fmt.Sprintf("The backing container exited with code %d.", *physical.Status.ExitCode)
		}
	case logicalContainerStateFailed:
		data.failMissing(data.message)
	case logicalContainerStateUnknown:
		if !data.started {
			data.state = logicalContainerStateCreating
		}
	}
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
	message := data.message
	if !data.terminal() && data.leaseFailureMessage != "" {
		message = data.leaseFailureMessage
	}
	projectionChange, delay, valid := logicalContainerProjections.applyWithReason(
		data.state, data.reason, message,
		&container.Status.Phase, &container.Status.Conditions, container.Generation,
	)
	return change | projectionChange, delay, valid
}

var logicalContainerProjections = physicalResourceProjectionTable[logicalContainerState, apiv2.LogicalContainerPhase]{
	invalidPhase: apiv2.LogicalContainerPhaseUnknown,
	projections: map[logicalContainerState]physicalResourceProjection[apiv2.LogicalContainerPhase]{
		logicalContainerStatePending: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonStarting,
		},
		logicalContainerStateWaitingForExisting: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonStarting, requeue: true, requeueDelay: MonitoringDelay,
		},
		logicalContainerStatePreparationFailed: {
			phase: apiv2.LogicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonFailedToStart,
		},
		logicalContainerStateCreating: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonStarting,
		},
		logicalContainerStateRunning: {
			phase: apiv2.LogicalContainerPhaseRunning, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.LogicalContainerReasonRunning, message: "The logical container is running.",
		},
		logicalContainerStatePaused: {
			phase: apiv2.LogicalContainerPhaseRunning, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonPaused, message: "The logical container is paused.",
		},
		logicalContainerStateExited: {
			phase: apiv2.LogicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonCompleted, message: "The logical container has exited.",
		},
		logicalContainerStateFailed: {
			phase: apiv2.LogicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonFailedToStart,
		},
		logicalContainerStateUnknown: {
			phase: apiv2.LogicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonLookupFailed, requeue: true, requeueDelay: LongDelay,
		},
		logicalContainerStateStopping: {
			phase: apiv2.LogicalContainerPhaseRunning, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonStopping, message: "Waiting for the physical controller to stop the runtime container.",
		},
		logicalContainerStateStopped: {
			phase: apiv2.LogicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerReasonStopped, message: "The logical container has stopped.",
		},
	},
}
