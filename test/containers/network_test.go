/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers_test

import (
	"context"
	"fmt"
	std_slices "slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	"github.com/microsoft/dcp/pkg/concurrency"
)

func TestNetworkMethods(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		networkName := containertest.UniqueName(t, "network")
		require.NoError(t, tracker.TrackNetwork(networkName))
		networkID, createErr := runtime.Orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
			Name:   networkName,
			Labels: tracker.MapLabels(),
		})
		require.NoError(t, createErr)
		require.NotEmpty(t, networkID)

		inspected, inspectErr := runtime.Orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
			Networks: []string{networkID},
		})
		require.NoError(t, inspectErr)
		require.Len(t, inspected, 1)
		require.Equal(t, networkName, inspected[0].Name)
		require.Equal(t, networkID, inspected[0].Id)
		require.Equal(t, tracker.RunID(), inspected[0].Labels[containertest.TestRunLabel])

		listed, listErr := runtime.Orchestrator.ListNetworks(ctx, containers.ListNetworksOptions{
			Filters: containers.ListNetworksFilters{
				LabelFilters: []containers.LabelFilter{{
					Key:   containertest.TestRunLabel,
					Value: tracker.RunID(),
				}},
			},
		})
		require.NoError(t, listErr)
		require.NotEqual(t, -1, std_slices.IndexFunc(listed, func(network containers.ListedNetwork) bool {
			return network.ID == networkID && network.Name == networkName
		}))

		_, containerID := runLongLivedContainer(t, ctx, runtime, tracker, "network-container")
		alias := containertest.UniqueName(t, "network-alias")
		connectErr := runtime.Orchestrator.ConnectNetwork(ctx, containers.ConnectNetworkOptions{
			Network:   networkID,
			Container: containerID,
			Aliases:   []string{alias},
		})
		require.NoError(t, connectErr)
		waitForNetworkConnection(t, ctx, runtime.Orchestrator, networkID, containerID, true)

		connectedContainers, listContainersErr := runtime.Orchestrator.ListContainers(ctx, containers.ListContainersOptions{
			All: true,
			Filters: containers.ListContainersFilters{
				NetworkFilters: []string{networkID},
			},
		})
		require.NoError(t, listContainersErr)
		require.NotEqual(t, -1, std_slices.IndexFunc(connectedContainers, func(container containers.ListedContainer) bool {
			return container.Id == containerID
		}))

		inspectedContainers, inspectContainerErr := runtime.Orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
			Containers: []string{containerID},
		})
		require.NoError(t, inspectContainerErr)
		require.Len(t, inspectedContainers, 1)
		networkIndex := std_slices.IndexFunc(inspectedContainers[0].Networks, func(network containers.InspectedContainerNetwork) bool {
			return network.Id == networkID || network.Name == networkName
		})
		require.NotEqual(t, -1, networkIndex)
		require.Contains(t, inspectedContainers[0].Networks[networkIndex].Aliases, alias)

		disconnectErr := runtime.Orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
			Network:   networkID,
			Container: containerID,
		})
		require.NoError(t, disconnectErr)
		waitForNetworkConnection(t, ctx, runtime.Orchestrator, networkID, containerID, false)

		removed, removeErr := runtime.Orchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
			Networks: []string{networkID},
		})
		require.NoError(t, removeErr)
		require.Equal(t, []string{networkID}, removed)
		waitForNetworkAbsent(t, ctx, runtime.Orchestrator, networkID)
	})
}

func TestWatchNetworksMethod(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		events := concurrency.NewUnboundedChan[containers.EventMessage](ctx)
		subscription, watchErr := runtime.Orchestrator.WatchNetworks(events.In)
		require.NoError(t, watchErr)
		t.Cleanup(subscription.Cancel)

		warmNetworkWatcher(t, ctx, runtime, tracker, events.Out)

		networkName := containertest.UniqueName(t, "watch-network")
		require.NoError(t, tracker.TrackNetwork(networkName))
		networkID, createErr := runtime.Orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
			Name:   networkName,
			Labels: tracker.MapLabels(),
		})
		require.NoError(t, createErr)
		_, containerID := runLongLivedContainer(t, ctx, runtime, tracker, "watch-network-container")
		connectErr := runtime.Orchestrator.ConnectNetwork(ctx, containers.ConnectNetworkOptions{
			Network:   networkID,
			Container: containerID,
		})
		require.NoError(t, connectErr)
		disconnectErr := runtime.Orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
			Network:   networkID,
			Container: containerID,
		})
		require.NoError(t, disconnectErr)
		_, removeErr := runtime.Orchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
			Networks: []string{networkID},
		})
		require.NoError(t, removeErr)

		collectionCtx, collectionCancel := context.WithTimeout(ctx, eventCollectionTimeout)
		actions, collectionErr := collectNetworkActions(collectionCtx, events.Out, networkID, containerID)
		collectionCancel()
		require.Contains(t, actions, containers.EventActionConnect)
		require.Contains(t, actions, containers.EventActionDisconnect)
		require.NoError(t, collectionErr, "received network actions: %v", actions)

		subscription.Cancel()
		waitForEventChannelClosed(t, ctx, events.Out)
	})
}

