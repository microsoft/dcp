/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type physicalContainerNetworkDataStateKey string

type physicalContainerNetworkData struct {
	conditionReason string
	networkID       string
	failureMessage  string
}

func (data *physicalContainerNetworkData) Clone() *physicalContainerNetworkData {
	return &physicalContainerNetworkData{
		conditionReason: data.conditionReason,
		networkID:       data.networkID,
		failureMessage:  data.failureMessage,
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
	if data.networkID != other.networkID {
		data.networkID = other.networkID
		updated = true
	}
	if data.failureMessage != other.failureMessage {
		data.failureMessage = other.failureMessage
		updated = true
	}

	return updated
}

func (data *physicalContainerNetworkData) operationInProgress() bool {
	return data.conditionReason == apiv2.PhysicalContainerNetworkReasonCreating
}

func (data *physicalContainerNetworkData) applyTo(network *apiv2.PhysicalContainerNetwork) objectChange {
	change := noChange
	if data.networkID != "" {
		change |= setValue(&network.Status.NetworkID, data.networkID)
	}

	switch data.conditionReason {
	case apiv2.PhysicalContainerNetworkReasonCreating:
		change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhasePending)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonCreating, "Runtime network creation is in progress.")
		return change
	case apiv2.PhysicalContainerNetworkReasonCreateFailed:
		change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseFailed)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
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
