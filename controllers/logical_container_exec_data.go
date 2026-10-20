/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type logicalContainerExecState int

const (
	logicalContainerExecStateNamespace logicalContainerExecState = iota + 1
	logicalContainerExecStateTarget
	logicalContainerExecStatePhysical
	logicalContainerExecStateRuntime
	logicalContainerExecStateDelete
	logicalContainerExecStateInvalid
)

type logicalContainerExecData struct {
	state              logicalContainerExecState
	progress           physicalResourceProgress
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
	if data.state == logicalContainerExecStateRuntime && data.progress == physicalResourceProgressMissing {
		source := &apiv2.LogSource{Name: physicalRef, UID: data.physicalUID, Complete: true}
		if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
			resource.Status.RuntimeLogs = source
			change |= statusChanged
		}
	}
	projectionChange, delay, valid := logicalContainerExecProjections.applyWithReason(
		data.state, data.progress, data.reason, data.message,
		&resource.Status.Phase, &resource.Status.Conditions, resource.Generation,
	)
	return change | projectionChange, delay, valid
}

var logicalContainerExecProjections = physicalResourceProjectionTable[logicalContainerExecState, apiv2.ContainerExecPhase]{
	invalidPhase: apiv2.ContainerExecPhaseFailed,
	projections: map[physicalResourceProjectionKey[logicalContainerExecState]]physicalResourceProjection[apiv2.ContainerExecPhase]{
		{state: logicalContainerExecStateNamespace, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotReady,
		},
		{state: logicalContainerExecStateNamespace, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceLookupFailed, requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerExecStateTarget, progress: physicalResourceProgressNotFound}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerNotFound",
		},
		{state: logicalContainerExecStateTarget, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerNotReady",
		},
		{state: logicalContainerExecStateTarget, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionPreparationFailed",
			requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerExecStateTarget, progress: physicalResourceProgressFailed}: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerUnavailable",
		},
		{state: logicalContainerExecStatePhysical, progress: physicalResourceProgressNotFound}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerNotFound",
		},
		{state: logicalContainerExecStatePhysical, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionNotReady",
		},
		{state: logicalContainerExecStatePhysical, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionPreparationFailed",
			requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerExecStatePhysical, progress: physicalResourceProgressFailed}: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionPreparationFailed",
		},
		{state: logicalContainerExecStateRuntime, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionNotReady",
		},
		{state: logicalContainerExecStateRuntime, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionLookupFailed",
			requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerExecStateRuntime, progress: physicalResourceProgressRunning}: {
			phase: apiv2.ContainerExecPhaseRunning, conditionStatus: metav1.ConditionTrue, conditionReason: "ExecutionRunning",
		},
		{state: logicalContainerExecStateRuntime, progress: physicalResourceProgressExited}: {
			phase: apiv2.ContainerExecPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: "ExecutionExited",
		},
		{state: logicalContainerExecStateRuntime, progress: physicalResourceProgressFailed}: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionFailed",
		},
		{state: logicalContainerExecStateRuntime, progress: physicalResourceProgressMissing}: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionChanged",
		},
		{state: logicalContainerExecStateDelete, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "DeletingPhysicalExecution",
			requeue: true,
		},
		{state: logicalContainerExecStateDelete, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PhysicalExecutionDeletionFailed",
			requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerExecStateInvalid, progress: physicalResourceProgressFailed}: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}
