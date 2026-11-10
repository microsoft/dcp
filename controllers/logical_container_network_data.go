/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"
	"maps"
	"reflect"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/statestore"
)

type logicalContainerNetworkState int

const (
	logicalContainerNetworkStatePending logicalContainerNetworkState = iota + 1
	logicalContainerNetworkStatePreparationFailed
	logicalContainerNetworkStateCreating
	logicalContainerNetworkStateReady
	logicalContainerNetworkStateFailed
	logicalContainerNetworkStateUnknown
)

func (state logicalContainerNetworkState) String() string {
	switch state {
	case 0:
		return "Unset"
	case logicalContainerNetworkStatePending:
		return "Pending"
	case logicalContainerNetworkStatePreparationFailed:
		return "PreparationFailed"
	case logicalContainerNetworkStateCreating:
		return "Creating"
	case logicalContainerNetworkStateReady:
		return "Ready"
	case logicalContainerNetworkStateFailed:
		return "Failed"
	case logicalContainerNetworkStateUnknown:
		return "Unknown"
	default:
		return fmt.Sprintf("logicalContainerNetworkState(%d)", int(state))
	}
}

func logicalContainerNetworkBackingState(physical *apiv2.PhysicalContainerNetwork) logicalContainerNetworkState {
	switch physical.Status.Phase {
	case apiv2.PhysicalContainerNetworkPhaseReady:
		ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
		if ready != nil && ready.Status == metav1.ConditionTrue {
			return logicalContainerNetworkStateReady
		}
	case apiv2.PhysicalContainerNetworkPhaseFailed:
		return logicalContainerNetworkStateFailed
	case apiv2.PhysicalContainerNetworkPhaseUnknown:
		return logicalContainerNetworkStateUnknown
	}
	return logicalContainerNetworkStateCreating
}

type logicalContainerNetworkData struct {
	state               logicalContainerNetworkState
	reason              apiv2.ConditionReason
	message             string
	retryAfter          time.Time
	lifecycleKey        string
	physicalNetworkUID  types.UID
	unobservedChildren  unobservedLogicalChildren
	lease               *statestore.ResourceLease
	leaseFailureMessage string
	started             bool
}

func (data *logicalContainerNetworkData) Clone() *logicalContainerNetworkData {
	cloned := *data
	cloned.unobservedChildren = maps.Clone(data.unobservedChildren)
	return &cloned
}

func (data *logicalContainerNetworkData) UpdateFrom(other *logicalContainerNetworkData) bool {
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

func (data *logicalContainerNetworkData) terminal() bool {
	return data.state == logicalContainerNetworkStatePreparationFailed || data.state == logicalContainerNetworkStateFailed
}

func (data *logicalContainerNetworkData) failMissing(message string) {
	data.state, data.reason, data.message = logicalContainerNetworkStateFailed,
		apiv2.LogicalContainerNetworkReasonFailedToStart, message
	if data.started {
		data.reason = apiv2.LogicalContainerNetworkReasonMissing
	}
}

func (data *logicalContainerNetworkData) updateLifecycleFromPhysicalContainerNetwork(physical *apiv2.PhysicalContainerNetwork) {
	data.state, data.reason, data.message = logicalContainerNetworkBackingState(physical), "", logicalBackingStatusMessage("PhysicalContainerNetwork", physical.Status.Conditions)
	switch data.state {
	case logicalContainerNetworkStateCreating:
		if data.started {
			data.state = logicalContainerNetworkStateUnknown
		}
	case logicalContainerNetworkStateReady:
		data.started = true
	case logicalContainerNetworkStateFailed:
		data.failMissing(data.message)
	case logicalContainerNetworkStateUnknown:
		if ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady)); ready != nil &&
			ready.Reason == string(apiv2.PhysicalContainerNetworkReasonRuntimeNetworkMissing) {
			data.failMissing(ready.Message)
		}
	}
}

func (data *logicalContainerNetworkData) applyTo(network *apiv2.LogicalContainerNetwork) (objectChange, AdditionalReconciliationDelay, bool) {
	change := setValue(&network.Status.LifecycleKey, data.lifecycleKey)
	if data.physicalNetworkUID == "" {
		change |= setValue(&network.Status.PhysicalNetworkRef, "")
		change |= applyLogicalContainerNetworkPhysicalDetails(network, &apiv2.PhysicalContainerNetworkStatus{})
	}
	message := data.message
	if !data.terminal() && data.leaseFailureMessage != "" {
		message = data.leaseFailureMessage
	}
	projectionChange, delay, valid := logicalContainerNetworkProjections.applyWithReason(
		data.state, data.reason, message, &network.Status.Phase, &network.Status.Conditions, network.Generation)
	if !data.terminal() && data.leaseFailureMessage != "" {
		projectionChange |= additionalReconciliationNeeded
		delay = LongDelay
	}
	if data.state == logicalContainerNetworkStatePending && !data.retryAfter.IsZero() {
		projectionChange |= additionalReconciliationNeeded
		delay = MonitoringDelay
	}
	return change | projectionChange, delay, valid
}

var logicalContainerNetworkProjections = physicalResourceProjectionTable[logicalContainerNetworkState, apiv2.LogicalContainerNetworkPhase]{
	invalidPhase: apiv2.LogicalContainerNetworkPhaseUnknown,
	projections: map[logicalContainerNetworkState]physicalResourceProjection[apiv2.LogicalContainerNetworkPhase]{
		logicalContainerNetworkStatePending: {
			phase: apiv2.LogicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerNetworkReasonStarting,
		},
		logicalContainerNetworkStatePreparationFailed: {
			phase: apiv2.LogicalContainerNetworkPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerNetworkReasonFailedToStart,
		},
		logicalContainerNetworkStateCreating: {
			phase: apiv2.LogicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerNetworkReasonStarting,
		},
		logicalContainerNetworkStateReady: {
			phase: apiv2.LogicalContainerNetworkPhaseReady, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.LogicalContainerNetworkReasonReady, message: "The logical network is available.",
		},
		logicalContainerNetworkStateFailed: {
			phase: apiv2.LogicalContainerNetworkPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerNetworkReasonFailedToStart,
		},
		logicalContainerNetworkStateUnknown: {
			phase: apiv2.LogicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerNetworkReasonLookupFailed, requeue: true, requeueDelay: LongDelay,
		},
	},
}
