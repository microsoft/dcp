/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

type recordingBuildImageOrchestrator struct {
	containers.ContainerOrchestrator
	buildOptions chan containers.BuildImageOptions
}

func (r *recordingBuildImageOrchestrator) BuildImage(ctx context.Context, options containers.BuildImageOptions) error {
	select {
	case r.buildOptions <- options:
	default:
	}
	return r.ContainerOrchestrator.BuildImage(ctx, options)
}

func TestV2PhysicalContainerImageControllerBuildsRawArchiveContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)

	var recordingOrchestrator *recordingBuildImageOrchestrator
	serverInfo, _, startErr := StartTestEnvironmentWithOptions(
		ctx,
		NamespaceController|PhysicalContainerImageController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			DecorateContainerOrchestrator: func(
				orchestrator containers.ContainerOrchestrator,
				_ *statestore.Store,
			) containers.ContainerOrchestrator {
				recordingOrchestrator = &recordingBuildImageOrchestrator{
					ContainerOrchestrator: orchestrator,
					buildOptions:          make(chan containers.BuildImageOptions, 1),
				}
				return recordingOrchestrator
			},
		},
	)
	require.NoError(t, startErr)
	defer shutdownTestEnvironment(serverInfo, cancel)

	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-pci-raw-archive"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(currentNamespace *apiv2.Namespace) (bool, error) {
		return currentNamespace.Status.Phase == apiv2.NamespacePhaseActive, nil
	})

	targetImage := "v2-pci-raw-archive-target"
	rawContents := base64.StdEncoding.EncodeToString(make([]byte, 1024))
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "raw-archive-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
			Image:      targetImage,
			PullPolicy: apiv2.PullPolicyAlways,
			Build: &apiv2.ContainerBuildContext{
				Digest: "empty-tar-v1",
				ContextArchive: &apiv2.ContainerBuildContextArchive{
					RawContents: rawContents,
				},
			},
		}},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, image))

	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, image.NamespacedName(), func(currentImage *apiv2.PhysicalContainerImage) (bool, error) {
		return currentImage.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
	})

	buildOptions := <-recordingOrchestrator.buildOptions
	require.NotNil(t, buildOptions.ContextArchive)
	require.Equal(t, "empty-tar-v1", buildOptions.Digest)
	require.Equal(t, rawContents, buildOptions.ContextArchive.RawContents)
	require.False(t, buildOptions.Pull)
}

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
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: "v2-pci-pulled-source"}},
	}
	require.NoError(t, client.Create(ctx, image))

	updatedImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, "v2-pci-pulled-source", updatedImage.Status.Image)
	require.NotEmpty(t, updatedImage.Status.ImageID)
	requireReadyCondition(t, updatedImage.Status.Conditions, metav1.ConditionTrue, apiv2.PhysicalContainerImageReasonImageAvailable)
	require.True(t, containerOrchestrator.HasImage(updatedImage.Status.Image))
}

func TestV2PhysicalContainerImageControllerUsesLocalBestEffortSourceAfterPullFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	sourceImage := "v2-pci-best-effort-local-source"
	imageID, pullErr := containerOrchestrator.PullImage(ctx, containers.PullImageOptions{Image: sourceImage})
	require.NoError(t, pullErr)
	containerOrchestrator.FailNextPullImage(sourceImage, errors.New("pull unavailable"))
	noRetries := int32(0)

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-best-effort-source")
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "best-effort-source-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
			Image:          sourceImage,
			PullPolicy:     apiv2.PullPolicyBestEffort,
			PullRetryLimit: &noRetries,
		}},
	}
	require.NoError(t, client.Create(ctx, image))

	updatedImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, imageID, updatedImage.Status.ImageID)
	require.Equal(t, 2, containerOrchestrator.PullImageCallCount(sourceImage))
}

func TestV2PhysicalContainerImageControllerPreservesRemovedRuntimeImageIdentity(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-preserve-removed")
	sourceImage := "v2-pci-preserve-removed-source"
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "preserve-removed-image", sourceImage)
	originalImageID := image.Status.ImageID
	originalDigest := image.Status.Digest
	originalTags := append([]string{}, image.Status.Tags...)
	restoreMissing := containerOrchestrator.FailInspectImage(originalImageID, containers.ErrNotFound)
	defer restoreMissing()

	require.NoError(t, retryOnConflict[apiv2.PhysicalContainerImage](ctx, image.NamespacedName(), func(ctx context.Context, currentImage *apiv2.PhysicalContainerImage) error {
		if currentImage.Annotations == nil {
			currentImage.Annotations = map[string]string{}
		}
		currentImage.Annotations["test.dcp.microsoft.com/reconcile"] = "runtime-image-removed"
		return client.Update(ctx, currentImage)
	}))

	unavailableImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseUnknown)
	require.Equal(t, originalImageID, unavailableImage.Status.ImageID)
	require.Equal(t, originalDigest, unavailableImage.Status.Digest)
	require.Equal(t, originalTags, unavailableImage.Status.Tags)
	requireReadyCondition(t, unavailableImage.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonLocalImageNotFound)
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(sourceImage))

	restoreMissing()
	readyImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, originalImageID, readyImage.Status.ImageID)
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(sourceImage))
}

