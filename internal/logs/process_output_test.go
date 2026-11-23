/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/osutil"
)

// Verifies that persistent process output uses the configured base directory without surrounding whitespace.
func TestPersistentExecutableOutputBaseDirCanBeConfigured(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "custom-peo")
	t.Setenv(DCP_PERSISTENT_EXECUTABLE_OUTPUT_DIR, " "+outputDir+" ")
	require.Equal(t, outputDir, persistentExecutableOutputBaseDir())
}

// Verifies that an empty persistent output override retains the V1 default directory.
func TestPersistentExecutableOutputBaseDirDefaultsForEmptyEnvVar(t *testing.T) {
	t.Setenv(DCP_PERSISTENT_EXECUTABLE_OUTPUT_DIR, " ")
	require.Equal(t, filepath.Join(os.TempDir(), persistentExecutableOutputDirName), persistentExecutableOutputBaseDir())
}

// Verifies that persistent process output uses an absolute path and isolates elevated callers.
func TestPersistentProcessOutputDirectorySeparatesElevation(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "output")
	t.Setenv(DCP_PERSISTENT_EXECUTABLE_OUTPUT_DIR, outputDir)
	isAdmin, adminErr := osutil.IsAdmin()
	require.NoError(t, adminErr)
	if isAdmin {
		outputDir += "-admin"
	}
	directory, directoryErr := PersistentProcessOutputDirectory()
	require.NoError(t, directoryErr)
	require.Equal(t, outputDir, directory)
	require.True(t, filepath.IsAbs(directory))
}
