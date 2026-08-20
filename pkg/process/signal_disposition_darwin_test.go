//go:build darwin

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

// Marks the re-executed test binary as the helper that performs the reset. The reset disables
// the Go runtime's signal handling, so it cannot run in the main test process.
const resetSignalDispositionsHelperEnvVar = "DCP_TEST_RESET_SIGNAL_DISPOSITIONS_HELPER"

func TestResetSignalDispositions(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	t.Cleanup(testCancel)

	helper := exec.CommandContext(testCtx, os.Args[0], "-test.run=TestResetSignalDispositionsHelper", "-test.v")
	helper.Env = append(os.Environ(), resetSignalDispositionsHelperEnvVar+"=1")

	output, runErr := helper.CombinedOutput()
	require.NoError(t, runErr, "helper process failed; output:\n%s", output)
}

// Runs inside the process started by TestResetSignalDispositions and is skipped otherwise.
func TestResetSignalDispositionsHelper(t *testing.T) {
	if os.Getenv(resetSignalDispositionsHelperEnvVar) != "1" {
		t.Skip("helper for TestResetSignalDispositions")
	}

	before, beforeErr := signalDisposition(int(syscall.SIGUSR1))
	require.NoError(t, beforeErr)
	require.NotZero(t, before.flags, "the Go runtime should have left flags set on SIGUSR1")

	ResetSignalDispositions()

	for sig := 1; sig < darwinNumSignals; sig++ {
		if sig == int(syscall.SIGKILL) || sig == int(syscall.SIGSTOP) {
			continue
		}

		after, afterErr := signalDisposition(sig)
		require.NoError(t, afterErr, "signal %d disposition should be readable", sig)
		require.Equal(t, darwinSigDfl, after.handler, "signal %d should be reset to SIG_DFL", sig)
		require.Zero(t, after.flags, "signal %d should have no flags", sig)
		require.Zero(t, after.mask, "signal %d should have an empty mask", sig)
	}
}
