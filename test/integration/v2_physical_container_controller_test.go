/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestV2PhysicalContainerControllerCreatesContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-create")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "created-image", "created-image")
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "created-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: "v2-pctr-created-container",
			Command:       []string{"run"},
			Env: []commonapi.EnvVar{
				{Name: "TEST_ENV", Value: "test-value"},
			},
		},
	}
	require.NoError(t, client.Create(ctx, container))

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	require.NotEmpty(t, updatedContainer.Finalizers)
	require.NotEmpty(t, updatedContainer.Status.ContainerID)
	require.Equal(t, "v2-pctr-created-container", updatedContainer.Status.ContainerName)
	require.Equal(t, "created-image", updatedContainer.Status.Image)
	requireReadyCondition(t, updatedContainer.Status.Conditions, metav1.ConditionTrue, "RuntimeContainerRunning")

	inspectedContainers, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedContainer.Status.ContainerID},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedContainers, 1)
	require.Equal(t, "created-image", inspectedContainers[0].Image)
	require.Equal(t, []string{"run"}, inspectedContainers[0].Args)
	require.Equal(t, "test-value", inspectedContainers[0].Env["TEST_ENV"])
	require.Equal(t, "false", inspectedContainers[0].Labels[controllers.PersistentLabel])
	require.NotEmpty(t, inspectedContainers[0].Labels[controllers.CreatorProcessIdLabel])
	require.NotEmpty(t, inspectedContainers[0].Labels[controllers.CreatorProcessStartTimeLabel])
}

func TestV2PhysicalContainerControllerReconcilesWhenReferencedImageBecomesReady(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-image-watch")
	imageName := "watched-image"
	containerName := "v2-pctr-watched-image-container"
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "watched-image-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      imageName,
			ContainerName: containerName,
		},
	}
	require.NoError(t, client.Create(ctx, container))

	pendingContainer := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(container *apiv2.PhysicalContainer) (bool, error) {
		return container.Status.Phase == apiv2.PhysicalContainerPhasePending && container.Status.ContainerID == "", nil
	})
	requireReadyCondition(t, pendingContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonImageNotFound)
	require.Equal(t, 0, containerOrchestrator.CreateContainerCallCount(containerName))

	sourceImage := "watched-source-image"
	releasePull := containerOrchestrator.BlockPullImage(sourceImage)
	defer releasePull()
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      imageName,
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image: sourceImage,
		},
	}
	require.NoError(t, client.Create(ctx, image))

	pendingContainer = waitObjectAssumesState(t, ctx, container.NamespacedName(), func(container *apiv2.PhysicalContainer) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(container.Status.Conditions, string(apiv2.ConditionReady))
		return container.Status.Phase == apiv2.PhysicalContainerPhasePending &&
			readyCondition != nil &&
			apiv2.ConditionReason(readyCondition.Reason) == apiv2.PhysicalContainerReasonImageNotReady, nil
	})
	requireReadyCondition(t, pendingContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonImageNotReady)
	releasePull()

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	removeRuntimeContainerOnCleanup(t, updatedContainer.Status.ContainerID)
	require.Equal(t, sourceImage, updatedContainer.Status.Image)
	require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(containerName))
}

func TestV2PhysicalContainerControllerCreatesContainerWithNetworks(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-networks")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "networked-image", "networked-image")
	networkName := "v2-pctr-networked-runtime"
	_, networkErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: networkName})
	require.NoError(t, networkErr)
	removeRuntimeNetworkOnCleanup(t, networkName)

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "networked-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: "v2-pctr-networked-container",
			Networks: []apiv2.ContainerNetworkConnectionConfig{
				{
					Name:    networkName,
					Aliases: []string{"api", "service"},
				},
			},
		},
	}
	require.NoError(t, client.Create(ctx, container))

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	removeRuntimeContainerOnCleanup(t, updatedContainer.Status.ContainerID)

	inspectedContainers, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedContainer.Status.ContainerID},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedContainers, 1)
	require.Len(t, inspectedContainers[0].Networks, 1)
	require.Equal(t, networkName, inspectedContainers[0].Networks[0].Name)
	require.ElementsMatch(t, []string{"api", "service"}, inspectedContainers[0].Networks[0].Aliases)
}

