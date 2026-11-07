/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
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
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestV2PhysicalContainerVolumeControllerCreatesRetainedVolumeByDefault(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-create")
	volumeName := "v2-pcv-created-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "created-volume", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerVolumeSpec{
			VolumeName: volumeName,
			Labels: []commonapi.Label{
				{Key: "test-label", Value: "test-value"},
				{Key: "com.microsoft.developer.usvc-dev.uid", Value: "caller-value"},
				{Key: controllers.PersistentLabel, Value: "caller-value"},
				{Key: controllers.CreatorProcessIdLabel, Value: "caller-value"},
				{Key: controllers.CreatorProcessStartTimeLabel, Value: "caller-value"},
			},
		},
	}
	require.NoError(t, client.Create(ctx, volume))

	readyVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Contains(t, readyVolume.Finalizers, apiv2.GroupName+"/physicalcontainervolume-reconciler")
	require.Equal(t, volumeName, readyVolume.Status.VolumeID)
	require.Equal(t, "local", readyVolume.Status.Driver)
	require.Equal(t, "local", readyVolume.Status.Scope)
	require.False(t, readyVolume.Status.CreatedAt.IsZero())
	requireReadyCondition(t, readyVolume.Status.Conditions, metav1.ConditionTrue, apiv2.PhysicalContainerVolumeReasonVolumeAvailable)

	inspectedVolume := inspectRuntimeVolume(t, ctx, volumeName)
	require.Equal(t, "test-value", inspectedVolume.Labels["test-label"])
	require.Equal(t, string(readyVolume.UID), inspectedVolume.Labels["com.microsoft.developer.usvc-dev.uid"])
	require.Equal(t, "true", inspectedVolume.Labels[controllers.PersistentLabel])
	require.NotEmpty(t, inspectedVolume.Labels[controllers.CreatorProcessIdLabel])
	require.NotEqual(t, "caller-value", inspectedVolume.Labels[controllers.CreatorProcessIdLabel])
	require.NotEmpty(t, inspectedVolume.Labels[controllers.CreatorProcessStartTimeLabel])
	require.NotEqual(t, "caller-value", inspectedVolume.Labels[controllers.CreatorProcessStartTimeLabel])
}

// Verifies that PhysicalContainerVolume retains reused volumes by default.
func TestV2PhysicalContainerVolumeControllerRetainsExistingVolumeByDefault(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-track")
	volumeName := "v2-pcv-tracked-runtime"
	require.NoError(t, containerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: volumeName}))
	removeRuntimeVolumeOnCleanup(t, volumeName)

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "tracked-volume", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalContainerVolumeSpec{VolumeName: volumeName},
	}
	require.NoError(t, client.Create(ctx, volume))

	readyVolume := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Equal(t, volumeName, readyVolume.Status.VolumeID)
	require.Equal(t, 1, containerOrchestrator.CreateVolumeCallCount(volumeName))

	require.NoError(t, client.Delete(ctx, volume))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerVolume](t, ctx, client, volume)
	require.NotNil(t, inspectRuntimeVolume(t, ctx, volumeName))
}

func TestV2PhysicalContainerVolumeControllerHonorsCreatedVolumeCleanupPolicy(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-delete")
	for _, removeRuntimeVolumeOnDelete := range []bool{false, true} {
		name := "retained"
		if removeRuntimeVolumeOnDelete {
			name = "removed"
		}
		volumeName := "v2-pcv-" + name + "-runtime"
		removeRuntimeVolumeOnCleanup(t, volumeName)
		volume := &apiv2.PhysicalContainerVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-volume", Namespace: namespace.Name},
			Spec: apiv2.PhysicalContainerVolumeSpec{
				VolumeName:                  volumeName,
				RemoveRuntimeVolumeOnDelete: removeRuntimeVolumeOnDelete,
			},
		}
		require.NoError(t, client.Create(ctx, volume))
		waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)

		require.NoError(t, client.Delete(ctx, volume))
		ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerVolume](t, ctx, client, volume)
		if removeRuntimeVolumeOnDelete {
			waitRuntimeVolumeMissing(t, ctx, volumeName)
		} else {
			inspectedVolume := inspectRuntimeVolume(t, ctx, volumeName)
			require.Equal(t, "true", inspectedVolume.Labels[controllers.PersistentLabel])
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
		Spec:       newRemovablePhysicalContainerVolumeSpec(volumeName),
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
		Spec:       newRemovablePhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))
	waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)

	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerVolume](t, ctx, client, volume)
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
	waitRuntimeVolumeMissing(t, ctx, volumeName)
}

