/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
)

func TestRemoveNetworksWithoutForceReturnsSuccessfulResult(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	orchestrator, orchestratorErr := NewTestContainerOrchestrator(ctx, logr.Discard(), TcoOptionNone)
	require.NoError(t, orchestratorErr)
	defer func() {
		require.NoError(t, orchestrator.Close())
	}()

	networkID, createErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: "non-forced-removal",
	})
	require.NoError(t, createErr)

	removedNetworkIDs, removeErr := orchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
		Networks: []string{networkID},
	})
	require.NoError(t, removeErr)
	require.Equal(t, []string{networkID}, removedNetworkIDs)
}

func TestIsBuiltInNetwork(t *testing.T) {
	t.Parallel()

	orchestrator := &TestContainerOrchestrator{}
	require.True(t, orchestrator.IsBuiltInNetwork("bridge"))
	require.True(t, orchestrator.IsBuiltInNetwork("host"))
	require.True(t, orchestrator.IsBuiltInNetwork("none"))
	require.False(t, orchestrator.IsBuiltInNetwork("application"))
}

func TestFailMatchingContainersMatchesBuiltImageTag(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	orchestrator, orchestratorErr := NewTestContainerOrchestrator(ctx, logr.Discard(), TcoOptionNone)
	require.NoError(t, orchestratorErr)
	defer func() {
		require.NoError(t, orchestrator.Close())
	}()

	const imageTag = "test-image:latest"
	buildErr := orchestrator.BuildImage(ctx, containers.BuildImageOptions{
		ContainerBuildContext: &containers.ContainerBuildContext{
			Tags: []string{imageTag},
		},
	})
	require.NoError(t, buildErr)

	images, inspectErr := orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
		Images: []string{imageTag},
	})
	require.NoError(t, inspectErr)
	require.Len(t, images, 1)

	containerID, createErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:  "test-container",
		Image: images[0].Id,
	})
	require.NoError(t, createErr)

	orchestrator.FailMatchingContainers(ctx, "test-image", 1, "expected startup failure")
	_, startErr := orchestrator.StartContainers(ctx, containers.StartContainersOptions{
		Containers: []string{containerID},
	})
	require.ErrorContains(t, startErr, "expected startup failure")
}
