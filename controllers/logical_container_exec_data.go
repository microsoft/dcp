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
)

type logicalContainerExecState int

func applyLogicalContainerExecBackingStatus(
	resource *apiv2.LogicalContainerExec, physical *apiv2.PhysicalContainerExec,
) (objectChange, logicalContainerExecState) {
	change := setValue(&resource.Status.ContainerID, physical.Status.ContainerID)
	change |= setTimestamp(&resource.Status.StartedAt, physical.Status.StartedAt)
	change |= setTimestamp(&resource.Status.FinishedAt, physical.Status.FinishedAt)
	change |= setExitCode(&resource.Status.ExitCode, physical.Status.ExitCode)
	source := physical.GetLogSource()
	if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
		resource.Status.RuntimeLogs = source
		change |= statusChanged
	}
	condition := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	switch physical.Status.Phase {
	case apiv2.ContainerExecPhaseRunning:
		if condition != nil && condition.Status == metav1.ConditionTrue {
			return change, logicalContainerExecStateRunning
		}
	case apiv2.ContainerExecPhaseExited:
		return change, logicalContainerExecStateExited
	case apiv2.ContainerExecPhaseFailed:
		return change, logicalContainerExecStateRuntimeFailed
	case apiv2.ContainerExecPhasePending, "":
		if condition != nil && condition.Reason == string(apiv2.ContainerExecReasonStopping) {
			return change, logicalContainerExecStateStopping
		}
	default:
		return change, logicalContainerExecStateInvalid
	}
	return change, logicalContainerExecStateCreating
}

const (
	logicalContainerExecStatePending logicalContainerExecState = iota + 1
	logicalContainerExecStateCreating
	logicalContainerExecStateRunning
	logicalContainerExecStateExited
	logicalContainerExecStateRuntimeFailed
	logicalContainerExecStateMissing
	logicalContainerExecStateFailed
	logicalContainerExecStateStopping
	logicalContainerExecStateUnknown
	logicalContainerExecStateInvalid
)

func (state logicalContainerExecState) String() string {
	switch state {
	case 0:
		return "Unset"
	case logicalContainerExecStatePending:
		return "Pending"
	case logicalContainerExecStateCreating:
		return "Creating"
	case logicalContainerExecStateRunning:
		return "Running"
	case logicalContainerExecStateExited:
		return "Exited"
	case logicalContainerExecStateRuntimeFailed:
		return "RuntimeFailed"
	case logicalContainerExecStateMissing:
		return "Missing"
	case logicalContainerExecStateFailed:
		return "Failed"
	case logicalContainerExecStateStopping:
		return "Stopping"
	case logicalContainerExecStateUnknown:
		return "Unknown"
	case logicalContainerExecStateInvalid:
		return "Invalid"
	default:
		return fmt.Sprintf("logicalContainerExecState(%d)", int(state))
	}
}

type logicalContainerExecData struct {
	state              logicalContainerExecState
	reason             apiv2.ConditionReason
	message            string
	physicalUID        types.UID
	unobservedChildren unobservedLogicalChildren
	pendingPhysicalRef string
	failureTime        metav1.MicroTime
	started            bool
}

func (data *logicalContainerExecData) Clone() *logicalContainerExecData {
	cloned := *data
	cloned.unobservedChildren = maps.Clone(data.unobservedChildren)
	return &cloned
}

func (data *logicalContainerExecData) UpdateFrom(other *logicalContainerExecData) bool {
	if other == nil || reflect.DeepEqual(data, other) {
		return false
	}
	*data = *other.Clone()
	return true
}

func (data *logicalContainerExecData) terminal() bool {
	switch data.state {
	case logicalContainerExecStateExited, logicalContainerExecStateRuntimeFailed, logicalContainerExecStateMissing,
		logicalContainerExecStateFailed, logicalContainerExecStateInvalid:
		return true
	default:
		return false
	}
}

func (data *logicalContainerExecData) failMissing(message string) {
	reason := apiv2.LogicalContainerExecReasonFailedToStart
	if data.started {
		reason = apiv2.LogicalContainerExecReasonMissing
	}
	data.fail(reason, message)
}

