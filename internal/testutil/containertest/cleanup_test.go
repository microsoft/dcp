/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containertest

import (
	"context"
	"encoding/json"
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

	lock               sync.Mutex
	present            map[ResourceKind]map[string]bool
	labels             map[ResourceKind]map[string]map[string]string
	inspectErrors      map[ResourceKind]map[string]error
	retained           map[ResourceKind]map[string]bool
	removed            []Resource
	inspectionStarted  chan struct{}
	continueInspection chan struct{}
	inspectionStart    sync.Once
}

func newCleanupTestOrchestrator(resources ...Resource) *cleanupTestOrchestrator {
	orchestrator := &cleanupTestOrchestrator{
		present: map[ResourceKind]map[string]bool{
			ResourceContainer: {},
			ResourceNetwork:   {},
			ResourceVolume:    {},
			ResourceImage:     {},
		},
		labels: map[ResourceKind]map[string]map[string]string{
			ResourceContainer: {},
			ResourceNetwork:   {},
			ResourceVolume:    {},
			ResourceImage:     {},
		},
		inspectErrors: map[ResourceKind]map[string]error{
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

func (orchestrator *cleanupTestOrchestrator) setOwned(header journalHeader, resources ...Resource) {
	orchestrator.lock.Lock()
	defer orchestrator.lock.Unlock()

	for _, resource := range resources {
		orchestrator.labels[resource.Kind][resource.Identifier] = resourceOwnershipLabels(header)
	}
}

func (orchestrator *cleanupTestOrchestrator) setLabels(resource Resource, labels map[string]string) {
	orchestrator.lock.Lock()
	defer orchestrator.lock.Unlock()
	orchestrator.labels[resource.Kind][resource.Identifier] = labels
}

func (orchestrator *cleanupTestOrchestrator) setInspectError(resource Resource, inspectErr error) {
	orchestrator.lock.Lock()
	defer orchestrator.lock.Unlock()
	orchestrator.inspectErrors[resource.Kind][resource.Identifier] = inspectErr
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

func (orchestrator *cleanupTestOrchestrator) inspect(
	ctx context.Context,
	kind ResourceKind,
	identifier string,
) (map[string]string, error) {
	if orchestrator.inspectionStarted != nil {
		orchestrator.inspectionStart.Do(func() {
			close(orchestrator.inspectionStarted)
		})
		select {
		case <-orchestrator.continueInspection:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	orchestrator.lock.Lock()
	defer orchestrator.lock.Unlock()
	if inspectErr := orchestrator.inspectErrors[kind][identifier]; inspectErr != nil {
		return nil, inspectErr
	}
	if !orchestrator.present[kind][identifier] {
		return nil, errors.Join(containers.ErrNotFound, containers.ErrIncomplete)
	}

	labels := make(map[string]string, len(orchestrator.labels[kind][identifier]))
	for key, value := range orchestrator.labels[kind][identifier] {
		labels[key] = value
	}
	return labels, nil
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

func (orchestrator *cleanupTestOrchestrator) InspectContainers(ctx context.Context, options containers.InspectContainersOptions) ([]containers.InspectedContainer, error) {
	labels, inspectErr := orchestrator.inspect(ctx, ResourceContainer, options.Containers[0])
	if inspectErr == nil {
		return []containers.InspectedContainer{{Name: options.Containers[0], Labels: labels}}, nil
	}
	return nil, inspectErr
}

func (orchestrator *cleanupTestOrchestrator) InspectNetworks(ctx context.Context, options containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	labels, inspectErr := orchestrator.inspect(ctx, ResourceNetwork, options.Networks[0])
	if inspectErr == nil {
		return []containers.InspectedNetwork{{Name: options.Networks[0], Labels: labels}}, nil
	}
	return nil, inspectErr
}

func (orchestrator *cleanupTestOrchestrator) InspectVolumes(ctx context.Context, options containers.InspectVolumesOptions) ([]containers.InspectedVolume, error) {
	labels, inspectErr := orchestrator.inspect(ctx, ResourceVolume, options.Volumes[0])
	if inspectErr == nil {
		return []containers.InspectedVolume{{Name: options.Volumes[0], Labels: labels}}, nil
	}
	return nil, inspectErr
}

func (orchestrator *cleanupTestOrchestrator) InspectImages(ctx context.Context, options containers.InspectImagesOptions) ([]containers.InspectedImage, error) {
	labels, inspectErr := orchestrator.inspect(ctx, ResourceImage, options.Images[0])
	if inspectErr == nil {
		return []containers.InspectedImage{{Id: options.Images[0], Labels: labels}}, nil
	}
	return nil, inspectErr
}

func TestResourceTrackerCleansInDependencyOrder(t *testing.T) {
	t.Parallel()

	resources := []Resource{
		{Kind: ResourceImage, Identifier: "image"},
		{Kind: ResourceVolume, Identifier: "volume"},
		{Kind: ResourceNetwork, Identifier: "network"},
		{Kind: ResourceContainer, Identifier: "container"},
	}
	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)
	orchestrator := newCleanupTestOrchestrator(resources...)
	orchestrator.setOwned(tracker.header, resources...)

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
	cleanupErr := cleanupResource(t.Context(), orchestrator, journalHeader{}, Resource{
		Kind:       ResourceContainer,
		Identifier: "already-gone",
	})
	require.NoError(t, cleanupErr)
}

func TestCleanupResourceRejectsUnlabeledObject(t *testing.T) {
	t.Parallel()

	resource := Resource{Kind: ResourceContainer, Identifier: "unowned"}
	orchestrator := newCleanupTestOrchestrator(resource)
	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)

	cleanupErr := cleanupResource(t.Context(), orchestrator, tracker.header, resource)

	require.ErrorContains(t, cleanupErr, "ownership labels do not match")
	require.True(t, orchestrator.exists(resource.Kind, resource.Identifier))
	require.Empty(t, orchestrator.removed)
}

func TestCleanupResourceRejectsObjectOwnedByDifferentRun(t *testing.T) {
	t.Parallel()

	resource := Resource{Kind: ResourceImage, Identifier: "other-run"}
	orchestrator := newCleanupTestOrchestrator(resource)
	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)
	otherHeader := tracker.header
	otherHeader.RunID = "different-run"
	orchestrator.setOwned(otherHeader, resource)

	cleanupErr := cleanupResource(t.Context(), orchestrator, tracker.header, resource)

	require.ErrorContains(t, cleanupErr, "ownership labels do not match")
	require.True(t, orchestrator.exists(resource.Kind, resource.Identifier))
	require.Empty(t, orchestrator.removed)
}

func TestCleanupResourceRequiresAllOwnershipLabels(t *testing.T) {
	t.Parallel()

	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)
	expectedLabels := resourceOwnershipLabels(tracker.header)

	for key := range expectedLabels {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			resource := Resource{Kind: ResourceNetwork, Identifier: key}
			orchestrator := newCleanupTestOrchestrator(resource)
			labels := resourceOwnershipLabels(tracker.header)
			delete(labels, key)
			orchestrator.setLabels(resource, labels)

			cleanupErr := cleanupResource(t.Context(), orchestrator, tracker.header, resource)

			require.ErrorContains(t, cleanupErr, "ownership labels do not match")
			require.True(t, orchestrator.exists(resource.Kind, resource.Identifier))
			require.Empty(t, orchestrator.removed)
		})
	}
}

func TestCleanupResourceDoesNotRemoveAfterInspectionFailure(t *testing.T) {
	t.Parallel()

	resource := Resource{Kind: ResourceVolume, Identifier: "inspect-failure"}
	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)
	orchestrator := newCleanupTestOrchestrator(resource)
	orchestrator.setOwned(tracker.header, resource)
	orchestrator.setInspectError(resource, errors.New("injected inspection error"))

	cleanupErr := cleanupResource(t.Context(), orchestrator, tracker.header, resource)

	require.ErrorContains(t, cleanupErr, "injected inspection error")
	require.True(t, orchestrator.exists(resource.Kind, resource.Identifier))
	require.Empty(t, orchestrator.removed)
}

