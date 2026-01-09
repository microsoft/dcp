// Copyright (c) Microsoft Corporation. All rights reserved.

package testutil

import (
	"fmt"
	"os"
	"path/filepath"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/randdata"
)

// Returns temporary directory root path for use in tests.
// Agents running tests in CI pipelines often require that tests use temporary directory that
// is different from what TEMP or TMPDIR environment variables point to. This function takes care of that.
func TestTempRoot() string {
	azdoTemp, found := os.LookupEnv("AGENT_TEMPDIRECTORY") // Azure DevOps pipeline
	if found {
		return azdoTemp
	}

	ghTemp, found := os.LookupEnv("RUNNER_TEMP") // GitHub Actions
	if found {
		return ghTemp
	}

	return os.TempDir()
}

// Returns a temporary directory path for use in tests.
// If session folder is set in the environment, returns that.
// Otherwise, returns the temporary directory root path.
func TestTempDir() string {
	sessionDir, found := os.LookupEnv(usvc_io.DCP_SESSION_FOLDER)
	if found {
		return sessionDir
	}

	return TestTempRoot()
}

// Creates a session directory for use in tests.
func CreateTestSessionDir() (string, error) {
	testRoot := TestTempRoot()

	suffix, randErr := randdata.MakeRandomString(8)
	if randErr != nil {
		return "", fmt.Errorf("failed to generate random suffix for session directory: %w", randErr)
	}
	dirName := fmt.Sprintf("usvc-test-%s", suffix)
	sessionDir := filepath.Join(testRoot, dirName)

	if err := os.MkdirAll(sessionDir, osutil.PermissionOnlyOwnerReadWriteTraverse); err != nil {
		return "", fmt.Errorf("failed to create session directory: %w", err)
	}

	return sessionDir, nil
}
