/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	"github.com/microsoft/dcp/internal/exerunners"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	dcpio "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

const cleanupTestRuntimeName = "test"

// Verifies that NewCleanupCommand registers the runtime flag and cleanupWorkloadOptionsFromCommand defaults volume cleanup off unless enabled.
func TestCleanupCommandDefaultsVolumeFlagOffAndRegistersRuntimeFlag(t *testing.T) {
	t.Parallel()

	cleanupCommand := NewCleanupCommand(&logger.Logger{Logger: logr.Discard()})
	options, optionsErr := cleanupWorkloadOptionsFromCommand(cleanupCommand)

	require.NoError(t, optionsErr)
	require.False(t, options.Volumes)

	require.NoError(t, cleanupCommand.Flags().Set(cleanupVolumesFlagName, "true"))
	options, optionsErr = cleanupWorkloadOptionsFromCommand(cleanupCommand)
	require.NoError(t, optionsErr)
	require.True(t, options.Volumes)
	require.NotNil(t, cleanupCommand.Flags().Lookup(container_flags.RuntimeFlagName))
}

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
		cleanupWorkloadOptions{},
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

// Verifies that remainingWorkloadContainerWorkItems removes workload containers across networks and excludes recorded and other-workload containers.
func TestRemainingWorkloadContainerWorkItemsSelectsUnrecordedWorkloadContainers(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)

	firstNetworkID, firstNetworkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: "app-network-1"})
	require.NoError(t, firstNetworkErr)
	secondNetworkID, secondNetworkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: "app-network-2"})
	require.NoError(t, secondNetworkErr)
	otherNetworkID, otherNetworkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: "other-network"})
	require.NoError(t, otherNetworkErr)

	createContainer := func(name string, workloadID string, networks ...string) string {
		createNetworks := make([]containers.CreateContainerNetworkOptions, 0, len(networks))
		for _, network := range networks {
			createNetworks = append(createNetworks, containers.CreateContainerNetworkOptions{Name: network})
		}
		containerID, createErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
			Name:     name,
			Image:    "test-image",
			Labels:   []containers.Label{{Key: controllers.WorkloadIDLabel, Value: workloadID}},
			Networks: createNetworks,
		})
		require.NoError(t, createErr)
		return containerID
	}

	sessionContainerID := createContainer("session", "workload-a", firstNetworkID, secondNetworkID)
	persistentContainerID := createContainer("persistent", "workload-a", firstNetworkID)
	differentWorkloadContainerID := createContainer("different-workload", "workload-b", firstNetworkID)
	differentNetworkContainerID := createContainer("different-network", "workload-a", otherNetworkID)
	unattachedContainerID := createContainer("unattached", "workload-a")
	orchestrator.FailNextRemoveContainer("session", errors.New("transient removal failure"))

	report := cleanupReport{WorkloadID: "workload-a"}
	workItems := remainingWorkloadContainerWorkItems(
		ctx,
		"workload-a",
		[]statestore.PersistentContainerRecord{{ContainerID: persistentContainerID, RuntimeName: cleanupTestRuntimeName}},
		[]statestore.PersistentNetworkRecord{
			{ResourceKey: "containernetworks/app-network-1", NetworkID: firstNetworkID, RuntimeName: cleanupTestRuntimeName},
			{ResourceKey: "containernetworks/app-network-2", NetworkID: secondNetworkID, RuntimeName: cleanupTestRuntimeName},
		},
		nil,
		nil,
		func(runtimeName string) (containers.ContainerOrchestrator, error) {
			require.Equal(t, cleanupTestRuntimeName, runtimeName)
			return orchestrator, nil
		},
	)
	require.Len(t, workItems, 3)
	cleanupErr := runCleanupResourceGroups(&report, []cleanupResourceGroup{{
		gvr:       cleanupResourceContainerGVR,
		workItems: workItems,
	}})

	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Containers: 3}, report.Stopped)
	require.Empty(t, report.Failures)
	require.GreaterOrEqual(t, orchestrator.RemoveContainerCallCount("session"), 2)
	for _, removedContainerID := range []string{sessionContainerID, differentNetworkContainerID, unattachedContainerID} {
		_, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{removedContainerID}})
		require.ErrorIs(t, inspectErr, containers.ErrNotFound)
	}
	for _, retainedContainerID := range []string{persistentContainerID, differentWorkloadContainerID} {
		inspected, retainedInspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
			Containers: []string{retainedContainerID},
		})
		require.NoError(t, retainedInspectErr)
		require.Len(t, inspected, 1)
	}
}

