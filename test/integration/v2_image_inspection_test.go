/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
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
	remaining             atomic.Int32
	injected              atomic.Int32
	successful            atomic.Int32
	inspectionError       error
	inspectionResults     []containers.InspectedImage
	failAfterPull         int32
	returnPulledReference bool
	failAfterBuild        int32
	runtimeUnavailable    atomic.Bool
	runtimeFailures       atomic.Int32
	uncachedHealthChecks  atomic.Int32
	outageAfterPull       bool
	failAfterVerification bool
	verificationObserved  atomic.Bool
}

func (inspector *boundedImageInspector) CheckStatus(ctx context.Context, usage containers.CachedRuntimeStatusUsage) containers.ContainerRuntimeStatus {
	if usage == containers.IgnoreCachedRuntimeStatus {
		inspector.uncachedHealthChecks.Add(1)
	}
	return inspector.ContainerOrchestrator.CheckStatus(ctx, usage)
}

func (inspector *boundedImageInspector) InspectImages(ctx context.Context, options containers.InspectImagesOptions) ([]containers.InspectedImage, error) {
	if inspector.runtimeUnavailable.Load() {
		inspector.runtimeFailures.Add(1)
		return nil, containers.ErrRuntimeNotHealthy
	}
	for {
		remaining := inspector.remaining.Load()
		if remaining <= 0 {
			inspected, inspectErr := inspector.ContainerOrchestrator.InspectImages(ctx, options)
			if inspectErr == nil {
				inspector.successful.Add(1)
			}
			if inspectErr == nil && len(inspected) == 1 && inspected[0].Id != "" &&
				inspector.failAfterVerification && inspector.verificationObserved.CompareAndSwap(false, true) {
				inspector.remaining.Store(1)
			}
			return inspected, inspectErr
		}
		if inspector.remaining.CompareAndSwap(remaining, remaining-1) {
			inspector.injected.Add(1)
			return inspector.inspectionResults, inspector.inspectionError
		}
	}
}

// Verifies that an outage after pulling but before initial ID verification preserves the
// selected ID and resumes inspection without performing another pull.
func TestV2ImageInspectionRecoversUnverifiedPulledIdentity(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var inspector *boundedImageInspector
	var runtime *ctrl_testutil.TestContainerOrchestrator
	server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
		NamespaceController|PhysicalContainerImageController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
			runtime = orchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			inspector = &boundedImageInspector{ContainerOrchestrator: orchestrator, outageAfterPull: true}
			return inspector
		}})
	require.NoError(t, startErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "unverified-image-outage"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image: &apiv2.PhysicalContainerImageConfig{Image: "unverified-outage-image", PullPolicy: apiv2.PullPolicyAlways},
		},
	}
	require.NoError(t, server.Client.Create(ctx, image))
	unavailable := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerImagePhasePending &&
			inspector.runtimeFailures.Load() >= 4, nil
	})
	require.Empty(t, unavailable.Status.ImageID)
	requireReadyCondition(t, unavailable.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalResourceReasonContainerRuntimeUnhealthy)
	selected, inspectErr := runtime.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{"unverified-outage-image"}})
	require.NoError(t, inspectErr)
	require.Len(t, selected, 1)
	inspector.runtimeUnavailable.Store(false)
	require.NoError(t, retryOnConflictEx(ctx, server.Client, image.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainerImage) error {
		current.Annotations = map[string]string{"test.dcp.microsoft.com/reconcile": "runtime-recovered"}
		return server.Client.Update(updateCtx, current)
	}))
	recovered := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
	})
	require.Equal(t, selected[0].Id, recovered.Status.ImageID)
	require.Equal(t, 1, runtime.PullImageCallCount("unverified-outage-image"))
	require.Zero(t, inspector.uncachedHealthChecks.Load())
}

