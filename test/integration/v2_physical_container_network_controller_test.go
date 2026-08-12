/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestV2PhysicalContainerNetworkControllerCreatesNetwork(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcn-create")
	networkName := "v2-pcn-created-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "created-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName: networkName,
			Labels: []commonapi.Label{
				{Key: "test-label", Value: "test-value"},
			},
		},
	}
	require.NoError(t, client.Create(ctx, network))

	updatedNetwork := waitPhysicalContainerNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)
	require.Contains(t, updatedNetwork.Finalizers, apiv2.GroupName+"/physicalcontainernetwork-reconciler")
	require.NotEmpty(t, updatedNetwork.Status.NetworkID)
	require.Equal(t, networkName, updatedNetwork.Status.NetworkName)
	require.Equal(t, "bridge", updatedNetwork.Status.Driver)
	require.False(t, updatedNetwork.Status.CreatedAt.IsZero())
	requireReadyCondition(t, updatedNetwork.Status.Conditions, metav1.ConditionTrue, apiv2.PhysicalContainerNetworkReasonNetworkReady)

	inspectedNetworks, inspectErr := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: []string{updatedNetwork.Status.NetworkID},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedNetworks, 1)
	require.Equal(t, networkName, inspectedNetworks[0].Name)

	// Creator labels let startup harvesting reclaim networks abandoned by a crashed DCP process.
	labels := runtimeNetworkLabels(t, ctx, networkName)
	require.Equal(t, "test-value", labels["test-label"])
	require.Equal(t, "false", labels[controllers.PersistentLabel])
	require.NotEmpty(t, labels[controllers.CreatorProcessIdLabel])
	require.NotEmpty(t, labels[controllers.CreatorProcessStartTimeLabel])
}

func TestV2PhysicalContainerNetworkControllerTracksExistingNetwork(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcn-track")
	networkName := "v2-pcn-tracked-runtime"
	networkID, createErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: networkName})
	require.NoError(t, createErr)
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tracked-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkID:          networkID,
			PreserveOnDeletion: true,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	updatedNetwork := waitPhysicalContainerNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)
	require.Equal(t, networkID, updatedNetwork.Status.NetworkID)
	require.Equal(t, networkName, updatedNetwork.Status.NetworkName)

	// Tracking must not create anything: the only create is the one this test performed.
	require.Equal(t, 1, containerOrchestrator.CreateNetworkCallCount(networkName))
}

func TestV2PhysicalContainerNetworkControllerRemovesCreatedNetworkOnDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcn-delete")
	networkName := "v2-pcn-deleted-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "deleted-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName: networkName,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	updatedNetwork := waitPhysicalContainerNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)
	networkID := updatedNetwork.Status.NetworkID

	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerNetwork](t, ctx, client, network)
	waitRuntimeNetworkMissing(t, ctx, networkID)
}

func TestV2PhysicalContainerNetworkControllerPreservesCreatedNetworkOnDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcn-retain")
	networkName := "v2-pcn-retained-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "retained-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName:        networkName,
			PreserveOnDeletion: true,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	updatedNetwork := waitPhysicalContainerNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)
	networkID := updatedNetwork.Status.NetworkID
	require.Equal(t, "true", runtimeNetworkLabels(t, ctx, networkName)[controllers.PersistentLabel])

	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerNetwork](t, ctx, client, network)

	inspectedNetworks, inspectErr := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: []string{networkID},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedNetworks, 1)
}

// A tracked network is removed on deletion unless preserveOnDeletion is set, matching PhysicalContainer.
func TestV2PhysicalContainerNetworkControllerRemovesTrackedNetworkOnDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcn-track-delete")
	networkName := "v2-pcn-tracked-deleted-runtime"
	networkID, createErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: networkName})
	require.NoError(t, createErr)
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tracked-deleted-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkID: networkID,
		},
	}
	require.NoError(t, client.Create(ctx, network))
	waitPhysicalContainerNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)

	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerNetwork](t, ctx, client, network)
	waitRuntimeNetworkMissing(t, ctx, networkID)
}