func TestV2PhysicalContainerControllerReportsPortMappings(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-ports")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "ported-image", "ported-image")
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ported-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: "v2-pctr-ported-container",
			Ports: []apiv2.ContainerPort{
				{
					ContainerPort: 8080,
				},
				{
					ContainerPort: 9090,
					Protocol:      commonapi.PortProtocolUDP,
					HostIP:        "127.0.0.2",
					HostPort:      19090,
				},
				{
					ContainerPort: 9100,
					RangeSize:     3,
					HostIP:        "127.0.0.3",
					HostPort:      19100,
				},
			},
		},
	}
	require.NoError(t, client.Create(ctx, container))

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	removeRuntimeContainerOnCleanup(t, updatedContainer.Status.ContainerID)
	require.Len(t, updatedContainer.Status.PortMappings, 5)

	firstMapping := updatedContainer.Status.PortMappings[0]
	require.Equal(t, int32(8080), firstMapping.ContainerPort)
	require.Equal(t, commonapi.PortProtocolTCP, firstMapping.Protocol)
	require.NotEmpty(t, firstMapping.HostIP)
	require.GreaterOrEqual(t, firstMapping.HostPort, int32(ctrl_testutil.MinRandomHostPort))
	require.LessOrEqual(t, firstMapping.HostPort, int32(ctrl_testutil.MaxRandomHostPort))

	secondMapping := updatedContainer.Status.PortMappings[1]
	require.Equal(t, int32(9090), secondMapping.ContainerPort)
	require.Equal(t, commonapi.PortProtocolUDP, secondMapping.Protocol)
	require.Equal(t, "127.0.0.2", secondMapping.HostIP)
	require.Equal(t, int32(19090), secondMapping.HostPort)

	thirdMapping := updatedContainer.Status.PortMappings[2]
	require.Equal(t, int32(9100), thirdMapping.ContainerPort)
	require.Equal(t, commonapi.PortProtocolTCP, thirdMapping.Protocol)
	require.Equal(t, "127.0.0.3", thirdMapping.HostIP)
	require.Equal(t, int32(19100), thirdMapping.HostPort)

	fourthMapping := updatedContainer.Status.PortMappings[3]
	require.Equal(t, int32(9101), fourthMapping.ContainerPort)
	require.Equal(t, commonapi.PortProtocolTCP, fourthMapping.Protocol)
	require.Equal(t, "127.0.0.3", fourthMapping.HostIP)
	require.Equal(t, int32(19101), fourthMapping.HostPort)

	fifthMapping := updatedContainer.Status.PortMappings[4]
	require.Equal(t, int32(9102), fifthMapping.ContainerPort)
	require.Equal(t, commonapi.PortProtocolTCP, fifthMapping.Protocol)
	require.Equal(t, "127.0.0.3", fifthMapping.HostIP)
	require.Equal(t, int32(19102), fifthMapping.HostPort)
}

func TestV2PhysicalContainerControllerCopiesCreateFilesBeforeStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-files")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "files-image", "files-image")
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "files-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: "v2-pctr-files-container",
			CreateFiles: []apiv2.CreateFileSystem{
				{
					Destination: "/workspace",
					Entries: []apiv2.FileSystemEntry{
						{
							Name:     "hello.txt",
							Contents: "hello",
						},
					},
				},
			},
		},
	}
	require.NoError(t, client.Create(ctx, container))

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	removeRuntimeContainerOnCleanup(t, updatedContainer.Status.ContainerID)

	files, getFileErr := containerOrchestrator.GetCreatedFiles(updatedContainer.Status.ContainerID)
	require.NoError(t, getFileErr)
	require.Len(t, files, 1)
	require.Equal(t, "/workspace", files[0].Destination)
	items, itemsErr := files[0].GetTarItems()
	require.NoError(t, itemsErr)
	require.Len(t, items, 1)
}

func TestV2PhysicalContainerControllerCreatesStoppedContainerWithoutStarting(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-stop-on-create")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "stop-on-create-image", "stop-on-create-image")
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stop-on-create-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: "v2-pctr-stop-on-create-container",
			Stop:          true,
		},
	}
	require.NoError(t, client.Create(ctx, container))

	updatedContainer := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(container *apiv2.PhysicalContainer) (bool, error) {
		return container.Status.ContainerID != "" &&
			container.Status.Phase == apiv2.PhysicalContainerPhasePending &&
			container.Status.RuntimeStatus == string(containers.ContainerStatusCreated), nil
	})
	removeRuntimeContainerOnCleanup(t, updatedContainer.Status.ContainerID)
	require.True(t, updatedContainer.Status.StartedAt.IsZero())
	requireReadyCondition(t, updatedContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerCreated)
}

