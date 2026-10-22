/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/testutil"
)

func createVolumeResetContainer(
	t *testing.T,
	ctx context.Context,
	server *ctrl_testutil.ApiServerInfo,
	persistent bool,
	mounts []apiv1.VolumeMount,
) (*apiv1.Container, containers.InspectedContainer) {
	t.Helper()
	container := &apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{Name: "reset-target"},
		Spec: apiv1.ContainerSpec{
			Image: "reset-image", ContainerName: "reset-target",
			Persistent: persistent, VolumeMounts: mounts,
		},
	}
	require.NoError(t, server.Client.Create(ctx, container))
	return ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, container)
}

func submitVolumeReset(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, container *apiv1.Container) *apiv1.ContainerVolumeReset {
	t.Helper()
	suffix, suffixErr := randdata.MakeRandomString(8)
	require.NoError(t, suffixErr)
	reset := &apiv1.ContainerVolumeReset{
		ObjectMeta: metav1.ObjectMeta{Name: "reset-" + string(suffix)},
		Spec:       apiv1.ContainerVolumeResetSpec{ContainerName: container.Name, ContainerUID: container.UID},
	}
	require.NoError(t, apiClient.Create(ctx, reset))
	return waitObjectAssumesStateEx(t, ctx, apiClient, ctrl_client.ObjectKeyFromObject(reset), func(updated *apiv1.ContainerVolumeReset) (bool, error) {
		return updated.Status.State == "Succeeded" || updated.Status.State == "Failed", nil
	})
}

// Verifies that Container volume reset removes session and persistent containers, selects fresh
// owned storage, and resumes the same Container without repeating the operation.
func TestContainerVolumeResetOwnedVolumes(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		persistent bool
		stopped    bool
		recreate   bool
	}{
		{name: "session"},
		{name: "persistent", persistent: true},
		{name: "stopped-session", stopped: true},
		{name: "stopped-persistent", persistent: true, stopped: true},
		{name: "owned-prior-uid", persistent: true, recreate: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			server, environment, startErr := StartTestEnvironmentWithOptions(t, ctx,
				ContainerController|VolumeController, "VolumeResetOwned"+scenario.name, t.TempDir(),
				TestEnvironmentOptions{WorkloadID: "reset-workload"})
			require.NoError(t, startErr)
			persistent := scenario.persistent
			volume := &apiv1.ContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"},
				Spec:       apiv1.ContainerVolumeSpec{Name: "reset-volume", Persistent: &persistent},
			}
			require.NoError(t, server.Client.Create(ctx, volume))
			before := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			container, runtimeContainer := createVolumeResetContainer(t, ctx, server, persistent,
				[]apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"},
					{Type: apiv1.BindMount, Source: t.TempDir(), Target: "/host"}})
			if scenario.recreate {
				require.NoError(t, server.Client.Delete(ctx, container))
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, container)
				require.NoError(t, server.Client.Delete(ctx, volume))
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, volume)
				volume = &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"}, Spec: volume.Spec}
				require.NoError(t, server.Client.Create(ctx, volume))
				_ = ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
				adopted, adoptedRuntime := createVolumeResetContainer(t, ctx, server, persistent, container.Spec.VolumeMounts)
				require.NotEqual(t, container.UID, adopted.UID)
				require.Equal(t, runtimeContainer.Id, adoptedRuntime.Id)
				container = adopted
			}
			if scenario.stopped {
				stopPatch := ctrl_client.MergeFrom(container.DeepCopy())
				container.Spec.Stop = true
				require.NoError(t, server.Client.Patch(ctx, container, stopPatch))
				container = ensureContainerState(t, ctx, server.Client, container, apiv1.ContainerStateExited)
			}

			resetContainer := submitVolumeReset(t, ctx, server.Client, container)
			require.Equal(t, "Succeeded", resetContainer.Status.State, resetContainer.Status.Message)
			require.True(t, resetContainer.Status.ContainerRemoved)
			require.Equal(t, []string{volume.Spec.Name}, resetContainer.Status.Volumes)
			_, removedInspectErr := server.ContainerOrchestrator.InspectContainers(ctx,
				containers.InspectContainersOptions{Containers: []string{runtimeContainer.Id}})
			require.ErrorIs(t, removedInspectErr, containers.ErrNotFound)
			after := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			require.True(t, after.CreatedAt.After(before.CreatedAt), "physical volume must have been replaced")
			requireSameVolumeOwnership(t, before, after)
			require.DirExists(t, container.Spec.VolumeMounts[1].Source, "bind-mounted host directories must be preserved")
			if persistent {
				record, recordErr := environment.StateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
				require.NoError(t, recordErr)
				require.Equal(t, after.Labels[containers.VolumeOwnershipTokenLabel], record.OwnershipToken)
			}
			if scenario.stopped {
				stopped := ensureContainerState(t, ctx, server.Client, container, apiv1.ContainerStateNotFound)
				require.Empty(t, stopped.Status.ContainerID)
				require.True(t, stopped.Spec.Stop)
				require.NoError(t, server.Client.Delete(ctx, resetContainer))
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resetContainer)
				return
			}
			resumed, newRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, container)
			require.Equal(t, container.UID, resumed.UID)
			require.NotEqual(t, runtimeContainer.Id, newRuntime.Id)
			unchanged := &apiv1.ContainerVolumeReset{}
			require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(resetContainer), unchanged))
			require.Equal(t, resetContainer.Status, unchanged.Status)
			require.NoError(t, server.Client.Delete(ctx, resetContainer))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resetContainer)
			finalVolume := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			require.Equal(t, after.CreatedAt, finalVolume.CreatedAt)
		})
	}
}

