/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

func TestBuildInspectAndRemoveImageMethods(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		image := imageReference(t, "build-image")
		require.NoError(t, tracker.TrackImage(image))
		marker := containertest.UniqueName(t, "build-marker")
		contextDir, dockerfilePath := writeBuildContext(t, ensureTestImage(t, ctx, runtime), marker)

		buildErr := runtime.Orchestrator.BuildImage(ctx, containers.BuildImageOptions{
			ContainerBuildContext: &containers.ContainerBuildContext{
				Context:    contextDir,
				Dockerfile: dockerfilePath,
				Tags:       []string{image},
				Labels:     tracker.MapLabels(),
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
	})
}

// Verifies real image builds from source-file and raw archives, including a nested Dockerfile, ownership labels, and matching IID output.
// Running each built image must expose the expected archived marker contents.
func TestBuildImageFromArchive(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)
		baseImage := ensureTestImage(t, ctx, runtime)
		marker := containertest.UniqueName(t, "archive-marker")
		writer := usvc_io.NewTarWriter()
		now := time.Now()
		dockerfile := fmt.Sprintf("FROM %s\nCOPY marker /dcp-build-marker\n", baseImage)
		require.NoError(t, writer.WriteFile([]byte(dockerfile), "nested/Containerfile", 0, 0, 0644, now, now, now))
		require.NoError(t, writer.WriteFile([]byte(marker), "marker", 0, 0, 0644, now, now, now))
		buffer, archiveErr := writer.Buffer()
		require.NoError(t, archiveErr)
		contents := buffer.Bytes()

		for _, representation := range []string{"source", "raw"} {
			t.Run(representation, func(t *testing.T) {
				image := imageReference(t, "archive-image")
				require.NoError(t, tracker.TrackImage(image))
				archive := &containers.ContainerBuildContextArchive{}
				if representation == "raw" {
					archive.RawContents = base64.StdEncoding.EncodeToString(contents)
				} else {
					archive.Source = filepath.Join(t.TempDir(), "context.tar")
					archive.SHA256 = fmt.Sprintf("sha256:%x", sha256.Sum256(contents))
					require.NoError(t, usvc_io.WriteFile(archive.Source, contents, osutil.PermissionOnlyOwnerReadWrite))
				}
				iidFile := filepath.Join(t.TempDir(), "image.iid")
				buildErr := runtime.Orchestrator.BuildImage(ctx, containers.BuildImageOptions{
					IidFile: iidFile,
					ContainerBuildContext: &containers.ContainerBuildContext{
						ContextArchive: archive,
						Dockerfile:     "nested/Containerfile",
						Tags:           []string{image},
						Labels:         tracker.MapLabels(),
					},
				})
				require.NoError(t, buildErr)
				imageID, iidErr := containers.ReadImageIDFile(iidFile)
				require.NoError(t, iidErr)
				inspected, inspectErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{image}})
				require.NoError(t, inspectErr)
				require.Len(t, inspected, 1)
				require.Equal(t, imageID, inspected[0].Id)
				require.Equal(t, tracker.RunID(), inspected[0].Labels[containertest.TestRunLabel])
				stdout, stderr := runImageAndCapture(t, ctx, runtime, tracker, "archive-container", image, []string{"cat", "/dcp-build-marker"})
				require.Equal(t, marker, stdout)
				require.Empty(t, stderr)
			})
		}
	})
}

func TestApplyImageLayersMethod(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		baseImage := ensureTestImage(t, ctx, runtime)
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
			Labels: tracker.MapLabels(),
			Tag:    image,
		})
		require.NoError(t, applyErr)
		require.Equal(t, image, imageRef)

		inspected, inspectErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{image},
		})
		require.NoError(t, inspectErr)
		require.Len(t, inspected, 1)
		require.NotEmpty(t, inspected[0].Id)
		for key, value := range tracker.MapLabels() {
			require.Equal(t, value, inspected[0].Labels[key], "label %q", key)
		}

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