// Verifies that remainingWorkloadContainerWorkItems reports a failed removal and permits another cleanup attempt.
func TestRemainingWorkloadContainerWorkItemsReportsRemovalFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)

	networkID, createNetworkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: "app-network", Labels: map[string]string{controllers.WorkloadIDLabel: "workload-a"},
	})
	require.NoError(t, createNetworkErr)
	const containerName = "remaining-container-removal-failure"
	containerID, createErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:     containerName,
		Image:    "test-image",
		Networks: []containers.CreateContainerNetworkOptions{{Name: networkID}},
		Labels: []containers.Label{
			{Key: controllers.WorkloadIDLabel, Value: "workload-a"},
		},
	})
	require.NoError(t, createErr)
	orchestrator.FailNextRemoveContainer(containerName, resiliency.Permanent(errors.New("simulated removal failure")))

	report := cleanupReport{WorkloadID: "workload-a"}
	workItems := remainingWorkloadContainerWorkItems(
		ctx,
		"workload-a",
		nil,
		[]statestore.PersistentNetworkRecord{{
			ResourceKey: "containernetworks/app-network",
			NetworkID:   networkID,
			RuntimeName: cleanupTestRuntimeName,
		}},
		nil,
		nil,
		func(string) (containers.ContainerOrchestrator, error) {
			return orchestrator, nil
		},
	)
	firstCleanupErr := runCleanupResourceGroups(&report, []cleanupResourceGroup{{
		gvr:       cleanupResourceContainerGVR,
		workItems: workItems,
	}})

	require.Error(t, firstCleanupErr)
	require.Equal(t, cleanupStoppedCounts{}, report.Stopped)
	require.Len(t, report.Failures, 1)
	require.Equal(t, "containers/"+containerName, report.Failures[0].ResourceKey)
	require.Equal(t, containerID, report.Failures[0].ResourceID)
	require.Contains(t, report.Failures[0].Error, "simulated removal failure")

	retryReport := cleanupReport{WorkloadID: "workload-a"}
	retryWorkItems := remainingWorkloadContainerWorkItems(
		ctx,
		"workload-a",
		nil,
		[]statestore.PersistentNetworkRecord{{
			ResourceKey: "containernetworks/app-network",
			NetworkID:   networkID,
			RuntimeName: cleanupTestRuntimeName,
		}},
		nil,
		nil,
		func(string) (containers.ContainerOrchestrator, error) {
			return orchestrator, nil
		},
	)
	retryCleanupErr := runCleanupResourceGroups(&retryReport, []cleanupResourceGroup{{
		gvr:       cleanupResourceContainerGVR,
		workItems: retryWorkItems,
	}})
	require.NoError(t, retryCleanupErr)
	require.Equal(t, cleanupStoppedCounts{Containers: 1}, retryReport.Stopped)
	require.Empty(t, retryReport.Failures)
}

// Verifies that cleanupWorkloadResources removes recorded and unrecorded workload resources and detaches unrelated containers from removed networks.
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

	networkID, createNetworkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: "app-network", Labels: map[string]string{controllers.WorkloadIDLabel: "workload-a"},
	})
	require.NoError(t, createNetworkErr)
	containerID, createContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:  "api",
		Image: "test-image",
	})
	require.NoError(t, createContainerErr)
	remainingContainerID, createRemainingContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:     "remaining-session",
		Image:    "test-image",
		Labels:   []containers.Label{{Key: controllers.WorkloadIDLabel, Value: "workload-a"}},
		Networks: []containers.CreateContainerNetworkOptions{{Name: networkID}},
	})
	require.NoError(t, createRemainingContainerErr)
	otherWorkloadContainerID, createOtherWorkloadContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:     "other-workload",
		Image:    "test-image",
		Labels:   []containers.Label{{Key: controllers.WorkloadIDLabel, Value: "workload-b"}},
		Networks: []containers.CreateContainerNetworkOptions{{Name: networkID}},
	})
	require.NoError(t, createOtherWorkloadContainerErr)

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
		cleanupWorkloadOptions{discoverRuntime: func() (containers.ContainerOrchestrator, error) {
			return orchestrator, nil
		}},
		logr.Discard(),
	)

	require.NoError(t, cleanupErr, "cleanup report: %+v", report)
	require.Equal(t, cleanupStoppedCounts{Containers: 2, Networks: 1}, report.Stopped)
	require.Empty(t, report.Failures)
	_, inspectContainerErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
	require.ErrorIs(t, inspectContainerErr, containers.ErrNotFound)
	_, inspectRemainingContainerErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{remainingContainerID}})
	require.ErrorIs(t, inspectRemainingContainerErr, containers.ErrNotFound)
	otherWorkloadContainers, inspectOtherWorkloadContainerErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{otherWorkloadContainerID},
	})
	require.NoError(t, inspectOtherWorkloadContainerErr)
	require.Len(t, otherWorkloadContainers, 1)
	require.NotContains(t, otherWorkloadContainers[0].Networks, "app-network")
	_, inspectNetworkErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})
	require.ErrorIs(t, inspectNetworkErr, containers.ErrNotFound)

	containerRecords, listContainerErr := stateStore.ListPersistentContainersByWorkloadID(ctx, "workload-a")
	require.NoError(t, listContainerErr)
	require.Empty(t, containerRecords)
	networkRecords, listNetworkErr := stateStore.ListPersistentNetworksByWorkloadID(ctx, "workload-a")
	require.NoError(t, listNetworkErr)
	require.Empty(t, networkRecords)
}

