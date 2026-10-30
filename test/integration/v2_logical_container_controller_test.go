/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that the LogicalContainer controller waits for namespace readiness and delegates
// image readiness to its backing physical container before reporting the runtime as running.
func TestV2LogicalContainerControllerWaitsForNamespace(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: "v2-logical-wait-namespace"},
		Spec: apiv2.LogicalContainerSpec{
			Container: &apiv2.LogicalContainerConfig{ImageRef: "unused-image"},
		},
	}
	require.NoError(t, client.Create(ctx, container))
	pendingContainer := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.LogicalContainerPhasePending &&
			readyCondition != nil && readyCondition.Reason == string(apiv2.PhysicalResourceReasonNamespaceNotFound), nil
	})
	require.NotEmpty(t, pendingContainer.Finalizers)
	requireReadyCondition(t, pendingContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalResourceReasonNamespaceNotFound)

	createActiveV2Namespace(t, ctx, container.Namespace)
	initializedContainer := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return readyCondition != nil && readyCondition.Reason == "PhysicalContainerNotReady" &&
			strings.Contains(readyCondition.Message, string(apiv2.PhysicalContainerReasonImageNotFound)), nil
	})
	require.Equal(t, apiv2.LogicalContainerPhasePending, initializedContainer.Status.Phase)
	requireReadyCondition(t, initializedContainer.Status.Conditions, metav1.ConditionFalse, "PhysicalContainerNotReady")
	require.Empty(t, initializedContainer.Status.ContainerID)
	require.NotEmpty(t, initializedContainer.Status.PhysicalContainerRef)
	require.Equal(t, "unused-image", initializedContainer.Status.ImageRef)

	var physicalContainers apiv2.PhysicalContainerList
	require.NoError(t, client.List(ctx, &physicalContainers, ctrl_client.InNamespace(container.Namespace)))
	require.Len(t, physicalContainers.Items, 1)
	require.Equal(t, "unused-image", physicalContainers.Items[0].Spec.Container.ImageRef)
	require.Zero(t, containerOrchestrator.CreateContainerCallCount(physicalContainers.Items[0].Spec.Container.ContainerName))
	var physicalImages apiv2.PhysicalContainerImageList
	require.NoError(t, client.List(ctx, &physicalImages, ctrl_client.InNamespace(container.Namespace)))
	require.Empty(t, physicalImages.Items)
	createReadyV2PhysicalContainerImage(t, ctx, container.Namespace, "unused-image", "v2-logical-wait-namespace-image")
	waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
}

type unavailableNamespaceClient struct {
	ctrl_client.Client
	unavailable atomic.Bool
}

func (client *unavailableNamespaceClient) Get(ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption) error {
	if _, namespace := obj.(*apiv2.Namespace); namespace && client.unavailable.Load() {
		return errors.New("injected namespace read failure")
	}
	return client.Client.Get(ctx, key, obj, options...)
}

// Verifies that LogicalContainer reports and retries initial namespace lookup errors, then
// forwards stop and observes its backing despite namespace read failures after initialization.
func TestV2LogicalContainerControllerHandlesNamespaceReadFailures(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var decorated *unavailableNamespaceClient
	server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
		NamespaceController|LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
		t.Name(), t.TempDir(), TestEnvironmentOptions{
			DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
				decorated = &unavailableNamespaceClient{Client: client}
				return decorated
			},
		})
	require.NoError(t, startErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-namespace-read-failure"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	decorated.unavailable.Store(true)
	defer decorated.unavailable.Store(false)
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
			Image: &apiv2.PhysicalContainerImageConfig{Image: "namespace-read-failure-image"},
		}},
	}
	require.NoError(t, server.Client.Create(ctx, container))
	lookupFailed := waitObjectAssumesStateEx(t, ctx, server.Client, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseUnknown, nil
	})
	requireReadyCondition(t, lookupFailed.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalResourceReasonNamespaceLookupFailed)
	require.Empty(t, lookupFailed.Status.PhysicalContainerRef)
	decorated.unavailable.Store(false)
	running := waitObjectAssumesStateEx(t, ctx, server.Client, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	decorated.unavailable.Store(true)
	require.NoError(t, retryOnConflictEx(ctx, server.Client, container.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
		current.Spec.Stop = true
		return server.Client.Update(updateCtx, current)
	}))
	stopped := waitObjectAssumesStateEx(t, ctx, server.Client, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
	})
	require.Equal(t, running.Status.ContainerID, stopped.Status.ContainerID)
	requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, "ContainerStopped")
}

// Verifies that LogicalContainer reports Stopping while physical status still reports Running,
// and projects the final exit once the physical status update succeeds.
func TestV2LogicalContainerControllerReportsStoppingBeforePhysicalStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var conflicts *backingStatusConflicts
	server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
		NamespaceController|LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
		t.Name(), t.TempDir(), TestEnvironmentOptions{
			DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
				conflicts = &backingStatusConflicts{
					Client: client, imageConflict: make(chan struct{}), containerConflict: make(chan struct{}),
				}
				return conflicts
			},
		})
	require.NoError(t, startErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-stopping-status"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
			Image: &apiv2.PhysicalContainerImageConfig{Image: "stopping-status-image"},
		}},
	}
	require.NoError(t, server.Client.Create(ctx, container))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	conflicts.stopStatusBlocked.Store(true)
	defer conflicts.stopStatusBlocked.Store(false)
	require.NoError(t, retryOnConflictEx(ctx, server.Client, container.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
		current.Spec.Stop = true
		return server.Client.Update(updateCtx, current)
	}))
	stopping := waitObjectAssumesStateEx(t, ctx, server.Client, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseStopping, nil
	})
	requireReadyCondition(t, stopping.Status.Conditions, metav1.ConditionFalse, "ContainerStopping")
	require.Equal(t, running.Status.ContainerID, stopping.Status.ContainerID)
	require.NotNil(t, running.Status.RuntimeLogs)
	require.NotNil(t, stopping.Status.RuntimeLogs)
	require.Equal(t, running.Status.RuntimeLogs.ContainerID, stopping.Status.RuntimeLogs.ContainerID)
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{
		Namespace: namespace.Name, Name: stopping.Status.PhysicalContainerRef,
	}, physical))
	require.True(t, physical.Spec.Stop)
	require.Equal(t, apiv2.PhysicalContainerPhaseRunning, physical.Status.Phase)
	ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	require.NotNil(t, ready)
	require.Equal(t, string(apiv2.PhysicalContainerReasonRuntimeContainerRunning), ready.Reason)

	conflicts.stopStatusBlocked.Store(false)
	stopped := waitObjectAssumesStateEx(t, ctx, server.Client, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
	})
	requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, "ContainerStopped")
	require.Equal(t, running.Status.ContainerID, stopped.Status.ContainerID)
}

