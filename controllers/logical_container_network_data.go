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

type logicalContainerNetworkState int

const (
	logicalContainerNetworkStateLifecycle logicalContainerNetworkState = iota + 1
	logicalContainerNetworkStateLookup
	logicalContainerNetworkStatePhysical
	logicalContainerNetworkStateRuntime
	logicalContainerNetworkStatePersistence
	logicalContainerNetworkStateDelete
	logicalContainerNetworkStateInvalid
)

type logicalContainerNetworkOperation struct {
	cancel context.CancelFunc
}

type logicalContainerNetworkData struct {
	state                logicalContainerNetworkState
	progress             physicalResourceProgress
	reason               apiv2.ConditionReason
	message              string
	retryAfter           time.Time
	operation            *logicalContainerNetworkOperation
	lifecycleKey         string
	generatedNetworkName string
	// Runtime identity selected for adoption, retained only until physical realization succeeds.
	pendingNetworkID        string
	replaceExisting         bool
	physicalNetworkUID      types.UID
	physicalNetworkReleased bool
	lease                   *statestore.ResourceLease
	leaseFailureMessage     string
}

func (data *logicalContainerNetworkData) operationInProgress() bool {
	return data != nil && data.operation != nil
}

func (data *logicalContainerNetworkData) Clone() *logicalContainerNetworkData {
	cloned := *data
	return &cloned
}

func (data *logicalContainerNetworkData) UpdateFrom(other *logicalContainerNetworkData) bool {
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

func (data *logicalContainerNetworkData) applyTo(network *apiv2.LogicalContainerNetwork) (objectChange, AdditionalReconciliationDelay, bool) {
	physicalRef := ""
	if data.physicalNetworkUID != "" {
		physicalRef = "logical-" + string(network.UID)
	}
	change := setValue(&network.Status.PhysicalNetworkRef, physicalRef)
	change |= setValue(&network.Status.LifecycleKey, data.lifecycleKey)
	if data.physicalNetworkUID == "" {
		change |= applyLogicalContainerNetworkPhysicalDetails(network, &apiv2.PhysicalContainerNetworkStatus{})
	}
	state, progress, reason, message := data.state, data.progress, data.reason, data.message
	if data.leaseFailureMessage != "" {
		state, progress = logicalContainerNetworkStatePersistence, physicalResourceProgressRetryPending
		reason, message = "", data.leaseFailureMessage
	} else if data.lease != nil && data.operation != nil && data.progress != physicalResourceProgressInProgress {
		state, progress = logicalContainerNetworkStatePersistence, physicalResourceProgressInProgress
		reason, message = "", ""
	}
	projectionChange, delay, valid := logicalContainerNetworkProjections.applyWithReason(
		state, progress, reason, message,
		&network.Status.Phase, &network.Status.Conditions, network.Generation,
	)
	return change | projectionChange, delay, valid
}

var logicalContainerNetworkProjections = physicalResourceProjectionTable[logicalContainerNetworkState, apiv2.LogicalContainerNetworkPhase]{
	invalidPhase: apiv2.LogicalContainerNetworkPhaseUnknown,
	projections: map[physicalResourceProjectionKey[logicalContainerNetworkState]]physicalResourceProjection[apiv2.LogicalContainerNetworkPhase]{
		{state: logicalContainerNetworkStateLifecycle, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.LogicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PreparingLifecycle", message: "Logical network lifecycle preparation is in progress.",
		},
		{state: logicalContainerNetworkStateLifecycle, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LifecyclePreparationFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerNetworkStateLifecycle, progress: physicalResourceProgressFailed}: {
			phase: apiv2.LogicalContainerNetworkPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LifecyclePreparationFailed",
		},
		{state: logicalContainerNetworkStateLookup, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.LogicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LookingUpNetwork", message: "Looking up the existing runtime network.",
		},
		{state: logicalContainerNetworkStateLookup, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "NetworkLookupFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerNetworkStateLookup, progress: physicalResourceProgressNotFound}: {
			phase: apiv2.LogicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "ExistingNetworkNotFound", requeue: true, requeueDelay: MonitoringDelay,
		},
		{state: logicalContainerNetworkStatePhysical, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.LogicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalNetworkPreparationPending",
		},
		{state: logicalContainerNetworkStatePhysical, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalNetworkPreparationFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerNetworkStatePhysical, progress: physicalResourceProgressFailed}: {
			phase: apiv2.LogicalContainerNetworkPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalNetworkPreparationFailed",
		},
		{state: logicalContainerNetworkStateRuntime, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.LogicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalNetworkNotReady",
		},
		{state: logicalContainerNetworkStateRuntime, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.LogicalContainerNetworkPhaseReady, conditionStatus: metav1.ConditionTrue,
			conditionReason: "NetworkAvailable", message: "The logical network is available.",
		},
		{state: logicalContainerNetworkStateRuntime, progress: physicalResourceProgressFailed}: {
			phase: apiv2.LogicalContainerNetworkPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalNetworkFailed",
		},
		{state: logicalContainerNetworkStateRuntime, progress: physicalResourceProgressUnknown}: {
			phase: apiv2.LogicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalNetworkStateUnknown",
		},
		{state: logicalContainerNetworkStateRuntime, progress: physicalResourceProgressMissing}: {
			phase: apiv2.LogicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalNetworkMissing",
		},
		{state: logicalContainerNetworkStateRuntime, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalNetworkLookupFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerNetworkStatePersistence, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.LogicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "RecordingPersistence", message: "Recording network ownership and releasing its startup lease.",
		},
		{state: logicalContainerNetworkStatePersistence, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PersistenceFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerNetworkStateDelete, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.LogicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "DeletingPhysicalNetwork", requeue: true,
		},
		{state: logicalContainerNetworkStateDelete, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalNetworkDeletionFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerNetworkStateInvalid, progress: physicalResourceProgressFailed}: {
			phase: apiv2.LogicalContainerNetworkPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}
