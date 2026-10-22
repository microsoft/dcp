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
)

type logicalProcessOperation struct {
	cancel context.CancelFunc
}

type logicalProcessData struct {
	state          logicalProcessState
	progress       physicalResourceProgress
	reason         apiv2.ConditionReason
	message        string
	retryAfter     time.Time
	operation      *logicalProcessOperation
	lifecycleKey   string
	physicalUID    types.UID
	replacementUID types.UID
	lease          *statestore.ResourceLease
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
	projectionChange, delay, valid := logicalProcessProjections.applyWithReason(
		data.state, data.progress, data.reason, data.message,
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
	projections: map[physicalResourceProjectionKey[logicalProcessState]]physicalResourceProjection[apiv2.LogicalProcessPhase]{
		{state: logicalProcessStateNamespace, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotReady,
		},
		{state: logicalProcessStateNamespace, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceLookupFailed, requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalProcessStatePrepare, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PreparingLifecycle",
		},
		{state: logicalProcessStatePrepare, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LifecyclePrepared", requeue: true,
		},
		{state: logicalProcessStatePrepare, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LifecyclePreparationFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalProcessStatePrepare, progress: physicalResourceProgressFailed}: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LifecyclePreparationFailed",
		},
		{state: logicalProcessStatePhysical, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessPreparationPending", requeue: true,
		},
		{state: logicalProcessStatePhysical, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessPreparationFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalProcessStatePhysical, progress: physicalResourceProgressFailed}: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessPreparationFailed",
		},
		{state: logicalProcessStateRuntime, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessNotReady",
		},
		{state: logicalProcessStateRuntime, progress: physicalResourceProgressRunning}: {
			phase: apiv2.LogicalProcessPhaseRunning, conditionStatus: metav1.ConditionTrue,
			conditionReason: "ProcessRunning",
		},
		{state: logicalProcessStateRuntime, progress: physicalResourceProgressExited}: {
			phase: apiv2.LogicalProcessPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: "ProcessExited",
		},
		{state: logicalProcessStateRuntime, progress: physicalResourceProgressFailed}: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessFailed",
		},
		{state: logicalProcessStateRuntime, progress: physicalResourceProgressUnknown}: {
			phase: apiv2.LogicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessStateUnknown",
		},
		{state: logicalProcessStateRuntime, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessLookupFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalProcessStatePersistence, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "RecordingPersistence",
		},
		{state: logicalProcessStatePersistence, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PersistenceRecorded", requeue: true,
		},
		{state: logicalProcessStatePersistence, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PersistenceFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalProcessStateDelete, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "DeletingPhysicalProcess", requeue: true,
		},
		{state: logicalProcessStateDelete, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalProcessDeletionFailed", requeue: true, requeueDelay: LongDelay,
		},
	},
}
