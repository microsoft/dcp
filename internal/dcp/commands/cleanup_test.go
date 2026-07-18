/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/exerunners"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	dcpio "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

const cleanupTestRuntimeName = "test"

func TestCleanupWorkloadResourcesNoRecordsDoesNotRequireContainerRuntime(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()

	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func(string) (containers.ContainerOrchestrator, error) {
			require.Fail(t, "container runtime should not be requested when there are no container or network records")
			return nil, nil
		},
		processExecutor,
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, commonapi.WorkloadID("workload-a"), report.WorkloadID)
	require.Equal(t, cleanupStoppedCounts{}, report.Stopped)
	require.Empty(t, report.Failures)
}

func TestCleanupRejectsTooLongWorkloadID(t *testing.T) {
	t.Parallel()

	cleanupErr := cleanup(logr.Discard())(
		&cobra.Command{},
		[]string{strings.Repeat("a", commonapi.MaxWorkloadIDLength+1)},
	)

	require.ErrorContains(t, cleanupErr, "workload ID cannot be longer than")
}

func TestCleanupWorkloadResourcesRemovesContainersAndNetworks(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)

	containerID, createContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: "api",
		ContainerSpec: apiv1.ContainerSpec{
			Image: "test-image",
		},
	})
	require.NoError(t, createContainerErr)
	networkID, createNetworkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: "app-network"})
	require.NoError(t, createNetworkErr)

	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
		ResourceKey:   "containers/api",
		ContainerID:   containerID,
		ContainerName: "api",
		RuntimeName:   cleanupTestRuntimeName,
		WorkloadID:    "workload-a",
	}))
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/app-network",
		NetworkID:   networkID,
		NetworkName: "app-network",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-a",
	}))

	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func(runtimeName string) (containers.ContainerOrchestrator, error) {
			require.Equal(t, cleanupTestRuntimeName, runtimeName)
			return orchestrator, nil
		},
		processExecutor,
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Containers: 1, Networks: 1}, report.Stopped)
	require.Empty(t, report.Failures)
	_, inspectContainerErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
	require.ErrorIs(t, inspectContainerErr, containers.ErrNotFound)
	_, inspectNetworkErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})
	require.ErrorIs(t, inspectNetworkErr, containers.ErrNotFound)

	containerRecords, listContainerErr := stateStore.ListPersistentContainersByWorkloadID(ctx, "workload-a")
	require.NoError(t, listContainerErr)
	require.Empty(t, containerRecords)
	networkRecords, listNetworkErr := stateStore.ListPersistentNetworksByWorkloadID(ctx, "workload-a")
	require.NoError(t, listNetworkErr)
	require.Empty(t, networkRecords)
}

func TestCleanupWorkloadResourcesRemovesContainersBeforeNetworks(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)
	wrappedOrchestrator := &orderedCleanupContainerOrchestrator{
		ContainerOrchestrator:   orchestrator,
		removeContainersEntered: make(chan struct{}),
		allowRemoveContainers:   make(chan struct{}),
		removeNetworksEntered:   make(chan struct{}),
	}

	networkID, createNetworkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: "app-network"})
	require.NoError(t, createNetworkErr)
	containerID, createContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: "api",
		Networks: []containers.CreateContainerNetworkOptions{
			{Name: "app-network"},
		},
	})
	require.NoError(t, createContainerErr)
	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
		ResourceKey:   "containers/api",
		ContainerID:   containerID,
		ContainerName: "api",
		RuntimeName:   cleanupTestRuntimeName,
		WorkloadID:    "workload-a",
	}))
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/app-network",
		NetworkID:   networkID,
		NetworkName: "app-network",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-a",
	}))

	type cleanupResult struct {
		report cleanupReport
		err    error
	}
	resultCh := make(chan cleanupResult, 1)
	go func() {
		report, cleanupErr := cleanupWorkloadResources(
			ctx,
			"workload-a",
			stateStore,
			leaseOwner,
			func(runtimeName string) (containers.ContainerOrchestrator, error) {
				if runtimeName != cleanupTestRuntimeName {
					return nil, errors.New("unexpected runtime name")
				}
				return wrappedOrchestrator, nil
			},
			processExecutor,
			logr.Discard(),
		)
		resultCh <- cleanupResult{report: report, err: cleanupErr}
	}()

	select {
	case <-wrappedOrchestrator.removeContainersEntered:
	case <-ctx.Done():
		require.FailNow(t, "container cleanup did not start", ctx.Err())
	}
	networkStartedEarlyTimer := time.NewTimer(100 * time.Millisecond)
	defer networkStartedEarlyTimer.Stop()
	select {
	case <-wrappedOrchestrator.removeNetworksEntered:
		close(wrappedOrchestrator.allowRemoveContainers)
		select {
		case <-resultCh:
		case <-ctx.Done():
			require.FailNow(t, "cleanup did not finish", ctx.Err())
		}
		require.FailNow(t, "network cleanup started before container cleanup finished")
	case <-networkStartedEarlyTimer.C:
	}
	close(wrappedOrchestrator.allowRemoveContainers)

	var result cleanupResult
	select {
	case result = <-resultCh:
	case <-ctx.Done():
		require.FailNow(t, "cleanup did not finish", ctx.Err())
	}
	require.NoError(t, result.err)
	require.Equal(t, cleanupStoppedCounts{Containers: 1, Networks: 1}, result.report.Stopped)
	require.Empty(t, result.report.Failures)
}

