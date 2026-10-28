/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcpclient"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

type backingStatusConflicts struct {
	ctrl_client.Client
	imageBlocked                 atomic.Bool
	containerBlocked             atomic.Bool
	imageConflict                chan struct{}
	containerConflict            chan struct{}
	imageOnce                    sync.Once
	containerOnce                sync.Once
	version                      atomic.Uint64
	containerReleaseResponseLost atomic.Bool
	imageReleaseResponseLost     atomic.Bool
	logicalFinalizerConflict     atomic.Bool
}

// Verifies that deleting a logical container waiting for an external image or volume completes
// without supplying that dependency, creating a runtime container, or retaining its startup lease.
func TestV2LogicalContainerDeletesWhileWaitingForDependencies(t *testing.T) {
	for _, dependency := range []string{"image", "volume"} {
		t.Run(dependency, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespace := createActiveV2Namespace(t, ctx, "v2-logical-delete-missing-"+dependency)
			config := &apiv2.LogicalContainerConfig{ImageRef: "missing"}
			reason := apiv2.PhysicalContainerReasonImageNotFound
			if dependency == "volume" {
				image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "ready-image", namespace.Name+"-image")
				config.ImageRef = image.Name
				config.VolumeMounts = []apiv2.VolumeMount{{Type: apiv2.NamedVolumeMount, VolumeRef: "missing", Target: "/data"}}
				reason = apiv2.PhysicalContainerReasonVolumeNotFound
			}
			logical := &apiv2.LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
				Spec: apiv2.LogicalContainerSpec{
					Mode: apiv2.LogicalContainerModePersistent, ContainerName: namespace.Name + "-runtime", Container: config,
				},
			}
			require.NoError(t, client.Create(ctx, logical))
			name := types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(logical.UID)}
			physical := waitObjectAssumesState(t, ctx, name, func(current *apiv2.PhysicalContainer) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return ready != nil && ready.Reason == string(reason), nil
			})
			require.NoError(t, client.Delete(ctx, logical))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, logical)
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
			require.Zero(t, containerOrchestrator.CreateContainerCallCount(logical.Spec.ContainerName))
			waitResourceLeaseReleased(t, ctx, logical)
		})
	}
}

// Verifies that deleting an unstarted container cancels its owned image acquisition
// instead of waiting for the pull to complete or creating a runtime container.
func TestV2LogicalContainerDeletionCancelsPendingOwnedImage(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-delete-pulling")
	source := namespace.Name + "-image"
	releasePull := containerOrchestrator.BlockPullImage(source)
	defer releasePull()
	logical := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			ContainerName: namespace.Name + "-runtime",
			Container:     &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: source}},
		},
	}
	require.NoError(t, client.Create(ctx, logical))
	waitPullImageCallCount(t, ctx, source, 1)
	name := types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(logical.UID)}
	physical := waitObjectAssumesState(t, ctx, name, func(current *apiv2.PhysicalContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == string(apiv2.PhysicalContainerReasonImageNotReady), nil
	})
	require.NoError(t, client.Delete(ctx, logical))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, logical)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
	image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace}}
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, image)
	require.Zero(t, containerOrchestrator.CreateContainerCallCount(logical.Spec.ContainerName))
}

// Verifies that logical deletion retains the physical backing while container creation is
// in flight, then removes the confirmed runtime container after creation and startup settle.
func TestV2LogicalContainerDeletionWaitsForInFlightCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-delete-creating")
	runtimeName := namespace.Name + "-runtime"
	releaseCreate := containerOrchestrator.BlockCreateContainer(runtimeName)
	defer releaseCreate()
	logical := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			ContainerName: runtimeName,
			Container:     &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
		},
	}
	require.NoError(t, client.Create(ctx, logical))
	waitCreateContainerCallCount(t, ctx, runtimeName, 1)
	require.NoError(t, client.Delete(ctx, logical))
	waitObjectAssumesState(t, ctx, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "DeletingPhysicalResources", nil
	})
	name := types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(logical.UID)}
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, name, physical))
	require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
	require.True(t, physical.DeletionTimestamp.IsZero())
	require.False(t, physical.Spec.Stop)
	releaseCreate()
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, logical)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
	_, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{runtimeName}})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
	require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(runtimeName))
}

