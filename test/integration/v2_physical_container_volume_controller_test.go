/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestV2PhysicalContainerVolumeControllerCreatesVolume(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-create")
	volumeName := "v2-pcv-created-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "created-volume", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerVolumeSpec{
			Volume: &apiv2.PhysicalContainerVolumeConfig{
				VolumeName: volumeName,
				Labels: []commonapi.Label{
					{Key: "test-label", Value: "test-value"},
					{Key: "com.microsoft.developer.usvc-dev.uid", Value: "caller-value"},
					{Key: controllers.PersistentLabel, Value: "caller-value"},
					{Key: controllers.CreatorProcessIdLabel, Value: "caller-value"},
					{Key: controllers.CreatorProcessStartTimeLabel, Value: "caller-value"},
				},
			},
		},
	}
	require.NoError(t, client.Create(ctx, volume))

	readyVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Contains(t, readyVolume.Finalizers, apiv2.GroupName+"/physicalcontainervolume-reconciler")
	require.Equal(t, volumeName, readyVolume.Status.VolumeID)
	require.Equal(t, volumeName, readyVolume.Status.VolumeName)
	require.Equal(t, "local", readyVolume.Status.Driver)
	require.Equal(t, "local", readyVolume.Status.Scope)
	require.False(t, readyVolume.Status.CreatedAt.IsZero())
	requireReadyCondition(t, readyVolume.Status.Conditions, metav1.ConditionTrue, apiv2.PhysicalContainerVolumeReasonVolumeAvailable)

	inspectedVolume := inspectRuntimeVolume(t, ctx, volumeName)
	require.Equal(t, "test-value", inspectedVolume.Labels["test-label"])
	require.Equal(t, string(readyVolume.UID), inspectedVolume.Labels["com.microsoft.developer.usvc-dev.uid"])
	require.Equal(t, "false", inspectedVolume.Labels[controllers.PersistentLabel])
	require.NotEmpty(t, inspectedVolume.Labels[controllers.CreatorProcessIdLabel])
	require.NotEqual(t, "caller-value", inspectedVolume.Labels[controllers.CreatorProcessIdLabel])
	require.NotEmpty(t, inspectedVolume.Labels[controllers.CreatorProcessStartTimeLabel])
	require.NotEqual(t, "caller-value", inspectedVolume.Labels[controllers.CreatorProcessStartTimeLabel])
}

func TestV2PhysicalContainerVolumeControllerRetainsReferencedVolume(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-track")
	volumeName := "v2-pcv-tracked-runtime"
	require.NoError(t, containerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: volumeName}))
	removeRuntimeVolumeOnCleanup(t, volumeName)

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "tracked-volume", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalContainerVolumeSpec{VolumeID: volumeName},
	}
	require.NoError(t, client.Create(ctx, volume))

	readyVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Equal(t, volumeName, readyVolume.Status.VolumeID)
	require.Equal(t, 1, containerOrchestrator.CreateVolumeCallCount(volumeName))

	require.NoError(t, client.Delete(ctx, volume))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerVolume](t, ctx, client, volume)
	require.NotNil(t, inspectRuntimeVolume(t, ctx, volumeName))
}

func TestV2PhysicalContainerVolumeControllerDeletesCreatedVolumesUnlessPersistent(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-delete")
	for _, persistent := range []bool{false, true} {
		name := "deleted"
		if persistent {
			name = "persistent"
		}
		volumeName := "v2-pcv-" + name + "-runtime"
		removeRuntimeVolumeOnCleanup(t, volumeName)
		volume := &apiv2.PhysicalContainerVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-volume", Namespace: namespace.Name},
			Spec: apiv2.PhysicalContainerVolumeSpec{
				Volume: &apiv2.PhysicalContainerVolumeConfig{
					VolumeName:          volumeName,
					RetainRuntimeVolume: persistent,
				},
			},
		}
		require.NoError(t, client.Create(ctx, volume))
		waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)

		require.NoError(t, client.Delete(ctx, volume))
		ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerVolume](t, ctx, client, volume)
		if persistent {
			inspectedVolume := inspectRuntimeVolume(t, ctx, volumeName)
			require.Equal(t, "true", inspectedVolume.Labels[controllers.PersistentLabel])
		} else {
			waitRuntimeVolumeMissing(t, ctx, volumeName)
		}
	}
}

