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
	"github.com/microsoft/dcp/pkg/process"
)

type physicalProcessDataStateKey string

type physicalProcessState int

const (
	physicalProcessStateNamespace physicalProcessState = iota + 1
	physicalProcessStateResolve
	physicalProcessStateLaunch
	physicalProcessStateRuntime
	physicalProcessStateStop
	physicalProcessStateInvalid
)

type physicalProcessData struct {
	resourceUID    types.UID
	state          physicalProcessState
	progress       physicalResourceProgress
	handle         process.ProcessHandle
	exitCode       *int32
	finishedAt     time.Time
	failureMessage string
	retryAfter     time.Time
}

func (data *physicalProcessData) Clone() *physicalProcessData {
	return &physicalProcessData{
		resourceUID:    data.resourceUID,
		state:          data.state,
		progress:       data.progress,
		handle:         data.handle,
		exitCode:       cloneInt32Pointer(data.exitCode),
		finishedAt:     data.finishedAt,
		failureMessage: data.failureMessage,
		retryAfter:     data.retryAfter,
	}
}

func (data *physicalProcessData) UpdateFrom(other *physicalProcessData) bool {
	if other == nil {
		return false
	}

	updated := data.resourceUID != other.resourceUID ||
		data.state != other.state ||
		data.progress != other.progress ||
		data.handle != other.handle ||
		!int32PointersEqual(data.exitCode, other.exitCode) ||
		!data.finishedAt.Equal(other.finishedAt) ||
		data.failureMessage != other.failureMessage ||
		!data.retryAfter.Equal(other.retryAfter)
	if updated {
		*data = *other.Clone()
	}
	return updated
}

func (data *physicalProcessData) operationInProgress() bool {
	return data.progress == physicalResourceProgressInProgress
}

func (data *physicalProcessData) applyTo(
	physicalProcess *apiv2.PhysicalProcess,
) (objectChange, AdditionalReconciliationDelay, bool) {
	change := noChange
	if data.handle.Pid > 0 {
		change |= setPhysicalProcessPID(&physicalProcess.Status.PID, int64(data.handle.Pid))
		change |= setTimestamp(&physicalProcess.Status.IdentityTimestamp, metav1.NewMicroTime(data.handle.IdentityTime))
	}
	if !data.finishedAt.IsZero() {
		change |= setTimestamp(&physicalProcess.Status.FinishedAt, metav1.NewMicroTime(data.finishedAt))
	}
	change |= setPhysicalProcessExitCode(&physicalProcess.Status.ExitCode, data.exitCode)

	stateChange, delay, valid := physicalProcessProjections.apply(
		data.state,
		data.progress,
		data.failureMessage,
		&physicalProcess.Status.Phase,
		&physicalProcess.Status.Conditions,
		physicalProcess.Generation,
	)
	return change | stateChange, delay, valid
}

var physicalProcessProjections = physicalResourceProjectionTable[physicalProcessState, apiv2.PhysicalProcessPhase]{
	invalidPhase: apiv2.PhysicalProcessPhaseUnknown,
	projections: map[physicalResourceProjectionKey[physicalProcessState]]physicalResourceProjection[apiv2.PhysicalProcessPhase]{
		{state: physicalProcessStateNamespace, progress: physicalResourceProgressNotFound}: {
			phase: apiv2.PhysicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotFound,
		},
		{state: physicalProcessStateNamespace, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.PhysicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotReady,
		},
		{state: physicalProcessStateNamespace, progress: physicalResourceProgressTerminating}: {
			phase: apiv2.PhysicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceTerminating,
		},
		{state: physicalProcessStateNamespace, progress: physicalResourceProgressNotActive}: {
			phase: apiv2.PhysicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotActive,
		},
		{state: physicalProcessStateNamespace, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceLookupFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalProcessStateLaunch, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.PhysicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonLaunching,
			message:         "Physical process launch is in progress.",
		},
		{state: physicalProcessStateLaunch, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonLaunchFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalProcessStateRuntime, progress: physicalResourceProgressRunning}: {
			phase: apiv2.PhysicalProcessPhaseRunning, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.PhysicalProcessReasonRuntimeProcessRunning,
			message:         "Runtime process is running.",
			requeue:         true, requeueDelay: MonitoringDelay,
		},
		{state: physicalProcessStateRuntime, progress: physicalResourceProgressExited}: {
			phase: apiv2.PhysicalProcessPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonRuntimeProcessExited,
			message:         "Runtime process has exited.",
		},
		{state: physicalProcessStateRuntime, progress: physicalResourceProgressMissing}: {
			phase: apiv2.PhysicalProcessPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonRuntimeProcessMissing,
			message:         "Runtime process was not found.",
		},
		{state: physicalProcessStateRuntime, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonRuntimeProcessInspectFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalProcessStateResolve, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonRuntimeProcessAlreadyTracked,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalProcessStateStop, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.PhysicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonStopping,
			message:         "Physical process termination is in progress.",
		},
		{state: physicalProcessStateStop, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonStopFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalProcessStateLaunch, progress: physicalResourceProgressSkipped}: {
			phase: apiv2.PhysicalProcessPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonStopRequested,
			message:         "Physical process was not launched because stop was requested.",
		},
		{state: physicalProcessStateResolve, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
		{state: physicalProcessStateInvalid, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
			requeue:         true, requeueDelay: LongDelay,
		},
	},
}

func physicalProcessDataKey(physicalProcess *apiv2.PhysicalProcess) physicalProcessDataStateKey {
	if physicalProcess.UID != "" {
		return physicalProcessDataStateKey(physicalProcess.UID)
	}
	return physicalProcessDataStateKey(physicalProcess.NamespacedName().String())
}

func physicalProcessHandleDataKey(handle process.ProcessHandle) physicalProcessDataStateKey {
	return physicalProcessDataStateKey(process.FormatIdentityTime(handle.IdentityTime) + "/" + handlePIDString(handle))
}

func cloneInt32Pointer(value *int32) *int32 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func int32PointersEqual(left *int32, right *int32) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func setPhysicalProcessPID(target **int64, value int64) objectChange {
	if *target != nil && **target == value {
		return noChange
	}
	*target = &value
	return statusChanged
}

func setPhysicalProcessExitCode(target **int32, value *int32) objectChange {
	if int32PointersEqual(*target, value) {
		return noChange
	}
	*target = cloneInt32Pointer(value)
	return statusChanged
}
