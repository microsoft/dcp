/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/testutil"
)

// imageLayerCacheTestOrchestrator is a minimal ContainerOrchestrator used to exercise the
// derived-image caching behavior in applyContainerImageLayers. Only the image-inspection,
// pull, and apply-layers methods are implemented; any other method panics (nil embedded
// interface) which keeps the test honest about the code path under test.
type imageLayerCacheTestOrchestrator struct {
	containers.ContainerOrchestrator

	mu         sync.Mutex
	images     map[string]containers.InspectedImage // ref/tag -> inspected image
	applyCalls []string                             // derived tags passed to ApplyImageLayers, in order
	pullCalls  int
}

func newImageLayerCacheTestOrchestrator() *imageLayerCacheTestOrchestrator {
	return &imageLayerCacheTestOrchestrator{images: map[string]containers.InspectedImage{}}
}

func (o *imageLayerCacheTestOrchestrator) addImage(ref string, img containers.InspectedImage) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.images[ref] = img
}

func (o *imageLayerCacheTestOrchestrator) InspectImages(_ context.Context, options containers.InspectImagesOptions) ([]containers.InspectedImage, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	var result []containers.InspectedImage
	var err error
	for _, ref := range options.Images {
		if img, ok := o.images[ref]; ok {
			result = append(result, img)
		} else {
			err = errors.Join(err, containers.ErrNotFound)
		}
	}
	return result, err
}

func (o *imageLayerCacheTestOrchestrator) PullImage(_ context.Context, options containers.PullImageOptions) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.pullCalls++
	img := containers.InspectedImage{Id: "pulled-" + options.Image, Tags: []string{options.Image}}
	o.images[options.Image] = img
	return img.Id, nil
}

func (o *imageLayerCacheTestOrchestrator) ApplyImageLayers(_ context.Context, options containers.ApplyImageLayersOptions) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.applyCalls = append(o.applyCalls, options.Tag)
	// Simulate the build registering the derived image so subsequent inspections hit the cache.
	o.images[options.Tag] = containers.InspectedImage{Id: "built-" + options.Tag, Tags: []string{options.Tag}}
	return options.Tag, nil
}

func newImageLayerRCD(image string, layers ...apiv1.ImageLayer) *runningContainerData {
	return &runningContainerData{
		runSpec: &apiv1.ContainerSpec{
			Image:       image,
			ImageLayers: layers,
		},
	}
}

// Verifies that a second apply with identical base image and layers reuses the cached derived
// image instead of rebuilding it.
func TestApplyContainerImageLayers_ReusesCachedDerivedImage(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	orch := newImageLayerCacheTestOrchestrator()
	orch.addImage("redis:7.2", containers.InspectedImage{Id: "base-redis-id", Tags: []string{"redis:7.2"}})

	r := &ContainerReconciler{orchestrator: orch}
	layer := apiv1.ImageLayer{Digest: "layer-digest-1", RawContents: "dGFy"}

	firstImage, firstErr := r.applyContainerImageLayers(ctx, newImageLayerRCD("redis:7.2", layer), nil, logr.Discard())
	require.NoError(t, firstErr)
	require.NotEqual(t, "redis:7.2", firstImage, "a derived image should be produced")
	require.Regexp(t, `^redis:7\.2-dcp-[0-9a-f]{64}$`, firstImage, "the derived tag should preserve the original tag as a prefix")

	secondImage, secondErr := r.applyContainerImageLayers(ctx, newImageLayerRCD("redis:7.2", layer), nil, logr.Discard())
	require.NoError(t, secondErr)

	require.Equal(t, firstImage, secondImage, "identical inputs must map to the same derived tag")
	require.Len(t, orch.applyCalls, 1, "the derived image should be built only once and reused on the second call")
}

// Verifies that changing layer content produces a different derived tag and triggers a rebuild.
func TestApplyContainerImageLayers_RebuildsWhenLayerContentChanges(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	orch := newImageLayerCacheTestOrchestrator()
	orch.addImage("redis:7.2", containers.InspectedImage{Id: "base-redis-id", Tags: []string{"redis:7.2"}})

	r := &ContainerReconciler{orchestrator: orch}

	firstImage, firstErr := r.applyContainerImageLayers(
		ctx, newImageLayerRCD("redis:7.2", apiv1.ImageLayer{Digest: "layer-digest-1"}), nil, logr.Discard())
	require.NoError(t, firstErr)

	secondImage, secondErr := r.applyContainerImageLayers(
		ctx, newImageLayerRCD("redis:7.2", apiv1.ImageLayer{Digest: "layer-digest-2"}), nil, logr.Discard())
	require.NoError(t, secondErr)

	require.NotEqual(t, firstImage, secondImage, "different layer content must map to different derived tags")
	require.Len(t, orch.applyCalls, 2, "a content change must trigger a rebuild")
}