func TestV2PhysicalContainerVolumeControllerWaitsForInUseVolume(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-in-use")
	volumeName := "v2-pcv-in-use-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "in-use-volume", Namespace: namespace.Name},
		Spec:       newPhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))
	waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)

	containerID, createErr := containerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:  "v2-pcv-in-use-container",
		Image: "v2-pcv-in-use-image",
		VolumeMounts: []containers.CreateContainerVolumeMount{{
			Type:   containers.NamedVolumeMount,
			Source: volumeName,
			Target: "/data",
		}},
	})
	require.NoError(t, createErr)
	removeRuntimeContainerOnCleanup(t, containerID)

	require.NoError(t, client.Delete(ctx, volume))
	terminatingVolume := waitPhysicalContainerVolumeReason(
		t,
		ctx,
		volume.NamespacedName(),
		apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoveFailed,
	)
	require.Equal(t, apiv2.PhysicalContainerVolumePhasePending, terminatingVolume.Status.Phase)
	require.Contains(t, terminatingVolume.Finalizers, apiv2.GroupName+"/physicalcontainervolume-reconciler")
	require.NotNil(t, inspectRuntimeVolume(t, ctx, volumeName))
	require.Len(t, inspectRuntimeContainers(t, ctx, containerID), 1)

	_, removeErr := containerOrchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{containerID},
		Force:      true,
	})
	require.NoError(t, removeErr)
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerVolume](t, ctx, client, volume)
	waitRuntimeVolumeMissing(t, ctx, volumeName)
}

func TestV2PhysicalContainerVolumeControllerCleansUpOnNamespaceDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-ns-cleanup")
	volumeName := "v2-pcv-ns-cleanup-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "namespace-volume", Namespace: namespace.Name},
		Spec:       newPhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))
	waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)

	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerVolume](t, ctx, client, volume)
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
	waitRuntimeVolumeMissing(t, ctx, volumeName)
}

func TestV2PhysicalContainerVolumeControllerDoesNotDuplicateCreate(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-single-create")
	volumeName := "v2-pcv-single-create-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)
	releaseCreate := containerOrchestrator.BlockCreateVolume(volumeName)
	defer releaseCreate()

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "single-create-volume", Namespace: namespace.Name},
		Spec:       newPhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))
	waitCreateVolumeCallCount(t, ctx, volumeName, 1)
	pendingVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhasePending)
	requireReadyCondition(t, pendingVolume.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonCreating)

	require.Never(t, func() bool {
		return containerOrchestrator.CreateVolumeCallCount(volumeName) > 1
	}, 3*time.Second, 250*time.Millisecond)

	releaseCreate()
	waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Equal(t, 1, containerOrchestrator.CreateVolumeCallCount(volumeName))
}