func TestV2PhysicalContainerNetworkControllerCleansUpOnNamespaceDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "v2-pcn-ns-cleanup",
		},
	}
	require.NoError(t, client.Create(ctx, namespace))
	waitV2NamespaceActive(t, ctx, namespace.Name)

	networkName := "v2-pcn-ns-cleanup-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)
	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "namespace-cleanup-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName: networkName,
		},
	}
	require.NoError(t, client.Create(ctx, network))
	readyNetwork := waitPhysicalContainerNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)
	networkID := readyNetwork.Status.NetworkID

	require.NoError(t, client.Delete(ctx, namespace))

	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerNetwork](t, ctx, client, network)
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
	waitRuntimeNetworkMissing(t, ctx, networkID)
}

func TestV2PhysicalContainerNetworkControllerDoesNotDuplicateCreate(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcn-single-create")
	networkName := "v2-pcn-single-create-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)
	releaseCreate := containerOrchestrator.BlockCreateNetwork(networkName)
	defer releaseCreate()

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "single-create-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName: networkName,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	waitCreateNetworkCallCount(t, ctx, networkName, 1)
	pendingNetwork := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(currentNetwork *apiv2.PhysicalContainerNetwork) (bool, error) {
		return currentNetwork.Status.Phase == apiv2.PhysicalContainerNetworkPhasePending, nil
	})
	requireReadyCondition(t, pendingNetwork.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonCreating)

	// Reconciliations while the create is in flight must not start a second one.
	require.Never(t, func() bool {
		return containerOrchestrator.CreateNetworkCallCount(networkName) > 1
	}, 3*time.Second, 250*time.Millisecond)

	releaseCreate()
	waitPhysicalContainerNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)
	require.Equal(t, 1, containerOrchestrator.CreateNetworkCallCount(networkName))
}

func TestV2PhysicalContainerNetworkControllerReportsTerminalCreateFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcn-create-fail")
	networkName := "v2-pcn-conflicting-runtime"
	_, createErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: networkName})
	require.NoError(t, createErr)
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflicting-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName:        networkName,
			PreserveOnDeletion: true,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	failedNetwork := waitPhysicalContainerNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseFailed)
	requireReadyCondition(t, failedNetwork.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonCreateFailed)

	// The spec is immutable, so a create failure is terminal and must not be retried.
	require.Never(t, func() bool {
		return containerOrchestrator.CreateNetworkCallCount(networkName) > 2
	}, 3*time.Second, 250*time.Millisecond)
}

// Steady-state polling must be paced by the monitoring delay and must not write an unchanged
// status, because a status write feeds a watch event back into the controller and turns the slow
// polling cadence into a tight re-inspect loop.
func TestV2PhysicalContainerNetworkControllerDoesNotChurnReadyStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcn-steady")
	networkName := "v2-pcn-steady-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "steady-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName: networkName,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	readyNetwork := waitPhysicalContainerNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)
	readyResourceVersion := readyNetwork.ResourceVersion
	// The status write that announced Ready drives exactly one more reconciliation, which
	// re-inspects and settles. Anything beyond that within the monitoring delay is churn.
	settledInspectCount := containerOrchestrator.InspectNetworkCallCount(readyNetwork.Status.NetworkID) + 1

	require.Never(t, func() bool {
		currentNetwork := &apiv2.PhysicalContainerNetwork{}
		if getErr := client.Get(ctx, network.NamespacedName(), currentNetwork); getErr != nil {
			return false
		}
		if currentNetwork.ResourceVersion != readyResourceVersion {
			return true
		}
		return containerOrchestrator.InspectNetworkCallCount(readyNetwork.Status.NetworkID) > settledInspectCount
	}, 5*time.Second, 250*time.Millisecond)
}

