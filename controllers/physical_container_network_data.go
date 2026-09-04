/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type physicalContainerNetworkDataStateKey string

type physicalContainerNetworkState int

const (
	physicalContainerNetworkStateNamespace physicalContainerNetworkState = iota + 1
	physicalContainerNetworkStateResolve
	physicalContainerNetworkStateCreate
	physicalContainerNetworkStateReplace
	physicalContainerNetworkStateRuntime
	physicalContainerNetworkStateRemove
)

const (
	physicalContainerNetworkOperationInProgress   = physicalResourceProgressInProgress
	physicalContainerNetworkOperationCompleted    = physicalResourceProgressCompleted
	physicalContainerNetworkOperationRetryPending = physicalResourceProgressRetryPending
	physicalContainerNetworkOperationFailed       = physicalResourceProgressFailed
)

type physicalContainerNetworkData struct {
	state          physicalContainerNetworkState
	progress       physicalResourceProgress
	networkID      string
	failureMessage string
	retryAfter     time.Time
	resolveByName  bool
}

func (data *physicalContainerNetworkData) Clone() *physicalContainerNetworkData {
	return &physicalContainerNetworkData{
		state:          data.state,
		progress:       data.progress,
		networkID:      data.networkID,
		failureMessage: data.failureMessage,
		retryAfter:     data.retryAfter,
		resolveByName:  data.resolveByName,
	}
}

func (data *physicalContainerNetworkData) UpdateFrom(other *physicalContainerNetworkData) bool {
	if other == nil {
		return false
	}

	updated := false
	if data.state != other.state {
		data.state = other.state
		updated = true
	}
	if data.progress != other.progress {
		data.progress = other.progress
		updated = true
	}
	if data.networkID != other.networkID {
		data.networkID = other.networkID
		updated = true
	}
	if data.failureMessage != other.failureMessage {
		data.failureMessage = other.failureMessage
		updated = true
	}
	if !data.retryAfter.Equal(other.retryAfter) {
		data.retryAfter = other.retryAfter
		updated = true
	}
	if data.resolveByName != other.resolveByName {
		data.resolveByName = other.resolveByName
		updated = true
	}

	return updated
}

func (data *physicalContainerNetworkData) applyTo(network *apiv2.PhysicalContainerNetwork) objectChange {
	change := noChange
	if data.networkID != "" {
		change |= setValue(&network.Status.NetworkID, data.networkID)
	}

	stateChange, _, _ := physicalContainerNetworkProjections.apply(
		data.state,
		data.progress,
		data.failureMessage,
		&network.Status.Phase,
		&network.Status.Conditions,
		network.Generation,
	)
	return change | stateChange
}

var physicalContainerNetworkProjections = physicalResourceProjectionTable[physicalContainerNetworkState, apiv2.PhysicalContainerNetworkPhase]{
	invalidPhase: apiv2.PhysicalContainerNetworkPhaseUnknown,
	projections: map[physicalResourceProjectionKey[physicalContainerNetworkState]]physicalResourceProjection[apiv2.PhysicalContainerNetworkPhase]{
		{state: physicalContainerNetworkStateNamespace, progress: physicalResourceProgressNotFound}: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotFound,
		},
		{state: physicalContainerNetworkStateNamespace, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotReady,
		},
		{state: physicalContainerNetworkStateNamespace, progress: physicalResourceProgressTerminating}: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceTerminating,
		},
		{state: physicalContainerNetworkStateNamespace, progress: physicalResourceProgressNotActive}: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotActive,
		},
		{state: physicalContainerNetworkStateNamespace, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceLookupFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerNetworkStateCreate, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonCreating,
			message:         "Runtime network creation is in progress.",
		},
		{state: physicalContainerNetworkStateCreate, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonCreated,
			message:         "Runtime network creation completed.",
		},
		{state: physicalContainerNetworkStateCreate, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonCreateFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerNetworkStateCreate, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalContainerNetworkPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonCreateFailed,
		},
		{state: physicalContainerNetworkStateReplace, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonExistingNetworkReplacementFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerNetworkStateRuntime, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.PhysicalContainerNetworkPhaseReady, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.PhysicalContainerNetworkReasonNetworkAvailable,
			message:         "Runtime network is available.",
			requeue:         true, requeueDelay: MonitoringDelay,
		},
		{state: physicalContainerNetworkStateRuntime, progress: physicalResourceProgressMissing}: {
			phase: apiv2.PhysicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonRuntimeNetworkMissing,
			message:         "Runtime network was not found.",
			requeue:         true, requeueDelay: MonitoringDelay,
		},
		{state: physicalContainerNetworkStateRuntime, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonRuntimeNetworkInspectFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerNetworkStateRemove, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonRuntimeNetworkRemoving,
			message:         "Runtime network removal is in progress.",
		},
		{state: physicalContainerNetworkStateRemove, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonRuntimeNetworkRemoveFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: physicalContainerNetworkStateRemove, progress: physicalResourceProgressCompleted}: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonRuntimeNetworkRemoved,
			message:         "Runtime network removal completed.",
		},
		{state: physicalContainerNetworkStateReplace, progress: physicalResourceProgressFailed}: {
			phase: apiv2.PhysicalContainerNetworkPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable,
		},
	},
}

func physicalContainerNetworkDataKey(network *apiv2.PhysicalContainerNetwork) physicalContainerNetworkDataStateKey {
	if network.UID != "" {
		return physicalContainerNetworkDataStateKey(network.UID)
	}
	return physicalContainerNetworkDataStateKey(network.NamespacedName().String())
}