func TestV2PhysicalContainerVolumeControllerWaitsForCreateBeforeDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-delete-during-create")
	volumeName := "v2-pcv-delete-during-create-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)
	releaseCreate := containerOrchestrator.BlockCreateVolume(volumeName)
	defer releaseCreate()

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "delete-during-create-volume", Namespace: namespace.Name},
		Spec:       newPhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))
	waitCreateVolumeCallCount(t, ctx, volumeName, 1)
	require.NoError(t, client.Delete(ctx, volume))

	terminatingVolume := waitObjectAssumesState(t, ctx, volume.NamespacedName(), func(current *apiv2.PhysicalContainerVolume) (bool, error) {
		return current.DeletionTimestamp != nil && !current.DeletionTimestamp.IsZero(), nil
	})
	require.Contains(t, terminatingVolume.Finalizers, apiv2.GroupName+"/physicalcontainervolume-reconciler")
	requireReadyCondition(t, terminatingVolume.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonCreating)
	require.Equal(t, 0, containerOrchestrator.RemoveVolumeCallCount(volumeName))

	releaseCreate()
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerVolume](t, ctx, client, volume)
	waitRuntimeVolumeMissing(t, ctx, volumeName)
	require.Equal(t, 1, containerOrchestrator.CreateVolumeCallCount(volumeName))
	require.Equal(t, 1, containerOrchestrator.RemoveVolumeCallCount(volumeName))
}

func TestV2PhysicalContainerVolumeControllerAdoptsVolumeAfterUncertainCreateFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-uncertain")
	volumeName := "v2-pcv-uncertain-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)
	containerOrchestrator.FailNextCreateVolumeAfterCreation(volumeName, errors.New("simulated lost create response"))

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "uncertain-volume", Namespace: namespace.Name},
		Spec:       newPhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))

	waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Equal(t, 1, containerOrchestrator.CreateVolumeCallCount(volumeName))
}

func TestV2PhysicalContainerVolumeControllerReportsTerminalNameCollision(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-collision")
	volumeName := "v2-pcv-collision-runtime"
	require.NoError(t, containerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: volumeName}))
	removeRuntimeVolumeOnCleanup(t, volumeName)
	initialRemoveCount := containerOrchestrator.RemoveVolumeCallCount(volumeName)

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "collision-volume", Namespace: namespace.Name},
		Spec:       newPhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))

	failedVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseFailed)
	requireReadyCondition(t, failedVolume.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonCreateFailed)
	require.Never(t, func() bool {
		return containerOrchestrator.CreateVolumeCallCount(volumeName) > 2
	}, 3*time.Second, 250*time.Millisecond)
	require.Equal(t, initialRemoveCount, containerOrchestrator.RemoveVolumeCallCount(volumeName))
	require.NotNil(t, inspectRuntimeVolume(t, ctx, volumeName))
}

func TestV2PhysicalContainerVolumeControllerReplacesAndPersistsExistingVolume(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-replace")
	volumeName := "v2-pcv-replace-runtime"
	require.NoError(t, containerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: volumeName}))
	removeRuntimeVolumeOnCleanup(t, volumeName)

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "replacement-volume", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerVolumeSpec{
			Volume: &apiv2.PhysicalContainerVolumeConfig{
				VolumeName:          volumeName,
				RetainRuntimeVolume: true,
				ReplaceExisting:     true,
			},
		},
	}
	require.NoError(t, client.Create(ctx, volume))

	readyVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Equal(t, volumeName, readyVolume.Status.VolumeID)
	require.Equal(t, 2, containerOrchestrator.CreateVolumeCallCount(volumeName))
	require.Equal(t, 1, containerOrchestrator.RemoveVolumeCallCount(volumeName))
	replacement := inspectRuntimeVolume(t, ctx, volumeName)
	require.Equal(t, string(readyVolume.UID), replacement.Labels["com.microsoft.developer.usvc-dev.uid"])
	require.Equal(t, "true", replacement.Labels[controllers.PersistentLabel])

	require.NoError(t, client.Delete(ctx, volume))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerVolume](t, ctx, client, volume)
	require.NotNil(t, inspectRuntimeVolume(t, ctx, volumeName))
}