func TestCleanupResourcesContinuesAfterStuckResource(t *testing.T) {
	t.Parallel()

	resources := []Resource{
		{Kind: ResourceContainer, Identifier: "stuck"},
		{Kind: ResourceContainer, Identifier: "removable"},
	}
	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)
	orchestrator := newCleanupTestOrchestrator(resources...)
	orchestrator.setOwned(tracker.header, resources...)
	orchestrator.retained[ResourceContainer]["stuck"] = true
	ctx, cancel := testutil.GetTestContext(t, time.Second)
	defer cancel()

	cleanupErr := cleanupResources(ctx, orchestrator, tracker.header, resources)

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
	require.NoError(t, recoverStaleJournalResources(t.Context(), orchestrator, journalHeader{}, missingJournalPath))
}

func TestFailedStaleRecoveryRetainsJournal(t *testing.T) {
	t.Parallel()

	resource := Resource{Kind: ResourceContainer, Identifier: "stuck"}
	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)
	orchestrator := newCleanupTestOrchestrator(resource)
	orchestrator.setOwned(tracker.header, resource)
	orchestrator.retained[ResourceContainer][resource.Identifier] = true
	require.NoError(t, tracker.Track(resource.Kind, resource.Identifier))

	tracker.lock.Lock()
	require.NoError(t, tracker.journalFile.Close())
	tracker.journalFile = nil
	journalPath := tracker.journalPath
	tracker.lock.Unlock()

	ctx, cancel := testutil.GetTestContext(t, 500*time.Millisecond)
	recoveryErr := recoverStaleJournalResources(ctx, orchestrator, tracker.header, journalPath)
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

	resource := Resource{Kind: ResourceContainer, Identifier: "container"}
	orchestrator := newCleanupTestOrchestrator(resource)
	orchestrator.setOwned(tracker.header, resource)
	require.NoError(t, tracker.Cleanup(t.Context(), orchestrator))
}