// Verifies that Container volume reset refuses API and stopped runtime consumers before removing
// or stopping the target, and returns names and IDs of the consumers that block the reset.
func TestContainerVolumeResetSharedVolumes(t *testing.T) {
	for _, consumerKind := range []string{"api", "stopped-runtime", "running-runtime"} {
		t.Run(consumerKind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			server, _, startErr := StartTestEnvironment(t, ctx, ContainerController|VolumeController,
				"VolumeResetShared"+consumerKind, t.TempDir())
			require.NoError(t, startErr)
			persistent := false
			volume := &apiv1.ContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"},
				Spec:       apiv1.ContainerVolumeSpec{Name: "reset-volume", Persistent: &persistent},
			}
			require.NoError(t, server.Client.Create(ctx, volume))
			before := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			mounts := []apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"}}
			target, targetRuntime := createVolumeResetContainer(t, ctx, server, false, mounts)
			if consumerKind == "api" {
				start := false
				consumer := &apiv1.Container{
					ObjectMeta: metav1.ObjectMeta{Name: "other-consumer"},
					Spec:       apiv1.ContainerSpec{Image: "reset-image", Start: &start, VolumeMounts: mounts},
				}
				require.NoError(t, server.Client.Create(ctx, consumer))
			} else {
				consumerID, createErr := server.ContainerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
					Name: "other-consumer", Image: "reset-image",
					VolumeMounts: []containers.CreateContainerVolumeMount{
						{Type: containers.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"},
					},
				})
				require.NoError(t, createErr)
				_, startConsumerErr := server.ContainerOrchestrator.StartContainers(ctx,
					containers.StartContainersOptions{Containers: []string{consumerID}})
				require.NoError(t, startConsumerErr)
				if consumerKind == "stopped-runtime" {
					_, stopConsumerErr := server.ContainerOrchestrator.StopContainers(ctx,
						containers.StopContainersOptions{Containers: []string{consumerID}})
					require.NoError(t, stopConsumerErr)
				}
			}
			result := submitVolumeReset(t, ctx, server.Client, target).Status
			require.Equal(t, "Failed", result.State)
			require.False(t, result.ContainerRemoved)
			require.Empty(t, result.Volumes)
			require.NotEmpty(t, result.Consumers)
			require.Contains(t, result.Message, "other-consumer")
			require.Equal(t, volume.Spec.Name, result.Consumers[0].VolumeName)
			require.Contains(t, result.Consumers[0].ContainerName, "other-consumer")
			if consumerKind != "api" {
				require.NotEmpty(t, result.Consumers[0].ContainerID)
			}
			inspected, inspectErr := server.ContainerOrchestrator.InspectContainers(ctx,
				containers.InspectContainersOptions{Containers: []string{targetRuntime.Id}})
			require.NoError(t, inspectErr)
			require.Equal(t, containers.ContainerStatusRunning, inspected[0].Status)
			after := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			require.Equal(t, before.CreatedAt, after.CreatedAt)
		})
	}
}

func runVolumeResetCommand(t *testing.T, ctx context.Context, orchestrator containers.ContainerOrchestrator, containerID, command string) {
	t.Helper()
	exit, execErr := orchestrator.ExecContainer(ctx, containers.ExecContainerOptions{
		Container: containerID, Command: "sh", Args: []string{"-c", command},
	})
	require.NoError(t, execErr)
	select {
	case code, valid := <-exit:
		require.True(t, valid, "exec exit channel closed without a result")
		require.Zero(t, code)
	case <-ctx.Done():
		require.NoError(t, ctx.Err(), "waiting for volume verification command")
	}
}