func TestCleanupWorkloadResourcesTreatsMissingRuntimeResourcesAsSuccess(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)

	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
		ResourceKey: "containers/missing",
		ContainerID: "missing-container",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-a",
	}))
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/missing",
		NetworkID:   "missing-network",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-a",
	}))

	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func(runtimeName string) (containers.ContainerOrchestrator, error) {
			require.Equal(t, cleanupTestRuntimeName, runtimeName)
			return orchestrator, nil
		},
		processExecutor,
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Containers: 1, Networks: 1}, report.Stopped)
	require.Empty(t, report.Failures)
}

func TestCleanupWorkloadResourcesRunsIndependentRecordsInParallel(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)
	wrappedOrchestrator := &parallelCleanupContainerOrchestrator{
		ContainerOrchestrator: orchestrator,
		firstRemoveEntered:    make(chan struct{}),
		secondRemoveEntered:   make(chan struct{}),
		releaseFirstRemove:    make(chan struct{}),
	}

	firstContainerID, createFirstErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{Name: "api"})
	require.NoError(t, createFirstErr)
	secondContainerID, createSecondErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{Name: "worker"})
	require.NoError(t, createSecondErr)
	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
		ResourceKey:   "containers/api",
		ContainerID:   firstContainerID,
		ContainerName: "api",
		RuntimeName:   cleanupTestRuntimeName,
		WorkloadID:    "workload-a",
	}))
	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
		ResourceKey:   "containers/worker",
		ContainerID:   secondContainerID,
		ContainerName: "worker",
		RuntimeName:   cleanupTestRuntimeName,
		WorkloadID:    "workload-a",
	}))

	type cleanupResult struct {
		report cleanupReport
		err    error
	}
	resultCh := make(chan cleanupResult, 1)
	go func() {
		report, cleanupErr := cleanupWorkloadResources(
			ctx,
			"workload-a",
			stateStore,
			leaseOwner,
			func(runtimeName string) (containers.ContainerOrchestrator, error) {
				if runtimeName != cleanupTestRuntimeName {
					return nil, errors.New("unexpected runtime name")
				}
				return wrappedOrchestrator, nil
			},
			processExecutor,
			logr.Discard(),
		)
		resultCh <- cleanupResult{report: report, err: cleanupErr}
	}()

	select {
	case <-wrappedOrchestrator.firstRemoveEntered:
	case <-ctx.Done():
		require.FailNow(t, "first container cleanup did not start", ctx.Err())
	}
	select {
	case <-wrappedOrchestrator.secondRemoveEntered:
	case <-ctx.Done():
		require.FailNow(t, "second container cleanup did not start while the first cleanup was blocked", ctx.Err())
	}
	close(wrappedOrchestrator.releaseFirstRemove)

	var result cleanupResult
	select {
	case result = <-resultCh:
	case <-ctx.Done():
		require.FailNow(t, "cleanup did not finish", ctx.Err())
	}
	require.NoError(t, result.err)
	require.Equal(t, cleanupStoppedCounts{Containers: 2}, result.report.Stopped)
	require.Empty(t, result.report.Failures)
}

