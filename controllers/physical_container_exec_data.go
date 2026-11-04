/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/logs"
)

type physicalContainerExecState int

const (
	physicalContainerExecStateTarget physicalContainerExecState = iota + 1
	physicalContainerExecStateTargetNotFound
	physicalContainerExecStateTargetRetry
	physicalContainerExecStateLaunch
	physicalContainerExecStateLaunchRetry
	physicalContainerExecStateRunning
	physicalContainerExecStateExited
	physicalContainerExecStateFailed
	physicalContainerExecStateStop
	physicalContainerExecStateInvalid
)

func (state physicalContainerExecState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalContainerExecStateTarget:
		return "Target"
	case physicalContainerExecStateTargetNotFound:
		return "TargetNotFound"
	case physicalContainerExecStateTargetRetry:
		return "TargetRetry"
	case physicalContainerExecStateLaunch:
		return "Launch"
	case physicalContainerExecStateLaunchRetry:
		return "LaunchRetry"
	case physicalContainerExecStateRunning:
		return "Running"
	case physicalContainerExecStateExited:
		return "Exited"
	case physicalContainerExecStateFailed:
		return "Failed"
	case physicalContainerExecStateStop:
		return "Stop"
	case physicalContainerExecStateInvalid:
		return "Invalid"
	default:
		return fmt.Sprintf("physicalContainerExecState(%d)", int(state))
	}
}

type physicalContainerExecSession struct {
	cancel context.CancelFunc
}

type physicalContainerExecData struct {
	state          physicalContainerExecState
	workInProgress bool
	reason         apiv2.ConditionReason
	message        string
	targetUID      types.UID
	containerID    string
	startedAt      metav1.MicroTime
	finishedAt     metav1.MicroTime
	exitCode       *int32
	retryAfter     time.Time
	session        *physicalContainerExecSession
	stdout         *logs.CaptureFile
	stderr         *logs.CaptureFile
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
	return data != nil && data.workInProgress
}

func (data *physicalContainerExecData) closeOutputLogs(option logs.CaptureFileCloseOption) error {
	var closeErr error
	for _, capture := range []*logs.CaptureFile{data.stdout, data.stderr} {
		if capture != nil {
			closeErr = errors.Join(closeErr, capture.Close(option))
		}
	}
	return closeErr
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
	complete := data.state == physicalContainerExecStateFailed ||
		data.state == physicalContainerExecStateInvalid || data.state == physicalContainerExecStateExited
	change |= setValue(&resource.Status.LogsComplete, complete)
	if data.stdout != nil {
		change |= setValue(&resource.Status.StdoutFile, data.stdout.Path)
	}
	if data.stderr != nil {
		change |= setValue(&resource.Status.StderrFile, data.stderr.Path)
	}
	projectionChange, delay, valid := physicalContainerExecProjections.applyWithReason(
		data.state, data.reason, data.message,
		&resource.Status.Phase, &resource.Status.Conditions, resource.Generation,
	)
	return change | projectionChange, delay, valid
}

var physicalContainerExecProjections = physicalResourceProjectionTable[physicalContainerExecState, apiv2.ContainerExecPhase]{
	invalidPhase: apiv2.ContainerExecPhaseFailed,
	projections: map[physicalContainerExecState]physicalResourceProjection[apiv2.ContainerExecPhase]{
		physicalContainerExecStateTargetNotFound: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerNotFound",
		},
		physicalContainerExecStateTarget: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerNotReady",
		},
		physicalContainerExecStateTargetRetry: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerLookupFailed",
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerExecStateLaunch: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "Launching",
		},
		physicalContainerExecStateLaunchRetry: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "LaunchQueueFailed",
			requeue: true, requeueDelay: LongDelay,
		},
		physicalContainerExecStateFailed: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "LaunchFailed",
		},
		physicalContainerExecStateRunning: {
			phase: apiv2.ContainerExecPhaseRunning, conditionStatus: metav1.ConditionTrue, conditionReason: "ExecutionRunning",
		},
		physicalContainerExecStateExited: {
			phase: apiv2.ContainerExecPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: "ExecutionExited",
		},
		physicalContainerExecStateStop: {
			phase: apiv2.ContainerExecPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "Stopping",
		},
		physicalContainerExecStateInvalid: {
			phase: apiv2.ContainerExecPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}
