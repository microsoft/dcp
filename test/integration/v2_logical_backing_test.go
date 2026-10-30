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
	stopStatusBlocked            atomic.Bool
	imageConflict                chan struct{}
	containerConflict            chan struct{}
	imageOnce                    sync.Once
	containerOnce                sync.Once
	version                      atomic.Uint64
	containerReleaseResponseLost atomic.Bool
	imageReleaseResponseLost     atomic.Bool
	logicalFinalizerConflict     atomic.Bool
}

// Verifies that logical deletion completes from dependency, creation-retry, replacement-retry,
// and ownership-conflict waits without creating new runtime work or retaining the startup lease.
func TestV2LogicalContainerDeletesWhileWaitingForDependenciesOrRetries(t *testing.T) {
	for _, dependency := range []string{"image", "volume", "create", "partial-create", "replacement", "tracked"} {
		t.Run(dependency, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespace := createActiveV2Namespace(t, ctx, "v2-logical-delete-missing-"+dependency)
			config := &apiv2.LogicalContainerConfig{ImageRef: "missing"}
			reason := apiv2.PhysicalContainerReasonImageNotFound
			runtimeName := namespace.Name + "-runtime"
			existingID := ""
			if dependency != "image" {
				image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "ready-image", namespace.Name+"-image")
				config.ImageRef = image.Name
			}
			switch dependency {
			case "volume":
				config.VolumeMounts = []apiv2.VolumeMount{{Type: apiv2.NamedVolumeMount, VolumeRef: "missing", Target: "/data"}}
				reason = apiv2.PhysicalContainerReasonVolumeNotFound
			case "create":
				containerOrchestrator.FailNextCreateContainer(runtimeName, containers.ErrRuntimeNotHealthy)
				reason = apiv2.PhysicalContainerReasonCreateFailed
			case "partial-create":
				containerOrchestrator.FailNextCreateContainerAfterCreation(runtimeName, containers.ErrRuntimeNotHealthy)
				reason = apiv2.PhysicalContainerReasonCreateFailed
			case "replacement", "tracked":
				key := "previous"
				if dependency == "tracked" {
					key = "desired"
				}
				var runErr error
				existingID, runErr = containerOrchestrator.RunContainer(ctx, containers.RunContainerOptions{
					CreateContainerOptions: containers.CreateContainerOptions{
						Name: runtimeName, Image: namespace.Name + "-image",
						Labels: map[string]string{
							"com.microsoft.developer.usvc-dev.lifecycle-key": key,
							"com.microsoft.developer.usvc-dev.build":         "test",
						},
					},
				})
				require.NoError(t, runErr)
				removeRuntimeContainerOnCleanup(t, existingID)
				if dependency == "replacement" {
					containerOrchestrator.FailNextRemoveContainer(runtimeName, containers.ErrRuntimeNotHealthy)
					reason = apiv2.PhysicalContainerReasonExistingContainerReplacementFailed
				} else {
					owner := &apiv2.PhysicalContainer{
						ObjectMeta: metav1.ObjectMeta{Name: "runtime-owner", Namespace: namespace.Name},
						Spec:       apiv2.PhysicalContainerSpec{ContainerID: existingID},
					}
					require.NoError(t, client.Create(ctx, owner))
					waitPhysicalContainerPhase(t, ctx, owner.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
					reason = apiv2.PhysicalContainerReasonRuntimeContainerAlreadyTracked
				}
			}
			logical := &apiv2.LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
				Spec: apiv2.LogicalContainerSpec{
					Mode: apiv2.LogicalContainerModePersistent, ContainerName: runtimeName,
					LifecycleKey: "desired", Container: config,
				},
			}
			require.NoError(t, client.Create(ctx, logical))
			name := types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(logical.UID)}
			physical := waitObjectAssumesState(t, ctx, name, func(current *apiv2.PhysicalContainer) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return ready != nil && ready.Reason == string(reason), nil
			})
			createCount := containerOrchestrator.CreateContainerCallCount(runtimeName)
			require.NoError(t, client.Delete(ctx, logical))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, logical)
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
			require.Equal(t, createCount, containerOrchestrator.CreateContainerCallCount(runtimeName))
			if existingID != "" {
				_, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{existingID}})
				require.NoError(t, inspectErr)
			}
			if dependency == "partial-create" {
				waitContainerMissing(t, ctx, runtimeName)
			}
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

// Verifies that logical deletion immediately marks an in-flight physical backing for deletion,
// then removes the confirmed runtime container without continuing startup.
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
	physical := waitObjectAssumesState(t, ctx, name, func(current *apiv2.PhysicalContainer) (bool, error) {
		return !current.DeletionTimestamp.IsZero(), nil
	})
	require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
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
	created  chan *apiv2.PhysicalContainer
	observe  chan struct{}
	creating atomic.Bool
}