func TestV2PhysicalContainerImageControllerRepairsStatusFromAuthoritativeData(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-repair-status")
	sourceImage := "v2-pci-repair-status-source"
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "repair-status-image", sourceImage)
	expectedImageID := image.Status.ImageID
	expectedDigest := image.Status.Digest
	expectedTags := append([]string{}, image.Status.Tags...)

	require.NoError(t, retryOnConflict[apiv2.PhysicalContainerImage](ctx, image.NamespacedName(), func(ctx context.Context, currentImage *apiv2.PhysicalContainerImage) error {
		currentImage.Status.ImageID = "stale-image-id"
		currentImage.Status.Digest = "stale-digest"
		currentImage.Status.Tags = []string{"stale-tag"}
		currentImage.Status.Phase = apiv2.PhysicalContainerImagePhasePending
		currentImage.Status.Conditions = nil
		return client.Status().Update(ctx, currentImage)
	}))

	repairedImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, expectedImageID, repairedImage.Status.ImageID)
	require.Equal(t, expectedDigest, repairedImage.Status.Digest)
	require.Equal(t, expectedTags, repairedImage.Status.Tags)
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(sourceImage))
}

func TestV2PhysicalContainerImageControllerRetriesMissingPullResultWithoutRepulling(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-pull-inspect-missing")
	sourceImage := "v2-pci-pull-inspect-missing-source"
	restoreInspection := containerOrchestrator.FailInspectImage(sourceImage, containers.ErrNotFound)
	defer restoreInspection()
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pull-inspect-missing-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image: &apiv2.PhysicalContainerImageConfig{
				Image:      sourceImage,
				PullPolicy: apiv2.PullPolicyAlways,
			},
		},
	}
	require.NoError(t, client.Create(ctx, image))

	unknownImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseUnknown)
	require.Empty(t, unknownImage.Status.ImageID)
	requireReadyCondition(t, unknownImage.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonLocalImageNotFound)
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(sourceImage))
	require.Never(t, func() bool {
		return containerOrchestrator.PullImageCallCount(sourceImage) > 1
	}, 2*time.Second, 250*time.Millisecond)

	restoreInspection()
	readyImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.NotEmpty(t, readyImage.Status.ImageID)
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(sourceImage))
}

func TestV2PhysicalContainerImageControllerTracksExistingImage(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-existing")
	imageID, pullErr := containerOrchestrator.PullImage(ctx, containers.PullImageOptions{Image: "v2-pci-existing-source"})
	require.NoError(t, pullErr)
	require.NotEmpty(t, imageID)

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{ImageID: imageID},
	}
	require.NoError(t, client.Create(ctx, image))

	updatedImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, imageID, updatedImage.Status.Image)
	require.Equal(t, imageID, updatedImage.Status.ImageID)
	requireReadyCondition(t, updatedImage.Status.Conditions, metav1.ConditionTrue, apiv2.PhysicalContainerImageReasonImageAvailable)
}

func TestV2PhysicalContainerImageControllerReportsRemovedExistingImage(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-removed-existing")
	imageID, pullErr := containerOrchestrator.PullImage(ctx, containers.PullImageOptions{Image: "v2-pci-removed-existing-source"})
	require.NoError(t, pullErr)
	require.NotEmpty(t, imageID)

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "removed-existing-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{ImageID: imageID},
	}
	require.NoError(t, client.Create(ctx, image))
	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)

	restoreMissing := containerOrchestrator.FailInspectImage(imageID, containers.ErrNotFound)
	defer restoreMissing()
	require.NoError(t, retryOnConflict[apiv2.PhysicalContainerImage](ctx, image.NamespacedName(), func(ctx context.Context, currentImage *apiv2.PhysicalContainerImage) error {
		if currentImage.Annotations == nil {
			currentImage.Annotations = map[string]string{}
		}
		currentImage.Annotations["test.dcp.microsoft.com/reconcile"] = "runtime-image-removed"
		return client.Update(ctx, currentImage)
	}))

	unavailableImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseUnknown)
	require.Equal(t, imageID, unavailableImage.Status.ImageID)
	requireReadyCondition(t, unavailableImage.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonLocalImageNotFound)

	restoreMissing()
	readyImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, imageID, readyImage.Status.ImageID)
}

func TestV2PhysicalContainerImageControllerReportsMissingExistingImage(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-missing-existing")
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-existing-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{ImageID: "missing-image-id"},
	}
	require.NoError(t, client.Create(ctx, image))

	updatedImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseFailed)
	requireReadyCondition(t, updatedImage.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonLocalImageNotFound)
}

func TestV2PhysicalContainerImageControllerReportsUnknownWhenInspectionFails(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-inspect-failure")
	sourceImage := "v2-pci-inspect-failure-source"
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "inspect-failure-image", sourceImage)
	restoreInspection := containerOrchestrator.FailInspectImage(sourceImage, errors.New("inspect failed"))
	defer restoreInspection()

	require.NoError(t, retryOnConflict[apiv2.PhysicalContainerImage](ctx, image.NamespacedName(), func(ctx context.Context, currentImage *apiv2.PhysicalContainerImage) error {
		if currentImage.Annotations == nil {
			currentImage.Annotations = map[string]string{}
		}
		currentImage.Annotations["test.dcp.microsoft.com/reconcile"] = "inspect"
		return client.Update(ctx, currentImage)
	}))

	unknownImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseUnknown)
	requireReadyCondition(t, unknownImage.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonRuntimeImageInspectFailed)
	restoreInspection()
	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
}