// Verifies that namespace deletion retries removal of an in-use volume during the grace period,
// then reports abandonment and releases its cleanup finalizer so namespace deletion can finish.
func TestV2PhysicalContainerVolumeControllerBoundsNamespaceDeletionWhileInUse(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 2*defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-ns-in-use-deadline")
	volumeName := namespace.Name + "-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "in-use-volume", Namespace: namespace.Name,
			Finalizers: []string{controllers.LogicalBackingFinalizer},
		},
		Spec: newRemovablePhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))
	removeDependencyHoldOnCleanup(t, ctx, volume, controllers.LogicalBackingFinalizer)
	waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	containerID, createErr := containerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: namespace.Name + "-consumer", Image: "in-use-image",
		VolumeMounts: []containers.CreateContainerVolumeMount{{
			Type: containers.NamedVolumeMount, Source: volumeName, Target: "/data",
		}},
	})
	require.NoError(t, createErr)
	removeRuntimeContainerOnCleanup(t, containerID)

	require.NoError(t, client.Delete(ctx, namespace))
	terminating := waitPhysicalContainerVolumeReason(t, ctx, volume.NamespacedName(),
		apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoveFailed)
	require.False(t, terminating.DeletionTimestamp.IsZero())
	require.Contains(t, terminating.Finalizers, apiv2.GroupName+"/physicalcontainervolume-reconciler")

	abandoned := waitPhysicalContainerVolumeReason(t, ctx, volume.NamespacedName(),
		apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemovalAbandoned)
	require.GreaterOrEqual(t, time.Since(terminating.DeletionTimestamp.Time), 30*time.Second,
		"namespace cleanup must allow the removal grace period before abandoning the volume")
	requireReadyCondition(t, abandoned.Status.Conditions, metav1.ConditionFalse,
		apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemovalAbandoned)
	released := waitObjectAssumesState(t, ctx, volume.NamespacedName(), func(current *apiv2.PhysicalContainerVolume) (bool, error) {
		return len(current.Finalizers) == 1 && current.Finalizers[0] == controllers.LogicalBackingFinalizer, nil
	})
	removalAttempts := containerOrchestrator.RemoveVolumeCallCount(volumeName)
	require.Greater(t, removalAttempts, 1, "the controller must retry failed removal before abandoning it")

	require.NoError(t, controllers.ReleaseLogicalBackingFinalizer(ctx, client, released))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, volume)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
	require.Equal(t, removalAttempts, containerOrchestrator.RemoveVolumeCallCount(volumeName),
		"the controller must stop attempting removal after abandoning the volume")
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
		Spec:       newRemovablePhysicalContainerVolumeSpec(volumeName),
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

// Verifies that PhysicalContainerVolume settles in-flight creation and publishes runtime removal
// before removing its cleanup finalizer, without waiting for the logical hold to be released.
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
		ObjectMeta: metav1.ObjectMeta{Name: "delete-during-create-volume", Namespace: namespace.Name,
			Finalizers: []string{controllers.LogicalBackingFinalizer}},
		Spec: newRemovablePhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))
	removeDependencyHoldOnCleanup(t, ctx, volume, controllers.LogicalBackingFinalizer)
	waitCreateVolumeCallCount(t, ctx, volumeName, 1)
	require.NoError(t, client.Delete(ctx, volume))

	finalizer := apiv2.GroupName + "/physicalcontainervolume-reconciler"
	terminatingVolume := waitObjectAssumesState(t, ctx, volume.NamespacedName(), func(current *apiv2.PhysicalContainerVolume) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.DeletionTimestamp != nil &&
			!current.DeletionTimestamp.IsZero() &&
			slices.Contains(current.Finalizers, finalizer) &&
			readyCondition != nil &&
			readyCondition.Status == metav1.ConditionFalse &&
			readyCondition.Reason == string(apiv2.PhysicalContainerVolumeReasonCreating), nil
	})
	require.Contains(t, terminatingVolume.Finalizers, finalizer)
	requireReadyCondition(t, terminatingVolume.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonCreating)
	require.Equal(t, 0, containerOrchestrator.RemoveVolumeCallCount(volumeName))

	releaseCreate()
	cleaned := waitObjectAssumesState(t, ctx, volume.NamespacedName(), func(current *apiv2.PhysicalContainerVolume) (bool, error) {
		return len(current.Finalizers) == 1 && current.Finalizers[0] == controllers.LogicalBackingFinalizer, nil
	})
	requireReadyCondition(t, cleaned.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoved)
	waitRuntimeVolumeMissing(t, ctx, volumeName)
	require.NoError(t, controllers.ReleaseLogicalBackingFinalizer(ctx, client, cleaned))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerVolume](t, ctx, client, volume)
	require.Equal(t, 1, containerOrchestrator.CreateVolumeCallCount(volumeName))
	require.Equal(t, 1, containerOrchestrator.RemoveVolumeCallCount(volumeName))
}