// Verifies that Container volume reset selects a fresh runtime volume without the old marker
// and that the recreated target mounts it, for both session and persistent lifetimes.
func TestContainerVolumeResetRealRuntime(t *testing.T) {
	testutil.SkipIfTrueContainerOrchestratorNotEnabled(t)
	dcppaths.EnableTestPathProbing()
	for _, scenario := range []struct {
		name           string
		persistent     bool
		recovery       bool
		preflightRetry bool
		workload       bool
	}{
		{name: "session"},
		{name: "persistent", persistent: true},
		{name: "session-recovery", recovery: true},
		{name: "persistent-recovery", persistent: true, recovery: true},
		{name: "persistent-preflight-retry", persistent: true, preflightRetry: true},
		{name: "persistent-workload-recovery", persistent: true, recovery: true, workload: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			persistent := scenario.persistent
			var fault *recoverableVolumeResetOrchestrator
			var stateStore *statestore.Store
			var workloadID commonapi.WorkloadID
			if scenario.workload {
				workloadID = "real-reset-workload"
			}
			server, environment, startErr := StartAdvancedTestEnvironmentWithOptions(ctx, ContainerController|VolumeController,
				"VolumeResetReal"+scenario.name, t.TempDir(), AdvancedTestEnvironmentOptions{
					ApiServerFlags: ctrl_testutil.ApiServerUseTrueContainerOrchestrator,
					WorkloadID:     workloadID,
					DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, store *statestore.Store) containers.ContainerOrchestrator {
						stateStore = store
						fault = &recoverableVolumeResetOrchestrator{ContainerOrchestrator: orchestrator}
						return fault
					},
				})
			require.NoError(t, startErr)
			defer environment.ProcessExecutor.Dispose()
			defer shutdownAdvancedTestEnvironment(t, ctx, cancel, server)
			suffix, suffixErr := randdata.MakeRandomString(8)
			require.NoError(t, suffixErr)
			resourceName := "volume-reset-real-" + string(suffix)
			var runtimeIDs []string
			defer func() {
				for _, runtimeID := range runtimeIDs {
					_, cleanupContainerErr := server.ContainerOrchestrator.RemoveContainers(context.Background(),
						containers.RemoveContainersOptions{Containers: []string{runtimeID}, Force: true})
					if cleanupContainerErr != nil && !errors.Is(cleanupContainerErr, containers.ErrNotFound) {
						t.Error(cleanupContainerErr)
					}
				}
				cleanupVolumeResetGenerations(t, server.ContainerOrchestrator, resourceName)
			}()
			volume := &apiv1.ContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec:       apiv1.ContainerVolumeSpec{Name: resourceName, Persistent: &persistent},
			}
			require.NoError(t, server.Client.Create(ctx, volume))
			before := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			if scenario.workload {
				require.NotEmpty(t, before.Labels[containers.VolumeOwnershipTokenLabel])
				record, recordErr := stateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
				require.NoError(t, recordErr)
				require.Equal(t, before.Labels[containers.VolumeOwnershipTokenLabel], record.OwnershipToken)
			}
			container := &apiv1.Container{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: apiv1.ContainerSpec{
					Image: "busybox:latest", ContainerName: resourceName, Persistent: persistent,
					Command: "sh", Args: []string{"-c", "sleep 600"},
					VolumeMounts: []apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: resourceName, Target: "/data"}},
				},
			}
			require.NoError(t, server.Client.Create(ctx, container))
			running, originalRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, container)
			runtimeIDs = append(runtimeIDs, originalRuntime.Id)
			runVolumeResetCommand(t, ctx, server.ContainerOrchestrator, originalRuntime.Id,
				"printf marker > /data/marker && test -s /data/marker")
			if scenario.preflightRetry {
				_, recordErr := stateStore.GetPersistentContainer(ctx, running.GetLeaseKey())
				require.ErrorIs(t, recordErr, statestore.ErrPersistentContainerNotFound, "this scenario requires no workload ownership record")
				blockerID, createBlockerErr := server.ContainerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
					Name: resourceName + "-blocker", Image: "busybox:latest",
					Command: []string{"sh", "-c", "exit 0"},
					VolumeMounts: []containers.CreateContainerVolumeMount{
						{Type: containers.NamedVolumeMount, Source: resourceName, Target: "/data"},
					},
				})
				require.NoError(t, createBlockerErr)
				runtimeIDs = append(runtimeIDs, blockerID)
				_, startBlockerErr := server.ContainerOrchestrator.StartContainers(ctx, containers.StartContainersOptions{Containers: []string{blockerID}})
				require.NoError(t, startBlockerErr)
				_, stopBlockerErr := server.ContainerOrchestrator.StopContainers(ctx, containers.StopContainersOptions{Containers: []string{blockerID}})
				require.NoError(t, stopBlockerErr)
				refused := submitVolumeReset(t, ctx, server.Client, running)
				require.Equal(t, "Failed", refused.Status.State)
				require.False(t, refused.Status.ContainerRemoved)
				_ = ensureContainerState(t, ctx, server.Client, running, apiv1.ContainerStateRunning)
				require.Contains(t, refused.Status.Message, resourceName+"-blocker")
				runVolumeResetCommand(t, ctx, server.ContainerOrchestrator, originalRuntime.Id, "test -s /data/marker")
				_, removeBlockerErr := server.ContainerOrchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{Containers: []string{blockerID}})
				require.NoError(t, removeBlockerErr)
				runtimeIDs = runtimeIDs[:1]
				resumed, adoptedRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, running)
				require.Equal(t, running.UID, resumed.UID)
				require.Equal(t, originalRuntime.Id, adoptedRuntime.Id)
				runVolumeResetCommand(t, ctx, server.ContainerOrchestrator, adoptedRuntime.Id, "test -s /data/marker")
				running = resumed
				require.NoError(t, server.Client.Delete(ctx, refused))
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, refused)
			}
			fault.failCreates.Store(scenario.recovery)
			reset := submitVolumeReset(t, ctx, server.Client, running)
			if reset.Status.ContainerRemoved {
				runtimeIDs = nil
			}
			if scenario.recovery {
				require.Equal(t, "Failed", reset.Status.State)
				require.Contains(t, reset.Status.Message, "old-generation cleanup is independent")
				waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(volume), func(updated *apiv1.ContainerVolume) (bool, error) {
					return updated.Status.State == apiv1.ContainerVolumeStatePending, nil
				})
			} else {
				require.Equal(t, "Succeeded", reset.Status.State, reset.Status.Message)
			}
			if scenario.recovery {
				priorAttempts := fault.volumeCreateAttempts.Load()
				waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(running), func(updated *apiv1.Container) (bool, error) {
					return updated.Status.State != apiv1.ContainerStateRunning && fault.volumeCreateAttempts.Load() > priorAttempts, nil
				})
				require.Equal(t, int32(1), fault.containerCreates.Load(), "startup must not auto-create a missing volume")
				fault.failCreates.Store(false)
			}
			resumed, recreatedRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, running)
			require.Equal(t, running.UID, resumed.UID)
			runtimeIDs = append(runtimeIDs, recreatedRuntime.Id)
			require.NotEqual(t, originalRuntime.Id, recreatedRuntime.Id)
			runVolumeResetCommand(t, ctx, server.ContainerOrchestrator, recreatedRuntime.Id,
				"test ! -e /data/marker && test -z \"$(ls -A /data)\"")
			after := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			requireSameVolumeOwnership(t, before, after)
			unchanged := &apiv1.ContainerVolumeReset{}
			require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(reset), unchanged))
			require.Equal(t, reset.Status, unchanged.Status)
			require.NoError(t, server.Client.Delete(ctx, reset))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, reset)
			if scenario.recovery {
				nextReset := submitVolumeReset(t, ctx, server.Client, resumed)
				require.Equal(t, "Succeeded", nextReset.Status.State, nextReset.Status.Message)
				_, finalRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, resumed)
				runtimeIDs = []string{finalRuntime.Id}
				require.NoError(t, server.Client.Delete(ctx, nextReset))
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, nextReset)
			}
			require.NoError(t, server.Client.Delete(ctx, resumed))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resumed)
			if !persistent {
				runtimeIDs = nil
			}
		})
	}
}

