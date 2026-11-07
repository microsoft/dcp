/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that runControllers exits successfully, without validating the (missing) kubeconfig,
// when the monitored owner process has already exited.
// Not parallel because monitor and kubeconfig flag values are process-wide globals.
func TestRunControllersExitsCleanlyWhenMonitoredOwnerAlreadyExited(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	delayToolPath, toolPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, toolPathErr)

	ownerCmd := exec.CommandContext(testCtx, delayToolPath, "--delay=100ms")
	require.NoError(t, ownerCmd.Start())
	ownerHandle, ownerHandleErr := process.ProcessHandleFromCmd(ownerCmd)
	require.NoError(t, ownerHandleErr)
	require.NoError(t, ownerCmd.Wait())

	runErr := executeRunControllers(t, testCtx, ownerHandle)
	require.NoError(t, runErr, "run-controllers should exit cleanly when the monitored owner is gone")
}

// Verifies that runControllers reports an error for a missing kubeconfig when the monitored owner process is alive.
// Not parallel because monitor and kubeconfig flag values are process-wide globals.
func TestRunControllersFailsOnMissingKubeconfigWhenMonitoredOwnerAlive(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	delayToolPath, toolPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, toolPathErr)

	ownerCmd := exec.CommandContext(testCtx, delayToolPath, "--delay=30s")
	require.NoError(t, ownerCmd.Start())
	t.Cleanup(func() {
		_ = ownerCmd.Process.Kill()
		_ = ownerCmd.Wait()
	})
	ownerHandle, ownerHandleErr := process.ProcessHandleFromCmd(ownerCmd)
	require.NoError(t, ownerHandleErr)

	runErr := executeRunControllers(t, testCtx, ownerHandle)
	require.ErrorContains(t, runErr, "kubeconfig file does not exist")
}

func executeRunControllers(t *testing.T, ctx context.Context, owner process.ProcessHandle) error {
	missingKubeconfig := filepath.Join(t.TempDir(), "missing-kubeconfig")

	cmd := NewRunControllersCommand(logger.New(t.Name()))
	cmd.RunE = runControllers(testutil.NewLogForTesting(t.Name()))
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"--monitor", strconv.FormatInt(int64(owner.Pid), 10),
		"--monitor-identity-time", owner.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--kubeconfig", missingKubeconfig,
	})

	return cmd.ExecuteContext(ctx)
}