// Verifies that a volume create error is reported before inspection and that reconciliation
// subsequently resolves the volume without issuing another create.
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
		Spec:       newRemovablePhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))

	failedAttempt := waitObjectAssumesState(t, ctx, volume.NamespacedName(), func(current *apiv2.PhysicalContainerVolume) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.PhysicalContainerVolumePhasePending &&
			ready != nil && ready.Reason == string(apiv2.PhysicalContainerVolumeReasonCreateFailed), nil
	})
	require.Contains(t, failedAttempt.Status.Conditions[0].Message, "simulated lost create response")
	require.Equal(t, 1, containerOrchestrator.InspectVolumeCallCount(volumeName))
	waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.Equal(t, 1, containerOrchestrator.CreateVolumeCallCount(volumeName))
}

type concurrentVolumeCreationOrchestrator struct {
	containers.ContainerOrchestrator
	name              string
	concurrent        bool
	idempotent        bool
	creationAttempted atomic.Bool
	inspectionFailed  atomic.Bool
}

func (orchestrator *concurrentVolumeCreationOrchestrator) CreateVolume(ctx context.Context, options containers.CreateVolumeOptions) error {
	if options.Name != orchestrator.name {
		return orchestrator.ContainerOrchestrator.CreateVolume(ctx, options)
	}
	if orchestrator.concurrent {
		concurrentErr := orchestrator.ContainerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
			Name: options.Name, Labels: map[string]string{"owner": "external"},
		})
		if concurrentErr != nil {
			return concurrentErr
		}
	}
	createErr := orchestrator.ContainerOrchestrator.CreateVolume(ctx, options)
	orchestrator.creationAttempted.Store(true)
	if orchestrator.idempotent && errors.Is(createErr, containers.ErrAlreadyExists) {
		return nil
	}
	return createErr
}

func (orchestrator *concurrentVolumeCreationOrchestrator) InspectVolumes(ctx context.Context, options containers.InspectVolumesOptions) ([]containers.InspectedVolume, error) {
	if slices.Contains(options.Volumes, orchestrator.name) && orchestrator.creationAttempted.Load() &&
		orchestrator.inspectionFailed.CompareAndSwap(false, true) {
		return nil, errors.New("transient post-create inspection failure")
	}
	return orchestrator.ContainerOrchestrator.InspectVolumes(ctx, options)
}

// Verifies that PhysicalContainerVolume consistently accepts successful and concurrent creates
// after inspection failures, preserving existing labels and never replacing the volume.
func TestV2PhysicalContainerVolumeControllerRecoversCreationRaces(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name       string
		concurrent bool
		idempotent bool
	}{
		{name: "created"},
		{name: "already-exists", concurrent: true},
		{name: "idempotent-create", concurrent: true, idempotent: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var decorated *concurrentVolumeCreationOrchestrator
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx, PhysicalContainerVolumeController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateContainerOrchestrator: func(original containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
					decorated = &concurrentVolumeCreationOrchestrator{
						ContainerOrchestrator: original, name: "volume-race-runtime",
						concurrent: testCase.concurrent, idempotent: testCase.idempotent,
					}
					return decorated
				}})
			require.NoError(t, startErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-volume-race")
			volume := &apiv2.PhysicalContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "volume", Namespace: namespace.Name},
				Spec: apiv2.PhysicalContainerVolumeSpec{
					VolumeName: decorated.name, Labels: []commonapi.Label{{Key: "owner", Value: "requested"}},
				},
			}
			require.NoError(t, server.Client.Create(ctx, volume))
			ready := waitPhysicalContainerVolumePhaseEx(t, ctx, server.Client, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
			require.True(t, decorated.inspectionFailed.Load())
			require.Equal(t, decorated.name, ready.Status.VolumeID)
			require.Equal(t, "local", ready.Status.Driver)
			inspected, inspectErr := decorated.ContainerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{decorated.name}})
			require.NoError(t, inspectErr)
			require.Len(t, inspected, 1)
			expectedOwner := "requested"
			expectedCreates := 1
			if testCase.concurrent {
				expectedOwner, expectedCreates = "external", 2
				require.Empty(t, inspected[0].Labels["com.microsoft.developer.usvc-dev.uid"])
			}
			require.Equal(t, expectedOwner, inspected[0].Labels["owner"])
			simulated := decorated.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			require.Equal(t, expectedCreates, simulated.CreateVolumeCallCount(decorated.name))
			require.Zero(t, simulated.RemoveVolumeCallCount(decorated.name))
		})
	}
}