func TestV2PhysicalContainerControllerDoesNotDuplicateCreateWhileStatusPending(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-create-gate")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "gated-image", "gated-image")
	containerName := "v2-pctr-create-gated-container"
	releaseCreate := containerOrchestrator.BlockCreateContainer(containerName)
	defer releaseCreate()

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gated-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: containerName,
		},
	}
	require.NoError(t, client.Create(ctx, container))
	waitCreateContainerCallCount(t, ctx, containerName, 1)

	require.NoError(t, retryOnConflict[apiv2.PhysicalContainer](ctx, container.NamespacedName(), func(ctx context.Context, currentContainer *apiv2.PhysicalContainer) error {
		if currentContainer.Annotations == nil {
			currentContainer.Annotations = map[string]string{}
		}
		currentContainer.Annotations["test.dcp.microsoft.com/reconcile"] = "again"
		return client.Update(ctx, currentContainer)
	}))

	releaseCreate()
	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	removeRuntimeContainerOnCleanup(t, updatedContainer.Status.ContainerID)
	require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(containerName))
}

func TestV2PhysicalContainerControllerRetriesCreateAfterFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-create-retry")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "retry-image", "retry-image")
	containerName := "v2-pctr-create-retry-container"
	containerOrchestrator.FailNextCreateContainerAfterCreation(containerName, errors.New("create failed once"))
	containerOrchestrator.FailNextRemoveContainer(containerName, errors.New("cleanup failed once"))

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "retry-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: containerName,
		},
	}
	require.NoError(t, client.Create(ctx, container))
	waitCreateContainerCallCount(t, ctx, containerName, 1)
	retryPendingContainer := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.PhysicalContainerPhasePending &&
			readyCondition != nil &&
			apiv2.ConditionReason(readyCondition.Reason) == apiv2.PhysicalContainerReasonCreateFailed, nil
	})
	requireReadyCondition(t, retryPendingContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonCreateFailed)

	cleanupPendingContainer := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.PhysicalContainerPhasePending &&
			readyCondition != nil &&
			apiv2.ConditionReason(readyCondition.Reason) == apiv2.PhysicalContainerReasonPartialContainerCleanupFailed, nil
	})
	requireReadyCondition(t, cleanupPendingContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonPartialContainerCleanupFailed)
	waitCreateContainerCallCount(t, ctx, containerName, 2)

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	removeRuntimeContainerOnCleanup(t, updatedContainer.Status.ContainerID)
	require.Equal(t, 2, containerOrchestrator.CreateContainerCallCount(containerName))
}

func TestV2PhysicalContainerControllerCleansUpPartialContainerAfterTerminalCreateFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-partial-create-failure")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "partial-create-failure-image", "partial-create-failure-image")
	containerName := "v2-pctr-partial-create-failure-container"
	containerOrchestrator.FailNextCreateContainerAfterCreation(
		containerName,
		errors.Join(containers.ErrCouldNotAllocate, errors.New("failed after creating runtime container")),
	)

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "partial-create-failure-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: containerName,
		},
	}
	require.NoError(t, client.Create(ctx, container))

	failedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseFailed)
	requireReadyCondition(t, failedContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonCreateFailed)
	require.Empty(t, failedContainer.Status.ContainerID)
	waitContainerMissing(t, ctx, containerName)
	require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(containerName))
	require.Never(t, func() bool {
		return containerOrchestrator.CreateContainerCallCount(containerName) > 1
	}, 3*time.Second, 250*time.Millisecond)
}

