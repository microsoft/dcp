/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/commonapi"
)

func TestPhysicalContainerNetworkReconcileDelay(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		phase    apiv2.PhysicalContainerNetworkPhase
		reason   string
		deleting bool
		expected AdditionalReconciliationDelay
	}{
		{
			name:     "ready networks poll on the monitoring cadence",
			phase:    apiv2.PhysicalContainerNetworkPhaseReady,
			reason:   apiv2.PhysicalContainerNetworkReasonNetworkReady,
			expected: MonitoringDelay,
		},
		{
			name:     "missing networks keep observing on the monitoring cadence",
			phase:    apiv2.PhysicalContainerNetworkPhaseMissing,
			reason:   apiv2.PhysicalContainerNetworkReasonRuntimeNetworkMissing,
			expected: MonitoringDelay,
		},
		{
			// An unhealthy runtime should be retried sooner than steady-state monitoring.
			name:     "recoverable failures retry on the long cadence",
			phase:    apiv2.PhysicalContainerNetworkPhaseFailed,
			reason:   apiv2.PhysicalContainerNetworkReasonReconciliationFailed,
			expected: LongDelay,
		},
		{
			// LongDelay forces a requeue, so a terminal failure must not use it.
			name:     "terminal create failures do not retry",
			phase:    apiv2.PhysicalContainerNetworkPhaseFailed,
			reason:   apiv2.PhysicalContainerNetworkReasonCreateFailed,
			expected: StandardDelay,
		},
		{
			name:     "pending networks use the standard cadence",
			phase:    apiv2.PhysicalContainerNetworkPhasePending,
			reason:   apiv2.PhysicalContainerNetworkReasonCreating,
			expected: StandardDelay,
		},
		{
			name:     "deleting ready networks use the standard cadence",
			phase:    apiv2.PhysicalContainerNetworkPhaseReady,
			reason:   apiv2.PhysicalContainerNetworkReasonNetworkReady,
			deleting: true,
			expected: StandardDelay,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			network := &apiv2.PhysicalContainerNetwork{
				Status: apiv2.PhysicalContainerNetworkStatus{
					Phase: testCase.phase,
					Conditions: []metav1.Condition{
						{
							Type:   apiv2.ConditionReady,
							Status: metav1.ConditionFalse,
							Reason: testCase.reason,
						},
					},
				},
			}
			if testCase.deleting {
				deletionTimestamp := metav1.Now()
				network.DeletionTimestamp = &deletionTimestamp
			}

			require.Equal(t, testCase.expected, physicalContainerNetworkReconcileDelay(network))
		})
	}
}

func TestPhysicalContainerNetworkCreatedDataRemainsUntilStatusSave(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
			UID:       "test-uid",
		},
	}
	data := &physicalContainerNetworkData{
		conditionReason: apiv2.PhysicalContainerNetworkReasonCreated,
		networkID:       "test-network-id",
	}
	reconciler := &PhysicalContainerNetworkReconciler{
		orchestrator: &canonicalNetworkOrchestrator{},
		networkData: NewObjectStateMap[
			physicalContainerNetworkDataStateKey,
			physicalContainerNetworkData,
			*physicalContainerNetworkData,
			*apiv2.PhysicalContainerNetwork,
		](),
	}
	stateKey := physicalContainerNetworkDataKey(network)
	reconciler.networkData.Store(network.NamespacedName(), stateKey, data)

	change := handlePhysicalContainerNetworkCreated(t.Context(), reconciler, network, "", data, logr.Discard())

	_, savedData := reconciler.networkData.BorrowByNamespacedName(network.NamespacedName())
	require.NotNil(t, savedData, "the create result must survive until the status write succeeds")
	require.Equal(t, "canonical-network-id", network.Status.NetworkID)

	onSuccessfulSave := reconciler.physicalContainerNetworkDataSaveCallback(stateKey, data, change)
	require.NotNil(t, onSuccessfulSave)
	onSuccessfulSave()

	_, savedData = reconciler.networkData.BorrowByNamespacedName(network.NamespacedName())
	require.Nil(t, savedData)
}

func TestPhysicalContainerNetworkDataSaveCallbackAcknowledgesAlreadyDurableStatus(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
			UID:       "test-uid",
		},
		Status: apiv2.PhysicalContainerNetworkStatus{NetworkID: "test-network-id"},
	}
	reconciler := &PhysicalContainerNetworkReconciler{
		networkData: NewObjectStateMap[
			physicalContainerNetworkDataStateKey,
			physicalContainerNetworkData,
			*physicalContainerNetworkData,
			*apiv2.PhysicalContainerNetwork,
		](),
	}
	data := &physicalContainerNetworkData{
		conditionReason: apiv2.PhysicalContainerNetworkReasonCreated,
		networkID:       "test-network-id",
	}
	stateKey := physicalContainerNetworkDataKey(network)
	reconciler.networkData.Store(network.NamespacedName(), stateKey, data)

	require.Nil(t, reconciler.physicalContainerNetworkDataSaveCallback(stateKey, data, additionalReconciliationNeeded))
	_, savedData := reconciler.networkData.BorrowByNamespacedName(network.NamespacedName())
	require.Nil(t, savedData)
}

