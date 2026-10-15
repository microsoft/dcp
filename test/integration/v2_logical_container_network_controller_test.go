/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// Verifies that LogicalContainerNetwork retains a persistent network, reuses a computed key
// across label ordering while preserving duplicate precedence, and replaces changed configuration.
func TestV2LogicalContainerNetworkControllerReusesAndReplacesPersistentNetwork(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-network-persistent")
	runtimeName := namespace.Name + "-runtime"
	removeRuntimeNetworkOnCleanup(t, runtimeName)
	spec := apiv2.LogicalContainerNetworkSpec{
		Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: runtimeName,
		Network: &apiv2.LogicalContainerNetworkConfig{Labels: []commonapi.Label{{Key: "b", Value: "2"}, {Key: "a", Value: "1"}}},
	}
	for labelIndex := range 32 {
		spec.Network.Labels = append(spec.Network.Labels,
			commonapi.Label{Key: "shared", Value: fmt.Sprintf("value-%d", labelIndex)},
			commonapi.Label{Key: fmt.Sprintf("other-%02d", 31-labelIndex), Value: "other"},
		)
	}
	first := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: namespace.Name}, Spec: *spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, first))
	created := waitObjectAssumesState(t, ctx, first.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	waitResourceLeaseReleased(t, ctx, first)
	require.NoError(t, client.Delete(ctx, first))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, first)
	retained, retainedErr := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{created.Status.NetworkID}})
	require.NoError(t, retainedErr)
	require.Len(t, retained, 1)
	require.Equal(t, "value-31", retained[0].Labels["shared"])
	slices.SortStableFunc(spec.Network.Labels, func(left, right commonapi.Label) int {
		return strings.Compare(left.Key, right.Key)
	})
	second := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: namespace.Name}, Spec: *spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, second))
	reused := waitObjectAssumesState(t, ctx, second.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	require.Equal(t, created.Status.NetworkID, reused.Status.NetworkID)
	require.Equal(t, created.Status.LifecycleKey, reused.Status.LifecycleKey)
	require.Equal(t, "value-31", runtimeNetworkLabels(t, ctx, runtimeName)["shared"])
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

// Verifies that LogicalContainerNetwork persists namespace workload overrides, falls back to
// the global workload, and releases its lease without a record when neither supplies ownership.
func TestV2LogicalContainerNetworkControllerResolvesNamespaceOwnership(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name       string
		globalID   commonapi.WorkloadID
		annotation string
		expectedID commonapi.WorkloadID
	}{
		{name: "override", globalID: "global-network-workload", annotation: " namespace-network-workload ", expectedID: "namespace-network-workload"},
		{name: "empty-default", annotation: "namespace-network-workload", expectedID: "namespace-network-workload"},
		{name: "fallback", globalID: "global-network-workload", expectedID: "global-network-workload"},
		{name: "unowned"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
				LogicalContainerNetworkController|PhysicalContainerNetworkController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{WorkloadID: testCase.globalID})
			require.NoError(t, startupErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-network-ownership"}}
			if testCase.annotation != "" {
				namespace.Annotations = map[string]string{apiv2.NamespaceWorkloadIDAnnotation: testCase.annotation}
			}
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
			ready := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
				return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
			})
			record, recordErr := environmentInfo.StateStore.GetPersistentNetwork(ctx, network.GetLeaseKey())
			if testCase.expectedID == "" {
				require.ErrorIs(t, recordErr, statestore.ErrPersistentNetworkNotFound)
			} else {
				require.NoError(t, recordErr)
				require.Equal(t, testCase.expectedID, record.WorkloadID)
				require.Equal(t, ready.Status.NetworkID, record.NetworkID)
			}
			require.ErrorIs(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
		})
	}
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

// Verifies that LogicalContainerNetwork keeps its startup lease and exposes retrying ownership
// failures until a real namespace lookup succeeds and its persistent workload record is written.
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
	require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner))
	_, missingRecordErr := environmentInfo.StateStore.GetPersistentNetwork(ctx, network.GetLeaseKey())
	require.ErrorIs(t, missingRecordErr, statestore.ErrPersistentNetworkNotFound)
	firstFailures := reader.failures.Load()
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		return reader.failures.Load() > firstFailures, nil
	}))
	require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner))
	reader.fail.Store(false)
	ready := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	record, recordErr := environmentInfo.StateStore.GetPersistentNetwork(ctx, network.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, commonapi.WorkloadID("recovered-network-workload"), record.WorkloadID)
	require.Equal(t, ready.Status.NetworkID, record.NetworkID)
	require.ErrorIs(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
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
	require.NoError(t, retryOnConflict(ctx, network.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainerNetwork) error {
		current.Annotations = map[string]string{"runtime-created": "true"}
		return client.Update(updateCtx, current)
	}))
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

// Verifies that LogicalContainer resolves logical networks only after runtime readiness,
// forwards physical references and aliases, and uses the physical-owned membership machinery.
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
	waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.PhysicalNetworkRef != "", nil
	})
	pending := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "PhysicalResourcePreparationPending", nil
	})
	require.Empty(t, pending.Status.PhysicalContainerRef)
	require.Zero(t, containerOrchestrator.CreateContainerCallCount(container.Spec.ContainerName))
	unblockCreate()
	running := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	readyNetwork := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return slices.Contains(current.Status.ContainerIDs, running.Status.ContainerID), nil
	})
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
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

