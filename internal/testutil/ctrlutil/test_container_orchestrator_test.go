/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that TestContainerOrchestrator allows queued removal failures before blocking recovery and handles concurrent removal of another batch member.
func TestContainerRemovalBlocksRecoveryAfterFailures(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 3*time.Minute)
	defer cancel()
	orchestrator, orchestratorErr := NewTestContainerOrchestrator(ctx, logr.Discard(), TcoOptionNone)
	require.NoError(t, orchestratorErr)
	defer func() {
		require.NoError(t, orchestrator.Close())
	}()
	imageID, pullErr := orchestrator.PullImage(ctx, containers.PullImageOptions{Image: "blocked-removal-image"})
	require.NoError(t, pullErr)
	const containerName = "blocked-removal"
	containerID, createErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: containerName, Image: imageID,
	})
	require.NoError(t, createErr)

	firstFailure := errors.New("first removal failure")
	failures := []error{firstFailure, nil}
	releaseRemoval := orchestrator.BlockRemoveContainerAfterFailures(containerName, failures...)
	defer releaseRemoval()
	require.Nil(t, failures[1])
	options := containers.RemoveContainersOptions{Containers: []string{containerID}, Force: true}
	_, firstRemoveErr := orchestrator.RemoveContainers(ctx, options)
	require.ErrorIs(t, firstRemoveErr, firstFailure)
	_, secondRemoveErr := orchestrator.RemoveContainers(ctx, options)
	require.ErrorContains(t, secondRemoveErr, "simulated container removal failure")
	require.Equal(t, 2, orchestrator.RemoveContainerCallCount(containerName))
	otherID, createOtherErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: "unblocked-removal", Image: imageID,
	})
	require.NoError(t, createOtherErr)
	batchOptions := containers.RemoveContainersOptions{Containers: []string{containerID, otherID}, Force: true}

	type removalResult struct {
		ids []string
		err error
	}
	results := make(chan removalResult, 1)
	var removals sync.WaitGroup
	defer func() {
		releaseRemoval()
		cancel()
		removals.Wait()
	}()
	removals.Go(func() {
		removed, removeErr := orchestrator.RemoveContainers(ctx, batchOptions)
		results <- removalResult{ids: removed, err: removeErr}
	})
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		return orchestrator.RemoveContainerCallCount(containerName) >= 3, nil
	}))
	require.Equal(t, 3, orchestrator.RemoveContainerCallCount(containerName))
	inspected, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
	require.NoError(t, inspectErr)
	require.Len(t, inspected, 1)
	select {
	case result, ok := <-results:
		require.True(t, ok)
		t.Fatalf("recovery removal completed before release: %v", result.err)
	default:
	}

	_, removeOtherErr := orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{otherID}, Force: true,
	})
	require.NoError(t, removeOtherErr)

	releaseRemoval()
	select {
	case result, ok := <-results:
		require.True(t, ok)
		require.ErrorIs(t, result.err, containers.ErrNotFound)
		require.ErrorIs(t, result.err, containers.ErrIncomplete)
		require.Equal(t, []string{containerID}, result.ids)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Equal(t, 3, orchestrator.RemoveContainerCallCount(containerName))
	require.Equal(t, 1, orchestrator.RemoveContainerCallCount("unblocked-removal"))
}