// Verifies that cleanupWorkloadResources finds a detached container on a subsequent run after removal fails and the network is cleaned up.
func TestCleanupWorkloadResourcesRetriesDetachedContainerAfterRemovalFailure(t *testing.T) {
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

	const containerName = "remaining-session"
	networkID, createNetworkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: "app-network"})
	require.NoError(t, createNetworkErr)
	containerID, createContainerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:     containerName,
		Image:    "test-image",
		Labels:   []containers.Label{{Key: controllers.WorkloadIDLabel, Value: "workload-a"}},
		Networks: []containers.CreateContainerNetworkOptions{{Name: networkID}},
	})
	require.NoError(t, createContainerErr)
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/app-network",
		NetworkID:   networkID,
		NetworkName: "app-network",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-a",
	}))
	orchestrator.FailNextRemoveContainer(containerName, resiliency.Permanent(errors.New("simulated removal failure")))

	getOrchestrator := func(runtimeName string) (containers.ContainerOrchestrator, error) {
		require.Equal(t, cleanupTestRuntimeName, runtimeName)
		return orchestrator, nil
	}
	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		getOrchestrator,
		processExecutor,
		cleanupWorkloadOptions{},
		logr.Discard(),
	)

	require.Error(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{}, report.Stopped)
	require.Len(t, report.Failures, 2)
	require.Equal(t, cleanupResourceName(cleanupResourceContainerGVR), report.Failures[0].Kind)
	require.Equal(t, cleanupResourceName(cleanupResourceNetworkGVR), report.Failures[1].Kind)
	inspectedContainer, inspectDetachedErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
	require.NoError(t, inspectDetachedErr)
	require.NotContains(t, inspectedContainer[0].Networks, "app-network")
	_, inspectNetworkErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})
	require.ErrorIs(t, inspectNetworkErr, containers.ErrNotFound)
	networkRecords, listNetworkErr := stateStore.ListPersistentNetworksByWorkloadID(ctx, "workload-a")
	require.NoError(t, listNetworkErr)
	require.Len(t, networkRecords, 1)

	retryReport, retryErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		getOrchestrator,
		processExecutor,
		cleanupWorkloadOptions{},
		logr.Discard(),
	)
	require.NoError(t, retryErr, "cleanup report: %+v", retryReport)
	require.Equal(t, cleanupStoppedCounts{Containers: 1, Networks: 1}, retryReport.Stopped)
	require.Empty(t, retryReport.Failures)
	_, inspectContainerErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
	require.ErrorIs(t, inspectContainerErr, containers.ErrNotFound)
}

// Verifies that cleanupWorkloadResources scans runtimes from network records for remaining workload containers, including unattached ones.
func TestCleanupWorkloadResourcesScansRecordedNetworkRuntimes(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()

	firstOrchestrator, firstOrchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, firstOrchestratorErr)
	secondOrchestrator, secondOrchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, secondOrchestratorErr)

	type runtimeCase struct {
		name         string
		orchestrator *ctrlutil.TestContainerOrchestrator
	}
	runtimes := []runtimeCase{
		{name: "runtime-a", orchestrator: firstOrchestrator},
		{name: "runtime-b", orchestrator: secondOrchestrator},
	}
	containerIDs := make([]string, 0, len(runtimes))
	networkIDs := make([]string, 0, len(runtimes))
	for _, runtime := range runtimes {
		networkName := runtime.name + "-network"
		networkID, createNetworkErr := runtime.orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: networkName})
		require.NoError(t, createNetworkErr)
		containerID, createContainerErr := runtime.orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
			Name:     runtime.name + "-container",
			Image:    "test-image",
			Labels:   []containers.Label{{Key: controllers.WorkloadIDLabel, Value: "workload-a"}},
			Networks: []containers.CreateContainerNetworkOptions{{Name: networkID}},
		})
		require.NoError(t, createContainerErr)
		require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
			ResourceKey: "containernetworks/" + networkName,
			NetworkID:   networkID,
			NetworkName: networkName,
			RuntimeName: runtime.name,
			WorkloadID:  "workload-a",
		}))
		containerIDs = append(containerIDs, containerID)
		networkIDs = append(networkIDs, networkID)
	}
	unattachedContainerID, createUnattachedErr := secondOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:   "unattached-on-recorded-runtime",
		Image:  "test-image",
		Labels: []containers.Label{{Key: controllers.WorkloadIDLabel, Value: "workload-a"}},
	})
	require.NoError(t, createUnattachedErr)

	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func(runtimeName string) (containers.ContainerOrchestrator, error) {
			for _, runtime := range runtimes {
				if runtimeName == runtime.name {
					return runtime.orchestrator, nil
				}
			}
			return nil, fmt.Errorf("unexpected runtime %q", runtimeName)
		},
		processExecutor,
		cleanupWorkloadOptions{},
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Containers: 3, Networks: 2}, report.Stopped)
	require.Empty(t, report.Failures)
	for i, runtime := range runtimes {
		_, inspectContainerErr := runtime.orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerIDs[i]}})
		require.ErrorIs(t, inspectContainerErr, containers.ErrNotFound)
		_, inspectNetworkErr := runtime.orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkIDs[i]}})
		require.ErrorIs(t, inspectNetworkErr, containers.ErrNotFound)
	}
	_, inspectUnattachedErr := secondOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{unattachedContainerID}})
	require.ErrorIs(t, inspectUnattachedErr, containers.ErrNotFound)
}