func TestReadJournalHeaderRejectsMissingIdentityFields(t *testing.T) {
	t.Parallel()

	validHeader := journalHeader{
		Version:          journalVersion,
		Runtime:          "test",
		ProcessID:        123,
		ProcessStartTime: time.Date(2026, time.September, 10, 12, 0, 0, 0, time.UTC),
		RunID:            "run-id",
	}
	testCases := []struct {
		name          string
		header        journalHeader
		expectedError string
	}{
		{
			name: "empty runtime",
			header: func() journalHeader {
				header := validHeader
				header.Runtime = ""
				return header
			}(),
			expectedError: "runtime cannot be empty",
		},
		{
			name: "zero process ID",
			header: func() journalHeader {
				header := validHeader
				header.ProcessID = 0
				return header
			}(),
			expectedError: "process ID must be positive",
		},
		{
			name: "zero process start time",
			header: func() journalHeader {
				header := validHeader
				header.ProcessStartTime = time.Time{}
				return header
			}(),
			expectedError: "process start time cannot be zero",
		},
		{
			name: "empty run ID",
			header: func() journalHeader {
				header := validHeader
				header.RunID = ""
				return header
			}(),
			expectedError: "run ID cannot be empty",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			headerBytes, marshalErr := json.Marshal(testCase.header)
			require.NoError(t, marshalErr)
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			require.NoError(t, usvc_io.WriteFile(
				path,
				append(headerBytes, '\n'),
				osutil.PermissionOnlyOwnerReadWrite,
			))

			_, readErr := readJournalHeader(path)

			require.ErrorContains(t, readErr, testCase.expectedError)
		})
	}
}

func TestCleanupContinuesAfterOwnershipMismatchAndRetainsJournal(t *testing.T) {
	t.Parallel()

	unowned := Resource{Kind: ResourceContainer, Identifier: "unowned"}
	owned := Resource{Kind: ResourceNetwork, Identifier: "owned"}
	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)
	require.NoError(t, tracker.Track(unowned.Kind, unowned.Identifier))
	require.NoError(t, tracker.Track(owned.Kind, owned.Identifier))
	journalPath := tracker.journalPath

	orchestrator := newCleanupTestOrchestrator(unowned, owned)
	orchestrator.setOwned(tracker.header, owned)

	cleanupErr := tracker.Cleanup(t.Context(), orchestrator)

	require.ErrorContains(t, cleanupErr, "ownership labels do not match")
	require.True(t, orchestrator.exists(unowned.Kind, unowned.Identifier))
	require.False(t, orchestrator.exists(owned.Kind, owned.Identifier))
	require.FileExists(t, journalPath)

	orchestrator.setOwned(tracker.header, unowned)
	require.NoError(t, tracker.Cleanup(t.Context(), orchestrator))
	require.NoFileExists(t, journalPath)
}