// Verifies that an initially stopped session waits for its namespace before completing
// without creating physical image/container resources.
func TestV2LogicalContainerControllerWaitsForNamespaceBeforeCompletingStop(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "stopped", Namespace: "v2-logical-stop-missing-namespace"},
		Spec: apiv2.LogicalContainerSpec{
			Stop: true, Container: &apiv2.LogicalContainerConfig{ImageRef: "unused"},
		},
	}
	require.NoError(t, client.Create(ctx, container))
	pending := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.LogicalContainerPhasePending &&
			readyCondition != nil && readyCondition.Reason == string(apiv2.PhysicalResourceReasonNamespaceNotFound), nil
	})
	require.Empty(t, pending.Status.PhysicalContainerRef)
	require.Empty(t, pending.Status.ImageRef)
	createActiveV2Namespace(t, ctx, container.Namespace)
	stopped := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
	})
	requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, "ContainerStopped")
	require.Empty(t, stopped.Status.ContainerID)
	require.Empty(t, stopped.Status.PhysicalContainerRef)
	require.NoError(t, client.Delete(ctx, container))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, container)
}

// Verifies that LogicalContainer waits for network references, then observes physical-owned image
// failure without waiting for network runtime readiness and releases its creation lease.
func TestV2LogicalContainerControllerFailsStartupWhenImageAcquisitionFails(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name           string
		missingNetwork bool
	}{
		{name: "without-network"},
		{name: "missing-network", missingNetwork: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespace := createActiveV2Namespace(t, ctx, "v2-logical-image-failure-"+testCase.name)
			sourceImage := namespace.Name + "-source"
			runtimeName := namespace.Name + "-runtime"
			containerOrchestrator.FailNextPullImage(sourceImage, errors.New("image acquisition denied"))
			noRetries := int32(0)
			container := &apiv2.LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
				Spec: apiv2.LogicalContainerSpec{
					Mode: apiv2.LogicalContainerModePersistent, ContainerName: runtimeName,
					Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{
						Image: sourceImage, PullPolicy: apiv2.PullPolicyAlways, PullRetryLimit: &noRetries,
					}},
				},
			}
			if testCase.missingNetwork {
				container.Spec.Container.Networks = []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "network"}}
			}
			require.NoError(t, client.Create(ctx, container))
			if testCase.missingNetwork {
				waitPhysicalContainerImagePhase(t, ctx,
					types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(container.UID)},
					apiv2.PhysicalContainerImagePhaseFailed)
				pending := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
					return current.Status.StartupLogs != nil && current.Status.StartupLogs.Complete, nil
				})
				require.Equal(t, apiv2.LogicalContainerPhasePending, pending.Status.Phase)
				require.Empty(t, pending.Status.PhysicalContainerRef)
				require.NoError(t, testStateStore.VerifyResourceLeaseHeld(ctx, container, testResourceLeaseOwner))
				networkRuntimeName := namespace.Name + "-network"
				releaseNetwork := containerOrchestrator.BlockCreateNetwork(networkRuntimeName)
				defer releaseNetwork()
				network := &apiv2.LogicalContainerNetwork{
					ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
					Spec: apiv2.LogicalContainerNetworkSpec{NetworkName: networkRuntimeName,
						Network: &apiv2.LogicalContainerNetworkConfig{}},
				}
				require.NoError(t, client.Create(ctx, network))
				pendingNetwork := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
					return current.Status.PhysicalNetworkRef != "", nil
				})
				require.Equal(t, apiv2.LogicalContainerNetworkPhasePending, pendingNetwork.Status.Phase)
			}
			failed := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return current.Status.Phase == apiv2.LogicalContainerPhaseFailed, nil
			})
			requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, "PhysicalContainerFailed")
			failureCondition := apimeta.FindStatusCondition(failed.Status.Conditions, string(apiv2.ConditionReady))
			require.Contains(t, failureCondition.Message, string(apiv2.PhysicalContainerImageReasonPullFailed))
			require.Contains(t, failureCondition.Message, "image acquisition denied")
			require.Empty(t, failed.Status.ContainerID)
			require.Zero(t, containerOrchestrator.CreateContainerCallCount(runtimeName))
			require.ErrorIs(t, testStateStore.VerifyResourceLeaseHeld(ctx, container, testResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)

			physical := &apiv2.PhysicalContainer{}
			physicalName := types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(container.UID)}
			require.NoError(t, client.Get(ctx, physicalName, physical))
			require.Equal(t, apiv2.PhysicalContainerPhaseFailed, physical.Status.Phase)
			requireReadyCondition(t, physical.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonImageFailed)
			image := &apiv2.PhysicalContainerImage{}
			require.NoError(t, client.Get(ctx, physicalName, image))
			require.Equal(t, apiv2.PhysicalContainerImagePhaseFailed, image.Status.Phase)
			require.NoError(t, client.Delete(ctx, image))
			require.NoError(t, client.Get(ctx, image.NamespacedName(), image))
			require.NotNil(t, image.DeletionTimestamp)
			require.Contains(t, image.Finalizers, controllers.LogicalBackingFinalizer)
			_, pullErr := containerOrchestrator.PullImage(ctx, containers.PullImageOptions{Image: sourceImage})
			require.NoError(t, pullErr)
			require.NoError(t, retryOnConflict(ctx, container.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
				current.Spec.Stop = true
				return client.Update(updateCtx, current)
			}))
			updatedPhysical := waitObjectAssumesState(t, ctx, physical.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return current.Spec.Stop && ready != nil && ready.ObservedGeneration == current.Generation, nil
			})
			require.Equal(t, apiv2.PhysicalContainerPhaseFailed, updatedPhysical.Status.Phase)
			requireReadyCondition(t, updatedPhysical.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonImageFailed)
			require.Empty(t, updatedPhysical.Status.ContainerID)
			require.Zero(t, containerOrchestrator.CreateContainerCallCount(runtimeName))
			require.NoError(t, client.Delete(ctx, container))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, image)
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, container)
		})
	}
}