func TestV2PhysicalContainerControllerRetriesPartialContainerCleanupAfterFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-partial-cleanup-retry")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "partial-cleanup-retry-image", "partial-cleanup-retry-image")
	containerName := "v2-pctr-partial-cleanup-retry-container"
	containerOrchestrator.FailNextCreateContainerAfterCreation(
		containerName,
		errors.Join(containers.ErrCouldNotAllocate, errors.New("failed after creating runtime container")),
	)
	containerOrchestrator.FailNextRemoveContainer(containerName, errors.New("remove failed once"))

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "partial-cleanup-retry-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: containerName,
		},
	}
	require.NoError(t, client.Create(ctx, container))

	failedCleanupContainer := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return readyCondition != nil &&
			strings.Contains(readyCondition.Message, "Failed to remove partially created runtime container: remove failed once"), nil
	})
	require.NotEmpty(t, failedCleanupContainer.Status.ContainerID)
	require.Equal(t, apiv2.PhysicalContainerPhaseFailed, failedCleanupContainer.Status.Phase)
	requireReadyCondition(t, failedCleanupContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonPartialContainerCleanupFailed)
	require.Equal(t, 1, containerOrchestrator.RemoveContainerCallCount(containerName))

	waitContainerMissing(t, ctx, containerName)
	failedContainer := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return readyCondition != nil &&
			strings.Contains(readyCondition.Message, "Failed to create physical container:") &&
			current.Status.ContainerID == "", nil
	})
	requireReadyCondition(t, failedContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonCreateFailed)
	require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(containerName))
	require.Equal(t, 2, containerOrchestrator.RemoveContainerCallCount(containerName))
}

func TestV2PhysicalContainerControllerReappliesPostCreateFailureStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-post-create-failure")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "post-create-failure-image", "post-create-failure-image")
	containerName := "v2-pctr-post-create-failure-container"
	containerOrchestrator.FailMatchingContainers(ctx, containerName, 1, "start failed")

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "post-create-failure-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: containerName,
		},
	}
	require.NoError(t, client.Create(ctx, container))

	failedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseFailed)
	containerID := failedContainer.Status.ContainerID
	require.NotEmpty(t, containerID)
	removeRuntimeContainerOnCleanup(t, containerID)
	requireReadyCondition(t, failedContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonStartFailed)

	require.NoError(t, retryOnConflict[apiv2.PhysicalContainer](ctx, container.NamespacedName(), func(ctx context.Context, currentContainer *apiv2.PhysicalContainer) error {
		currentContainer.Status.Phase = apiv2.PhysicalContainerPhasePending
		currentContainer.Status.Conditions = nil
		return client.Status().Update(ctx, currentContainer)
	}))

	reappliedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseFailed)
	require.Equal(t, containerID, reappliedContainer.Status.ContainerID)
	requireReadyCondition(t, reappliedContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonStartFailed)
}

func TestV2PhysicalContainerControllerTracksExistingContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-existing")
	existingContainerID := runExistingTestContainer(t, ctx, "v2-pctr-existing-runtime", "existing-image")

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ContainerID: existingContainerID,
		},
	}
	require.NoError(t, client.Create(ctx, container))

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	require.Equal(t, existingContainerID, updatedContainer.Status.ContainerID)
	require.Equal(t, "v2-pctr-existing-runtime", updatedContainer.Status.ContainerName)

	inspectedContainers, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{existingContainerID},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedContainers, 1)
	require.NotContains(t, inspectedContainers[0].Labels, controllers.CreatorProcessIdLabel)
}

func TestV2PhysicalContainerControllerReportsMissingExistingContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-missing")
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ContainerID: "missing-v2-container-id",
		},
	}
	require.NoError(t, client.Create(ctx, container))

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseUnknown)
	require.Equal(t, "missing-v2-container-id", updatedContainer.Status.ContainerID)
	requireReadyCondition(t, updatedContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerMissing)
}

func TestV2PhysicalContainerControllerReportsRuntimePhases(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-runtime-phases")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "runtime-phases-image", "runtime-phases-image")
	containerName := "v2-pctr-runtime-phases-container"
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-phases-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: containerName,
		},
	}
	require.NoError(t, client.Create(ctx, container))

	runningContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	removeRuntimeContainerOnCleanup(t, runningContainer.Status.ContainerID)

	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, runningContainer.Status.ContainerID, containers.ContainerStatusPaused))
	pausedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhasePaused)
	requireReadyCondition(t, pausedContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerPaused)

	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, runningContainer.Status.ContainerID, containers.ContainerStatusRestarting))
	restartingContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhasePending)
	requireReadyCondition(t, restartingContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerRestarting)

	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, runningContainer.Status.ContainerID, containers.ContainerStatusRunning))
	runningContainer = waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	requireReadyCondition(t, runningContainer.Status.Conditions, metav1.ConditionTrue, apiv2.PhysicalContainerReasonRuntimeContainerRunning)

	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, runningContainer.Status.ContainerID, containers.ContainerStatusRemoving))
	removingContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhasePending)
	requireReadyCondition(t, removingContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerRemoving)

	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, runningContainer.Status.ContainerID, containers.ContainerStatus("unrecognized")))
	unknownContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseUnknown)
	requireReadyCondition(t, unknownContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerStatusUnknown)

	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, runningContainer.Status.ContainerID, containers.ContainerStatusDead))
	deadContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseExited)
	requireReadyCondition(t, deadContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerDead)
}