func TestV2PhysicalContainerNetworkControllerRecoversFromRuntimeFailure(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)

	// We are going to use a separate instance of the API server because we need to simulate the
	// container runtime being unhealthy, and that would interfere with other tests if we used the
	// shared container orchestrator.
	serverInfo, _, startupErr := StartTestEnvironment(ctx, NamespaceController|PhysicalContainerNetworkController, t.Name(), NoSeparateWorkingDir)
	require.NoError(t, startupErr, "Failed to start the API server")

	defer func() {
		cancel()

		// Wait for the API server cleanup to complete.
		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-time.After(5 * time.Second):
		}
	}()

	tco, isTCO := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTCO, "Container orchestrator should be a TestContainerOrchestrator")

	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-pcn-recovery"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, types.NamespacedName{Name: namespace.Name}, func(updated *apiv2.Namespace) (bool, error) {
		return updated.Status.Phase == apiv2.NamespacePhaseActive, nil
	})

	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "recovering-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName: "v2-pcn-recovery-runtime",
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, network))

	waitPhysicalContainerNetworkPhaseEx(t, ctx, serverInfo.Client, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)

	t.Logf("Setting container runtime to unhealthy...")
	tco.SetRuntimeHealth(false)

	// Annotating the network forces a prompt re-inspection instead of waiting out the monitoring
	// delay. Only the spec is immutable, so annotating an existing network is allowed.
	failedNetwork := &apiv2.PhysicalContainerNetwork{}
	require.NoError(t, serverInfo.Client.Get(ctx, network.NamespacedName(), failedNetwork))
	failedNetwork.Annotations = map[string]string{"test-probe": "1"}
	require.NoError(t, serverInfo.Client.Update(ctx, failedNetwork))

	t.Logf("Ensure that the PhysicalContainerNetwork reports the runtime failure...")
	failedNetwork = waitPhysicalContainerNetworkPhaseEx(t, ctx, serverInfo.Client, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseFailed)
	requireReadyCondition(t, failedNetwork.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonReconciliationFailed)

	// Repeating an identical failure produces no status change, so nothing but a self-scheduled
	// retry can wake the network. Waiting for further inspections proves the retry loop is alive;
	// restoring the runtime before this point would let an already in-flight reconciliation
	// recover the network whether or not the controller retries on its own.
	t.Logf("Ensure that the PhysicalContainerNetwork keeps retrying while the runtime is unhealthy...")
	networkID := failedNetwork.Status.NetworkID
	waitInspectNetworkCallCount(t, ctx, tco, networkID, tco.InspectNetworkCallCount(networkID)+2)

	t.Logf("Setting container runtime to healthy...")
	tco.SetRuntimeHealth(true)

	// Recovery must happen on its own. Nothing touches the network from here on, so the only way
	// back to Ready is the controller retrying the inspection it previously failed.
	t.Logf("Ensure that the PhysicalContainerNetwork recovers without further changes...")
	recoveredNetwork := waitPhysicalContainerNetworkPhaseEx(t, ctx, serverInfo.Client, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)
	requireReadyCondition(t, recoveredNetwork.Status.Conditions, metav1.ConditionTrue, apiv2.PhysicalContainerNetworkReasonNetworkReady)
}

func TestV2PhysicalContainerNetworkControllerRecoversFromCreateFailure(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)

	serverInfo, _, startupErr := StartTestEnvironment(ctx, NamespaceController|PhysicalContainerNetworkController, t.Name(), NoSeparateWorkingDir)
	require.NoError(t, startupErr, "Failed to start the API server")

	defer func() {
		cancel()

		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-time.After(5 * time.Second):
		}
	}()

	tco, isTCO := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTCO, "Container orchestrator should be a TestContainerOrchestrator")

	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-pcn-create-recovery"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, types.NamespacedName{Name: namespace.Name}, func(updated *apiv2.Namespace) (bool, error) {
		return updated.Status.Phase == apiv2.NamespacePhaseActive, nil
	})

	tco.SetRuntimeHealth(false)
	networkName := "v2-pcn-create-recovery-runtime"
	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "create-recovery-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName: networkName,
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, network))

	failedNetwork := waitPhysicalContainerNetworkPhaseEx(t, ctx, serverInfo.Client, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseFailed)
	requireReadyCondition(t, failedNetwork.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonReconciliationFailed)

	// A status update wakes the controller immediately. Waiting for repeated verification proves
	// the jittered retry remains active after that watch event has been consumed.
	waitInspectNetworkCallCount(t, ctx, tco, networkName, tco.InspectNetworkCallCount(networkName)+2)

	tco.SetRuntimeHealth(true)
	recoveredNetwork := waitPhysicalContainerNetworkPhaseEx(t, ctx, serverInfo.Client, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)
	requireReadyCondition(t, recoveredNetwork.Status.Conditions, metav1.ConditionTrue, apiv2.PhysicalContainerNetworkReasonNetworkReady)
}

