/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logstream

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Verifies that StartupLogFileName preserves resource identity and separates startup output sources.
func TestStartupLogFileName(t *testing.T) {
	stdout, stdoutErr := StartupLogFileName("image", "uid", LogStreamSourceStartupStdout)
	require.NoError(t, stdoutErr)
	require.Equal(t, "image_startout_uid", stdout)
	stderr, stderrErr := StartupLogFileName("image", "uid", LogStreamSourceStartupStderr)
	require.NoError(t, stderrErr)
	require.Equal(t, "image_starterr_uid", stderr)
	_, invalidErr := StartupLogFileName("image", "uid", LogStreamSourceSystem)
	require.ErrorContains(t, invalidErr, "unknown startup log source")
}
