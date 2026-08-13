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
