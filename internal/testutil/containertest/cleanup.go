/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containertest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	std_slices "slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	container_runtimes "github.com/microsoft/dcp/internal/containers/runtimes"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	journalVersion       = 1
	journalDirectoryName = "dcp-container-orchestrator-tests"
	resourcePollInterval = 200 * time.Millisecond
	resourceCleanupTime  = 1 * time.Minute

	// TestRunLabel identifies physical resources owned by one true-runtime test.
	TestRunLabel = "com.microsoft.developer.dcp.container-orchestrator-test-run"
)

// ResourceKind identifies the physical runtime object represented by a cleanup entry.
type ResourceKind string

const (
	ResourceContainer ResourceKind = "container"
	ResourceNetwork   ResourceKind = "network"
	ResourceVolume    ResourceKind = "volume"
	ResourceImage     ResourceKind = "image"
)

type journalHeader struct {
	Version          int           `json:"version"`
	Runtime          string        `json:"runtime"`
	ProcessID        process.Pid_t `json:"processId"`
	ProcessStartTime time.Time     `json:"processStartTime"`
	RunID            string        `json:"runId"`
}

// Resource identifies one physical runtime object that must be removed.
type Resource struct {
	Kind       ResourceKind `json:"kind"`
	Identifier string       `json:"identifier"`
}

type cleanupJournal interface {
	io.Writer
	Sync() error
	Close() error
}

// ResourceTracker durably records physical runtime objects and removes them in dependency order.
type ResourceTracker struct {
	runtimeName string
	header      journalHeader

	lock            sync.Mutex
	cleanupLock     sync.Mutex
	journalPath     string
	journalFile     cleanupJournal
	resources       []Resource
	trackingClosed  bool
	cleanupComplete bool
}

var activeTrackers sync.Map

// NewResourceTracker creates a tracker whose cleanup runs before the runtime subtest's
// process executor is disposed.
func NewResourceTracker(t *testing.T, runtime Runtime) *ResourceTracker {
	t.Helper()

	tracker, trackerErr := newResourceTracker(runtime.Name)
	if trackerErr != nil {
		t.Fatalf("could not create resource tracker for runtime %q: %v", runtime.Name, trackerErr)
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), resourceCleanupTime)
		defer cleanupCancel()

		if cleanupErr := tracker.Cleanup(cleanupCtx, runtime.Orchestrator); cleanupErr != nil {
			t.Errorf("could not clean tracked resources for runtime %q: %v", runtime.Name, cleanupErr)
		}
	})

	return tracker
}

func newResourceTracker(runtimeName string) (*ResourceTracker, error) {
	currentProcess, processErr := process.This()
	if processErr != nil {
		return nil, fmt.Errorf("getting current process identity: %w", processErr)
	}

	runIDBytes, runIDErr := randdata.MakeRandomString(12)
	if runIDErr != nil {
		return nil, fmt.Errorf("generating test run ID: %w", runIDErr)
	}

	return &ResourceTracker{
		runtimeName: runtimeName,
		header: journalHeader{
			Version:          journalVersion,
			Runtime:          runtimeName,
			ProcessID:        currentProcess.Pid,
			ProcessStartTime: currentProcess.IdentityTime.UTC(),
			RunID:            string(runIDBytes),
		},
	}, nil
}

func (tracker *ResourceTracker) RunID() string {
	return tracker.header.RunID
}

func (tracker *ResourceTracker) MapLabels() map[string]string {
	return resourceOwnershipLabels(tracker.header)
}

func (tracker *ResourceTracker) Labels() []commonapi.Label {
	mapLabels := tracker.MapLabels()
	labels := make([]commonapi.Label, 0, len(mapLabels))
	for key, value := range mapLabels {
		labels = append(labels, commonapi.Label{Key: key, Value: value})
	}
	return labels
}

func (tracker *ResourceTracker) TrackContainer(identifier string) error {
	return tracker.Track(ResourceContainer, identifier)
}

func (tracker *ResourceTracker) TrackNetwork(identifier string) error {
	return tracker.Track(ResourceNetwork, identifier)
}