func TestCleanupResourceGroupsUnblocksDependentsWhenOnlyTheirPrerequisitesFinish(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()
	containerStarted := make(chan struct{})
	releaseContainer := make(chan struct{})
	executableStarted := make(chan struct{})
	releaseExecutable := make(chan struct{})
	networkStarted := make(chan struct{})

	type cleanupResult struct {
		results []cleanupWorkResult
		err     error
	}
	resultCh := make(chan cleanupResult, 1)
	go func() {
		results, cleanupErr := cleanupResourceGroups([]cleanupResourceGroup{
			{
				gvr: cleanupResourceContainerGVR,
				workItems: []cleanupWorkItem{
					{
						gvr: cleanupResourceContainerGVR,
						clean: func() (string, bool, error) {
							close(containerStarted)
							select {
							case <-releaseContainer:
							case <-ctx.Done():
								return "", false, ctx.Err()
							}
							return "container-id", true, nil
						},
					},
				},
			},
			{
				gvr: cleanupResourceExecutableGVR,
				workItems: []cleanupWorkItem{
					{
						gvr: cleanupResourceExecutableGVR,
						clean: func() (string, bool, error) {
							close(executableStarted)
							select {
							case <-releaseExecutable:
							case <-ctx.Done():
								return "", false, ctx.Err()
							}
							return "executable-id", true, nil
						},
					},
				},
			},
			{
				gvr:          cleanupResourceNetworkGVR,
				cleanUpAfter: []schema.GroupVersionResource{cleanupResourceContainerGVR},
				workItems: []cleanupWorkItem{
					{
						gvr: cleanupResourceNetworkGVR,
						clean: func() (string, bool, error) {
							close(networkStarted)
							return "network-id", true, nil
						},
					},
				},
			},
		})
		resultCh <- cleanupResult{results: results, err: cleanupErr}
	}()

	select {
	case <-containerStarted:
	case <-ctx.Done():
		require.FailNow(t, "container cleanup did not start", ctx.Err())
	}
	select {
	case <-executableStarted:
	case <-ctx.Done():
		require.FailNow(t, "executable cleanup did not start", ctx.Err())
	}
	close(releaseContainer)
	select {
	case <-networkStarted:
	case <-ctx.Done():
		require.FailNow(t, "network cleanup did not start after container cleanup finished", ctx.Err())
	}
	close(releaseExecutable)

	var result cleanupResult
	select {
	case result = <-resultCh:
	case <-ctx.Done():
		require.FailNow(t, "cleanup did not finish", ctx.Err())
	}
	require.NoError(t, result.err)
	require.Len(t, result.results, 3)
}

func TestRunCleanupResourceGroupsReportsCompletedWorkBeforeDependencyError(t *testing.T) {
	t.Parallel()

	report := cleanupReport{WorkloadID: "workload-a"}
	cleanupItemErr := errors.New("cleanup item failed")
	cleanupErr := runCleanupResourceGroups(&report, []cleanupResourceGroup{
		{
			gvr: cleanupResourceContainerGVR,
			workItems: []cleanupWorkItem{
				{
					gvr: cleanupResourceContainerGVR,
					clean: func() (string, bool, error) {
						return "container-id", true, nil
					},
				},
			},
		},
		{
			gvr: cleanupResourceExecutableGVR,
			workItems: []cleanupWorkItem{
				{
					gvr:                cleanupResourceExecutableGVR,
					resourceKey:        "executables/api",
					fallbackResourceID: "123",
					clean: func() (string, bool, error) {
						return "", false, cleanupItemErr
					},
				},
			},
		},
		{
			gvr:          cleanupResourceNetworkGVR,
			cleanUpAfter: []schema.GroupVersionResource{{Resource: "missing"}},
			workItems: []cleanupWorkItem{
				{
					gvr: cleanupResourceNetworkGVR,
					clean: func() (string, bool, error) {
						return "network-id", true, nil
					},
				},
			},
		},
	})

	require.Error(t, cleanupErr)
	require.ErrorContains(t, cleanupErr, "could not resolve cleanup resource dependencies")
	require.ErrorContains(t, cleanupErr, "failed to clean up 1 persistent resource")
	require.Equal(t, cleanupStoppedCounts{Containers: 1}, report.Stopped)
	require.Len(t, report.Failures, 1)
	require.Equal(t, cleanupResourceName(cleanupResourceExecutableGVR), report.Failures[0].Kind)
	require.Equal(t, "executables/api", report.Failures[0].ResourceKey)
	require.Equal(t, "123", report.Failures[0].ResourceID)
	require.Equal(t, cleanupItemErr.Error(), report.Failures[0].Error)
}

