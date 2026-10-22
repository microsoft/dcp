/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/testutil"
)

type partialGenerationOrchestrator struct {
	containers.ContainerOrchestrator
}

// Verifies that ordinary adoption of another instance's labeled persistent head does not
// authorize retiring any of its older generations without workload or creation evidence.
func TestContainerVolumeAdoptedGenerationsPreserved(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, startErr := StartTestEnvironment(t, ctx, VolumeController, "AdoptedGenerations", t.TempDir())
	require.NoError(t, startErr)
	for _, entry := range []struct{ name, generation string }{{"external", "0"}, {"external-dcp-1", "1"}} {
		require.NoError(t, server.ContainerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
			Name: entry.name, Labels: map[string]string{
				"com.microsoft.developer.usvc-dev.uid": "earlier-instance",
				containers.VolumeLogicalNameLabel:      "external", containers.VolumeGenerationLabel: entry.generation,
			},
		}))
	}
	persistent := true
	volume := &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: "external"},
		Spec: apiv1.ContainerVolumeSpec{Name: "external", Persistent: &persistent}}
	require.NoError(t, server.Client.Create(ctx, volume))
	selected := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	require.Equal(t, "external-dcp-1", selected.Name)
	require.NoError(t, server.Client.Delete(ctx, volume))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, volume)
	preserved, preservedErr := server.ContainerOrchestrator.InspectVolumes(ctx,
		containers.InspectVolumesOptions{Volumes: []string{"external", "external-dcp-1"}})
	require.NoError(t, preservedErr)
	require.Len(t, preserved, 2)
}

// Verifies that selecting an already-created owned head commits its persistent mapping and
// retirement cannot use a matching API UID to override a conflicting ownership token.
func TestContainerVolumeGenerationHeadCommit(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, environment, startErr := StartTestEnvironmentWithOptions(t, ctx, VolumeController, "HeadCommit", t.TempDir(),
		TestEnvironmentOptions{WorkloadID: "head-workload"})
	require.NoError(t, startErr)
	persistent := true
	volume := &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: apiv1.ContainerVolumeSpec{Name: "data", Persistent: &persistent}}
	require.NoError(t, server.Client.Create(ctx, volume))
	original := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	for _, entry := range []struct{ name, generation, token string }{
		{"data-dcp-1", "1", "another-workload"},
		{"data-dcp-2", "2", original.Labels[containers.VolumeOwnershipTokenLabel]},
	} {
		labels := maps.Clone(original.Labels)
		labels[containers.VolumeGenerationLabel] = entry.generation
		labels[containers.VolumeOwnershipTokenLabel] = entry.token
		require.NoError(t, server.ContainerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: entry.name, Labels: labels}))
	}
	current := &apiv1.ContainerVolume{}
	require.NoError(t, server.Client.Get(ctx, volume.NamespacedName(), current))
	patch := ctrl_client.MergeFrom(current.DeepCopy())
	current.Spec.Generation = 2
	require.NoError(t, server.Client.Patch(ctx, current, patch))
	selected := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	require.Equal(t, "data-dcp-2", selected.Name)
	record, recordErr := environment.StateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, selected.Name, record.VolumeName)
	_, foreignErr := server.ContainerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{"data-dcp-1"}})
	require.NoError(t, foreignErr)
}