func (tracker *ResourceTracker) TrackVolume(identifier string) error {
	return tracker.Track(ResourceVolume, identifier)
}

func (tracker *ResourceTracker) TrackImage(identifier string) error {
	return tracker.Track(ResourceImage, identifier)
}

// Track durably records an intended resource name before the create operation is issued.
func (tracker *ResourceTracker) Track(kind ResourceKind, identifier string) error {
	tracker.lock.Lock()
	defer tracker.lock.Unlock()

	if tracker.trackingClosed {
		return fmt.Errorf("resource tracker is closed")
	}
	if identifier == "" {
		return fmt.Errorf("resource identifier cannot be empty")
	}
	if !validResourceKind(kind) {
		return fmt.Errorf("unsupported resource kind %q", kind)
	}

	if tracker.journalFile == nil {
		if journalErr := tracker.openJournal(); journalErr != nil {
			return journalErr
		}
	}

	resource := Resource{Kind: kind, Identifier: identifier}
	if encodeErr := json.NewEncoder(tracker.journalFile).Encode(resource); encodeErr != nil {
		return fmt.Errorf("writing cleanup journal entry: %w", encodeErr)
	}
	if syncErr := tracker.journalFile.Sync(); syncErr != nil {
		return fmt.Errorf("syncing cleanup journal entry: %w", syncErr)
	}

	tracker.resources = append(tracker.resources, resource)
	return nil
}

func (tracker *ResourceTracker) openJournal() error {
	journalDir := filepath.Join(testutil.TestTempRoot(), journalDirectoryName)
	if mkdirErr := os.MkdirAll(journalDir, osutil.PermissionOnlyOwnerReadWriteTraverse); mkdirErr != nil {
		return fmt.Errorf("creating cleanup journal directory: %w", mkdirErr)
	}

	suffix, suffixErr := randdata.MakeRandomString(10)
	if suffixErr != nil {
		return fmt.Errorf("generating cleanup journal suffix: %w", suffixErr)
	}

	tracker.journalPath = filepath.Join(
		journalDir,
		fmt.Sprintf("resources-%d-%s.jsonl", tracker.header.ProcessID, suffix),
	)
	pendingJournalPath := tracker.journalPath + ".tmp"
	pendingJournal, openErr := usvc_io.CreateNewFile(pendingJournalPath, osutil.PermissionOnlyOwnerReadWrite)
	if openErr != nil {
		return fmt.Errorf("opening pending cleanup journal: %w", openErr)
	}

	if encodeErr := json.NewEncoder(pendingJournal).Encode(tracker.header); encodeErr != nil {
		_ = pendingJournal.Close()
		_ = os.Remove(pendingJournalPath)
		return fmt.Errorf("writing cleanup journal header: %w", encodeErr)
	}
	if syncErr := pendingJournal.Sync(); syncErr != nil {
		_ = pendingJournal.Close()
		_ = os.Remove(pendingJournalPath)
		return fmt.Errorf("syncing cleanup journal header: %w", syncErr)
	}
	if closeErr := pendingJournal.Close(); closeErr != nil {
		_ = os.Remove(pendingJournalPath)
		return fmt.Errorf("closing pending cleanup journal: %w", closeErr)
	}
	if renameErr := os.Rename(pendingJournalPath, tracker.journalPath); renameErr != nil {
		_ = os.Remove(pendingJournalPath)
		return fmt.Errorf("publishing cleanup journal: %w", renameErr)
	}

	journalFile, reopenErr := usvc_io.OpenOrCreateFileForAppending(tracker.journalPath, osutil.PermissionOnlyOwnerReadWrite)
	if reopenErr != nil {
		_ = os.Remove(tracker.journalPath)
		return fmt.Errorf("reopening cleanup journal: %w", reopenErr)
	}
	tracker.journalFile = journalFile

	activeTrackers.Store(tracker.journalPath, tracker)
	return nil
}

