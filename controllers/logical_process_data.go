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
	"github.com/microsoft/dcp/internal/statestore"
)

type logicalProcessState int

const (
	logicalProcessStatePrepare logicalProcessState = iota + 1
	logicalProcessStatePhysical
	logicalProcessStateRuntime
	logicalProcessStateDelete
	logicalProcessStateMissing
	logicalProcessStatePrepareFailed
	logicalProcessStatePhysicalFailed
	logicalProcessStateRunning
	logicalProcessStateExited
	logicalProcessStateRuntimeFailed
	logicalProcessStateUnknown
	logicalProcessStateInvalid
)

func (state logicalProcessState) String() string {
	switch state {
	case 0:
		return "Unset"
	case logicalProcessStatePrepare:
		return "Prepare"
	case logicalProcessStatePrepareFailed:
		return "PrepareFailed"
	case logicalProcessStatePhysical:
		return "Physical"
	case logicalProcessStatePhysicalFailed:
		return "PhysicalFailed"
	case logicalProcessStateRuntime:
		return "Runtime"
	case logicalProcessStateRunning:
		return "Running"
	case logicalProcessStateExited:
		return "Exited"
	case logicalProcessStateRuntimeFailed:
		return "RuntimeFailed"
	case logicalProcessStateUnknown:
		return "Unknown"
	case logicalProcessStateDelete:
		return "Delete"
	case logicalProcessStateInvalid:
		return "Invalid"
	case logicalProcessStateMissing:
		return "Missing"
	default:
		return fmt.Sprintf("logicalProcessState(%d)", int(state))
	}
}

type logicalProcessData struct {
	state      logicalProcessState
	reason     apiv2.ConditionReason
	message    string
	retryAfter time.Time
	// Most recent coordination error, retained until the next attempt.
	reconcileErr        error
	lifecycleKey        string
	physicalUID         types.UID
	replacementUID      types.UID
	lease               *statestore.ResourceLease
	leaseFailureMessage string
	// Stored identity is a handoff input, cleared after physical realization.
	pendingRecord   *statestore.PersistentProcessRecord
	replaceExisting bool
}

func (data *logicalProcessData) Clone() *logicalProcessData {
	cloned := *data
	if data.pendingRecord != nil {
		record := *data.pendingRecord
		cloned.pendingRecord = &record
	}
	return &cloned
}

func (data *logicalProcessData) UpdateFrom(other *logicalProcessData) bool {
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

func (data *logicalProcessData) applyTo(resource *apiv2.LogicalProcess) (objectChange, AdditionalReconciliationDelay, bool) {
	physicalRef := ""
	if data.physicalUID != "" {
		physicalRef = "logical-" + string(resource.UID)
	}
	change := setValue(&resource.Status.PhysicalProcessRef, physicalRef)
	change |= setValue(&resource.Status.LifecycleKey, data.lifecycleKey)
	if _, valid := logicalProcessProjections.project(data.state); valid &&
		(data.reconcileErr != nil || data.leaseFailureMessage != "") {
		phase := apiv2.LogicalProcessPhaseUnknown
		reason, message := data.reason, data.message
		if reason == "" {
			switch data.state {
			case logicalProcessStatePrepare:
				phase, reason = apiv2.LogicalProcessPhasePending, "LifecyclePreparationFailed"
			case logicalProcessStatePhysical:
				reason = "PhysicalProcessPreparationFailed"
			case logicalProcessStateDelete:
				reason = "PhysicalProcessDeletionFailed"
			default:
				reason = "PhysicalProcessLookupFailed"
			}
		}
		if data.leaseFailureMessage != "" {
			phase, reason, message = apiv2.LogicalProcessPhaseUnknown, "PersistenceFailed", data.leaseFailureMessage
		}
		change |= setValue(&resource.Status.Phase, phase)
		change |= setCondition(&resource.Status.Conditions, apiv2.ConditionReady, resource.Generation,
			metav1.ConditionFalse, reason, message)
		return change | additionalReconciliationNeeded, LongDelay, true
	}
	projectionChange, delay, valid := logicalProcessProjections.applyWithReason(
		data.state, data.reason, data.message,
		&resource.Status.Phase, &resource.Status.Conditions, resource.Generation,
	)
	return change | projectionChange, delay, valid
}

func (data *logicalProcessData) applyMissingLogSource(resource *apiv2.LogicalProcess) objectChange {
	// Preserve the backing identity so logical-only followers can observe its completion.
	source := &apiv2.LogSource{
		Name: "logical-" + string(resource.UID), UID: data.physicalUID, Complete: true,
	}
	if reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
		return noChange
	}
	resource.Status.RuntimeLogs = source
	return statusChanged
}

var logicalProcessProjections = physicalResourceProjectionTable[logicalProcessState, apiv2.LogicalProcessPhase]{
	invalidPhase: apiv2.LogicalProcessPhaseUnknown,
	projections: map[logicalProcessState]physicalResourceProjection[apiv2.LogicalProcessPhase]{
		logicalProcessStatePrepare: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PreparingLifecycle",
		},
		logicalProcessStatePrepareFailed: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LifecyclePreparationFailed",
		},
		logicalProcessStatePhysical: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessPreparationPending", requeue: true,
		},
		logicalProcessStatePhysicalFailed: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessPreparationFailed",
		},
		logicalProcessStateRuntime: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessNotReady",
		},
		logicalProcessStateRunning: {
			phase: apiv2.LogicalProcessPhaseRunning, conditionStatus: metav1.ConditionTrue,
			conditionReason: "ProcessRunning",
		},
		logicalProcessStateExited: {
			phase: apiv2.LogicalProcessPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: "ProcessExited",
		},
		logicalProcessStateRuntimeFailed: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessFailed",
		},
		logicalProcessStateUnknown: {
			phase: apiv2.LogicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessStateUnknown",
		},
		logicalProcessStateMissing: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessMissing",
		},
		logicalProcessStateDelete: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "DeletingPhysicalProcess", requeue: true,
		},
		logicalProcessStateInvalid: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}
