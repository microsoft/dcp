/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that delay reports readiness with and without children, and does not pass its readiness file to children.
func TestDelayReportsReadinessAfterStartingChildren(t *testing.T) {
	t.Parallel()

	for _, childSpec := range []string{"", "1"} {
		t.Run("children="+childSpec, func(t *testing.T) {
			t.Parallel()
			testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
			defer testCancel()
			delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
			require.NoError(t, delayPathErr)
			readyPath := filepath.Join(t.TempDir(), "ready")
			cmd := exec.Command(delayPath, "--delay=60s", "--ready-file="+readyPath)
			if childSpec != "" {
				cmd.Args = append(cmd.Args, "--child-spec="+childSpec, "--couple-children")
			}

			executor := process.NewOSExecutor(logr.Discard())
			defer executor.Dispose()
			exitHandler := process.NewConcurrentProcessExitHandler()
			root, startWaiting, startErr := executor.StartProcess(
				testCtx, cmd, exitHandler, process.CreationFlagEnsureKillOnDispose, nil)
			require.NoError(t, startErr)
			startWaiting()
			defer func() {
				cleanupCtx, cleanupCancel := process.WithDetachedStopTimeout(testCtx)
				defer cleanupCancel()
				stopErr := executor.StopProcess(cleanupCtx, root)
				require.True(t, stopErr == nil || process.IsProcessGoneErr(stopErr), "could not stop delay: %v", stopErr)
				select {
				case <-exitHandler.Exited():
				case <-cleanupCtx.Done():
					t.Fatal("delay did not exit after cleanup")
				}
			}()

			readyErr := wait.PollUntilContextCancel(testCtx, 25*time.Millisecond, true, func(context.Context) (bool, error) {
				select {
				case <-exitHandler.Exited():
					return false, fmt.Errorf("delay exited before reporting readiness: %+v", exitHandler.ExitInfo())
				default:
				}
				_, statErr := os.Stat(readyPath)
				if errors.Is(statErr, os.ErrNotExist) {
					return false, nil
				}
				return statErr == nil, statErr
			})
			require.NoError(t, readyErr)

			tree, treeErr := process.GetProcessTree(testCtx, root)
			require.NoError(t, treeErr)
			expectedProcesses := 1
			if childSpec != "" {
				expectedProcesses++
			}
			require.Len(t, tree, expectedProcesses)
		})
	}
}

// Verifies that delay reports readiness-file creation errors without replacing an existing file.
func TestDelayRejectsInvalidReadinessFile(t *testing.T) {
	t.Parallel()

	for _, existingFile := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existingFile), func(t *testing.T) {
			t.Parallel()
			testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
			defer testCancel()
			delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
			require.NoError(t, delayPathErr)
			readyPath := filepath.Join(t.TempDir(), "ready")
			if existingFile {
				require.NoError(t, usvc_io.WriteFile(readyPath, []byte("existing"), osutil.PermissionOnlyOwnerReadWrite))
			} else {
				readyPath = filepath.Join(readyPath, "missing-directory")
			}

			output, runErr := exec.CommandContext(testCtx, delayPath, "--ready-file="+readyPath).CombinedOutput()
			require.Error(t, runErr)
			require.Contains(t, string(output), "could not create readiness file")
			if existingFile {
				contents, readErr := os.ReadFile(readyPath)
				require.NoError(t, readErr)
				require.Equal(t, "existing", string(contents))
			}
		})
	}
}

// Verifies that delay does not report readiness when starting its children fails.
func TestDelayDoesNotReportReadinessOnChildStartupFailure(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	readyPath := filepath.Join(t.TempDir(), "ready")

	output, runErr := exec.CommandContext(testCtx, delayPath, "--ready-file="+readyPath, "--child-spec=0").CombinedOutput()
	require.Error(t, runErr)
	require.Contains(t, string(output), "invalid child spec")
	_, statErr := os.Stat(readyPath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

// Verifies that delay retains its completion output and requested exit code without readiness reporting.
func TestDelayRetainsDefaultCompletion(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	cmd := exec.CommandContext(testCtx, delayPath, "--delay=1ms", "--exit-code=7")
	cmd.Dir = t.TempDir()

	output, runErr := cmd.Output()
	var exitErr *exec.ExitError
	require.ErrorAs(t, runErr, &exitErr)
	require.Equal(t, 7, exitErr.ExitCode())
	require.Contains(t, string(output), "Ran to completion")
	entries, readErr := os.ReadDir(cmd.Dir)
	require.NoError(t, readErr)
	require.Empty(t, entries)
}
