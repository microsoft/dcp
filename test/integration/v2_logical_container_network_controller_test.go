/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that LogicalContainerNetwork waits for an active namespace, delegates session
// creation to an owned physical resource, and removes that resource and its runtime on deletion.
func TestV2LogicalContainerNetworkControllerDelegatesSessionLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: "v2-logical-network-session"},
		Spec: apiv2.LogicalContainerNetworkSpec{Network: &apiv2.LogicalContainerNetworkConfig{
			IPv6: true, Labels: []commonapi.Label{{Key: "app", Value: "session"}},
		}},
	}
	require.NoError(t, client.Create(ctx, network))
	pending := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == string(apiv2.PhysicalResourceReasonNamespaceNotFound), nil
	})
	require.Empty(t, pending.Status.PhysicalNetworkRef)
	createActiveV2Namespace(t, ctx, network.Namespace)
	ready := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	requireReadyCondition(t, ready.Status.Conditions, metav1.ConditionTrue, "NetworkAvailable")
	require.NotEmpty(t, ready.Status.LifecycleKey)
	require.NotEmpty(t, ready.Status.NetworkID)
	require.True(t, ready.Status.IPv6)
	require.False(t, ready.Status.CreatedAt.IsZero())
	physical := &apiv2.PhysicalContainerNetwork{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: network.Namespace, Name: ready.Status.PhysicalNetworkRef}, physical))
	require.True(t, metav1.IsControlledBy(physical, network))
	require.NotNil(t, physical.Spec.Network)
	require.False(t, physical.Spec.Network.RetainRuntimeNetwork)
	require.Equal(t, physical.Status.NetworkID, ready.Status.NetworkID)
	labels := runtimeNetworkLabels(t, ctx, ready.Status.NetworkName)
	require.Equal(t, "session", labels["app"])
	require.Equal(t, ready.Status.LifecycleKey, labels["com.microsoft.developer.usvc-dev.lifecycle-key"])
	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, network)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
	waitRuntimeNetworkMissing(t, ctx, ready.Status.NetworkID)
}

// Verifies that LogicalContainerNetwork overrides duplicate lifecycle labels, retains and reuses
// an equivalent persistent network, and delegates replacement when its configuration changes.
func TestV2LogicalContainerNetworkControllerReusesAndReplacesPersistentNetwork(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-network-persistent")
	runtimeName := namespace.Name + "-runtime"
	removeRuntimeNetworkOnCleanup(t, runtimeName)
	spec := apiv2.LogicalContainerNetworkSpec{
		Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: runtimeName,
		Network: &apiv2.LogicalContainerNetworkConfig{Labels: []commonapi.Label{
			{Key: "b", Value: "2"}, {Key: "a", Value: "1"},
			{Key: "com.microsoft.developer.usvc-dev.lifecycle-key", Value: "caller-first"},
			{Key: "com.microsoft.developer.usvc-dev.lifecycle-key", Value: "caller-last"},
		}},
	}
	first := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: namespace.Name}, Spec: *spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, first))
	created := waitObjectAssumesState(t, ctx, first.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	waitResourceLeaseReleased(t, ctx, first)
	labels := runtimeNetworkLabels(t, ctx, runtimeName)
	require.Equal(t, created.Status.LifecycleKey, labels["com.microsoft.developer.usvc-dev.lifecycle-key"])
	require.NoError(t, client.Delete(ctx, first))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, first)
	retained, retainedErr := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{created.Status.NetworkID}})
	require.NoError(t, retainedErr)
	require.Len(t, retained, 1)
	spec.Network.Labels[0], spec.Network.Labels[1] = spec.Network.Labels[1], spec.Network.Labels[0]
	second := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: namespace.Name}, Spec: *spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, second))
	reused := waitObjectAssumesState(t, ctx, second.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	require.Equal(t, created.Status.NetworkID, reused.Status.NetworkID)
	require.Equal(t, created.Status.LifecycleKey, reused.Status.LifecycleKey)
	physical := &apiv2.PhysicalContainerNetwork{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: reused.Status.PhysicalNetworkRef}, physical))
	require.Equal(t, reused.Status.NetworkID, physical.Spec.NetworkID)
	require.Nil(t, physical.Spec.Network)
	require.Equal(t, 1, containerOrchestrator.CreateNetworkCallCount(runtimeName))
	require.NoError(t, client.Delete(ctx, second))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, second)
	spec.Network.IPv6 = true
	third := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "third", Namespace: namespace.Name}, Spec: *spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, third))
	replaced := waitObjectAssumesState(t, ctx, third.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	require.NotEqual(t, created.Status.NetworkID, replaced.Status.NetworkID)
	require.NotEqual(t, created.Status.LifecycleKey, replaced.Status.LifecycleKey)
	require.True(t, replaced.Status.IPv6)
	require.Equal(t, 2, containerOrchestrator.CreateNetworkCallCount(runtimeName))
	waitRuntimeNetworkMissing(t, ctx, created.Status.NetworkID)
}