type backingCreateResponseLoss struct {
	ctrl_client.Client
	created chan *apiv2.PhysicalContainer
	observe chan struct{}
}

type logicalObservationGate struct {
	ctrl_client.Client
	observe <-chan struct{}
}

func (client *logicalObservationGate) Get(ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption) error {
	if _, logical := obj.(*apiv2.LogicalContainer); logical {
		select {
		case <-client.observe:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return client.Client.Get(ctx, key, obj, options...)
}

// Verifies that a LogicalContainer first observed during deletion removes its finalizer
// without initializing lifecycle work, creating backings, or acquiring a startup lease.
func TestV2LogicalContainerDeletesWithoutInitializingLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	observe := make(chan struct{})
	var observeOnce sync.Once
	allowObservation := func() { observeOnce.Do(func() { close(observe) }) }
	defer allowObservation()
	server, environment, startErr := StartTestEnvironmentWithOptions(t, ctx,
		NamespaceController|LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
		t.Name(), t.TempDir(), TestEnvironmentOptions{
			WorkloadID: "uninitialized-workload",
			DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
				return &logicalObservationGate{Client: client, observe: observe}
			},
		})
	require.NoError(t, startErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "uninitialized-logical"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	logical := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name: "logical", Namespace: namespace.Name,
			Finalizers: []string{apiv2.GroupVersion.Group + "/logicalcontainer-reconciler"},
		},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: "uninitialized-runtime",
			Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "uninitialized-image"}},
		},
	}
	require.NoError(t, server.Client.Create(ctx, logical))
	require.NoError(t, server.Client.Delete(ctx, logical))
	allowObservation()
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
	var physicalContainers apiv2.PhysicalContainerList
	require.NoError(t, server.Client.List(ctx, &physicalContainers, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, physicalContainers.Items)
	var images apiv2.PhysicalContainerImageList
	require.NoError(t, server.Client.List(ctx, &images, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, images.Items)
	orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.Zero(t, orchestrator.CreateContainerCallCount("uninitialized-runtime"))
	require.Zero(t, orchestrator.PullImageCallCount("uninitialized-image"))
	require.ErrorIs(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, logical, environment.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
}

func (client *backingCreateResponseLoss) Get(ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption) error {
	if _, physical := obj.(*apiv2.PhysicalContainer); physical {
		select {
		case <-client.observe:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return client.Client.Get(ctx, key, obj, options...)
}

func (client *backingCreateResponseLoss) Create(ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	createErr := client.Client.Create(ctx, obj, options...)
	if createErr != nil {
		return createErr
	}
	if physical, isPhysical := obj.(*apiv2.PhysicalContainer); isPhysical {
		select {
		case client.created <- physical.DeepCopy():
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-client.observe:
			return fmt.Errorf("injected lost response after committed backing create")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Verifies that deletion before the first physical reconciliation is protected by atomically
// installed finalizers, and namespace cleanup settles a lost create response without runtime creation or ownership.
func TestV2LogicalContainerDeletesUnobservedBackingAfterNamespaceTermination(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	created := make(chan *apiv2.PhysicalContainer, 1)
	observe := make(chan struct{})
	var observeOnce sync.Once
	allowObservation := func() { observeOnce.Do(func() { close(observe) }) }
	defer allowObservation()
	server, environment, startErr := StartTestEnvironmentWithOptions(t, ctx,
		NamespaceController|LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
		t.Name(), t.TempDir(), TestEnvironmentOptions{
			WorkloadID: "unobserved-workload",
			DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
				return &backingCreateResponseLoss{Client: client, created: created, observe: observe}
			},
		})
	require.NoError(t, startErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "unobserved-backing"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	logical := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: "unobserved-runtime",
			Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "unobserved-image"}},
		},
	}
	require.NoError(t, server.Client.Create(ctx, logical))
	var physical *apiv2.PhysicalContainer
	select {
	case physical = <-created:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Len(t, physical.Finalizers, 2)
	require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
	require.Empty(t, physical.Status.Conditions)
	require.NoError(t, server.Client.Delete(ctx, physical))
	require.NoError(t, server.Client.Delete(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return !current.DeletionTimestamp.IsZero(), nil
	})
	allowObservation()
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
	orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.Zero(t, orchestrator.CreateContainerCallCount("unobserved-runtime"))
	records, recordsErr := environment.StateStore.ListPersistentContainersByWorkloadID(ctx, "unobserved-workload")
	require.NoError(t, recordsErr)
	require.Empty(t, records)
	require.ErrorIs(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, logical, environment.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
}

func (client *backingStatusConflicts) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.PatchOption) error {
	if logical, isLogical := obj.(*apiv2.LogicalContainer); isLogical &&
		!slices.Contains(logical.Finalizers, apiv2.GroupVersion.Group+"/logicalcontainer-reconciler") &&
		client.logicalFinalizerConflict.CompareAndSwap(false, true) {
		competing := &apiv2.LogicalContainer{}
		getErr := client.Client.Get(ctx, logical.NamespacedName(), competing)
		if getErr != nil {
			return getErr
		}
		competing.Annotations = map[string]string{"test.dcp.microsoft.com/finalizer-conflict": "true"}
		updateErr := client.Client.Update(ctx, competing)
		if updateErr != nil {
			return updateErr
		}
	}
	loseResponse := false
	if !slices.Contains(obj.GetFinalizers(), controllers.LogicalBackingFinalizer) {
		switch obj.(type) {
		case *apiv2.PhysicalContainer:
			loseResponse = client.containerReleaseResponseLost.CompareAndSwap(false, true)
		case *apiv2.PhysicalContainerImage:
			loseResponse = client.imageReleaseResponseLost.CompareAndSwap(false, true)
		}
	}
	patchErr := client.Client.Patch(ctx, obj, patch, options...)
	if patchErr != nil {
		return patchErr
	}
	if loseResponse {
		return fmt.Errorf("injected lost response after committed backing release")
	}
	return nil
}

// Verifies that removing either owned backing's logical hold is a terminal protocol failure,
// including after backing deletion and a later logical metadata wake, without recreating the backing.
func TestV2LogicalContainerRejectsUnauthorizedBackingRelease(t *testing.T) {
	for _, backingKind := range []string{"container", "image"} {
		t.Run(backingKind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
				NamespaceController|LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
				t.Name(), t.TempDir(), TestEnvironmentOptions{})
			require.NoError(t, startErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "unauthorized-release"}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			logical := &apiv2.LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
				Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
					Image: &apiv2.PhysicalContainerImageConfig{Image: "held-image"},
				}},
			}
			require.NoError(t, server.Client.Create(ctx, logical))
			running := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
			})
			orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			require.NoError(t, orchestrator.SimulateContainerLogging(running.Status.ContainerID, apiv1.LogStreamSourceStdout, []byte("history\n")))
			gvr := logical.GetGroupVersionResource()
			follower, followErr := server.RestClient.Get().
				AbsPath("/apis", gvr.Group, gvr.Version, "namespaces", namespace.Name, gvr.Resource, logical.Name, apiv2.LogSubresourceName).
				SpecificallyVersionedParams(&apiv2.ResourceLogOptions{Follow: true},
					runtime.NewParameterCodec(dcpclient.NewScheme()), apiv2.GroupVersion).Stream(ctx)
			require.NoError(t, followErr)
			defer func() { require.NoError(t, follower.Close()) }()
			history := make([]byte, len("history\n"))
			_, historyErr := io.ReadFull(follower, history)
			require.NoError(t, historyErr)
			name := types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(logical.UID)}
			var backing ctrl_client.Object = &apiv2.PhysicalContainer{}
			reason := "PhysicalContainerChanged"
			if backingKind == "image" {
				backing = &apiv2.PhysicalContainerImage{}
				reason = "PhysicalImageChanged"
			}
			require.NoError(t, server.Client.Get(ctx, name, backing))
			switch backing.(type) {
			case *apiv2.PhysicalContainer:
				require.NoError(t, retryOnConflictEx(ctx, server.Client, name, func(updateCtx context.Context, current *apiv2.PhysicalContainer) error {
					current.Finalizers = slices.DeleteFunc(current.Finalizers, func(finalizer string) bool {
						return finalizer == controllers.LogicalBackingFinalizer
					})
					return server.Client.Update(updateCtx, current)
				}))
			case *apiv2.PhysicalContainerImage:
				require.NoError(t, retryOnConflictEx(ctx, server.Client, name, func(updateCtx context.Context, current *apiv2.PhysicalContainerImage) error {
					current.Finalizers = slices.DeleteFunc(current.Finalizers, func(finalizer string) bool {
						return finalizer == controllers.LogicalBackingFinalizer
					})
					return server.Client.Update(updateCtx, current)
				}))
			}
			failed := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return current.Status.Phase == apiv2.LogicalContainerPhaseFailed && ready != nil && ready.Reason == reason, nil
			})
			remaining, readErr := io.ReadAll(follower)
			require.NoError(t, readErr)
			require.Empty(t, remaining)
			require.NoError(t, server.Client.Delete(ctx, backing))
			switch currentBacking := backing.(type) {
			case *apiv2.PhysicalContainer:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, currentBacking)
			case *apiv2.PhysicalContainerImage:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, currentBacking)
			}
			require.NoError(t, retryOnConflictEx(ctx, server.Client, logical.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
				current.Annotations = map[string]string{"test.dcp.microsoft.com/wake": "after-deletion"}
				return server.Client.Update(updateCtx, current)
			}))
			require.NoError(t, server.Client.Get(ctx, failed.NamespacedName(), failed))
			require.Equal(t, apiv2.LogicalContainerPhaseFailed, failed.Status.Phase)
			require.Equal(t, reason, apimeta.FindStatusCondition(failed.Status.Conditions, string(apiv2.ConditionReady)).Reason)
			require.True(t, apierrors.IsNotFound(server.Client.Get(ctx, name, backing)))
			require.Equal(t, 1, orchestrator.PullImageCallCount("held-image"))
		})
	}
}

