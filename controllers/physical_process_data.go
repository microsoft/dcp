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
	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/pkg/process"
)

type physicalProcessDataStateKey string

type physicalProcessState int

const (
	physicalProcessStateExited physicalProcessState = iota + 1
	physicalProcessStateInspectFailed
	physicalProcessStateInvalid
	physicalProcessStateLaunch
	physicalProcessStateLaunchFailed
	physicalProcessStateLaunchRetry
	physicalProcessStateLaunchSkipped
	physicalProcessStateMissing
	physicalProcessStateResolve
	physicalProcessStateResolveFailed
	physicalProcessStateResolveRetry
	physicalProcessStateRunning
	physicalProcessStateStop
	physicalProcessStateStopRetry
)

func (state physicalProcessState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalProcessStateExited:
		return "Exited"
	case physicalProcessStateInspectFailed:
		return "InspectFailed"
	case physicalProcessStateInvalid:
		return "Invalid"
	case physicalProcessStateLaunch:
		return "Launch"
	case physicalProcessStateLaunchFailed:
		return "LaunchFailed"
	case physicalProcessStateLaunchRetry:
		return "LaunchRetry"
	case physicalProcessStateLaunchSkipped:
		return "LaunchSkipped"
	case physicalProcessStateMissing:
		return "Missing"
	case physicalProcessStateResolve:
		return "Resolve"
	case physicalProcessStateResolveFailed:
		return "ResolveFailed"
	case physicalProcessStateResolveRetry:
		return "ResolveRetry"
	case physicalProcessStateRunning:
		return "Running"
	case physicalProcessStateStop:
		return "Stop"
	case physicalProcessStateStopRetry:
		return "StopRetry"
	default:
		return fmt.Sprintf("physicalProcessState(%d)", int(state))
	}
}

type physicalProcessData struct {
	resourceUID        types.UID
	state              physicalProcessState
	workInProgress     bool
	handle             process.ProcessHandle
	exitCode           *int32
	finishedAt         time.Time
	failureReason      apiv2.ConditionReason
	failureMessage     string
	cleanupUnconfirmed bool
	retryAfter         time.Time
	stdout             *logs.CaptureFile
	stderr             *logs.CaptureFile
	outputComplete     bool
}

func (data *physicalProcessData) Clone() *physicalProcessData {
	return &physicalProcessData{
		resourceUID:        data.resourceUID,
		state:              data.state,
		workInProgress:     data.workInProgress,
		handle:             data.handle,
		exitCode:           cloneInt32Pointer(data.exitCode),
		finishedAt:         data.finishedAt,
		failureReason:      data.failureReason,
		failureMessage:     data.failureMessage,
		cleanupUnconfirmed: data.cleanupUnconfirmed,
		retryAfter:         data.retryAfter,
		stdout:             data.stdout,
		stderr:             data.stderr,
		outputComplete:     data.outputComplete,
	}
}

func (data *physicalProcessData) UpdateFrom(other *physicalProcessData) bool {
	if other == nil {
		return false
	}

	updated := data.resourceUID != other.resourceUID ||
		data.state != other.state ||
		data.workInProgress != other.workInProgress ||
		data.handle != other.handle ||
		!int32PointersEqual(data.exitCode, other.exitCode) ||
		!data.finishedAt.Equal(other.finishedAt) ||
		data.failureReason != other.failureReason ||
		data.failureMessage != other.failureMessage ||
		data.cleanupUnconfirmed != other.cleanupUnconfirmed ||
		data.stdout != other.stdout || data.stderr != other.stderr ||
		data.outputComplete != other.outputComplete ||
		!data.retryAfter.Equal(other.retryAfter)
	if updated {
		*data = *other.Clone()
	}
	return updated
}

func (data *physicalProcessData) logsComplete(physicalProcess *apiv2.PhysicalProcess) bool {
	return data.outputComplete || data.state == physicalProcessStateLaunchFailed ||
		data.state == physicalProcessStateResolveFailed || data.state == physicalProcessStateLaunchSkipped ||
		(data.stdout == nil && ((data.state == physicalProcessStateRunning &&
			physicalProcess.Spec.StdoutFile == "" && physicalProcess.Spec.StderrFile == "") ||
			data.state == physicalProcessStateExited || data.state == physicalProcessStateMissing))
}

