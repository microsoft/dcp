/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containertest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

type cleanupTestOrchestrator struct {
	containers.ContainerOrchestrator

	lock     sync.Mutex
	present  map[ResourceKind]map[string]bool
	retained map[ResourceKind]map[string]bool
	removed  []Resource
}

func newCleanupTestOrchestrator(resources ...Resource) *cleanupTestOrchestrator {
	orchestrator := &cleanupTestOrchestrator{
		present: map[ResourceKind]map[string]bool{
			ResourceContainer: {},
			ResourceNetwork:   {},
			ResourceVolume:    {},
			ResourceImage:     {},
		},
		retained: map[ResourceKind]map[string]bool{
			ResourceContainer: {},
			ResourceNetwork:   {},
			ResourceVolume:    {},
			ResourceImage:     {},
		},
	}
	for _, resource := range resources {
		orchestrator.present[resource.Kind][resource.Identifier] = true
	}
	return orchestrator
}

func (orchestrator *cleanupTestOrchestrator) remove(kind ResourceKind, identifiers []string) ([]string, error) {
	orchestrator.lock.Lock()
	defer orchestrator.lock.Unlock()

	var removed []string
	var removeErrors error
	for _, identifier := range identifiers {
		if !orchestrator.present[kind][identifier] {
			removeErrors = errors.Join(removeErrors, containers.ErrNotFound)
			continue
		}
		if orchestrator.retained[kind][identifier] {
			removed = append(removed, identifier)
			continue
		}
		delete(orchestrator.present[kind], identifier)
		orchestrator.removed = append(orchestrator.removed, Resource{Kind: kind, Identifier: identifier})
		removed = append(removed, identifier)
	}
	if len(removed) < len(identifiers) {
		removeErrors = errors.Join(removeErrors, containers.ErrIncomplete)
	}
	return removed, removeErrors
}

func (orchestrator *cleanupTestOrchestrator) exists(kind ResourceKind, identifier string) bool {
	orchestrator.lock.Lock()
	defer orchestrator.lock.Unlock()
	return orchestrator.present[kind][identifier]
}

func (orchestrator *cleanupTestOrchestrator) RemoveContainers(_ context.Context, options containers.RemoveContainersOptions) ([]string, error) {
	return orchestrator.remove(ResourceContainer, options.Containers)
}

func (orchestrator *cleanupTestOrchestrator) RemoveNetworks(_ context.Context, options containers.RemoveNetworksOptions) ([]string, error) {
	return orchestrator.remove(ResourceNetwork, options.Networks)
}

func (orchestrator *cleanupTestOrchestrator) RemoveVolumes(_ context.Context, options containers.RemoveVolumesOptions) ([]string, error) {
	return orchestrator.remove(ResourceVolume, options.Volumes)
}

func (orchestrator *cleanupTestOrchestrator) RemoveImages(_ context.Context, options containers.RemoveImagesOptions) ([]string, error) {
	return orchestrator.remove(ResourceImage, options.Images)
}

func (orchestrator *cleanupTestOrchestrator) InspectContainers(_ context.Context, options containers.InspectContainersOptions) ([]containers.InspectedContainer, error) {
	if orchestrator.exists(ResourceContainer, options.Containers[0]) {
		return []containers.InspectedContainer{{Name: options.Containers[0]}}, nil
	}
	return nil, errors.Join(containers.ErrNotFound, containers.ErrIncomplete)
}

func (orchestrator *cleanupTestOrchestrator) InspectNetworks(_ context.Context, options containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	if orchestrator.exists(ResourceNetwork, options.Networks[0]) {
		return []containers.InspectedNetwork{{Name: options.Networks[0]}}, nil
	}
	return nil, errors.Join(containers.ErrNotFound, containers.ErrIncomplete)
}

func (orchestrator *cleanupTestOrchestrator) InspectVolumes(_ context.Context, options containers.InspectVolumesOptions) ([]containers.InspectedVolume, error) {
	if orchestrator.exists(ResourceVolume, options.Volumes[0]) {
		return []containers.InspectedVolume{{Name: options.Volumes[0]}}, nil
	}
	return nil, errors.Join(containers.ErrNotFound, containers.ErrIncomplete)
}

func (orchestrator *cleanupTestOrchestrator) InspectImages(_ context.Context, options containers.InspectImagesOptions) ([]containers.InspectedImage, error) {
	if orchestrator.exists(ResourceImage, options.Images[0]) {
		return []containers.InspectedImage{{Id: options.Images[0]}}, nil
	}
	return nil, errors.Join(containers.ErrNotFound, containers.ErrIncomplete)
}

