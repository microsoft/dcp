/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
)

// Verifies that native missing-container, image, network, and volume diagnostics map to the shared not-found error.
func TestNormalizeCliErrorsRecognizesWslcMissingObjects(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		message string
		match   containers.ErrorMatch
	}{
		{
			name:    "container",
			message: "Container 'missing-container' not found.\n",
			match:   containerNotFoundMatch,
		},
		{
			name:    "image",
			message: "Image 'missing-image' not found.\n",
			match:   imageNotFoundMatch,
		},
		{
			name:    "network",
			message: "Network not found: 'missing-network'\n",
			match:   networkNotFoundMatch,
		},
		{
			name:    "volume",
			message: "Volume not found: 'missing-volume'\n",
			match:   volumeNotFoundMatch,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			normalizedErr := normalizeCliErrors(bytes.NewBufferString(testCase.message), testCase.match)
			require.ErrorIs(t, normalizedErr, containers.ErrNotFound)
			require.False(t, errors.Is(normalizedErr, containers.ErrUnmatched))
		})
	}
}

// Verifies that session/control failures mark WSLC unhealthy while unrelated registry connection failures do not.
func TestNormalizeCliErrorsRestrictsRuntimeHealthClassification(t *testing.T) {
	t.Parallel()

	registryErr := normalizeCliErrors(bytes.NewBufferString(
		"failed to connect to registry.example.test: connection refused\n",
	))
	require.ErrorIs(t, registryErr, containers.ErrUnmatched)
	require.NotErrorIs(t, registryErr, containers.ErrRuntimeNotHealthy)

	sessionManagerErr := normalizeCliErrors(bytes.NewBufferString(
		"failed to connect to WSLC session manager: connection refused\n",
	))
	require.ErrorIs(t, sessionManagerErr, containers.ErrRuntimeNotHealthy)
	require.NotErrorIs(t, sessionManagerErr, containers.ErrUnmatched)

	defaultSessionErr := normalizeCliErrors(bytes.NewBufferString(
		"default session is unavailable\n",
	))
	require.ErrorIs(t, defaultSessionErr, containers.ErrRuntimeNotHealthy)
}

// Verifies that asId trims surrounding whitespace, preserves embedded whitespace, and rejects empty or multiple identifiers.
func TestAsIdRejectsEmptyAndAmbiguousOutput(t *testing.T) {
	t.Parallel()

	identifier, identifierErr := asId(bytes.NewBufferString("\r\n id-value \r\n"))
	require.NoError(t, identifierErr)
	require.Equal(t, "id-value", identifier)

	embeddedWhitespace, whitespaceErr := asId(bytes.NewBufferString("id with spaces\tand tabs\n"))
	require.NoError(t, whitespaceErr)
	require.Equal(t, "id with spaces\tand tabs", embeddedWhitespace)

	_, emptyErr := asId(bytes.NewBuffer(nil))
	require.Error(t, emptyErr)

	_, multipleErr := asId(bytes.NewBufferString("first\nsecond\n"))
	require.Error(t, multipleErr)
}
