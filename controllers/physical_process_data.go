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

type physicalProcessOperationProgress int

const (
	physicalProcessOperationInProgress physicalProcessOperationProgress = iota + 1
	physicalProcessOperationCompleted
	physicalProcessOperationRetryPending
)

type physicalProcessData struct {
	resourceUID     types.UID
	conditionReason apiv2.ConditionReason
	progress        physicalProcessOperationProgress
	handle          process.ProcessHandle
	exitCode        *int32
	finishedAt      time.Time
	failureMessage  string
	retryAfter      time.Time
}

func (data *physicalProcessData) Clone() *physicalProcessData {
	return &physicalProcessData{
		resourceUID:     data.resourceUID,
		conditionReason: data.conditionReason,
		progress:        data.progress,
		handle:          data.handle,
		exitCode:        cloneInt32Pointer(data.exitCode),
		finishedAt:      data.finishedAt,
		failureMessage:  data.failureMessage,
		retryAfter:      data.retryAfter,
	}
}

func (data *physicalProcessData) UpdateFrom(other *physicalProcessData) bool {
	if other == nil {
		return false
	}

	updated := data.resourceUID != other.resourceUID ||
		data.conditionReason != other.conditionReason ||
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
	return data.progress == physicalProcessOperationInProgress
}

func (data *physicalProcessData) applyTo(physicalProcess *apiv2.PhysicalProcess) objectChange {
	change := noChange
	if data.handle.Pid > 0 {
		change |= setPhysicalProcessPID(&physicalProcess.Status.PID, int64(data.handle.Pid))
		change |= setTimestamp(&physicalProcess.Status.IdentityTimestamp, metav1.NewMicroTime(data.handle.IdentityTime))
	}
	if !data.finishedAt.IsZero() {
		change |= setTimestamp(&physicalProcess.Status.FinishedAt, metav1.NewMicroTime(data.finishedAt))
	}
	change |= setPhysicalProcessExitCode(&physicalProcess.Status.ExitCode, data.exitCode)

	switch data.conditionReason {
	case apiv2.PhysicalProcessReasonLaunching:
		change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhasePending)
		change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, data.conditionReason, "Physical process launch is in progress.")
	case apiv2.PhysicalProcessReasonLaunchFailed:
		change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhasePending)
		change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
	case apiv2.PhysicalProcessReasonStopping:
		change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhasePending)
		change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, data.conditionReason, "Physical process termination is in progress.")
	case apiv2.PhysicalProcessReasonStopFailed:
		change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseUnknown)
		change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
	case apiv2.PhysicalProcessReasonRuntimeProcessExited:
		change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseExited)
		change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, data.conditionReason, "Runtime process has exited.")
	case apiv2.PhysicalProcessReasonRuntimeProcessMissing:
		change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseExited)
		change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, data.conditionReason, "Runtime process was not found.")
	}

	return change
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