func TestV2PhysicalContainerControllerDeletesCreatedContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-delete")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "deleted-created-image", "deleted-created-image")
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "deleted-created-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: "v2-pctr-delete-created",
		},
	}
	require.NoError(t, client.Create(ctx, container))

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	containerID := updatedContainer.Status.ContainerID
	require.NotEmpty(t, containerID)

	require.NoError(t, client.Delete(ctx, updatedContainer))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainer](t, ctx, client, container)
	waitContainerMissing(t, ctx, containerID)
}

func TestV2PhysicalContainerControllerPreservesCreatedContainerOnDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-retain")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "retained-created-image", "retained-created-image")
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "retained-created-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:               image.Name,
			ContainerName:          "v2-pctr-retain-created",
			RetainRuntimeContainer: true,
		},
	}
	require.NoError(t, client.Create(ctx, container))

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	containerID := updatedContainer.Status.ContainerID
	require.NotEmpty(t, containerID)
	removeRuntimeContainerOnCleanup(t, containerID)

	require.NoError(t, client.Delete(ctx, updatedContainer))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainer](t, ctx, client, container)

	inspectedContainers, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{containerID},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedContainers, 1)
	require.Equal(t, "true", inspectedContainers[0].Labels[controllers.PersistentLabel])
}

func TestV2PhysicalContainerControllerPreservesExistingContainerOnDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-preserve")
	existingContainerID := runExistingTestContainer(t, ctx, "v2-pctr-preserve-runtime", "preserved-image")

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "preserved-existing-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ContainerID: existingContainerID,
		},
	}
	require.NoError(t, client.Create(ctx, container))
	waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)

	require.NoError(t, client.Delete(ctx, container))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainer](t, ctx, client, container)

	inspectedContainers, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{existingContainerID},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedContainers, 1)
}

func TestV2PhysicalContainerControllerRejectsExistingContainerWithoutReplacement(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-existing-conflict")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "conflict-image", "conflict-image")
	containerName := "v2-pctr-existing-conflict-runtime"
	existingContainerID := runExistingTestContainer(t, ctx, containerName, "existing-image")

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflicting-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: containerName,
		},
	}
	require.NoError(t, client.Create(ctx, container))

	failedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseFailed)
	requireReadyCondition(t, failedContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonCreateFailed)
	require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(containerName))
	require.Never(t, func() bool {
		return containerOrchestrator.CreateContainerCallCount(containerName) > 1
	}, 3*time.Second, 250*time.Millisecond)

	inspectedContainers, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{existingContainerID},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedContainers, 1)
}

func TestV2PhysicalContainerControllerReplacesExistingContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-replace-existing")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "replacement-image", "replacement-image")
	containerName := "v2-pctr-replace-existing-runtime"
	existingContainerID := runExistingTestContainer(t, ctx, containerName, "replaced-image")
	containerOrchestrator.FailNextRemoveContainer(containerName, errors.New("replace failed once"))

	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "replacement-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:        image.Name,
			ContainerName:   containerName,
			ReplaceExisting: true,
		},
	}
	require.NoError(t, client.Create(ctx, container))
	retryPendingContainer := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.PhysicalContainerPhasePending &&
			readyCondition != nil &&
			apiv2.ConditionReason(readyCondition.Reason) == apiv2.PhysicalContainerReasonExistingContainerReplacementFailed, nil
	})
	requireReadyCondition(t, retryPendingContainer.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonExistingContainerReplacementFailed)

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	require.NotEqual(t, existingContainerID, updatedContainer.Status.ContainerID)
	removeRuntimeContainerOnCleanup(t, updatedContainer.Status.ContainerID)
	waitContainerMissing(t, ctx, existingContainerID)

	require.NoError(t, client.Delete(ctx, updatedContainer))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainer](t, ctx, client, container)
	waitContainerMissing(t, ctx, updatedContainer.Status.ContainerID)
}

func TestV2PhysicalContainerControllerStopsContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-stop")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "stopped-image", "stopped-image")
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stopped-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: "v2-pctr-stopped-container",
		},
	}
	require.NoError(t, client.Create(ctx, container))

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	containerID := updatedContainer.Status.ContainerID
	require.NotEmpty(t, containerID)
	removeRuntimeContainerOnCleanup(t, containerID)

	updateErr := retryOnConflict[apiv2.PhysicalContainer](ctx, container.NamespacedName(), func(ctx context.Context, currentContainer *apiv2.PhysicalContainer) error {
		currentContainer.Spec.Stop = true
		return client.Update(ctx, currentContainer)
	})
	require.NoError(t, updateErr)

	updatedContainer = waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseExited)
	require.Equal(t, containerID, updatedContainer.Status.ContainerID)
	require.Equal(t, string(containers.ContainerStatusExited), updatedContainer.Status.RuntimeStatus)
	requireReadyCondition(t, updatedContainer.Status.Conditions, metav1.ConditionFalse, "RuntimeContainerExited")
}

func TestV2PhysicalContainerControllerTracksRuntimeContainerEvents(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pctr-events")
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "event-image", "event-image")
	container := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "event-container",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{
			ImageRef:      image.Name,
			ContainerName: "v2-pctr-event-container",
		},
	}
	require.NoError(t, client.Create(ctx, container))

	updatedContainer := waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseRunning)
	containerID := updatedContainer.Status.ContainerID
	require.NotEmpty(t, containerID)
	removeRuntimeContainerOnCleanup(t, containerID)

	_, stopErr := containerOrchestrator.StopContainers(ctx, containers.StopContainersOptions{
		Containers: []string{containerID},
	})
	require.NoError(t, stopErr)

	updatedContainer = waitPhysicalContainerPhase(t, ctx, container.NamespacedName(), apiv2.PhysicalContainerPhaseExited)
	require.Equal(t, containerID, updatedContainer.Status.ContainerID)
	require.Equal(t, string(containers.ContainerStatusExited), updatedContainer.Status.RuntimeStatus)
}

func waitCreateContainerCallCount(t *testing.T, ctx context.Context, name string, expected int) {
	t.Helper()

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		return containerOrchestrator.CreateContainerCallCount(name) >= expected, nil
	})
	require.NoError(t, waitErr)
}

func createActiveV2Namespace(t *testing.T, ctx context.Context, name string) *apiv2.Namespace {
	t.Helper()

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	require.NoError(t, client.Create(ctx, namespace))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultIntegrationTestTimeout)
		defer cleanupCancel()
		_ = client.Delete(cleanupCtx, namespace)
	})

	return waitV2NamespaceActive(t, ctx, name)
}

func runExistingTestContainer(t *testing.T, ctx context.Context, name string, image string) string {
	t.Helper()

	containerID, runErr := containerOrchestrator.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name:  name,
			Image: image,
		},
	})
	require.NoError(t, runErr)
	removeRuntimeContainerOnCleanup(t, containerID)

	return containerID
}

func removeRuntimeContainerOnCleanup(t *testing.T, containerID string) {
	t.Helper()

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultIntegrationTestTimeout)
		defer cleanupCancel()
		_, removeErr := containerOrchestrator.RemoveContainersWithoutEvents(cleanupCtx, containers.RemoveContainersOptions{
			Containers: []string{containerID},
			Force:      true,
		})
		if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
			require.NoError(t, removeErr)
		}
	})
}

func removeRuntimeNetworkOnCleanup(t *testing.T, networkName string) {
	t.Helper()

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultIntegrationTestTimeout)
		defer cleanupCancel()
		_, removeErr := containerOrchestrator.RemoveNetworks(cleanupCtx, containers.RemoveNetworksOptions{
			Networks: []string{networkName},
			Force:    true,
		})
		if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
			require.NoError(t, removeErr)
		}
	})
}