// Cleanup removes every tracked resource and deletes the journal after absence is verified.
func (tracker *ResourceTracker) Cleanup(ctx context.Context, orchestrator containers.ContainerOrchestrator) error {
	tracker.cleanupLock.Lock()
	defer tracker.cleanupLock.Unlock()

	tracker.lock.Lock()
	if tracker.cleanupComplete {
		tracker.lock.Unlock()
		return nil
	}
	tracker.trackingClosed = true
	journalFile := tracker.journalFile
	tracker.journalFile = nil
	resources := append([]Resource(nil), tracker.resources...)
	journalPath := tracker.journalPath
	header := tracker.header
	tracker.lock.Unlock()

	var closeJournalErr error
	if journalFile != nil {
		if closeErr := journalFile.Close(); closeErr != nil {
			closeJournalErr = fmt.Errorf("closing cleanup journal: %w", closeErr)
		}
	}

	cleanupErr := cleanupResources(ctx, orchestrator, header, resources)

	var removeJournalErr error
	if cleanupErr == nil && journalPath != "" {
		if removeErr := os.Remove(journalPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			removeJournalErr = fmt.Errorf("removing cleanup journal: %w", removeErr)
		}
	}

	if cleanupErr == nil && removeJournalErr == nil {
		tracker.lock.Lock()
		tracker.cleanupComplete = true
		tracker.lock.Unlock()
		if journalPath != "" {
			activeTrackers.Delete(journalPath)
		}
	}

	return errors.Join(closeJournalErr, cleanupErr, removeJournalErr)
}

func resourceOwnershipLabels(header journalHeader) map[string]string {
	return map[string]string{
		TestRunLabel:                      header.RunID,
		controllers.PersistentLabel:       "false",
		controllers.CreatorProcessIdLabel: fmt.Sprintf("%d", header.ProcessID),
		controllers.CreatorProcessStartTimeLabel: header.ProcessStartTime.Format(
			osutil.RFC3339MiliTimestampFormat,
		),
	}
}

// CleanupActiveResources retries cleanup for all journals created by the current test process.
func CleanupActiveResources(ctx context.Context) error {
	var cleanupErrors error
	var trackers []*ResourceTracker

	activeTrackers.Range(func(_, value any) bool {
		trackers = append(trackers, value.(*ResourceTracker))
		return true
	})

	for trackerIndex, tracker := range trackers {
		trackerCtx, trackerCancel := nextCleanupContext(ctx, len(trackers)-trackerIndex)
		log := testutil.NewLogForTesting("cleanup-" + tracker.runtimeName)
		executor := process.NewOSExecutor(log)
		orchestrator, orchestratorErr := container_runtimes.FindContainerRuntime(
			trackerCtx,
			tracker.runtimeName,
			log.WithName("ContainerOrchestrator"),
			executor,
		)
		if orchestratorErr != nil {
			cleanupErrors = errors.Join(cleanupErrors, fmt.Errorf("creating %s orchestrator for cleanup: %w", tracker.runtimeName, orchestratorErr))
			executor.Dispose()
			trackerCancel()
			continue
		}

		cleanupErrors = errors.Join(cleanupErrors, tracker.Cleanup(trackerCtx, orchestrator))
		executor.Dispose()
		trackerCancel()
	}

	return cleanupErrors
}

// RecoverStaleResources removes resources recorded by dead test processes for one runtime.
func RecoverStaleResources(ctx context.Context, runtimeName string, orchestrator containers.ContainerOrchestrator) error {
	journalDir := filepath.Join(testutil.TestTempRoot(), journalDirectoryName)
	dirEntries, readDirErr := os.ReadDir(journalDir)
	if errors.Is(readDirErr, os.ErrNotExist) {
		return nil
	}
	if readDirErr != nil {
		return fmt.Errorf("reading cleanup journal directory: %w", readDirErr)
	}

	var recoveryErrors error
	var journalPaths []string
	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() || !strings.HasSuffix(dirEntry.Name(), ".jsonl") {
			continue
		}
		journalPaths = append(journalPaths, filepath.Join(journalDir, dirEntry.Name()))
	}

	for journalIndex, journalPath := range journalPaths {
		journalCtx, journalCancel := nextCleanupContext(ctx, len(journalPaths)-journalIndex)
		recoveryErrors = errors.Join(
			recoveryErrors,
			recoverStaleJournal(journalCtx, runtimeName, orchestrator, journalPath),
		)
		journalCancel()
	}

	return recoveryErrors
}