// Verifies that cleanupWorkloadResources discovers workload networks on the selected runtime while leaving other workloads and failed recorded containers intact.
func TestCleanupWorkloadResourcesDiscoversUnrecordedNetworksOnSelectedRuntime(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()
	selectedRuntime, selectedErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, selectedErr)
	recordedRuntime, recordedErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, recordedErr)

	networkID, createNetworkErr := selectedRuntime.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name:   "session-network",
		Labels: map[string]string{controllers.WorkloadIDLabel: "workload-a"},
	})
	require.NoError(t, createNetworkErr)
	otherNetworkID, createOtherNetworkErr := selectedRuntime.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name:   "other-network",
		Labels: map[string]string{controllers.WorkloadIDLabel: "workload-b"},
	})
	require.NoError(t, createOtherNetworkErr)
	recordedNetworkID, createRecordedNetworkErr := recordedRuntime.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: "persistent-network"})
	require.NoError(t, createRecordedNetworkErr)
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/persistent-network",
		NetworkID:   recordedNetworkID,
		RuntimeName: "recorded-runtime",
		WorkloadID:  "workload-a",
	}))

	createContainer := func(name, workload string) string {
		containerID, createErr := selectedRuntime.CreateContainer(ctx, containers.CreateContainerOptions{
			Name: name, Image: "test-image",
			Labels:   []containers.Label{{Key: controllers.WorkloadIDLabel, Value: workload}},
			Networks: []containers.CreateContainerNetworkOptions{{Name: networkID}},
		})
		require.NoError(t, createErr)
		return containerID
	}
	sessionContainerID := createContainer("remaining-session", "workload-a")
	recordedContainerID := createContainer("failed-persistent", "workload-a")
	otherContainerID := createContainer("other-workload", "workload-b")
	require.NoError(t, stateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
		ResourceKey: "containers/failed-persistent",
		ContainerID: recordedContainerID,
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  "workload-a",
	}))
	selectedRuntime.FailNextRemoveContainer("failed-persistent", resiliency.Permanent(errors.New("record-backed removal failed")))

	discoveryCalls := 0
	report, cleanupErr := cleanupWorkloadResources(
		ctx, "workload-a", stateStore, leaseOwner,
		func(name string) (containers.ContainerOrchestrator, error) {
			switch name {
			case cleanupTestRuntimeName:
				return selectedRuntime, nil
			case "recorded-runtime":
				return recordedRuntime, nil
			default:
				return nil, fmt.Errorf("unexpected runtime %q", name)
			}
		},
		processExecutor,
		cleanupWorkloadOptions{discoverRuntime: func() (containers.ContainerOrchestrator, error) {
			discoveryCalls++
			return selectedRuntime, nil
		}},
		logr.Discard(),
	)

	require.Error(t, cleanupErr)
	require.Equal(t, 1, discoveryCalls)
	require.Equal(t, cleanupStoppedCounts{Containers: 1, Networks: 1}, report.Stopped)
	require.Len(t, report.Failures, 2)
	require.Equal(t, "containers/failed-persistent", report.Failures[0].ResourceKey)
	require.Equal(t, "containernetworks/session-network", report.Failures[1].ResourceKey)
	require.Equal(t, 1, selectedRuntime.RemoveContainerCallCount("failed-persistent"))
	for _, containerID := range []string{sessionContainerID, recordedContainerID} {
		_, inspectErr := selectedRuntime.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
		if containerID == sessionContainerID {
			require.ErrorIs(t, inspectErr, containers.ErrNotFound)
		} else {
			require.NoError(t, inspectErr)
		}
	}
	other, inspectOtherErr := selectedRuntime.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{otherContainerID}})
	require.NoError(t, inspectOtherErr)
	require.NotContains(t, other[0].Networks, "session-network")
	for _, networkID := range []string{networkID, recordedNetworkID} {
		orchestrator := selectedRuntime
		if networkID == recordedNetworkID {
			orchestrator = recordedRuntime
		}
		_, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})
		require.ErrorIs(t, inspectErr, containers.ErrNotFound)
	}
	_, inspectOtherNetworkErr := selectedRuntime.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{otherNetworkID}})
	require.NoError(t, inspectOtherNetworkErr)
	remainingRecords, listErr := stateStore.ListPersistentContainersByWorkloadID(ctx, "workload-a")
	require.NoError(t, listErr)
	require.Len(t, remainingRecords, 1)
	require.Equal(t, recordedContainerID, remainingRecords[0].ContainerID)
}

// Verifies that cleanupWorkloadResources removes an unrecorded workload network and container and detaches unrelated containers without persistence records.
func TestCleanupWorkloadResourcesRemovesUnrecordedNetworkWithoutRecords(t *testing.T) {
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
	networkID, networkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: "session-network", Labels: map[string]string{controllers.WorkloadIDLabel: "workload-a"},
	})
	require.NoError(t, networkErr)
	containerID, containerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: "session-container", Image: "test-image",
		Labels:   []containers.Label{{Key: controllers.WorkloadIDLabel, Value: "workload-a"}},
		Networks: []containers.CreateContainerNetworkOptions{{Name: networkID}},
	})
	require.NoError(t, containerErr)
	otherID, otherErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: "unrelated", Image: "test-image",
		Networks: []containers.CreateContainerNetworkOptions{{Name: networkID}},
	})
	require.NoError(t, otherErr)

	report, cleanupErr := cleanupWorkloadResources(
		ctx, "workload-a", stateStore, leaseOwner,
		func(string) (containers.ContainerOrchestrator, error) {
			require.Fail(t, "no persistent resource needs a runtime")
			return nil, errors.New("unexpected runtime")
		},
		processExecutor,
		cleanupWorkloadOptions{discoverRuntime: func() (containers.ContainerOrchestrator, error) {
			return orchestrator, nil
		}},
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Containers: 1, Networks: 1}, report.Stopped)
	require.Empty(t, report.Failures)
	_, inspectContainerErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
	require.ErrorIs(t, inspectContainerErr, containers.ErrNotFound)
	unrelated, inspectOtherErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{otherID}})
	require.NoError(t, inspectOtherErr)
	require.NotContains(t, unrelated[0].Networks, "session-network")
	_, inspectNetworkErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})
	require.ErrorIs(t, inspectNetworkErr, containers.ErrNotFound)
}