// Verifies that recreating persistent API resources keeps the selected head and does not
// adopt a physical container mounted on an obsolete generation, even with a custom lifecycle key.
func TestContainerVolumeResetPersistentGenerationAdoption(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, startErr := StartTestEnvironment(t, ctx, ContainerController|VolumeController, "GenerationAdoption", t.TempDir())
	require.NoError(t, startErr)
	persistent := true
	volume := &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: apiv1.ContainerVolumeSpec{Name: "data", Persistent: &persistent}}
	require.NoError(t, server.Client.Create(ctx, volume))
	_ = ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	target := &apiv1.Container{ObjectMeta: metav1.ObjectMeta{Name: "reset-target"}, Spec: apiv1.ContainerSpec{
		Image: "reset-image", ContainerName: "reset-target", Persistent: true, LifecycleKey: "custom-key",
		VolumeMounts: []apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: "data", Target: "/data"}},
	}}
	require.NoError(t, server.Client.Create(ctx, target))
	target, original := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
	currentVolume := &apiv1.ContainerVolume{}
	require.NoError(t, server.Client.Get(ctx, volume.NamespacedName(), currentVolume))
	advancePatch := ctrl_client.MergeFrom(currentVolume.DeepCopy())
	currentVolume.Spec.Generation = 1
	require.NoError(t, server.Client.Patch(ctx, currentVolume, advancePatch))
	selected := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	require.Equal(t, "data-dcp-1", selected.Name)
	require.NoError(t, server.Client.Delete(ctx, target))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, target)
	require.NoError(t, server.Client.Delete(ctx, volume))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, volume)
	replacementVolume := &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: "data"}, Spec: volume.Spec}
	require.NoError(t, server.Client.Create(ctx, replacementVolume))
	adoptedVolume := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, replacementVolume)
	require.Equal(t, selected.Name, adoptedVolume.Name)
	replacement := &apiv1.Container{ObjectMeta: metav1.ObjectMeta{Name: target.Name}, Spec: target.Spec}
	require.NoError(t, server.Client.Create(ctx, replacement))
	replacement, physical := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, replacement)
	require.NotEqual(t, original.Id, physical.Id)
	require.True(t, len(physical.Mounts) > 0)
	require.Equal(t, selected.Name, physical.Mounts[0].Source)
	reset := submitVolumeReset(t, ctx, server.Client, replacement)
	require.Equal(t, "Succeeded", reset.Status.State, reset.Status.Message)
	require.Equal(t, int64(2), reset.Status.VolumeGenerations[0].RequestedGeneration)
}

// Verifies that a physical generation-name collision is never adopted as fresh storage,
// even when its generation labels are valid, and that the original volume is preserved.
func TestContainerVolumeResetGenerationCollision(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, startErr := StartTestEnvironment(t, ctx, ContainerController|VolumeController, "GenerationCollision", t.TempDir())
	require.NoError(t, startErr)
	volume := &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: "collision"}, Spec: apiv1.ContainerVolumeSpec{Name: "collision"}}
	require.NoError(t, server.Client.Create(ctx, volume))
	old := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	require.NoError(t, server.ContainerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
		Name: "collision-dcp-1", Labels: map[string]string{
			containers.VolumeLogicalNameLabel: "collision", containers.VolumeGenerationLabel: "1",
		},
	}))
	target, _ := createVolumeResetContainer(t, ctx, server, false,
		[]apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: "collision", Target: "/data"}})
	result := submitVolumeReset(t, ctx, server.Client, target)
	require.Equal(t, "Failed", result.Status.State, result.Status.Message)
	require.True(t, result.Status.ContainerRemoved)
	require.Contains(t, result.Status.Message, "conflicting ownership")
	require.Empty(t, result.Status.Volumes)
	preserved, preservedErr := server.ContainerOrchestrator.InspectVolumes(ctx,
		containers.InspectVolumesOptions{Volumes: []string{old.Name, "collision-dcp-1"}})
	require.NoError(t, preservedErr)
	require.Len(t, preserved, 2)
}

func (o *partialGenerationOrchestrator) CreateVolume(ctx context.Context, options containers.CreateVolumeOptions) error {
	if options.Name == "b-volume-dcp-1" {
		return errors.New("injected fresh storage failure")
	}
	return o.ContainerOrchestrator.CreateVolume(ctx, options)
}