func TestV2PhysicalContainerVolumeControllerRetriesInUseReplacementWithoutRemovingContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-replace-in-use")
	volumeName := "v2-pcv-replace-in-use-runtime"
	require.NoError(t, containerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: volumeName}))
	removeRuntimeVolumeOnCleanup(t, volumeName)
	containerID, createContainerErr := containerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:  "v2-pcv-replace-in-use-container",
		Image: "v2-pcv-replace-in-use-image",
		VolumeMounts: []containers.CreateContainerVolumeMount{{
			Type:   containers.NamedVolumeMount,
			Source: volumeName,
			Target: "/data",
		}},
	})
	require.NoError(t, createContainerErr)
	removeRuntimeContainerOnCleanup(t, containerID)

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "in-use-replacement-volume", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerVolumeSpec{
			Volume: &apiv2.PhysicalContainerVolumeConfig{
				VolumeName:      volumeName,
				ReplaceExisting: true,
			},
		},
	}
	require.NoError(t, client.Create(ctx, volume))
	failedVolume := waitPhysicalContainerVolumeReason(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed)
	require.Equal(t, apiv2.PhysicalContainerVolumePhasePending, failedVolume.Status.Phase)
	requireReadyCondition(t, failedVolume.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed)
	require.NotNil(t, inspectRuntimeVolume(t, ctx, volumeName))
	require.Len(t, inspectRuntimeContainers(t, ctx, containerID), 1)
	require.Equal(t, 1, containerOrchestrator.CreateVolumeCallCount(volumeName))

	_, removeContainerErr := containerOrchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{containerID},
		Force:      true,
	})
	require.NoError(t, removeContainerErr)
	readyVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Equal(t, string(readyVolume.UID), inspectRuntimeVolume(t, ctx, volumeName).Labels["com.microsoft.developer.usvc-dev.uid"])
}

func TestV2PhysicalContainerVolumeControllerToleratesReplacementRemovalRace(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-replace-race")
	volumeName := "v2-pcv-replace-race-runtime"
	require.NoError(t, containerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: volumeName}))
	removeRuntimeVolumeOnCleanup(t, volumeName)
	containerOrchestrator.FailNextRemoveVolumeAfterRemoval(
		volumeName,
		errors.Join(containers.ErrNotFound, containers.ErrIncomplete),
	)

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "replacement-race-volume", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerVolumeSpec{
			Volume: &apiv2.PhysicalContainerVolumeConfig{
				VolumeName:      volumeName,
				ReplaceExisting: true,
			},
		},
	}
	require.NoError(t, client.Create(ctx, volume))
	readyVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Equal(t, string(readyVolume.UID), inspectRuntimeVolume(t, ctx, volumeName).Labels["com.microsoft.developer.usvc-dev.uid"])
	require.Equal(t, 2, containerOrchestrator.CreateVolumeCallCount(volumeName))
}

func TestV2PhysicalContainerVolumeControllerRetriesTransientReplacementRemovalFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-replace-remove-retry")
	volumeName := "v2-pcv-replace-remove-retry-runtime"
	require.NoError(t, containerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: volumeName}))
	removeRuntimeVolumeOnCleanup(t, volumeName)
	containerOrchestrator.FailNextRemoveVolume(volumeName, errors.New("simulated transient removal failure"))

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "replacement-remove-retry-volume", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerVolumeSpec{
			Volume: &apiv2.PhysicalContainerVolumeConfig{
				VolumeName:      volumeName,
				ReplaceExisting: true,
			},
		},
	}
	require.NoError(t, client.Create(ctx, volume))
	failedVolume := waitPhysicalContainerVolumeReason(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed)
	require.Equal(t, apiv2.PhysicalContainerVolumePhasePending, failedVolume.Status.Phase)
	requireReadyCondition(t, failedVolume.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed)
	require.NotNil(t, inspectRuntimeVolume(t, ctx, volumeName))

	readyVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Equal(t, string(readyVolume.UID), inspectRuntimeVolume(t, ctx, volumeName).Labels["com.microsoft.developer.usvc-dev.uid"])
	require.GreaterOrEqual(t, containerOrchestrator.RemoveVolumeCallCount(volumeName), 2)
}