// Verifies that LogicalContainerNetwork records namespace workload ownership and releases
// its startup lease before reporting readiness, even without a global workload ID.
func TestV2LogicalContainerNetworkControllerRecordsNamespaceWorkload(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-network-ownership")
	require.NoError(t, retryOnConflict(ctx, namespace.NamespacedName(), func(updateCtx context.Context, current *apiv2.Namespace) error {
		current.Annotations = map[string]string{apiv2.NamespaceWorkloadIDAnnotation: " namespace-network-workload "}
		return client.Update(updateCtx, current)
	}))
	runtimeName := namespace.Name + "-runtime"
	removeRuntimeNetworkOnCleanup(t, runtimeName)
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerNetworkSpec{
			Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: runtimeName,
			Network: &apiv2.LogicalContainerNetworkConfig{},
		},
	}
	require.NoError(t, client.Create(ctx, network))
	ready := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	record, recordErr := testStateStore.GetPersistentNetwork(ctx, network.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, commonapi.WorkloadID("namespace-network-workload"), record.WorkloadID)
	require.Equal(t, ready.Status.NetworkID, record.NetworkID)
	require.ErrorIs(t, testStateStore.VerifyResourceLeaseHeld(ctx, network, testResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
}

type namespaceWorkloadFailureReader struct {
	ctrl_client.Reader
	namespaceName string
	fail          atomic.Bool
	failures      atomic.Uint32
}

func (reader *namespaceWorkloadFailureReader) Get(
	ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, opts ...ctrl_client.GetOption,
) error {
	if _, isNamespace := obj.(*apiv2.Namespace); isNamespace && key.Name == reader.namespaceName && reader.fail.Load() {
		reader.failures.Add(1)
		return errors.New("namespace workload lookup unavailable")
	}
	return reader.Reader.Get(ctx, key, obj, opts...)
}

// Verifies that LogicalContainerNetwork keeps its startup lease and physical backing while
// ownership recording fails, including on deletion, then settles ownership before child removal.
func TestV2LogicalContainerNetworkControllerRetriesNamespaceOwnershipFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	const namespaceName = "v2-logical-network-ownership-failure"
	var reader *namespaceWorkloadFailureReader
	serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerNetworkController|PhysicalContainerNetworkController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
			reader = &namespaceWorkloadFailureReader{Reader: apiReader, namespaceName: namespaceName}
			return reader
		}})
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: namespaceName, Annotations: map[string]string{apiv2.NamespaceWorkloadIDAnnotation: "recovered-network-workload"},
	}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	reader.fail.Store(true)
	defer reader.fail.Store(false)
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerNetworkSpec{
			Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: namespaceName + "-runtime",
			Network: &apiv2.LogicalContainerNetworkConfig{},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	failed := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "PersistenceFailed" && strings.Contains(ready.Message, "namespace workload lookup unavailable"), nil
	})
	require.Equal(t, apiv2.LogicalContainerNetworkPhaseUnknown, failed.Status.Phase)
	require.NotEmpty(t, failed.Status.NetworkID)
	systemLogs, systemLogErr := serverInfo.RestClient.Get().
		AbsPath("/apis", apiv2.GroupName, apiv2.GroupVersion.Version, "namespaces", namespace.Name, "logicalcontainernetworks", network.Name, apiv2.LogSubresourceName).
		Param("source", "system").Do(ctx).Raw()
	require.NoError(t, systemLogErr)
	require.Contains(t, string(systemLogs), "namespace workload lookup unavailable")
	require.Contains(t, string(systemLogs), "LogicalContainerNetworkReconciler")
	require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner))
	_, missingRecordErr := environmentInfo.StateStore.GetPersistentNetwork(ctx, network.GetLeaseKey())
	require.ErrorIs(t, missingRecordErr, statestore.ErrPersistentNetworkNotFound)
	firstFailures := reader.failures.Load()
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		return reader.failures.Load() > firstFailures, nil
	}))
	require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner))
	failuresBeforeDeletion := reader.failures.Load()
	require.NoError(t, serverInfo.Client.Delete(ctx, network))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return !current.DeletionTimestamp.IsZero() && ready != nil && ready.Reason == "PersistenceFailed", nil
	})
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		return reader.failures.Load() > failuresBeforeDeletion, nil
	}))
	physical := &apiv2.PhysicalContainerNetwork{}
	require.NoError(t, serverInfo.Client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: failed.Status.PhysicalNetworkRef}, physical))
	require.True(t, physical.DeletionTimestamp.IsZero())
	require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner))
	reader.fail.Store(false)
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, network)
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, physical)
	record, recordErr := environmentInfo.StateStore.GetPersistentNetwork(ctx, network.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, commonapi.WorkloadID("recovered-network-workload"), record.WorkloadID)
	require.Equal(t, failed.Status.NetworkID, record.NetworkID)
	require.ErrorIs(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
}

