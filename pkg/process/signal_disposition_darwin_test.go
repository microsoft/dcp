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

const (
	prepareSIGUSR1ForExecHelperEnvVar = "DCP_TEST_PREPARE_SIGUSR1_FOR_EXEC_HELPER"
	prepareSIGUSR1DefaultMode         = "default"
	prepareSIGUSR1IgnoredMode         = "ignored"
)

func TestPrepareSIGUSR1ForExec(t *testing.T) {
	t.Parallel()

	runPrepareSIGUSR1ForExecHelper(t, prepareSIGUSR1DefaultMode)
}

func TestPrepareSIGUSR1ForExecPreservesIgnoredDisposition(t *testing.T) {
	t.Parallel()

	runPrepareSIGUSR1ForExecHelper(t, prepareSIGUSR1IgnoredMode)
}

func runPrepareSIGUSR1ForExecHelper(t *testing.T, mode string) {
	t.Helper()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	t.Cleanup(testCancel)

	helper := exec.CommandContext(testCtx, os.Args[0], "-test.run=^TestPrepareSIGUSR1ForExecHelper$", "-test.v")
	helper.Env = append(os.Environ(), prepareSIGUSR1ForExecHelperEnvVar+"="+mode)

	output, runErr := helper.CombinedOutput()
	require.NoError(t, runErr, "helper process failed; output:\n%s", output)
}

func TestPrepareSIGUSR1ForExecHelper(t *testing.T) {
	mode := os.Getenv(prepareSIGUSR1ForExecHelperEnvVar)
	if mode == "" {
		t.Skip("helper for TestPrepareSIGUSR1ForExec")
	}

	switch mode {
	case prepareSIGUSR1DefaultMode:
		testPrepareSIGUSR1ForExecDefault(t)
	case prepareSIGUSR1IgnoredMode:
		testPrepareSIGUSR1ForExecIgnored(t)
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func testPrepareSIGUSR1ForExecDefault(t *testing.T) {
	t.Helper()

	before := readSignalDispositions(t)
	beforeSIGUSR1 := before[int(syscall.SIGUSR1)]
	require.NotEqual(t, darwinSigIgn, beforeSIGUSR1.handler, "the Go runtime should not ignore SIGUSR1")
	require.NotZero(t, beforeSIGUSR1.flags, "the Go runtime should have left flags set on SIGUSR1")

	require.NoError(t, PrepareSIGUSR1ForExec())

	after := readSignalDispositions(t)
	for sig := 1; sig < darwinNumSignals; sig++ {
		if sig == int(syscall.SIGKILL) || sig == int(syscall.SIGSTOP) {
			continue
		}

		if sig == int(syscall.SIGUSR1) {
			require.Equal(t, darwinSigDfl, after[sig].handler, "SIGUSR1 should be reset to SIG_DFL")
			require.Zero(t, after[sig].flags, "SIGUSR1 should have no flags")
			require.Zero(t, after[sig].mask, "SIGUSR1 should have an empty mask")
			continue
		}

		require.Equal(t, before[sig], after[sig], "signal %d disposition should remain unchanged", sig)
	}
}

func testPrepareSIGUSR1ForExecIgnored(t *testing.T) {
	t.Helper()

	ignoredAction := darwinSigactionNew{handler: darwinSigIgn}
	require.NoError(t, setSignalDisposition(int(syscall.SIGUSR1), &ignoredAction))

	before, beforeErr := signalDisposition(int(syscall.SIGUSR1))
	require.NoError(t, beforeErr)
	require.Equal(t, darwinSigIgn, before.handler)

	require.NoError(t, PrepareSIGUSR1ForExec())

	after, afterErr := signalDisposition(int(syscall.SIGUSR1))
	require.NoError(t, afterErr)
	require.Equal(t, before, after, "an ignored SIGUSR1 disposition should remain unchanged")
}

func readSignalDispositions(t *testing.T) map[int]darwinSigactionOld {
	t.Helper()

	dispositions := make(map[int]darwinSigactionOld, darwinNumSignals-3)
	for sig := 1; sig < darwinNumSignals; sig++ {
		if sig == int(syscall.SIGKILL) || sig == int(syscall.SIGSTOP) {
			continue
		}

		disposition, dispositionErr := signalDisposition(sig)
		require.NoError(t, dispositionErr, "signal %d disposition should be readable", sig)
		dispositions[sig] = disposition
	}

	return dispositions
}