// Verifies that cleanupWorkloadResources removes an unattached workload container without requiring a workload network.
func TestCleanupWorkloadResourcesRemovesUnrecordedContainerWithoutWorkloadNetworks(t *testing.T) {
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

	containerID, createErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: "unattached-container", Image: "test-image",
		Labels: []containers.Label{{Key: controllers.WorkloadIDLabel, Value: "workload-a"}},
	})
	require.NoError(t, createErr)
	otherID, createOtherErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: "different-workload", Image: "test-image",
		Labels: []containers.Label{{Key: controllers.WorkloadIDLabel, Value: "workload-b"}},
	})
	require.NoError(t, createOtherErr)

	report, cleanupErr := cleanupWorkloadResources(
		ctx, "workload-a", stateStore, leaseOwner,
		func(string) (containers.ContainerOrchestrator, error) {
			require.Fail(t, "no persistent resource needs a runtime")
			return nil, errors.New("unexpected runtime")
		},
		processExecutor,
		cleanupWorkloadOptions{discoverRuntime: func() (containers.ContainerOrchestrator, error) {
			return orchestrator, nil
		}},
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Containers: 1}, report.Stopped)
	require.Empty(t, report.Failures)
	_, inspectContainerErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
	require.ErrorIs(t, inspectContainerErr, containers.ErrNotFound)
	_, inspectOtherErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{otherID}})
	require.NoError(t, inspectOtherErr)
}

// Verifies that cleanupWorkloadResources can find a detached container on a subsequent run after container discovery fails.
func TestCleanupWorkloadResourcesRecoversDetachedContainerAfterDiscoveryFailure(t *testing.T) {
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
	wrappedOrchestrator := &failingWorkloadContainerScanOrchestrator{ContainerOrchestrator: orchestrator}

	networkID, networkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: "session-network", Labels: map[string]string{controllers.WorkloadIDLabel: "workload-a"},
	})
	require.NoError(t, networkErr)
	containerID, containerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: "session-container", Image: "test-image",
		Labels:   []containers.Label{{Key: controllers.WorkloadIDLabel, Value: "workload-a"}},
		Networks: []containers.CreateContainerNetworkOptions{{Name: networkID}},
	})
	require.NoError(t, containerErr)

	runCleanup := func() (cleanupReport, error) {
		return cleanupWorkloadResources(
			ctx, "workload-a", stateStore, leaseOwner,
			func(string) (containers.ContainerOrchestrator, error) {
				require.Fail(t, "no persistent resource needs a runtime")
				return nil, errors.New("unexpected runtime")
			},
			processExecutor,
			cleanupWorkloadOptions{discoverRuntime: func() (containers.ContainerOrchestrator, error) {
				return wrappedOrchestrator, nil
			}},
			logr.Discard(),
		)
	}

	firstReport, firstErr := runCleanup()
	require.Error(t, firstErr)
	require.Equal(t, cleanupStoppedCounts{}, firstReport.Stopped)
	require.Len(t, firstReport.Failures, 2)
	require.ErrorContains(t, errors.New(firstReport.Failures[0].Error), "simulated container scan failure")
	inspectedContainer, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
	require.NoError(t, inspectErr)
	require.NotContains(t, inspectedContainer[0].Networks, "session-network")
	_, inspectNetworkErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})
	require.ErrorIs(t, inspectNetworkErr, containers.ErrNotFound)

	retryReport, retryErr := runCleanup()
	require.NoError(t, retryErr)
	require.Equal(t, cleanupStoppedCounts{Containers: 1}, retryReport.Stopped)
	require.Empty(t, retryReport.Failures)
	_, inspectContainerErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
	require.ErrorIs(t, inspectContainerErr, containers.ErrNotFound)
}

// Verifies that cleanupWorkloadResources finds recorded and unrecorded resources using a comma-containing workload ID.
func TestCleanupWorkloadResourcesFindsResourcesWithCommaInWorkloadID(t *testing.T) {
	t.Parallel()

	const workloadID = "workload-a,segment"
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)

	networkIDs := make([]string, 0, 2)
	containerIDs := make([]string, 0, 2)
	for _, name := range []string{"recorded-network", "session-network"} {
		networkID, networkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
			Name: name, Labels: map[string]string{controllers.WorkloadIDLabel: workloadID},
		})
		require.NoError(t, networkErr)
		containerID, containerErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
			Name:     name + "-container",
			Image:    "test-image",
			Labels:   []containers.Label{{Key: controllers.WorkloadIDLabel, Value: workloadID}},
			Networks: []containers.CreateContainerNetworkOptions{{Name: networkID}},
		})
		require.NoError(t, containerErr)
		networkIDs = append(networkIDs, networkID)
		containerIDs = append(containerIDs, containerID)
	}
	require.NoError(t, stateStore.UpsertPersistentNetwork(ctx, statestore.PersistentNetworkRecord{
		ResourceKey: "containernetworks/recorded-network",
		NetworkID:   networkIDs[0],
		NetworkName: "recorded-network",
		RuntimeName: cleanupTestRuntimeName,
		WorkloadID:  workloadID,
	}))

	report, cleanupErr := cleanupWorkloadResources(
		ctx, workloadID, stateStore, leaseOwner,
		func(string) (containers.ContainerOrchestrator, error) { return orchestrator, nil },
		processExecutor,
		cleanupWorkloadOptions{discoverRuntime: func() (containers.ContainerOrchestrator, error) {
			return orchestrator, nil
		}},
		logr.Discard(),
	)

	require.NoError(t, cleanupErr, "cleanup report: %+v", report)
	require.Equal(t, cleanupStoppedCounts{Containers: 2, Networks: 2}, report.Stopped)
	require.Empty(t, report.Failures)
	for _, containerID := range containerIDs {
		_, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
		require.ErrorIs(t, inspectErr, containers.ErrNotFound)
	}
	for _, networkID := range networkIDs {
		_, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})
		require.ErrorIs(t, inspectErr, containers.ErrNotFound)
	}
}

