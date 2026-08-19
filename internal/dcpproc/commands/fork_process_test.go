/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/process"
)

// Verifies that the child is redirected through the 'fork-process-exec' command on platforms
// that need it, and left alone everywhere else. The redirection is what clears the Go runtime's
// signal handler flags before the real program starts, so losing it reintroduces crashes in
// child runtimes that inspect those flags.
func TestUseExecShim(t *testing.T) {
	t.Parallel()

	childCmd := exec.Command("sh", "-c", "exit 0")
	originalPath := childCmd.Path
	originalArgs := childCmd.Args

	require.NoError(t, useExecShim(childCmd))

	if !process.SignalDispositionsLeakToChildren() {
		require.Equal(t, originalPath, childCmd.Path, "the command should not be redirected on this platform")
		require.Equal(t, originalArgs, childCmd.Args, "the arguments should not be rewritten on this platform")
		return
	}

	dcpPath, dcpPathErr := os.Executable()
	require.NoError(t, dcpPathErr)

	expectedArgs := append(
		[]string{dcpPath, ForkProcessExecCmdName, "--" + execPathFlagName, originalPath, "--"},
		originalArgs...,
	)

	require.Equal(t, dcpPath, childCmd.Path, "the command should run the current executable")
	require.Equal(t, expectedArgs, childCmd.Args, "the original program and arguments should be passed to the shim")
}

// Verifies that a command that could not be resolved is left untouched, so that starting it
// reports the original lookup failure instead of one produced by the shim.
func TestUseExecShimLeavesUnresolvedCommand(t *testing.T) {
	t.Parallel()

	childCmd := exec.Command("dcp-command-that-does-not-exist")
	require.Error(t, childCmd.Err, "the test requires a command that cannot be resolved")

	originalPath := childCmd.Path
	originalArgs := childCmd.Args

	require.NoError(t, useExecShim(childCmd))

	require.Equal(t, originalPath, childCmd.Path, "an unresolved command should not be redirected")
	require.Equal(t, originalArgs, childCmd.Args, "an unresolved command should not have its arguments rewritten")
}