// Verifies that LogicalContainerNetwork deletion waits on inspection errors, but removes its
// backing and releases the lease without recording ownership once runtime removal is confirmed.
func TestV2LogicalContainerNetworkControllerSettlesMissingRuntimeDuringOwnershipFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	const namespaceName = "v2-logical-network-missing-ownership"
	var reader *namespaceWorkloadFailureReader
	serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerNetworkController|PhysicalContainerNetworkController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
			reader = &namespaceWorkloadFailureReader{Reader: apiReader, namespaceName: namespaceName}
			return reader
		}})
	require.NoError(t, startupErr)
	orchestrator, isTestOrchestrator := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTestOrchestrator)
	defer orchestrator.SetRuntimeHealth(true)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: namespaceName, Annotations: map[string]string{apiv2.NamespaceWorkloadIDAnnotation: "missing-network-workload"},
	}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	reader.fail.Store(true)
	defer reader.fail.Store(false)
	for _, testCase := range []struct {
		name     string
		deleting bool
	}{
		{name: "before-deletion"},
		{name: "during-deletion", deleting: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			network := &apiv2.LogicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: testCase.name, Namespace: namespace.Name},
				Spec: apiv2.LogicalContainerNetworkSpec{
					Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: namespaceName + "-" + testCase.name,
					Network: &apiv2.LogicalContainerNetworkConfig{},
				},
			}
			require.NoError(t, serverInfo.Client.Create(ctx, network))
			failed := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return ready != nil && ready.Reason == "PersistenceFailed", nil
			})
			require.NotEmpty(t, failed.Status.NetworkID)
			require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner))
			physical := &apiv2.PhysicalContainerNetwork{}
			require.NoError(t, serverInfo.Client.Get(ctx, types.NamespacedName{
				Namespace: namespace.Name, Name: failed.Status.PhysicalNetworkRef,
			}, physical))
			if testCase.deleting {
				require.NoError(t, serverInfo.Client.Delete(ctx, network))
				waitObjectAssumesStateEx(t, ctx, serverInfo.Client, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
					ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
					return !current.DeletionTimestamp.IsZero() && ready != nil && ready.Reason == "PersistenceFailed", nil
				})
			}
			orchestrator.SetRuntimeHealth(false)
			require.NoError(t, retryOnConflictEx(ctx, serverInfo.Client, physical.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainerNetwork) error {
				current.Annotations = map[string]string{"test-probe": "inspect-failure"}
				return serverInfo.Client.Update(updateCtx, current)
			}))
			inspectFailed := waitPhysicalContainerNetworkPhaseEx(t, ctx, serverInfo.Client, physical.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseUnknown)
			requireReadyCondition(t, inspectFailed.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonRuntimeNetworkInspectFailed)
			require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner))
			orchestrator.SetRuntimeHealth(true)
			_, removeErr := serverInfo.ContainerOrchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
				Networks: []string{failed.Status.NetworkID},
			})
			require.NoError(t, removeErr)
			require.NoError(t, retryOnConflictEx(ctx, serverInfo.Client, physical.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainerNetwork) error {
				current.Annotations = map[string]string{"test-probe": "missing"}
				return serverInfo.Client.Update(updateCtx, current)
			}))
			if !testCase.deleting {
				missing := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, physical.NamespacedName(), func(current *apiv2.PhysicalContainerNetwork) (bool, error) {
					ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
					return current.Status.Phase == apiv2.PhysicalContainerNetworkPhaseUnknown && ready != nil &&
						ready.Reason == string(apiv2.PhysicalContainerNetworkReasonRuntimeNetworkMissing), nil
				})
				require.Equal(t, failed.Status.NetworkID, missing.Status.NetworkID)
				require.NoError(t, serverInfo.Client.Delete(ctx, network))
			}
			ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, network)
			ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, physical)
			require.ErrorIs(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
			_, recordErr := environmentInfo.StateStore.GetPersistentNetwork(ctx, network.GetLeaseKey())
			require.ErrorIs(t, recordErr, statestore.ErrPersistentNetworkNotFound)
		})
	}
	require.NoError(t, serverInfo.Client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, namespace)
}

