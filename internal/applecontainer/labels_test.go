/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package applecontainer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEncodeLabelValueLeavesPlainValuesUntouched(t *testing.T) {
	for _, value := range []string{"", "dev", "6379/tcp", "a,b,c", "host.docker.internal", "line1\nline2"} {
		require.Equal(t, value, encodeLabelValue(value), "values without '=' should be passed through unchanged")
	}
}

func TestEncodeLabelValueEncodesValuesContainingEquals(t *testing.T) {
	encoded := encodeLabelValue("type=volume,src=redis-data")
	require.NotContains(t, encoded, "=", "the encoded value must not contain '=' (the Apple CLI rejects it)")
	require.True(t, strings.HasPrefix(encoded, appleEncodedLabelPrefix))
}

func TestLabelValueRoundTrips(t *testing.T) {
	for _, value := range []string{
		"type=volume,src=redis.apphost-12ca8fe773-redis-data",
		"type=volume,src=a\ntype=volume,src=b",
		"plain-value",
		"k=v=w",
		"",
	} {
		require.Equal(t, value, decodeLabelValue(encodeLabelValue(value)))
	}
}

func TestDecodeLabelValueIgnoresUnencodedValues(t *testing.T) {
	// A value that was never encoded (no sentinel prefix) must be returned unchanged, even if it
	// happens to contain characters used by the encoding.
	require.Equal(t, "some/plain.value-123", decodeLabelValue("some/plain.value-123"))
}

func TestDecodeLabelsDecodesOnlyEncodedEntries(t *testing.T) {
	in := map[string]string{
		"com.microsoft.developer.usvc-dev.mountsLabel": encodeLabelValue("type=volume,src=data"),
		"com.microsoft.developer.usvc-dev.build":       "dev",
		"image.label.from.elsewhere":                   "unrelated=value-we-did-not-set",
	}

	out := decodeLabels(in)

	require.Equal(t, "type=volume,src=data", out["com.microsoft.developer.usvc-dev.mountsLabel"])
	require.Equal(t, "dev", out["com.microsoft.developer.usvc-dev.build"])
	// Not encoded by us -> left untouched.
	require.Equal(t, "unrelated=value-we-did-not-set", out["image.label.from.elsewhere"])
}

func TestLabelArgNeverContainsEqualsInValue(t *testing.T) {
	arg := labelArg("com.microsoft.developer.usvc-dev.mountsLabel", "type=volume,src=x")
	key, value, found := strings.Cut(arg, "=")
	require.True(t, found)
	require.Equal(t, "com.microsoft.developer.usvc-dev.mountsLabel", key)
	require.NotContains(t, value, "=")
}