func TestV2PhysicalContainerImageControllerBlocksWithoutNamespace(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespaceName := "v2-pci-wait-namespace"
	sourceImage := "v2-pci-wait-namespace-source"
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "waiting-image",
			Namespace: namespaceName,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: sourceImage}},
	}
	require.NoError(t, client.Create(ctx, image))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultIntegrationTestTimeout)
		defer cleanupCancel()
		_ = client.Delete(cleanupCtx, image)
	})

	pendingImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhasePending)
	requireReadyCondition(t, pendingImage.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalResourceReasonNamespaceNotFound)
	require.Equal(t, 0, containerOrchestrator.PullImageCallCount(sourceImage))

	createActiveV2Namespace(t, ctx, namespaceName)
	readyImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, sourceImage, readyImage.Status.Image)
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(sourceImage))
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
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: sourceImage, PullPolicy: apiv2.PullPolicyAlways}},
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
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: sourceImage, PullPolicy: apiv2.PullPolicyAlways}},
	}
	require.NoError(t, client.Create(ctx, image))
	waitPullImageCallCount(t, ctx, sourceImage, 1)
	waitPullImageCallCount(t, ctx, sourceImage, 2)

	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, 2, containerOrchestrator.PullImageCallCount(sourceImage))
}

func TestV2PhysicalContainerImageControllerReportsMissingPullImageID(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-pull-missing-id")
	sourceImage := "v2-pci-pull-missing-id-source"
	restoreImageID := containerOrchestrator.OmitPullImageID(sourceImage)
	defer restoreImageID()

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-pull-image-id",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: sourceImage, PullPolicy: apiv2.PullPolicyAlways}},
	}
	require.NoError(t, client.Create(ctx, image))

	pendingImage := waitObjectAssumesState(t, ctx, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.PhysicalContainerImagePhasePending &&
			readyCondition != nil &&
			apiv2.ConditionReason(readyCondition.Reason) == apiv2.PhysicalContainerImageReasonPullResultMissingImageID, nil
	})
	requireReadyCondition(t, pendingImage.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonPullResultMissingImageID)

	restoreImageID()
	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
}

func TestV2PhysicalContainerImageControllerFailsWhenLocalImageIsMissing(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-local-image-missing")
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-local-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: "v2-pci-missing-local-source", PullPolicy: apiv2.PullPolicyNever}},
	}
	require.NoError(t, client.Create(ctx, image))

	failedImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseFailed)
	requireReadyCondition(t, failedImage.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonLocalImageNotFound)
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
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: "v2-pci-built-target-image", Build: &apiv2.ContainerBuildContext{
			Context: "test-context",
			Tags:    []string{"v2-pci-built-image"},
			Args: []commonapi.EnvVar{
				{Name: "TEST_ARG", Value: "test-value"},
			},
			Labels: []commonapi.Label{
				{Key: "test-label", Value: "test-value"},
				{Key: "com.microsoft.developer.usvc-dev.uid", Value: "caller-value"},
				{Key: controllers.PersistentLabel, Value: "caller-value"},
				{Key: controllers.CreatorProcessIdLabel, Value: "caller-value"},
				{Key: controllers.CreatorProcessStartTimeLabel, Value: "caller-value"},
			},
		}},
		},
	}
	require.NoError(t, client.Create(ctx, image))

	updatedImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, "v2-pci-built-target-image", updatedImage.Status.Image)
	require.NotEmpty(t, updatedImage.Status.ImageID)
	requireReadyCondition(t, updatedImage.Status.Conditions, metav1.ConditionTrue, apiv2.PhysicalContainerImageReasonImageAvailable)

	inspectedImages, inspectErr := containerOrchestrator.InspectImages(ctx, containers.InspectImagesOptions{
		Images: []string{updatedImage.Status.Image},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedImages, 1)
	require.Equal(t, "test-value", inspectedImages[0].Labels["test-label"])
	require.Equal(t, string(updatedImage.UID), inspectedImages[0].Labels["com.microsoft.developer.usvc-dev.uid"])
	require.Equal(t, "true", inspectedImages[0].Labels[controllers.PersistentLabel])
	require.NotEmpty(t, inspectedImages[0].Labels[controllers.CreatorProcessIdLabel])
	require.NotEqual(t, "caller-value", inspectedImages[0].Labels[controllers.CreatorProcessIdLabel])
	require.NotEmpty(t, inspectedImages[0].Labels[controllers.CreatorProcessStartTimeLabel])
	require.NotEqual(t, "caller-value", inspectedImages[0].Labels[controllers.CreatorProcessStartTimeLabel])
	require.Contains(t, inspectedImages[0].Tags, "v2-pci-built-image")
	require.Contains(t, inspectedImages[0].Tags, "v2-pci-built-target-image")
}