func TestCleanupPersistentContainerRecordSkipsRecordThatChangedWorkload(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)

	oldContainerID, createOldContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{Name: "old-api"})
	require.NoError(t, createOldContainerErr)
	newContainerID, createNewContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{Name: "new-api"})
	require.NoError(t, createNewContainerErr)

	staleRecord := statestore.PersistentContainerRecord{
		ResourceKey:   "containers/api",
		ContainerID:   oldContainerID,
		ContainerName: "old-api",
		RuntimeName:   cleanupTestRuntimeName,
		WorkloadID:    "workload-a",
	}
	currentRecord := statestore.PersistentContainerRecord{
		ResourceKey:   staleRecord.ResourceKey,
		ContainerID:   newContainerID,
		ContainerName: "new-api",
		RuntimeName:   cleanupTestRuntimeName,
		WorkloadID:    "workload-b",
	}
	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, currentRecord))

	resourceID, cleaned, cleanupErr := cleanupPersistentContainerRecord(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func(string) (containers.ContainerOrchestrator, error) {
			require.Fail(t, "container runtime should not be requested for stale container records")
			return orchestrator, nil
		},
		staleRecord,
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Empty(t, resourceID)
	require.False(t, cleaned)
	_, inspectOldErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{oldContainerID}})
	require.NoError(t, inspectOldErr)
	_, inspectNewErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{newContainerID}})
	require.NoError(t, inspectNewErr)
	record, getErr := stateStore.GetPersistentContainer(ctx, "containers/api")
	require.NoError(t, getErr)
	require.Equal(t, currentRecord.WorkloadID, record.WorkloadID)
	require.Equal(t, currentRecord.ContainerID, record.ContainerID)
}

func TestCleanupPersistentContainerRecordWaitsForHeldLease(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	heldLeaseOwner, heldLeaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, heldLeaseOwnerErr)
	cleanupLeaseOwner := process.ProcessHandle{
		Pid:          heldLeaseOwner.Pid,
		IdentityTime: heldLeaseOwner.IdentityTime.Add(-time.Hour),
	}
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)

	containerID, createContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{Name: "api"})
	require.NoError(t, createContainerErr)
	record := statestore.PersistentContainerRecord{
		ResourceKey:   "containers/api",
		ContainerID:   containerID,
		ContainerName: "api",
		RuntimeName:   cleanupTestRuntimeName,
		WorkloadID:    "workload-a",
	}
	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, record))
	_, leaseErr := stateStore.AcquireResourceLease(ctx, cleanupLeaseResource(record.ResourceKey), heldLeaseOwner, time.Minute)
	require.NoError(t, leaseErr)

	type cleanupResult struct {
		resourceID string
		cleaned    bool
		err        error
	}
	resultCh := make(chan cleanupResult, 1)
	go func() {
		resourceID, cleaned, cleanupErr := cleanupPersistentContainerRecord(
			ctx,
			"workload-a",
			stateStore,
			cleanupLeaseOwner,
			func(runtimeName string) (containers.ContainerOrchestrator, error) {
				if runtimeName != cleanupTestRuntimeName {
					return nil, errors.New("unexpected runtime name")
				}
				return orchestrator, nil
			},
			record,
			logr.Discard(),
		)
		resultCh <- cleanupResult{resourceID: resourceID, cleaned: cleaned, err: cleanupErr}
	}()

	select {
	case result := <-resultCh:
		require.FailNow(t, "cleanup finished before the held lease was released", result.err)
	default:
	}

	require.NoError(t, stateStore.ReleaseResourceLease(ctx, cleanupLeaseResource(record.ResourceKey), heldLeaseOwner))

	var result cleanupResult
	require.NoError(t, resiliency.RetryExponential(ctx, func() error {
		select {
		case result = <-resultCh:
			return nil
		default:
			return errors.New("cleanup did not finish after the held lease was released")
		}
	}))
	require.NoError(t, result.err)
	require.Equal(t, containerID, result.resourceID)
	require.True(t, result.cleaned)
	_, getErr := stateStore.GetPersistentContainer(ctx, record.ResourceKey)
	require.ErrorIs(t, getErr, statestore.ErrPersistentContainerNotFound)
	_, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
}

