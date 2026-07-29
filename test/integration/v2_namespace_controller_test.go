/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestV2NamespaceControllerSetsActivePhase(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "v2-ns-active",
		},
	}
	require.NoError(t, client.Create(ctx, namespace))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultIntegrationTestTimeout)
		defer cleanupCancel()
		_ = client.Delete(cleanupCtx, namespace)
	})

	updatedNamespace := waitV2NamespaceActive(t, ctx, namespace.Name)
	require.Equal(t, apiv2.NamespacePhaseActive, updatedNamespace.Status.Phase)
	require.NotEmpty(t, updatedNamespace.Finalizers)
}

func TestV2NamespaceControllerCleansUpPhysicalContainers(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "v2-ns-cleanup",
		},
	}
	require.NoError(t, client.Create(ctx, namespace))
	waitV2NamespaceActive(t, ctx, namespace.Name)
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "cleanup-image", "cleanup-image")

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cleanup-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: "v2-ns-cleanup-container",
		},
	}
	require.NoError(t, client.Create(ctx, container))
	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	require.NotEmpty(t, updatedContainer.Status.ContainerID)
	containerID := updatedContainer.Status.ContainerID

	require.NoError(t, client.Delete(ctx, namespace))

	waitObjectAssumesState(t, ctx, types.NamespacedName{Name: namespace.Name}, func(currentNamespace *apiv2.Namespace) (bool, error) {
		return currentNamespace.Status.Phase == apiv2.NamespacePhaseTerminating, nil
	})
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainer](t, ctx, client, container)
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerImage](t, ctx, client, image)
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
	waitContainerMissing(t, ctx, containerID)
}

func TestV2NamespaceLifecycleAdmissionRejectsChildCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	requireV2ChildCreateRejected(t, ctx, newTestPhysicalContainerImage("missing-image", "missing-v2-namespace"), "namespace does not exist")
	requireV2ChildCreateRejected(t, ctx, newTestPhysicalContainer("missing-container", "missing-v2-namespace"), "namespace does not exist")

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "v2-ns-admission",
		},
	}
	require.NoError(t, client.Create(ctx, namespace))
	waitV2NamespaceActive(t, ctx, namespace.Name)

	blockingContainer := newTestPhysicalContainer("blocking-container", namespace.Name)
	blockingContainer.Finalizers = []string{"test.dcp.microsoft.com/hold"}
	require.NoError(t, client.Create(ctx, blockingContainer))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultIntegrationTestTimeout)
		defer cleanupCancel()
		_ = retryOnConflict[apiv2.PhysicalContainer](cleanupCtx, blockingContainer.NamespacedName(), func(ctx context.Context, currentContainer *apiv2.PhysicalContainer) error {
			currentContainer.Finalizers = nil
			return client.Update(ctx, currentContainer)
		})
	})

	require.NoError(t, client.Delete(ctx, namespace))
	waitObjectAssumesState(t, ctx, types.NamespacedName{Name: namespace.Name}, func(currentNamespace *apiv2.Namespace) (bool, error) {
		return currentNamespace.DeletionTimestamp != nil && !currentNamespace.DeletionTimestamp.IsZero(), nil
	})

	requireV2ChildCreateRejected(t, ctx, newTestPhysicalContainerImage("terminating-image", namespace.Name), "namespace is terminating")
	requireV2ChildCreateRejected(t, ctx, newTestPhysicalContainer("terminating-container", namespace.Name), "namespace is terminating")
}

func requireV2ChildCreateRejected(t *testing.T, ctx context.Context, obj ctrl_client.Object, expectedMessage string) {
	t.Helper()

	createErr := client.Create(ctx, obj)
	require.Error(t, createErr)
	require.Contains(t, createErr.Error(), expectedMessage)
}

func newTestPhysicalContainerImage(name string, namespace string) *apiv2.PhysicalContainerImage {
	return &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image: "test-image",
		},
	}
}

func newTestPhysicalContainer(name string, namespace string) *apiv2.PhysicalContainer {
	return &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef: "test-image",
		},
	}
}

func waitV2NamespaceActive(t *testing.T, ctx context.Context, name string) *apiv2.Namespace {
	t.Helper()

	return waitObjectAssumesState(t, ctx, types.NamespacedName{Name: name}, func(namespace *apiv2.Namespace) (bool, error) {
		return namespace.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
}

func waitPhysicalContainerPhase(
	t *testing.T,
	ctx context.Context,
	name types.NamespacedName,
	phase apiv2.PhysicalContainerPhase,
) *apiv2.PhysicalContainer {
	t.Helper()

	return waitObjectAssumesState(t, ctx, name, func(container *apiv2.PhysicalContainer) (bool, error) {
		return container.Status.Phase == phase, nil
	})
}

func createReadyV2PhysicalContainerImage(
	t *testing.T,
	ctx context.Context,
	namespace string,
	name string,
	imageRef string,
) *apiv2.PhysicalContainerImage {
	t.Helper()

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image: imageRef,
		},
	}
	require.NoError(t, client.Create(ctx, image))

	return waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
}

func waitPhysicalContainerImagePhase(
	t *testing.T,
	ctx context.Context,
	name types.NamespacedName,
	phase apiv2.PhysicalContainerImagePhase,
) *apiv2.PhysicalContainerImage {
	t.Helper()

	return waitObjectAssumesState(t, ctx, name, func(image *apiv2.PhysicalContainerImage) (bool, error) {
		return image.Status.Phase == phase, nil
	})
}

func waitContainerMissing(t *testing.T, ctx context.Context, containerID string) {
	t.Helper()

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
			Containers: []string{containerID},
		})
		if inspectErr == nil {
			return false, nil
		}
		if errors.Is(inspectErr, containers.ErrNotFound) {
			return true, nil
		}
		return false, inspectErr
	})
	require.NoError(t, err)
}
