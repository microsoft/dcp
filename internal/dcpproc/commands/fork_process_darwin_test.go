//go:build darwin

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	execShimIgnoredEndToEndEnvVar   = "DCP_TEST_EXEC_SHIM_IGNORED_END_TO_END_HELPER"
	execShimIgnoredEndToEndTestName = "TestExecShimPreservesIgnoredSIGUSR1Helper"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ForkProcessExecCmdName {
		os.Exit(runForkProcessExecTestCommand())
	}

	os.Exit(m.Run())
}

func runForkProcessExecTestCommand() int {
	forkProcessExecCmd, commandErr := NewForkProcessExecCommand(logr.Discard())
	if commandErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "could not create fork-process-exec test command: %v\n", commandErr)
		return 1
	}

	forkProcessExecCmd.SetArgs(os.Args[2:])
	executeErr := forkProcessExecCmd.Execute()
	if executeErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "fork-process-exec test command failed: %v\n", executeErr)
		return 1
	}

	return 0
}

func TestUseExecShimCarriesIgnoredSIGUSR1(t *testing.T) {
	t.Parallel()

	childCmd := exec.Command("/bin/sh", "-c", "exit 0")
	execShim, shimErr := useExecShimWithDisposition(childCmd, true)
	require.NoError(t, shimErr)
	require.NotNil(t, execShim)
	t.Cleanup(execShim.close)

	require.Contains(
		t,
		childCmd.Args,
		"--"+callerSIGUSR1IgnoredFlagName+"=true",
		"the shim invocation should carry the caller's ignored disposition",
	)
}

func TestExecShimPreservesIgnoredSIGUSR1ForNonGoTarget(t *testing.T) {
	t.Parallel()

	runExecShimTestHelper(t, execShimIgnoredEndToEndTestName, execShimIgnoredEndToEndEnvVar)
}

func TestExecShimPreservesIgnoredSIGUSR1Helper(t *testing.T) {
	if os.Getenv(execShimIgnoredEndToEndEnvVar) == "" {
		t.Skip("helper for TestExecShimPreservesIgnoredSIGUSR1ForNonGoTarget")
	}

	// Only this subprocess changes its signal state; the parallel parent test remains untouched.
	signal.Ignore(syscall.SIGUSR1)
	ignoredByCaller, dispositionErr := process.IsSIGUSR1Ignored()
	require.NoError(t, dispositionErr)
	require.True(t, ignoredByCaller)

	childCmd := exec.Command("/bin/sh", "-c", `kill -USR1 $$; printf ignored`)
	var childOutput bytes.Buffer
	childCmd.Stdout = &childOutput
	childCmd.Stderr = &childOutput

	execShim, shimErr := useExecShimWithDisposition(childCmd, true)
	require.NoError(t, shimErr)
	require.NotNil(t, execShim)
	t.Cleanup(execShim.close)

	// The exec shim starts from this ignored disposition, then its Go runtime replaces it.
	childStartErr := childCmd.Start()
	require.NoError(t, childStartErr)

	childFinished := false
	t.Cleanup(func() {
		if !childFinished {
			_ = childCmd.Process.Kill()
			_ = childCmd.Wait()
		}
	})

	handshakeErr := execShim.wait()
	require.NoError(t, handshakeErr, "the Go exec shim should reach the non-Go target")

	childWaitErr := childCmd.Wait()
	childFinished = true
	require.NoError(t, childWaitErr, "the target should survive SIGUSR1; output:\n%s", childOutput.String())
	require.Equal(t, "ignored", childOutput.String())
}

func runExecShimTestHelper(t *testing.T, testName string, envVarName string) {
	t.Helper()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	t.Cleanup(testCancel)

	helperCmd := exec.CommandContext(testCtx, os.Args[0], "-test.run=^"+testName+"$", "-test.v")
	helperCmd.Env = append(os.Environ(), envVarName+"=1")

	output, runErr := helperCmd.CombinedOutput()
	require.NoError(t, runErr, "helper process failed; output:\n%s", output)
}