// Verifies that the LogicalContainer controller completes a persistent stop while image readiness
// is pending, without creating a runtime container or retaining the startup lease.
func TestV2LogicalContainerControllerCompletesStopBeforeCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-stop-before-create")
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: namespace.Name + "-runtime",
			Container: &apiv2.LogicalContainerConfig{ImageRef: "unavailable-image"},
		},
	}
	require.NoError(t, client.Create(ctx, container))
	pending := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.PhysicalContainerRef != "" && ready != nil &&
			strings.Contains(ready.Message, string(apiv2.PhysicalContainerReasonImageNotFound)), nil
	})
	require.NoError(t, testStateStore.VerifyResourceLeaseHeld(ctx, container, testResourceLeaseOwner))
	require.NoError(t, retryOnConflict(ctx, container.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	}))
	stopped := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
	})
	requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, "ContainerStopped")
	require.Empty(t, stopped.Status.ContainerID)
	require.Zero(t, containerOrchestrator.CreateContainerCallCount(container.Spec.ContainerName))
	waitResourceLeaseReleased(t, ctx, container)
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: pending.Status.PhysicalContainerRef}, physical))
	require.Equal(t, apiv2.PhysicalContainerPhaseExited, physical.Status.Phase)
	requireReadyCondition(t, physical.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonStopRequested)
}

// Verifies that the LogicalContainer controller completes stop for a never-started persistent
// container, retaining its runtime identity and releasing the startup lease.
func TestV2LogicalContainerControllerStopsNeverStartedPersistentContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-stop-created")
	runtimeName := namespace.Name + "-runtime"
	runtimeID, createErr := containerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name: runtimeName, Image: "existing-image",
	})
	require.NoError(t, createErr)
	removeRuntimeContainerOnCleanup(t, runtimeID)
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: runtimeName, Stop: true,
			Container: &apiv2.LogicalContainerConfig{ImageRef: "unused-image"},
		},
	}
	require.NoError(t, client.Create(ctx, container))
	stopped := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
	})
	requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, "ContainerStopped")
	require.Equal(t, runtimeID, stopped.Status.ContainerID)
	require.True(t, stopped.Status.StartedAt.IsZero())
	require.Nil(t, stopped.Status.ExitCode)
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: stopped.Status.PhysicalContainerRef}, physical))
	require.Equal(t, apiv2.PhysicalContainerPhaseExited, physical.Status.Phase)
	require.Equal(t, string(containers.ContainerStatusCreated), physical.Status.RuntimeStatus)
	requireReadyCondition(t, physical.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonStopRequested)
	require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(runtimeName))
	waitResourceLeaseReleased(t, ctx, container)
}

// Verifies that namespace cleanup deletes logical containers and their referenced logical networks,
// releasing container finalizers even after the namespace begins terminating.
func TestV2NamespaceControllerCleansUpLogicalContainers(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-logical-namespace-cleanup")
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	require.NoError(t, client.Create(ctx, network))
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Container: &apiv2.LogicalContainerConfig{
				ImageRef: "unused-image",
				Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: network.Name}},
			},
		},
	}
	require.NoError(t, client.Create(ctx, container))
	pendingContainer := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhasePending && len(current.Finalizers) != 0, nil
	})
	require.NotNil(t, apimeta.FindStatusCondition(pendingContainer.Status.Conditions, string(apiv2.ConditionReady)))

	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.LogicalContainer](t, ctx, client, container)
	ctrl_testutil.WaitObjectDeleted[apiv2.LogicalContainerNetwork](t, ctx, client, network)
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
}

// Verifies that the LogicalContainer controller delegates image creation, container startup, stop,
// and deletion to owned physical resources while projecting details and computing logical readiness.
func TestV2LogicalContainerControllerDelegatesSessionLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-session")
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
			Image:   &apiv2.PhysicalContainerImageConfig{Image: "v2-logical-session-image"},
			Command: []string{"run"}, Env: []commonapi.EnvVar{{Name: "VALUE", Value: "expected"}},
			Ports:  []apiv2.ContainerPort{{ContainerPort: 8080}},
			Labels: []commonapi.Label{{Key: "user-label", Value: "preserved"}},
		}},
	}
	require.NoError(t, client.Create(ctx, container))
	running := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	requireReadyCondition(t, running.Status.Conditions, metav1.ConditionTrue, "ContainerRunning")
	require.True(t, strings.HasPrefix(running.Status.ContainerName, "session-"))
	expectedLifecycleKey, _, lifecycleKeyErr := container.Spec.GetLifecycleKey()
	require.NoError(t, lifecycleKeyErr)
	require.Equal(t, expectedLifecycleKey, running.Status.LifecycleKey)
	physicalName := types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, physicalName, physical))
	require.True(t, metav1.IsControlledBy(physical, running))
	require.False(t, physical.Spec.Container.RetainRuntimeContainer)
	require.Equal(t, running.Status.ImageRef, physical.Spec.Container.ImageRef)
	require.Equal(t, physical.Status.ContainerID, running.Status.ContainerID)
	require.Equal(t, physical.Status.CreatedAt, running.Status.CreatedAt)
	require.Equal(t, physical.Status.StartedAt, running.Status.StartedAt)
	require.Equal(t, physical.Status.PortMappings, running.Status.PortMappings)
	require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(running.Status.ContainerName))
	inspected, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{running.Status.ContainerID}})
	require.NoError(t, inspectErr)
	require.Len(t, inspected, 1)
	require.Equal(t, running.Status.LifecycleKey, inspected[0].Labels["com.microsoft.developer.usvc-dev.lifecycle-key"])
	require.Equal(t, "preserved", inspected[0].Labels["user-label"])

	stopErr := retryOnConflict(ctx, container.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	})
	require.NoError(t, stopErr)
	stopped := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
	})
	requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, "ContainerStopped")
	require.NoError(t, client.Get(ctx, physicalName, physical))
	require.True(t, physical.Spec.Stop)
	require.Equal(t, physical.Status.ExitCode, stopped.Status.ExitCode)
	require.Equal(t, physical.Status.FinishedAt, stopped.Status.FinishedAt)
	image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: running.Status.ImageRef, Namespace: namespace.Name}}
	require.NoError(t, client.Delete(ctx, container))
	ctrl_testutil.WaitObjectDeleted[apiv2.LogicalContainer](t, ctx, client, container)
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainer](t, ctx, client, physical)
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerImage](t, ctx, client, image)
	waitContainerMissing(t, ctx, running.Status.ContainerID)
}

