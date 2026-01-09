// Copyright (c) Microsoft Corporation. All rights reserved.

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