// Verifies that a multi-volume selection failure reports accepted requests and successful
// selections independently, preserves unselected old storage, and blocks target startup.
func TestContainerVolumeResetPartialGenerationFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, startErr := StartTestEnvironmentWithOptions(t, ctx, ContainerController|VolumeController,
		"PartialGenerations", t.TempDir(), TestEnvironmentOptions{
			DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
				return &partialGenerationOrchestrator{ContainerOrchestrator: orchestrator}
			},
		})
	require.NoError(t, startErr)
	var mounts []apiv1.VolumeMount
	for _, name := range []string{"a-volume", "b-volume"} {
		volume := &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: apiv1.ContainerVolumeSpec{Name: name}}
		require.NoError(t, server.Client.Create(ctx, volume))
		_ = ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
		mounts = append(mounts, apiv1.VolumeMount{Type: apiv1.NamedVolumeMount, Source: name, Target: "/" + name})
	}
	target, _ := createVolumeResetContainer(t, ctx, server, false, mounts)
	failed := submitVolumeReset(t, ctx, server.Client, target)
	require.Equal(t, "Failed", failed.Status.State, failed.Status.Message)
	require.True(t, failed.Status.ContainerRemoved)
	require.Contains(t, failed.Status.Message, "generation selection may be partial")
	require.Contains(t, failed.Status.Message, "b-volume")
	require.Len(t, failed.Status.VolumeGenerations, 2)
	for _, selection := range failed.Status.VolumeGenerations {
		require.True(t, selection.Requested)
		require.Equal(t, int64(1), selection.RequestedGeneration)
	}
	require.Contains(t, failed.Status.VolumeGenerations[1].Message, "injected fresh storage failure")
	preserved, preservedErr := server.ContainerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{"b-volume"}})
	require.NoError(t, preservedErr)
	require.Len(t, preserved, 1)
	selected := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator,
		&apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: "a-volume"}, Spec: apiv1.ContainerVolumeSpec{Name: "a-volume"}})
	require.Equal(t, "a-volume-dcp-1", selected.Name)
	waitObjectAssumesStateEx(t, ctx, server.Client, target.NamespacedName(), func(updated *apiv1.Container) (bool, error) {
		return updated.Status.State != apiv1.ContainerStateRunning && updated.Status.ContainerID == "", nil
	})
	unchanged := &apiv1.ContainerVolumeReset{}
	require.NoError(t, server.Client.Get(ctx, failed.NamespacedName(), unchanged))
	require.Equal(t, failed.Status, unchanged.Status)
}