// Verifies that a system stream opened while a logical container is Pending receives later
// physical failures, which are also available from the physical resource's own log endpoint.
func TestV2LogicalContainerControllerForwardsPhysicalSystemDiagnostics(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-system-diagnostics")
	runtimeName := namespace.Name + "-runtime"
	containerOrchestrator.FailNextCreateContainer(runtimeName,
		errors.Join(containers.ErrCouldNotAllocate, errors.New("system-log-routing-test")))
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			ContainerName: runtimeName,
			Container:     &apiv2.LogicalContainerConfig{ImageRef: "image"},
		},
	}
	require.NoError(t, client.Create(ctx, container))
	waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhasePending && current.Status.PhysicalContainerRef != "", nil
	})
	logicalStream, streamErr := openV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Source: "system", Follow: true})
	require.NoError(t, streamErr)
	defer func() { require.NoError(t, logicalStream.Close()) }()
	createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "image", namespace.Name+"-image")
	failed := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseFailed, nil
	})
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: failed.Status.PhysicalContainerRef}, physical))
	logicalContents, logicalErr := bufio.NewReader(logicalStream).ReadString('\n')
	require.NoError(t, logicalErr)
	physicalContents, physicalErr := readV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Source: "system"})
	require.NoError(t, physicalErr)
	require.Contains(t, logicalContents, "system-log-routing-test")
	require.Contains(t, physicalContents, "system-log-routing-test")
	require.Contains(t, logicalContents, "PhysicalContainerReconciler")
	require.Contains(t, physicalContents, "PhysicalContainerReconciler")
	require.NotContains(t, physicalContents, "LogicalContainerReconciler")
	require.NoError(t, client.Delete(ctx, container))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, container)
}

// Verifies that the LogicalContainer controller selects adoption or physical replacement according
// to persistent lifecycle keys, runtime state, and DCP ownership without upgrading adopted monitors.
// Inline image references are published only when the image is realized for creation.
func TestV2LogicalContainerControllerSelectsPersistentContainer(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		managed  bool
		key      string
		stopped  bool
		replaced bool
	}{
		{name: "matching", managed: true, key: "desired"},
		{name: "changed-key", managed: true, key: "previous", replaced: true},
		{name: "stopped", managed: true, key: "desired", stopped: true, replaced: true},
		{name: "unmanaged", key: "previous"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespaceName := "v2-logical-persistent-" + testCase.name
			namespace := createActiveV2Namespace(t, ctx, namespaceName)
			image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "image", namespaceName+"-image")
			runtimeName := namespaceName + "-runtime"
			labels := map[string]string{"com.microsoft.developer.usvc-dev.lifecycle-key": testCase.key}
			if testCase.managed {
				labels["com.microsoft.developer.usvc-dev.build"] = "test"
			}
			existingID, runErr := containerOrchestrator.RunContainer(ctx, containers.RunContainerOptions{
				CreateContainerOptions: containers.CreateContainerOptions{Name: runtimeName, Image: image.Status.ImageID, Labels: labels},
			})
			require.NoError(t, runErr)
			removeRuntimeContainerOnCleanup(t, existingID)
			if testCase.stopped {
				require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, existingID, containers.ContainerStatusExited))
			}
			monitorPID := int64(testResourceLeaseOwner.Pid)
			container := &apiv2.LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
				Spec: apiv2.LogicalContainerSpec{
					Mode: apiv2.LogicalContainerModePersistent, ContainerName: runtimeName, LifecycleKey: "desired",
					MonitorPID: &monitorPID, MonitorTimestamp: metav1.NewMicroTime(testResourceLeaseOwner.IdentityTime),
					Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespaceName + "-image"}},
				},
			}
			require.NoError(t, client.Create(ctx, container))
			running := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
			})
			physical := &apiv2.PhysicalContainer{}
			require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
			if testCase.replaced {
				require.NotEqual(t, existingID, running.Status.ContainerID)
				require.NotEmpty(t, running.Status.ImageRef)
				require.Equal(t, physical.Spec.Container.ImageRef, running.Status.ImageRef)
				require.True(t, physical.Spec.Container.ReplaceExisting)
				require.True(t, physical.Spec.Container.RetainRuntimeContainer)
				require.Equal(t, container.Spec.MonitorPID, physical.Spec.Container.MonitorPID)
				require.Len(t, physicalContainerMonitorProcesses(running.Status.ContainerID), 1)
				waitContainerMissing(t, ctx, existingID)
			} else {
				require.Equal(t, existingID, running.Status.ContainerID)
				require.Empty(t, running.Status.ImageRef)
				require.Equal(t, existingID, physical.Spec.ContainerID)
				require.Nil(t, physical.Spec.Container)
				require.Empty(t, physicalContainerMonitorProcesses(existingID))
			}
			removeRuntimeContainerOnCleanup(t, running.Status.ContainerID)
			waitResourceLeaseReleased(t, ctx, container)
			require.NoError(t, client.Delete(ctx, container))
			ctrl_testutil.WaitObjectDeleted[apiv2.LogicalContainer](t, ctx, client, container)
			remaining, remainingErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{running.Status.ContainerID}})
			require.NoError(t, remainingErr)
			require.Len(t, remaining, 1)
		})
	}
}