// Verifies that blocked container removal honors cancellation and rechecks container existence after concurrent removals are released.
func TestBlockedContainerRemovalHandlesCancellationAndConcurrentDeletion(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 3*time.Minute)
	defer cancel()
	orchestrator, orchestratorErr := NewTestContainerOrchestrator(ctx, logr.Discard(), TcoOptionNone)
	require.NoError(t, orchestratorErr)
	defer func() {
		require.NoError(t, orchestrator.Close())
	}()
	imageID, pullErr := orchestrator.PullImage(ctx, containers.PullImageOptions{Image: "concurrent-removal-image"})
	require.NoError(t, pullErr)
	const containerName = "concurrent-removal"
	containerID, createErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: containerName, Image: imageID,
	})
	require.NoError(t, createErr)
	releaseRemoval := orchestrator.BlockRemoveContainerAfterFailures(containerName)
	defer releaseRemoval()
	options := containers.RemoveContainersOptions{Containers: []string{containerID}, Force: true}
	results := make(chan error, 2)
	var removals sync.WaitGroup
	defer func() {
		releaseRemoval()
		cancel()
		removals.Wait()
	}()
	removalCtx, cancelRemoval := context.WithCancel(ctx)
	defer cancelRemoval()
	removals.Go(func() {
		_, removeErr := orchestrator.RemoveContainers(removalCtx, options)
		results <- removeErr
	})
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		return orchestrator.RemoveContainerCallCount(containerName) >= 1, nil
	}))
	cancelRemoval()
	select {
	case removeErr, ok := <-results:
		require.True(t, ok)
		require.ErrorIs(t, removeErr, context.Canceled)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	for index := range 2 {
		removals.Go(func() {
			var removeErr error
			if index == 0 {
				_, removeErr = orchestrator.RemoveContainersWithoutEvents(ctx, options)
			} else {
				_, removeErr = orchestrator.RemoveContainers(ctx, options)
			}
			results <- removeErr
		})
	}
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		return orchestrator.RemoveContainerCallCount(containerName) >= 3, nil
	}))
	releaseRemoval()
	successCount := 0
	for range 2 {
		select {
		case removeErr, ok := <-results:
			require.True(t, ok)
			if removeErr == nil {
				successCount++
			} else {
				require.ErrorIs(t, removeErr, containers.ErrNotFound)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.Equal(t, 1, successCount)
	require.Equal(t, 3, orchestrator.RemoveContainerCallCount(containerName))
}

// Verifies that TestContainerOrchestrator preserves remaining batch members when runtime health changes during a blocked removal.
func TestBlockedContainerRemovalPreservesBatchWhenRuntimeBecomesUnhealthy(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 3*time.Minute)
	defer cancel()
	orchestrator, orchestratorErr := NewTestContainerOrchestrator(ctx, logr.Discard(), TcoOptionNone)
	require.NoError(t, orchestratorErr)
	defer func() {
		require.NoError(t, orchestrator.Close())
	}()
	imageID, pullErr := orchestrator.PullImage(ctx, containers.PullImageOptions{Image: "unhealthy-removal-image"})
	require.NoError(t, pullErr)
	const blockedContainerName = "unhealthy-blocked-removal"
	blockedContainerID, createBlockedErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: blockedContainerName, Image: imageID,
	})
	require.NoError(t, createBlockedErr)
	const remainingContainerName = "unhealthy-remaining-removal"
	remainingContainerID, createRemainingErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: remainingContainerName, Image: imageID,
	})
	require.NoError(t, createRemainingErr)

	releaseRemoval := orchestrator.BlockRemoveContainerAfterFailures(blockedContainerName)
	defer releaseRemoval()
	type removalResult struct {
		ids []string
		err error
	}
	results := make(chan removalResult, 1)
	var removals sync.WaitGroup
	defer func() {
		releaseRemoval()
		cancel()
		removals.Wait()
	}()
	removals.Go(func() {
		removed, removeErr := orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
			Containers: []string{blockedContainerID, remainingContainerID},
			Force:      true,
		})
		results <- removalResult{ids: removed, err: removeErr}
	})
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		return orchestrator.RemoveContainerCallCount(blockedContainerName) >= 1, nil
	}))

	orchestrator.SetRuntimeHealth(false)
	releaseRemoval()
	select {
	case result, ok := <-results:
		require.True(t, ok)
		require.ErrorIs(t, result.err, errRuntimeUnhealthy)
		require.ErrorIs(t, result.err, containers.ErrIncomplete)
		require.Empty(t, result.ids)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Equal(t, 1, orchestrator.RemoveContainerCallCount(blockedContainerName))
	require.Equal(t, 0, orchestrator.RemoveContainerCallCount(remainingContainerName))

	orchestrator.SetRuntimeHealth(true)
	inspected, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{blockedContainerID, remainingContainerID},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspected, 2)
}

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

// Verifies that InspectImages returns snapshots whose modification does not affect stored image data.
func TestInspectImagesReturnsSnapshot(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	orchestrator, orchestratorErr := NewTestContainerOrchestrator(ctx, logr.Discard(), TcoOptionNone)
	require.NoError(t, orchestratorErr)
	t.Cleanup(func() {
		require.NoError(t, orchestrator.Close())
	})

	const imageTag = "snapshot-image:latest"
	buildErr := orchestrator.BuildImage(ctx, containers.BuildImageOptions{
		ContainerBuildContext: &containers.ContainerBuildContext{
			Tags:   []string{imageTag},
			Labels: map[string]string{"snapshot-label": "original"},
		},
	})
	require.NoError(t, buildErr)

	images, inspectErr := orchestrator.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{imageTag}})
	require.NoError(t, inspectErr)
	require.Len(t, images, 1)
	imageID := images[0].Id
	images[0].Tags[0] = "modified:latest"
	images[0].Labels["snapshot-label"] = "modified"

	inspectedAgain, inspectAgainErr := orchestrator.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{imageID}})
	require.NoError(t, inspectAgainErr)
	require.Len(t, inspectedAgain, 1)
	require.Equal(t, []string{imageTag}, inspectedAgain[0].Tags)
	require.Equal(t, "original", inspectedAgain[0].Labels["snapshot-label"])
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