func (data *physicalProcessData) operationInProgress() bool {
	return data != nil && data.workInProgress
}

func (data *physicalProcessData) applyTo(
	physicalProcess *apiv2.PhysicalProcess,
) (objectChange, AdditionalReconciliationDelay, bool) {
	change := noChange
	if data.stdout != nil {
		change |= setValue(&physicalProcess.Status.StdoutFile, data.stdout.Path)
		change |= setValue(&physicalProcess.Status.StderrFile, data.stderr.Path)
	} else {
		change |= setValue(&physicalProcess.Status.StdoutFile, physicalProcess.Spec.StdoutFile)
		change |= setValue(&physicalProcess.Status.StderrFile, physicalProcess.Spec.StderrFile)
	}
	change |= setValue(&physicalProcess.Status.LogsComplete, data.logsComplete(physicalProcess))
	if data.handle.Pid > 0 {
		change |= setPhysicalProcessPID(&physicalProcess.Status.PID, int64(data.handle.Pid))
		change |= setTimestamp(&physicalProcess.Status.IdentityTimestamp, metav1.NewMicroTime(data.handle.IdentityTime))
	}
	if !data.finishedAt.IsZero() {
		change |= setTimestamp(&physicalProcess.Status.FinishedAt, metav1.NewMicroTime(data.finishedAt))
	}
	change |= setPhysicalProcessExitCode(&physicalProcess.Status.ExitCode, data.exitCode)

	stateChange, delay, valid := physicalProcessProjections.applyWithReason(
		data.state,

		data.failureReason,
		data.failureMessage,
		&physicalProcess.Status.Phase,
		&physicalProcess.Status.Conditions,
		physicalProcess.Generation,
	)
	return change | stateChange, delay, valid
}

var physicalProcessProjections = physicalResourceProjectionTable[physicalProcessState, apiv2.PhysicalProcessPhase]{
	invalidPhase: apiv2.PhysicalProcessPhaseUnknown,
	projections: map[physicalProcessState]physicalResourceProjection[apiv2.PhysicalProcessPhase]{
		physicalProcessStateLaunch: {
			phase: apiv2.PhysicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonLaunching,
			message:         "Physical process launch is in progress.",
		},
		physicalProcessStateLaunchRetry: {
			phase: apiv2.PhysicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonLaunchFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalProcessStateLaunchFailed: {
			phase: apiv2.PhysicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonLaunchFailed,
		},
		physicalProcessStateRunning: {
			phase: apiv2.PhysicalProcessPhaseRunning, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.PhysicalProcessReasonRuntimeProcessRunning,
			message:         "Runtime process is running.",
			requeue:         true, requeueDelay: MonitoringDelay,
		},
		physicalProcessStateExited: {
			phase: apiv2.PhysicalProcessPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonRuntimeProcessExited,
			message:         "Runtime process has exited.",
		},
		physicalProcessStateMissing: {
			phase: apiv2.PhysicalProcessPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonRuntimeProcessMissing,
			message:         "Runtime process was not found.",
		},
		physicalProcessStateInspectFailed: {
			phase: apiv2.PhysicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonRuntimeProcessInspectFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalProcessStateResolveRetry: {
			phase: apiv2.PhysicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonRuntimeProcessAlreadyTracked,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalProcessStateStop: {
			phase: apiv2.PhysicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonStopping,
			message:         "Physical process termination is in progress.",
		},
		physicalProcessStateStopRetry: {
			phase: apiv2.PhysicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonStopFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalProcessStateLaunchSkipped: {
			phase: apiv2.PhysicalProcessPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalProcessReasonStopRequested,
			message:         "Physical process was not launched because stop was requested.",
		},
		physicalProcessStateResolveFailed: {
			phase: apiv2.PhysicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
		physicalProcessStateInvalid: {
			phase: apiv2.PhysicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
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