// Verifies that a known runtime outage during initial local-image inspection remains retryable
// beyond the ordinary inspection budget, then recovers without pulling an image.
func TestV2ImageInspectionDefersStartupDuringRuntimeOutage(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"existing", "never-pull"} {
		t.Run(mode, func(t *testing.T) {
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
					inspector.runtimeUnavailable.Store(true)
					return inspector
				}})
			require.NoError(t, startErr)
			imageID, pullErr := runtime.PullImage(ctx, containers.PullImageOptions{Image: "startup-outage-image"})
			require.NoError(t, pullErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "startup-outage"}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			image := &apiv2.PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: namespace.Name},
				Spec:       apiv2.PhysicalContainerImageSpec{ImageID: imageID},
			}
			if mode == "never-pull" {
				image.Spec = apiv2.PhysicalContainerImageSpec{
					Image: &apiv2.PhysicalContainerImageConfig{Image: "startup-outage-image", PullPolicy: apiv2.PullPolicyNever},
				}
			}
			require.NoError(t, server.Client.Create(ctx, image))
			waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
				return current.Status.Phase == apiv2.PhysicalContainerImagePhasePending &&
					inspector.runtimeFailures.Load() >= 4, nil
			})
			inspector.runtimeUnavailable.Store(false)
			require.NoError(t, retryOnConflictEx(ctx, server.Client, image.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainerImage) error {
				current.Annotations = map[string]string{"test.dcp.microsoft.com/reconcile": "runtime-recovered"}
				return server.Client.Update(updateCtx, current)
			}))
			ready := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
				return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
			})
			require.Equal(t, imageID, ready.Status.ImageID)
			require.Equal(t, 1, runtime.PullImageCallCount("startup-outage-image"))
			require.Zero(t, inspector.uncachedHealthChecks.Load())
		})
	}
}

// Verifies that a runtime outage after acquisition preserves the pinned identity and
// does not consume inspection retries, repull, or rebuild when observation resumes.
func TestV2ImageInspectionPreservesIdentityAcrossRuntimeOutage(t *testing.T) {
	t.Parallel()
	for _, acquisition := range []string{"pull", "build"} {
		t.Run(acquisition, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var inspector *boundedImageInspector
			var runtime *ctrl_testutil.TestContainerOrchestrator
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
				NamespaceController|PhysicalContainerImageController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
					runtime = orchestrator.(*ctrl_testutil.TestContainerOrchestrator)
					inspector = &boundedImageInspector{ContainerOrchestrator: orchestrator, inspectionError: errors.New("temporary inspection failure")}
					return inspector
				}})
			require.NoError(t, startErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "observation-outage"}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			config := &apiv2.PhysicalContainerImageConfig{Image: "pinned-outage-image", PullPolicy: apiv2.PullPolicyAlways}
			if acquisition == "build" {
				config.Build = &apiv2.ContainerBuildContext{Context: t.TempDir()}
				config.BuildPolicy = apiv2.BuildPolicyAlways
			}
			image := &apiv2.PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: namespace.Name},
				Spec:       apiv2.PhysicalContainerImageSpec{Image: config},
			}
			require.NoError(t, server.Client.Create(ctx, image))
			initial := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
				return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
			})
			pulls, builds := runtime.PullImageCallCount(config.Image), runtime.BuildImageCallCount(config.Image)
			inspector.runtimeUnavailable.Store(true)
			require.NoError(t, retryOnConflictEx(ctx, server.Client, image.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainerImage) error {
				current.Annotations = map[string]string{"test.dcp.microsoft.com/reconcile": "runtime-outage"}
				return server.Client.Update(updateCtx, current)
			}))
			unavailable := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
				return current.Status.Phase == apiv2.PhysicalContainerImagePhasePending &&
					inspector.runtimeFailures.Load() >= 4, nil
			})
			requireReadyCondition(t, unavailable.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalResourceReasonContainerRuntimeUnhealthy)
			require.Equal(t, initial.Status.ImageID, unavailable.Status.ImageID)
			require.Equal(t, initial.Status.Digest, unavailable.Status.Digest)
			require.Equal(t, initial.Status.Tags, unavailable.Status.Tags)
			inspector.remaining.Store(3)
			inspector.runtimeUnavailable.Store(false)
			require.NoError(t, retryOnConflictEx(ctx, server.Client, image.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainerImage) error {
				current.Annotations = map[string]string{"test.dcp.microsoft.com/reconcile": "runtime-recovered"}
				return server.Client.Update(updateCtx, current)
			}))
			recovered := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
				return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
			})
			require.Equal(t, initial.Status.ImageID, recovered.Status.ImageID)
			require.Equal(t, int32(3), inspector.injected.Load())
			require.Equal(t, pulls, runtime.PullImageCallCount(config.Image))
			require.Equal(t, builds, runtime.BuildImageCallCount(config.Image))
			require.Zero(t, inspector.uncachedHealthChecks.Load())
		})
	}
}