func TestPhysicalContainerNetworkCreatedDataSurvivesNamespaceNotReady(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	apiClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "missing-namespace",
			UID:       "test-uid",
		},
	}
	data := &physicalContainerNetworkData{
		conditionReason: apiv2.PhysicalContainerNetworkReasonCreated,
		networkID:       "test-network-id",
	}
	reconciler := &PhysicalContainerNetworkReconciler{
		ReconcilerBase: &ReconcilerBase[apiv2.PhysicalContainerNetwork, *apiv2.PhysicalContainerNetwork]{
			Client: apiClient,
		},
		networkData: NewObjectStateMap[
			physicalContainerNetworkDataStateKey,
			physicalContainerNetworkData,
			*physicalContainerNetworkData,
			*apiv2.PhysicalContainerNetwork,
		](),
	}
	reconciler.networkData.Store(network.NamespacedName(), physicalContainerNetworkDataKey(network), data)

	change, onSuccessfulSave := reconciler.managePhysicalContainerNetwork(t.Context(), network, logr.Discard())

	require.NotZero(t, change&statusChanged)
	require.NotZero(t, change&additionalReconciliationNeeded)
	require.Nil(t, onSuccessfulSave, "a namespace status update must not acknowledge unprojected create data")
	require.Empty(t, network.Status.NetworkID)
	_, savedData := reconciler.networkData.BorrowByNamespacedName(network.NamespacedName())
	require.NotNil(t, savedData)
	require.Equal(t, "test-network-id", savedData.networkID)
}

func TestPhysicalContainerNetworkRecoverableCreateFailureWaitsBeforeRetry(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalContainerNetwork{
		Spec: apiv2.PhysicalContainerNetworkSpec{NetworkName: "test-network"},
	}
	data := &physicalContainerNetworkData{
		conditionReason: apiv2.PhysicalContainerNetworkReasonReconciliationFailed,
		retryAfter:      time.Now().Add(time.Hour),
	}

	change := handlePhysicalContainerNetworkRecoverableCreateFailed(
		t.Context(),
		&PhysicalContainerNetworkReconciler{},
		network,
		"",
		data,
		logr.Discard(),
	)

	require.Equal(t, additionalReconciliationNeeded, change)
}

func TestPhysicalContainerNetworkRecoverableCreateFailureAdoptsCreatedNetwork(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
			UID:       "test-uid",
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{NetworkName: "test-runtime-network"},
	}
	data := &physicalContainerNetworkData{
		conditionReason: apiv2.PhysicalContainerNetworkReasonReconciliationFailed,
		failureMessage:  "create result was uncertain",
		retryAfter:      time.Now().Add(-time.Second),
	}
	reconciler := &PhysicalContainerNetworkReconciler{
		orchestrator: &canonicalNetworkOrchestrator{},
		networkData: NewObjectStateMap[
			physicalContainerNetworkDataStateKey,
			physicalContainerNetworkData,
			*physicalContainerNetworkData,
			*apiv2.PhysicalContainerNetwork,
		](),
	}
	reconciler.networkData.Store(network.NamespacedName(), physicalContainerNetworkDataKey(network), data.Clone())

	change := data.applyTo(network)
	change |= handlePhysicalContainerNetworkRecoverableCreateFailed(
		t.Context(),
		reconciler,
		network,
		"",
		data,
		logr.Discard(),
	)

	require.NotZero(t, change&statusChanged)
	require.Equal(t, apiv2.PhysicalContainerNetworkPhaseReady, network.Status.Phase)
	require.Equal(t, "canonical-network-id", network.Status.NetworkID)
	_, savedData := reconciler.networkData.BorrowByNamespacedName(network.NamespacedName())
	require.NotNil(t, savedData)
	require.Equal(t, apiv2.PhysicalContainerNetworkReasonCreated, savedData.conditionReason)
	require.Equal(t, "canonical-network-id", savedData.networkID)
}

func TestPhysicalContainerNetworkAlreadyExistsRemainsRetryableWhenVerificationFails(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{UID: "test-uid"},
		Spec:       apiv2.PhysicalContainerNetworkSpec{NetworkName: "test-runtime-network"},
	}
	data := &physicalContainerNetworkData{}
	reconciler := &PhysicalContainerNetworkReconciler{
		orchestrator: &alreadyExistsWithFailedInspectionOrchestrator{},
	}

	reconciler.applyPhysicalContainerNetworkCreateResult(
		t.Context(),
		network,
		data,
		"",
		containers.ErrAlreadyExists,
		logr.Discard(),
	)

	require.Equal(t, apiv2.PhysicalContainerNetworkReasonReconciliationFailed, data.conditionReason)
	require.Contains(t, data.failureMessage, "failed to verify whether creation succeeded")
	require.False(t, data.retryAfter.IsZero())
}

