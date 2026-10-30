/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
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
	logicalProcessStateNamespace logicalProcessState = iota + 1
	logicalProcessStatePrepare
	logicalProcessStatePhysical
	logicalProcessStateRuntime
	logicalProcessStatePersistence
	logicalProcessStateDelete
	logicalProcessStateMissing
	logicalProcessStatePrepared
	logicalProcessStatePrepareRetry
	logicalProcessStatePrepareFailed
	logicalProcessStatePhysicalRetry
	logicalProcessStatePhysicalFailed
	logicalProcessStateRunning
	logicalProcessStateExited
	logicalProcessStateRuntimeFailed
	logicalProcessStateUnknown
	logicalProcessStateRuntimeRetry
	logicalProcessStatePersistenceRetry
	logicalProcessStateDeleteRetry
)

func (state logicalProcessState) String() string {
	switch state {
	case 0:
		return "Unset"
	case logicalProcessStateNamespace:
		return "Namespace"
	case logicalProcessStatePrepare:
		return "Prepare"
	case logicalProcessStatePrepared:
		return "Prepared"
	case logicalProcessStatePrepareRetry:
		return "PrepareRetry"
	case logicalProcessStatePrepareFailed:
		return "PrepareFailed"
	case logicalProcessStatePhysical:
		return "Physical"
	case logicalProcessStatePhysicalRetry:
		return "PhysicalRetry"
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
	case logicalProcessStateRuntimeRetry:
		return "RuntimeRetry"
	case logicalProcessStatePersistence:
		return "Persistence"
	case logicalProcessStatePersistenceRetry:
		return "PersistenceRetry"
	case logicalProcessStateDelete:
		return "Delete"
	case logicalProcessStateDeleteRetry:
		return "DeleteRetry"
	case logicalProcessStateMissing:
		return "Missing"
	default:
		return fmt.Sprintf("logicalProcessState(%d)", int(state))
	}
}

type logicalProcessOperation struct {
	cancel context.CancelFunc
}

type logicalProcessData struct {
	state               logicalProcessState
	reason              apiv2.ConditionReason
	message             string
	retryAfter          time.Time
	operation           *logicalProcessOperation
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
	left.operation, right.operation = nil, nil
	left.lease, right.lease = nil, nil
	if data.operation == other.operation && data.lease == other.lease && reflect.DeepEqual(left, right) {
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
	state, reason, message := data.state, data.reason, data.message
	if data.leaseFailureMessage != "" {
		state, reason, message = logicalProcessStatePersistenceRetry, "", data.leaseFailureMessage
	} else if data.lease != nil && data.operation != nil && data.state != logicalProcessStatePrepare {
		state, reason, message = logicalProcessStatePersistence, "", ""
	}
	projectionChange, delay, valid := logicalProcessProjections.applyWithReason(
		state, reason, message,
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
		logicalProcessStateNamespace: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotReady,
		},
		logicalProcessStatePrepare: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PreparingLifecycle",
		},
		logicalProcessStatePrepared: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LifecyclePrepared", requeue: true,
		},
		logicalProcessStatePrepareRetry: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LifecyclePreparationFailed", requeue: true, requeueDelay: LongDelay,
		},
		logicalProcessStatePrepareFailed: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LifecyclePreparationFailed",
		},
		logicalProcessStatePhysical: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessPreparationPending", requeue: true,
		},
		logicalProcessStatePhysicalRetry: {
			phase: apiv2.LogicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessPreparationFailed", requeue: true, requeueDelay: LongDelay,
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
		logicalProcessStateRuntimeRetry: {
			phase: apiv2.LogicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessLookupFailed", requeue: true, requeueDelay: LongDelay,
		},
		logicalProcessStateMissing: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessMissing",
		},
		logicalProcessStatePersistence: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "RecordingPersistence",
		},
		logicalProcessStatePersistenceRetry: {
			phase: apiv2.LogicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PersistenceFailed", requeue: true, requeueDelay: LongDelay,
		},
		logicalProcessStateDelete: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "DeletingPhysicalProcess", requeue: true,
		},
		logicalProcessStateDeleteRetry: {
			phase: apiv2.LogicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessDeletionFailed", requeue: true, requeueDelay: LongDelay,
		},
	},
}
