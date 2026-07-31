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

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestV2PhysicalNetworkControllerCreatesNetwork(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pnet-create")
	networkName := "v2-pnet-created-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "created-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalNetworkSpec{
			NetworkName: networkName,
			Labels: []commonapi.Label{
				{Key: "test-label", Value: "test-value"},
			},
		},
	}
	require.NoError(t, client.Create(ctx, network))

	updatedNetwork := waitPhysicalNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalNetworkPhaseReady)
	require.Contains(t, updatedNetwork.Finalizers, apiv2.GroupName+"/physicalnetwork-reconciler")
	require.NotEmpty(t, updatedNetwork.Status.NetworkID)
	require.Equal(t, networkName, updatedNetwork.Status.NetworkName)
	require.Equal(t, "bridge", updatedNetwork.Status.Driver)
	require.False(t, updatedNetwork.Status.CreatedAt.IsZero())
	requireReadyCondition(t, updatedNetwork.Status.Conditions, metav1.ConditionTrue, apiv2.PhysicalNetworkReasonNetworkReady)

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

func TestV2PhysicalNetworkControllerTracksExistingNetwork(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pnet-track")
	networkName := "v2-pnet-tracked-runtime"
	networkID, createErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: networkName})
	require.NoError(t, createErr)
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tracked-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalNetworkSpec{
			NetworkID:          networkID,
			PreserveOnDeletion: true,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	updatedNetwork := waitPhysicalNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalNetworkPhaseReady)
	require.Equal(t, networkID, updatedNetwork.Status.NetworkID)
	require.Equal(t, networkName, updatedNetwork.Status.NetworkName)

	// Tracking must not create anything: the only create is the one this test performed.
	require.Equal(t, 1, containerOrchestrator.CreateNetworkCallCount(networkName))
}

func TestV2PhysicalNetworkControllerRemovesCreatedNetworkOnDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pnet-delete")
	networkName := "v2-pnet-deleted-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "deleted-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalNetworkSpec{
			NetworkName: networkName,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	updatedNetwork := waitPhysicalNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalNetworkPhaseReady)
	networkID := updatedNetwork.Status.NetworkID

	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalNetwork](t, ctx, client, network)
	waitRuntimeNetworkMissing(t, ctx, networkID)
}

func TestV2PhysicalNetworkControllerPreservesCreatedNetworkOnDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pnet-retain")
	networkName := "v2-pnet-retained-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "retained-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalNetworkSpec{
			NetworkName:        networkName,
			PreserveOnDeletion: true,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	updatedNetwork := waitPhysicalNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalNetworkPhaseReady)
	networkID := updatedNetwork.Status.NetworkID
	require.Equal(t, "true", runtimeNetworkLabels(t, ctx, networkName)[controllers.PersistentLabel])

	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalNetwork](t, ctx, client, network)

	inspectedNetworks, inspectErr := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: []string{networkID},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedNetworks, 1)
}

// A tracked network is removed on deletion unless preserveOnDeletion is set, matching PhysicalContainer.
func TestV2PhysicalNetworkControllerRemovesTrackedNetworkOnDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pnet-track-delete")
	networkName := "v2-pnet-tracked-deleted-runtime"
	networkID, createErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: networkName})
	require.NoError(t, createErr)
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tracked-deleted-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalNetworkSpec{
			NetworkID: networkID,
		},
	}
	require.NoError(t, client.Create(ctx, network))
	waitPhysicalNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalNetworkPhaseReady)

	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalNetwork](t, ctx, client, network)
	waitRuntimeNetworkMissing(t, ctx, networkID)
}

func TestV2PhysicalNetworkControllerCleansUpOnNamespaceDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "v2-pnet-ns-cleanup",
		},
	}
	require.NoError(t, client.Create(ctx, namespace))
	waitV2NamespaceActive(t, ctx, namespace.Name)

	networkName := "v2-pnet-ns-cleanup-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)
	network := &apiv2.PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "namespace-cleanup-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalNetworkSpec{
			NetworkName: networkName,
		},
	}
	require.NoError(t, client.Create(ctx, network))
	readyNetwork := waitPhysicalNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalNetworkPhaseReady)
	networkID := readyNetwork.Status.NetworkID

	require.NoError(t, client.Delete(ctx, namespace))

	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalNetwork](t, ctx, client, network)
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
	waitRuntimeNetworkMissing(t, ctx, networkID)
}