// Verifies that resetting an adopted persistent container without original creation evidence
// is refused even when its labels claim an earlier DCP API UID.
func TestContainerVolumeResetUnownedPersistentContainer(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, startErr := StartTestEnvironment(t, ctx, ContainerController|VolumeController,
		"VolumeResetUnownedPersistentContainer", t.TempDir())
	require.NoError(t, startErr)
	volume := &apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"},
		Spec:       apiv1.ContainerVolumeSpec{Name: "reset-volume"},
	}
	require.NoError(t, server.Client.Create(ctx, volume))
	before := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	mounts := []apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"}}
	externalID, createExternalErr := server.ContainerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: "reset-target", Image: "reset-image",
		Labels: map[string]string{
			"com.microsoft.developer.usvc-dev.uid":        "earlier-api-uid",
			"com.microsoft.developer.usvc-dev.persistent": "true",
		},
		VolumeMounts: []containers.CreateContainerVolumeMount{
			{Type: containers.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"},
		},
	})
	require.NoError(t, createExternalErr)
	_, startExternalErr := server.ContainerOrchestrator.StartContainers(ctx, containers.StartContainersOptions{Containers: []string{externalID}})
	require.NoError(t, startExternalErr)
	target, adoptedRuntime := createVolumeResetContainer(t, ctx, server, true, mounts)
	require.Equal(t, externalID, adoptedRuntime.Id)
	refused := submitVolumeReset(t, ctx, server.Client, target)
	require.Equal(t, "Failed", refused.Status.State)
	require.Contains(t, refused.Status.Message, "not owned by this workload")
	require.False(t, refused.Status.ContainerRemoved)
	after := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	require.Equal(t, before.CreatedAt, after.CreatedAt)
}