// Verifies that cleanupWorkloadResources reports selected-runtime discovery errors as network cleanup failures.
func TestCleanupWorkloadResourcesReportsNetworkDiscoveryFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	processExecutor := process.NewOSExecutor(logr.Discard())
	defer processExecutor.Dispose()
	discoveryErr := errors.New("runtime not running")

	report, cleanupErr := cleanupWorkloadResources(
		ctx, "workload-a", stateStore, leaseOwner,
		func(string) (containers.ContainerOrchestrator, error) {
			require.Fail(t, "no persistent resource needs a runtime")
			return nil, errors.New("unexpected runtime")
		},
		processExecutor,
		cleanupWorkloadOptions{discoverRuntime: func() (containers.ContainerOrchestrator, error) {
			return nil, discoveryErr
		}},
		logr.Discard(),
	)

	require.ErrorIs(t, cleanupErr, discoveryErr)
	require.Equal(t, cleanupStoppedCounts{}, report.Stopped)
	require.Len(t, report.Failures, 1)
	require.Equal(t, cleanupResourceName(cleanupResourceNetworkGVR), report.Failures[0].Kind)
	require.ErrorContains(t, errors.New(report.Failures[0].Error), discoveryErr.Error())
}

func TestRemovePersistentNetworkRetriesTransientRemovalFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)
	wrappedOrchestrator := &transientNetworkRemovalOrchestrator{
		ContainerOrchestrator: orchestrator,
		failuresRemaining:     2,
	}

	networkID, createNetworkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: "app-network"})
	require.NoError(t, createNetworkErr)

	removeErr := removePersistentNetwork(ctx, wrappedOrchestrator, networkID)

	require.NoError(t, removeErr)
	require.Equal(t, 3, wrappedOrchestrator.removeCalls)
	_, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
}

// Verifies that removeContainerWithRetryTimeout cancels a blocked runtime removal when its timeout expires.
func TestRemoveContainerWithRetryTimeoutCancelsBlockingRuntimeOperation(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)
	wrappedOrchestrator := &blockingContainerRemovalOrchestrator{
		ContainerOrchestrator: orchestrator,
		removeStarted:         make(chan struct{}),
	}
	containerID, createErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:  "blocked-removal",
		Image: "test-image",
	})
	require.NoError(t, createErr)

	removeErr := removeContainerWithRetryTimeout(ctx, wrappedOrchestrator, containerID, 100*time.Millisecond)

	require.ErrorIs(t, removeErr, context.DeadlineExceeded)
	require.Eventually(t, func() bool {
		select {
		case <-wrappedOrchestrator.removeStarted:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func TestCleanupWorkloadResourcesOnlyRemovesVolumesWhenEnabled(t *testing.T) {
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

	const volumeName = "app-data"
	const ownershipToken = "app-data-token"
	require.NoError(t, orchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
		Name: volumeName,
		Labels: map[string]string{
			containers.VolumeOwnershipTokenLabel: ownershipToken,
		},
	}))
	require.NoError(t, stateStore.UpsertPersistentVolume(ctx, statestore.PersistentVolumeRecord{
		ResourceKey:    "containervolumes/" + volumeName,
		VolumeName:     volumeName,
		RuntimeName:    cleanupTestRuntimeName,
		WorkloadID:     "workload-a",
		OwnershipToken: ownershipToken,
	}))

	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func(string) (containers.ContainerOrchestrator, error) {
			require.Fail(t, "container runtime should not be requested when volume cleanup is disabled")
			return nil, nil
		},
		processExecutor,
		cleanupWorkloadOptions{},
		logr.Discard(),
	)
	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{}, report.Stopped)
	_, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
	require.NoError(t, inspectErr)
	volumeRecords, listErr := stateStore.ListPersistentVolumesByWorkloadID(ctx, "workload-a")
	require.NoError(t, listErr)
	require.Len(t, volumeRecords, 1)

	report, cleanupErr = cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func(runtimeName string) (containers.ContainerOrchestrator, error) {
			require.Equal(t, cleanupTestRuntimeName, runtimeName)
			return orchestrator, nil
		},
		processExecutor,
		cleanupWorkloadOptions{Volumes: true},
		logr.Discard(),
	)
	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Volumes: 1}, report.Stopped)
	_, inspectErr = orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
	volumeRecords, listErr = stateStore.ListPersistentVolumesByWorkloadID(ctx, "workload-a")
	require.NoError(t, listErr)
	require.Empty(t, volumeRecords)
}

