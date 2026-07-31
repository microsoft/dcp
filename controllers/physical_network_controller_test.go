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

func TestPhysicalNetworkCreateFailedTerminally(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		phase    apiv2.PhysicalNetworkPhase
		reason   string
		expected bool
	}{
		{
			name:     "create failure is terminal",
			phase:    apiv2.PhysicalNetworkPhaseFailed,
			reason:   apiv2.PhysicalNetworkReasonCreateFailed,
			expected: true,
		},
		{
			// A transient reconciliation failure (for example, a namespace read error)
			// must stay retryable, otherwise the network is stranded permanently.
			name:     "reconciliation failure stays retryable",
			phase:    apiv2.PhysicalNetworkPhaseFailed,
			reason:   apiv2.PhysicalNetworkReasonReconciliationFailed,
			expected: false,
		},
		{
			name:     "missing network is not terminal",
			phase:    apiv2.PhysicalNetworkPhaseMissing,
			reason:   apiv2.PhysicalNetworkReasonRuntimeNetworkMissing,
			expected: false,
		},
		{
			name:     "pending network is not terminal",
			phase:    apiv2.PhysicalNetworkPhasePending,
			reason:   apiv2.PhysicalNetworkReasonCreating,
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			network := &apiv2.PhysicalNetwork{
				Status: apiv2.PhysicalNetworkStatus{
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

			require.Equal(t, tc.expected, physicalNetworkCreateFailedTerminally(network))
		})
	}
}

func TestPhysicalNetworkCreateFailedTerminallyWithoutReadyCondition(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalNetwork{
		Status: apiv2.PhysicalNetworkStatus{
			Phase: apiv2.PhysicalNetworkPhaseFailed,
		},
	}

	require.False(t, physicalNetworkCreateFailedTerminally(network))
}

func TestPhysicalNetworkCreationLabels(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalNetwork{
		Spec: apiv2.PhysicalNetworkSpec{
			NetworkName: "test-runtime-network",
			Labels: []commonapi.Label{
				{Key: "test-label", Value: "test-value"},
			},
		},
	}

	labels := physicalNetworkCreationLabels(network, logr.Discard())

	require.Equal(t, "test-value", labels["test-label"])
	// Creator labels let startup harvesting reclaim networks abandoned by a crashed DCP process.
	require.Equal(t, "false", labels[PersistentLabel])
	require.NotEmpty(t, labels[CreatorProcessIdLabel])
	require.NotEmpty(t, labels[CreatorProcessStartTimeLabel])
}

func TestPhysicalNetworkCreationLabelsMarkPreservedNetworks(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalNetwork{
		Spec: apiv2.PhysicalNetworkSpec{
			NetworkName:        "test-runtime-network",
			PreserveOnDeletion: true,
		},
	}

	labels := physicalNetworkCreationLabels(network, logr.Discard())

	require.Equal(t, "true", labels[PersistentLabel])
}