func TestV2PhysicalContainerImageControllerReusesBuildOutputWhenInputsMatch(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	targetImage := "v2-pci-existing-build-target"
	namespace := createActiveV2Namespace(t, ctx, "v2-pci-existing-build")
	createImage := func(name, digest, rawContents, labelValue string) *apiv2.PhysicalContainerImage {
		image := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace.Name,
			},
			Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
				Image:      targetImage,
				PullPolicy: apiv2.PullPolicyMissing,
				Build: &apiv2.ContainerBuildContext{
					Digest: digest,
					ContextArchive: &apiv2.ContainerBuildContextArchive{
						RawContents: rawContents,
					},
					Labels: []commonapi.Label{{Key: "material-label", Value: labelValue}},
				},
			}},
		}
		require.NoError(t, client.Create(ctx, image))
		return waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	}

	firstImage := createImage("first-build-image", "context-v1", "dGVzdA==", "label-v1")
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))

	secondImage := createImage("second-build-image", "context-v1", "ZGlmZmVyZW50", "label-v1")
	require.Equal(t, firstImage.Status.ImageID, secondImage.Status.ImageID)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))

	thirdImage := createImage("third-build-image", "context-v2", "ZGlmZmVyZW50", "label-v1")
	require.NotEqual(t, secondImage.Status.ImageID, thirdImage.Status.ImageID)
	require.Equal(t, 2, containerOrchestrator.BuildImageCallCount(targetImage))

	fourthImage := createImage("fourth-build-image", "context-v2", "ZGlmZmVyZW50", "label-v2")
	require.NotEqual(t, thirdImage.Status.ImageID, fourthImage.Status.ImageID)
	require.Equal(t, 3, containerOrchestrator.BuildImageCallCount(targetImage))
}

func TestV2PhysicalContainerImageControllerRebuildsWhenInheritedBuildInputsChange(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const (
		buildArgumentName = "DCP_TEST_V2_PHYSICAL_IMAGE_BUILD_ARGUMENT"
		buildSecretName   = "DCP_TEST_V2_PHYSICAL_IMAGE_BUILD_SECRET"
	)
	t.Setenv(buildArgumentName, "argument-one")
	t.Setenv(buildSecretName, "secret-one")

	targetImage := "v2-pci-inherited-build-argument-target"
	namespace := createActiveV2Namespace(t, ctx, "v2-pci-inherited-build-argument")
	createImage := func(name string) *apiv2.PhysicalContainerImage {
		image := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace.Name,
			},
			Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
				Image: targetImage,
				Build: &apiv2.ContainerBuildContext{
					Digest:         "context-v1",
					ContextArchive: &apiv2.ContainerBuildContextArchive{RawContents: "dGVzdA=="},
					Args:           []commonapi.EnvVar{{Name: buildArgumentName}},
					Secrets: []apiv2.ContainerBuildSecret{{
						Type: apiv2.EnvSecret,
						ID:   buildSecretName,
					}},
				},
			}},
		}
		require.NoError(t, client.Create(ctx, image))
		return waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	}

	firstImage := createImage("first-inherited-build-argument")
	secondImage := createImage("second-inherited-build-argument")
	require.Equal(t, firstImage.Status.ImageID, secondImage.Status.ImageID)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))

	t.Setenv(buildArgumentName, "argument-two")
	thirdImage := createImage("third-inherited-build-argument")
	require.NotEqual(t, secondImage.Status.ImageID, thirdImage.Status.ImageID)
	require.Equal(t, 2, containerOrchestrator.BuildImageCallCount(targetImage))

	t.Setenv(buildArgumentName, "argument-one")
	t.Setenv(buildSecretName, "secret-two")
	fourthImage := createImage("fourth-inherited-build-argument")
	require.NotEqual(t, thirdImage.Status.ImageID, fourthImage.Status.ImageID)
	require.Equal(t, 3, containerOrchestrator.BuildImageCallCount(targetImage))
}

func TestV2PhysicalContainerImageControllerReusesExplicitBuildInputsAcrossAmbientChanges(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const (
		buildArgumentName = "DCP_TEST_V2_PHYSICAL_IMAGE_EXPLICIT_BUILD_ARGUMENT"
		buildSecretName   = "DCP_TEST_V2_PHYSICAL_IMAGE_EXPLICIT_BUILD_SECRET"
	)
	t.Setenv(buildArgumentName, "ambient-argument-one")
	t.Setenv(buildSecretName, "ambient-secret-one")

	targetImage := "v2-pci-explicit-build-input-target"
	namespace := createActiveV2Namespace(t, ctx, "v2-pci-explicit-build-input")
	createImage := func(name string) *apiv2.PhysicalContainerImage {
		image := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace.Name,
			},
			Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
				Image: targetImage,
				Build: &apiv2.ContainerBuildContext{
					Digest:         "context-v1",
					ContextArchive: &apiv2.ContainerBuildContextArchive{RawContents: "dGVzdA=="},
					Args: []commonapi.EnvVar{{
						Name:  buildArgumentName,
						Value: "explicit-argument",
					}},
					Secrets: []apiv2.ContainerBuildSecret{{
						Type:   apiv2.EnvSecret,
						ID:     "secret",
						Source: buildSecretName,
						Value:  "explicit-secret",
					}},
				},
			}},
		}
		require.NoError(t, client.Create(ctx, image))
		return waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	}

	firstImage := createImage("first-explicit-build-input")
	t.Setenv(buildArgumentName, "ambient-argument-two")
	t.Setenv(buildSecretName, "ambient-secret-two")
	secondImage := createImage("second-explicit-build-input")
	require.Equal(t, firstImage.Status.ImageID, secondImage.Status.ImageID)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))
}