func TestRemovePersistentVolumeDoesNotForceRemoval(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	orchestrator, orchestratorErr := ctrlutil.NewTestContainerOrchestrator(ctx, logr.Discard(), ctrlutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)
	wrappedOrchestrator := &recordingVolumeRemovalOrchestrator{ContainerOrchestrator: orchestrator}

	const volumeName = "app-data"
	const ownershipToken = "app-data-token"
	require.NoError(t, orchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
		Name: volumeName,
		Labels: map[string]string{
			containers.VolumeOwnershipTokenLabel: ownershipToken,
		},
	}))

	removed, removeErr := removePersistentVolume(ctx, wrappedOrchestrator, volumeName, ownershipToken)

	require.NoError(t, removeErr)
	require.True(t, removed)
	require.False(t, wrappedOrchestrator.options.Force)
}

func TestCleanupWorkloadResourcesPreservesReplacementVolume(t *testing.T) {
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

	const volumeName = "replacement-data"
	require.NoError(t, orchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
		Name: volumeName,
		Labels: map[string]string{
			containers.VolumeOwnershipTokenLabel: "replacement-token",
		},
	}))
	require.NoError(t, stateStore.UpsertPersistentVolume(ctx, statestore.PersistentVolumeRecord{
		ResourceKey:    "containervolumes/" + volumeName,
		VolumeName:     volumeName,
		RuntimeName:    cleanupTestRuntimeName,
		WorkloadID:     "workload-a",
		OwnershipToken: "original-token",
	}))

	report, cleanupErr := cleanupWorkloadResources(
		ctx,
		"workload-a",
		stateStore,
		leaseOwner,
		func(string) (containers.ContainerOrchestrator, error) {
			return orchestrator, nil
		},
		processExecutor,
		cleanupWorkloadOptions{Volumes: true},
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{}, report.Stopped)
	_, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
	require.NoError(t, inspectErr)
	_, getRecordErr := stateStore.GetPersistentVolume(ctx, "containervolumes/"+volumeName)
	require.ErrorIs(t, getRecordErr, statestore.ErrPersistentVolumeNotFound)
}

