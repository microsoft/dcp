/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
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

	images := make([]*apiv2.PhysicalContainerImage, 2)
	physicalContainers := make([]*apiv2.PhysicalContainer, 2)
	containerIDs := make([]string, 2)
	for i := range images {
		imageName := fmt.Sprintf("cleanup-image-%d", i)
		images[i] = createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, imageName, imageName)
		physicalContainers[i] = &apiv2.PhysicalContainer{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("cleanup-container-%d", i),
				Namespace: namespace.Name,
			},
			Spec: apiv2.PhysicalContainerSpec{Container: &apiv2.PhysicalContainerConfig{ImageRef: images[i].Name,
				ContainerName: fmt.Sprintf("v2-ns-cleanup-container-%d", i)},
			},
		}
		require.NoError(t, client.Create(ctx, physicalContainers[i]))
		updatedContainer := waitPhysicalContainerPhase(t, ctx, physicalContainers[i].NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
		require.NotEmpty(t, updatedContainer.Status.ContainerID)
		containerIDs[i] = updatedContainer.Status.ContainerID
	}

	require.NoError(t, client.Delete(ctx, namespace))

	for i := range physicalContainers {
		ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainer](t, ctx, client, physicalContainers[i])
		ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerImage](t, ctx, client, images[i])
		waitContainerMissing(t, ctx, containerIDs[i])
	}
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
}

func TestV2NamespaceControllerCleansUpContainersWhileProcessDeletionIsBlocked(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "v2-ns-independent-cleanup",
		},
	}
	require.NoError(t, client.Create(ctx, namespace))
	waitV2NamespaceActive(t, ctx, namespace.Name)

	executablePath := "v2-ns-independent-cleanup-command"
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{executablePath}}
	finishExecution := make(chan struct{})
	testProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria,
		RunCommand: func(*internal_testutil.ProcessExecution) int32 {
			<-finishExecution
			return 0
		},
		StopError: func(*internal_testutil.ProcessExecution) error {
			return errors.New("simulated stop failure")
		},
	})
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			testProcessExecutor.RemoveAutoExecution(criteria)
			close(finishExecution)
		})
	}
	defer cleanup()

	physicalProcess := createRunningPhysicalProcess(t, ctx, namespace.Name, "blocked-process", executablePath)
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "independent-image", "independent-image")
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "independent-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			Container: &apiv2.PhysicalContainerConfig{
				ImageRef:      image.Name,
				ContainerName: "v2-ns-independent-cleanup-container",
			},
		},
	}
	require.NoError(t, client.Create(ctx, container))
	runningContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)

	require.NoError(t, client.Delete(ctx, namespace))
	waitObjectAssumesState(t, ctx, physicalProcess.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return readyCondition != nil &&
			apiv2.ConditionReason(readyCondition.Reason) == apiv2.PhysicalProcessReasonStopFailed, nil
	})

	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainer](t, ctx, client, container)
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerImage](t, ctx, client, image)
	waitContainerMissing(t, ctx, runningContainer.Status.ContainerID)

	currentProcess := &apiv2.PhysicalProcess{}
	require.NoError(t, client.Get(ctx, physicalProcess.NamespacedName(), currentProcess))

	cleanup()
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
}

func TestV2NamespaceRejectsResourceCreationAfterDeletionStarts(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespaceName := "v2-ns-create-gate"
	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespaceName},
	}
	require.NoError(t, client.Create(ctx, namespace))
	waitV2NamespaceActive(t, ctx, namespace.Name)

	pid := int64(2147483647)
	require.NoError(t, client.Delete(ctx, namespace))

	blockedProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "blocked-process", Namespace: namespaceName},
		Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
	}
	createErr := client.Create(ctx, blockedProcess)
	require.Error(t, createErr)
	require.True(t, apierrors.IsForbidden(createErr), "expected forbidden error, got %v", createErr)

	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)

	stillBlockedProcess := blockedProcess.DeepCopy()
	stillBlockedProcess.Name = "still-blocked-process"
	createAfterDeletionErr := client.Create(ctx, stillBlockedProcess)
	require.Error(t, createAfterDeletionErr)
	require.True(t, apierrors.IsForbidden(createAfterDeletionErr), "expected forbidden error, got %v", createAfterDeletionErr)

	replacementNamespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespaceName},
	}
	require.NoError(t, client.Create(ctx, replacementNamespace))
	waitV2NamespaceActive(t, ctx, replacementNamespace.Name)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultIntegrationTestTimeout)
		defer cleanupCancel()
		_ = client.Delete(cleanupCtx, replacementNamespace)
	})

	allowedProcess := blockedProcess.DeepCopy()
	allowedProcess.Name = "allowed-process"
	require.NoError(t, client.Create(ctx, allowedProcess))
}

func TestV2NamespaceImmediateDeletionCleansPrecreatedResources(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespaceName := "v2-ns-immediate-delete"
	pid := int64(2147483647)
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "precreated-process", Namespace: namespaceName},
		Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespaceName},
	}
	require.NoError(t, client.Create(ctx, namespace))
	require.Contains(t, namespace.Finalizers, apiv2.NamespaceFinalizer)
	require.NoError(t, client.Delete(ctx, namespace))

	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalProcess](t, ctx, client, physicalProcess)
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
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
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: imageRef}},
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

func TestV2NamespaceControllerReportsPendingCleanupResources(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "v2-ns-stalled-cleanup",
		},
	}
	require.NoError(t, client.Create(ctx, namespace))
	waitV2NamespaceActive(t, ctx, namespace.Name)
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "stalled-image", "stalled-image")

	// Blocking the create keeps the container controller from releasing its finalizer,
	// which stalls namespace cleanup on the PhysicalContainer.
	containerName := "v2-ns-stalled-container"
	releaseCreate := containerOrchestrator.BlockCreateContainer(containerName)
	defer releaseCreate()

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stalled-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{Container: &apiv2.PhysicalContainerConfig{ImageRef: image.Name,
			ContainerName: containerName},
		},
	}
	require.NoError(t, client.Create(ctx, container))
	waitCreateContainerCallCount(t, ctx, containerName, 1)

	require.NoError(t, client.Delete(ctx, namespace))

	stalledNamespace := waitObjectAssumesState(t, ctx, types.NamespacedName{Name: namespace.Name}, func(currentNamespace *apiv2.Namespace) (bool, error) {
		condition := apimeta.FindStatusCondition(currentNamespace.Status.Conditions, "CleanupComplete")
		return condition != nil && strings.Contains(condition.Message, "physicalcontainers"), nil
	})
	cleanupCondition := apimeta.FindStatusCondition(stalledNamespace.Status.Conditions, "CleanupComplete")
	require.Equal(t, metav1.ConditionFalse, cleanupCondition.Status)
	require.Equal(t, "CleanupInProgress", cleanupCondition.Reason)
	require.Equal(t, "Namespace cleanup is waiting for 1 physicalcontainers to be deleted.", cleanupCondition.Message)

	releaseCreate()
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
}
