/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type physicalNetworkDataStateKey string

type physicalNetworkData struct {
	conditionReason string
	networkID       string
	failureMessage  string
}

func (data *physicalNetworkData) Clone() *physicalNetworkData {
	return &physicalNetworkData{
		conditionReason: data.conditionReason,
		networkID:       data.networkID,
		failureMessage:  data.failureMessage,
	}
}

func (data *physicalNetworkData) UpdateFrom(other *physicalNetworkData) bool {
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

func (data *physicalNetworkData) operationInProgress() bool {
	return data.conditionReason == apiv2.PhysicalNetworkReasonCreating
}

func (data *physicalNetworkData) applyTo(network *apiv2.PhysicalNetwork) objectChange {
	change := noChange
	if data.networkID != "" {
		change |= setValue(&network.Status.NetworkID, data.networkID)
	}

	switch data.conditionReason {
	case apiv2.PhysicalNetworkReasonCreating:
		change |= setValue(&network.Status.Phase, apiv2.PhysicalNetworkPhasePending)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalNetworkReasonCreating, "Runtime network creation is in progress.")
		return change
	case apiv2.PhysicalNetworkReasonCreateFailed:
		change |= setValue(&network.Status.Phase, apiv2.PhysicalNetworkPhaseFailed)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
		return change
	default:
		return change
	}
}

func physicalNetworkDataKey(network *apiv2.PhysicalNetwork) physicalNetworkDataStateKey {
	if network.UID != "" {
		return physicalNetworkDataStateKey(network.UID)
	}
	return physicalNetworkDataStateKey(network.NamespacedName().String())
}