func TestCleanupWorkloadResourcesTreatsMissingProcessAsSuccess(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()

	require.NoError(t, stateStore.UpsertPersistentProcess(ctx, statestore.PersistentProcessRecord{
		ResourceKey:  "default/missing",
		LifecycleKey: "missing-lifecycle",
		PID:          process.Pid_t(999999),
		IdentityTime: time.Unix(1, 0).UTC(),
		RunID:        "missing-run",
		WorkloadID:   "workload-a",
	}))

	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func(string) (containers.ContainerOrchestrator, error) {
			require.Fail(t, "container runtime should not be requested for executable-only cleanup")
			return nil, nil
		},
		processExecutor,
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Executables: 1}, report.Stopped)
	require.Empty(t, report.Failures)
	records, listErr := stateStore.ListPersistentProcessesByWorkloadID(ctx, "workload-a")
	require.NoError(t, listErr)
	require.Empty(t, records)
}

func TestCleanupPersistentProcessRecordPreservesLogFiles(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)

	stdoutPath := filepath.Join(t.TempDir(), "stdout.log")
	stderrPath := filepath.Join(t.TempDir(), "stderr.log")
	require.NoError(t, dcpio.WriteFile(stdoutPath, []byte("stdout"), 0o600))
	require.NoError(t, dcpio.WriteFile(stderrPath, []byte("stderr"), 0o600))
	record := statestore.PersistentProcessRecord{
		ResourceKey:  "default/missing",
		LifecycleKey: "missing-lifecycle",
		PID:          process.Pid_t(999999),
		IdentityTime: time.Unix(1, 0).UTC(),
		RunID:        "missing-run",
		StdOutFile:   stdoutPath,
		StdErrFile:   stderrPath,
		WorkloadID:   "workload-a",
	}
	require.NoError(t, stateStore.UpsertPersistentProcess(ctx, record))

	resourceID, cleaned, cleanupErr := cleanupPersistentProcessRecord(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		&fakePersistentProcessCleanupRunner{
			checkErr: &process.ErrProcessNotFound{Pid: record.PID},
		},
		record,
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, "999999", resourceID)
	require.True(t, cleaned)
	_, getErr := stateStore.GetPersistentProcess(ctx, record.ResourceKey)
	require.ErrorIs(t, getErr, statestore.ErrPersistentProcessNotFound)
	require.FileExists(t, stdoutPath)
	require.FileExists(t, stderrPath)
}

