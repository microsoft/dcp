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

// Verifies that makeWslcCommand leaves platform process setup to the shared executor and preserves native command arguments.
func TestMakeWslcCommandUsesExecutorLaunchPolicy(t *testing.T) {
	t.Parallel()

	command := makeWslcCommand("container", "list")
	require.Nil(t, command.SysProcAttr)
	require.Equal(t, command.Path, command.Args[0])
	require.Equal(t, []string{"container", "list"}, command.Args[1:])
}

// Verifies that JSON-line decoding accepts empty listings and retains valid objects while reporting malformed lines.
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

// Verifies that JSON-array decoding preserves valid image objects when another element has an invalid field type.
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

// Verifies that WSLC network decoding accepts both string-encoded and ordinary JSON boolean values.
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
