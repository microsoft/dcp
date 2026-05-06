/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcptun"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/testutil"
)

const clientProxyBaseImageDigestLabel = "com.microsoft.developer.usvc-dev.base-image-digest"

// Verifies that the client proxy image can be built successfully and has the expected tag prefix.
func TestClientProxyImageBuild(t *testing.T) {
	t.Parallel()

	const testTimeout = 20 * time.Second
	ctx, cancel := testutil.GetTestContext(t, testTimeout)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	co, coErr := ctrl_testutil.NewTestContainerOrchestrator(ctx, log, ctrl_testutil.TcoOptionNone)
	require.NoError(t, coErr, "Failed to create test container orchestrator")

	dcppaths.EnableTestPathProbing()

	path := filepath.Join(t.TempDir(), t.Name()+".imglist")
	defer func() { _ = os.Remove(path) }() // Best effort cleanup
	opts := dcptun.BuildClientProxyImageOptions{
		TimeoutOption:                 containers.TimeoutOption{Timeout: testTimeout / 2},
		MostRecentImageBuildsFilePath: path,
	}
	imageName, buildErr := dcptun.EnsureClientProxyImage(ctx, opts, co, log)
	require.NoError(t, buildErr, "Failed to ensure client proxy image")

	imgs, inspectErr := co.InspectImages(ctx, containers.InspectImagesOptions{
		Images: []string{imageName},
	})
	require.NoError(t, inspectErr, "Failed to inspect client proxy image")
	require.Len(t, imgs, 1, "Expected exactly one inspected image")
	img := imgs[0]
	require.Len(t, img.Tags, 1, "Image should have exactly one tag")

	const expectedTagPrefix = "dcptun_developer_ms:"
	require.True(t, strings.HasPrefix(img.Tags[0], expectedTagPrefix))
	require.Greater(t, len(img.Tags[0]), len(expectedTagPrefix), "Image tag should have a version suffix")
}

// Verifies that a pull failure for a locally available base image does not prevent building the client proxy image.
func TestClientProxyImageBuildFallsBackToLocalBaseImageWhenPullFails(t *testing.T) {
	t.Parallel()

	const testTimeout = 20 * time.Second
	ctx, cancel := testutil.GetTestContext(t, testTimeout)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	co, coErr := ctrl_testutil.NewTestContainerOrchestrator(ctx, log, ctrl_testutil.TcoOptionNone)
	require.NoError(t, coErr, "Failed to create test container orchestrator")

	dcppaths.EnableTestPathProbing()

	baseImage := testBaseImageName(t)
	baseImageID, basePullErr := co.PullImage(ctx, containers.PullImageOptions{Image: baseImage})
	require.NoError(t, basePullErr, "Failed to seed local base image")

	baseImages, baseInspectErr := co.InspectImages(ctx, containers.InspectImagesOptions{
		Images: []string{baseImageID},
	})
	require.NoError(t, baseInspectErr, "Failed to inspect seeded local base image")
	require.Len(t, baseImages, 1, "Expected exactly one seeded local base image")

	co.FailPullImage(errors.New("simulated base image pull failure"))

	path := filepath.Join(t.TempDir(), t.Name()+".imglist")
	defer func() { _ = os.Remove(path) }() // Best effort cleanup
	opts := dcptun.BuildClientProxyImageOptions{
		BaseImage:                     baseImage,
		TimeoutOption:                 containers.TimeoutOption{Timeout: testTimeout / 2},
		MostRecentImageBuildsFilePath: path,
	}
	imageName, buildErr := dcptun.EnsureClientProxyImage(ctx, opts, co, log)
	require.NoError(t, buildErr, "Failed to ensure client proxy image using local base image")

	imgs, inspectErr := co.InspectImages(ctx, containers.InspectImagesOptions{
		Images: []string{imageName},
	})
	require.NoError(t, inspectErr, "Failed to inspect client proxy image")
	require.Len(t, imgs, 1, "Expected exactly one inspected image")
	require.Equal(t, baseImages[0].Digest, imgs[0].Labels[clientProxyBaseImageDigestLabel])
}

// Verifies that a pull failure still fails when there is no local base image to use.
func TestClientProxyImageBuildFailsWhenPullFailsAndBaseImageIsUnavailable(t *testing.T) {
	t.Parallel()

	const testTimeout = 20 * time.Second
	ctx, cancel := testutil.GetTestContext(t, testTimeout)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	co, coErr := ctrl_testutil.NewTestContainerOrchestrator(ctx, log, ctrl_testutil.TcoOptionNone)
	require.NoError(t, coErr, "Failed to create test container orchestrator")

	dcppaths.EnableTestPathProbing()
	co.FailPullImage(errors.New("simulated base image pull failure"))

	path := filepath.Join(t.TempDir(), t.Name()+".imglist")
	defer func() { _ = os.Remove(path) }() // Best effort cleanup
	opts := dcptun.BuildClientProxyImageOptions{
		BaseImage:                     testBaseImageName(t),
		TimeoutOption:                 containers.TimeoutOption{Timeout: testTimeout / 2},
		MostRecentImageBuildsFilePath: path,
	}

	_, buildErr := dcptun.EnsureClientProxyImage(ctx, opts, co, log)
	require.Error(t, buildErr, "Expected client proxy image build to fail without a local base image")
	require.ErrorContains(t, buildErr, "no local copy was available")
}

// Verifies that the client proxy image is built once even if EnsureClientProxyImage() is called concurrently multiple times.
func TestConcurrentClientProxyImageBuild(t *testing.T) {
	t.Parallel()

	const testTimeout = 20 * time.Second
	ctx, cancel := testutil.GetTestContext(t, testTimeout)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	co, coErr := ctrl_testutil.NewTestContainerOrchestrator(ctx, log, ctrl_testutil.TcoOptionNone)
	require.NoError(t, coErr, "Failed to create test container orchestrator")

	dcppaths.EnableTestPathProbing()

	path := filepath.Join(t.TempDir(), t.Name()+".imglist")
	defer func() { _ = os.Remove(path) }() // Best effort cleanup
	opts := dcptun.BuildClientProxyImageOptions{
		TimeoutOption:                 containers.TimeoutOption{Timeout: testTimeout / 2},
		MostRecentImageBuildsFilePath: path,
	}

	const numConcurrentBuilds = 5
	wg := sync.WaitGroup{}
	wg.Add(numConcurrentBuilds)

	type imgBuildRes struct {
		imageName string
		buildErr  error
	}
	rchan := make(chan imgBuildRes, numConcurrentBuilds)

	for i := 0; i < numConcurrentBuilds; i++ {
		go func() {
			defer wg.Done()
			imageName, buildErr := dcptun.EnsureClientProxyImage(ctx, opts, co, log)
			rchan <- imgBuildRes{imageName, buildErr}
		}()
	}

	wg.Wait()
	var results []imgBuildRes
	for i := 0; i < numConcurrentBuilds; i++ {
		res := <-rchan
		require.NoError(t, res.buildErr, "Failed to ensure client proxy image in concurrent build")
		require.NotEmpty(t, res.imageName, "Image name should not be empty")
		results = append(results, res)
	}
	require.Truef(t, slices.All(results, func(r imgBuildRes) bool {
		return r.imageName == results[0].imageName
	}), "All concurrent builds should return the same image name, but results are: %v", results)
}

func testBaseImageName(t *testing.T) string {
	return "example.test/" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + ":latest"
}