func (data *logicalContainerExecData) updateLifecycleFromPhysicalContainerExec(physical *apiv2.PhysicalContainerExec, state logicalContainerExecState) {
	data.state, data.reason, data.message = state, "", logicalBackingStatusMessage("PhysicalContainerExec", physical.Status.Conditions)
	switch state {
	case logicalContainerExecStateCreating:
		if data.started {
			data.state = logicalContainerExecStateUnknown
		}
	case logicalContainerExecStateRunning:
		data.started = true
	case logicalContainerExecStateStopping:
		if !data.started {
			data.state = logicalContainerExecStateCreating
		}
	case logicalContainerExecStateExited:
		if physical.Status.ExitCode != nil {
			data.message = fmt.Sprintf("The attached execution exited with code %d.", *physical.Status.ExitCode)
		}
	case logicalContainerExecStateRuntimeFailed:
		data.failMissing(data.message)
	}
}

func (data *logicalContainerExecData) applyMissingLogSource(resource *apiv2.LogicalContainerExec) objectChange {
	source := &apiv2.LogSource{Name: "logical-" + string(resource.UID), UID: data.physicalUID, Complete: true}
	if reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
		return noChange
	}
	resource.Status.RuntimeLogs = source
	return statusChanged
}

func (data *logicalContainerExecData) applyTo(resource *apiv2.LogicalContainerExec) (objectChange, AdditionalReconciliationDelay, bool) {
	physicalRef := ""
	if data.physicalUID != "" {
		physicalRef = "logical-" + string(resource.UID)
	}
	change := setValue(&resource.Status.PhysicalExecRef, physicalRef)
	if !data.failureTime.IsZero() {
		change |= setTimestamp(&resource.Status.FinishedAt, data.failureTime)
	}
	if data.state == logicalContainerExecStateMissing {
		change |= data.applyMissingLogSource(resource)
	}
	projectionChange, delay, valid := logicalContainerExecProjections.applyWithReason(
		data.state, data.reason, data.message,
		&resource.Status.Phase, &resource.Status.Conditions, resource.Generation,
	)
	return change | projectionChange, delay, valid
}

var logicalContainerExecProjections = physicalResourceProjectionTable[logicalContainerExecState, apiv2.LogicalContainerExecPhase]{
	invalidPhase: apiv2.LogicalContainerExecPhaseFailed,
	projections: map[logicalContainerExecState]physicalResourceProjection[apiv2.LogicalContainerExecPhase]{
		logicalContainerExecStatePending: {
			phase: apiv2.LogicalContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerExecReasonStarting,
		},
		logicalContainerExecStateFailed: {
			phase: apiv2.LogicalContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerExecReasonFailedToStart,
		},
		logicalContainerExecStateCreating: {
			phase: apiv2.LogicalContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerExecReasonStarting,
		},
		logicalContainerExecStateRunning: {
			phase: apiv2.LogicalContainerExecPhaseRunning, conditionStatus: metav1.ConditionTrue, conditionReason: apiv2.LogicalContainerExecReasonRunning,
		},
		logicalContainerExecStateExited: {
			phase: apiv2.LogicalContainerExecPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerExecReasonCompleted,
		},
		logicalContainerExecStateRuntimeFailed: {
			phase: apiv2.LogicalContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerExecReasonFailedToStart,
		},
		logicalContainerExecStateMissing: {
			phase: apiv2.LogicalContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerExecReasonMissing,
		},
		logicalContainerExecStateStopping: {
			phase: apiv2.LogicalContainerExecPhaseRunning, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerExecReasonStopping,
			requeue: true,
		},
		logicalContainerExecStateUnknown: {
			phase: apiv2.LogicalContainerExecPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerExecReasonLookupFailed, requeue: true, requeueDelay: LongDelay,
		},
		logicalContainerExecStateInvalid: {
			phase: apiv2.LogicalContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerExecReasonFailedToStart,
		},
	},
}