func TestCleanupWorkloadResourcesRuntimeFailureDoesNotPreventExecutableCleanup(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()

	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
		ResourceKey: "containers/api",
		ContainerID: "api-container",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-a",
	}))
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/api",
		NetworkID:   "api-network",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-a",
	}))
	require.NoError(t, stateStore.UpsertPersistentProcess(ctx, statestore.PersistentProcessRecord{
		ResourceKey:  "default/api",
		LifecycleKey: "api-lifecycle",
		PID:          process.Pid_t(999999),
		IdentityTime: time.Unix(1, 0).UTC(),
		RunID:        "api-run",
		WorkloadID:   "workload-a",
	}))

	runtimeErr := errors.New("container runtime unavailable")
	runtimeRequests := 0
	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func(runtimeName string) (containers.ContainerOrchestrator, error) {
			require.Equal(t, cleanupTestRuntimeName, runtimeName)
			runtimeRequests++
			return nil, runtimeErr
		},
		processExecutor,
		logr.Discard(),
	)

	require.Error(t, cleanupErr)
	require.Equal(t, 1, runtimeRequests)
	require.Equal(t, cleanupStoppedCounts{Executables: 1}, report.Stopped)
	require.Len(t, report.Failures, 2)
	require.Equal(t, cleanupResourceName(cleanupResourceContainerGVR), report.Failures[0].Kind)
	require.Equal(t, "containers/api", report.Failures[0].ResourceKey)
	require.Equal(t, "api-container", report.Failures[0].ResourceID)
	require.ErrorContains(t, errors.New(report.Failures[0].Error), runtimeErr.Error())
	require.Equal(t, cleanupResourceName(cleanupResourceNetworkGVR), report.Failures[1].Kind)
	require.Equal(t, "containernetworks/api", report.Failures[1].ResourceKey)
	require.Equal(t, "api-network", report.Failures[1].ResourceID)
	require.ErrorContains(t, errors.New(report.Failures[1].Error), runtimeErr.Error())
	processRecords, listProcessErr := stateStore.ListPersistentProcessesByWorkloadID(ctx, "workload-a")
	require.NoError(t, listProcessErr)
	require.Empty(t, processRecords)
	containerRecords, listContainerErr := stateStore.ListPersistentContainersByWorkloadID(ctx, "workload-a")
	require.NoError(t, listContainerErr)
	require.Len(t, containerRecords, 1)
	networkRecords, listNetworkErr := stateStore.ListPersistentNetworksByWorkloadID(ctx, "workload-a")
	require.NoError(t, listNetworkErr)
	require.Len(t, networkRecords, 1)
}

func TestCleanupPersistentContainerAndNetworkRecordsSkipRecordsThatChangedWorkloadBeforeRuntimeResolution(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)

	staleContainerRecord := statestore.PersistentContainerRecord{
		ResourceKey: "containers/api",
		ContainerID: "old-api-container",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-a",
	}
	currentContainerRecord := statestore.PersistentContainerRecord{
		ResourceKey: staleContainerRecord.ResourceKey,
		ContainerID: "new-api-container",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-b",
	}
	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, currentContainerRecord))
	staleNetworkRecord := statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/api",
		NetworkID:   "old-api-network",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-a",
	}
	currentNetworkRecord := statestore.PersistentNetworkRecord{
		ResourceKey: staleNetworkRecord.ResourceKey,
		NetworkID:   "new-api-network",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-b",
	}
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, currentNetworkRecord))

	runtimeErr := errors.New("container runtime unavailable")
	getContainerOrchestrator := func(string) (containers.ContainerOrchestrator, error) {
		require.Fail(t, "container runtime should not be requested for stale container or network records")
		return nil, runtimeErr
	}
	containerResourceID, containerCleaned, containerCleanupErr := cleanupPersistentContainerRecord(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		getContainerOrchestrator,
		staleContainerRecord,
		logr.Discard(),
	)
	networkResourceID, networkCleaned, networkCleanupErr := cleanupPersistentNetworkRecord(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		getContainerOrchestrator,
		staleNetworkRecord,
		logr.Discard(),
	)

	require.NoError(t, containerCleanupErr)
	require.Empty(t, containerResourceID)
	require.False(t, containerCleaned)
	require.NoError(t, networkCleanupErr)
	require.Empty(t, networkResourceID)
	require.False(t, networkCleaned)
	containerRecords, listContainerErr := stateStore.ListPersistentContainersByWorkloadID(ctx, "workload-b")
	require.NoError(t, listContainerErr)
	require.Len(t, containerRecords, 1)
	require.Equal(t, currentContainerRecord.ContainerID, containerRecords[0].ContainerID)
	networkRecords, listNetworkErr := stateStore.ListPersistentNetworksByWorkloadID(ctx, "workload-b")
	require.NoError(t, listNetworkErr)
	require.Len(t, networkRecords, 1)
	require.Equal(t, currentNetworkRecord.NetworkID, networkRecords[0].NetworkID)
}

