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
	"k8s.io/apimachinery/pkg/util/wait"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
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
			PullPolicy: commonapi.PullPolicyAlways,
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
			PullPolicy: commonapi.PullPolicyAlways,
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
			Build: &commonapi.ContainerBuildContext{
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
			Build: &commonapi.ContainerBuildContext{
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