func TestV2PhysicalNetworkControllerDoesNotDuplicateCreate(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pnet-single-create")
	networkName := "v2-pnet-single-create-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)
	releaseCreate := containerOrchestrator.BlockCreateNetwork(networkName)
	defer releaseCreate()

	network := &apiv2.PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "single-create-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalNetworkSpec{
			NetworkName: networkName,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	waitCreateNetworkCallCount(t, ctx, networkName, 1)
	pendingNetwork := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(currentNetwork *apiv2.PhysicalNetwork) (bool, error) {
		return currentNetwork.Status.Phase == apiv2.PhysicalNetworkPhasePending, nil
	})
	requireReadyCondition(t, pendingNetwork.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalNetworkReasonCreating)

	// Reconciliations while the create is in flight must not start a second one.
	require.Never(t, func() bool {
		return containerOrchestrator.CreateNetworkCallCount(networkName) > 1
	}, 3*time.Second, 250*time.Millisecond)

	releaseCreate()
	waitPhysicalNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalNetworkPhaseReady)
	require.Equal(t, 1, containerOrchestrator.CreateNetworkCallCount(networkName))
}

func TestV2PhysicalNetworkControllerReportsTerminalCreateFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pnet-create-fail")
	networkName := "v2-pnet-conflicting-runtime"
	_, createErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: networkName})
	require.NoError(t, createErr)
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflicting-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalNetworkSpec{
			NetworkName:        networkName,
			PreserveOnDeletion: true,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	failedNetwork := waitPhysicalNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalNetworkPhaseFailed)
	requireReadyCondition(t, failedNetwork.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalNetworkReasonCreateFailed)

	// The spec is immutable, so a create failure is terminal and must not be retried.
	require.Never(t, func() bool {
		return containerOrchestrator.CreateNetworkCallCount(networkName) > 2
	}, 3*time.Second, 250*time.Millisecond)
}

// Steady-state polling must be paced by the monitoring delay and must not write an unchanged
// status, because a status write feeds a watch event back into the controller and turns the slow
// polling cadence into a tight re-inspect loop.
func TestV2PhysicalNetworkControllerDoesNotChurnReadyStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pnet-steady")
	networkName := "v2-pnet-steady-runtime"
	removeRuntimeNetworkOnCleanup(t, networkName)

	network := &apiv2.PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "steady-network",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalNetworkSpec{
			NetworkName: networkName,
		},
	}
	require.NoError(t, client.Create(ctx, network))

	readyNetwork := waitPhysicalNetworkPhase(t, ctx, network.NamespacedName(), apiv2.PhysicalNetworkPhaseReady)
	readyResourceVersion := readyNetwork.ResourceVersion
	// The status write that announced Ready drives exactly one more reconciliation, which
	// re-inspects and settles. Anything beyond that within the monitoring delay is churn.
	settledInspectCount := containerOrchestrator.InspectNetworkCallCount(readyNetwork.Status.NetworkID) + 1

	require.Never(t, func() bool {
		currentNetwork := &apiv2.PhysicalNetwork{}
		if getErr := client.Get(ctx, network.NamespacedName(), currentNetwork); getErr != nil {
			return false
		}
		if currentNetwork.ResourceVersion != readyResourceVersion {
			return true
		}
		return containerOrchestrator.InspectNetworkCallCount(readyNetwork.Status.NetworkID) > settledInspectCount
	}, 5*time.Second, 250*time.Millisecond)
}

func waitPhysicalNetworkPhase(
	t *testing.T,
	ctx context.Context,
	name types.NamespacedName,
	phase apiv2.PhysicalNetworkPhase,
) *apiv2.PhysicalNetwork {
	t.Helper()

	return waitObjectAssumesState(t, ctx, name, func(network *apiv2.PhysicalNetwork) (bool, error) {
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