type logicalObservationGate struct {
	ctrl_client.Client
	observe                  <-chan struct{}
	imagesHidden             atomic.Bool
	containerHidden          atomic.Bool
	containerCreationBlocked atomic.Bool
	containerLookupBlocked   atomic.Bool
}

// Verifies owner-based discovery independently of child names, terminal duplicate detection,
// and deletion of every owned backing even when the logical controller has never recorded a UID.
func TestV2LogicalContainerDiscoversAndDeletesOwnedBackings(t *testing.T) {
	for _, testCase := range []struct {
		name                string
		images              bool
		count               int
		deleteBeforeObserve bool
		withContainer       bool
	}{
		{name: "container", count: 1},
		{name: "duplicate-containers", count: 2},
		{name: "image", images: true, count: 1},
		{name: "duplicate-images", images: true, count: 2},
		{name: "image-with-container", images: true, count: 1, withContainer: true},
		{name: "duplicate-images-with-container", images: true, count: 2, withContainer: true},
		{name: "unobserved-containers", count: 2, deleteBeforeObserve: true},
		{name: "unobserved-images", images: true, count: 2, deleteBeforeObserve: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			observe := make(chan struct{})
			var observeOnce sync.Once
			allowObservation := func() { observeOnce.Do(func() { close(observe) }) }
			defer allowObservation()
			var gate *logicalObservationGate
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
				LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
				t.Name(), t.TempDir(), TestEnvironmentOptions{
					DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
						gate = &logicalObservationGate{Client: client, observe: observe}
						return gate
					},
				})
			require.NoError(t, startErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "owner-discovery"}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			logical := &apiv2.LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name: "logical", Namespace: namespace.Name,
					Finalizers: []string{apiv2.GroupName + "/logicalcontainer-reconciler"},
				},
				Spec: apiv2.LogicalContainerSpec{
					Mode: apiv2.LogicalContainerModeExisting, ContainerName: "owner-discovery-runtime",
				},
			}
			runtimeID := ""
			if testCase.images {
				logical.Spec.Mode = apiv2.LogicalContainerModeSession
				logical.Spec.Container = &apiv2.LogicalContainerConfig{
					Image: &apiv2.PhysicalContainerImageConfig{Image: "owner-discovery-image"},
				}
			} else {
				var runErr error
				runtimeID, runErr = server.ContainerOrchestrator.RunContainer(ctx, containers.RunContainerOptions{
					CreateContainerOptions: containers.CreateContainerOptions{Name: logical.Spec.ContainerName, Image: "existing-image"},
				})
				require.NoError(t, runErr)
			}
			require.NoError(t, server.Client.Create(ctx, logical))
			owner := *metav1.NewControllerRef(logical, apiv2.GroupVersion.WithKind("LogicalContainer"))
			for i := 0; i < testCase.count; i++ {
				metadata := metav1.ObjectMeta{
					Name: fmt.Sprintf("owned-child-%d", i), Namespace: namespace.Name,
					OwnerReferences: []metav1.OwnerReference{owner},
				}
				if testCase.images {
					image := &apiv2.PhysicalContainerImage{
						ObjectMeta: metadata, Spec: apiv2.PhysicalContainerImageSpec{Image: logical.Spec.Container.Image.DeepCopy()},
					}
					controllers.InitializeLogicalBackingFinalizers(image, apiv2.GroupName+"/physicalcontainerimage-reconciler")
					require.NoError(t, server.Client.Create(ctx, image))
					waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
						return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
					})
				} else {
					physical := &apiv2.PhysicalContainer{
						ObjectMeta: metadata, Spec: apiv2.PhysicalContainerSpec{ContainerID: runtimeID},
					}
					controllers.InitializeLogicalBackingFinalizers(physical, apiv2.GroupName+"/physicalcontainer-reconciler")
					require.NoError(t, server.Client.Create(ctx, physical))
					waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
						ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
						return ready != nil && (ready.Reason == string(apiv2.PhysicalContainerReasonRuntimeContainerRunning) ||
							ready.Reason == string(apiv2.PhysicalContainerReasonRuntimeContainerAlreadyTracked)), nil
					})
				}
			}
			if testCase.withContainer {
				physical := &apiv2.PhysicalContainer{
					ObjectMeta: metav1.ObjectMeta{
						Name: "owned-container", Namespace: namespace.Name,
						OwnerReferences: []metav1.OwnerReference{owner},
					},
					Spec: apiv2.PhysicalContainerSpec{Container: &apiv2.PhysicalContainerConfig{
						ContainerName: logical.Spec.ContainerName, ImageRef: "owned-child-0",
					}},
				}
				controllers.InitializeLogicalBackingFinalizers(physical, apiv2.GroupName+"/physicalcontainer-reconciler")
				require.NoError(t, server.Client.Create(ctx, physical))
				waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerPhaseRunning, nil
				})
			}
			if testCase.deleteBeforeObserve {
				require.NoError(t, server.Client.Delete(ctx, logical))
				waitObjectAssumesStateEx(t, ctx, gate.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
					return !current.DeletionTimestamp.IsZero(), nil
				})
			}
			allowObservation()
			if !testCase.deleteBeforeObserve {
				expected := apiv2.LogicalContainerPhaseRunning
				if testCase.count > 1 {
					expected = apiv2.LogicalContainerPhaseFailed
				}
				observed := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
					return current.Status.Phase == expected, nil
				})
				if testCase.count > 1 {
					var reason apiv2.ConditionReason = "MultiplePhysicalContainers"
					if testCase.images {
						reason = "MultiplePhysicalImages"
					}
					requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, reason)
				} else if testCase.images {
					require.Equal(t, "owned-child-0", observed.Status.ImageRef)
				} else {
					require.Equal(t, "owned-child-0", observed.Status.PhysicalContainerRef)
					require.Equal(t, runtimeID, observed.Status.ContainerID)
				}
				var physicals apiv2.PhysicalContainerList
				var images apiv2.PhysicalContainerImageList
				require.NoError(t, server.Client.List(ctx, &physicals, ctrl_client.InNamespace(namespace.Name)))
				require.NoError(t, server.Client.List(ctx, &images, ctrl_client.InNamespace(namespace.Name)))
				if testCase.images {
					require.Len(t, images.Items, testCase.count)
					if testCase.count == 1 || testCase.withContainer {
						require.Len(t, physicals.Items, 1)
					} else {
						require.Empty(t, physicals.Items)
					}
				} else {
					require.Len(t, physicals.Items, testCase.count)
					require.Empty(t, images.Items)
				}
				require.NoError(t, server.Client.Delete(ctx, logical))
			}
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
			var remainingPhysicals apiv2.PhysicalContainerList
			var remainingImages apiv2.PhysicalContainerImageList
			require.NoError(t, server.Client.List(ctx, &remainingPhysicals, ctrl_client.InNamespace(namespace.Name)))
			require.NoError(t, server.Client.List(ctx, &remainingImages, ctrl_client.InNamespace(namespace.Name)))
			require.Empty(t, remainingPhysicals.Items)
			require.Empty(t, remainingImages.Items)
			if runtimeID != "" {
				inspected, inspectErr := server.ContainerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{runtimeID}})
				require.NoError(t, inspectErr)
				require.Len(t, inspected, 1)
				require.Equal(t, containers.ContainerStatusRunning, inspected[0].Status)
			}
		})
	}
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