func TestV2PhysicalContainerVolumeControllerRetriesTransientReplacementInspectionFailure(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	serverInfo, _, startupErr := StartTestEnvironment(ctx, NamespaceController|PhysicalContainerVolumeController, t.Name(), NoSeparateWorkingDir)
	require.NoError(t, startupErr)
	defer func() {
		cancel()
		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-time.After(5 * time.Second):
		}
	}()

	testOrchestrator, isTestOrchestrator := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTestOrchestrator)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-pcv-replace-inspect-retry"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, types.NamespacedName{Name: namespace.Name}, func(updated *apiv2.Namespace) (bool, error) {
		return updated.Status.Phase == apiv2.NamespacePhaseActive, nil
	})

	volumeName := "v2-pcv-replace-inspect-retry-runtime"
	require.NoError(t, testOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: volumeName}))
	testOrchestrator.SetRuntimeHealth(false)
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "replacement-inspect-retry-volume", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerVolumeSpec{
			Volume: &apiv2.PhysicalContainerVolumeConfig{
				VolumeName:      volumeName,
				ReplaceExisting: true,
			},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, volume))
	failedVolume := waitPhysicalContainerVolumeReasonEx(t, ctx, serverInfo.Client, volume.NamespacedName(), apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed)
	require.Equal(t, apiv2.PhysicalContainerVolumePhasePending, failedVolume.Status.Phase)
	requireReadyCondition(t, failedVolume.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed)

	testOrchestrator.SetRuntimeHealth(true)
	readyVolume := waitPhysicalContainerVolumePhaseEx(t, ctx, serverInfo.Client, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	inspectedVolumes, inspectErr := testOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedVolumes, 1)
	require.Equal(t, string(readyVolume.UID), inspectedVolumes[0].Labels["com.microsoft.developer.usvc-dev.uid"])
}

func TestV2PhysicalContainerVolumeControllerAdoptsSameResourceVolumeAfterStateLoss(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespaceName := "v2-pcv-adopt-state-loss"
	volumeName := "v2-pcv-adopt-state-loss-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "state-loss-volume", Namespace: namespaceName},
		Spec:       newPhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))
	pendingVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhasePending)
	require.NotEmpty(t, pendingVolume.UID)
	require.NoError(t, containerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
		Name: volumeName,
		Labels: map[string]string{
			"com.microsoft.developer.usvc-dev.uid": string(pendingVolume.UID),
		},
	}))

	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}
	require.NoError(t, client.Create(ctx, namespace))
	waitV2NamespaceActive(t, ctx, namespaceName)
	readyVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Equal(t, string(readyVolume.UID), inspectRuntimeVolume(t, ctx, volumeName).Labels["com.microsoft.developer.usvc-dev.uid"])
	require.Equal(t, 2, containerOrchestrator.CreateVolumeCallCount(volumeName))
}

func TestV2PhysicalContainerVolumeControllerReportsExternalRemovalWithoutRecreating(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-missing")
	volumeName := "v2-pcv-missing-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-volume", Namespace: namespace.Name},
		Spec:       newPhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))
	readyVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	createCount := containerOrchestrator.CreateVolumeCallCount(volumeName)

	_, removeErr := containerOrchestrator.RemoveVolumes(ctx, containers.RemoveVolumesOptions{Volumes: []string{volumeName}})
	require.NoError(t, removeErr)
	readyVolume.Annotations = map[string]string{"test-probe": "missing"}
	require.NoError(t, client.Update(ctx, readyVolume))

	missingVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseUnknown)
	requireReadyCondition(t, missingVolume.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonRuntimeVolumeMissing)
	require.Equal(t, createCount, containerOrchestrator.CreateVolumeCallCount(volumeName))
}