func TestV2PhysicalContainerImageControllerRebuildsWhenFileSecretChanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	secretPath := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, usvc_io.WriteFile(secretPath, []byte("secret-one"), osutil.PermissionOnlyOwnerReadWrite))

	targetImage := "v2-pci-file-secret-target"
	namespace := createActiveV2Namespace(t, ctx, "v2-pci-file-secret")
	createImage := func(name string) *apiv2.PhysicalContainerImage {
		image := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace.Name,
			},
			Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
				Image: targetImage,
				Build: &apiv2.ContainerBuildContext{
					Digest:         "context-v1",
					ContextArchive: &apiv2.ContainerBuildContextArchive{RawContents: "dGVzdA=="},
					Secrets: []apiv2.ContainerBuildSecret{{
						Type:   apiv2.FileSecret,
						ID:     "secret",
						Source: secretPath,
					}},
				},
			}},
		}
		require.NoError(t, client.Create(ctx, image))
		return waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	}

	firstImage := createImage("first-file-secret")
	secondImage := createImage("second-file-secret")
	require.Equal(t, firstImage.Status.ImageID, secondImage.Status.ImageID)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))

	require.NoError(t, usvc_io.WriteFile(secretPath, []byte("secret-two"), osutil.PermissionOnlyOwnerReadWrite))
	thirdImage := createImage("third-file-secret")
	require.NotEqual(t, secondImage.Status.ImageID, thirdImage.Status.ImageID)
	require.Equal(t, 2, containerOrchestrator.BuildImageCallCount(targetImage))
}

func TestV2PhysicalContainerImageControllerReportsUnreadableFileSecret(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	secretPath := filepath.Join(t.TempDir(), "missing-secret")
	namespace := createActiveV2Namespace(t, ctx, "v2-pci-missing-file-secret")
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-file-secret",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
			Image: "v2-pci-missing-file-secret-target",
			Build: &apiv2.ContainerBuildContext{
				Digest:         "context-v1",
				ContextArchive: &apiv2.ContainerBuildContextArchive{RawContents: "dGVzdA=="},
				Secrets: []apiv2.ContainerBuildSecret{{
					Type:   apiv2.FileSecret,
					ID:     "secret",
					Source: secretPath,
				}},
			},
		}},
	}
	require.NoError(t, client.Create(ctx, image))

	failedImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseFailed)
	requireReadyCondition(t, failedImage.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonBuildFailed)
	readyCondition := apimeta.FindStatusCondition(failedImage.Status.Conditions, string(apiv2.ConditionReady))
	require.NotNil(t, readyCondition)
	require.Contains(t, readyCondition.Message, secretPath)
}

func TestV2PhysicalContainerImageControllerReusesDirectoryBuildOutputByDigest(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	targetImage := "v2-pci-directory-build-target"
	namespace := createActiveV2Namespace(t, ctx, "v2-pci-directory-build")
	createImage := func(name, contextPath, digest string) *apiv2.PhysicalContainerImage {
		image := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace.Name,
			},
			Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
				Image: targetImage,
				Build: &apiv2.ContainerBuildContext{
					Context: contextPath,
					Digest:  digest,
				},
			}},
		}
		require.NoError(t, client.Create(ctx, image))
		return waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	}

	firstImage := createImage("first-directory-build", "first-context", "context-v1")
	secondImage := createImage("second-directory-build", "second-context", "context-v1")
	require.Equal(t, firstImage.Status.ImageID, secondImage.Status.ImageID)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))

	thirdImage := createImage("third-directory-build", "second-context", "context-v2")
	require.NotEqual(t, secondImage.Status.ImageID, thirdImage.Status.ImageID)
	require.Equal(t, 2, containerOrchestrator.BuildImageCallCount(targetImage))
}

func TestV2PhysicalContainerImageControllerBuildsWithoutContextDigest(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	targetImage := "v2-pci-no-context-digest-target"
	namespace := createActiveV2Namespace(t, ctx, "v2-pci-no-context-digest")
	createImage := func(name string) *apiv2.PhysicalContainerImage {
		image := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace.Name,
			},
			Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
				Image: targetImage,
				Build: &apiv2.ContainerBuildContext{Context: "test-context"},
			}},
		}
		require.NoError(t, client.Create(ctx, image))
		return waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	}

	firstImage := createImage("first-no-context-digest")
	secondImage := createImage("second-no-context-digest")
	require.NotEqual(t, firstImage.Status.ImageID, secondImage.Status.ImageID)
	require.Equal(t, 2, containerOrchestrator.BuildImageCallCount(targetImage))
}

func TestV2PhysicalContainerImageControllerRebuildsWhenBuildTagsChange(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	targetImage := "v2-pci-build-tags-target"
	namespace := createActiveV2Namespace(t, ctx, "v2-pci-build-tags")
	createImage := func(name string, secondaryTags []string) *apiv2.PhysicalContainerImage {
		image := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace.Name,
			},
			Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
				Image: targetImage,
				Build: &apiv2.ContainerBuildContext{
					Context: "test-context",
					Digest:  "context-v1",
					Tags:    secondaryTags,
				},
			}},
		}
		require.NoError(t, client.Create(ctx, image))
		return waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	}

	firstImage := createImage("first-build-tags", []string{"v2-pci-secondary-a", "v2-pci-secondary-b"})
	secondImage := createImage("second-build-tags", []string{"v2-pci-secondary-b", "v2-pci-secondary-a"})
	require.Equal(t, firstImage.Status.ImageID, secondImage.Status.ImageID)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))

	thirdImage := createImage("third-build-tags", []string{"v2-pci-secondary-c"})
	require.NotEqual(t, secondImage.Status.ImageID, thirdImage.Status.ImageID)
	require.Equal(t, 2, containerOrchestrator.BuildImageCallCount(targetImage))

	inspectedImages, inspectErr := containerOrchestrator.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{targetImage}})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedImages, 1)
	require.Contains(t, inspectedImages[0].Tags, "v2-pci-secondary-c")
}