// Verifies that duplicate caller lifecycle labels cannot override the selected key or force
// replacement of a matching persistent runtime, while ordinary labels remain last-value-wins.
func TestV2LogicalContainerControllerReservedLabelsOverrideDuplicates(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-system-labels")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "image", namespace.Name+"-image")
	original := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: namespace.Name + "-runtime",
			LifecycleKey: "controller-selected-key",
			Container: &apiv2.LogicalContainerConfig{
				ImageRef: image.Name,
				Labels: []commonapi.Label{
					{Key: "com.microsoft.developer.usvc-dev.lifecycle-key", Value: "caller-first"},
					{Key: "user-label", Value: "first"},
					{Key: "com.microsoft.developer.usvc-dev.lifecycle-key", Value: "caller-last"},
					{Key: "user-label", Value: "last"},
				},
			},
		},
	}
	require.NoError(t, client.Create(ctx, original))
	first := waitObjectAssumesState(t, ctx, original.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	removeRuntimeContainerOnCleanup(t, first.Status.ContainerID)
	inspected, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{first.Status.ContainerID},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspected, 1)
	require.Equal(t, original.Spec.LifecycleKey, inspected[0].Labels["com.microsoft.developer.usvc-dev.lifecycle-key"])
	require.Equal(t, "last", inspected[0].Labels["user-label"])
	require.NoError(t, client.Delete(ctx, original))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, original)
	reused := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: namespace.Name},
		Spec:       *original.Spec.DeepCopy(),
	}
	require.NoError(t, client.Create(ctx, reused))
	second := waitObjectAssumesState(t, ctx, reused.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	require.Equal(t, first.Status.ContainerID, second.Status.ContainerID)
	require.Equal(t, original.Spec.LifecycleKey, second.Status.LifecycleKey)
	require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(original.Spec.ContainerName))
	require.Zero(t, containerOrchestrator.RemoveContainerCallCount(original.Spec.ContainerName))
	require.NoError(t, client.Delete(ctx, reused))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, reused)
}

// Verifies that the LogicalContainer controller uses a stable default key across map and port
// ordering and resource names, then delegates replacement when the desired command changes.
func TestV2LogicalContainerControllerCalculatesLifecycleKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-default-key")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "image", "v2-logical-default-key-image")
	original := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: "v2-logical-default-key-runtime",
			Container: &apiv2.LogicalContainerConfig{
				ImageRef: image.Name, Command: []string{"initial"},
				Env:    []commonapi.EnvVar{{Name: "B", Value: "2"}, {Name: "A", Value: "1"}},
				Labels: []commonapi.Label{{Key: "b", Value: "2"}, {Key: "a", Value: "1"}},
				Ports: []apiv2.ContainerPort{
					{ContainerPort: 8080},
					{ContainerPort: 5353, Protocol: commonapi.PortProtocolUDP},
				},
			},
		},
	}
	require.NoError(t, client.Create(ctx, original))
	first := waitObjectAssumesState(t, ctx, original.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	removeRuntimeContainerOnCleanup(t, first.Status.ContainerID)
	require.NoError(t, client.Delete(ctx, original))
	ctrl_testutil.WaitObjectDeleted[apiv2.LogicalContainer](t, ctx, client, original)
	second := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: namespace.Name},
		Spec:       *original.Spec.DeepCopy(),
	}
	second.Spec.Container.ImageRef = namespace.Name + "/" + image.Name
	second.Spec.Container.Env[0], second.Spec.Container.Env[1] = second.Spec.Container.Env[1], second.Spec.Container.Env[0]
	second.Spec.Container.Labels[0], second.Spec.Container.Labels[1] = second.Spec.Container.Labels[1], second.Spec.Container.Labels[0]
	second.Spec.Container.Ports[0], second.Spec.Container.Ports[1] = second.Spec.Container.Ports[1], second.Spec.Container.Ports[0]
	require.NoError(t, client.Create(ctx, second))
	reused := waitObjectAssumesState(t, ctx, second.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	require.Equal(t, first.Status.LifecycleKey, reused.Status.LifecycleKey)
	require.Equal(t, first.Status.ContainerID, reused.Status.ContainerID)
	require.NoError(t, client.Delete(ctx, second))
	ctrl_testutil.WaitObjectDeleted[apiv2.LogicalContainer](t, ctx, client, second)
	third := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "third", Namespace: namespace.Name},
		Spec:       *second.Spec.DeepCopy(),
	}
	third.Spec.Container.Command = []string{"changed"}
	require.NoError(t, client.Create(ctx, third))
	replaced := waitObjectAssumesState(t, ctx, third.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	removeRuntimeContainerOnCleanup(t, replaced.Status.ContainerID)
	require.NotEqual(t, reused.Status.LifecycleKey, replaced.Status.LifecycleKey)
	require.NotEqual(t, reused.Status.ContainerID, replaced.Status.ContainerID)
}

// Verifies that the LogicalContainer controller never creates a missing existing-mode container
// and uses physical observations, rather than logical status, for readiness and runtime details.
func TestV2LogicalContainerControllerObservesExistingContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-existing")
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerSpec{Mode: apiv2.LogicalContainerModeExisting, ContainerName: "v2-logical-existing-runtime"},
	}
	require.NoError(t, client.Create(ctx, container))
	waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "ExistingContainerNotFound", nil
	})
	require.Zero(t, containerOrchestrator.CreateContainerCallCount(container.Spec.ContainerName))
	runtimeID := runExistingTestContainer(t, ctx, container.Spec.ContainerName, "existing-image")
	running := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	require.Equal(t, runtimeID, running.Status.ContainerID)
	tamperErr := retryOnConflict(ctx, container.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
		current.Status.PhysicalContainerRef = "not-the-physical-container"
		current.Status.ContainerID = "not-the-runtime-container"
		current.Status.Phase = apiv2.LogicalContainerPhaseFailed
		return client.Status().Update(updateCtx, current)
	})
	require.NoError(t, tamperErr)
	restored := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.ContainerID == runtimeID && current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	require.Equal(t, running.Status.PhysicalContainerRef, restored.Status.PhysicalContainerRef)
	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, runtimeID, containers.ContainerStatusPaused))
	paused := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhasePaused, nil
	})
	requireReadyCondition(t, paused.Status.Conditions, metav1.ConditionFalse, "ContainerPaused")
	require.NoError(t, client.Delete(ctx, container))
	ctrl_testutil.WaitObjectDeleted[apiv2.LogicalContainer](t, ctx, client, container)
	remaining, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{runtimeID}})
	require.NoError(t, inspectErr)
	require.Len(t, remaining, 1)
	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, runtimeID, containers.ContainerStatusExited))
}

type blockingLogicalContainerInspector struct {
	containers.ContainerOrchestrator
	containerName string
	blocked       atomic.Bool
	entered       chan string
	release       chan struct{}
}