// Verifies that a runtime consumer racing after preflight retains its old marker while the
// target mounts fresh storage, and retirement removes the old generation only after release.
func TestContainerVolumeResetRealRuntimeRetainedGeneration(t *testing.T) {
	testutil.SkipIfTrueContainerOrchestratorNotEnabled(t)
	dcppaths.EnableTestPathProbing()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var gate *gatedVolumeResetOrchestrator
	server, environment, startErr := StartAdvancedTestEnvironmentWithOptions(ctx, ContainerController|VolumeController,
		"RetainedGeneration", t.TempDir(), AdvancedTestEnvironmentOptions{
			ApiServerFlags: ctrl_testutil.ApiServerUseTrueContainerOrchestrator,
			DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
				gate = &gatedVolumeResetOrchestrator{ContainerOrchestrator: orchestrator, phase: "recreation",
					entered: make(chan struct{}, 1), release: make(chan struct{}, 1)}
				return gate
			},
		})
	require.NoError(t, startErr)
	defer environment.ProcessExecutor.Dispose()
	defer shutdownAdvancedTestEnvironment(t, ctx, cancel, server)
	suffix, suffixErr := randdata.MakeRandomString(8)
	require.NoError(t, suffixErr)
	name := "retained-generation-" + string(suffix)
	var runtimeIDs []string
	defer func() {
		for _, id := range runtimeIDs {
			inspected, inspectErr := server.ContainerOrchestrator.InspectContainers(context.Background(),
				containers.InspectContainersOptions{Containers: []string{id}})
			if len(inspected) == 0 && errors.Is(inspectErr, containers.ErrNotFound) {
				continue
			}
			_, removeErr := server.ContainerOrchestrator.RemoveContainers(context.Background(),
				containers.RemoveContainersOptions{Containers: []string{id}, Force: true})
			if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
				t.Error(removeErr)
			}
		}
		cleanupVolumeResetGenerations(t, server.ContainerOrchestrator, name)
	}()
	volume := &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: apiv1.ContainerVolumeSpec{Name: name}}
	require.NoError(t, server.Client.Create(ctx, volume))
	old := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	target := &apiv1.Container{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: apiv1.ContainerSpec{
		Image: "busybox:latest", ContainerName: name, Command: "sh", Args: []string{"-c", "sleep 600"},
		VolumeMounts: []apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: name, Target: "/data"}},
	}}
	require.NoError(t, server.Client.Create(ctx, target))
	running, original := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
	runtimeIDs = append(runtimeIDs, original.Id)
	runVolumeResetCommand(t, ctx, server.ContainerOrchestrator, original.Id, "printf retained > /data/marker")
	gate.armed.Store(true)
	reset := &apiv1.ContainerVolumeReset{ObjectMeta: metav1.ObjectMeta{Name: name + "-reset"},
		Spec: apiv1.ContainerVolumeResetSpec{ContainerName: running.Name, ContainerUID: running.UID}}
	require.NoError(t, server.Client.Create(ctx, reset))
	select {
	case <-gate.entered:
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	_, goneErr := server.ContainerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{original.Id}})
	require.ErrorIs(t, goneErr, containers.ErrNotFound)
	consumerID, consumerErr := server.ContainerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: name + "-late", Image: "busybox:latest", Command: []string{"sh", "-c", "sleep 600"},
		VolumeMounts: []containers.CreateContainerVolumeMount{{Type: containers.NamedVolumeMount, Source: old.Name, Target: "/data"}},
	})
	require.NoError(t, consumerErr)
	runtimeIDs = append(runtimeIDs, consumerID)
	_, startConsumerErr := server.ContainerOrchestrator.StartContainers(ctx, containers.StartContainersOptions{Containers: []string{consumerID}})
	require.NoError(t, startConsumerErr)
	gate.release <- struct{}{}
	completed := waitObjectAssumesStateEx(t, ctx, server.Client, reset.NamespacedName(), func(updated *apiv1.ContainerVolumeReset) (bool, error) {
		return updated.Status.State == "Succeeded" || updated.Status.State == "Failed", nil
	})
	require.Equal(t, "Succeeded", completed.Status.State, completed.Status.Message)
	resumed, fresh := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, running)
	runtimeIDs = append(runtimeIDs, fresh.Id)
	require.Equal(t, running.UID, resumed.UID)
	runVolumeResetCommand(t, ctx, server.ContainerOrchestrator, fresh.Id, "test -z \"$(ls -A /data)\"")
	runVolumeResetCommand(t, ctx, server.ContainerOrchestrator, consumerID, "test \"$(cat /data/marker)\" = retained")
	selected := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	require.NotEqual(t, old.Name, selected.Name)
	require.Equal(t, selected.Name, completed.Status.VolumeGenerations[0].SelectedVolumeName)
	_, removeConsumerErr := server.ContainerOrchestrator.RemoveContainers(ctx,
		containers.RemoveContainersOptions{Containers: []string{consumerID}, Force: true})
	require.NoError(t, removeConsumerErr)
	currentVolume := &apiv1.ContainerVolume{}
	require.NoError(t, server.Client.Get(ctx, volume.NamespacedName(), currentVolume))
	retirementPatch := ctrl_client.MergeFrom(currentVolume.DeepCopy())
	currentVolume.Annotations = map[string]string{"retire": "retry"}
	require.NoError(t, server.Client.Patch(ctx, currentVolume, retirementPatch))
	waitObjectAssumesStateEx(t, ctx, server.Client, volume.NamespacedName(), func(_ *apiv1.ContainerVolume) (bool, error) {
		_, inspectErr := server.ContainerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{old.Name}})
		return errors.Is(inspectErr, containers.ErrNotFound), nil
	})
	require.NoError(t, server.Client.Delete(ctx, resumed))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resumed)
}