func TestPhysicalContainerNetworkPreservedDeletionSkipsUncertainCreateInspection(t *testing.T) {
	t.Parallel()

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-network",
			Namespace:  "test-namespace",
			UID:        "test-uid",
			Finalizers: []string{physicalContainerNetworkFinalizer},
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName:        "test-runtime-network",
			PreserveOnDeletion: true,
		},
	}
	data := &physicalContainerNetworkData{
		conditionReason: apiv2.PhysicalContainerNetworkReasonReconciliationFailed,
	}
	reconciler := &PhysicalContainerNetworkReconciler{
		orchestrator: &failingNetworkOrchestrator{},
		networkData: NewObjectStateMap[
			physicalContainerNetworkDataStateKey,
			physicalContainerNetworkData,
			*physicalContainerNetworkData,
			*apiv2.PhysicalContainerNetwork,
		](),
	}
	reconciler.networkData.Store(network.NamespacedName(), physicalContainerNetworkDataKey(network), data)

	change := reconciler.handleDeletionRequest(t.Context(), network, logr.Discard())

	require.Equal(t, metadataChanged, change)
	require.Empty(t, network.Finalizers)
}

// A recoverable inspection failure must keep asking for reconciliation even when repeating the
// failure produces no status change, otherwise the network never recovers once the runtime does.
func TestApplyRuntimeNetworkStatusKeepsRetryingRecoverableFailures(t *testing.T) {
	t.Parallel()

	reconciler := &PhysicalContainerNetworkReconciler{orchestrator: &failingNetworkOrchestrator{}}
	network := &apiv2.PhysicalContainerNetwork{
		Status: apiv2.PhysicalContainerNetworkStatus{NetworkID: "test-network-id"},
	}

	firstChange := reconciler.applyRuntimeNetworkStatus(t.Context(), network, "test-network-id", logr.Discard())
	require.NotZero(t, firstChange&statusChanged, "the first failure should record the failure in status")
	require.NotZero(t, firstChange&additionalReconciliationNeeded)
	require.Equal(t, apiv2.PhysicalContainerNetworkPhaseFailed, network.Status.Phase)

	secondChange := reconciler.applyRuntimeNetworkStatus(t.Context(), network, "test-network-id", logr.Discard())
	require.Zero(t, secondChange&statusChanged, "an unchanged failure should not produce a status write")
	require.NotZero(t, secondChange&additionalReconciliationNeeded, "an unchanged failure must still be retried")
}

// A missing runtime network keeps being observed rather than settling permanently.
func TestApplyRuntimeNetworkStatusKeepsObservingMissingNetworks(t *testing.T) {
	t.Parallel()

	reconciler := &PhysicalContainerNetworkReconciler{orchestrator: &missingNetworkOrchestrator{}}
	network := &apiv2.PhysicalContainerNetwork{}

	firstChange := reconciler.applyRuntimeNetworkStatus(t.Context(), network, "test-network-id", logr.Discard())
	require.NotZero(t, firstChange&additionalReconciliationNeeded)
	require.Equal(t, apiv2.PhysicalContainerNetworkPhaseMissing, network.Status.Phase)

	secondChange := reconciler.applyRuntimeNetworkStatus(t.Context(), network, "test-network-id", logr.Discard())
	require.Zero(t, secondChange&statusChanged, "an unchanged missing network should not produce a status write")
	require.NotZero(t, secondChange&additionalReconciliationNeeded)
}

type failingNetworkOrchestrator struct {
	containers.NetworkOrchestrator
}

func (o *failingNetworkOrchestrator) InspectNetworks(_ context.Context, _ containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	return nil, errors.New("container runtime is unhealthy")
}

type missingNetworkOrchestrator struct {
	containers.NetworkOrchestrator
}

func (o *missingNetworkOrchestrator) InspectNetworks(_ context.Context, _ containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	return nil, containers.ErrNotFound
}

type canonicalNetworkOrchestrator struct {
	containers.NetworkOrchestrator
}

func (o *canonicalNetworkOrchestrator) InspectNetworks(_ context.Context, _ containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	return []containers.InspectedNetwork{{
		Id:     "canonical-network-id",
		Labels: map[string]string{uidLabel: "test-uid"},
	}}, nil
}

type alreadyExistsWithFailedInspectionOrchestrator struct {
	containers.NetworkOrchestrator
}

func (o *alreadyExistsWithFailedInspectionOrchestrator) InspectNetworks(
	_ context.Context,
	_ containers.InspectNetworksOptions,
) ([]containers.InspectedNetwork, error) {
	return nil, errors.New("runtime inspection failed")
}

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
		ObjectMeta: metav1.ObjectMeta{UID: "test-uid"},
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
	require.Equal(t, "test-uid", labels[uidLabel])
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