func TestV2PhysicalContainerImageControllerAlwaysBuildPolicyRebuildsMatchingOutput(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	targetImage := "v2-pci-always-build-target"
	namespace := createActiveV2Namespace(t, ctx, "v2-pci-always-build")
	createImage := func(name string) *apiv2.PhysicalContainerImage {
		image := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace.Name,
			},
			Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
				Image:       targetImage,
				BuildPolicy: apiv2.BuildPolicyAlways,
				Build: &apiv2.ContainerBuildContext{
					Digest: "context-v1",
					ContextArchive: &apiv2.ContainerBuildContextArchive{
						RawContents: "dGVzdA==",
					},
				},
			}},
		}
		require.NoError(t, client.Create(ctx, image))
		return waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	}

	firstImage := createImage("first-always-build")
	secondImage := createImage("second-always-build")
	require.NotEqual(t, firstImage.Status.ImageID, secondImage.Status.ImageID)
	require.Equal(t, 2, containerOrchestrator.BuildImageCallCount(targetImage))
}

func TestV2PhysicalContainerImageControllerAlwaysPullPolicyReusesMatchingBuildOutput(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	baseImage := "v2-pci-always-pull-base"
	targetImage := "v2-pci-always-pull-target"
	_, pullErr := containerOrchestrator.PullImage(ctx, containers.PullImageOptions{Image: baseImage})
	require.NoError(t, pullErr)

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-always-pull-build")
	createImage := func(name string) *apiv2.PhysicalContainerImage {
		image := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace.Name,
			},
			Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
				Image:      targetImage,
				PullPolicy: apiv2.PullPolicyAlways,
				Build: &apiv2.ContainerBuildContext{
					Digest: "context-v1",
					ContextArchive: &apiv2.ContainerBuildContextArchive{
						RawContents: "dGVzdA==",
					},
					BaseImages: []string{baseImage},
				},
			}},
		}
		require.NoError(t, client.Create(ctx, image))
		return waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	}

	firstImage := createImage("first-always-pull-build")
	secondImage := createImage("second-always-pull-build")
	require.Equal(t, firstImage.Status.ImageID, secondImage.Status.ImageID)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))
	require.Equal(t, 3, containerOrchestrator.PullImageCallCount(baseImage))
}

func TestV2PhysicalContainerImageControllerNeverPullPolicyUsesLocalBuildBase(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	baseImage := "v2-pci-never-pull-base"
	targetImage := "v2-pci-never-pull-target"
	_, pullErr := containerOrchestrator.PullImage(ctx, containers.PullImageOptions{Image: baseImage})
	require.NoError(t, pullErr)

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-never-pull-build")
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "never-pull-build",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
			Image:      targetImage,
			PullPolicy: apiv2.PullPolicyNever,
			Build: &apiv2.ContainerBuildContext{
				Context:    "test-context",
				BaseImages: []string{baseImage},
			},
		}},
	}
	require.NoError(t, client.Create(ctx, image))

	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(baseImage))
}

func TestV2PhysicalContainerImageControllerMissingPullPolicyPullsMissingBuildBase(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	baseImage := "v2-pci-missing-pull-base"
	targetImage := "v2-pci-missing-pull-target"
	namespace := createActiveV2Namespace(t, ctx, "v2-pci-missing-pull-build")
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-pull-build",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
			Image:      targetImage,
			PullPolicy: apiv2.PullPolicyMissing,
			Build: &apiv2.ContainerBuildContext{
				Context:    "test-context",
				BaseImages: []string{baseImage},
			},
		}},
	}
	require.NoError(t, client.Create(ctx, image))

	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(baseImage))
}

func TestV2PhysicalContainerImageControllerMissingPullPolicyTracksLocalBuildBaseIdentity(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	baseImage := "v2-pci-missing-pull-identity-base"
	targetImage := "v2-pci-missing-pull-identity-target"
	buildBaseImage := func() {
		require.NoError(t, containerOrchestrator.BuildImage(ctx, containers.BuildImageOptions{
			ContainerBuildContext: &containers.ContainerBuildContext{
				Context: "base-context",
				Tags:    []string{baseImage},
			},
		}))
	}
	buildBaseImage()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-missing-pull-identity")
	createImage := func(name string) *apiv2.PhysicalContainerImage {
		image := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace.Name,
			},
			Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
				Image:      targetImage,
				PullPolicy: apiv2.PullPolicyMissing,
				Build: &apiv2.ContainerBuildContext{
					Digest: "context-v1",
					ContextArchive: &apiv2.ContainerBuildContextArchive{
						RawContents: "dGVzdA==",
					},
					BaseImages: []string{baseImage},
				},
			}},
		}
		require.NoError(t, client.Create(ctx, image))
		return waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	}

	firstImage := createImage("first-missing-pull-identity")
	secondImage := createImage("second-missing-pull-identity")
	require.Equal(t, firstImage.Status.ImageID, secondImage.Status.ImageID)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))

	buildBaseImage()
	thirdImage := createImage("third-missing-pull-identity")
	require.NotEqual(t, secondImage.Status.ImageID, thirdImage.Status.ImageID)
	require.Equal(t, 2, containerOrchestrator.BuildImageCallCount(targetImage))
	require.Equal(t, 0, containerOrchestrator.PullImageCallCount(baseImage))
}