// Verifies that Container volume reset refuses adopted volumes, another workload's volumes,
// and bind-only containers without destroying the target or changing storage.
func TestContainerVolumeResetOwnershipRefusal(t *testing.T) {
	for _, scenario := range []string{"adopted", "other-workload", "other-container-workload", "bind-only"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			server, environment, startErr := StartTestEnvironmentWithOptions(t, ctx,
				ContainerController|VolumeController, "VolumeResetRefused"+scenario, t.TempDir(),
				TestEnvironmentOptions{WorkloadID: "reset-workload"})
			require.NoError(t, startErr)
			persistent := true
			volume := &apiv1.ContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"},
				Spec:       apiv1.ContainerVolumeSpec{Name: "reset-volume", Persistent: &persistent},
			}
			if scenario == "adopted" {
				require.NoError(t, server.ContainerOrchestrator.CreateVolume(ctx,
					containers.CreateVolumeOptions{Name: volume.Spec.Name}))
			}
			require.NoError(t, server.Client.Create(ctx, volume))
			before := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			if scenario == "other-workload" {
				record, recordErr := environment.StateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
				require.NoError(t, recordErr)
				record.WorkloadID = "other-workload"
				require.NoError(t, environment.StateStore.UpsertPersistentVolume(ctx, *record))
			}
			mounts := []apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"}}
			if scenario == "bind-only" {
				mounts = []apiv1.VolumeMount{{Type: apiv1.BindMount, Source: t.TempDir(), Target: "/data"}}
			}
			target, targetRuntime := createVolumeResetContainer(t, ctx, server, scenario == "other-container-workload", mounts)
			if scenario == "other-container-workload" {
				record, recordErr := environment.StateStore.GetPersistentContainer(ctx, target.GetLeaseKey())
				require.NoError(t, recordErr)
				record.WorkloadID = "other-workload"
				require.NoError(t, environment.StateStore.UpsertPersistentContainer(ctx, *record))
			}
			result := submitVolumeReset(t, ctx, server.Client, target).Status
			require.Equal(t, "Failed", result.State)
			require.NotEmpty(t, result.Message)
			require.False(t, result.ContainerRemoved)
			inspected, inspectErr := server.ContainerOrchestrator.InspectContainers(ctx,
				containers.InspectContainersOptions{Containers: []string{targetRuntime.Id}})
			require.NoError(t, inspectErr)
			require.Equal(t, containers.ContainerStatusRunning, inspected[0].Status)
			after := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			require.Equal(t, before.CreatedAt, after.CreatedAt)
		})
	}
}

type volumeResetRuntimeFailure struct {
	containers.ContainerOrchestrator
	scenario string
	creates  atomic.Int32
}

type recoverableVolumeResetOrchestrator struct {
	containers.ContainerOrchestrator
	failCreates          atomic.Bool
	volumeCreateAttempts atomic.Int32
	containerCreates     atomic.Int32
}

func (o *recoverableVolumeResetOrchestrator) CreateVolume(ctx context.Context, options containers.CreateVolumeOptions) error {
	o.volumeCreateAttempts.Add(1)
	if o.failCreates.Load() {
		return containers.ErrAlreadyExists
	}
	return o.ContainerOrchestrator.CreateVolume(ctx, options)
}

