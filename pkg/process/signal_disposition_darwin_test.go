//go:build darwin

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	prepareSIGUSR1ForExecHelperEnvVar        = "DCP_TEST_PREPARE_SIGUSR1_FOR_EXEC_HELPER"
	prepareSIGUSR1DefaultMode                = "default"
	prepareSIGUSR1IgnoredMode                = "ignored"
	darwinTestSARestart               int32  = 0x2
	dirtySignalMask                   uint32 = 1
)

func TestPrepareSIGUSR1ForExecUsesDefaultDisposition(t *testing.T) {
	t.Parallel()

	runPrepareSIGUSR1ForExecHelper(t, prepareSIGUSR1DefaultMode)
}

func TestPrepareSIGUSR1ForExecUsesIgnoredDisposition(t *testing.T) {
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

	initiallyIgnored, ignoredErr := IsSIGUSR1Ignored()
	require.NoError(t, ignoredErr)
	require.False(t, initiallyIgnored, "the Go test process should not start with SIGUSR1 ignored")

	// The requested target disposition, rather than the shim's current disposition, must win.
	setDirtySIGUSR1Disposition(t, darwinSigIgn)
	before := readSignalDispositions(t)
	beforeSIGUSR1 := before[int(syscall.SIGUSR1)]
	require.Equal(t, darwinSigIgn, beforeSIGUSR1.handler)
	require.NotZero(t, beforeSIGUSR1.flags)
	require.NotZero(t, beforeSIGUSR1.mask)

	require.NoError(t, PrepareSIGUSR1ForExec(false))

	assertPreparedSIGUSR1Disposition(t, before, darwinSigDfl)
}

func testPrepareSIGUSR1ForExecIgnored(t *testing.T) {
	t.Helper()

	signal.Ignore(syscall.SIGUSR1)
	ignored, ignoredErr := IsSIGUSR1Ignored()
	require.NoError(t, ignoredErr)
	require.True(t, ignored, "SIGUSR1 should be reported as ignored")

	// The requested target disposition, rather than the shim's current disposition, must win.
	setDirtySIGUSR1Disposition(t, darwinSigDfl)
	before := readSignalDispositions(t)
	beforeSIGUSR1 := before[int(syscall.SIGUSR1)]
	require.Equal(t, darwinSigDfl, beforeSIGUSR1.handler)
	require.NotZero(t, beforeSIGUSR1.flags)
	require.NotZero(t, beforeSIGUSR1.mask)

	require.NoError(t, PrepareSIGUSR1ForExec(true))

	assertPreparedSIGUSR1Disposition(t, before, darwinSigIgn)
}

func setDirtySIGUSR1Disposition(t *testing.T, handler uintptr) {
	t.Helper()

	dirtyAction := darwinSigactionNew{
		handler: handler,
		mask:    dirtySignalMask,
		flags:   darwinTestSARestart,
	}
	setErr := setSignalDisposition(int(syscall.SIGUSR1), &dirtyAction)
	require.NoError(t, setErr)
}

func assertPreparedSIGUSR1Disposition(
	t *testing.T,
	before map[int]darwinSigactionOld,
	expectedHandler uintptr,
) {
	t.Helper()

	after := readSignalDispositions(t)
	afterSIGUSR1 := after[int(syscall.SIGUSR1)]
	require.Equal(t, expectedHandler, afterSIGUSR1.handler, "SIGUSR1 should have the requested handler")
	require.Zero(t, afterSIGUSR1.flags, "SIGUSR1 should have no flags")
	require.Zero(t, afterSIGUSR1.mask, "SIGUSR1 should have an empty mask")
	require.Equal(
		t,
		before[int(syscall.SIGURG)],
		after[int(syscall.SIGURG)],
		"SIGURG disposition should remain unchanged",
	)

	for sig := 1; sig < darwinNumSignals; sig++ {
		if sig == int(syscall.SIGKILL) ||
			sig == int(syscall.SIGSTOP) ||
			sig == int(syscall.SIGUSR1) ||
			sig == int(syscall.SIGURG) {
			continue
		}

		require.Equal(t, before[sig], after[sig], "signal %d disposition should remain unchanged", sig)
	}
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
