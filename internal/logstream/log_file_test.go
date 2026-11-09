/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logstream

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Verifies that LogFileName uses one naming contract for ordinary and startup output,
// preserves existing filenames, and distinguishes successive capture instances.
func TestLogFileName(t *testing.T) {
	for _, testCase := range []struct {
		source   LogStreamSource
		suffix   string
		expected string
	}{
		{"", "", "resource_out_uid"},
		{LogStreamSourceStdout, "", "resource_out_uid"},
		{LogStreamSourceStderr, "", "resource_err_uid"},
		{LogStreamSourceStdout, "instance", "resource_out_uid_instance"},
		{LogStreamSourceStderr, "instance", "resource_err_uid_instance"},
		{LogStreamSourceStartupStdout, "", "resource_startout_uid"},
		{LogStreamSourceStartupStderr, "", "resource_starterr_uid"},
	} {
		name, nameErr := LogFileName("resource", "uid", testCase.source, testCase.suffix)
		require.NoError(t, nameErr)
		require.Equal(t, testCase.expected, name)
	}
	_, invalidErr := LogFileName("resource", "uid", LogStreamSourceSystem, "")
	require.ErrorContains(t, invalidErr, "unknown capture log source")
}