func (inspector *boundedImageInspector) PullImage(ctx context.Context, options containers.PullImageOptions) (string, error) {
	imageID, pullErr := inspector.ContainerOrchestrator.PullImage(ctx, options)
	if pullErr == nil {
		inspector.remaining.Store(inspector.failAfterPull)
		if inspector.outageAfterPull {
			inspector.runtimeUnavailable.Store(true)
		}
		if inspector.returnPulledReference {
			reference := options.Image
			if options.Digest != "" {
				reference += "@" + options.Digest
			}
			return reference, nil
		}
	}
	return imageID, pullErr
}

func (inspector *boundedImageInspector) BuildImage(ctx context.Context, options containers.BuildImageOptions) error {
	buildErr := inspector.ContainerOrchestrator.BuildImage(ctx, options)
	if buildErr == nil {
		inspector.remaining.Store(inspector.failAfterBuild)
	}
	return buildErr
}

// Verifies that pulled, built and existing image identity verification retries failures,
// becomes terminal on exhaustion, and never repeats completed acquisition.
func TestV2ImageIdentityVerificationDoesNotRepeatAcquisition(t *testing.T) {
	t.Parallel()
	for _, acquisition := range []string{"pull", "build", "existing"} {
		for _, failure := range []string{"missing", "inspection", "empty-ID"} {
			t.Run(acquisition+"/"+failure, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
				t.Cleanup(cancel)
				var inspector *boundedImageInspector
				var runtime *ctrl_testutil.TestContainerOrchestrator
				server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
					NamespaceController|PhysicalContainerImageController, t.Name(), t.TempDir(),
					TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
						runtime = orchestrator.(*ctrl_testutil.TestContainerOrchestrator)
						inspector = &boundedImageInspector{ContainerOrchestrator: orchestrator, failAfterPull: 100, failAfterBuild: 100,
							inspectionError: containers.ErrNotFound}
						if failure == "inspection" {
							inspector.inspectionError = errors.New("injected identity inspection failure")
						} else if failure == "empty-ID" {
							inspector.inspectionError = nil
							inspector.inspectionResults = []containers.InspectedImage{{}}
						}
						return inspector
					}})
				require.NoError(t, startErr)
				namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "identity-verification"}}
				require.NoError(t, server.Client.Create(ctx, namespace))
				source := "identity-verification-source"
				config := &apiv2.PhysicalContainerImageConfig{Image: source, PullPolicy: apiv2.PullPolicyAlways}
				image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: namespace.Name},
					Spec: apiv2.PhysicalContainerImageSpec{Image: config}}
				if acquisition == "build" {
					config.Image, config.BuildPolicy = "", apiv2.BuildPolicyAlways
					config.Build = &apiv2.ContainerBuildContext{Context: t.TempDir(), Tags: []string{source}}
				} else if acquisition == "existing" {
					imageID, pullErr := runtime.PullImage(ctx, containers.PullImageOptions{Image: source})
					require.NoError(t, pullErr)
					image.Spec = apiv2.PhysicalContainerImageSpec{ImageID: imageID}
					inspector.remaining.Store(100)
				}
				require.NoError(t, server.Client.Create(ctx, image))
				failed := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerImagePhaseFailed, nil
				})
				reason := apiv2.PhysicalContainerImageReasonLocalImageNotFound
				if failure != "missing" {
					reason = apiv2.PhysicalContainerImageReasonRuntimeImageInspectFailed
				}
				requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, reason)
				require.Empty(t, failed.Status.ImageID)
				require.Greater(t, inspector.injected.Load(), int32(1))
				pulls, builds := runtime.PullImageCallCount(source), runtime.BuildImageCallCount(source)
				if acquisition == "build" {
					require.Zero(t, pulls)
					require.Equal(t, 1, builds)
				} else {
					require.Equal(t, 1, pulls)
					require.Zero(t, builds)
				}
				inspector.remaining.Store(0)
				require.NoError(t, retryOnConflictEx(ctx, server.Client, image.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainerImage) error {
					current.Annotations = map[string]string{"test.dcp.microsoft.com/reconcile": "after-verification-exhaustion"}
					return server.Client.Update(updateCtx, current)
				}))
				stillFailed := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerImagePhaseFailed &&
						current.Annotations["test.dcp.microsoft.com/reconcile"] == "after-verification-exhaustion", nil
				})
				require.Empty(t, stillFailed.Status.ImageID)
				require.Equal(t, pulls, runtime.PullImageCallCount(source))
				require.Equal(t, builds, runtime.BuildImageCallCount(source))
				require.NoError(t, server.Client.Delete(ctx, namespace))
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
			})
		}
	}
}

