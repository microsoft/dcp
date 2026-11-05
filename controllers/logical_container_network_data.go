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

type logicalContainerNetworkState int

const (
	logicalContainerNetworkStatePending logicalContainerNetworkState = iota + 1
	logicalContainerNetworkStatePreparationFailed
	logicalContainerNetworkStateCreating
	logicalContainerNetworkStateReady
	logicalContainerNetworkStateFailed
	logicalContainerNetworkStateUnknown
	logicalContainerNetworkStateDelete
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
	case logicalContainerNetworkStateDelete:
		return "Delete"
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
	reconcileErr        error
	lifecycleKey        string
	physicalNetworkUID  types.UID
	lease               *statestore.ResourceLease
	leaseFailureMessage string
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
	left.lease, right.lease = nil, nil
	if data.lease == other.lease && reflect.DeepEqual(left, right) {
		return false
	}
	*data = *other.Clone()
	return true
}

func (data *logicalContainerNetworkData) applyTo(network *apiv2.LogicalContainerNetwork) (objectChange, AdditionalReconciliationDelay, bool) {
	change := setValue(&network.Status.LifecycleKey, data.lifecycleKey)
	if data.physicalNetworkUID == "" {
		change |= setValue(&network.Status.PhysicalNetworkRef, "")
		change |= applyLogicalContainerNetworkPhysicalDetails(network, &apiv2.PhysicalContainerNetworkStatus{})
	}
	if _, valid := logicalContainerNetworkProjections.project(data.state); valid &&
		(data.reconcileErr != nil || data.leaseFailureMessage != "") {
		reason, message := data.reason, data.message
		if reason == "" {
			switch data.state {
			case logicalContainerNetworkStatePending:
				reason = apiv2.LogicalContainerNetworkReasonPhysicalNetworkPreparationFailed
			case logicalContainerNetworkStateDelete:
				reason = apiv2.LogicalContainerNetworkReasonPhysicalNetworkDeletionFailed
			default:
				reason = apiv2.LogicalContainerNetworkReasonPhysicalNetworkLookupFailed
			}
		}
		if data.leaseFailureMessage != "" {
			reason, message = apiv2.LogicalResourceReasonPersistenceFailed, data.leaseFailureMessage
		}
		change |= setValue(&network.Status.Phase, apiv2.LogicalContainerNetworkPhaseUnknown)
		change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation,
			metav1.ConditionFalse, reason, message)
		return change | additionalReconciliationNeeded, LongDelay, true
	}
	projectionChange, delay, valid := logicalContainerNetworkProjections.applyWithReason(
		data.state, data.reason, data.message, &network.Status.Phase, &network.Status.Conditions, network.Generation)
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
			conditionReason: apiv2.LogicalContainerNetworkReasonPhysicalNetworkPreparationPending,
		},
		logicalContainerNetworkStatePreparationFailed: {
			phase: apiv2.LogicalContainerNetworkPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerNetworkReasonPhysicalNetworkPreparationFailed,
		},
		logicalContainerNetworkStateCreating: {
			phase: apiv2.LogicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerNetworkReasonPhysicalNetworkNotReady,
		},
		logicalContainerNetworkStateReady: {
			phase: apiv2.LogicalContainerNetworkPhaseReady, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.LogicalContainerNetworkReasonNetworkAvailable, message: "The logical network is available.",
		},
		logicalContainerNetworkStateFailed: {
			phase: apiv2.LogicalContainerNetworkPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerNetworkReasonPhysicalNetworkFailed,
		},
		logicalContainerNetworkStateUnknown: {
			phase: apiv2.LogicalContainerNetworkPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerNetworkReasonPhysicalNetworkStateUnknown,
		},
		logicalContainerNetworkStateDelete: {
			phase: apiv2.LogicalContainerNetworkPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerNetworkReasonDeletingPhysicalNetwork, requeue: true,
		},
	},
}
