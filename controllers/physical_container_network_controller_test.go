/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/commonapi"
)

func TestPhysicalContainerNetworkCreateFailedTerminally(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		phase    apiv2.PhysicalContainerNetworkPhase
		reason   string
		expected bool
	}{
		{
			name:     "create failure is terminal",
			phase:    apiv2.PhysicalContainerNetworkPhaseFailed,
			reason:   apiv2.PhysicalContainerNetworkReasonCreateFailed,
			expected: true,
		},
		{
			// A transient reconciliation failure (for example, a namespace read error)
			// must stay retryable, otherwise the network is stranded permanently.
			name:     "reconciliation failure stays retryable",
			phase:    apiv2.PhysicalContainerNetworkPhaseFailed,
			reason:   apiv2.PhysicalContainerNetworkReasonReconciliationFailed,
			expected: false,
		},
		{
			name:     "missing network is not terminal",
			phase:    apiv2.PhysicalContainerNetworkPhaseMissing,
			reason:   apiv2.PhysicalContainerNetworkReasonRuntimeNetworkMissing,
			expected: false,
		},
		{
			name:     "pending network is not terminal",
			phase:    apiv2.PhysicalContainerNetworkPhasePending,
			reason:   apiv2.PhysicalContainerNetworkReasonCreating,
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			network := &apiv2.PhysicalContainerNetwork{
				Status: apiv2.PhysicalContainerNetworkStatus{
					Phase: tc.phase,
					Conditions: []metav1.Condition{
						{
							Type:   apiv2.ConditionReady,
							Status: metav1.ConditionFalse,
							Reason: tc.reason,
						},
					},
				},
			}

			require.Equal(t, tc.expected, physicalContainerNetworkCreateFailedTerminally(network))
		})
	}
}

func TestPhysicalContainerNetworkCreateFailedTerminallyWithoutReadyCondition(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalContainerNetwork{
		Status: apiv2.PhysicalContainerNetworkStatus{
			Phase: apiv2.PhysicalContainerNetworkPhaseFailed,
		},
	}

	require.False(t, physicalContainerNetworkCreateFailedTerminally(network))
}

func TestPhysicalContainerNetworkCreationLabels(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalContainerNetwork{
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName: "test-runtime-network",
			Labels: []commonapi.Label{
				{Key: "test-label", Value: "test-value"},
			},
		},
	}

	labels := physicalContainerNetworkCreationLabels(network, logr.Discard())

	require.Equal(t, "test-value", labels["test-label"])
	// Creator labels let startup harvesting reclaim networks abandoned by a crashed DCP process.
	require.Equal(t, "false", labels[PersistentLabel])
	require.NotEmpty(t, labels[CreatorProcessIdLabel])
	require.NotEmpty(t, labels[CreatorProcessStartTimeLabel])
}

func TestPhysicalContainerNetworkCreationLabelsMarkPreservedNetworks(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalContainerNetwork{
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName:        "test-runtime-network",
			PreserveOnDeletion: true,
		},
	}

	labels := physicalContainerNetworkCreationLabels(network, logr.Discard())

	require.Equal(t, "true", labels[PersistentLabel])
}

func TestApplyReadyPhysicalContainerNetworkStatus(t *testing.T) {
	t.Parallel()

	createdAt := time.Now().Add(-time.Minute)
	network := &apiv2.PhysicalContainerNetwork{}
	inspectedNetwork := &containers.InspectedNetwork{
		Id:        "test-network-id",
		Name:      "test-runtime-network",
		Driver:    "bridge",
		IPv6:      true,
		Subnets:   []string{"10.0.0.0/24"},
		Gateways:  []string{"10.0.0.1"},
		CreatedAt: createdAt,
	}

	change := applyReadyPhysicalContainerNetworkStatus(network, inspectedNetwork)

	require.NotEqual(t, noChange, change&statusChanged)
	require.Equal(t, apiv2.PhysicalContainerNetworkPhaseReady, network.Status.Phase)
	require.Equal(t, "test-network-id", network.Status.NetworkID)
	require.Equal(t, "test-runtime-network", network.Status.NetworkName)
	require.Equal(t, "bridge", network.Status.Driver)
	require.True(t, network.Status.IPv6)
	require.Equal(t, []string{"10.0.0.0/24"}, network.Status.Subnets)
	require.Equal(t, []string{"10.0.0.1"}, network.Status.Gateways)
	require.False(t, network.Status.CreatedAt.IsZero())
	// Polling keeps a network removed outside of DCP from leaving a stale Ready status.
	require.NotEqual(t, noChange, change&additionalReconciliationNeeded)
}

// A ready network is re-inspected on every monitoring interval. The projection must report that
// nothing changed, otherwise steady-state polling produces a status write on every pass.
func TestApplyReadyPhysicalContainerNetworkStatusIsIdempotent(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalContainerNetwork{}
	inspectedNetwork := &containers.InspectedNetwork{
		Id:        "test-network-id",
		Name:      "test-runtime-network",
		Driver:    "bridge",
		Subnets:   []string{"10.0.0.0/24"},
		Gateways:  []string{"10.0.0.1"},
		CreatedAt: time.Now().Add(-time.Minute),
	}

	applyReadyPhysicalContainerNetworkStatus(network, inspectedNetwork)
	change := applyReadyPhysicalContainerNetworkStatus(network, inspectedNetwork)

	require.Equal(t, noChange, change&statusChanged)
	require.Equal(t, additionalReconciliationNeeded, change)
}

