/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	stdmaps "maps"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type physicalContainerNetworkDataStateKey string

type physicalContainerNetworkState int

const (
	physicalContainerNetworkStateCreate physicalContainerNetworkState = iota + 1
	physicalContainerNetworkStateCreateFailed
	physicalContainerNetworkStateCreateRetry
	physicalContainerNetworkStateCreated
	physicalContainerNetworkStateInspectFailed
	physicalContainerNetworkStateInvalid
	physicalContainerNetworkStateMissing
	physicalContainerNetworkStateReady
	physicalContainerNetworkStateRemove
	physicalContainerNetworkStateRemoveRetry
	physicalContainerNetworkStateRemoved
	physicalContainerNetworkStateReplaceRetry
	physicalContainerNetworkStateReplaced
	physicalContainerNetworkStateReplacementRejected
	physicalContainerNetworkStateResolve
)

type physicalContainerNetworkData struct {
	state          physicalContainerNetworkState
	workInProgress bool
	networkID      string
	failureMessage string
	retryAfter     time.Time
	resolveByName  bool
	connections    map[string]int
	expected       int
	connected      int
}

func (data *physicalContainerNetworkData) operationInProgress() bool {
	return data != nil && data.workInProgress
}

func (data *physicalContainerNetworkData) Clone() *physicalContainerNetworkData {
	return &physicalContainerNetworkData{
		state:          data.state,
		workInProgress: data.workInProgress,
		networkID:      data.networkID,
		failureMessage: data.failureMessage,
		retryAfter:     data.retryAfter,
		resolveByName:  data.resolveByName,
		connections:    stdmaps.Clone(data.connections),
		expected:       data.expected,
		connected:      data.connected,
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
	if data.workInProgress != other.workInProgress {
		data.workInProgress = other.workInProgress
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
	if !stdmaps.Equal(data.connections, other.connections) {
		data.connections = stdmaps.Clone(other.connections)
		updated = true
	}
	if data.expected != other.expected {
		data.expected = other.expected
		updated = true
	}
	if data.connected != other.connected {
		data.connected = other.connected
		updated = true
	}

	return updated
}

func (data *physicalContainerNetworkData) applyTo(
	network *apiv2.PhysicalContainerNetwork,
) (objectChange, AdditionalReconciliationDelay, bool) {
	change := noChange
	if data.networkID != "" {
		change |= setValue(&network.Status.NetworkID, data.networkID)
	}

	stateChange, delay, valid := physicalContainerNetworkProjections.apply(
		data.state,

		data.failureMessage,
		&network.Status.Phase,
		&network.Status.Conditions,
		network.Generation,
	)
	return change | stateChange, delay, valid
}

var physicalContainerNetworkProjections = physicalResourceProjectionTable[physicalContainerNetworkState, apiv2.PhysicalContainerNetworkPhase]{
	invalidPhase: apiv2.PhysicalContainerNetworkPhaseUnknown,
	projections: map[physicalContainerNetworkState]physicalResourceProjection[apiv2.PhysicalContainerNetworkPhase]{
		physicalContainerNetworkStateCreate: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonCreating,
			message:         "Runtime network creation is in progress.",
		},
		physicalContainerNetworkStateCreated: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonCreated,
			message:         "Runtime network creation completed.",
		},
		physicalContainerNetworkStateCreateRetry: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonCreateFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalContainerNetworkStateCreateFailed: {
			phase: apiv2.PhysicalContainerNetworkPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonCreateFailed,
		},
		physicalContainerNetworkStateReplaceRetry: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonExistingNetworkReplacementFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalContainerNetworkStateReady: {
			phase: apiv2.PhysicalContainerNetworkPhaseReady, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.PhysicalContainerNetworkReasonNetworkAvailable,
			message:         "Runtime network is available.",
			requeue:         true, requeueDelay: MonitoringDelay,
		},
		physicalContainerNetworkStateMissing: {
			phase: apiv2.PhysicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonRuntimeNetworkMissing,
			message:         "Runtime network was not found.",
			requeue:         true, requeueDelay: MonitoringDelay,
		},
		physicalContainerNetworkStateInspectFailed: {
			phase: apiv2.PhysicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonRuntimeNetworkInspectFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalContainerNetworkStateRemove: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonRuntimeNetworkRemoving,
			message:         "Runtime network removal is in progress.",
		},
		physicalContainerNetworkStateRemoveRetry: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonRuntimeNetworkRemoveFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		physicalContainerNetworkStateRemoved: {
			phase: apiv2.PhysicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonRuntimeNetworkRemoved,
			message:         "Runtime network removal completed.",
		},
		physicalContainerNetworkStateReplacementRejected: {
			phase: apiv2.PhysicalContainerNetworkPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable,
		},
		physicalContainerNetworkStateInvalid: {
			phase: apiv2.PhysicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}

func physicalContainerNetworkDataKey(network *apiv2.PhysicalContainerNetwork) physicalContainerNetworkDataStateKey {
	if network.UID != "" {
		return physicalContainerNetworkDataStateKey(network.UID)
	}
	return physicalContainerNetworkDataStateKey(network.NamespacedName().String())
}