// Verifies that LogicalContainerNetwork adopts an unmanaged persistent runtime without
// replacing it or applying desired labels and IPv6 configuration.
func TestV2LogicalContainerNetworkControllerAdoptsUnmanagedNetwork(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-network-unmanaged")
	runtimeName := namespace.Name + "-runtime"
	runtimeID, createErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: runtimeName})
	require.NoError(t, createErr)
	removeRuntimeNetworkOnCleanup(t, runtimeName)
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerNetworkSpec{
			Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: runtimeName, LifecycleKey: "desired",
			Network: &apiv2.LogicalContainerNetworkConfig{IPv6: true, Labels: []commonapi.Label{{Key: "app", Value: "desired"}}},
		},
	}
	require.NoError(t, client.Create(ctx, network))
	adopted := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	require.Equal(t, runtimeID, adopted.Status.NetworkID)
	require.Empty(t, adopted.Status.LifecycleKey)
	require.False(t, adopted.Status.IPv6)
	require.NotContains(t, runtimeNetworkLabels(t, ctx, runtimeName), "app")
	require.Equal(t, 1, containerOrchestrator.CreateNetworkCallCount(runtimeName))
	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, network)
	retained, inspectErr := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{runtimeID}})
	require.NoError(t, inspectErr)
	require.Len(t, retained, 1)
}

// Verifies that LogicalContainerNetwork existing mode retries a missing runtime, tracks it
// when it appears, and never creates or removes the referenced runtime network.
func TestV2LogicalContainerNetworkControllerObservesExistingNetwork(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-network-existing")
	runtimeName := namespace.Name + "-runtime"
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{Mode: apiv2.LogicalContainerNetworkModeExisting, NetworkName: runtimeName},
	}
	require.NoError(t, client.Create(ctx, network))
	waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "ExistingNetworkNotFound", nil
	})
	require.Zero(t, containerOrchestrator.CreateNetworkCallCount(runtimeName))
	runtimeID, createErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: runtimeName})
	require.NoError(t, createErr)
	removeRuntimeNetworkOnCleanup(t, runtimeName)
	ready := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	require.Equal(t, runtimeID, ready.Status.NetworkID)
	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, network)
	retained, inspectErr := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{runtimeID}})
	require.NoError(t, inspectErr)
	require.Len(t, retained, 1)
	require.Equal(t, 1, containerOrchestrator.CreateNetworkCallCount(runtimeName))
}

// Verifies that LogicalContainer prepares its image while the logical network is pending,
// then forwards the published physical reference and aliases for physical-owned membership.
func TestV2LogicalContainerControllerRealizesNetworkAttachments(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-network-attachments")
	runtimeName := namespace.Name + "-runtime"
	unblockCreate := containerOrchestrator.BlockCreateNetwork(runtimeName)
	defer unblockCreate()
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{NetworkName: runtimeName, Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	require.NoError(t, client.Create(ctx, network))
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			ContainerName: namespace.Name + "-container",
			Container: &apiv2.LogicalContainerConfig{
				Image:    &apiv2.PhysicalContainerImageConfig{Image: "logical-network-attachments-image"},
				Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: namespace.Name + "/" + network.Name, Aliases: []string{"app"}}},
			},
		},
	}
	require.NoError(t, client.Create(ctx, container))
	pendingNetwork := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.PhysicalNetworkRef != "", nil
	})
	require.Equal(t, apiv2.LogicalContainerNetworkPhasePending, pendingNetwork.Status.Phase)
	requireReadyCondition(t, pendingNetwork.Status.Conditions, metav1.ConditionFalse, "PhysicalNetworkNotReady")
	pending := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "PhysicalResourcePreparationPending", nil
	})
	require.Empty(t, pending.Status.PhysicalContainerRef)
	require.Zero(t, containerOrchestrator.CreateContainerCallCount(container.Spec.ContainerName))
	image := waitPhysicalContainerImagePhase(t, ctx,
		types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(container.UID)},
		apiv2.PhysicalContainerImagePhaseReady)
	require.True(t, metav1.IsControlledBy(image, container))
	require.NotEmpty(t, image.Status.ImageID)
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(container.Spec.Container.Image.Image))
	require.Zero(t, containerOrchestrator.CreateContainerCallCount(container.Spec.ContainerName))
	unblockCreate()
	running := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	readyNetwork := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return slices.Contains(current.Status.ContainerIDs, running.Status.ContainerID), nil
	})
	require.Equal(t, pendingNetwork.Status.PhysicalNetworkRef, readyNetwork.Status.PhysicalNetworkRef)
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
	require.Equal(t, image.Name, physical.Spec.Container.ImageRef)
	require.Equal(t, []apiv2.ContainerNetworkConnectionConfig{{NetworkRef: readyNetwork.Status.PhysicalNetworkRef, Aliases: []string{"app"}}}, physical.Spec.Container.Networks)
	connections := &apiv2.PhysicalContainerNetworkConnectionList{}
	require.NoError(t, client.List(ctx, connections, ctrl_client.InNamespace(namespace.Name)))
	require.Len(t, connections.Items, 1)
	require.True(t, metav1.IsControlledBy(&connections.Items[0], physical))
	require.Equal(t, readyNetwork.Status.PhysicalNetworkRef, connections.Items[0].Spec.NetworkRef)
	inspected, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{running.Status.ContainerID}})
	require.NoError(t, inspectErr)
	require.Len(t, inspected, 1)
	require.Len(t, inspected[0].Networks, 1)
	require.Equal(t, readyNetwork.Status.NetworkID, inspected[0].Networks[0].Id)
	require.Equal(t, []string{"app"}, inspected[0].Networks[0].Aliases)
	waitRuntimeContainerNetworkMembership(t, ctx, readyNetwork.Status.NetworkID, running.Status.ContainerID, true)
	require.NoError(t, client.Delete(ctx, container))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, container)
	_, removedErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{running.Status.ContainerID}})
	require.ErrorIs(t, removedErr, containers.ErrNotFound)
	waitRuntimeContainerNetworkMembership(t, ctx, readyNetwork.Status.NetworkID, running.Status.ContainerID, false)
	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, network)
	waitRuntimeNetworkMissing(t, ctx, readyNetwork.Status.NetworkID)
}

