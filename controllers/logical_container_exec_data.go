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
)

type logicalContainerExecState int

const (
	logicalContainerExecStateNamespace logicalContainerExecState = iota + 1
	logicalContainerExecStateNamespaceRetry
	logicalContainerExecStateTarget
	logicalContainerExecStateTargetNotFound
	logicalContainerExecStateTargetRetry
	logicalContainerExecStatePhysical
	logicalContainerExecStatePhysicalNotFound
	logicalContainerExecStatePhysicalRetry
	logicalContainerExecStateRuntime
	logicalContainerExecStateRuntimeRetry
	logicalContainerExecStateRunning
	logicalContainerExecStateExited
	logicalContainerExecStateRuntimeFailed
	logicalContainerExecStateMissing
	logicalContainerExecStateFailed
	logicalContainerExecStateDelete
	logicalContainerExecStateDeleteRetry
	logicalContainerExecStateInvalid
)

func (state logicalContainerExecState) String() string {
	switch state {
	case 0:
		return "Unset"
	case logicalContainerExecStateNamespace:
		return "Namespace"
	case logicalContainerExecStateNamespaceRetry:
		return "NamespaceRetry"
	case logicalContainerExecStateTarget:
		return "Target"
	case logicalContainerExecStateTargetNotFound:
		return "TargetNotFound"
	case logicalContainerExecStateTargetRetry:
		return "TargetRetry"
	case logicalContainerExecStatePhysical:
		return "Physical"
	case logicalContainerExecStatePhysicalNotFound:
		return "PhysicalNotFound"
	case logicalContainerExecStatePhysicalRetry:
		return "PhysicalRetry"
	case logicalContainerExecStateRuntime:
		return "Runtime"
	case logicalContainerExecStateRuntimeRetry:
		return "RuntimeRetry"
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
	case logicalContainerExecStateDeleteRetry:
		return "DeleteRetry"
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
	pendingPhysicalRef string
	failureTime        metav1.MicroTime
	retryAfter         time.Time
}

func (data *logicalContainerExecData) Clone() *logicalContainerExecData {
	cloned := *data
	return &cloned
}

func (data *logicalContainerExecData) UpdateFrom(other *logicalContainerExecData) bool {
	if other == nil || *data == *other {
		return false
	}
	*data = *other
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
	projectionChange, delay, valid := logicalContainerExecProjections.applyWithReason(
		data.state, data.reason, data.message,
		&resource.Status.Phase, &resource.Status.Conditions, resource.Generation,
	)
	return change | projectionChange, delay, valid
}

var logicalContainerExecProjections = physicalResourceProjectionTable[logicalContainerExecState, apiv2.ContainerExecPhase]{
	invalidPhase: apiv2.ContainerExecPhaseFailed,
	projections: map[logicalContainerExecState]physicalResourceProjection[apiv2.ContainerExecPhase]{
		logicalContainerExecStateNamespace: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotReady,
		},
		logicalContainerExecStateNamespaceRetry: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceLookupFailed, requeue: true, requeueDelay: LongDelay,
		},
		logicalContainerExecStateTargetNotFound: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerNotFound",
		},
		logicalContainerExecStateTarget: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerNotReady",
		},
		logicalContainerExecStateTargetRetry: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionPreparationFailed",
			requeue: true, requeueDelay: LongDelay,
		},
		logicalContainerExecStatePhysicalNotFound: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerNotFound",
		},
		logicalContainerExecStatePhysical: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionNotReady",
		},
		logicalContainerExecStatePhysicalRetry: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionPreparationFailed",
			requeue: true, requeueDelay: LongDelay,
		},
		logicalContainerExecStateFailed: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionPreparationFailed",
		},
		logicalContainerExecStateRuntime: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionNotReady",
		},
		logicalContainerExecStateRuntimeRetry: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionLookupFailed",
			requeue: true, requeueDelay: LongDelay,
		},
		logicalContainerExecStateRunning: {
			phase: apiv2.ContainerExecPhaseRunning, conditionStatus: metav1.ConditionTrue, conditionReason: "ExecutionRunning",
		},
		logicalContainerExecStateExited: {
			phase: apiv2.ContainerExecPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: "ExecutionExited",
		},
		logicalContainerExecStateRuntimeFailed: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionFailed",
		},
		logicalContainerExecStateMissing: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionChanged",
		},
		logicalContainerExecStateDelete: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "DeletingPhysicalExecution",
			requeue: true,
		},
		logicalContainerExecStateDeleteRetry: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionDeletionFailed",
			requeue: true, requeueDelay: LongDelay,
		},
		logicalContainerExecStateInvalid: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}