// Verifies that LogicalContainerNetwork performs name lookup asynchronously, allows other
// networks to progress, and cancels lookup and releases its startup lease on deletion.
func TestV2LogicalContainerNetworkControllerCancelsLookup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var inspector *blockingLogicalNetworkInspector
	serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerNetworkController|PhysicalContainerNetworkController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
			inspector = &blockingLogicalNetworkInspector{
				ContainerOrchestrator: orchestrator, networkName: "blocked-runtime",
				entered: make(chan struct{}, 1), cancelled: make(chan struct{}, 1),
			}
			return inspector
		}})
	require.NoError(t, startupErr)
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
	unrelated := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, unrelated))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, unrelated.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	require.NoError(t, serverInfo.Client.Delete(ctx, blocked))
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, blocked)
	require.ErrorIs(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, blocked, environmentInfo.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
	select {
	case _, open := <-inspector.cancelled:
		require.True(t, open)
	case <-ctx.Done():
		require.FailNow(t, "network lookup was not cancelled")
	}
}

// Verifies that LogicalContainerNetwork records retained workload ownership before releasing
// its startup lease, even when deletion arrives during physical network creation.
func TestV2LogicalContainerNetworkControllerSettlesPersistenceDuringDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerNetworkController|PhysicalContainerNetworkController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{WorkloadID: "logical-network-workload"})
	require.NoError(t, startupErr)
	orchestrator := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-network-delete-settle"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	runtimeName := namespace.Name + "-runtime"
	unblockCreate := orchestrator.BlockCreateNetwork(runtimeName)
	defer unblockCreate()
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: runtimeName, Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		return orchestrator.CreateNetworkCallCount(runtimeName) == 1, nil
	}))
	require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner))
	require.NoError(t, serverInfo.Client.Delete(ctx, network))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "DeletingPhysicalNetwork", nil
	})
	unblockCreate()
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, network)
	record, recordErr := environmentInfo.StateStore.GetPersistentNetwork(ctx, network.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, commonapi.WorkloadID("logical-network-workload"), record.WorkloadID)
	require.Equal(t, runtimeName, record.NetworkName)
	require.ErrorIs(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
	retained, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{record.NetworkID}})
	require.NoError(t, inspectErr)
	require.Len(t, retained, 1)
	require.Equal(t, 1, orchestrator.CreateNetworkCallCount(runtimeName))
}

// Verifies that LogicalContainerNetwork uses the V1 runtime-name lease, reports contention,
// and proceeds to persist workload ownership only after that startup lease becomes available.
func TestV2LogicalContainerNetworkControllerWaitsForSharedLease(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerNetworkController|PhysicalContainerNetworkController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{WorkloadID: "shared-network-workload"})
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-network-lease"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	runtimeName := namespace.Name + "-runtime"
	v1Network := &apiv1.ContainerNetwork{Spec: apiv1.ContainerNetworkSpec{NetworkName: runtimeName}}
	competingOwner := environmentInfo.ResourceLeaseOwner
	competingOwner.IdentityTime = competingOwner.IdentityTime.Add(time.Second)
	lease, leaseErr := environmentInfo.StateStore.AcquireResourceLease(ctx, v1Network, competingOwner, time.Minute)
	require.NoError(t, leaseErr)
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{Mode: apiv2.LogicalContainerNetworkModePersistent, NetworkName: runtimeName, Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	pending := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "ResourceLeaseHeld", nil
	})
	require.Empty(t, pending.Status.PhysicalNetworkRef)
	orchestrator := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.Zero(t, orchestrator.CreateNetworkCallCount(runtimeName))
	require.NoError(t, lease.Release(ctx))
	ready := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	record, recordErr := environmentInfo.StateStore.GetPersistentNetwork(ctx, v1Network.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, ready.Status.NetworkID, record.NetworkID)
	require.Equal(t, commonapi.WorkloadID("shared-network-workload"), record.WorkloadID)
	require.ErrorIs(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, network, environmentInfo.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
}

// Verifies that LogicalContainerNetwork exposes physical creation failures and retains
// its finalizer while the physical controller retries runtime deletion failures.
func TestV2LogicalContainerNetworkControllerReportsPhysicalFailures(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	serverInfo, _, startupErr := StartTestEnvironment(t, ctx,
		LogicalContainerNetworkController|PhysicalContainerNetworkController, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	orchestrator := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-network-failure"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{NetworkName: "collision-runtime", Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	_, collisionErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: network.Spec.NetworkName})
	require.NoError(t, collisionErr)
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	failed := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseFailed, nil
	})
	require.Contains(t, apimeta.FindStatusCondition(failed.Status.Conditions, string(apiv2.ConditionReady)).Message, "CreateFailed")
	require.NoError(t, serverInfo.Client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, network)
	foreign, foreignErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{network.Spec.NetworkName}})
	require.NoError(t, foreignErr)
	require.Len(t, foreign, 1)

	session := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{NetworkName: "delete-failure-runtime", Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, session))
	readySession := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, session.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	orchestrator.SetRuntimeHealth(false)
	require.NoError(t, serverInfo.Client.Delete(ctx, session))
	physicalName := types.NamespacedName{Namespace: namespace.Name, Name: readySession.Status.PhysicalNetworkRef}
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, physicalName, func(current *apiv2.PhysicalContainerNetwork) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "RuntimeNetworkRemoveFailed", nil
	})
	deleting := &apiv2.LogicalContainerNetwork{}
	require.NoError(t, serverInfo.Client.Get(ctx, session.NamespacedName(), deleting))
	require.NotEmpty(t, deleting.Finalizers)
	orchestrator.SetRuntimeHealth(true)
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, session)
	_, missingErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{readySession.Status.NetworkID}})
	require.ErrorIs(t, missingErr, containers.ErrNotFound)
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