func TestV2PhysicalContainerImageControllerNeverPullPolicyFailsForMissingBuildBase(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	baseImage := "v2-pci-never-pull-missing-base"
	targetImage := "v2-pci-never-pull-missing-target"
	namespace := createActiveV2Namespace(t, ctx, "v2-pci-never-pull-missing-build")
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "never-pull-missing-build",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
			Image:      targetImage,
			PullPolicy: apiv2.PullPolicyNever,
			Build: &apiv2.ContainerBuildContext{
				Context:    "test-context",
				BaseImages: []string{baseImage},
			},
		}},
	}
	require.NoError(t, client.Create(ctx, image))

	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseFailed)
	require.Equal(t, 0, containerOrchestrator.BuildImageCallCount(targetImage))
	require.Equal(t, 0, containerOrchestrator.PullImageCallCount(baseImage))
}

func TestV2PhysicalContainerImageControllerRebuildsWhenBestEffortBaseImageChanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	baseImage := "v2-pci-best-effort-base"
	targetImage := "v2-pci-best-effort-target"
	buildBaseImage := func() {
		require.NoError(t, containerOrchestrator.BuildImage(ctx, containers.BuildImageOptions{
			ContainerBuildContext: &containers.ContainerBuildContext{
				Context: "base-context",
				Tags:    []string{baseImage},
			},
		}))
	}
	buildBaseImage()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-best-effort-build")
	createImage := func(name string) *apiv2.PhysicalContainerImage {
		image := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace.Name,
			},
			Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
				Image:      targetImage,
				PullPolicy: apiv2.PullPolicyBestEffort,
				Build: &apiv2.ContainerBuildContext{
					Digest: "context-v1",
					ContextArchive: &apiv2.ContainerBuildContextArchive{
						RawContents: "dGVzdA==",
					},
					BaseImages: []string{baseImage},
				},
			}},
		}
		require.NoError(t, client.Create(ctx, image))
		return waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	}

	firstImage := createImage("first-best-effort-build")
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))

	secondImage := createImage("second-best-effort-build")
	require.Equal(t, firstImage.Status.ImageID, secondImage.Status.ImageID)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))

	buildBaseImage()
	thirdImage := createImage("third-best-effort-build")
	require.NotEqual(t, secondImage.Status.ImageID, thirdImage.Status.ImageID)
	require.Equal(t, 2, containerOrchestrator.BuildImageCallCount(targetImage))
	require.Equal(t, 3, containerOrchestrator.PullImageCallCount(baseImage))
}

func TestV2PhysicalContainerImageControllerUsesLocalBestEffortBaseImageAfterPullFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	baseImage := "v2-pci-best-effort-local-base"
	targetImage := "v2-pci-best-effort-local-target"
	_, pullErr := containerOrchestrator.PullImage(ctx, containers.PullImageOptions{Image: baseImage})
	require.NoError(t, pullErr)
	containerOrchestrator.FailNextPullImage(baseImage, errors.New("pull unavailable"))
	noRetries := int32(0)

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-best-effort-local")
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "best-effort-local-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
			Image:          targetImage,
			PullPolicy:     apiv2.PullPolicyBestEffort,
			PullRetryLimit: &noRetries,
			Build: &apiv2.ContainerBuildContext{
				Context:    "test-context",
				BaseImages: []string{baseImage},
			},
		}},
	}
	require.NoError(t, client.Create(ctx, image))

	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))
	require.Equal(t, 2, containerOrchestrator.PullImageCallCount(baseImage))
}

func TestV2PhysicalContainerImageControllerFailsWhenBestEffortBaseImageIsUnavailable(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	baseImage := "v2-pci-best-effort-missing-base"
	targetImage := "v2-pci-best-effort-missing-target"
	containerOrchestrator.FailNextPullImage(baseImage, errors.New("pull unavailable"))
	noRetries := int32(0)

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-best-effort-missing")
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "best-effort-missing-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
			Image:          targetImage,
			PullPolicy:     apiv2.PullPolicyBestEffort,
			PullRetryLimit: &noRetries,
			Build: &apiv2.ContainerBuildContext{
				Context:    "test-context",
				BaseImages: []string{baseImage},
			},
		}},
	}
	require.NoError(t, client.Create(ctx, image))

	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseFailed)
	require.Equal(t, 0, containerOrchestrator.BuildImageCallCount(targetImage))
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(baseImage))
}

func TestV2PhysicalContainerImageControllerReportsMissingBuildImageID(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-build-missing-id")
	targetImage := "v2-pci-build-missing-id-target"
	restoreImageID := containerOrchestrator.OmitBuildImageID(targetImage)
	defer restoreImageID()

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-build-image-id",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: targetImage, Build: &apiv2.ContainerBuildContext{
			Context: "test-context",
		}},
		},
	}
	require.NoError(t, client.Create(ctx, image))

	failedImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseFailed)
	requireReadyCondition(t, failedImage.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonBuildResultMissingImageID)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))
}

