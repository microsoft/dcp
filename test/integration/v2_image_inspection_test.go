/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

type boundedImageInspector struct {
	containers.ContainerOrchestrator
	remaining       atomic.Int32
	injected        atomic.Int32
	inspectionError error
	failAfterPull   int32
}

func (inspector *boundedImageInspector) InspectImages(ctx context.Context, options containers.InspectImagesOptions) ([]containers.InspectedImage, error) {
	for {
		remaining := inspector.remaining.Load()
		if remaining <= 0 {
			return inspector.ContainerOrchestrator.InspectImages(ctx, options)
		}
		if inspector.remaining.CompareAndSwap(remaining, remaining-1) {
			inspector.injected.Add(1)
			return nil, inspector.inspectionError
		}
	}
}

func (inspector *boundedImageInspector) PullImage(ctx context.Context, options containers.PullImageOptions) (string, error) {
	imageID, pullErr := inspector.ContainerOrchestrator.PullImage(ctx, options)
	if pullErr == nil {
		inspector.remaining.Store(inspector.failAfterPull)
	}
	return imageID, pullErr
}

// Verifies that pre-pull inspection errors and initially missing acquired image IDs recover
// within three retries without duplicating image pulls.
func TestV2ImageInspectionRetriesTransientFailures(t *testing.T) {
	for _, stage := range []string{"before-pull", "after-pull"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var inspector *boundedImageInspector
			var runtime *ctrl_testutil.TestContainerOrchestrator
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
				NamespaceController|PhysicalContainerImageController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
					runtime = orchestrator.(*ctrl_testutil.TestContainerOrchestrator)
					inspector = &boundedImageInspector{ContainerOrchestrator: orchestrator}
					if stage == "before-pull" {
						inspector.inspectionError = errors.New("temporary inspection timeout")
						inspector.remaining.Store(3)
					} else {
						inspector.inspectionError = containers.ErrNotFound
						inspector.failAfterPull = 3
					}
					return inspector
				}})
			require.NoError(t, startErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "inspection-recovery"}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			image := &apiv2.PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: namespace.Name},
				Spec:       apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: "retry-source"}},
			}
			require.NoError(t, server.Client.Create(ctx, image))
			ready := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
				return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
			})
			require.NotEmpty(t, ready.Status.ImageID)
			require.Equal(t, int32(3), inspector.injected.Load())
			require.Equal(t, 1, runtime.PullImageCallCount("retry-source"))
		})
	}
}

// Verifies that the fourth consecutive inspection failure exhausts three retries, reports
// a terminal inspection failure, and never starts a pull or retries after a metadata wake.
func TestV2ImageInspectionStopsAfterRetryLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var inspector *boundedImageInspector
	var runtime *ctrl_testutil.TestContainerOrchestrator
	server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
		NamespaceController|PhysicalContainerImageController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
			runtime = orchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			inspector = &boundedImageInspector{ContainerOrchestrator: orchestrator, inspectionError: errors.New("inspection unavailable")}
			inspector.remaining.Store(10)
			return inspector
		}})
	require.NoError(t, startErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "inspection-exhaustion"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: "never-pulled"}},
	}
	require.NoError(t, server.Client.Create(ctx, image))
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerImagePhaseFailed, nil
	})
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonRuntimeImageInspectFailed)
	require.Equal(t, int32(4), inspector.injected.Load())
	require.Zero(t, runtime.PullImageCallCount("never-pulled"))
	require.NoError(t, retryOnConflictEx(ctx, server.Client, image.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainerImage) error {
		current.Status.Phase = apiv2.PhysicalContainerImagePhasePending
		return server.Client.Status().Update(updateCtx, current)
	}))
	waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerImagePhaseFailed, nil
	})
	require.Equal(t, int32(4), inspector.injected.Load())
}

// Verifies that successful inspection resets the retry budget, so two separate transient
// failure bursts during runtime monitoring recover without reacquiring the image.
func TestV2ImageInspectionResetsRetriesAfterSuccess(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var inspector *boundedImageInspector
	var runtime *ctrl_testutil.TestContainerOrchestrator
	server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
		NamespaceController|PhysicalContainerImageController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
			runtime = orchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			inspector = &boundedImageInspector{ContainerOrchestrator: orchestrator, inspectionError: errors.New("temporary inspect failure")}
			return inspector
		}})
	require.NoError(t, startErr)
	imageID, pullErr := runtime.PullImage(ctx, containers.PullImageOptions{Image: "existing-retry-image"})
	require.NoError(t, pullErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "inspection-reset"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalContainerImageSpec{ImageID: imageID},
	}
	require.NoError(t, server.Client.Create(ctx, image))
	for attempt := 0; attempt < 3; attempt++ {
		ready := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
			return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
		})
		require.Equal(t, imageID, ready.Status.ImageID)
		if attempt == 2 {
			break
		}
		inspector.remaining.Store(2)
		require.NoError(t, retryOnConflictEx(ctx, server.Client, image.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainerImage) error {
			current.Status.Phase = apiv2.PhysicalContainerImagePhaseUnknown
			return server.Client.Status().Update(updateCtx, current)
		}))
	}
	require.Equal(t, int32(4), inspector.injected.Load())
	require.Equal(t, 1, runtime.PullImageCallCount("existing-retry-image"))
}