func recoverStaleJournal(
	ctx context.Context,
	runtimeName string,
	orchestrator containers.ContainerOrchestrator,
	journalPath string,
) error {
	header, headerErr := readJournalHeader(journalPath)
	if errors.Is(headerErr, os.ErrNotExist) {
		return nil
	}
	if headerErr != nil {
		return fmt.Errorf("reading cleanup journal header %q: %w", journalPath, headerErr)
	}
	if header.Runtime != runtimeName {
		return nil
	}

	processHandle := process.NewHandle(header.ProcessID, header.ProcessStartTime)
	runningProcess, findErr := process.FindProcess(processHandle)
	if findErr == nil {
		_ = runningProcess.Release()
		return nil
	}
	if !process.IsProcessGoneErr(findErr) {
		return fmt.Errorf("checking cleanup journal owner %d: %w", header.ProcessID, findErr)
	}

	return recoverStaleJournalResources(ctx, orchestrator, header, journalPath)
}

func recoverStaleJournalResources(
	ctx context.Context,
	orchestrator containers.ContainerOrchestrator,
	header journalHeader,
	journalPath string,
) error {
	resources, resourcesErr := readJournalResources(journalPath, true)
	if errors.Is(resourcesErr, os.ErrNotExist) {
		return nil
	}
	if resourcesErr != nil {
		return fmt.Errorf("reading cleanup journal resources %q: %w", journalPath, resourcesErr)
	}
	if cleanupErr := cleanupResources(ctx, orchestrator, header, resources); cleanupErr != nil {
		return fmt.Errorf("cleaning resources from %q: %w", journalPath, cleanupErr)
	}
	if removeErr := os.Remove(journalPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("removing recovered cleanup journal %q: %w", journalPath, removeErr)
	}

	return nil
}

func readJournal(path string) (journalHeader, []Resource, error) {
	header, headerErr := readJournalHeader(path)
	if headerErr != nil {
		return journalHeader{}, nil, headerErr
	}
	resources, resourcesErr := readJournalResources(path, false)
	if resourcesErr != nil {
		return journalHeader{}, nil, resourcesErr
	}
	return header, resources, nil
}

func readJournalHeader(path string) (journalHeader, error) {
	file, openErr := usvc_io.OpenFileReadOnly(path)
	if openErr != nil {
		return journalHeader{}, openErr
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		if scanErr := scanner.Err(); scanErr != nil {
			return journalHeader{}, scanErr
		}
		return journalHeader{}, io.ErrUnexpectedEOF
	}

	var header journalHeader
	if decodeErr := json.Unmarshal(scanner.Bytes(), &header); decodeErr != nil {
		return journalHeader{}, fmt.Errorf("decoding journal header: %w", decodeErr)
	}
	if header.Version != journalVersion {
		return journalHeader{}, fmt.Errorf("unsupported cleanup journal version %d", header.Version)
	}
	if header.Runtime == "" {
		return journalHeader{}, fmt.Errorf("cleanup journal runtime cannot be empty")
	}
	if header.ProcessID <= 0 {
		return journalHeader{}, fmt.Errorf("cleanup journal process ID must be positive")
	}
	if header.ProcessStartTime.IsZero() {
		return journalHeader{}, fmt.Errorf("cleanup journal process start time cannot be zero")
	}
	if header.RunID == "" {
		return journalHeader{}, fmt.Errorf("cleanup journal run ID cannot be empty")
	}

	return header, nil
}

