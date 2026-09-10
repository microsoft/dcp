/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/process"
)

// Verifies that the child is redirected through the 'fork-process-exec' command on platforms
// that need it, and left alone everywhere else. The redirection is what cleans the Go runtime's
// SIGUSR1 disposition before the real program starts.
func TestUseExecShim(t *testing.T) {
	t.Parallel()

	childCmd := exec.Command("sh", "-c", "exit 0")
	childCmd.Env = []string{"EXISTING=value", "GODEBUG=gctrace=1,asyncpreemptoff=0,schedtrace=1000"}
	originalPath := childCmd.Path
	originalArgs := childCmd.Args
	originalEnv := childCmd.Env

	execShim, shimErr := useExecShim(childCmd)
	require.NoError(t, shimErr)
	if execShim != nil {
		defer execShim.close()
	}

	if !process.NeedsExecSignalDispositionWorkaround() {
		require.Nil(t, execShim, "no handshake is needed on this platform")
		require.Equal(t, originalPath, childCmd.Path, "the command should not be redirected on this platform")
		require.Equal(t, originalArgs, childCmd.Args, "the arguments should not be rewritten on this platform")
		require.Equal(t, originalEnv, childCmd.Env, "the environment should not be rewritten on this platform")
		return
	}

	dcpPath, dcpPathErr := os.Executable()
	require.NoError(t, dcpPathErr)

	expectedArgs := append(
		[]string{
			dcpPath,
			ForkProcessExecCmdName,
			"--" + execPathFlagName,
			originalPath,
			"--",
		},
		originalArgs...,
	)

	require.Equal(t, dcpPath, childCmd.Path, "the command should run the current executable")
	require.Equal(t, expectedArgs, childCmd.Args, "the original program and arguments should be passed to the shim")
	require.Equal(t, originalEnv, childCmd.Env, "the target environment should not be rewritten")

	require.NotNil(t, execShim, "the shim should report whether the exec succeeded")
	require.Len(t, childCmd.ExtraFiles, 1, "the status descriptor should be passed to the shim")
}

// Verifies that a command that could not be resolved is left untouched, so that starting it
// reports the original lookup failure instead of one produced by the shim.
func TestUseExecShimLeavesUnresolvedCommand(t *testing.T) {
	t.Parallel()

	childCmd := exec.Command("dcp-command-that-does-not-exist")
	require.Error(t, childCmd.Err, "the test requires a command that cannot be resolved")

	originalPath := childCmd.Path
	originalArgs := childCmd.Args

	execShim, shimErr := useExecShim(childCmd)
	require.NoError(t, shimErr)
	require.Nil(t, execShim, "an unresolved command should not be redirected through the shim")

	require.Equal(t, originalPath, childCmd.Path, "an unresolved command should not be redirected")
	require.Equal(t, originalArgs, childCmd.Args, "an unresolved command should not have its arguments rewritten")
}

// Verifies that the handshake reports a successful exec, which the shim signals by closing the
// status descriptor without writing to it.
func TestExecShimHandshakeReportsSuccess(t *testing.T) {
	t.Parallel()

	handshake := newTestExecShimHandshake(t)

	// Stand in for the shim: a successful execve closes the inherited descriptor.
	require.NoError(t, handshake.statusW.Close())

	require.NoError(t, handshake.wait())
}

// Verifies that the errno the shim reports is surfaced to the caller. Without this the caller
// would be handed the PID of a process that never became the requested program.
func TestExecShimHandshakeReportsExecFailure(t *testing.T) {
	t.Parallel()

	handshake := newTestExecShimHandshake(t)

	// Stand in for the shim reporting a failed execve.
	writeForkProcessExecFailure(handshake.statusW, fmt.Errorf("executing child: %w", syscall.ENOENT))

	require.ErrorIs(t, handshake.wait(), syscall.ENOENT)
}

func newTestExecShimHandshake(t *testing.T) *execShimHandshake {
	t.Helper()

	statusR, statusW, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)

	handshake := &execShimHandshake{statusR: statusR, statusW: statusW}
	t.Cleanup(handshake.close)

	return handshake
}
