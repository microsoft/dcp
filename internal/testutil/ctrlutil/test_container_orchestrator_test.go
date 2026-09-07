/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"context"
	"errors"
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

func TestRemoveImagesReturnsRequestedIdentifiers(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	orchestrator, orchestratorErr := NewTestContainerOrchestrator(ctx, logr.Discard(), TcoOptionNone)
	require.NoError(t, orchestratorErr)
	t.Cleanup(func() {
		require.NoError(t, orchestrator.Close())
	})

	firstID, firstPullErr := orchestrator.PullImage(ctx, containers.PullImageOptions{Image: "example.test/first:latest"})
	require.NoError(t, firstPullErr)
	_, secondPullErr := orchestrator.PullImage(ctx, containers.PullImageOptions{Image: "example.test/second:latest"})
	require.NoError(t, secondPullErr)

	removed, removeErr := orchestrator.RemoveImages(ctx, containers.RemoveImagesOptions{
		Images: []string{"example.test/first:latest", firstID, "missing", "example.test/second:latest"},
		Force:  true,
	})

	require.Equal(t, []string{"example.test/first:latest", "example.test/second:latest"}, removed)
	require.ErrorIs(t, removeErr, containers.ErrNotFound)
	require.ErrorIs(t, removeErr, containers.ErrIncomplete)
	require.False(t, orchestrator.HasImage("example.test/first:latest"))
	require.False(t, orchestrator.HasImage("example.test/second:latest"))

	_, inspectErr := orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
		Images: []string{"example.test/first:latest", "example.test/second:latest"},
	})
	require.True(t, errors.Is(inspectErr, containers.ErrNotFound))
}