func readJournalResources(path string, allowIncompleteFinalRecord bool) ([]Resource, error) {
	file, openErr := usvc_io.OpenFileReadOnly(path)
	if openErr != nil {
		return nil, openErr
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var lines [][]byte
	for scanner.Scan() {
		lines = append(lines, bytes.Clone(scanner.Bytes()))
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return nil, scanErr
	}
	if len(lines) == 0 {
		return nil, io.ErrUnexpectedEOF
	}

	var resources []Resource
	resourceLines := lines[1:]
	for lineIndex, line := range resourceLines {
		var resource Resource
		if decodeErr := json.Unmarshal(line, &resource); decodeErr != nil {
			isFinalRecord := lineIndex == len(resourceLines)-1
			if allowIncompleteFinalRecord && isFinalRecord {
				break
			}
			return nil, fmt.Errorf("decoding journal resource: %w", decodeErr)
		}
		if !validResourceKind(resource.Kind) || resource.Identifier == "" {
			isFinalRecord := lineIndex == len(resourceLines)-1
			if allowIncompleteFinalRecord && isFinalRecord {
				break
			}
			return nil, fmt.Errorf("invalid cleanup journal resource: %+v", resource)
		}
		resources = append(resources, resource)
	}

	return resources, nil
}

func cleanupResources(
	ctx context.Context,
	orchestrator containers.ContainerOrchestrator,
	header journalHeader,
	resources []Resource,
) error {
	resources = append([]Resource(nil), resources...)
	std_slices.SortStableFunc(resources, func(left, right Resource) int {
		return resourceCleanupPriority(left.Kind) - resourceCleanupPriority(right.Kind)
	})

	var cleanupErrors error
	for resourceIndex, resource := range resources {
		resourceCtx, resourceCancel := nextCleanupContext(ctx, len(resources)-resourceIndex)
		if cleanupErr := cleanupResource(resourceCtx, orchestrator, header, resource); cleanupErr != nil {
			cleanupErrors = errors.Join(cleanupErrors, cleanupErr)
		}
		resourceCancel()
	}
	return cleanupErrors
}

func nextCleanupContext(ctx context.Context, remainingItems int) (context.Context, context.CancelFunc) {
	if remainingItems <= 0 {
		return context.WithCancel(ctx)
	}

	cleanupBudget := resourceCleanupTime
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		cleanupBudget = time.Until(deadline) / time.Duration(remainingItems)
	}

	return context.WithTimeout(ctx, cleanupBudget)
}

type cleanupResourceState uint8

const (
	cleanupResourceAbsent cleanupResourceState = iota
	cleanupResourceOwned
	cleanupResourceUnowned
)

func cleanupResource(
	ctx context.Context,
	orchestrator containers.ContainerOrchestrator,
	header journalHeader,
	resource Resource,
) error {
	resourceState, inspectErr := inspectCleanupResource(ctx, orchestrator, header, resource)
	if inspectErr != nil {
		return fmt.Errorf("inspecting %s %q before cleanup: %w", resource.Kind, resource.Identifier, inspectErr)
	}
	switch resourceState {
	case cleanupResourceAbsent:
		return nil
	case cleanupResourceUnowned:
		return fmt.Errorf(
			"refusing to remove %s %q because its ownership labels do not match the cleanup journal",
			resource.Kind,
			resource.Identifier,
		)
	case cleanupResourceOwned:
	default:
		return fmt.Errorf("unsupported cleanup state %d for %s %q", resourceState, resource.Kind, resource.Identifier)
	}

	var removeErr error
	switch resource.Kind {
	case ResourceContainer:
		_, removeErr = orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
			Containers: []string{resource.Identifier},
			Force:      true,
		})
	case ResourceNetwork:
		_, removeErr = orchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
			Networks: []string{resource.Identifier},
			Force:    true,
		})
	case ResourceVolume:
		_, removeErr = orchestrator.RemoveVolumes(ctx, containers.RemoveVolumesOptions{
			Volumes: []string{resource.Identifier},
			Force:   true,
		})
	case ResourceImage:
		_, removeErr = orchestrator.RemoveImages(ctx, containers.RemoveImagesOptions{
			Images: []string{resource.Identifier},
			Force:  true,
		})
	default:
		return fmt.Errorf("unsupported cleanup resource kind %q", resource.Kind)
	}

	absenceErr := wait.PollUntilContextCancel(ctx, resourcePollInterval, true, func(ctx context.Context) (bool, error) {
		return resourceAbsent(ctx, orchestrator, resource)
	})
	if absenceErr == nil {
		return nil
	}

	verificationErr := fmt.Errorf("verifying %s %q removal: %w", resource.Kind, resource.Identifier, absenceErr)
	if removeErr == nil {
		return verificationErr
	}
	return errors.Join(fmt.Errorf("removing %s %q: %w", resource.Kind, resource.Identifier, removeErr), verificationErr)
}

