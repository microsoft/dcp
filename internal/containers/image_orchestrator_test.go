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
)

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
