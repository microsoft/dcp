/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

type pulledImageInspector struct {
	calls           int
	failures        int
	results         []InspectedImage
	inspectionError error
}

func (inspector *pulledImageInspector) InspectImages(_ context.Context, _ InspectImagesOptions) ([]InspectedImage, error) {
	inspector.calls++
	if inspector.calls <= inspector.failures {
		return inspector.results, inspector.inspectionError
	}
	return []InspectedImage{{Id: "sha256:resolved"}}, nil
}

// Verifies that InspectPulledImage retries transient errors, NotFound, and incomplete identities,
// returning the immutable ID once inspection succeeds.
func TestInspectPulledImageRetriesUntilIdentityIsAvailable(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name            string
		results         []InspectedImage
		inspectionError error
	}{
		{name: "inspection error", inspectionError: errors.New("temporary inspection failure")},
		{name: "not found", inspectionError: ErrNotFound},
		{name: "empty result"},
		{name: "empty ID", results: []InspectedImage{{}}},
		{name: "ambiguous result", results: []InspectedImage{{Id: "first"}, {Id: "second"}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			inspector := &pulledImageInspector{failures: 1, results: testCase.results, inspectionError: testCase.inspectionError}
			imageID, inspectErr := InspectPulledImage(ctx, inspector, "image:latest")
			require.NoError(t, inspectErr)
			require.Equal(t, "sha256:resolved", imageID)
			require.Equal(t, 2, inspector.calls)
		})
	}
}

// Verifies that InspectPulledImage stops retrying within its backoff budget and preserves
// the underlying inspection error rather than returning an unverified identity.
func TestInspectPulledImageExhaustsRetryBudget(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	inspector := &pulledImageInspector{failures: 100, inspectionError: ErrNotFound}
	imageID, inspectErr := InspectPulledImage(ctx, inspector, "image:latest")
	require.Empty(t, imageID)
	require.ErrorIs(t, inspectErr, ErrNotFound)
	require.Greater(t, inspector.calls, 1)
	require.LessOrEqual(t, inspector.calls, 6)
}

// Verifies that InspectPulledImage respects an already canceled context without issuing
// an inspection request.
func TestInspectPulledImageSkipsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	cancel()
	inspector := &pulledImageInspector{}
	imageID, inspectErr := InspectPulledImage(ctx, inspector, "image:latest")
	require.Empty(t, imageID)
	require.ErrorIs(t, inspectErr, context.Canceled)
	require.Zero(t, inspector.calls)
}

// Verifies that InspectPulledImage returns a recognized runtime outage immediately rather
// than repeatedly inspecting an unavailable runtime.
func TestInspectPulledImageDefersRuntimeOutage(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	inspector := &pulledImageInspector{failures: 100, inspectionError: ErrRuntimeNotHealthy}
	imageID, inspectErr := InspectPulledImage(ctx, inspector, "image:latest")
	require.Empty(t, imageID)
	require.ErrorIs(t, inspectErr, ErrRuntimeNotHealthy)
	require.Equal(t, 1, inspector.calls)
}

// Verifies that ImageReferenceWithDefaultTag equates implicit latest tags while preserving
// explicit tags, registry ports, digest references, and runtime-specific short-name resolution.
func TestImageReferenceWithDefaultTag(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct{ reference, expected string }{
		{"example.test/team/image", "example.test/team/image:latest"},
		{"example.test/team/image:latest", "example.test/team/image:latest"},
		{"example.test:5000/team/image", "example.test:5000/team/image:latest"},
		{"example.test:5000/team/image:v2", "example.test:5000/team/image:v2"},
		{"example.test/team/image@sha256:1234", "example.test/team/image@sha256:1234"},
		{"image", "image:latest"},
	} {
		require.Equal(t, testCase.expected, ImageReferenceWithDefaultTag(testCase.reference))
	}
}

func TestRemoveImagesSequentiallyReturnsRequestedIdentifiers(t *testing.T) {
	t.Parallel()

	requested := []string{"example.test/image:one", "sha256:0123456789"}
	var calls []string

	removed, removeErr := RemoveImagesSequentially(
		t.Context(),
		RemoveImagesOptions{Images: requested, Force: true},
		func(_ context.Context, image string, force bool) error {
			require.True(t, force)
			calls = append(calls, image)
			return nil
		},
	)

	require.NoError(t, removeErr)
	require.Equal(t, requested, calls)
	require.Equal(t, requested, removed)
}

func TestRemoveImagesSequentiallyReportsPartialSuccess(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("image is in use")
	removed, removeErr := RemoveImagesSequentially(
		t.Context(),
		RemoveImagesOptions{Images: []string{"first", "second", "third"}},
		func(_ context.Context, image string, _ bool) error {
			if image == "second" {
				return expectedErr
			}
			return nil
		},
	)

	require.Equal(t, []string{"first", "third"}, removed)
	require.ErrorIs(t, removeErr, expectedErr)
	require.ErrorIs(t, removeErr, ErrIncomplete)
	require.ErrorContains(t, removeErr, `removing image "second"`)
}

func TestRemoveImagesSequentiallyValidatesOptions(t *testing.T) {
	t.Parallel()

	removed, removeErr := RemoveImagesSequentially(t.Context(), RemoveImagesOptions{}, func(context.Context, string, bool) error {
		return nil
	})
	require.Nil(t, removed)
	require.EqualError(t, removeErr, "must specify at least one image")

	removed, removeErr = RemoveImagesSequentially(
		t.Context(),
		RemoveImagesOptions{Images: []string{"image"}},
		nil,
	)
	require.Nil(t, removed)
	require.EqualError(t, removeErr, "image removal function cannot be nil")
}
