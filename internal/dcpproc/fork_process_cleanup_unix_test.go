//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that fork-process honors tree/root cleanup on owner exit and cancellation,
// and escalates an uncooperative root without stopping independently grouped descendants.
func TestForkProcessCleanupTreePolicy(t *testing.T) {
	for _, cancellation := range []bool{false, true} {
		for _, cleanupMode := range []string{"disabled", "tree", "root"} {
			for _, escalation := range []bool{false, true} {
				if escalation && (cancellation || cleanupMode != "root") {
					continue
				}
				t.Run(fmt.Sprintf("%s,cancel=%t,escalate=%t", cleanupMode, cancellation, escalation), func(t *testing.T) {
					t.Parallel()
					testCtx, testCancel := testutil.GetTestContext(t, 0)
					defer testCancel()
					ownerCmd, owner := startCleanupOwner(t, testCtx)
					readyPath := filepath.Join(t.TempDir(), "child-ready")
					childArgs := []string{"--delay=2m", "--child-spec=1", "--fork-children", "--ready-file=" + readyPath}
					if escalation {
						childArgs = append(childArgs, "--ignore-sigterm")
					}
					fork := startCleanupFork(t, testCtx, owner, cleanupMode, childArgs...)
					child := waitCleanupForkChild(t, testCtx, fork)
					waitCleanupFile(t, testCtx, readyPath)
					tree, treeErr := process.GetProcessTree(testCtx, child)
					require.NoError(t, treeErr)
					require.Len(t, tree, 2)
					t.Cleanup(func() {
						for _, handle := range tree {
							stopCleanupProcess(testCtx, handle)
						}
					})
					startedAt := time.Now()
					if cancellation {
						require.NoError(t, fork.command.Process.Signal(syscall.SIGTERM))
					} else {
						require.NoError(t, ownerCmd.Process.Kill())
					}
					runErr := waitCleanupForkExit(t, testCtx, fork)
					if cancellation {
						var exitErr *exec.ExitError
						require.ErrorAs(t, runErr, &exitErr)
						require.NotZero(t, exitErr.ExitCode())
					} else {
						require.NoError(t, runErr, fork.output.Stderr())
					}
					if escalation {
						require.GreaterOrEqual(t, time.Since(startedAt), 14*time.Second)
					}
					executor := process.NewOSExecutor(logr.Discard())
					defer executor.Dispose()
					if cleanupMode == "disabled" {
						require.NoError(t, executor.CheckProcessRunning(child))
					} else {
						require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(child)))
					}
					if cleanupMode == "tree" {
						require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(tree[1])))
					} else {
						require.NoError(t, executor.CheckProcessRunning(tree[1]))
					}
					if cancellation {
						require.NoError(t, executor.CheckProcessRunning(owner))
					}
					require.Equal(t, strconv.FormatInt(int64(child.Pid), 10)+"\n", fork.stdout())
				})
			}
		}
	}
}