// Verifies that LogicalContainer preserves adopted persistent container attachments even when
// its creation configuration references a logical network that has not been realized.
func TestV2LogicalContainerControllerPreservesAdoptedNetworkAttachments(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-network-adoption")
	runtimeName := namespace.Name + "-container"
	runtimeID := runExistingTestContainer(t, ctx, runtimeName, "logical-network-adopted-image")
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: runtimeName,
			Container: &apiv2.LogicalContainerConfig{ImageRef: "unused-image",
				Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "unrealized-network"}}},
		},
	}
	require.NoError(t, client.Create(ctx, container))
	running := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	require.Equal(t, runtimeID, running.Status.ContainerID)
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
	require.Equal(t, runtimeID, physical.Spec.ContainerID)
	require.Nil(t, physical.Spec.Container)
	var connections apiv2.PhysicalContainerNetworkConnectionList
	require.NoError(t, client.List(ctx, &connections, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, connections.Items)
}

type blockingLogicalNetworkInspector struct {
	containers.ContainerOrchestrator
	networkName string
	blocked     atomic.Bool
	entered     chan struct{}
	cancelled   chan struct{}
}

func (inspector *blockingLogicalNetworkInspector) InspectNetworks(
	ctx context.Context, options containers.InspectNetworksOptions,
) ([]containers.InspectedNetwork, error) {
	if len(options.Networks) == 1 && options.Networks[0] == inspector.networkName && inspector.blocked.CompareAndSwap(false, true) {
		inspector.entered <- struct{}{}
		<-ctx.Done()
		inspector.cancelled <- struct{}{}
		return nil, ctx.Err()
	}
	return inspector.ContainerOrchestrator.InspectNetworks(ctx, options)
}

// Verifies that LogicalContainerNetwork cancels an in-flight lookup and releases its startup
// lease without claiming a pre-existing runtime network before physical handoff.
func TestV2LogicalContainerNetworkControllerCancelsLookup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var inspector *blockingLogicalNetworkInspector
	serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerNetworkController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{WorkloadID: "canceled-network-workload", DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
			inspector = &blockingLogicalNetworkInspector{
				ContainerOrchestrator: orchestrator, networkName: "blocked-runtime",
				entered: make(chan struct{}, 1), cancelled: make(chan struct{}, 1),
			}
			return inspector
		}})
	require.NoError(t, startupErr)
	existingID, createErr := serverInfo.ContainerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: inspector.networkName})
	require.NoError(t, createErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-network-lookup"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	blocked := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "blocked", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerNetworkSpec{
			Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: inspector.networkName,
			Network: &apiv2.LogicalContainerNetworkConfig{},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, blocked))
	select {
	case _, open := <-inspector.entered:
		require.True(t, open)
	case <-ctx.Done():
		require.FailNow(t, "network lookup did not start")
	}
	var physicals apiv2.PhysicalContainerNetworkList
	require.NoError(t, serverInfo.Client.List(ctx, &physicals, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, physicals.Items)
	require.NoError(t, serverInfo.Client.Delete(ctx, blocked))
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, blocked)
	require.ErrorIs(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, blocked, environmentInfo.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
	_, recordErr := environmentInfo.StateStore.GetPersistentNetwork(ctx, blocked.GetLeaseKey())
	require.ErrorIs(t, recordErr, statestore.ErrPersistentNetworkNotFound)
	remaining, inspectErr := serverInfo.ContainerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{existingID}})
	require.NoError(t, inspectErr)
	require.Len(t, remaining, 1)
	require.Equal(t, existingID, remaining[0].Id)
	select {
	case _, open := <-inspector.cancelled:
		require.True(t, open)
	case <-ctx.Done():
		require.FailNow(t, "network lookup was not cancelled")
	}
}