func TestCleanupPersistentProcessRecordSkipsRecordThatChangedWorkload(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()

	staleRecord := statestore.PersistentProcessRecord{
		ResourceKey:  "default/api",
		LifecycleKey: "old-lifecycle",
		PID:          process.Pid_t(999999),
		IdentityTime: time.Unix(1, 0).UTC(),
		RunID:        "old-run",
		WorkloadID:   "workload-a",
	}
	currentRecord := statestore.PersistentProcessRecord{
		ResourceKey:  staleRecord.ResourceKey,
		LifecycleKey: "new-lifecycle",
		PID:          process.Pid_t(999998),
		IdentityTime: time.Unix(2, 0).UTC(),
		RunID:        "new-run",
		WorkloadID:   "workload-b",
	}
	require.NoError(t, stateStore.UpsertPersistentProcess(ctx, currentRecord))

	resourceID, cleaned, cleanupErr := cleanupPersistentProcessRecord(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		exerunners.NewProcessExecutableRunner(processExecutor),
		staleRecord,
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Empty(t, resourceID)
	require.False(t, cleaned)
	actual, getErr := stateStore.GetPersistentProcess(ctx, currentRecord.ResourceKey)
	require.NoError(t, getErr)
	require.Equal(t, currentRecord.WorkloadID, actual.WorkloadID)
	require.Equal(t, currentRecord.PID, actual.PID)
}

func TestCleanupPersistentProcessRecordReportsLivenessCheckFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)

	record := statestore.PersistentProcessRecord{
		ResourceKey:  "default/api",
		LifecycleKey: "api-lifecycle",
		PID:          process.Pid_t(1234),
		IdentityTime: time.Unix(1, 0).UTC(),
		RunID:        "api-run",
		WorkloadID:   "workload-a",
	}
	require.NoError(t, stateStore.UpsertPersistentProcess(ctx, record))

	checkErr := errors.New("process table unavailable")
	runner := &fakePersistentProcessCleanupRunner{checkErr: checkErr}
	resourceID, _, cleanupErr := cleanupPersistentProcessRecord(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		runner,
		record,
		logr.Discard(),
	)

	require.ErrorIs(t, cleanupErr, checkErr)
	require.Equal(t, "1234", resourceID)
	require.False(t, runner.stopCalled)
	actual, getErr := stateStore.GetPersistentProcess(ctx, record.ResourceKey)
	require.NoError(t, getErr)
	require.Equal(t, record.WorkloadID, actual.WorkloadID)
	require.Equal(t, record.PID, actual.PID)
}

func TestCleanupPersistentProcessRecordTreatsStopGoneErrorAsSuccess(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)

	record := statestore.PersistentProcessRecord{
		ResourceKey:  "default/api",
		LifecycleKey: "api-lifecycle",
		PID:          process.Pid_t(1234),
		IdentityTime: time.Unix(1, 0).UTC(),
		RunID:        "api-run",
		WorkloadID:   "workload-a",
	}
	require.NoError(t, stateStore.UpsertPersistentProcess(ctx, record))

	runner := &fakePersistentProcessCleanupRunner{
		stopErr: &process.ErrProcessNotFound{Pid: record.PID},
	}
	resourceID, cleaned, cleanupErr := cleanupPersistentProcessRecord(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		runner,
		record,
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, "1234", resourceID)
	require.True(t, cleaned)
	require.True(t, runner.stopCalled)
	_, getErr := stateStore.GetPersistentProcess(ctx, record.ResourceKey)
	require.ErrorIs(t, getErr, statestore.ErrPersistentProcessNotFound)
}

func TestCleanupWorkloadResourcesReportsFailuresAndContinues(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)
	removeContainerErr := errors.New("container removal failed")
	wrappedOrchestrator := &failingCleanupContainerOrchestrator{
		ContainerOrchestrator: orchestrator,
		removeContainersErr:   removeContainerErr,
	}

	containerID, createContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{Name: "invalid"})
	require.NoError(t, createContainerErr)
	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
		ResourceKey:   "containers/invalid",
		ContainerID:   containerID,
		ContainerName: "invalid",
		RuntimeName:   cleanupTestRuntimeName,
		WorkloadID:    "workload-a",
	}))
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/missing",
		NetworkID:   "missing-network",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-a",
	}))

	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func(runtimeName string) (containers.ContainerOrchestrator, error) {
			require.Equal(t, cleanupTestRuntimeName, runtimeName)
			return wrappedOrchestrator, nil
		},
		processExecutor,
		logr.Discard(),
	)

	require.Error(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Networks: 1}, report.Stopped)
	require.Len(t, report.Failures, 1)
	require.Equal(t, cleanupResourceName(cleanupResourceContainerGVR), report.Failures[0].Kind)
	require.Equal(t, "containers/invalid", report.Failures[0].ResourceKey)
	require.Equal(t, containerID, report.Failures[0].ResourceID)
	require.ErrorContains(t, errors.New(report.Failures[0].Error), removeContainerErr.Error())
}