// Verifies that pre-pull inspection errors and initially missing acquired image IDs recover
// within three retries without duplicating image pulls, and successful verification resets
// pre-pull failures before subsequent runtime monitoring.
func TestV2ImageInspectionRetriesTransientFailures(t *testing.T) {
	t.Parallel()
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
					inspector = &boundedImageInspector{ContainerOrchestrator: orchestrator, returnPulledReference: true}
					if stage == "before-pull" {
						inspector.inspectionError = errors.New("temporary inspection timeout")
						inspector.remaining.Store(3)
						inspector.failAfterVerification = true
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
				return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady ||
					current.Status.Phase == apiv2.PhysicalContainerImagePhaseFailed, nil
			})
			require.Equal(t, apiv2.PhysicalContainerImagePhaseReady, ready.Status.Phase)
			require.NotEmpty(t, ready.Status.ImageID)
			require.Equal(t, 1, runtime.PullImageCallCount("retry-source"))
			if stage == "before-pull" {
				require.NoError(t, retryOnConflictEx(ctx, server.Client, image.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainerImage) error {
					current.Annotations = map[string]string{"test.dcp.microsoft.com/reconcile": "monitor-after-verification"}
					return server.Client.Update(updateCtx, current)
				}))
				recovered := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerImagePhaseFailed ||
						(current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady && inspector.injected.Load() == 4), nil
				})
				require.Equal(t, apiv2.PhysicalContainerImagePhaseReady, recovered.Status.Phase)
				require.Equal(t, ready.Status.ImageID, recovered.Status.ImageID)
				require.Equal(t, int32(4), inspector.injected.Load())
				require.Equal(t, 1, runtime.PullImageCallCount("retry-source"))
			} else {
				require.Equal(t, int32(3), inspector.injected.Load())
			}
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

// Verifies that each runtime inspection gets a fresh bounded backoff window, allowing two
// four-failure bursts to recover without reacquiring the verified image.
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
	waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
	})
	for attempt := 1; attempt <= 2; attempt++ {
		successes := inspector.successful.Load()
		inspector.remaining.Store(4)
		require.NoError(t, retryOnConflictEx(ctx, server.Client, image.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainerImage) error {
			current.Annotations = map[string]string{"test-inspection": fmt.Sprint(attempt)}
			return server.Client.Update(updateCtx, current)
		}))
		recovered := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
			return current.Status.Phase == apiv2.PhysicalContainerImagePhaseFailed ||
				(current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady && inspector.injected.Load() == int32(4*attempt) &&
					inspector.successful.Load() > successes), nil
		})
		require.Equal(t, apiv2.PhysicalContainerImagePhaseReady, recovered.Status.Phase)
		require.Equal(t, imageID, recovered.Status.ImageID)
	}
	require.Equal(t, int32(8), inspector.injected.Load())
	require.Equal(t, 1, runtime.PullImageCallCount("existing-retry-image"))
}