func TestV2PhysicalContainerVolumeControllerDoesNotChurnReadyStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-steady")
	volumeName := "v2-pcv-steady-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "steady-volume", Namespace: namespace.Name},
		Spec:       newPhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))
	readyVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	readyResourceVersion := readyVolume.ResourceVersion
	settledInspectCount := containerOrchestrator.InspectVolumeCallCount(volumeName) + 1

	require.Never(t, func() bool {
		currentVolume := &apiv2.PhysicalContainerVolume{}
		if getErr := client.Get(ctx, volume.NamespacedName(), currentVolume); getErr != nil {
			return false
		}
		return currentVolume.ResourceVersion != readyResourceVersion ||
			containerOrchestrator.InspectVolumeCallCount(volumeName) > settledInspectCount
	}, 5*time.Second, 250*time.Millisecond)
}

func TestV2PhysicalContainerVolumeControllerRecoversFromRuntimeAndCreateFailures(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	serverInfo, _, startupErr := StartTestEnvironment(ctx, NamespaceController|PhysicalContainerVolumeController, t.Name(), NoSeparateWorkingDir)
	require.NoError(t, startupErr)
	defer func() {
		cancel()
		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-time.After(5 * time.Second):
		}
	}()

	testOrchestrator, isTestOrchestrator := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTestOrchestrator)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-pcv-recovery"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, types.NamespacedName{Name: namespace.Name}, func(updated *apiv2.Namespace) (bool, error) {
		return updated.Status.Phase == apiv2.NamespacePhaseActive, nil
	})

	testOrchestrator.SetRuntimeHealth(false)
	volumeName := "v2-pcv-recovery-runtime"
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "recovering-volume", Namespace: namespace.Name},
		Spec:       newPhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, serverInfo.Client.Create(ctx, volume))
	failedVolume := waitPhysicalContainerVolumeReasonEx(t, ctx, serverInfo.Client, volume.NamespacedName(), apiv2.PhysicalContainerVolumeReasonCreateFailed)
	require.Equal(t, apiv2.PhysicalContainerVolumePhasePending, failedVolume.Status.Phase)
	requireReadyCondition(t, failedVolume.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonCreateFailed)

	waitInspectVolumeCallCount(t, ctx, testOrchestrator, volumeName, testOrchestrator.InspectVolumeCallCount(volumeName)+2)
	testOrchestrator.SetRuntimeHealth(true)
	recoveredVolume := waitPhysicalContainerVolumePhaseEx(t, ctx, serverInfo.Client, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	requireReadyCondition(t, recoveredVolume.Status.Conditions, metav1.ConditionTrue, apiv2.PhysicalContainerVolumeReasonVolumeAvailable)

	testOrchestrator.SetRuntimeHealth(false)
	recoveredVolume.Annotations = map[string]string{"test-probe": "runtime-failure"}
	require.NoError(t, serverInfo.Client.Update(ctx, recoveredVolume))
	failedVolume = waitPhysicalContainerVolumeReasonEx(t, ctx, serverInfo.Client, volume.NamespacedName(), apiv2.PhysicalContainerVolumeReasonRuntimeVolumeInspectFailed)
	require.Equal(t, apiv2.PhysicalContainerVolumePhaseUnknown, failedVolume.Status.Phase)
	testOrchestrator.SetRuntimeHealth(true)
	waitPhysicalContainerVolumePhaseEx(t, ctx, serverInfo.Client, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
}

func TestV2PhysicalContainerVolumeControllerWaitsForNamespace(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	volumeName := "v2-pcv-wait-namespace-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "wait-namespace-volume", Namespace: "v2-pcv-wait-namespace"},
		Spec:       newPhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))
	waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhasePending)
	require.Equal(t, 0, containerOrchestrator.CreateVolumeCallCount(volumeName))

	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: volume.Namespace}}
	require.NoError(t, client.Create(ctx, namespace))
	waitV2NamespaceActive(t, ctx, namespace.Name)
	waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Equal(t, 1, containerOrchestrator.CreateVolumeCallCount(volumeName))
}