func (client *logicalObservationGate) List(ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	listErr := client.Client.List(ctx, list, options...)
	if listErr != nil {
		return listErr
	}
	listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
	if listOptions.FieldSelector == nil {
		return nil
	}
	_, ownerQuery := listOptions.FieldSelector.RequiresExactMatch(".metadata.logicalContainerOwnerUID")
	if !ownerQuery {
		return nil
	}
	if _, isContainerList := list.(*apiv2.PhysicalContainerList); isContainerList && client.containerLookupBlocked.Load() {
		return fmt.Errorf("injected owned container lookup failure")
	}
	if images, isImageList := list.(*apiv2.PhysicalContainerImageList); isImageList && client.imagesHidden.Load() {
		images.Items = nil
	}
	if physicals, isContainerList := list.(*apiv2.PhysicalContainerList); isContainerList && client.containerHidden.CompareAndSwap(true, false) {
		physicals.Items = nil
	}
	return nil
}

func (client *logicalObservationGate) Create(ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	if _, isContainer := obj.(*apiv2.PhysicalContainer); isContainer && client.containerCreationBlocked.Load() {
		return fmt.Errorf("injected physical container creation failure")
	}
	return client.Client.Create(ctx, obj, options...)
}

// Verifies that Pending waits for its owned image even when the physical container already exists,
// including after AlreadyExists confirmation, and exposes startup logs while container declaration is retrying.
func TestV2LogicalContainerPendingAcquiresOwnedImage(t *testing.T) {
	for _, testCase := range []struct {
		name              string
		existingContainer bool
		confirmTemplate   bool
	}{
		{name: "container-with-unobserved-image", existingContainer: true},
		{name: "confirmed-template-with-unobserved-image", existingContainer: true, confirmTemplate: true},
		{name: "container-create-retry"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			observe := make(chan struct{})
			var observeOnce sync.Once
			allowObservation := func() { observeOnce.Do(func() { close(observe) }) }
			defer allowObservation()
			var gate *logicalObservationGate
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
				LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
				t.Name(), t.TempDir(), TestEnvironmentOptions{
					DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
						gate = &logicalObservationGate{Client: client, observe: observe}
						gate.imagesHidden.Store(testCase.existingContainer)
						gate.containerHidden.Store(testCase.confirmTemplate)
						gate.containerCreationBlocked.Store(!testCase.existingContainer)
						return gate
					},
				})
			require.NoError(t, startErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "creating-image"}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			logical := &apiv2.LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name: "logical", Namespace: namespace.Name,
					Finalizers: []string{apiv2.GroupName + "/logicalcontainer-reconciler"},
				},
				Spec: apiv2.LogicalContainerSpec{
					ContainerName: "creating-image-runtime",
					Container: &apiv2.LogicalContainerConfig{
						Image: &apiv2.PhysicalContainerImageConfig{Image: "creating-image-source"},
					},
				},
			}
			if testCase.confirmTemplate {
				logical.Spec.Mode = apiv2.LogicalContainerModePersistent
				logical.Spec.Stop = true
			}
			require.NoError(t, server.Client.Create(ctx, logical))
			if testCase.existingContainer {
				owner := *metav1.NewControllerRef(logical, apiv2.GroupVersion.WithKind("LogicalContainer"))
				image := &apiv2.PhysicalContainerImage{
					ObjectMeta: metav1.ObjectMeta{
						Name: "owned-image", Namespace: namespace.Name, OwnerReferences: []metav1.OwnerReference{owner},
					},
					Spec: apiv2.PhysicalContainerImageSpec{Image: logical.Spec.Container.Image.DeepCopy()},
				}
				controllers.InitializeLogicalBackingFinalizers(image, apiv2.GroupName+"/physicalcontainerimage-reconciler")
				require.NoError(t, server.Client.Create(ctx, image))
				waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
				})
				physical := &apiv2.PhysicalContainer{
					ObjectMeta: metav1.ObjectMeta{
						Name: "owned-container", Namespace: namespace.Name, OwnerReferences: []metav1.OwnerReference{owner},
					},
					Spec: apiv2.PhysicalContainerSpec{Container: &apiv2.PhysicalContainerConfig{
						ContainerName: logical.Spec.ContainerName, ImageRef: image.Name,
					}},
				}
				if testCase.confirmTemplate {
					physical.Name = "logical-" + string(logical.UID)
				}
				controllers.InitializeLogicalBackingFinalizers(physical, apiv2.GroupName+"/physicalcontainer-reconciler")
				require.NoError(t, server.Client.Create(ctx, physical))
				waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerPhaseRunning, nil
				})
			}
			allowObservation()
			expectedPhase := apiv2.LogicalContainerPhaseUnknown
			var expectedReason apiv2.ConditionReason = "PhysicalResourcePreparationFailed"
			if testCase.existingContainer {
				expectedPhase, expectedReason = apiv2.LogicalContainerPhasePending, "PhysicalResourcePreparationPending"
			}
			waiting := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return current.Status.Phase == apiv2.LogicalContainerPhaseRunning ||
					(current.Status.Phase == expectedPhase && ready != nil && ready.Reason == string(expectedReason)), nil
			})
			require.Equal(t, expectedPhase, waiting.Status.Phase)
			requireReadyCondition(t, waiting.Status.Conditions, metav1.ConditionFalse, expectedReason)
			if testCase.existingContainer {
				require.Nil(t, waiting.Status.StartupLogs)
			} else {
				require.NotNil(t, waiting.Status.StartupLogs)
			}
			gate.imagesHidden.Store(false)
			gate.containerCreationBlocked.Store(false)
			settledPhase := apiv2.LogicalContainerPhaseRunning
			if testCase.confirmTemplate {
				settledPhase = apiv2.LogicalContainerPhaseExited
			}
			settled := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return current.Status.Phase == settledPhase, nil
			})
			require.NotNil(t, settled.Status.StartupLogs)
			var physicals apiv2.PhysicalContainerList
			var images apiv2.PhysicalContainerImageList
			require.NoError(t, server.Client.List(ctx, &physicals, ctrl_client.InNamespace(namespace.Name)))
			require.NoError(t, server.Client.List(ctx, &images, ctrl_client.InNamespace(namespace.Name)))
			require.Len(t, physicals.Items, 1)
			require.Len(t, images.Items, 1)
			require.Equal(t, images.Items[0].Name, settled.Status.ImageRef)
			require.Equal(t, physicals.Items[0].Name, settled.Status.PhysicalContainerRef)
			require.NoError(t, server.Client.Delete(ctx, logical))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
		})
	}
}