func TestResourceTrackerCleansInDependencyOrder(t *testing.T) {
	t.Parallel()

	resources := []Resource{
		{Kind: ResourceImage, Identifier: "image"},
		{Kind: ResourceVolume, Identifier: "volume"},
		{Kind: ResourceNetwork, Identifier: "network"},
		{Kind: ResourceContainer, Identifier: "container"},
	}
	orchestrator := newCleanupTestOrchestrator(resources...)
	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)

	for _, resource := range resources {
		require.NoError(t, tracker.Track(resource.Kind, resource.Identifier))
	}
	journalPath := tracker.journalPath
	header, journalResources, readErr := readJournal(journalPath)
	require.NoError(t, readErr)
	require.Equal(t, "test", header.Runtime)
	require.Equal(t, resources, journalResources)

	require.NoError(t, tracker.Cleanup(t.Context(), orchestrator))
	require.Equal(t, []Resource{
		{Kind: ResourceContainer, Identifier: "container"},
		{Kind: ResourceNetwork, Identifier: "network"},
		{Kind: ResourceVolume, Identifier: "volume"},
		{Kind: ResourceImage, Identifier: "image"},
	}, orchestrator.removed)

	_, statErr := os.Stat(journalPath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestCleanupResourceAcceptsAlreadyAbsentObject(t *testing.T) {
	t.Parallel()

	orchestrator := newCleanupTestOrchestrator()
	cleanupErr := cleanupResource(t.Context(), orchestrator, Resource{
		Kind:       ResourceContainer,
		Identifier: "already-gone",
	})
	require.NoError(t, cleanupErr)
}

func TestCleanupResourcesContinuesAfterStuckResource(t *testing.T) {
	t.Parallel()

	resources := []Resource{
		{Kind: ResourceContainer, Identifier: "stuck"},
		{Kind: ResourceContainer, Identifier: "removable"},
	}
	orchestrator := newCleanupTestOrchestrator(resources...)
	orchestrator.retained[ResourceContainer]["stuck"] = true
	ctx, cancel := testutil.GetTestContext(t, time.Second)
	defer cancel()

	cleanupErr := cleanupResources(ctx, orchestrator, resources)

	require.ErrorIs(t, cleanupErr, context.DeadlineExceeded)
	require.True(t, orchestrator.exists(ResourceContainer, "stuck"))
	require.False(t, orchestrator.exists(ResourceContainer, "removable"))
	require.Contains(t, orchestrator.removed, Resource{Kind: ResourceContainer, Identifier: "removable"})
}

func TestRecoverStaleJournalAcceptsConcurrentRemoval(t *testing.T) {
	t.Parallel()

	orchestrator := newCleanupTestOrchestrator()
	missingJournalPath := filepath.Join(t.TempDir(), "already-removed.jsonl")

	require.NoError(t, recoverStaleJournal(t.Context(), "test", orchestrator, missingJournalPath))
	require.NoError(t, recoverStaleJournalResources(t.Context(), orchestrator, missingJournalPath))
}

func TestFailedStaleRecoveryRetainsJournal(t *testing.T) {
	t.Parallel()

	resource := Resource{Kind: ResourceContainer, Identifier: "stuck"}
	orchestrator := newCleanupTestOrchestrator(resource)
	orchestrator.retained[ResourceContainer][resource.Identifier] = true
	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)
	require.NoError(t, tracker.Track(resource.Kind, resource.Identifier))

	tracker.lock.Lock()
	require.NoError(t, tracker.journalFile.Close())
	tracker.journalFile = nil
	journalPath := tracker.journalPath
	tracker.lock.Unlock()

	ctx, cancel := testutil.GetTestContext(t, 500*time.Millisecond)
	recoveryErr := recoverStaleJournalResources(ctx, orchestrator, journalPath)
	cancel()

	require.ErrorIs(t, recoveryErr, context.DeadlineExceeded)
	require.FileExists(t, journalPath)

	delete(orchestrator.retained[ResourceContainer], resource.Identifier)
	require.NoError(t, tracker.Cleanup(t.Context(), orchestrator))
}

func TestReadJournalResourcesToleratesTruncatedFinalRecordForRecovery(t *testing.T) {
	t.Parallel()

	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)
	require.NoError(t, tracker.TrackContainer("container"))

	tracker.lock.Lock()
	require.NoError(t, tracker.journalFile.Close())
	tracker.journalFile = nil
	journalPath := tracker.journalPath
	tracker.lock.Unlock()

	journalFile, openErr := usvc_io.OpenOrCreateFileForAppending(journalPath, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, openErr)
	_, writeErr := journalFile.WriteString(`{"kind":"image","identifier":`)
	require.NoError(t, writeErr)
	require.NoError(t, journalFile.Close())

	resources, recoveryReadErr := readJournalResources(journalPath, true)
	require.NoError(t, recoveryReadErr)
	require.Equal(t, []Resource{{Kind: ResourceContainer, Identifier: "container"}}, resources)

	_, strictReadErr := readJournalResources(journalPath, false)
	require.Error(t, strictReadErr)

	orchestrator := newCleanupTestOrchestrator(Resource{Kind: ResourceContainer, Identifier: "container"})
	require.NoError(t, tracker.Cleanup(t.Context(), orchestrator))
}
