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

type physicalContainerNetworkOperationProgress int

const (
	physicalContainerNetworkOperationInProgress physicalContainerNetworkOperationProgress = iota + 1
	physicalContainerNetworkOperationCompleted
	physicalContainerNetworkOperationRetryPending
	physicalContainerNetworkOperationFailed
)

type physicalContainerNetworkData struct {
	conditionReason apiv2.ConditionReason
	progress        physicalContainerNetworkOperationProgress
	networkID       string
	failureMessage  string
	retryAfter      time.Time
	resolveByName   bool
}

func (data *physicalContainerNetworkData) Clone() *physicalContainerNetworkData {
	return &physicalContainerNetworkData{
		conditionReason: data.conditionReason,
		progress:        data.progress,
		networkID:       data.networkID,
		failureMessage:  data.failureMessage,
		retryAfter:      data.retryAfter,
		resolveByName:   data.resolveByName,
	}
}

func (data *physicalContainerNetworkData) UpdateFrom(other *physicalContainerNetworkData) bool {
	if other == nil {
		return false
	}

	updated := false
	if data.conditionReason != other.conditionReason {
		data.conditionReason = other.conditionReason
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

	switch data.conditionReason {
	case apiv2.PhysicalContainerNetworkReasonCreating:
		change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhasePending)
		change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonCreating, "Runtime network creation is in progress.")
		return change
	case apiv2.PhysicalContainerNetworkReasonRuntimeNetworkRemoving:
		change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhasePending)
		change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonRuntimeNetworkRemoving, "Runtime network removal is in progress.")
		return change
	case apiv2.PhysicalContainerNetworkReasonCreateFailed,
		apiv2.PhysicalContainerNetworkReasonExistingNetworkReplacementFailed:
		if data.progress == physicalContainerNetworkOperationRetryPending {
			change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhasePending)
		} else {
			change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseFailed)
		}
		change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
		return change
	case apiv2.PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable:
		change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseFailed)
		change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
		return change
	case apiv2.PhysicalContainerNetworkReasonRuntimeNetworkRemoveFailed:
		change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhasePending)
		change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
		return change
	case apiv2.PhysicalContainerNetworkReasonRuntimeNetworkRemoved:
		change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhasePending)
		change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionFalse, data.conditionReason, "Runtime network removal completed.")
		return change
	default:
		return change
	}
}

func physicalContainerNetworkDataKey(network *apiv2.PhysicalContainerNetwork) physicalContainerNetworkDataStateKey {
	if network.UID != "" {
		return physicalContainerNetworkDataStateKey(network.UID)
	}
	return physicalContainerNetworkDataStateKey(network.NamespacedName().String())
}
