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
	if !physical.DeletionTimestamp.IsZero() && hasFinalizer(physical, physicalContainerExecFinalizer) {
		return change, logicalContainerExecStateCreating
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
	logicalContainerExecStateDelete
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
	case logicalContainerExecStateDelete:
		return "Delete"
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
	reconcileErr       error
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
		source := &apiv2.LogSource{Name: physicalRef, UID: data.physicalUID, Complete: true}
		if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
			resource.Status.RuntimeLogs = source
			change |= statusChanged
		}
	}
	if _, valid := logicalContainerExecProjections.project(data.state); valid && data.reconcileErr != nil {
		reason := data.reason
		if reason == "" {
			switch data.state {
			case logicalContainerExecStatePending:
				reason = apiv2.LogicalContainerExecReasonPhysicalExecutionPreparationFailed
			case logicalContainerExecStateDelete:
				reason = apiv2.LogicalContainerExecReasonPhysicalExecutionDeletionFailed
			default:
				reason = apiv2.LogicalContainerExecReasonPhysicalExecutionLookupFailed
			}
		}
		change |= setValue(&resource.Status.Phase, apiv2.ContainerExecPhasePending)
		change |= setCondition(&resource.Status.Conditions, apiv2.ConditionReady, resource.Generation,
			metav1.ConditionFalse, reason, data.message)
		return change | additionalReconciliationNeeded, LongDelay, true
	}
	projectionChange, delay, valid := logicalContainerExecProjections.applyWithReason(
		data.state, data.reason, data.message,
		&resource.Status.Phase, &resource.Status.Conditions, resource.Generation,
	)
	return change | projectionChange, delay, valid
}

var logicalContainerExecProjections = physicalResourceProjectionTable[logicalContainerExecState, apiv2.ContainerExecPhase]{
	invalidPhase: apiv2.ContainerExecPhaseFailed,
	projections: map[logicalContainerExecState]physicalResourceProjection[apiv2.ContainerExecPhase]{
		logicalContainerExecStatePending: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ContainerExecReasonContainerNotReady,
		},
		logicalContainerExecStateFailed: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerExecReasonPhysicalExecutionPreparationFailed,
		},
		logicalContainerExecStateCreating: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerExecReasonPhysicalExecutionNotReady,
		},
		logicalContainerExecStateRunning: {
			phase: apiv2.ContainerExecPhaseRunning, conditionStatus: metav1.ConditionTrue, conditionReason: apiv2.ContainerExecReasonRunning,
		},
		logicalContainerExecStateExited: {
			phase: apiv2.ContainerExecPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ContainerExecReasonExited,
		},
		logicalContainerExecStateRuntimeFailed: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerExecReasonPhysicalExecutionFailed,
		},
		logicalContainerExecStateMissing: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerExecReasonPhysicalExecutionChanged,
		},
		logicalContainerExecStateDelete: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerExecReasonDeletingPhysicalExecution,
			requeue: true,
		},
		logicalContainerExecStateInvalid: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}