func (client *backingStatusConflicts) Status() ctrl_client.SubResourceWriter {
	return &backingStatusWriter{SubResourceWriter: client.Client.Status(), client: client}
}

type backingStatusWriter struct {
	ctrl_client.SubResourceWriter
	client *backingStatusConflicts
}

func (writer *backingStatusWriter) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.SubResourcePatchOption) error {
	block := false
	var conflictSignal chan struct{}
	var conflictOnce *sync.Once
	switch current := obj.(type) {
	case *apiv2.PhysicalContainerImage:
		block = current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady && writer.client.imageBlocked.Load()
		conflictSignal, conflictOnce = writer.client.imageConflict, &writer.client.imageOnce
	case *apiv2.PhysicalContainer:
		block = current.Status.Phase == apiv2.PhysicalContainerPhaseRunning && writer.client.containerBlocked.Load()
		conflictSignal, conflictOnce = writer.client.containerConflict, &writer.client.containerOnce
	}
	if block {
		// Commit a competing metadata write so the real API rejects the stale status patch.
		competing := obj.DeepCopyObject().(ctrl_client.Object)
		getErr := writer.client.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(obj), competing)
		if getErr != nil {
			return getErr
		}
		annotations := competing.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["test.dcp.microsoft.com/status-conflict"] = fmt.Sprint(writer.client.version.Add(1))
		competing.SetAnnotations(annotations)
		updateErr := writer.client.Client.Update(ctx, competing)
		if updateErr != nil {
			return updateErr
		}
	}
	patchErr := writer.SubResourceWriter.Patch(ctx, obj, patch, options...)
	if block && apierrors.IsConflict(patchErr) {
		conflictOnce.Do(func() { close(conflictSignal) })
	}
	return patchErr
}

