/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/testutil/containertest"
)

func TestPullAndInspectImageMethods(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		// Fixed image references can be shared by overlapping test runs, so leave this cached.
		imageID, pullErr := runtime.Orchestrator.PullImage(ctx, containers.PullImageOptions{
			Image: pullImageReference,
		})
		require.NoError(t, pullErr)
		require.NotEmpty(t, imageID)

		byName, inspectNameErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{pullImageReference},
		})
		require.NoError(t, inspectNameErr)
		require.Len(t, byName, 1)
		require.NotEmpty(t, byName[0].Id)

		byID, inspectIDErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{imageID},
		})
		require.NoError(t, inspectIDErr)
		require.Len(t, byID, 1)
		require.Equal(t, byName[0].Id, byID[0].Id)
	})
}

func TestBuildInspectAndRemoveImageMethods(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		image := imageReference(t, "build-image")
		require.NoError(t, tracker.TrackImage(image))
		marker := containertest.UniqueName(t, "build-marker")
		contextDir, dockerfilePath := writeBuildContext(t, ensureBaseImage(t, ctx, runtime), marker)

		buildErr := runtime.Orchestrator.BuildImage(ctx, containers.BuildImageOptions{
			ContainerBuildContext: &containers.ContainerBuildContext{
				Context:    contextDir,
				Dockerfile: dockerfilePath,
				Tags:       []string{image},
				Labels:     tracker.Labels(),
			},
		})
		require.NoError(t, buildErr)

		inspected, inspectErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{image},
		})
		require.NoError(t, inspectErr)
		require.Len(t, inspected, 1)
		require.NotEmpty(t, inspected[0].Id)
		require.Contains(t, inspected[0].Tags, image)
		require.Equal(t, tracker.RunID(), inspected[0].Labels[containertest.TestRunLabel])

		stdout, stderr := runImageAndCapture(
			t,
			ctx,
			runtime,
			tracker,
			"run-built-image",
			image,
			[]string{"cat", "/dcp-build-marker"},
		)
		require.Equal(t, marker, stdout)
		require.Empty(t, stderr)

		removed, removeErr := runtime.Orchestrator.RemoveImages(ctx, containers.RemoveImagesOptions{
			Images: []string{image},
		})
		require.NoError(t, removeErr)
		require.Equal(t, []string{image}, removed)
		waitForImageAbsent(t, ctx, runtime.Orchestrator, image)
	})
}

func TestApplyImageLayersMethod(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		baseImage := ensureBaseImage(t, ctx, runtime)
		inspectedBase, inspectBaseErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{baseImage},
		})
		require.NoError(t, inspectBaseErr)
		require.Len(t, inspectedBase, 1)

		image := imageReference(t, "layered-image")
		require.NoError(t, tracker.TrackImage(image))
		marker := containertest.UniqueName(t, "layer-marker")
		imageRef, applyErr := runtime.Orchestrator.ApplyImageLayers(ctx, containers.ApplyImageLayersOptions{
			BaseImage: inspectedBase[0],
			Layers: []containers.ImageLayer{{
				Digest:      marker,
				RawContents: rawImageLayer(t, "dcp-layer-marker", marker),
			}},
			Tag: image,
		})
		require.NoError(t, applyErr)
		require.Equal(t, image, imageRef)

		inspected, inspectErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{image},
		})
		require.NoError(t, inspectErr)
		require.Len(t, inspected, 1)
		require.NotEmpty(t, inspected[0].Id)

		stdout, stderr := runImageAndCapture(
			t,
			ctx,
			runtime,
			tracker,
			"run-layered-image",
			image,
			[]string{"cat", "/dcp-layer-marker"},
		)
		require.Equal(t, marker, stdout)
		require.Empty(t, stderr)

		removed, removeErr := runtime.Orchestrator.RemoveImages(ctx, containers.RemoveImagesOptions{
			Images: []string{image},
		})
		require.NoError(t, removeErr)
		require.Equal(t, []string{image}, removed)
		waitForImageAbsent(t, ctx, runtime.Orchestrator, image)
	})
}