func TestResourceTrackerCleanupRetriesAfterFailure(t *testing.T) {
	t.Parallel()

	resource := Resource{Kind: ResourceContainer, Identifier: "retry"}
	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)
	require.NoError(t, tracker.Track(resource.Kind, resource.Identifier))
	journalPath := tracker.journalPath

	orchestrator := newCleanupTestOrchestrator(resource)
	orchestrator.setOwned(tracker.header, resource)
	orchestrator.retained[resource.Kind][resource.Identifier] = true
	ctx, cancel := testutil.GetTestContext(t, 500*time.Millisecond)
	cleanupErr := tracker.Cleanup(ctx, orchestrator)
	cancel()

	require.ErrorIs(t, cleanupErr, context.DeadlineExceeded)
	require.FileExists(t, journalPath)
	require.ErrorContains(t, tracker.TrackImage("late"), "resource tracker is closed")

	delete(orchestrator.retained[resource.Kind], resource.Identifier)
	require.NoError(t, tracker.Cleanup(t.Context(), orchestrator))
	require.NoFileExists(t, journalPath)
}

func TestResourceTrackerContinuesCleanupAfterJournalCloseError(t *testing.T) {
	t.Parallel()

	resource := Resource{Kind: ResourceVolume, Identifier: "close-error"}
	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)
	require.NoError(t, tracker.Track(resource.Kind, resource.Identifier))
	journalPath := tracker.journalPath

	tracker.lock.Lock()
	tracker.journalFile = &closeErrorJournal{
		cleanupJournal: tracker.journalFile,
		closeErr:       errors.New("injected close error"),
	}
	tracker.lock.Unlock()

	orchestrator := newCleanupTestOrchestrator(resource)
	orchestrator.setOwned(tracker.header, resource)

	cleanupErr := tracker.Cleanup(t.Context(), orchestrator)

	require.ErrorContains(t, cleanupErr, "injected close error")
	require.False(t, orchestrator.exists(resource.Kind, resource.Identifier))
	require.NoFileExists(t, journalPath)
	require.NoError(t, tracker.Cleanup(t.Context(), orchestrator))
}

func TestTrackCannotReopenJournalAfterCleanupBegins(t *testing.T) {
	resource := Resource{Kind: ResourceContainer, Identifier: "original"}
	tracker, trackerErr := newResourceTracker("test")
	require.NoError(t, trackerErr)
	require.NoError(t, tracker.Track(resource.Kind, resource.Identifier))
	journalPath := tracker.journalPath

	orchestrator := newCleanupTestOrchestrator(resource)
	orchestrator.setOwned(tracker.header, resource)
	orchestrator.inspectionStarted = make(chan struct{})
	orchestrator.continueInspection = make(chan struct{})

	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- tracker.Cleanup(t.Context(), orchestrator)
	}()

	select {
	case <-orchestrator.inspectionStarted:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}

	require.ErrorContains(t, tracker.TrackImage("late"), "resource tracker is closed")
	require.Equal(t, journalPath, tracker.journalPath)
	activeTracker, found := activeTrackers.Load(journalPath)
	require.True(t, found)
	require.Same(t, tracker, activeTracker)
	_, journalResources, readErr := readJournal(journalPath)
	require.NoError(t, readErr)
	require.Equal(t, []Resource{resource}, journalResources)

	close(orchestrator.continueInspection)
	require.NoError(t, <-cleanupDone)
	require.NoFileExists(t, journalPath)
}

type closeErrorJournal struct {
	cleanupJournal
	closeErr error
}

func (journal *closeErrorJournal) Close() error {
	return errors.Join(journal.cleanupJournal.Close(), journal.closeErr)
}