// Verifies that LogicalContainerNetwork retains its finalizer, startup lease, and physical
// backing until retained creation and ownership settlement finish before physical deletion.
func TestV2LogicalContainerNetworkControllerSettlesPersistenceDuringDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-network-delete-settle")
	require.NoError(t, retryOnConflict(ctx, namespace.NamespacedName(), func(updateCtx context.Context, current *apiv2.Namespace) error {
		current.Annotations = map[string]string{apiv2.NamespaceWorkloadIDAnnotation: "logical-network-workload"}
		return client.Update(updateCtx, current)
	}))
	runtimeName := namespace.Name + "-runtime"
	removeRuntimeNetworkOnCleanup(t, runtimeName)
	unblockCreate := containerOrchestrator.BlockCreateNetwork(runtimeName)
	defer unblockCreate()
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: runtimeName, Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	require.NoError(t, client.Create(ctx, network))
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		return containerOrchestrator.CreateNetworkCallCount(runtimeName) == 1, nil
	}))
	require.NoError(t, testStateStore.VerifyResourceLeaseHeld(ctx, network, testResourceLeaseOwner))
	require.NoError(t, client.Delete(ctx, network))
	deleting := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "DeletingPhysicalNetwork", nil
	})
	physicalName := types.NamespacedName{Namespace: namespace.Name, Name: deleting.Status.PhysicalNetworkRef}
	physical := &apiv2.PhysicalContainerNetwork{}
	require.NoError(t, client.Get(ctx, physicalName, physical))
	require.True(t, physical.DeletionTimestamp.IsZero())
	require.NotEmpty(t, physical.Finalizers)
	require.NoError(t, client.Get(ctx, network.NamespacedName(), deleting))
	require.Contains(t, deleting.Finalizers, apiv2.GroupName+"/logicalcontainernetwork-reconciler")
	require.NoError(t, testStateStore.VerifyResourceLeaseHeld(ctx, network, testResourceLeaseOwner))
	_, missingRecordErr := testStateStore.GetPersistentNetwork(ctx, network.GetLeaseKey())
	require.ErrorIs(t, missingRecordErr, statestore.ErrPersistentNetworkNotFound)
	unblockCreate()
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, network)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
	record, recordErr := testStateStore.GetPersistentNetwork(ctx, network.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, commonapi.WorkloadID("logical-network-workload"), record.WorkloadID)
	require.Equal(t, runtimeName, record.NetworkName)
	require.ErrorIs(t, testStateStore.VerifyResourceLeaseHeld(ctx, network, testResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
	retained, inspectErr := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{record.NetworkID}})
	require.NoError(t, inspectErr)
	require.Len(t, retained, 1)
	require.Equal(t, 1, containerOrchestrator.CreateNetworkCallCount(runtimeName))
}

type delayedPhysicalNetworkCreator struct {
	ctrl_client.Client
	entered chan struct{}
	resume  chan struct{}
}

