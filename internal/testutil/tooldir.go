/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package testutil

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/microsoft/dcp/pkg/osutil"
)

// Returns the folder containing desired test tool (executable).
func GetTestToolDir(exeName string) (string, error) {
	if len(exeName) == 0 {
		return "", fmt.Errorf("empty test tool name")
	}

	if runtime.GOOS == "windows" && !strings.HasSuffix(exeName, ".exe") {
		exeName += ".exe"
	}

	rootDir, err := osutil.FindRootFor(osutil.FileTarget, ".toolbin", exeName)
	if err == nil {
		return filepath.Join(rootDir, ".toolbin"), nil
	} else {
		return "", fmt.Errorf("could not find '%s' test tool: %w", exeName, err)
	}
}

func GetTestToolPath(exeName string) (string, error) {
	dir, err := GetTestToolDir(exeName)
	if err != nil {
		return "", err
	}

	if runtime.GOOS == "windows" && !strings.HasSuffix(exeName, ".exe") {
		exeName += ".exe"
	}

	return filepath.Join(dir, exeName), nil
}

// GetTestContainerToolPath returns the path to a test tool built for execution inside a
// container. The exact filename is preserved instead of applying the host executable suffix.
func GetTestContainerToolPath(exeName string) (string, error) {
	if len(exeName) == 0 {
		return "", fmt.Errorf("empty test tool name")
	}

	rootDir, findRootErr := osutil.FindRootFor(osutil.FileTarget, ".toolbin", exeName)
	if findRootErr != nil {
		return "", fmt.Errorf("could not find '%s' test container tool: %w", exeName, findRootErr)
	}

	return filepath.Join(rootDir, ".toolbin", exeName), nil
}
