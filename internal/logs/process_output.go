/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/dcp/pkg/osutil"
)

const (
	DCP_PERSISTENT_EXECUTABLE_OUTPUT_DIR = "DCP_PERSISTENT_EXECUTABLE_OUTPUT_DIR"
	persistentExecutableOutputDirName    = "dcp-peo"
)

// PersistentProcessOutputDirectory resolves a session-independent output directory with separate elevated storage.
func PersistentProcessOutputDirectory() (string, error) {
	isAdmin, adminErr := osutil.IsAdmin()
	if adminErr != nil {
		return "", fmt.Errorf("could not determine current elevation: %w", adminErr)
	}
	baseDir, absolutePathErr := filepath.Abs(persistentExecutableOutputBaseDir())
	if absolutePathErr != nil {
		return "", fmt.Errorf("could not resolve persistent process output directory: %w", absolutePathErr)
	}
	if isAdmin {
		return baseDir + "-admin", nil
	}
	return baseDir, nil
}

func persistentExecutableOutputBaseDir() string {
	if outputDir, found := os.LookupEnv(DCP_PERSISTENT_EXECUTABLE_OUTPUT_DIR); found && strings.TrimSpace(outputDir) != "" {
		return strings.TrimSpace(outputDir)
	}
	return filepath.Join(os.TempDir(), persistentExecutableOutputDirName)
}