func waitForNetworkConnection(
	t *testing.T,
	ctx context.Context,
	orchestrator containers.ContainerOrchestrator,
	networkID string,
	containerID string,
	expected bool,
) {
	t.Helper()

	waitErr := wait.PollUntilContextCancel(ctx, 200*time.Millisecond, pollImmediately, func(ctx context.Context) (bool, error) {
		inspected, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
			Networks: []string{networkID},
		})
		if inspectErr != nil {
			return false, inspectErr
		}
		if len(inspected) != 1 {
			return false, nil
		}
		connected := std_slices.ContainsFunc(inspected[0].Containers, func(container containers.InspectedNetworkContainer) bool {
			return container.Id == containerID
		})
		return connected == expected, nil
	})
	require.NoError(t, waitErr)
}

func warmNetworkWatcher(
	t *testing.T,
	ctx context.Context,
	runtime containertest.Runtime,
	tracker *containertest.ResourceTracker,
	events <-chan containers.EventMessage,
) {
	t.Helper()

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		networkName := containertest.UniqueName(t, fmt.Sprintf("network-warmup-%d", attempt))
		require.NoError(t, tracker.TrackNetwork(networkName))
		networkID, createErr := runtime.Orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
			Name:   networkName,
			Labels: tracker.MapLabels(),
		})
		require.NoError(t, createErr)
		_, containerID := runLongLivedContainer(t, ctx, runtime, tracker, fmt.Sprintf("network-warmup-container-%d", attempt))
		connectErr := runtime.Orchestrator.ConnectNetwork(ctx, containers.ConnectNetworkOptions{
			Network:   networkID,
			Container: containerID,
		})
		require.NoError(t, connectErr)

		warmupCtx, warmupCancel := context.WithTimeout(ctx, eventWatcherWarmupTimeout)
		_, lastErr = waitForEvent(warmupCtx, events, func(event containers.EventMessage) bool {
			return networkEventMatches(event, networkID, containerID) && event.Action == containers.EventActionConnect
		})
		warmupCancel()

		disconnectErr := runtime.Orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
			Network:   networkID,
			Container: containerID,
		})
		require.NoError(t, disconnectErr)
		_, removeErr := runtime.Orchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
			Networks: []string{networkID},
		})
		require.NoError(t, removeErr)
		if lastErr == nil {
			return
		}
	}

	require.NoError(t, lastErr, "network event watcher did not become ready")
}

func collectNetworkActions(
	ctx context.Context,
	events <-chan containers.EventMessage,
	networkID string,
	containerID string,
) (map[containers.EventAction]bool, error) {
	actions := map[containers.EventAction]bool{}
	for {
		if actions[containers.EventActionConnect] && actions[containers.EventActionDisconnect] {
			return actions, nil
		}

		event, eventErr := waitForEvent(ctx, events, func(event containers.EventMessage) bool {
			return networkEventMatches(event, networkID, containerID)
		})
		if eventErr != nil {
			return actions, eventErr
		}
		actions[event.Action] = true
	}
}

func TestCollectNetworkActionsReturnsPartialResult(t *testing.T) {
	t.Parallel()

	events := make(chan containers.EventMessage, 1)
	events <- containers.EventMessage{
		Source: containers.EventSourceNetwork,
		Action: containers.EventActionConnect,
		Actor:  containers.EventActor{ID: "network"},
		Attributes: map[string]string{
			"container": "container",
		},
	}
	close(events)

	actions, collectionErr := collectNetworkActions(t.Context(), events, "network", "container")

	require.Error(t, collectionErr)
	require.True(t, actions[containers.EventActionConnect])
	require.False(t, actions[containers.EventActionDisconnect])
}

func networkEventMatches(event containers.EventMessage, networkID string, containerID string) bool {
	return event.Source == containers.EventSourceNetwork &&
		event.Actor.ID == networkID &&
		event.Attributes["container"] == containerID
}
