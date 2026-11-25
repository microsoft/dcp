/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcppaths

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

// Verifies that test path probing honors OUTPUT_BIN and rejects missing or directory-valued executables.
func TestProbeForDcpDirUsesTestOutputBin(t *testing.T) {
	outputBin := t.TempDir()
	t.Setenv("OUTPUT_BIN", outputBin)
	previous := enableTestPathProbing.Swap(true)
	t.Cleanup(func() { enableTestPathProbing.Store(previous) })

	_, missingErr := probeForDcpDir()
	require.ErrorIs(t, missingErr, os.ErrNotExist)

	dcpPath := filepath.Join(outputBin, dcpExeName)
	require.NoError(t, os.Mkdir(dcpPath, 0700))
	_, directoryErr := probeForDcpDir()
	require.ErrorContains(t, directoryErr, "is a directory")
	require.NoError(t, os.Remove(dcpPath))

	file, createErr := usvc_io.CreateNewFile(dcpPath, 0600)
	require.NoError(t, createErr)
	require.NoError(t, file.Close())
	result, probeErr := probeForDcpDir()
	require.NoError(t, probeErr)
	require.Equal(t, outputBin, result)
}