func TestApplyReadyPhysicalNetworkStatus(t *testing.T) {
	t.Parallel()

	createdAt := time.Now().Add(-time.Minute)
	network := &apiv2.PhysicalNetwork{}
	inspectedNetwork := &containers.InspectedNetwork{
		Id:        "test-network-id",
		Name:      "test-runtime-network",
		Driver:    "bridge",
		IPv6:      true,
		Subnets:   []string{"10.0.0.0/24"},
		Gateways:  []string{"10.0.0.1"},
		CreatedAt: createdAt,
	}

	change := applyReadyPhysicalNetworkStatus(network, inspectedNetwork)

	require.NotEqual(t, noChange, change&statusChanged)
	require.Equal(t, apiv2.PhysicalNetworkPhaseReady, network.Status.Phase)
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
func TestApplyReadyPhysicalNetworkStatusIsIdempotent(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalNetwork{}
	inspectedNetwork := &containers.InspectedNetwork{
		Id:        "test-network-id",
		Name:      "test-runtime-network",
		Driver:    "bridge",
		Subnets:   []string{"10.0.0.0/24"},
		Gateways:  []string{"10.0.0.1"},
		CreatedAt: time.Now().Add(-time.Minute),
	}

	applyReadyPhysicalNetworkStatus(network, inspectedNetwork)
	change := applyReadyPhysicalNetworkStatus(network, inspectedNetwork)

	require.Equal(t, noChange, change&statusChanged)
	require.Equal(t, additionalReconciliationNeeded, change)
}

func TestSetPhysicalNetworkAddresses(t *testing.T) {
	t.Parallel()

	addresses := []string{"10.0.0.0/24"}

	require.Equal(t, statusChanged, setPhysicalNetworkAddresses(&addresses, []string{"10.0.1.0/24"}))
	require.Equal(t, []string{"10.0.1.0/24"}, addresses)
	require.Equal(t, noChange, setPhysicalNetworkAddresses(&addresses, []string{"10.0.1.0/24"}))
}

func TestPhysicalNetworkDataAppliesProgressToStatus(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		data           physicalNetworkData
		expectedPhase  apiv2.PhysicalNetworkPhase
		expectedReason string
		expectedStatus metav1.ConditionStatus
	}{
		{
			name:           "creating",
			data:           physicalNetworkData{conditionReason: apiv2.PhysicalNetworkReasonCreating},
			expectedPhase:  apiv2.PhysicalNetworkPhasePending,
			expectedReason: apiv2.PhysicalNetworkReasonCreating,
			expectedStatus: metav1.ConditionFalse,
		},
		{
			name: "create failed",
			data: physicalNetworkData{
				conditionReason: apiv2.PhysicalNetworkReasonCreateFailed,
				failureMessage:  "Failed to create runtime network: boom",
			},
			expectedPhase:  apiv2.PhysicalNetworkPhaseFailed,
			expectedReason: apiv2.PhysicalNetworkReasonCreateFailed,
			expectedStatus: metav1.ConditionFalse,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			network := &apiv2.PhysicalNetwork{}
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
func TestPhysicalNetworkDataCreatedOnlyRecordsNetworkID(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalNetwork{}
	data := physicalNetworkData{
		conditionReason: apiv2.PhysicalNetworkReasonCreated,
		networkID:       "test-network-id",
	}

	change := data.applyTo(network)

	require.Equal(t, statusChanged, change)
	require.Equal(t, "test-network-id", network.Status.NetworkID)
	require.Empty(t, network.Status.Phase)
	require.Empty(t, network.Status.Conditions)
}

func TestPhysicalNetworkDataOperationInProgress(t *testing.T) {
	t.Parallel()

	creating := physicalNetworkData{conditionReason: apiv2.PhysicalNetworkReasonCreating}
	created := physicalNetworkData{conditionReason: apiv2.PhysicalNetworkReasonCreated}
	failed := physicalNetworkData{conditionReason: apiv2.PhysicalNetworkReasonCreateFailed}

	require.True(t, creating.operationInProgress())
	require.False(t, created.operationInProgress())
	require.False(t, failed.operationInProgress())
}

func TestPhysicalNetworkDataUpdateFrom(t *testing.T) {
	t.Parallel()

	data := &physicalNetworkData{conditionReason: apiv2.PhysicalNetworkReasonCreating}
	result := &physicalNetworkData{
		conditionReason: apiv2.PhysicalNetworkReasonCreated,
		networkID:       "test-network-id",
	}

	require.True(t, data.UpdateFrom(result))
	require.Equal(t, apiv2.PhysicalNetworkReasonCreated, data.conditionReason)
	require.Equal(t, "test-network-id", data.networkID)

	require.False(t, data.UpdateFrom(result))
	require.False(t, data.UpdateFrom(nil))
}

func TestPhysicalNetworkDataKeyPrefersUID(t *testing.T) {
	t.Parallel()

	withUID := &apiv2.PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
			UID:       "test-uid",
		},
	}
	withoutUID := &apiv2.PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
		},
	}

	require.Equal(t, physicalNetworkDataStateKey("test-uid"), physicalNetworkDataKey(withUID))
	require.Equal(t, physicalNetworkDataStateKey("test-namespace/test-network"), physicalNetworkDataKey(withoutUID))
}
