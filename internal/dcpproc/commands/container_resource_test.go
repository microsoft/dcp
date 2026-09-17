/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestCleanupNetworkDisconnectsContainersWithoutRemovingThem(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	orchestrator, orchestratorErr := ctrl_testutil.NewTestContainerOrchestrator(ctx, log, ctrl_testutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)
	defer func() {
		require.NoError(t, orchestrator.Close())
	}()

	createdNetworkID, createNetworkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: "cleanup-network",
	})
	require.NoError(t, createNetworkErr)
	createdContainerID, createContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:     "cleanup-network-container",
		Image:    "cleanup-network-image",
		Networks: []containers.CreateContainerNetworkOptions{{Name: createdNetworkID}},
	})
	require.NoError(t, createContainerErr)

	require.NoError(t, doCleanupNetwork(ctx, createdNetworkID, log, orchestrator))

	_, inspectNetworkErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: []string{createdNetworkID},
	})
	require.ErrorIs(t, inspectNetworkErr, containers.ErrNotFound)
	inspectedContainers, inspectContainerErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{createdContainerID},
	})
	require.NoError(t, inspectContainerErr)
	require.Len(t, inspectedContainers, 1)
}

func TestCleanupVolumeDoesNotForceRemoval(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	orchestrator, orchestratorErr := ctrl_testutil.NewTestContainerOrchestrator(ctx, log, ctrl_testutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)
	defer func() {
		require.NoError(t, orchestrator.Close())
	}()

	const createdVolumeID = "cleanup-volume"
	require.NoError(t, orchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: createdVolumeID}))
	createdContainerID, createContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:  "cleanup-volume-container",
		Image: "cleanup-volume-image",
		VolumeMounts: []containers.CreateContainerVolumeMount{{
			Type:   containers.NamedVolumeMount,
			Source: createdVolumeID,
			Target: "/data",
		}},
	})
	require.NoError(t, createContainerErr)

	cleanupErr := doCleanupVolume(ctx, createdVolumeID, orchestrator)
	require.Error(t, cleanupErr)

	inspectedVolumes, inspectVolumeErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
		Volumes: []string{createdVolumeID},
	})
	require.NoError(t, inspectVolumeErr)
	require.Len(t, inspectedVolumes, 1)
	_, removeContainerErr := orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{createdContainerID},
		Force:      true,
	})
	require.NoError(t, removeContainerErr)
	require.NoError(t, doCleanupVolume(ctx, createdVolumeID, orchestrator))
	_, inspectRemovedVolumeErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
		Volumes: []string{createdVolumeID},
	})
	require.True(t, errors.Is(inspectRemovedVolumeErr, containers.ErrNotFound))
}

func TestCleanupVolumeWaitsForContainerCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	orchestrator, orchestratorErr := ctrl_testutil.NewTestContainerOrchestrator(ctx, log, ctrl_testutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)
	defer func() {
		require.NoError(t, orchestrator.Close())
	}()

	const (
		createdVolumeID    = "cleanup-volume-after-container"
		createdContainerID = "cleanup-volume-after-container-container"
	)
	require.NoError(t, orchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: createdVolumeID}))
	_, createContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:  createdContainerID,
		Image: "cleanup-volume-after-container-image",
		VolumeMounts: []containers.CreateContainerVolumeMount{{
			Type:   containers.NamedVolumeMount,
			Source: createdVolumeID,
			Target: "/data",
		}},
	})
	require.NoError(t, createContainerErr)

	orderedOrchestrator := &removeContainerAfterVolumeAttemptOrchestrator{
		TestContainerOrchestrator: orchestrator,
		containerID:               createdContainerID,
	}
	require.NoError(t, cleanupVolumeAfterMonitorExit(
		ctx,
		createdVolumeID,
		backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Millisecond), 1),
		log,
		orderedOrchestrator,
	))
	require.Equal(t, 2, orchestrator.RemoveVolumeCallCount(createdVolumeID))

	_, inspectRemovedVolumeErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
		Volumes: []string{createdVolumeID},
	})
	require.ErrorIs(t, inspectRemovedVolumeErr, containers.ErrNotFound)
}

func TestCleanupVolumeStopsRetryingWhileContainerRemains(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	orchestrator, orchestratorErr := ctrl_testutil.NewTestContainerOrchestrator(ctx, log, ctrl_testutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)
	defer func() {
		require.NoError(t, orchestrator.Close())
	}()

	const createdVolumeID = "bounded-cleanup-volume"
	require.NoError(t, orchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: createdVolumeID}))
	_, createContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:  "bounded-cleanup-volume-container",
		Image: "bounded-cleanup-volume-image",
		VolumeMounts: []containers.CreateContainerVolumeMount{{
			Type:   containers.NamedVolumeMount,
			Source: createdVolumeID,
			Target: "/data",
		}},
	})
	require.NoError(t, createContainerErr)

	cleanupErr := cleanupVolumeAfterMonitorExit(
		ctx,
		createdVolumeID,
		backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Millisecond), 1),
		log,
		orchestrator,
	)
	require.ErrorIs(t, cleanupErr, containers.ErrObjectInUse)
	require.Equal(t, 2, orchestrator.RemoveVolumeCallCount(createdVolumeID))
}

type removeContainerAfterVolumeAttemptOrchestrator struct {
	*ctrl_testutil.TestContainerOrchestrator
	containerID string
}

func (orchestrator *removeContainerAfterVolumeAttemptOrchestrator) RemoveVolumes(
	ctx context.Context,
	options containers.RemoveVolumesOptions,
) ([]string, error) {
	removedVolumes, removeVolumeErr := orchestrator.TestContainerOrchestrator.RemoveVolumes(ctx, options)
	if errors.Is(removeVolumeErr, containers.ErrObjectInUse) {
		_, removeContainerErr := orchestrator.TestContainerOrchestrator.RemoveContainers(
			ctx,
			containers.RemoveContainersOptions{
				Containers: []string{orchestrator.containerID},
				Force:      true,
			},
		)
		return removedVolumes, errors.Join(removeVolumeErr, removeContainerErr)
	}
	return removedVolumes, removeVolumeErr
}
