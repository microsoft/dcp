/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/logs"
)

type physicalContainerExecState int

const (
	physicalContainerExecStateNamespace physicalContainerExecState = iota + 1
	physicalContainerExecStateTarget
	physicalContainerExecStateLaunch
	physicalContainerExecStateRuntime
	physicalContainerExecStateStop
	physicalContainerExecStateInvalid
)

type physicalContainerExecSession struct {
	cancel context.CancelFunc
}

type physicalContainerExecData struct {
	state       physicalContainerExecState
	progress    physicalResourceProgress
	reason      apiv2.ConditionReason
	message     string
	targetUID   types.UID
	containerID string
	startedAt   metav1.MicroTime
	finishedAt  metav1.MicroTime
	exitCode    *int32
	retryAfter  time.Time
	session     *physicalContainerExecSession
	stdout      *logs.CaptureFile
	stderr      *logs.CaptureFile
}

func (data *physicalContainerExecData) Clone() *physicalContainerExecData {
	cloned := *data
	cloned.exitCode = cloneInt32Pointer(data.exitCode)
	return &cloned
}

func (data *physicalContainerExecData) UpdateFrom(other *physicalContainerExecData) bool {
	if other == nil {
		return false
	}
	left, right := *data, *other
	left.session, right.session = nil, nil
	if data.session == other.session && reflect.DeepEqual(left, right) {
		return false
	}
	*data = *other.Clone()
	return true
}

func (data *physicalContainerExecData) operationInProgress() bool {
	return (data.state == physicalContainerExecStateLaunch || data.state == physicalContainerExecStateStop) &&
		data.progress == physicalResourceProgressInProgress
}

func (data *physicalContainerExecData) matchesTarget(target *apiv2.PhysicalContainer) bool {
	return data.targetUID == "" || (target.UID == data.targetUID &&
		(target.Status.ContainerID == "" || target.Status.ContainerID == data.containerID))
}

func (data *physicalContainerExecData) applyTo(resource *apiv2.PhysicalContainerExec) (objectChange, AdditionalReconciliationDelay, bool) {
	change := setValue(&resource.Status.ContainerID, data.containerID)
	change |= setTimestamp(&resource.Status.StartedAt, data.startedAt)
	change |= setTimestamp(&resource.Status.FinishedAt, data.finishedAt)
	change |= setPhysicalProcessExitCode(&resource.Status.ExitCode, data.exitCode)
	complete := data.progress == physicalResourceProgressFailed ||
		(data.state == physicalContainerExecStateRuntime && data.progress == physicalResourceProgressExited)
	change |= setValue(&resource.Status.LogsComplete, complete)
	if data.stdout != nil {
		change |= setValue(&resource.Status.StdoutFile, data.stdout.Path)
	}
	if data.stderr != nil {
		change |= setValue(&resource.Status.StderrFile, data.stderr.Path)
	}
	projectionChange, delay, valid := physicalContainerExecProjections.applyWithReason(
		data.state, data.progress, data.reason, data.message,
		&resource.Status.Phase, &resource.Status.Conditions, resource.Generation,
	)
	return change | projectionChange, delay, valid
}

var physicalContainerExecProjections = physicalResourceProjectionTable[physicalContainerExecState, apiv2.ContainerExecPhase]{
	invalidPhase: apiv2.ContainerExecPhaseFailed,
	projections: map[physicalResourceProjectionKey[physicalContainerExecState]]physicalResourceProjection[apiv2.ContainerExecPhase]{
		{state: physicalContainerExecStateNamespace, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotReady,
		},
		{state: physicalContainerExecStateNamespace, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceLookupFailed, requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerExecStateTarget, progress: physicalResourceProgressNotFound}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerNotFound",
		},
		{state: physicalContainerExecStateTarget, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerNotReady",
		},
		{state: physicalContainerExecStateTarget, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerLookupFailed",
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerExecStateTarget, progress: physicalResourceProgressFailed}: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerUnavailable",
		},
		{state: physicalContainerExecStateLaunch, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "Launching",
		},
		{state: physicalContainerExecStateLaunch, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "LaunchQueueFailed",
			requeue: true, requeueDelay: LongDelay,
		},
		{state: physicalContainerExecStateLaunch, progress: physicalResourceProgressFailed}: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "LaunchFailed",
		},
		{state: physicalContainerExecStateRuntime, progress: physicalResourceProgressRunning}: {
			phase: apiv2.ContainerExecPhaseRunning, conditionStatus: metav1.ConditionTrue, conditionReason: "ExecutionRunning",
		},
		{state: physicalContainerExecStateRuntime, progress: physicalResourceProgressExited}: {
			phase: apiv2.ContainerExecPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: "ExecutionExited",
		},
		{state: physicalContainerExecStateRuntime, progress: physicalResourceProgressFailed}: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "CompletionUnknown",
		},
		{state: physicalContainerExecStateStop, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "Stopping",
		},
		{state: physicalContainerExecStateInvalid, progress: physicalResourceProgressFailed}: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}