func setPhysicalVolumeFailure(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, volume *apiv2.PhysicalContainerVolume) {
	t.Helper()
	volume.Status.Phase = apiv2.PhysicalContainerVolumePhaseFailed
	apimeta.SetStatusCondition(&volume.Status.Conditions, metav1.Condition{
		Type: string(apiv2.ConditionReady), Status: metav1.ConditionFalse,
		ObservedGeneration: volume.Generation, Reason: string(apiv2.PhysicalContainerVolumeReasonCreateFailed),
		Message: "Injected volume creation failure.",
	})
	require.NoError(t, apiClient.Status().Update(ctx, volume))
}

// Verifies that PhysicalContainerVolume reuses in-use volumes without changing their labels or
// recreating them, retaining them by default and honoring explicit non-forced deletion.
func TestV2PhysicalContainerVolumeControllerReusesExistingVolume(t *testing.T) {
	t.Parallel()
	for _, removeOnDelete := range []bool{false, true} {
		name := "retained"
		if removeOnDelete {
			name = "removed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespace := createActiveV2Namespace(t, ctx, "v2-pcv-reuse-"+name)
			volumeName := namespace.Name + "-runtime"
			require.NoError(t, containerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
				Name: volumeName, Labels: map[string]string{"owner": "external"},
			}))
			removeRuntimeVolumeOnCleanup(t, volumeName)
			original := inspectRuntimeVolume(t, ctx, volumeName)
			containerID, createErr := containerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
				Name: namespace.Name + "-consumer", Image: "existing-volume-image",
				VolumeMounts: []containers.CreateContainerVolumeMount{{
					Type: containers.NamedVolumeMount, Source: volumeName, Target: "/data",
				}},
			})
			require.NoError(t, createErr)
			removeRuntimeContainerOnCleanup(t, containerID)
			volume := &apiv2.PhysicalContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "volume", Namespace: namespace.Name},
				Spec: apiv2.PhysicalContainerVolumeSpec{
					VolumeName: volumeName, RemoveRuntimeVolumeOnDelete: removeOnDelete,
					Labels: []commonapi.Label{{Key: "owner", Value: "requested"}},
				},
			}
			require.NoError(t, client.Create(ctx, volume))
			ready := waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
			require.Equal(t, volumeName, ready.Status.VolumeID)
			require.Equal(t, original, inspectRuntimeVolume(t, ctx, volumeName))
			require.Equal(t, 1, containerOrchestrator.CreateVolumeCallCount(volumeName))
			require.Zero(t, containerOrchestrator.RemoveVolumeCallCount(volumeName))
			require.Len(t, inspectRuntimeContainers(t, ctx, containerID), 1)
			require.NoError(t, client.Delete(ctx, volume))
			if removeOnDelete {
				waitPhysicalContainerVolumeReason(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoveFailed)
				require.Equal(t, original, inspectRuntimeVolume(t, ctx, volumeName))
				require.Len(t, inspectRuntimeContainers(t, ctx, containerID), 1)
			} else {
				ctrl_testutil.WaitObjectDeleted(t, ctx, client, volume)
				require.Equal(t, original, inspectRuntimeVolume(t, ctx, volumeName))
				require.Zero(t, containerOrchestrator.RemoveVolumeCallCount(volumeName))
			}
			_, removeErr := containerOrchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
				Containers: []string{containerID}, Force: true,
			})
			require.NoError(t, removeErr)
			if removeOnDelete {
				ctrl_testutil.WaitObjectDeleted(t, ctx, client, volume)
				waitRuntimeVolumeMissing(t, ctx, volumeName)
			}
		})
	}
}

