/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestV2PhysicalContainerNetworkConnectionReconcilesMembership(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcnc-reconcile")
	networkName := "v2-pcnc-reconcile-runtime"
	networkID, createNetworkErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: networkName})
	require.NoError(t, createNetworkErr)
	removeRuntimeNetworkOnCleanup(t, networkName)

	physicalNetwork := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalContainerNetworkSpec{NetworkID: networkID},
	}
	require.NoError(t, client.Create(ctx, physicalNetwork))
	readyNetwork := waitPhysicalContainerNetworkPhase(t, ctx, physicalNetwork.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)

	containerID := runExistingTestContainer(t, ctx, "v2-pcnc-reconcile-container", "v2-pcnc-image")
	physicalContainer := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalContainerSpec{ContainerID: containerID},
	}
	require.NoError(t, client.Create(ctx, physicalContainer))
	readyContainer := waitPhysicalContainerPhase(t, ctx, physicalContainer.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)

	connection := &apiv2.PhysicalContainerNetworkConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "connection", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerNetworkConnectionSpec{
			ContainerRef: physicalContainer.Name,
			NetworkRef:   physicalNetwork.Name,
			Aliases:      []string{"physical-alias"},
		},
	}
	require.NoError(t, client.Create(ctx, connection))

	waitRuntimeContainerNetworkMembership(t, ctx, readyNetwork.Status.NetworkID, readyContainer.Status.ContainerID, true)
	waitPhysicalContainerNetworkMembershipStatus(t, ctx, physicalNetwork.NamespacedName(), readyContainer.Status.ContainerID, true)

	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, readyContainer.Status.ContainerID, containers.ContainerStatusPaused))
	waitPhysicalContainerPhase(t, ctx, physicalContainer.NamespacedName(), apiv2.PhysicalContainerPhasePaused)
	disconnectErr := containerOrchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
		Network:   readyNetwork.Status.NetworkID,
		Container: readyContainer.Status.ContainerID,
	})
	require.NoError(t, disconnectErr)
	waitRuntimeContainerNetworkMembership(t, ctx, readyNetwork.Status.NetworkID, readyContainer.Status.ContainerID, true)
	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, readyContainer.Status.ContainerID, containers.ContainerStatusRunning))
	waitPhysicalContainerPhase(t, ctx, physicalContainer.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)

	require.NoError(t, client.Delete(ctx, connection))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, connection)
	waitRuntimeContainerNetworkMembership(t, ctx, readyNetwork.Status.NetworkID, readyContainer.Status.ContainerID, false)
	waitPhysicalContainerNetworkMembershipStatus(t, ctx, physicalNetwork.NamespacedName(), readyContainer.Status.ContainerID, false)
	require.NoError(t, client.Delete(ctx, physicalContainer))
	require.NoError(t, client.Delete(ctx, physicalNetwork))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physicalContainer)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physicalNetwork)
}

func TestV1NetworkControllerPreservesV2ManagedConnection(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	network := &apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "v1-network-with-v2-connection"},
	}
	require.NoError(t, client.Create(ctx, network))
	readyV1Network := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv1.ContainerNetwork) (bool, error) {
		if current.Status.State == apiv1.ContainerNetworkStateFailedToStart {
			return false, fmt.Errorf("network creation failed: %s", current.Status.Message)
		}
		return current.Status.State == apiv1.ContainerNetworkStateRunning && current.Status.ID != "", nil
	})

	namespace := createActiveV2Namespace(t, ctx, "v1-network-v2-connection")
	physicalNetwork := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalContainerNetworkSpec{NetworkID: readyV1Network.Status.ID},
	}
	require.NoError(t, client.Create(ctx, physicalNetwork))
	readyPhysicalNetwork := waitPhysicalContainerNetworkPhase(t, ctx, physicalNetwork.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady)

	containerID := runExistingTestContainer(t, ctx, "v1-network-v2-connection-container", "v1-network-v2-connection-image")
	physicalContainer := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalContainerSpec{ContainerID: containerID},
	}
	require.NoError(t, client.Create(ctx, physicalContainer))
	readyContainer := waitPhysicalContainerPhase(t, ctx, physicalContainer.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)

	connection := &apiv2.PhysicalContainerNetworkConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "connection", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerNetworkConnectionSpec{
			ContainerRef: physicalContainer.Name,
			NetworkRef:   physicalNetwork.Name,
		},
	}
	require.NoError(t, client.Create(ctx, connection))
	waitRuntimeContainerNetworkMembership(t, ctx, readyPhysicalNetwork.Status.NetworkID, readyContainer.Status.ContainerID, true)
	waitPhysicalContainerNetworkMembershipStatus(t, ctx, physicalNetwork.NamespacedName(), readyContainer.Status.ContainerID, true)

	_ = waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv1.ContainerNetwork) (bool, error) {
		for _, connectedContainerID := range current.Status.ContainerIDs {
			if connectedContainerID == readyContainer.Status.ContainerID {
				return true, nil
			}
		}
		return false, nil
	})

	waitRuntimeContainerNetworkMembership(t, ctx, readyPhysicalNetwork.Status.NetworkID, readyContainer.Status.ContainerID, true)

	require.NoError(t, client.Delete(ctx, connection))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, connection)
	require.NoError(t, client.Delete(ctx, physicalContainer))
	require.NoError(t, client.Delete(ctx, physicalNetwork))
	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physicalContainer)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physicalNetwork)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, network)
}

func waitPhysicalContainerNetworkMembershipStatus(
	t *testing.T,
	ctx context.Context,
	name types.NamespacedName,
	containerID string,
	expected bool,
) *apiv2.PhysicalContainerNetwork {
	t.Helper()

	return waitObjectAssumesState(t, ctx, name, func(network *apiv2.PhysicalContainerNetwork) (bool, error) {
		found := false
		for _, connectedContainerID := range network.Status.ContainerIDs {
			if connectedContainerID == containerID {
				found = true
				break
			}
		}
		return found == expected, nil
	})
}

func waitRuntimeContainerNetworkMembership(
	t *testing.T,
	ctx context.Context,
	networkID string,
	containerID string,
	expected bool,
) {
	t.Helper()

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		inspectedNetworks, inspectErr := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
			Networks: []string{networkID},
		})
		if inspectErr != nil {
			return false, inspectErr
		}
		if len(inspectedNetworks) != 1 {
			return false, nil
		}

		found := false
		for _, container := range inspectedNetworks[0].Containers {
			if container.Id == containerID {
				found = true
				break
			}
		}
		return found == expected, nil
	})
	require.NoError(t, waitErr)
}