// Verifies that the same layer applied on top of different base image content yields different
// derived tags, confirming the base image ID is folded into the cache key.
func TestApplyContainerImageLayers_BaseImageIdAffectsDerivedTag(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	layer := apiv1.ImageLayer{Digest: "shared-layer-digest"}

	orchA := newImageLayerCacheTestOrchestrator()
	orchA.addImage("redis:7.2", containers.InspectedImage{Id: "base-id-A", Tags: []string{"redis:7.2"}})
	rA := &ContainerReconciler{orchestrator: orchA}
	imageA, errA := rA.applyContainerImageLayers(ctx, newImageLayerRCD("redis:7.2", layer), nil, logr.Discard())
	require.NoError(t, errA)

	orchB := newImageLayerCacheTestOrchestrator()
	orchB.addImage("redis:7.2", containers.InspectedImage{Id: "base-id-B", Tags: []string{"redis:7.2"}})
	rB := &ContainerReconciler{orchestrator: orchB}
	imageB, errB := rB.applyContainerImageLayers(ctx, newImageLayerRCD("redis:7.2", layer), nil, logr.Discard())
	require.NoError(t, errB)

	require.NotEqual(t, imageA, imageB, "a change in the base image content must change the derived tag")
}

// Verifies that layers baked at creation time (passed as extraLayers) are folded into the cache
// key, and that the cache hits across calls when the baked content is unchanged.
func TestApplyContainerImageLayers_ExtraLayersParticipateInCacheKey(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	orch := newImageLayerCacheTestOrchestrator()
	orch.addImage("postgres:16", containers.InspectedImage{Id: "base-pg-id", Tags: []string{"postgres:16"}})

	r := &ContainerReconciler{orchestrator: orch}
	extra := []apiv1.ImageLayer{{Digest: "cert-layer-digest"}}

	firstImage, firstErr := r.applyContainerImageLayers(ctx, newImageLayerRCD("postgres:16"), extra, logr.Discard())
	require.NoError(t, firstErr)

	secondImage, secondErr := r.applyContainerImageLayers(ctx, newImageLayerRCD("postgres:16"), extra, logr.Discard())
	require.NoError(t, secondErr)

	require.Equal(t, firstImage, secondImage)
	require.Len(t, orch.applyCalls, 1, "unchanged baked layers should hit the cache on the second call")

	differentExtra := []apiv1.ImageLayer{{Digest: "cert-layer-digest-rotated"}}
	thirdImage, thirdErr := r.applyContainerImageLayers(ctx, newImageLayerRCD("postgres:16"), differentExtra, logr.Discard())
	require.NoError(t, thirdErr)

	require.NotEqual(t, firstImage, thirdImage, "rotated baked content must produce a new derived tag")
	require.Len(t, orch.applyCalls, 2)
}

// Verifies the derived tag format preserves the base repository and original tag as a prefix across
// the supported image reference forms (plain, registry-with-port, and digest-only).
func TestApplyContainerImageLayers_DerivedTagFormat(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	layer := apiv1.ImageLayer{Digest: "layer-digest"}

	cases := []struct {
		name        string
		image       string
		tagDigest   string // digest portion to register the base image under, if any
		wantPattern string
	}{
		{
			name:        "plain repository and tag",
			image:       "redis:7.2",
			wantPattern: `^redis:7\.2-dcp-[0-9a-f]{64}$`,
		},
		{
			name:        "registry with port and tag",
			image:       "localhost:5000/redis:7.2",
			wantPattern: `^localhost:5000/redis:7\.2-dcp-[0-9a-f]{64}$`,
		},
		{
			name:        "digest-only reference has no original tag to preserve",
			image:       "redis@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			wantPattern: `^redis:dcp-[0-9a-f]{64}$`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := newImageLayerCacheTestOrchestrator()
			orch.addImage(tc.image, containers.InspectedImage{Id: "base-id-" + tc.name, Tags: []string{tc.image}})
			r := &ContainerReconciler{orchestrator: orch}

			derived, err := r.applyContainerImageLayers(ctx, newImageLayerRCD(tc.image, layer), nil, logr.Discard())
			require.NoError(t, err)
			require.Regexp(t, tc.wantPattern, derived)
		})
	}
}
