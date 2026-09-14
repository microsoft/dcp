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

func TestDecodeJSONLinesAllowsEmptyListingsAndPreservesValidLines(t *testing.T) {
	t.Parallel()

	empty, emptyErr := decodeJSONLines[wslcListedVolume](bytes.NewBuffer(nil))
	require.NoError(t, emptyErr)
	require.Empty(t, empty)

	decoded, decodeErr := decodeJSONLines[wslcListedVolume](bytes.NewBufferString(
		"{\"Name\":\"first\"}\nnot-json\n{\"Name\":\"second\"}\n",
	))
	require.ErrorIs(t, decodeErr, containers.ErrUnmarshalling)
	require.Equal(t, []wslcListedVolume{{Name: "first"}, {Name: "second"}}, decoded)
}

func TestDecodeJSONArrayPreservesValidObjects(t *testing.T) {
	t.Parallel()

	decoded, decodeErr := decodeJSONArray[wslcInspectedImage](bytes.NewBufferString(
		`[{"Id":"sha256:first"},{"Id":42},{"Id":"sha256:second"}]`,
	))

	require.ErrorIs(t, decodeErr, containers.ErrUnmarshalling)
	require.Len(t, decoded, 2)
	require.Equal(t, "sha256:first", decoded[0].ID)
	require.Equal(t, "sha256:second", decoded[1].ID)
}

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

func TestParseSingleIdentifierRejectsEmptyAndAmbiguousOutput(t *testing.T) {
	t.Parallel()

	identifier, identifierErr := parseSingleIdentifier(bytes.NewBufferString("\r\n id-value \r\n"))
	require.NoError(t, identifierErr)
	require.Equal(t, "id-value", identifier)

	_, emptyErr := parseSingleIdentifier(bytes.NewBuffer(nil))
	require.Error(t, emptyErr)

	_, multipleErr := parseSingleIdentifier(bytes.NewBufferString("first\nsecond\n"))
	require.Error(t, multipleErr)
}

func TestWslcBoolAcceptsStringsAndBooleans(t *testing.T) {
	t.Parallel()

	decoded, decodeErr := decodeJSONLines[wslcListedNetwork](bytes.NewBufferString(
		"{\"ID\":\"one\",\"Name\":\"first\",\"IPv6\":\"true\",\"Internal\":false}\n",
	))

	require.NoError(t, decodeErr)
	require.Len(t, decoded, 1)
	require.True(t, bool(decoded[0].IPv6))
	require.False(t, bool(decoded[0].Internal))
}