func (creator *delayedPhysicalNetworkCreator) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	if _, isPhysicalNetwork := object.(*apiv2.PhysicalContainerNetwork); !isPhysicalNetwork {
		return creator.Client.Create(ctx, object, options...)
	}
	createErr := creator.Client.Create(ctx, object, options...)
	if createErr != nil {
		return createErr
	}
	select {
	case creator.entered <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case _, open := <-creator.resume:
		if open {
			return errors.New("unexpected value on physical network creation gate")
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return errors.New("physical network create response lost")
}

func (creator *delayedPhysicalNetworkCreator) Get(
	ctx context.Context, key ctrl_client.ObjectKey, object ctrl_client.Object, options ...ctrl_client.GetOption,
) error {
	if _, isPhysicalNetwork := object.(*apiv2.PhysicalContainerNetwork); isPhysicalNetwork {
		select {
		case _, open := <-creator.resume:
			if open {
				return errors.New("unexpected value on physical network creation gate")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return creator.Client.Get(ctx, key, object, options...)
}

// Verifies that LogicalContainerNetwork settles a committed physical handoff with a lost response
// when namespace termination prevents runtime startup, without claiming a runtime or stalling cleanup.
func TestV2LogicalContainerNetworkControllerDeletesUnstartedPhysicalDuringNamespaceCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var creator *delayedPhysicalNetworkCreator
	serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerNetworkController|PhysicalContainerNetworkController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{
			WorkloadID: "unstarted-network-workload",
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				creator = &delayedPhysicalNetworkCreator{Client: apiClient, entered: make(chan struct{}, 1), resume: make(chan struct{})}
				return creator
			},
		})
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-network-unstarted-cleanup"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerNetworkSpec{
			Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: namespace.Name + "-runtime",
			Network: &apiv2.LogicalContainerNetworkConfig{},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	select {
	case _, open := <-creator.entered:
		require.True(t, open)
	case <-ctx.Done():
		require.FailNow(t, "physical network creation did not start")
	}
	physical := &apiv2.PhysicalContainerNetwork{}
	require.NoError(t, serverInfo.Client.Get(ctx, types.NamespacedName{
		Namespace: namespace.Name, Name: "logical-" + string(network.UID),
	}, physical))
	require.True(t, metav1.IsControlledBy(physical, network))
	require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner))
	require.NoError(t, serverInfo.Client.Delete(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, creator.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseTerminating, nil
	})
	close(creator.resume)
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, namespace)
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, network)
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, physical)
	_, inspectErr := serverInfo.ContainerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{network.Spec.NetworkName}})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
	require.ErrorIs(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
	_, recordErr := environmentInfo.StateStore.GetPersistentNetwork(ctx, network.GetLeaseKey())
	require.ErrorIs(t, recordErr, statestore.ErrPersistentNetworkNotFound)
}