// Verifies that transient backing lookup failures preserve logical backing identity during
// running, stopping, and deletion, and that reconciliation resumes when observation recovers.
func TestV2LogicalContainerRetriesBackingLookup(t *testing.T) {
	for _, action := range []string{"running", "stopping", "deleting"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			observe := make(chan struct{})
			close(observe)
			var gate *logicalObservationGate
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
				LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
				t.Name(), t.TempDir(), TestEnvironmentOptions{
					DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
						gate = &logicalObservationGate{Client: client, observe: observe}
						return gate
					},
				})
			require.NoError(t, startErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lookup-retry"}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			logical := &apiv2.LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
				Spec: apiv2.LogicalContainerSpec{
					ContainerName: "lookup-retry-runtime",
					Container: &apiv2.LogicalContainerConfig{
						Image: &apiv2.PhysicalContainerImageConfig{Image: "lookup-retry-image"},
					},
				},
			}
			require.NoError(t, server.Client.Create(ctx, logical))
			running := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
			})
			gate.containerLookupBlocked.Store(true)
			expectedPhase := apiv2.LogicalContainerPhaseUnknown
			var expectedReason apiv2.ConditionReason = "PhysicalContainerLookupFailed"
			if action == "deleting" {
				expectedReason = "PhysicalResourceDeletionFailed"
				require.NoError(t, server.Client.Delete(ctx, logical))
			} else {
				require.NoError(t, retryOnConflictEx(ctx, server.Client, logical.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
					current.Annotations = map[string]string{"test.dcp.microsoft.com/lookup-retry": action}
					if action == "stopping" {
						current.Spec.Stop = true
					}
					return server.Client.Update(updateCtx, current)
				}))
				if action == "stopping" {
					expectedPhase = apiv2.LogicalContainerPhaseStopping
				}
			}
			waiting := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return current.Status.Phase == expectedPhase && ready != nil && ready.Reason == string(expectedReason), nil
			})
			requireReadyCondition(t, waiting.Status.Conditions, metav1.ConditionFalse, expectedReason)
			require.Equal(t, running.Status.ContainerID, waiting.Status.ContainerID)
			require.Equal(t, running.Status.PhysicalContainerRef, waiting.Status.PhysicalContainerRef)
			gate.containerLookupBlocked.Store(false)
			if action != "deleting" {
				settledPhase := apiv2.LogicalContainerPhaseRunning
				if action == "stopping" {
					settledPhase = apiv2.LogicalContainerPhaseExited
				}
				settled := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
					return current.Status.Phase == settledPhase, nil
				})
				require.Equal(t, running.Status.ContainerID, settled.Status.ContainerID)
				require.Equal(t, running.Status.PhysicalContainerRef, settled.Status.PhysicalContainerRef)
				require.NoError(t, server.Client.Delete(ctx, logical))
			}
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
			orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			require.Equal(t, 1, orchestrator.CreateContainerCallCount(logical.Spec.ContainerName))
		})
	}
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
	if _, physical := obj.(*apiv2.PhysicalContainer); physical && client.creating.Load() {
		select {
		case <-client.observe:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return client.Client.Get(ctx, key, obj, options...)
}

func (client *backingCreateResponseLoss) Create(ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	_, physicalCreate := obj.(*apiv2.PhysicalContainer)
	if physicalCreate {
		client.creating.Store(true)
	}
	createErr := client.Client.Create(ctx, obj, options...)
	if createErr != nil {
		if physicalCreate {
			client.creating.Store(false)
		}
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
		block = current.Status.Phase == apiv2.PhysicalContainerPhaseRunning && writer.client.containerBlocked.Load() ||
			current.Spec.Stop && writer.client.stopStatusBlocked.Load()
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

// Verifies that physical cleanup retains its final outcome through status conflicts,
// and logical deletion observes that outcome before settling ownership and releasing its hold.
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
	conflicts.imageBlocked.Store(false)
	select {
	case <-conflicts.containerConflict:
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
	require.NoError(t, server.Client.Delete(ctx, logical))
	require.NoError(t, server.Client.Get(ctx, name, physical))
	require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
	require.Contains(t, physical.Finalizers, apiv2.GroupName+"/physicalcontainer-reconciler")
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
