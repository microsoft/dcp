/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that controller-manager startup unwinds on cancellation and deadlines, tolerates late readiness, and preserves runnable errors and healthy startup.
func TestControllerManagerStartupCancellation(t *testing.T) {
	t.Parallel()

	toolName := "controller-manager-startup"
	if runtime.GOOS == "windows" {
		toolName += ".exe"
	}
	toolRoot, toolRootErr := osutil.FindRootFor(osutil.FileTarget, ".toolbin", toolName)
	require.NoError(t, toolRootErr, "run make test-prereqs before selective tests")
	toolPath := filepath.Join(toolRoot, ".toolbin", toolName)

	for _, scenario := range []string{
		"cancel-before-start", "cancel-during-start", "deadline-before-start", "late-ready", "healthy", "runnable-error",
	} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			sessionFolder, sessionFolderErr := testutil.CreateTestSessionDir()
			require.NoError(t, sessionFolderErr)
			log := testutil.NewLogWithResourceSinkForTesting(t.Name(), sessionFolder)
			serverInfo, serverErr := ctrl_testutil.StartApiServer(ctx, ctrl_testutil.ApiServerFlagsNone, log, sessionFolder)
			require.NoError(t, serverErr)
			t.Cleanup(serverInfo.Dispose)

			remaining := 3 * time.Minute
			if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
				remaining = min(remaining, time.Until(deadline))
			}
			// This process has no workloads or descendants; cancellation must kill a regressed spin immediately.
			command := exec.CommandContext(ctx, toolPath, "--kubeconfig", serverInfo.KubeconfigPath,
				"--scenario", scenario, "--timeout", remaining.String())
			output, runErr := command.CombinedOutput()
			require.NoError(t, runErr, "startup scenario %s failed: %s", scenario, output)
		})
	}
}
