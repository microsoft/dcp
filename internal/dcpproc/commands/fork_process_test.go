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
// that need it, and left alone everywhere else. The redirection is what clears the Go runtime's
// signal handler flags before the real program starts, so losing it reintroduces crashes in
// child runtimes that inspect those flags.
func TestUseExecShim(t *testing.T) {
	t.Parallel()

	childCmd := exec.Command("sh", "-c", "exit 0")
	const originalGoDebug = "gctrace=1,asyncpreemptoff=0,schedtrace=1000"
	childCmd.Env = []string{"EXISTING=value", goDebugEnvVar + "=" + originalGoDebug}
	originalPath := childCmd.Path
	originalArgs := childCmd.Args
	originalEnv := childCmd.Env

	execShim, shimErr := useExecShim(childCmd)
	require.NoError(t, shimErr)
	if execShim != nil {
		defer execShim.close()
	}

	if !process.SignalDispositionsLeakToChildren() {
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
			"--" + targetGoDebugFlagName + "=" + originalGoDebug,
			"--",
		},
		originalArgs...,
	)

	require.Equal(t, dcpPath, childCmd.Path, "the command should run the current executable")
	require.Equal(t, expectedArgs, childCmd.Args, "the original program and arguments should be passed to the shim")
	shimGoDebug, shimGoDebugSet := environmentVariable(childCmd.Env, goDebugEnvVar)
	require.True(t, shimGoDebugSet)
	require.Equal(t, "gctrace=1,schedtrace=1000,asyncpreemptoff=1", shimGoDebug)

	require.NotNil(t, execShim, "the shim should report whether the exec succeeded")
	require.Len(t, childCmd.ExtraFiles, 1, "the status descriptor should be passed to the shim")
}

func TestGoDebugWithAsyncPreemptionDisabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		goDebug  string
		expected string
	}{
		{name: "empty", expected: "asyncpreemptoff=1"},
		{name: "other settings", goDebug: "gctrace=1,schedtrace=1000", expected: "gctrace=1,schedtrace=1000,asyncpreemptoff=1"},
		{name: "preemption enabled", goDebug: "gctrace=1,asyncpreemptoff=0", expected: "gctrace=1,asyncpreemptoff=1"},
		{name: "duplicate preemption settings", goDebug: "asyncpreemptoff=0,gctrace=1,asyncpreemptoff=1", expected: "gctrace=1,asyncpreemptoff=1"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.expected, goDebugWithAsyncPreemptionDisabled(test.goDebug))
		})
	}
}

func TestUseExecShimPreservesGoDebugPresence(t *testing.T) {
	t.Parallel()

	if !process.SignalDispositionsLeakToChildren() {
		t.Skip("the exec shim is only used on platforms whose signal dispositions leak to children")
	}

	tests := []struct {
		name               string
		env                []string
		expectedTargetFlag string
	}{
		{
			name: "unset",
			env:  []string{"EXISTING=value"},
		},
		{
			name:               "explicitly empty",
			env:                []string{"EXISTING=value", goDebugEnvVar + "="},
			expectedTargetFlag: "--" + targetGoDebugFlagName + "=",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			childCmd := exec.Command("sh", "-c", "exit 0")
			childCmd.Env = test.env

			execShim, shimErr := useExecShim(childCmd)
			require.NoError(t, shimErr)
			require.NotNil(t, execShim)
			t.Cleanup(execShim.close)

			if test.expectedTargetFlag == "" {
				for _, arg := range childCmd.Args {
					require.NotContains(t, arg, "--"+targetGoDebugFlagName)
				}
			} else {
				require.Contains(t, childCmd.Args, test.expectedTargetFlag)
			}

			shimGoDebug, shimGoDebugSet := environmentVariable(childCmd.Env, goDebugEnvVar)
			require.True(t, shimGoDebugSet)
			require.Equal(t, "asyncpreemptoff=1", shimGoDebug)
		})
	}
}

func TestSetEnvironmentVariable(t *testing.T) {
	t.Parallel()

	env := []string{"FIRST=1", "GODEBUG=old", "SECOND=2", "GODEBUG=effective"}

	value, found := environmentVariable(env, goDebugEnvVar)
	require.True(t, found)
	require.Equal(t, "effective", value)

	updatedEnv := setEnvironmentVariable(env, goDebugEnvVar, "restored", true)
	require.Equal(t, []string{"FIRST=1", "SECOND=2", "GODEBUG=restored"}, updatedEnv)

	emptyEnv := setEnvironmentVariable(env, goDebugEnvVar, "", true)
	require.Equal(t, []string{"FIRST=1", "SECOND=2", "GODEBUG="}, emptyEnv)

	unsetEnv := setEnvironmentVariable(env, goDebugEnvVar, "", false)
	require.Equal(t, []string{"FIRST=1", "SECOND=2"}, unsetEnv)
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
	_, writeErr := fmt.Fprintf(handshake.statusW, "%d", int(syscall.ENOENT))
	require.NoError(t, writeErr)
	require.NoError(t, handshake.statusW.Close())

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