func openCleanupTestStore(t *testing.T, ctx context.Context) *statestore.Store {
	t.Helper()

	stateStorePath := filepath.Join(t.TempDir(), "state-store", "state.sqlite3")
	stateStore, openErr := statestore.Open(ctx, statestore.Options{
		Path:        stateStorePath,
		BusyTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, openErr)
	t.Cleanup(func() {
		require.NoError(t, stateStore.Close())
	})
	return stateStore
}

type fakePersistentProcessCleanupRunner struct {
	checkErr   error
	stopErr    error
	stopCalled bool
}

func (r *fakePersistentProcessCleanupRunner) CheckProcessRunning(process.ProcessHandle) error {
	return r.checkErr
}

func (r *fakePersistentProcessCleanupRunner) StopPersistentProcess(context.Context, *apiv1.Executable, *statestore.PersistentProcessRecord, logr.Logger) error {
	r.stopCalled = true
	return r.stopErr
}

type parallelCleanupContainerOrchestrator struct {
	containers.ContainerOrchestrator

	lock                sync.Mutex
	removeCount         int
	firstRemoveEntered  chan struct{}
	secondRemoveEntered chan struct{}
	releaseFirstRemove  chan struct{}
}

func (o *parallelCleanupContainerOrchestrator) RemoveContainers(ctx context.Context, options containers.RemoveContainersOptions) ([]string, error) {
	o.lock.Lock()
	o.removeCount++
	removeNumber := o.removeCount
	switch removeNumber {
	case 1:
		close(o.firstRemoveEntered)
	case 2:
		close(o.secondRemoveEntered)
	}
	o.lock.Unlock()

	if removeNumber == 1 {
		select {
		case <-o.releaseFirstRemove:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return o.ContainerOrchestrator.RemoveContainers(ctx, options)
}

type orderedCleanupContainerOrchestrator struct {
	containers.ContainerOrchestrator

	lock                    sync.Mutex
	removeContainersDone    bool
	removeContainersOnce    sync.Once
	removeNetworksOnce      sync.Once
	removeContainersEntered chan struct{}
	allowRemoveContainers   chan struct{}
	removeNetworksEntered   chan struct{}
}

func (o *orderedCleanupContainerOrchestrator) RemoveContainers(ctx context.Context, options containers.RemoveContainersOptions) ([]string, error) {
	o.removeContainersOnce.Do(func() {
		close(o.removeContainersEntered)
	})
	select {
	case <-o.allowRemoveContainers:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	ids, removeErr := o.ContainerOrchestrator.RemoveContainers(ctx, options)
	o.lock.Lock()
	o.removeContainersDone = true
	o.lock.Unlock()
	return ids, removeErr
}

func (o *orderedCleanupContainerOrchestrator) RemoveNetworks(ctx context.Context, options containers.RemoveNetworksOptions) ([]string, error) {
	o.removeNetworksOnce.Do(func() {
		close(o.removeNetworksEntered)
	})
	o.lock.Lock()
	removeContainersDone := o.removeContainersDone
	o.lock.Unlock()
	if !removeContainersDone {
		return nil, errors.New("network cleanup started before container cleanup finished")
	}

	return o.ContainerOrchestrator.RemoveNetworks(ctx, options)
}

type failingCleanupContainerOrchestrator struct {
	containers.ContainerOrchestrator

	removeContainersErr error
}

func (o *failingCleanupContainerOrchestrator) RemoveContainers(context.Context, containers.RemoveContainersOptions) ([]string, error) {
	return nil, o.removeContainersErr
}