func (o *recoverableVolumeResetOrchestrator) CreateContainer(ctx context.Context, options containers.CreateContainerOptions) (string, error) {
	o.containerCreates.Add(1)
	return o.ContainerOrchestrator.CreateContainer(ctx, options)
}

// Verifies that ContainerVolume recovery preserves ownership after a failed reset, blocks container
// startup until storage is repaired, leaves the reset Failed, and permits a subsequent reset.
func TestContainerVolumeResetRecreationRecovery(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		persistent  bool
		earlyResume bool
		expire      bool
	}{
		{name: "session"},
		{name: "persistent", persistent: true},
		{name: "session-early-resume", earlyResume: true},
		{name: "persistent-early-resume", persistent: true, earlyResume: true},
		{name: "session-gc-repair", earlyResume: true, expire: true},
		{name: "persistent-gc-repair", persistent: true, earlyResume: true, expire: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var fault *recoverableVolumeResetOrchestrator
			persistent := scenario.persistent
			base := time.Now().UTC().Truncate(time.Microsecond)
			var elapsed atomic.Int64
			var clockReads atomic.Int64
			var resetClock func() time.Time
			if scenario.expire {
				resetClock = func() time.Time {
					clockReads.Add(1)
					return base.Add(time.Duration(elapsed.Load()))
				}
			}
			server, environment, startErr := StartTestEnvironmentWithOptions(t, ctx,
				ContainerController|VolumeController, "VolumeResetRepair"+scenario.name, t.TempDir(), TestEnvironmentOptions{
					WorkloadID:       "reset-recovery-workload",
					VolumeResetClock: resetClock,
					DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
						fault = &recoverableVolumeResetOrchestrator{ContainerOrchestrator: orchestrator}
						return fault
					},
				})
			require.NoError(t, startErr)
			volume := &apiv1.ContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"},
				Spec:       apiv1.ContainerVolumeSpec{Name: "reset-volume", Persistent: &persistent},
			}
			require.NoError(t, server.Client.Create(ctx, volume))
			before := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			target, _ := createVolumeResetContainer(t, ctx, server, persistent,
				[]apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"}})
			fault.failCreates.Store(true)
			failed := submitVolumeReset(t, ctx, server.Client, target)
			require.Equal(t, "Failed", failed.Status.State)
			require.True(t, failed.Status.ContainerRemoved)
			require.Contains(t, failed.Status.Message, "old-generation cleanup is independent")
			waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(volume), func(updated *apiv1.ContainerVolume) (bool, error) {
				return updated.Status.State == apiv1.ContainerVolumeStatePending, nil
			})
			preserved, preservedErr := server.ContainerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volume.Spec.Name}})
			require.NoError(t, preservedErr)
			require.Equal(t, before.CreatedAt, preserved[0].CreatedAt)
			if persistent {
				record, recordErr := environment.StateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
				require.NoError(t, recordErr)
				require.Equal(t, before.Labels[containers.VolumeOwnershipTokenLabel], record.OwnershipToken)
			}
			if !scenario.earlyResume {
				fault.failCreates.Store(false)
				repairedBeforeResume := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
				requireSameVolumeOwnership(t, before, repairedBeforeResume)
				stillFailed := &apiv1.ContainerVolumeReset{}
				require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(failed), stillFailed))
				require.Equal(t, failed.Status, stillFailed.Status, "repair must not change the terminal reset outcome")
			}
			if scenario.earlyResume {
				if scenario.expire {
					waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(failed), func(updated *apiv1.ContainerVolumeReset) (bool, error) {
						return updated.Status.State == "Failed" && clockReads.Load() >= 3, nil
					})
					elapsed.Store(int64(controllers.ContainerVolumeResetTerminalRetention))
					expiryPatch := ctrl_client.MergeFrom(failed.DeepCopy())
					failed.Annotations = map[string]string{"retention-check": "expire-during-repair"}
					require.NoError(t, server.Client.Patch(ctx, failed, expiryPatch))
				} else {
					require.NoError(t, server.Client.Delete(ctx, failed))
				}
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, failed)
				priorAttempts := fault.volumeCreateAttempts.Load()
				waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(target), func(updated *apiv1.Container) (bool, error) {
					require.NotEqual(t, apiv1.ContainerStateFailedToStart, updated.Status.State)
					return updated.Status.State != apiv1.ContainerStateRunning && fault.volumeCreateAttempts.Load() > priorAttempts, nil
				})
				require.Equal(t, int32(1), fault.containerCreates.Load(), "no container may auto-create missing storage")
				fault.failCreates.Store(false)
			}
			repaired := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			requireSameVolumeOwnership(t, before, repaired)
			running, _ := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
			require.Equal(t, target.UID, running.UID)
			if !scenario.earlyResume {
				stillFailed := &apiv1.ContainerVolumeReset{}
				require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(failed), stillFailed))
				require.Equal(t, failed.Status, stillFailed.Status)
			}
			succeeded := submitVolumeReset(t, ctx, server.Client, running)
			require.Equal(t, "Succeeded", succeeded.Status.State, succeeded.Status.Message)
			require.Equal(t, []string{volume.Spec.Name}, succeeded.Status.Volumes)
		})
	}
}