// Verifies that atomically held container/image backings survive deletion during status conflicts,
// retain their successful runtime outcomes, and release only the logical hold after stable ownership settlement.
func TestV2LogicalContainerBackingsWaitForDurableStatusBeforeDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var conflicts *backingStatusConflicts
	server, environment, startErr := StartTestEnvironmentWithOptions(t, ctx,
		NamespaceController|LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
		t.Name(), t.TempDir(), TestEnvironmentOptions{
			WorkloadID: commonapi.WorkloadID("backing-workload"),
			DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
				conflicts = &backingStatusConflicts{
					Client: client, imageConflict: make(chan struct{}), containerConflict: make(chan struct{}),
				}
				conflicts.imageBlocked.Store(true)
				conflicts.containerBlocked.Store(true)
				return conflicts
			},
		})
	require.NoError(t, startErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "held-physical-backings"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	logical := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Mode: apiv2.LogicalContainerModePersistent, ContainerName: "held-runtime-container",
			Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "held-runtime-image"}},
		},
	}
	require.NoError(t, server.Client.Create(ctx, logical))
	select {
	case <-conflicts.imageConflict:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	name := types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(logical.UID)}
	physical := waitObjectAssumesStateEx(t, ctx, server.Client, name, func(current *apiv2.PhysicalContainer) (bool, error) {
		return current.Spec.Container != nil, nil
	})
	image := &apiv2.PhysicalContainerImage{}
	require.NoError(t, server.Client.Get(ctx, name, image))
	require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
	require.Contains(t, image.Finalizers, controllers.LogicalBackingFinalizer)
	require.Len(t, physical.Finalizers, 2)
	require.Len(t, image.Finalizers, 2)
	const independentFinalizer = "test.dcp.microsoft.com/independent-owner"
	require.NoError(t, retryOnConflictEx(ctx, server.Client, name, func(updateCtx context.Context, current *apiv2.PhysicalContainerImage) error {
		current.Finalizers = append(current.Finalizers, independentFinalizer)
		return server.Client.Update(updateCtx, current)
	}))
	require.NoError(t, server.Client.Delete(ctx, image))
	require.NoError(t, server.Client.Delete(ctx, physical))
	conflicts.imageBlocked.Store(false)
	select {
	case <-conflicts.containerConflict:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, server.Client.Delete(ctx, logical))
	require.NoError(t, server.Client.Get(ctx, name, physical))
	require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
	require.NotEqual(t, apiv2.PhysicalContainerPhaseRunning, physical.Status.Phase)
	orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.Equal(t, 1, orchestrator.PullImageCallCount("held-runtime-image"))
	require.Equal(t, 1, orchestrator.CreateContainerCallCount("held-runtime-container"))
	conflicts.containerBlocked.Store(false)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
	remainingImage := waitObjectAssumesStateEx(t, ctx, server.Client, name, func(current *apiv2.PhysicalContainerImage) (bool, error) {
		return len(current.Finalizers) == 1 && current.Finalizers[0] == independentFinalizer, nil
	})
	require.NotContains(t, remainingImage.Finalizers, controllers.LogicalBackingFinalizer)
	records, recordsErr := environment.StateStore.ListPersistentContainersByWorkloadID(ctx, "backing-workload")
	require.NoError(t, recordsErr)
	require.Len(t, records, 1)
	require.Equal(t, "held-runtime-container", records[0].ContainerName)
	remainingImage.Finalizers = slices.DeleteFunc(remainingImage.Finalizers, func(finalizer string) bool { return finalizer == independentFinalizer })
	require.NoError(t, server.Client.Update(ctx, remainingImage))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, image)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
	require.True(t, conflicts.containerReleaseResponseLost.Load())
	require.True(t, conflicts.imageReleaseResponseLost.Load())
	require.True(t, conflicts.logicalFinalizerConflict.Load())
}