func (inspector *blockingLogicalContainerInspector) InspectContainers(
	ctx context.Context, options containers.InspectContainersOptions,
) ([]containers.InspectedContainer, error) {
	if len(options.Containers) == 1 && options.Containers[0] == inspector.containerName &&
		inspector.blocked.CompareAndSwap(false, true) {
		inspector.entered <- inspector.containerName
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case _, open := <-inspector.release:
			if !open {
				return nil, errors.New("lookup release channel was closed")
			}
		}
	}
	return inspector.ContainerOrchestrator.InspectContainers(ctx, options)
}

// Verifies that LogicalContainer reconciliation lets unrelated resources progress during lookup
// and honors deletion on the next reconciliation by removing its backing and releasing its lease.
func TestV2LogicalContainerControllerDeletesAfterLookup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var inspector *blockingLogicalContainerInspector
	serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerController|PhysicalContainerController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
			inspector = &blockingLogicalContainerInspector{
				ContainerOrchestrator: orchestrator, containerName: "blocked-runtime",
				entered: make(chan string, 1), release: make(chan struct{}, 1),
			}
			return inspector
		}})
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-delete-after-lookup"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	blocked := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "blocked", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: inspector.containerName,
			LifecycleKey: "custom", Container: &apiv2.LogicalContainerConfig{ImageRef: "unused-image"},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, blocked))
	select {
	case enteredName, open := <-inspector.entered:
		require.True(t, open)
		require.Equal(t, inspector.containerName, enteredName)
	case <-ctx.Done():
		require.FailNow(t, "logical container lookup did not start")
	}
	unrelated := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "unrelated-image"}},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, unrelated))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, unrelated.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.PhysicalContainerRef != "", nil
	})
	require.NoError(t, serverInfo.Client.Delete(ctx, blocked))
	inspector.release <- struct{}{}
	ctrl_testutil.WaitObjectDeleted[apiv2.LogicalContainer](t, ctx, serverInfo.Client, blocked)
	leaseErr := environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, blocked, environmentInfo.ResourceLeaseOwner)
	require.ErrorIs(t, leaseErr, statestore.ErrResourceLeaseNotHeld)
	var backings apiv2.PhysicalContainerList
	require.NoError(t, serverInfo.Client.List(ctx, &backings, ctrl_client.InNamespace(namespace.Name)))
	for _, backing := range backings.Items {
		require.False(t, metav1.IsControlledBy(&backing, blocked))
	}
}

// Verifies that adopting a runtime container never claims it for workload cleanup or reassigns
// an existing ownership record, including deletion before the backing reports readiness.
func TestV2LogicalContainerControllerDoesNotClaimAdoptedContainers(t *testing.T) {
	for _, testCase := range []struct {
		name              string
		managed           bool
		deleteBeforeReady bool
	}{
		{name: "unmanaged"},
		{name: "managed", managed: true},
		{name: "unmanaged-deleting", deleteBeforeReady: true},
		{name: "managed-deleting", managed: true, deleteBeforeReady: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var conflicts *backingStatusConflicts
			server, environment, startErr := StartTestEnvironmentWithOptions(t, ctx,
				LogicalContainerController|PhysicalContainerController, t.Name(), t.TempDir(), TestEnvironmentOptions{
					WorkloadID: "adopting-workload",
					DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
						conflicts = &backingStatusConflicts{
							Client: client, imageConflict: make(chan struct{}), containerConflict: make(chan struct{}),
						}
						conflicts.containerBlocked.Store(testCase.deleteBeforeReady)
						return conflicts
					},
				})
			require.NoError(t, startErr)
			defer conflicts.containerBlocked.Store(false)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-adopted-ownership"}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			var labels map[string]string
			if testCase.managed {
				labels = map[string]string{
					"com.microsoft.developer.usvc-dev.build":         "test",
					"com.microsoft.developer.usvc-dev.lifecycle-key": "custom",
				}
			}
			runtimeID, runErr := server.ContainerOrchestrator.RunContainer(ctx, containers.RunContainerOptions{
				CreateContainerOptions: containers.CreateContainerOptions{Name: "adopted-runtime", Image: "existing-image", Labels: labels},
			})
			require.NoError(t, runErr)
			container := &apiv2.LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
				Spec: apiv2.LogicalContainerSpec{
					Mode: apiv2.LogicalContainerModePersistent, ContainerName: "adopted-runtime",
					LifecycleKey: "custom", Container: &apiv2.LogicalContainerConfig{ImageRef: "unused-image"},
				},
			}
			if testCase.managed {
				require.NoError(t, environment.StateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
					ResourceKey: container.GetLeaseKey(), ContainerID: runtimeID, ContainerName: "adopted-runtime",
					RuntimeName: server.ContainerOrchestrator.Name(), WorkloadID: "original-workload",
				}))
			}
			require.NoError(t, server.Client.Create(ctx, container))
			if testCase.deleteBeforeReady {
				select {
				case <-conflicts.containerConflict:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				require.NoError(t, server.Client.Delete(ctx, container))
				waitObjectAssumesStateEx(t, ctx, server.Client, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
					ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
					return ready != nil && ready.Reason == "DeletingPhysicalResources", nil
				})
				require.ErrorIs(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, container, environment.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
				conflicts.containerBlocked.Store(false)
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, container)
			} else {
				running := waitObjectAssumesStateEx(t, ctx, server.Client, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
					return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
				})
				require.Equal(t, runtimeID, running.Status.ContainerID)
			}
			record, recordErr := environment.StateStore.GetPersistentContainer(ctx, container.GetLeaseKey())
			if testCase.managed {
				require.NoError(t, recordErr)
				require.Equal(t, commonapi.WorkloadID("original-workload"), record.WorkloadID)
				require.Equal(t, runtimeID, record.ContainerID)
			} else {
				require.ErrorIs(t, recordErr, statestore.ErrPersistentContainerNotFound)
			}
			records, recordsErr := environment.StateStore.ListPersistentContainersByWorkloadID(ctx, "adopting-workload")
			require.NoError(t, recordsErr)
			require.Empty(t, records)
			require.ErrorIs(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, container, environment.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
			if !testCase.deleteBeforeReady {
				require.NoError(t, server.Client.Delete(ctx, container))
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, container)
			}
			inspected, inspectErr := server.ContainerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{runtimeID}})
			require.NoError(t, inspectErr)
			require.Len(t, inspected, 1)
			require.Equal(t, containers.ContainerStatusRunning, inspected[0].Status)
		})
	}
}