func inspectCleanupResource(
	ctx context.Context,
	orchestrator containers.ContainerOrchestrator,
	header journalHeader,
	resource Resource,
) (cleanupResourceState, error) {
	var labels map[string]string
	var count int
	var inspectErr error

	switch resource.Kind {
	case ResourceContainer:
		var inspected []containers.InspectedContainer
		inspected, inspectErr = orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
			Containers: []string{resource.Identifier},
		})
		count = len(inspected)
		if count == 1 {
			labels = inspected[0].Labels
		}
	case ResourceNetwork:
		var inspected []containers.InspectedNetwork
		inspected, inspectErr = orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
			Networks: []string{resource.Identifier},
		})
		count = len(inspected)
		if count == 1 {
			labels = inspected[0].Labels
		}
	case ResourceVolume:
		var inspected []containers.InspectedVolume
		inspected, inspectErr = orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
			Volumes: []string{resource.Identifier},
		})
		count = len(inspected)
		if count == 1 {
			labels = inspected[0].Labels
		}
	case ResourceImage:
		var inspected []containers.InspectedImage
		inspected, inspectErr = orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{resource.Identifier},
		})
		count = len(inspected)
		if count == 1 {
			labels = inspected[0].Labels
		}
	default:
		return cleanupResourceUnowned, fmt.Errorf("unsupported cleanup resource kind %q", resource.Kind)
	}

	if count == 0 && (inspectErr == nil || errors.Is(inspectErr, containers.ErrNotFound)) {
		return cleanupResourceAbsent, nil
	}
	if inspectErr != nil {
		return cleanupResourceUnowned, inspectErr
	}
	if count != 1 {
		return cleanupResourceUnowned, fmt.Errorf("runtime returned %d matching objects", count)
	}

	for key, expectedValue := range resourceOwnershipLabels(header) {
		if labels[key] != expectedValue {
			return cleanupResourceUnowned, nil
		}
	}
	return cleanupResourceOwned, nil
}

func resourceAbsent(ctx context.Context, orchestrator containers.ContainerOrchestrator, resource Resource) (bool, error) {
	var count int
	var inspectErr error

	switch resource.Kind {
	case ResourceContainer:
		var inspected []containers.InspectedContainer
		inspected, inspectErr = orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
			Containers: []string{resource.Identifier},
		})
		count = len(inspected)
	case ResourceNetwork:
		var inspected []containers.InspectedNetwork
		inspected, inspectErr = orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
			Networks: []string{resource.Identifier},
		})
		count = len(inspected)
	case ResourceVolume:
		var inspected []containers.InspectedVolume
		inspected, inspectErr = orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
			Volumes: []string{resource.Identifier},
		})
		count = len(inspected)
	case ResourceImage:
		var inspected []containers.InspectedImage
		inspected, inspectErr = orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{resource.Identifier},
		})
		count = len(inspected)
	default:
		return false, fmt.Errorf("unsupported cleanup resource kind %q", resource.Kind)
	}

	if count > 0 {
		return false, nil
	}
	if inspectErr == nil || errors.Is(inspectErr, containers.ErrNotFound) {
		return true, nil
	}
	return false, inspectErr
}

func validResourceKind(kind ResourceKind) bool {
	switch kind {
	case ResourceContainer, ResourceNetwork, ResourceVolume, ResourceImage:
		return true
	default:
		return false
	}
}

func resourceCleanupPriority(kind ResourceKind) int {
	switch kind {
	case ResourceContainer:
		return 0
	case ResourceNetwork:
		return 1
	case ResourceVolume:
		return 2
	case ResourceImage:
		return 3
	default:
		return 4
	}
}