// Verifies that LogicalContainerNetwork serializes same-process startups sharing a runtime
// name, retains the first startup's lease, and reuses its network after ownership settlement.
func TestV2LogicalContainerNetworkControllerSerializesSameProcessStartups(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-network-lease")
	require.NoError(t, retryOnConflict(ctx, namespace.NamespacedName(), func(updateCtx context.Context, current *apiv2.Namespace) error {
		current.Annotations = map[string]string{apiv2.NamespaceWorkloadIDAnnotation: "shared-network-workload"}
		return client.Update(updateCtx, current)
	}))
	runtimeName := namespace.Name + "-runtime"
	removeRuntimeNetworkOnCleanup(t, runtimeName)
	releaseCreate := containerOrchestrator.BlockCreateNetwork(runtimeName)
	defer releaseCreate()
	v1Network := &apiv1.ContainerNetwork{Spec: apiv1.ContainerNetworkSpec{NetworkName: runtimeName}}
	first := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: runtimeName, Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	require.NoError(t, client.Create(ctx, first))
	waitCreateNetworkCallCount(t, ctx, runtimeName, 1)
	require.NoError(t, testStateStore.VerifyResourceLeaseHeld(ctx, v1Network, testResourceLeaseOwner))
	second := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: namespace.Name},
		Spec:       *first.Spec.DeepCopy(),
	}
	require.NoError(t, client.Create(ctx, second))
	pending := waitObjectAssumesState(t, ctx, second.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && (ready.Reason == "ResourceLeaseHeld" || current.Status.PhysicalNetworkRef != ""), nil
	})
	requireReadyCondition(t, pending.Status.Conditions, metav1.ConditionFalse, "ResourceLeaseHeld")
	require.Empty(t, pending.Status.PhysicalNetworkRef)
	require.Equal(t, 1, containerOrchestrator.CreateNetworkCallCount(runtimeName))
	require.NoError(t, testStateStore.VerifyResourceLeaseHeld(ctx, first, testResourceLeaseOwner))
	releaseCreate()
	firstReady := waitObjectAssumesState(t, ctx, first.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	secondReady := waitObjectAssumesState(t, ctx, second.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	require.NotEmpty(t, firstReady.Status.NetworkID)
	require.Equal(t, firstReady.Status.NetworkID, secondReady.Status.NetworkID)
	require.NotEqual(t, firstReady.Status.PhysicalNetworkRef, secondReady.Status.PhysicalNetworkRef)
	require.Equal(t, 1, containerOrchestrator.CreateNetworkCallCount(runtimeName))
	record, recordErr := testStateStore.GetPersistentNetwork(ctx, v1Network.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, firstReady.Status.NetworkID, record.NetworkID)
	require.Equal(t, commonapi.WorkloadID("shared-network-workload"), record.WorkloadID)
	require.ErrorIs(t, testStateStore.VerifyResourceLeaseHeld(ctx, second, testResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
}

type observedV1NetworkClient struct {
	ctrl_client.Client
	networkName string
	reads       atomic.Uint32
}

func (observer *observedV1NetworkClient) Get(
	ctx context.Context, key ctrl_client.ObjectKey, object ctrl_client.Object, options ...ctrl_client.GetOption,
) error {
	getErr := observer.Client.Get(ctx, key, object, options...)
	if _, isNetwork := object.(*apiv1.ContainerNetwork); isNetwork && key.Name == observer.networkName && getErr == nil {
		observer.reads.Add(1)
	}
	return getErr
}

// Verifies that a V1 persistent network waits for a same-process V2 creation lease, then shares
// the created runtime network without releasing the pending startup's lease or workload record.
func TestV2LogicalContainerNetworkControllerSerializesV1Startup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	const runtimeName = "v2-logical-network-v1-lease-runtime"
	var observer *observedV1NetworkClient
	var orchestrator *ctrl_testutil.TestContainerOrchestrator
	serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		NetworkController|LogicalContainerNetworkController|PhysicalContainerNetworkController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{
			WorkloadID: "mixed-network-workload",
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				observer = &observedV1NetworkClient{Client: apiClient, networkName: "v1-network"}
				return observer
			},
			DecorateContainerOrchestrator: func(runtime containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
				orchestrator = runtime.(*ctrl_testutil.TestContainerOrchestrator)
				return runtime
			},
		})
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-network-v1-lease"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	releaseCreate := orchestrator.BlockCreateNetwork(runtimeName)
	defer releaseCreate()
	logical := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerNetworkSpec{
			Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: runtimeName,
			Network: &apiv2.LogicalContainerNetworkConfig{},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, logical))
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		return orchestrator.CreateNetworkCallCount(runtimeName) == 1, nil
	}))
	v1Network := &apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: observer.networkName},
		Spec:       apiv1.ContainerNetworkSpec{Mode: apiv1.ContainerNetworkModePersistent, NetworkName: runtimeName},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, v1Network))
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		// A retry after the finalizer and initial startup reconciliations proves V1 attempted acquisition.
		return observer.reads.Load() >= 3 || orchestrator.CreateNetworkCallCount(runtimeName) > 1, nil
	}))
	require.Equal(t, 1, orchestrator.CreateNetworkCallCount(runtimeName))
	require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, logical, environmentInfo.ResourceLeaseOwner))
	releaseCreate()
	ready := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	running := ensureNetworkCreatedEx(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, v1Network)
	require.Equal(t, ready.Status.NetworkID, running.Status.ID)
	require.Equal(t, 1, orchestrator.CreateNetworkCallCount(runtimeName))
	record, recordErr := environmentInfo.StateStore.GetPersistentNetwork(ctx, logical.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, ready.Status.NetworkID, record.NetworkID)
	require.Equal(t, commonapi.WorkloadID("mixed-network-workload"), record.WorkloadID)
	require.ErrorIs(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, logical, environmentInfo.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
	require.NoError(t, serverInfo.Client.Delete(ctx, v1Network))
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, v1Network)
	retainedRecord, retainedErr := environmentInfo.StateStore.GetPersistentNetwork(ctx, logical.GetLeaseKey())
	require.NoError(t, retainedErr)
	require.Equal(t, record, retainedRecord)
}

// Verifies that LogicalContainerNetwork exposes physical creation failure conditions
// and preserves the unrelated runtime network that caused the name collision.
func TestV2LogicalContainerNetworkControllerReportsPhysicalCreationFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-network-failure")
	runtimeName := namespace.Name + "-runtime"
	removeRuntimeNetworkOnCleanup(t, runtimeName)
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{NetworkName: runtimeName, Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	existingID, collisionErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: runtimeName})
	require.NoError(t, collisionErr)
	require.NoError(t, client.Create(ctx, network))
	failed := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseFailed, nil
	})
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, "PhysicalNetworkFailed")
	require.Contains(t, apimeta.FindStatusCondition(failed.Status.Conditions, string(apiv2.ConditionReady)).Message, "CreateFailed")
	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, network)
	foreign, foreignErr := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{runtimeName}})
	require.NoError(t, foreignErr)
	require.Len(t, foreign, 1)
	require.Equal(t, existingID, foreign[0].Id)
}

// Verifies that namespace cleanup waits for the logical and physical network finalizers
// and removes a session runtime network without relying on namespace readiness during deletion.
func TestV2LogicalContainerNetworkControllerCleansUpNamespace(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-network-namespace-cleanup")
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	require.NoError(t, client.Create(ctx, network))
	ready := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, network)
	waitRuntimeNetworkMissing(t, ctx, ready.Status.NetworkID)
}