// Verifies that Creating settles persistent workload registration when a runtime ID is
// reported, before the physical container's Running status can be saved.
func TestV2LogicalContainerControllerRecordsWorkloadBeforeStartupSettles(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var conflicts *backingStatusConflicts
	server, environment, startErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
		t.Name(), t.TempDir(), TestEnvironmentOptions{
			WorkloadID: "early-workload",
			DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
				conflicts = &backingStatusConflicts{
					Client: client, imageConflict: make(chan struct{}), containerConflict: make(chan struct{}),
				}
				conflicts.containerBlocked.Store(true)
				return conflicts
			},
		})
	require.NoError(t, startErr)
	defer conflicts.containerBlocked.Store(false)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "early-workload"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	logical := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: "early-workload-runtime",
			Container: &apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: "early-workload-image"},
			},
		},
	}
	require.NoError(t, server.Client.Create(ctx, logical))
	observed := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.ContainerID != "" && current.Status.Phase == apiv2.LogicalContainerPhasePending, nil
	})
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{
		Namespace: namespace.Name, Name: observed.Status.PhysicalContainerRef,
	}, physical))
	require.Equal(t, apiv2.PhysicalContainerPhasePending, physical.Status.Phase)
	record, recordErr := environment.StateStore.GetPersistentContainer(ctx, logical.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, observed.Status.ContainerID, record.ContainerID)
	require.Equal(t, commonapi.WorkloadID("early-workload"), record.WorkloadID)
	require.ErrorIs(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, logical, environment.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
	conflicts.containerBlocked.Store(false)
	require.NoError(t, server.Client.Delete(ctx, logical))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
}

// Verifies that the LogicalContainer controller records persistent workload ownership and
// releases its startup lease before reporting a newly created runtime container as ready.
func TestV2LogicalContainerControllerRecordsPersistentWorkload(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{WorkloadID: "logical-container-workload"})
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-workload"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: "v2-logical-workload-runtime",
			LifecycleKey: "custom", Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "workload-image"}},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, container))
	running := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	require.NotEmpty(t, running.Status.ContainerID)
	record, recordErr := environmentInfo.StateStore.GetPersistentContainer(ctx, container.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, running.Status.ContainerID, record.ContainerID)
	require.Equal(t, commonapi.WorkloadID("logical-container-workload"), record.WorkloadID)
	verifyErr := environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, container, environmentInfo.ResourceLeaseOwner)
	require.ErrorIs(t, verifyErr, statestore.ErrResourceLeaseNotHeld)
}

// Verifies that the LogicalContainer controller records a namespace workload override even
// without a global workload ID, and releases the startup lease before reporting readiness.
func TestV2LogicalContainerControllerRecordsNamespaceWorkload(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-namespace-workload")
	require.NoError(t, retryOnConflict(ctx, namespace.NamespacedName(), func(updateCtx context.Context, current *apiv2.Namespace) error {
		current.Annotations = map[string]string{apiv2.NamespaceWorkloadIDAnnotation: " namespace-container-workload "}
		return client.Update(updateCtx, current)
	}))
	runtimeName := namespace.Name + "-runtime"
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: runtimeName,
			LifecycleKey: "custom", Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: runtimeName + "-image"}},
		},
	}
	require.NoError(t, client.Create(ctx, container))
	running := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	require.NotEmpty(t, running.Status.ContainerID)
	record, recordErr := testStateStore.GetPersistentContainer(ctx, container.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, running.Status.ContainerID, record.ContainerID)
	require.Equal(t, commonapi.WorkloadID("namespace-container-workload"), record.WorkloadID)
	waitResourceLeaseReleased(t, ctx, container)
}

// Verifies that the LogicalContainer controller does not silently omit logical network
// attachments or create a runtime container before logical network realization is available.
func TestV2LogicalContainerControllerWaitsForLogicalNetworks(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-networks")
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "networked", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
			Image:    &apiv2.PhysicalContainerImageConfig{Image: "networked-image"},
			Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "network"}},
		}},
	}
	require.NoError(t, client.Create(ctx, container))
	pending := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "PhysicalResourcePreparationPending", nil
	})
	require.Equal(t, apiv2.LogicalContainerPhasePending, pending.Status.Phase)
	require.Empty(t, pending.Status.PhysicalContainerRef)
	var physicalContainers apiv2.PhysicalContainerList
	require.NoError(t, client.List(ctx, &physicalContainers, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, physicalContainers.Items)
}

// Verifies that the LogicalContainer controller reports a running physical container as not ready
// when the runtime's port mappings cannot be resolved.
func TestV2LogicalContainerControllerReportsPhysicalPortMappingFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-port-mapping-failure")
	runtimeName := namespace.Name + "-runtime"
	runtimeID := runExistingTestContainer(t, ctx, runtimeName, "existing-image")
	require.NoError(t, containerOrchestrator.SimulateContainerPortMappings(ctx, runtimeID,
		containers.InspectedContainerPortMapping{"invalid-port": nil}))
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerSpec{Mode: apiv2.LogicalContainerModeExisting, ContainerName: runtimeName},
	}
	require.NoError(t, client.Create(ctx, container))
	notReady := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.ContainerID == runtimeID && ready != nil &&
			strings.Contains(ready.Message, string(apiv2.PhysicalContainerReasonPortMappingResolutionFailed)), nil
	})
	require.Equal(t, apiv2.LogicalContainerPhasePending, notReady.Status.Phase)
	requireReadyCondition(t, notReady.Status.Conditions, metav1.ConditionFalse, "PhysicalContainerNotReady")
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: notReady.Status.PhysicalContainerRef}, physical))
	require.Equal(t, apiv2.PhysicalContainerPhaseRunning, physical.Status.Phase)
	require.Equal(t, runtimeID, physical.Status.ContainerID)
	requireReadyCondition(t, physical.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonPortMappingResolutionFailed)
}