// Verifies that a fresh controller manager honors a persisted volume ownership token while its
// physical volume is missing, waits instead of auto-creating storage, and refuses mismatched ownership.
func TestContainerVolumeResetPersistentRecoveryWithoutRegistry(t *testing.T) {
	for _, scenario := range []string{"missing", "conflicting"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var fault *recoverableVolumeResetOrchestrator
			server, environment, startErr := StartTestEnvironmentWithOptions(t, ctx,
				ContainerController|VolumeController, "VolumeResetNewManager"+scenario, t.TempDir(), TestEnvironmentOptions{
					WorkloadID: "reset-recovery-workload",
					DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
						fault = &recoverableVolumeResetOrchestrator{ContainerOrchestrator: orchestrator}
						fault.failCreates.Store(true)
						return fault
					},
				})
			require.NoError(t, startErr)
			persistent := true
			volume := &apiv1.ContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"},
				Spec:       apiv1.ContainerVolumeSpec{Name: "reset-volume", Persistent: &persistent},
			}
			const token = "persisted-ownership-token"
			require.NoError(t, environment.StateStore.UpsertPersistentVolume(ctx, statestore.PersistentVolumeRecord{
				ResourceKey: volume.GetLeaseKey(), VolumeName: volume.Spec.Name,
				RuntimeName: server.ContainerOrchestrator.Name(), WorkloadID: "reset-recovery-workload",
				OwnershipToken: token,
			}))
			if scenario == "conflicting" {
				require.NoError(t, fault.ContainerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
					Name: volume.Spec.Name, Labels: map[string]string{containers.VolumeOwnershipTokenLabel: "another-token"},
				}))
			}
			target := &apiv1.Container{
				ObjectMeta: metav1.ObjectMeta{Name: "reset-target"},
				Spec: apiv1.ContainerSpec{Image: "reset-image", ContainerName: "reset-target",
					VolumeMounts: []apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"}}},
			}
			require.NoError(t, server.Client.Create(ctx, target))
			waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(target), func(updated *apiv1.Container) (bool, error) {
				if scenario == "conflicting" {
					return updated.Status.State == apiv1.ContainerStateFailedToStart, nil
				}
				return len(updated.Finalizers) != 0, nil
			})
			require.Zero(t, fault.containerCreates.Load())
			if scenario == "conflicting" {
				return
			}
			require.NoError(t, server.Client.Create(ctx, volume))
			waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(volume), func(updated *apiv1.ContainerVolume) (bool, error) {
				return fault.volumeCreateAttempts.Load() > 1, nil
			})
			require.Zero(t, fault.containerCreates.Load())
			fault.failCreates.Store(false)
			repaired := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			require.Equal(t, token, repaired.Labels[containers.VolumeOwnershipTokenLabel])
			running, _ := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
			nextReset := submitVolumeReset(t, ctx, server.Client, running)
			require.Equal(t, "Succeeded", nextReset.Status.State, nextReset.Status.Message)
		})
	}
}

func (o *volumeResetRuntimeFailure) RemoveVolumes(ctx context.Context, options containers.RemoveVolumesOptions) ([]string, error) {
	if o.scenario == "remove" && len(options.Volumes) == 1 && options.Volumes[0] == "reset-volume" {
		return nil, containers.ErrObjectInUse
	}
	return o.ContainerOrchestrator.RemoveVolumes(ctx, options)
}

func (o *volumeResetRuntimeFailure) CreateVolume(ctx context.Context, options containers.CreateVolumeOptions) error {
	if o.creates.Add(1) > 1 && o.scenario == "recreate" {
		return containers.ErrAlreadyExists
	}
	return o.ContainerOrchestrator.CreateVolume(ctx, options)
}

