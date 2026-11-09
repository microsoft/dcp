/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logstream

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// Verifies that shared log options decode formatting and follow parameters and reject
// incompatible limits without changing the legacy startup source set.
func TestLogOptionsDecodeAndValidate(t *testing.T) {
	query := url.Values{
		"source": {"startup_stdout"}, "follow": {"true"}, "tail": {"2"},
		"timestamps": {"true"}, "line_numbers": {"true"},
	}
	options := &LogOptions{}
	require.NoError(t, UrlValuesToLogOptions(&query, options, nil))
	require.True(t, options.Follow)
	require.True(t, options.Timestamps)
	require.True(t, options.LineNumbers)
	require.Equal(t, int64(2), *options.Tail)
	require.NoError(t, options.Validate())
	limit := int64(1)
	options.Limit = &limit
	require.ErrorContains(t, options.Validate(), "follow mode")
	query.Set("tail", "not-a-number")
	require.ErrorContains(t, UrlValuesToLogOptions(&query, options, nil), "tail")
}