func TestCleanupWorkloadResourcesRemovesContainersBeforeNetworksAndVolumes(t *testing.T) {
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
		removeVolumesEntered:    make(chan struct{}),
	}

	networkID, createNetworkErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: "app-network"})
	require.NoError(t, createNetworkErr)
	const volumeOwnershipToken = "app-data-token"
	require.NoError(t, orchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
		Name: "app-data",
		Labels: map[string]string{
			containers.VolumeOwnershipTokenLabel: volumeOwnershipToken,
		},
	}))
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
	require.NoError(t, stateStore.UpsertPersistentVolume(ctx, statestore.PersistentVolumeRecord{
		ResourceKey:    "containervolumes/app-data",
		VolumeName:     "app-data",
		RuntimeName:    cleanupTestRuntimeName,
		WorkloadID:     "workload-a",
		OwnershipToken: volumeOwnershipToken,
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
			cleanupWorkloadOptions{Volumes: true},
			logr.Discard(),
		)
		resultCh <- cleanupResult{report: report, err: cleanupErr}
	}()

	select {
	case <-wrappedOrchestrator.removeContainersEntered:
	case <-ctx.Done():
		require.FailNow(t, "container cleanup did not start", ctx.Err())
	}
	dependentStartedEarlyTimer := time.NewTimer(100 * time.Millisecond)
	defer dependentStartedEarlyTimer.Stop()
	select {
	case <-wrappedOrchestrator.removeNetworksEntered:
		close(wrappedOrchestrator.allowRemoveContainers)
		select {
		case <-resultCh:
		case <-ctx.Done():
			require.FailNow(t, "cleanup did not finish", ctx.Err())
		}
		require.FailNow(t, "network cleanup started before container cleanup finished")
	case <-wrappedOrchestrator.removeVolumesEntered:
		close(wrappedOrchestrator.allowRemoveContainers)
		select {
		case <-resultCh:
		case <-ctx.Done():
			require.FailNow(t, "cleanup did not finish", ctx.Err())
		}
		require.FailNow(t, "volume cleanup started before container cleanup finished")
	case <-dependentStartedEarlyTimer.C:
	}
	close(wrappedOrchestrator.allowRemoveContainers)

	var result cleanupResult
	select {
	case result = <-resultCh:
	case <-ctx.Done():
		require.FailNow(t, "cleanup did not finish", ctx.Err())
	}
	require.NoError(t, result.err)
	require.Equal(t, cleanupStoppedCounts{Containers: 1, Networks: 1, Volumes: 1}, result.report.Stopped)
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
	require.NoError(t, stateStore.UpsertPersistentVolume(ctx, statestore.PersistentVolumeRecord{
		ResourceKey:    "containervolumes/missing",
		VolumeName:     "missing-volume",
		RuntimeName:    cleanupTestRuntimeName,
		WorkloadID:     "workload-a",
		OwnershipToken: "missing-token",
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
		cleanupWorkloadOptions{Volumes: true},
		logr.Discard(),
	)

	require.NoError(t, cleanupErr)
	require.Equal(t, cleanupStoppedCounts{Containers: 1, Networks: 1}, report.Stopped)
	require.Empty(t, report.Failures)
	_, getVolumeErr := stateStore.GetPersistentVolume(ctx, "containervolumes/missing")
	require.ErrorIs(t, getVolumeErr, statestore.ErrPersistentVolumeNotFound)
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
			cleanupWorkloadOptions{},
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

// Verifies that runCleanupResourceGroups reports completed work and resource failures even when dependency resolution fails.
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
	require.ErrorContains(t, cleanupErr, "failed to clean up 1 resource")
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
	// Start cleanup on a goroutine so it has to wait on the held resource lease.
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

	// Hold the lease long enough for at least one retry, then release it while cleanup is still waiting.
	time.Sleep(2*workloadCleanupLeaseRetryInterval + 100*time.Millisecond)
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
		cleanupWorkloadOptions{},
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

// Verifies that cleanupWorkloadResources reports runtime failures while still removing executable records.
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
		cleanupWorkloadOptions{},
		logr.Discard(),
	)

	require.Error(t, cleanupErr)
	require.Equal(t, 1, runtimeRequests)
	require.Equal(t, cleanupStoppedCounts{Executables: 1}, report.Stopped)
	require.Len(t, report.Failures, 3)
	require.Equal(t, cleanupResourceName(cleanupResourceContainerGVR), report.Failures[0].Kind)
	require.Equal(t, "containers/api", report.Failures[0].ResourceKey)
	require.Equal(t, "api-container", report.Failures[0].ResourceID)
	require.ErrorContains(t, errors.New(report.Failures[0].Error), runtimeErr.Error())
	require.Equal(t, cleanupResourceName(cleanupResourceContainerGVR), report.Failures[1].Kind)
	require.Equal(t, "containers/"+cleanupTestRuntimeName, report.Failures[1].ResourceKey)
	require.ErrorContains(t, errors.New(report.Failures[1].Error), runtimeErr.Error())
	require.Equal(t, cleanupResourceName(cleanupResourceNetworkGVR), report.Failures[2].Kind)
	require.Equal(t, "containernetworks/api", report.Failures[2].ResourceKey)
	require.Equal(t, "api-network", report.Failures[2].ResourceID)
	require.ErrorContains(t, errors.New(report.Failures[2].Error), runtimeErr.Error())
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
		cleanupWorkloadOptions{},
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

type blockingContainerRemovalOrchestrator struct {
	containers.ContainerOrchestrator

	removeStarted chan struct{}
	startOnce     sync.Once
}

func (o *blockingContainerRemovalOrchestrator) RemoveContainers(ctx context.Context, _ containers.RemoveContainersOptions) ([]string, error) {
	o.startOnce.Do(func() {
		close(o.removeStarted)
	})
	<-ctx.Done()
	return nil, ctx.Err()
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
	removeVolumesEntered    chan struct{}
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

func (o *orderedCleanupContainerOrchestrator) RemoveVolumes(ctx context.Context, options containers.RemoveVolumesOptions) ([]string, error) {
	close(o.removeVolumesEntered)
	o.lock.Lock()
	removeContainersDone := o.removeContainersDone
	o.lock.Unlock()
	if !removeContainersDone {
		return nil, errors.New("volume cleanup started before container cleanup finished")
	}

	return o.ContainerOrchestrator.RemoveVolumes(ctx, options)
}

type failingCleanupContainerOrchestrator struct {
	containers.ContainerOrchestrator

	removeContainersErr error
}

func (o *failingCleanupContainerOrchestrator) RemoveContainers(context.Context, containers.RemoveContainersOptions) ([]string, error) {
	return nil, o.removeContainersErr
}

type failingWorkloadContainerScanOrchestrator struct {
	containers.ContainerOrchestrator
	failOnce sync.Once
}

func (o *failingWorkloadContainerScanOrchestrator) ListContainers(ctx context.Context, options containers.ListContainersOptions) ([]containers.ListedContainer, error) {
	if len(options.Filters.LabelFilters) > 0 && len(options.Filters.NetworkFilters) == 0 {
		var scanErr error
		o.failOnce.Do(func() {
			scanErr = errors.New("simulated container scan failure")
		})
		if scanErr != nil {
			return nil, scanErr
		}
	}
	return o.ContainerOrchestrator.ListContainers(ctx, options)
}

type transientNetworkRemovalOrchestrator struct {
	containers.ContainerOrchestrator

	failuresRemaining int
	removeCalls       int
}

func (o *transientNetworkRemovalOrchestrator) RemoveNetworks(ctx context.Context, options containers.RemoveNetworksOptions) ([]string, error) {
	o.removeCalls++
	if o.failuresRemaining > 0 {
		o.failuresRemaining--
		return nil, errors.New("network has active endpoints")
	}
	return o.ContainerOrchestrator.RemoveNetworks(ctx, options)
}

type recordingVolumeRemovalOrchestrator struct {
	containers.ContainerOrchestrator
	options containers.RemoveVolumesOptions
}

func (o *recordingVolumeRemovalOrchestrator) RemoveVolumes(ctx context.Context, options containers.RemoveVolumesOptions) ([]string, error) {
	o.options = options
	return o.ContainerOrchestrator.RemoveVolumes(ctx, options)
}