func (o *volumeResetRuntimeFailure) ListContainers(ctx context.Context, options containers.ListContainersOptions) ([]containers.ListedContainer, error) {
	if options.All && len(options.Filters.LabelFilters) == 0 {
		switch o.scenario {
		case "cancel-preflight":
			return nil, context.Canceled
		case "list-preflight":
			return nil, errors.New("runtime container enumeration failed")
		}
	}
	return o.ContainerOrchestrator.ListContainers(ctx, options)
}

// Verifies that Container volume reset surfaces selection, enumeration, and cancellation failures,
// reports partial progress, and treats deferred retirement independently of fresh selection.
func TestContainerVolumeResetRuntimeFailure(t *testing.T) {
	for _, scenario := range []string{"remove", "recreate", "cancel-preflight", "list-preflight"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx, ContainerController|VolumeController,
				"VolumeResetRuntimeFailure"+scenario, t.TempDir(), TestEnvironmentOptions{
					DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
						return &volumeResetRuntimeFailure{ContainerOrchestrator: orchestrator, scenario: scenario}
					},
				})
			require.NoError(t, startErr)
			persistent := false
			volume := &apiv1.ContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"},
				Spec:       apiv1.ContainerVolumeSpec{Name: "reset-volume", Persistent: &persistent},
			}
			require.NoError(t, server.Client.Create(ctx, volume))
			before := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			target, originalRuntime := createVolumeResetContainer(t, ctx, server, false,
				[]apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"}})
			updated := submitVolumeReset(t, ctx, server.Client, target)
			if scenario == "remove" {
				require.Equal(t, "Succeeded", updated.Status.State, updated.Status.Message)
				after := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
				require.NotEqual(t, before.Name, after.Name)
				preserved, preservedErr := server.ContainerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{before.Name}})
				require.NoError(t, preservedErr)
				require.Equal(t, before.CreatedAt, preserved[0].CreatedAt)
				return
			}
			require.Equal(t, "Failed", updated.Status.State)
			require.False(t, updated.Status.FinishTimestamp.IsZero())
			require.Empty(t, updated.Status.Volumes)
			if scenario == "cancel-preflight" || scenario == "list-preflight" {
				require.False(t, updated.Status.ContainerRemoved)
				require.Contains(t, updated.Status.Message, "list runtime volume consumers")
				if scenario == "cancel-preflight" {
					require.Contains(t, updated.Status.Message, "context canceled")
				} else {
					require.Contains(t, updated.Status.Message, "runtime container enumeration failed")
				}
				unchanged, currentRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
				require.Equal(t, target.UID, unchanged.UID)
				require.Equal(t, originalRuntime.Id, currentRuntime.Id)
			} else {
				require.True(t, updated.Status.ContainerRemoved)
			}
			if scenario == "recreate" {
				require.Contains(t, updated.Status.Message, "old-generation cleanup is independent")
				_, volumeInspectErr := server.ContainerOrchestrator.InspectVolumes(ctx,
					containers.InspectVolumesOptions{Volumes: []string{volume.Spec.Name}})
				require.NoError(t, volumeInspectErr)
			} else {
				after := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
				require.Equal(t, before.CreatedAt, after.CreatedAt)
			}
		})
	}
}

// Verifies that selected generations retain the original volume's ownership identity.
func requireSameVolumeOwnership(t *testing.T, before, after containers.InspectedVolume) {
	t.Helper()
	for key, value := range before.Labels {
		if key == "com.microsoft.developer.usvc-dev.volumeGeneration" {
			continue
		}
		require.Equal(t, value, after.Labels[key], "ownership label %s", key)
	}
}

func cleanupVolumeResetGenerations(t *testing.T, orchestrator containers.VolumeOrchestrator, logicalName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), defaultIntegrationTestTimeout)
	defer cancel()
	listed, listErr := orchestrator.ListVolumes(ctx, containers.ListVolumesOptions{
		Filters: containers.ListVolumesFilters{LabelFilters: []containers.LabelFilter{{Key: containers.VolumeLogicalNameLabel, Value: logicalName}}},
	})
	if listErr != nil {
		t.Error(listErr)
		return
	}
	names := map[string]bool{logicalName: true}
	for _, candidate := range listed {
		names[candidate.Name] = true
	}
	for name := range names {
		_, removeErr := orchestrator.RemoveVolumes(ctx, containers.RemoveVolumesOptions{Volumes: []string{name}})
		if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
			t.Error(removeErr)
		}
	}
}