func TestV2PhysicalContainerNetworkControllerWaitsForNamespace(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	networkName := "v2-pcn-wait-namespace-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)
	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wait-namespace-network",
			Namespace: "v2-pcn-wait-namespace",
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			NetworkName: networkName,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	pendingNetwork := waitPhysicalContainerNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhasePending)
	requireReadyCondition(t, pendingNetwork.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonPending)
	require.Equal(t, 0, containerOrchestrator.CreateNetworkCallCount(networkName))

	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: network.Namespace}}
	require.NoError(t, client.Create(ctx, namespace))
	waitV2NamespaceActive(t, ctx, namespace.Name)

	readyNetwork := waitPhysicalContainerNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)
	require.NotEmpty(t, readyNetwork.Status.NetworkID)
	require.Equal(t, 1, containerOrchestrator.CreateNetworkCallCount(networkName))
}

func waitPhysicalContainerNetworkPhase(
	t *testing.T,
	ctx context.Context,
	name types.NamespacedName,
	phase apiv2.PhysicalContainerNetworkPhase,
) *apiv2.PhysicalContainerNetwork {
	t.Helper()

	return waitObjectAssumesState(t, ctx, name, func(network *apiv2.PhysicalContainerNetwork) (bool, error) {
		return network.Status.Phase == phase, nil
	})
}

func waitPhysicalContainerNetworkPhaseEx(
	t *testing.T,
	ctx context.Context,
	apiClient ctrl_client.Client,
	name types.NamespacedName,
	phase apiv2.PhysicalContainerNetworkPhase,
) *apiv2.PhysicalContainerNetwork {
	t.Helper()

	return waitObjectAssumesStateEx(t, ctx, apiClient, name, func(network *apiv2.PhysicalContainerNetwork) (bool, error) {
		return network.Status.Phase == phase, nil
	})
}

func waitCreateNetworkCallCount(t *testing.T, ctx context.Context, networkName string, expected int) {
	t.Helper()

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		return containerOrchestrator.CreateNetworkCallCount(networkName) >= expected, nil
	})
	require.NoError(t, waitErr)
}

func waitInspectNetworkCallCount(
	t *testing.T,
	ctx context.Context,
	orchestrator *ctrl_testutil.TestContainerOrchestrator,
	networkID string,
	expected int,
) {
	t.Helper()

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		return orchestrator.InspectNetworkCallCount(networkID) >= expected, nil
	})
	require.NoError(t, waitErr)
}

func waitRuntimeNetworkMissing(t *testing.T, ctx context.Context, networkID string) {
	t.Helper()

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		inspectedNetworks, inspectErr := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
			Networks: []string{networkID},
		})
		if len(inspectedNetworks) > 0 {
			return false, nil
		}
		if errors.Is(inspectErr, containers.ErrNotFound) {
			return true, nil
		}
		return false, inspectErr
	})
	require.NoError(t, waitErr)
}

func runtimeNetworkLabels(t *testing.T, ctx context.Context, networkName string) map[string]string {
	t.Helper()

	listedNetworks, listErr := containerOrchestrator.ListNetworks(ctx, containers.ListNetworksOptions{})
	require.NoError(t, listErr)

	networkIndex := slices.IndexFunc(listedNetworks, func(network containers.ListedNetwork) bool {
		return network.Name == networkName
	})
	require.GreaterOrEqual(t, networkIndex, 0)

	return listedNetworks[networkIndex].Labels
}
