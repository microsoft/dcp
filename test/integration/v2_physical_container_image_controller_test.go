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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestV2PhysicalContainerImageControllerPullsSourceImage(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-pull")
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pulled-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image: "v2-pci-pulled-source",
		},
	}
	require.NoError(t, client.Create(ctx, image))

	updatedImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, "v2-pci-pulled-source", updatedImage.Status.Image)
	require.NotEmpty(t, updatedImage.Status.ImageID)
	requireReadyCondition(t, updatedImage.Status.Conditions, metav1.ConditionTrue, "ImageReady")
	require.True(t, containerOrchestrator.HasImage(updatedImage.Status.Image))
}

func TestV2PhysicalContainerImageControllerDoesNotDuplicatePullWhileStatusPending(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-pull-gate")
	sourceImage := "v2-pci-gated-source"
	releasePull := containerOrchestrator.BlockPullImage(sourceImage)
	defer releasePull()

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gated-pulled-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image:      sourceImage,
			PullPolicy: apiv2.PullPolicyAlways,
		},
	}
	require.NoError(t, client.Create(ctx, image))
	waitPullImageCallCount(t, ctx, sourceImage, 1)

	require.NoError(t, retryOnConflict[apiv2.PhysicalContainerImage](ctx, image.NamespacedName(), func(ctx context.Context, currentImage *apiv2.PhysicalContainerImage) error {
		if currentImage.Annotations == nil {
			currentImage.Annotations = map[string]string{}
		}
		currentImage.Annotations["test.dcp.microsoft.com/reconcile"] = "again"
		return client.Update(ctx, currentImage)
	}))

	releasePull()
	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(sourceImage))
}

func TestV2PhysicalContainerImageControllerRetriesPullAfterFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-pull-retry")
	sourceImage := "v2-pci-retried-source"
	containerOrchestrator.FailNextPullImage(sourceImage, errors.New("pull failed once"))

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "retried-pulled-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image:      sourceImage,
			PullPolicy: apiv2.PullPolicyAlways,
		},
	}
	require.NoError(t, client.Create(ctx, image))
	waitPullImageCallCount(t, ctx, sourceImage, 1)
	waitPullImageCallCount(t, ctx, sourceImage, 2)

	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, 2, containerOrchestrator.PullImageCallCount(sourceImage))
}

func TestV2PhysicalContainerImageControllerBuildsImage(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-build")
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "built-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image: "v2-pci-built-target-image",
			Build: &apiv2.ContainerBuildContext{
				Context: "test-context",
				Tags:    []string{"v2-pci-built-image"},
				Args: []commonapi.EnvVar{
					{Name: "TEST_ARG", Value: "test-value"},
				},
				Labels: []commonapi.Label{
					{Key: "test-label", Value: "test-value"},
				},
			},
		},
	}
	require.NoError(t, client.Create(ctx, image))

	updatedImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, "v2-pci-built-target-image", updatedImage.Status.Image)
	require.NotEmpty(t, updatedImage.Status.ImageID)
	requireReadyCondition(t, updatedImage.Status.Conditions, metav1.ConditionTrue, "ImageReady")

	inspectedImages, inspectErr := containerOrchestrator.InspectImages(ctx, containers.InspectImagesOptions{
		Images: []string{updatedImage.Status.Image},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedImages, 1)
	require.Equal(t, "test-value", inspectedImages[0].Labels["test-label"])
	require.Contains(t, inspectedImages[0].Tags, "v2-pci-built-image")
	require.Contains(t, inspectedImages[0].Tags, "v2-pci-built-target-image")
}

func TestV2PhysicalContainerImageControllerDoesNotDuplicateBuildWhileStatusPending(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-build-gate")
	targetImage := "v2-pci-gated-build-target"
	releaseBuild := containerOrchestrator.BlockBuildImage(targetImage)
	defer releaseBuild()

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gated-built-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image: targetImage,
			Build: &apiv2.ContainerBuildContext{
				Context: "test-context",
			},
		},
	}
	require.NoError(t, client.Create(ctx, image))
	waitBuildImageCallCount(t, ctx, targetImage, 1)

	require.NoError(t, retryOnConflict[apiv2.PhysicalContainerImage](ctx, image.NamespacedName(), func(ctx context.Context, currentImage *apiv2.PhysicalContainerImage) error {
		if currentImage.Annotations == nil {
			currentImage.Annotations = map[string]string{}
		}
		currentImage.Annotations["test.dcp.microsoft.com/reconcile"] = "again"
		return client.Update(ctx, currentImage)
	}))

	releaseBuild()
	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))
}

func waitPullImageCallCount(t *testing.T, ctx context.Context, image string, expected int) {
	t.Helper()

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		return containerOrchestrator.PullImageCallCount(image) >= expected, nil
	})
	require.NoError(t, waitErr)
}

func waitBuildImageCallCount(t *testing.T, ctx context.Context, tag string, expected int) {
	t.Helper()

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		return containerOrchestrator.BuildImageCallCount(tag) >= expected, nil
	})
	require.NoError(t, waitErr)
}

func TestV2PhysicalContainerImageControllerHonorsDisabledPullRetries(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-no-retry")
	sourceImage := "v2-pci-no-retry-source"
	containerOrchestrator.FailNextPullImage(sourceImage, errors.New("pull failed once"))

	noRetries := int32(0)
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-retry-pulled-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image:          sourceImage,
			PullPolicy:     apiv2.PullPolicyAlways,
			PullRetryLimit: &noRetries,
		},
	}
	require.NoError(t, client.Create(ctx, image))

	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseFailed)

	// The single attempt must be the only one: retries are disabled and a recorded
	// pull failure is terminal, so the controller must not re-enter the pull path.
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(sourceImage))
	require.Never(t, func() bool {
		return containerOrchestrator.PullImageCallCount(sourceImage) > 1
	}, 3*time.Second, 250*time.Millisecond)
}

func TestV2PhysicalContainerImageControllerCancelsPullOnDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-delete-pull")
	sourceImage := "v2-pci-deleted-source"
	releasePull := containerOrchestrator.BlockPullImage(sourceImage)
	defer releasePull()

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "deleted-pulling-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image:      sourceImage,
			PullPolicy: apiv2.PullPolicyAlways,
		},
	}
	require.NoError(t, client.Create(ctx, image))
	waitPullImageCallCount(t, ctx, sourceImage, 1)

	pullingImage := waitObjectAssumesState(t, ctx, image.NamespacedName(), func(currentImage *apiv2.PhysicalContainerImage) (bool, error) {
		return len(currentImage.Finalizers) > 0, nil
	})
	require.Contains(t, pullingImage.Finalizers, apiv2.GroupName+"/physicalcontainerimage-reconciler")

	require.NoError(t, client.Delete(ctx, image))

	// The finalizer must be released without waiting for the blocked pull to complete.
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerImage](t, ctx, client, image)

	// Releasing the block proves the pull was cancelled rather than merely orphaned: a still-running
	// pull would resume here and register the image with the runtime.
	releasePull()
	require.Never(t, func() bool {
		return containerOrchestrator.HasImage(sourceImage)
	}, 2*time.Second, 250*time.Millisecond)
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(sourceImage))
}
