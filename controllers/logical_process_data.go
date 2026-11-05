/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"
	"reflect"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/statestore"
)

type logicalProcessState int

const (
	logicalProcessStatePrepare logicalProcessState = iota + 1
	logicalProcessStatePhysical
	logicalProcessStateCreating
	logicalProcessStateDelete
	logicalProcessStateMissing
	logicalProcessStatePrepareFailed
	logicalProcessStatePhysicalFailed
	logicalProcessStateRunning
	logicalProcessStateExited
	logicalProcessStateRuntimeFailed
	logicalProcessStateUnknown
	logicalProcessStateStopping
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
	case logicalProcessStateCreating:
		return "Creating"
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
	case logicalProcessStateStopping:
		return "Stopping"
	case logicalProcessStateMissing:
		return "Missing"
	default:
		return fmt.Sprintf("logicalProcessState(%d)", int(state))
	}
}

func logicalProcessBackingState(physical *apiv2.PhysicalProcess) logicalProcessState {
	if !physical.DeletionTimestamp.IsZero() {
		return logicalProcessStateUnknown
	}
	ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	switch physical.Status.Phase {
	case apiv2.PhysicalProcessPhaseRunning:
		if ready != nil && ready.Status == metav1.ConditionTrue {
			return logicalProcessStateRunning
		}
	case apiv2.PhysicalProcessPhaseExited:
		return logicalProcessStateExited
	case apiv2.PhysicalProcessPhaseFailed:
		return logicalProcessStateRuntimeFailed
	case apiv2.PhysicalProcessPhaseUnknown:
		return logicalProcessStateUnknown
	}
	return logicalProcessStateCreating
}

func logicalPhysicalProcessStartupSettled(physical *apiv2.PhysicalProcess) bool {
	if !physical.DeletionTimestamp.IsZero() {
		return !hasFinalizer(physical, physicalProcessFinalizer)
	}
	ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	if ready == nil {
		return false
	}
	switch physical.Status.Phase {
	case apiv2.PhysicalProcessPhaseRunning:
		return ready.Status == metav1.ConditionTrue
	case apiv2.PhysicalProcessPhaseExited, apiv2.PhysicalProcessPhaseFailed:
		return true
	default:
		return physical.Status.Phase == apiv2.PhysicalProcessPhasePending &&
			ready.Reason == string(apiv2.PhysicalResourceReasonNamespaceTerminating)
	}
}

func applyLogicalProcessBackingStatus(resource *apiv2.LogicalProcess, physical *apiv2.PhysicalProcess) (objectChange, logicalProcessState) {
	change := noChange
	status := &physical.Status
	source := physical.GetLogSource()
	if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
		resource.Status.RuntimeLogs = source
		change |= statusChanged
	}
	if !reflect.DeepEqual(resource.Status.PID, status.PID) {
		resource.Status.PID = status.DeepCopy().PID
		change |= statusChanged
	}
	if !reflect.DeepEqual(resource.Status.ExitCode, status.ExitCode) {
		resource.Status.ExitCode = cloneInt32Pointer(status.ExitCode)
		change |= statusChanged
	}
	change |= setTimestamp(&resource.Status.IdentityTimestamp, status.IdentityTimestamp)
	change |= setTimestamp(&resource.Status.FinishedAt, status.FinishedAt)
	return change, logicalProcessBackingState(physical)
}

type logicalProcessData struct {
	state      logicalProcessState
	reason     apiv2.ConditionReason
	message    string
	retryAfter time.Time
	// Most recent coordination error, retained until the next attempt.
	reconcileErr        error
	runConfig           *apiv2.LogicalProcessConfig
	ports               []logicalPortData
	lifecycleKey        string
	physicalUID         types.UID
	replacementUID      types.UID
	lease               *statestore.ResourceLease
	leaseFailureMessage string
	// Stored process identity is a handoff input, cleared after the backing PhysicalProcess is established.
	pendingRecord   *statestore.PersistentProcessRecord
	replaceExisting bool
}

func (data *logicalProcessData) Clone() *logicalProcessData {
	cloned := *data
	cloned.runConfig = data.runConfig.DeepCopy()
	cloned.ports = append([]logicalPortData(nil), data.ports...)
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
	change |= projectLogicalPorts(data.ports, &resource.Status.Ports)
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
		logicalProcessStateCreating: {
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
		logicalProcessStateStopping: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "ProcessStopping", message: "Waiting for the physical controller to stop the process.", requeue: true,
		},
	},
}