func TestV2PhysicalContainerImageControllerRetriesCompletedBuildInspectionWithoutRebuilding(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-build-inspect-retry")
	targetImage := "v2-pci-build-inspect-retry-target"
	restoreInspection := containerOrchestrator.FailInspectImage(targetImage, errors.New("inspect failed"))
	defer restoreInspection()

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "build-inspect-retry-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: targetImage, Build: &apiv2.ContainerBuildContext{
			Context: "test-context",
		}},
		},
	}
	require.NoError(t, client.Create(ctx, image))
	waitBuildImageCallCount(t, ctx, targetImage, 1)

	unknownImage := waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseUnknown)
	requireReadyCondition(t, unknownImage.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonRuntimeImageInspectFailed)
	restoreInspection()

	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))
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
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: targetImage, Build: &apiv2.ContainerBuildContext{
			Context: "test-context",
		}},
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
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: sourceImage, PullPolicy: apiv2.PullPolicyAlways,
			PullRetryLimit: &noRetries},
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

func TestV2PhysicalContainerImageControllerWaitsForHealthyRuntimeBeforePull(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	containerOrchestrator.SetRuntimeHealth(false)
	defer containerOrchestrator.SetRuntimeHealth(true)

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-runtime-gate")
	sourceImage := "v2-pci-runtime-gate-source"
	noRetries := int32(0)
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-gated-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
			Image:          sourceImage,
			PullPolicy:     apiv2.PullPolicyAlways,
			PullRetryLimit: &noRetries,
		}},
	}
	require.NoError(t, client.Create(ctx, image))

	waitObjectAssumesState(t, ctx, image.NamespacedName(), func(currentImage *apiv2.PhysicalContainerImage) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(currentImage.Status.Conditions, string(apiv2.ConditionReady))
		return currentImage.Status.Phase == apiv2.PhysicalContainerImagePhasePending &&
			readyCondition != nil &&
			apiv2.ConditionReason(readyCondition.Reason) == apiv2.PhysicalResourceReasonContainerRuntimeUnhealthy, nil
	})
	require.Equal(t, 0, containerOrchestrator.PullImageCallCount(sourceImage))

	containerOrchestrator.SetRuntimeHealth(true)

	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(sourceImage))
}

func TestV2PhysicalContainerImageControllerPreservesPullBudgetWhenRuntimeBecomesUnhealthy(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-runtime-pull")
	sourceImage := "v2-pci-runtime-pull-source"
	releasePull := containerOrchestrator.BlockPullImage(sourceImage)
	defer releasePull()

	noRetries := int32(0)
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-interrupted-pull-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
			Image:          sourceImage,
			PullPolicy:     apiv2.PullPolicyAlways,
			PullRetryLimit: &noRetries,
		}},
	}
	require.NoError(t, client.Create(ctx, image))
	waitPullImageCallCount(t, ctx, sourceImage, 1)

	containerOrchestrator.SetRuntimeHealth(false)
	defer containerOrchestrator.SetRuntimeHealth(true)
	releasePull()

	waitObjectAssumesState(t, ctx, image.NamespacedName(), func(currentImage *apiv2.PhysicalContainerImage) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(currentImage.Status.Conditions, string(apiv2.ConditionReady))
		return currentImage.Status.Phase == apiv2.PhysicalContainerImagePhasePending &&
			readyCondition != nil &&
			apiv2.ConditionReason(readyCondition.Reason) == apiv2.PhysicalResourceReasonContainerRuntimeUnhealthy, nil
	})
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(sourceImage))

	containerOrchestrator.SetRuntimeHealth(true)

	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, 2, containerOrchestrator.PullImageCallCount(sourceImage))
}

func TestV2PhysicalContainerImageControllerPreservesBasePullBudgetWhenRuntimeBecomesUnhealthy(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pci-runtime-base-pull")
	baseImage := "v2-pci-runtime-base-pull-source"
	targetImage := "v2-pci-runtime-base-pull-target"
	releasePull := containerOrchestrator.BlockPullImage(baseImage)
	defer releasePull()

	noRetries := int32(0)
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-interrupted-base-pull-image",
			Namespace: namespace.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
			Image:          targetImage,
			PullPolicy:     apiv2.PullPolicyAlways,
			PullRetryLimit: &noRetries,
			Build: &apiv2.ContainerBuildContext{
				Context:    "test-context",
				BaseImages: []string{baseImage},
			},
		}},
	}
	require.NoError(t, client.Create(ctx, image))
	waitPullImageCallCount(t, ctx, baseImage, 1)

	containerOrchestrator.SetRuntimeHealth(false)
	defer containerOrchestrator.SetRuntimeHealth(true)
	releasePull()

	waitObjectAssumesState(t, ctx, image.NamespacedName(), func(currentImage *apiv2.PhysicalContainerImage) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(currentImage.Status.Conditions, string(apiv2.ConditionReady))
		return currentImage.Status.Phase == apiv2.PhysicalContainerImagePhasePending &&
			readyCondition != nil &&
			apiv2.ConditionReason(readyCondition.Reason) == apiv2.PhysicalResourceReasonContainerRuntimeUnhealthy, nil
	})
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(baseImage))
	require.Equal(t, 0, containerOrchestrator.BuildImageCallCount(targetImage))

	containerOrchestrator.SetRuntimeHealth(true)

	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	require.Equal(t, 2, containerOrchestrator.PullImageCallCount(baseImage))
	require.Equal(t, 1, containerOrchestrator.BuildImageCallCount(targetImage))
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
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: sourceImage, PullPolicy: apiv2.PullPolicyAlways}},
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