func TestSetPhysicalContainerNetworkAddresses(t *testing.T) {
	t.Parallel()

	addresses := []string{"10.0.0.0/24"}

	require.Equal(t, statusChanged, setPhysicalContainerNetworkAddresses(&addresses, []string{"10.0.1.0/24"}))
	require.Equal(t, []string{"10.0.1.0/24"}, addresses)
	require.Equal(t, noChange, setPhysicalContainerNetworkAddresses(&addresses, []string{"10.0.1.0/24"}))
}

func TestPhysicalContainerNetworkDataAppliesProgressToStatus(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		data           physicalContainerNetworkData
		expectedPhase  apiv2.PhysicalContainerNetworkPhase
		expectedReason string
		expectedStatus metav1.ConditionStatus
	}{
		{
			name:           "creating",
			data:           physicalContainerNetworkData{conditionReason: apiv2.PhysicalContainerNetworkReasonCreating},
			expectedPhase:  apiv2.PhysicalContainerNetworkPhasePending,
			expectedReason: apiv2.PhysicalContainerNetworkReasonCreating,
			expectedStatus: metav1.ConditionFalse,
		},
		{
			name: "create failed",
			data: physicalContainerNetworkData{
				conditionReason: apiv2.PhysicalContainerNetworkReasonCreateFailed,
				failureMessage:  "Failed to create runtime network: boom",
			},
			expectedPhase:  apiv2.PhysicalContainerNetworkPhaseFailed,
			expectedReason: apiv2.PhysicalContainerNetworkReasonCreateFailed,
			expectedStatus: metav1.ConditionFalse,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			network := &apiv2.PhysicalContainerNetwork{}
			data := tc.data

			change := data.applyTo(network)
			require.NotEqual(t, noChange, change&statusChanged)
			require.Equal(t, tc.expectedPhase, network.Status.Phase)

			readyCondition := network.Status.Conditions[0]
			require.Equal(t, apiv2.ConditionReady, readyCondition.Type)
			require.Equal(t, tc.expectedReason, readyCondition.Reason)
			require.Equal(t, tc.expectedStatus, readyCondition.Status)

			// Re-applying unchanged progress must not produce another status write.
			require.Equal(t, noChange, data.applyTo(network))
		})
	}
}

// Created is an internal progress marker; the reconciler projects the runtime status instead,
// so applying it must not overwrite the status with a partial record.
func TestPhysicalContainerNetworkDataCreatedOnlyRecordsNetworkID(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalContainerNetwork{}
	data := physicalContainerNetworkData{
		conditionReason: apiv2.PhysicalContainerNetworkReasonCreated,
		networkID:       "test-network-id",
	}

	change := data.applyTo(network)

	require.Equal(t, statusChanged, change)
	require.Equal(t, "test-network-id", network.Status.NetworkID)
	require.Empty(t, network.Status.Phase)
	require.Empty(t, network.Status.Conditions)
}

func TestPhysicalContainerNetworkDataOperationInProgress(t *testing.T) {
	t.Parallel()

	creating := physicalContainerNetworkData{conditionReason: apiv2.PhysicalContainerNetworkReasonCreating}
	created := physicalContainerNetworkData{conditionReason: apiv2.PhysicalContainerNetworkReasonCreated}
	failed := physicalContainerNetworkData{conditionReason: apiv2.PhysicalContainerNetworkReasonCreateFailed}

	require.True(t, creating.operationInProgress())
	require.False(t, created.operationInProgress())
	require.False(t, failed.operationInProgress())
}

func TestPhysicalContainerNetworkDataUpdateFrom(t *testing.T) {
	t.Parallel()

	data := &physicalContainerNetworkData{conditionReason: apiv2.PhysicalContainerNetworkReasonCreating}
	result := &physicalContainerNetworkData{
		conditionReason: apiv2.PhysicalContainerNetworkReasonCreated,
		networkID:       "test-network-id",
	}

	require.True(t, data.UpdateFrom(result))
	require.Equal(t, apiv2.PhysicalContainerNetworkReasonCreated, data.conditionReason)
	require.Equal(t, "test-network-id", data.networkID)

	require.False(t, data.UpdateFrom(result))
	require.False(t, data.UpdateFrom(nil))
}

func TestPhysicalContainerNetworkDataKeyPrefersUID(t *testing.T) {
	t.Parallel()

	withUID := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
			UID:       "test-uid",
		},
	}
	withoutUID := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
		},
	}

	require.Equal(t, physicalContainerNetworkDataStateKey("test-uid"), physicalContainerNetworkDataKey(withUID))
	require.Equal(t, physicalContainerNetworkDataStateKey("test-namespace/test-network"), physicalContainerNetworkDataKey(withoutUID))
}