func waitPhysicalContainerVolumePhase(
	t *testing.T,
	ctx context.Context,
	name types.NamespacedName,
	phase apiv2.PhysicalContainerVolumePhase,
) *apiv2.PhysicalContainerVolume {
	t.Helper()
	return waitObjectAssumesState(t, ctx, name, func(volume *apiv2.PhysicalContainerVolume) (bool, error) {
		return volume.Status.Phase == phase, nil
	})
}

func waitPhysicalContainerVolumeReason(
	t *testing.T,
	ctx context.Context,
	name types.NamespacedName,
	reason apiv2.ConditionReason,
) *apiv2.PhysicalContainerVolume {
	t.Helper()
	return waitPhysicalContainerVolumeReasonEx(t, ctx, client, name, reason)
}

func waitPhysicalContainerVolumeReasonEx(
	t *testing.T,
	ctx context.Context,
	testClient ctrl_client.Client,
	name types.NamespacedName,
	reason apiv2.ConditionReason,
) *apiv2.PhysicalContainerVolume {
	t.Helper()
	return waitObjectAssumesStateEx(t, ctx, testClient, name, func(volume *apiv2.PhysicalContainerVolume) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(volume.Status.Conditions, string(apiv2.ConditionReady))
		return readyCondition != nil && readyCondition.Reason == string(reason), nil
	})
}

func newPhysicalContainerVolumeSpec(volumeName string) apiv2.PhysicalContainerVolumeSpec {
	return apiv2.PhysicalContainerVolumeSpec{
		Volume: &apiv2.PhysicalContainerVolumeConfig{VolumeName: volumeName},
	}
}

func waitPhysicalContainerVolumePhaseEx(
	t *testing.T,
	ctx context.Context,
	apiClient ctrl_client.Client,
	name types.NamespacedName,
	phase apiv2.PhysicalContainerVolumePhase,
) *apiv2.PhysicalContainerVolume {
	t.Helper()
	return waitObjectAssumesStateEx(t, ctx, apiClient, name, func(volume *apiv2.PhysicalContainerVolume) (bool, error) {
		return volume.Status.Phase == phase, nil
	})
}

func waitCreateVolumeCallCount(t *testing.T, ctx context.Context, volumeName string, expected int) {
	t.Helper()
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		return containerOrchestrator.CreateVolumeCallCount(volumeName) >= expected, nil
	})
	require.NoError(t, waitErr)
}

func waitInspectVolumeCallCount(
	t *testing.T,
	ctx context.Context,
	orchestrator *ctrl_testutil.TestContainerOrchestrator,
	volumeName string,
	expected int,
) {
	t.Helper()
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		return orchestrator.InspectVolumeCallCount(volumeName) >= expected, nil
	})
	require.NoError(t, waitErr)
}

func inspectRuntimeVolume(t *testing.T, ctx context.Context, volumeName string) *containers.InspectedVolume {
	t.Helper()
	inspectedVolumes, inspectErr := containerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedVolumes, 1)
	return &inspectedVolumes[0]
}

func inspectRuntimeContainers(t *testing.T, ctx context.Context, containerIDs ...string) []containers.InspectedContainer {
	t.Helper()
	inspectedContainers, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: containerIDs})
	require.NoError(t, inspectErr)
	return inspectedContainers
}

func waitRuntimeVolumeMissing(t *testing.T, ctx context.Context, volumeName string) {
	t.Helper()
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		inspectedVolumes, inspectErr := containerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
		if len(inspectedVolumes) > 0 {
			return false, nil
		}
		if errors.Is(inspectErr, containers.ErrNotFound) {
			return true, nil
		}
		return false, inspectErr
	})
	require.NoError(t, waitErr)
}

func removeRuntimeVolumeOnCleanup(t *testing.T, volumeName string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = containerOrchestrator.RemoveVolumes(context.Background(), containers.RemoveVolumesOptions{Volumes: []string{volumeName}})
	})
}