// Verifies that the LogicalContainer controller forwards an initial persistent stop request to an
// adopted physical reference instead of replacing a container whose lifecycle key no longer matches.
func TestV2LogicalContainerControllerStopsWithoutReplacingPersistentContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-stop-persistent")
	runtimeName := "v2-logical-stop-persistent-runtime"
	runtimeID, runErr := containerOrchestrator.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name: runtimeName, Image: "existing-image",
			Labels: map[string]string{
				"com.microsoft.developer.usvc-dev.build":         "test",
				"com.microsoft.developer.usvc-dev.lifecycle-key": "old-key",
			},
		},
	})
	require.NoError(t, runErr)
	removeRuntimeContainerOnCleanup(t, runtimeID)
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: runtimeName, Stop: true,
			LifecycleKey: "new-key", Container: &apiv2.LogicalContainerConfig{ImageRef: "unused-image"},
		},
	}
	require.NoError(t, client.Create(ctx, container))
	stopped := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
	})
	requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, "ContainerStopped")
	require.Equal(t, runtimeID, stopped.Status.ContainerID)
	require.Equal(t, "old-key", stopped.Status.LifecycleKey)
	require.Zero(t, containerOrchestrator.CreateContainerCallCount(runtimeName))
	require.Zero(t, containerOrchestrator.RemoveContainerCallCount(runtimeName))
	waitResourceLeaseReleased(t, ctx, container)
}

// Verifies that stopping a declared replacement stops the original runtime without creating
// a new one, and that its template-backed ID is registered even when stop is requested.
func TestV2LogicalContainerControllerStopsExistingRuntimeDuringReplacement(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-stop-replacement")
	require.NoError(t, retryOnConflictEx(ctx, client, namespace.NamespacedName(), func(updateCtx context.Context, current *apiv2.Namespace) error {
		current.Annotations = map[string]string{apiv2.NamespaceWorkloadIDAnnotation: "stopping-workload"}
		return client.Update(updateCtx, current)
	}))
	runtimeName := namespace.Name + "-runtime"
	runtimeID, runErr := containerOrchestrator.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name: runtimeName, Image: "existing-image",
			Labels: map[string]string{
				"com.microsoft.developer.usvc-dev.build":         "test",
				"com.microsoft.developer.usvc-dev.lifecycle-key": "old-key",
			},
		},
	})
	require.NoError(t, runErr)
	removeRuntimeContainerOnCleanup(t, runtimeID)
	source := namespace.Name + "-image"
	releasePull := containerOrchestrator.BlockPullImage(source)
	defer releasePull()
	logical := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: runtimeName, LifecycleKey: "new-key",
			Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: source}},
		},
	}
	require.NoError(t, testStateStore.UpsertPersistentContainer(ctx, statestore.PersistentContainerRecord{
		ResourceKey: logical.GetLeaseKey(), ContainerID: runtimeID, ContainerName: runtimeName,
		RuntimeName: containerOrchestrator.Name(), WorkloadID: "original-workload",
	}))
	require.NoError(t, client.Create(ctx, logical))
	waitPullImageCallCount(t, ctx, source, 1)
	physicalName := types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(logical.UID)}
	waitObjectAssumesState(t, ctx, physicalName, func(current *apiv2.PhysicalContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Spec.Container != nil && current.Spec.Container.ReplaceExisting &&
			ready != nil && ready.Reason == string(apiv2.PhysicalContainerReasonImageNotReady), nil
	})
	require.NoError(t, retryOnConflictEx(ctx, client, logical.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	}))
	stopped := waitObjectAssumesState(t, ctx, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
	})
	require.Equal(t, runtimeID, stopped.Status.ContainerID)
	requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, "ContainerStopped")
	inspected, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{runtimeID}})
	require.NoError(t, inspectErr)
	require.Len(t, inspected, 1)
	require.Equal(t, containers.ContainerStatusExited, inspected[0].Status)
	require.Zero(t, containerOrchestrator.CreateContainerCallCount(runtimeName))
	require.Zero(t, containerOrchestrator.RemoveContainerCallCount(runtimeName))
	record, recordErr := testStateStore.GetPersistentContainer(ctx, logical.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, runtimeID, record.ContainerID)
	require.Equal(t, commonapi.WorkloadID("stopping-workload"), record.WorkloadID)
	require.NoError(t, client.Delete(ctx, logical))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, logical)
	retained, retainedErr := testStateStore.GetPersistentContainer(ctx, logical.GetLeaseKey())
	require.NoError(t, retainedErr)
	require.Equal(t, runtimeID, retained.ContainerID)
}

// Verifies that a stop request received during lookup is forwarded to the resulting physical
// backing on a subsequent reconciliation, regardless of the initial creation decision.
func TestV2LogicalContainerControllerForwardsStopAfterLookup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var inspector *blockingLogicalContainerInspector
	serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerController|PhysicalContainerController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
			inspector = &blockingLogicalContainerInspector{
				ContainerOrchestrator: orchestrator, containerName: "v2-logical-stop-during-lookup-runtime",
				entered: make(chan string, 1), release: make(chan struct{}, 1),
			}
			return inspector
		}})
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-stop-during-lookup"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	_, runErr := serverInfo.ContainerOrchestrator.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name: inspector.containerName, Image: "existing-image",
			Labels: map[string]string{
				"com.microsoft.developer.usvc-dev.build":         "test",
				"com.microsoft.developer.usvc-dev.lifecycle-key": "old-key",
			},
		},
	})
	require.NoError(t, runErr)
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: inspector.containerName,
			LifecycleKey: "new-key", Container: &apiv2.LogicalContainerConfig{ImageRef: "unused-image"},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, container))
	select {
	case _, open := <-inspector.entered:
		require.True(t, open)
	case <-ctx.Done():
		require.FailNow(t, "logical container lookup did not start")
	}
	stopErr := retryOnConflictEx(ctx, serverInfo.Client, container.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
		current.Spec.Stop = true
		return serverInfo.Client.Update(updateCtx, current)
	})
	require.NoError(t, stopErr)
	inspector.release <- struct{}{}
	stopped := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
	})
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, serverInfo.Client.Get(ctx, types.NamespacedName{
		Namespace: namespace.Name, Name: stopped.Status.PhysicalContainerRef,
	}, physical))
	require.True(t, physical.Spec.Stop)
	require.Equal(t, apiv2.PhysicalContainerPhaseExited, physical.Status.Phase)
}
