/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/exerunners"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

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
		func() (containers.ContainerOrchestrator, error) {
			require.Fail(t, "container runtime should not be requested when there are no container or network records")
			return nil, nil
		},
		processExecutor,
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, "workload-a", report.WorkloadID)
	require.Equal(t, cleanupStoppedCounts{}, report.Stopped)
	require.Empty(t, report.Failures)
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
		WorkloadID:    "workload-a",
	}))
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/app-network",
		NetworkID:   networkID,
		NetworkName: "app-network",
		WorkloadID:  "workload-a",
	}))

	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func() (containers.ContainerOrchestrator, error) {
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
		WorkloadID:  "workload-a",
	}))
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/missing",
		NetworkID:   "missing-network",
		WorkloadID:  "workload-a",
	}))

	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func() (containers.ContainerOrchestrator, error) {
			return orchestrator, nil
		},
		processExecutor,
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Containers: 1, Networks: 1}, report.Stopped)
	require.Empty(t, report.Failures)
}

func TestCleanupWorkloadResourcesSkipsContainerRecordThatChangesWorkloadBeforeLease(t *testing.T) {
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

	oldContainerID, createOldContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{Name: "old-api"})
	require.NoError(t, createOldContainerErr)
	newContainerID, createNewContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{Name: "new-api"})
	require.NoError(t, createNewContainerErr)

	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
		ResourceKey:   "containers/api",
		ContainerID:   oldContainerID,
		ContainerName: "old-api",
		WorkloadID:    "workload-a",
	}))

	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func() (containers.ContainerOrchestrator, error) {
			require.NoError(t, stateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
				ResourceKey:   "containers/api",
				ContainerID:   newContainerID,
				ContainerName: "new-api",
				WorkloadID:    "workload-b",
			}))
			return orchestrator, nil
		},
		processExecutor,
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{}, report.Stopped)
	require.Empty(t, report.Failures)
	_, inspectOldErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{oldContainerID}})
	require.NoError(t, inspectOldErr)
	_, inspectNewErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{newContainerID}})
	require.NoError(t, inspectNewErr)
	record, getErr := stateStore.GetPersistentContainer(ctx, "containers/api")
	require.NoError(t, getErr)
	require.Equal(t, "workload-b", record.WorkloadID)
	require.Equal(t, newContainerID, record.ContainerID)
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
		func() (containers.ContainerOrchestrator, error) {
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
		WorkloadID:  "workload-a",
	}))
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/api",
		NetworkID:   "api-network",
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
	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func() (containers.ContainerOrchestrator, error) {
			return nil, runtimeErr
		},
		processExecutor,
		logr.Discard(),
	)

	require.Error(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Executables: 1}, report.Stopped)
	require.Len(t, report.Failures, 2)
	require.Equal(t, "container", report.Failures[0].Kind)
	require.Equal(t, "containers/api", report.Failures[0].ResourceKey)
	require.Equal(t, "api-container", report.Failures[0].ResourceID)
	require.Equal(t, runtimeErr.Error(), report.Failures[0].Error)
	require.Equal(t, "network", report.Failures[1].Kind)
	require.Equal(t, "containernetworks/api", report.Failures[1].ResourceKey)
	require.Equal(t, "api-network", report.Failures[1].ResourceID)
	require.Equal(t, runtimeErr.Error(), report.Failures[1].Error)
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

func TestCleanupWorkloadResourcesRuntimeFailureSkipsContainerAndNetworkRecordsThatChangedWorkload(t *testing.T) {
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
		ContainerID: "old-api-container",
		WorkloadID:  "workload-a",
	}))
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/api",
		NetworkID:   "old-api-network",
		WorkloadID:  "workload-a",
	}))

	runtimeErr := errors.New("container runtime unavailable")
	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func() (containers.ContainerOrchestrator, error) {
			require.NoError(t, stateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
				ResourceKey: "containers/api",
				ContainerID: "new-api-container",
				WorkloadID:  "workload-b",
			}))
			require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
				ResourceKey: "containernetworks/api",
				NetworkID:   "new-api-network",
				WorkloadID:  "workload-b",
			}))
			return nil, runtimeErr
		},
		processExecutor,
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{}, report.Stopped)
	require.Empty(t, report.Failures)
	containerRecords, listContainerErr := stateStore.ListPersistentContainersByWorkloadID(ctx, "workload-b")
	require.NoError(t, listContainerErr)
	require.Len(t, containerRecords, 1)
	require.Equal(t, "new-api-container", containerRecords[0].ContainerID)
	networkRecords, listNetworkErr := stateStore.ListPersistentNetworksByWorkloadID(ctx, "workload-b")
	require.NoError(t, listNetworkErr)
	require.Len(t, networkRecords, 1)
	require.Equal(t, "new-api-network", networkRecords[0].NetworkID)
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
	heldLeaseOwner, heldLeaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, heldLeaseOwnerErr)
	cleanupLeaseOwner := process.ProcessHandle{
		Pid:          heldLeaseOwner.Pid,
		IdentityTime: heldLeaseOwner.IdentityTime.Add(-time.Hour),
	}
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)

	containerID, createContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{Name: "blocked"})
	require.NoError(t, createContainerErr)
	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
		ResourceKey:   "containers/blocked",
		ContainerID:   containerID,
		ContainerName: "blocked",
		WorkloadID:    "workload-a",
	}))
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/missing",
		NetworkID:   "missing-network",
		WorkloadID:  "workload-a",
	}))
	_, leaseErr := stateStore.AcquireResourceLease(ctx, cleanupLeaseResource("containers/blocked"), heldLeaseOwner, time.Minute)
	require.NoError(t, leaseErr)

	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		cleanupLeaseOwner,
		func() (containers.ContainerOrchestrator, error) {
			return orchestrator, nil
		},
		processExecutor,
		logr.Discard(),
	)

	require.Error(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Networks: 1}, report.Stopped)
	require.Len(t, report.Failures, 1)
	require.Equal(t, "container", report.Failures[0].Kind)
	require.NotEmpty(t, report.Failures[0].Error)
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