// Verifies that PhysicalContainerVolume retries failed deletion without recreating the volume.
func TestV2PhysicalContainerVolumeControllerRetriesTransientRemovalFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pcv-remove-retry")
	volumeName := namespace.Name + "-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)
	containerOrchestrator.FailNextRemoveVolume(volumeName, errors.New("simulated transient removal failure"))

	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "volume", Namespace: namespace.Name},
		Spec:       newRemovablePhysicalContainerVolumeSpec(volumeName),
	}
	require.NoError(t, client.Create(ctx, volume))
	waitPhysicalContainerVolumePhase(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	require.NoError(t, client.Delete(ctx, volume))
	failedVolume := waitPhysicalContainerVolumeReason(t, ctx, volume.NamespacedName(), apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoveFailed)
	require.Equal(t, apiv2.PhysicalContainerVolumePhasePending, failedVolume.Status.Phase)
	require.NotNil(t, inspectRuntimeVolume(t, ctx, volumeName))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, volume)
	waitRuntimeVolumeMissing(t, ctx, volumeName)
	require.Equal(t, 1, containerOrchestrator.CreateVolumeCallCount(volumeName))
	require.GreaterOrEqual(t, containerOrchestrator.RemoveVolumeCallCount(volumeName), 2)
}

// Verifies that PhysicalContainerVolume retries initial lookup failures and reuses the existing
// volume once inspection recovers without creating or replacing it.
func TestV2PhysicalContainerVolumeControllerRetriesInitialInspectionFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	serverInfo, _, startupErr := StartTestEnvironment(t, ctx, NamespaceController|PhysicalContainerVolumeController, t.Name(), NoSeparateWorkingDir)
	require.NoError(t, startupErr)

	testOrchestrator, isTestOrchestrator := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTestOrchestrator)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-pcv-inspect-retry"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, types.NamespacedName{Name: namespace.Name}, func(updated *apiv2.Namespace) (bool, error) {
		return updated.Status.Phase == apiv2.NamespacePhaseActive, nil
	})

	volumeName := namespace.Name + "-runtime"
	require.NoError(t, testOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: volumeName}))
	testOrchestrator.SetRuntimeHealth(false)
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "volume", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalContainerVolumeSpec{VolumeName: volumeName},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, volume))
	failedVolume := waitPhysicalContainerVolumeReasonEx(t, ctx, serverInfo.Client, volume.NamespacedName(), apiv2.PhysicalContainerVolumeReasonCreateFailed)
	require.Equal(t, apiv2.PhysicalContainerVolumePhasePending, failedVolume.Status.Phase)

	testOrchestrator.SetRuntimeHealth(true)
	readyVolume := waitPhysicalContainerVolumePhaseEx(t, ctx, serverInfo.Client, volume.NamespacedName(), apiv2.PhysicalContainerVolumePhaseReady)
	inspectedVolumes, inspectErr := testOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedVolumes, 1)
	require.Equal(t, volumeName, readyVolume.Status.VolumeID)
	require.Empty(t, inspectedVolumes[0].Labels)
	require.Equal(t, 1, testOrchestrator.CreateVolumeCallCount(volumeName))
	require.Zero(t, testOrchestrator.RemoveVolumeCallCount(volumeName))
}

// Verifies that PhysicalContainerVolume reuses a volume from an earlier attempt without another create.
func TestV2PhysicalContainerVolumeControllerAdoptsSameResourceVolumeAfterStateLoss(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespaceName := "v2-pcv-adopt-state-loss"
	volumeName := "v2-pcv-adopt-state-loss-runtime"
	removeRuntimeVolumeOnCleanup(t, volumeName)
	volume := &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "state-loss-volume", Namespace: namespaceName},
		Spec:       newRemovablePhysicalContainerVolumeSpec(volumeName),
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
	require.Equal(t, 1, containerOrchestrator.CreateVolumeCallCount(volumeName))
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
		Spec:       newRemovablePhysicalContainerVolumeSpec(volumeName),
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

func TestV2PhysicalContainerVolumeControllerRecoversFromRuntimeAndCreateFailures(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	serverInfo, _, startupErr := StartTestEnvironment(t, ctx, NamespaceController|PhysicalContainerVolumeController, t.Name(), NoSeparateWorkingDir)
	require.NoError(t, startupErr)

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
		Spec:       newRemovablePhysicalContainerVolumeSpec(volumeName),
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
		Spec:       newRemovablePhysicalContainerVolumeSpec(volumeName),
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

func newRemovablePhysicalContainerVolumeSpec(volumeName string) apiv2.PhysicalContainerVolumeSpec {
	return apiv2.PhysicalContainerVolumeSpec{
		VolumeName:                  volumeName,
		RemoveRuntimeVolumeOnDelete: true,
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
