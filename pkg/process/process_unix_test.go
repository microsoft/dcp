//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	wait "k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/testutil"
)

const sigtermResistantFixture = "DCP_SIGTERM_RESISTANT_FIXTURE"

// Provides a SIGTERM-resistant child with a readiness handshake so the stop test cannot
// pass because the fixture exited naturally or received SIGTERM before installing its handler.
func TestSigtermResistantFixture(t *testing.T) {
	if os.Getenv(sigtermResistantFixture) != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	_, readyErr := fmt.Fprintln(os.Stdout, "ready")
	require.NoError(t, readyErr)
	_, inputErr := io.Copy(io.Discard, os.Stdin)
	require.NoError(t, inputErr)
}

// Tests that processes that ignore SIGTERM can still be terminated.
// Run on Unix-like systems only, because Windows does not have signals.
func TestStopProcessIgnoreSigterm(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 35*time.Second)
	defer testCancel()

	cmd := exec.Command(os.Args[0], "-test.run=^TestSigtermResistantFixture$")
	cmd.Env = append(os.Environ(), sigtermResistantFixture+"=1")
	stdout, stdoutErr := cmd.StdoutPipe()
	require.NoError(t, stdoutErr)
	stdin, stdinErr := cmd.StdinPipe()
	require.NoError(t, stdinErr)
	defer func() { _ = stdin.Close() }()
	startErr := cmd.Start()
	require.NoError(t, startErr, "could not start the SIGTERM-resistant fixture")
	defer func() {
		_ = cmd.Wait()
	}()
	ready, readyErr := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, readyErr)
	require.Equal(t, "ready\n", ready, "the test must not signal before the SIGTERM handler is installed")

	rootP, handleErr := process.ProcessHandleFromCmd(cmd)
	require.NoError(t, handleErr)
	require.False(t, rootP.IdentityTime.IsZero(), "process start time should not be zero")

	executor := process.NewOSExecutor(log)
	defer executor.Dispose()
	start := time.Now()
	stopErr := executor.StopProcess(testCtx, rootP)
	require.NoError(t, stopErr)
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, 14*time.Second,
		"the fixture must remain alive through the graceful SIGTERM phase")
	require.Less(t, elapsed, 20*time.Second,
		"force-kill confirmation must finish before the complete 21-second stop budget")
	ensureAllStopped(t, []process.ProcessHandle{rootP}, 5*time.Second)
}

func ensureAllStopped(t *testing.T, processes []process.ProcessHandle, timeout time.Duration) {
	timeoutCtx, timeoutCtxCancelFn := context.WithTimeout(context.Background(), timeout)
	defer timeoutCtxCancelFn()

	err := wait.PollUntilContextCancel(
		timeoutCtx,
		100*time.Millisecond,
		true, // Don't wait before polling for the first time
		func(_ context.Context) (bool, error) {
			noStopped := slices.LenIf(processes, isStopped)
			return noStopped == len(processes), nil
		},
	)

	require.NoError(t, err, "not all processes could be stopped")
}

func isStopped(pp process.ProcessHandle) bool {
	proc, findProcessErr := process.FindProcess(pp)
	if findProcessErr != nil {
		return process.IsProcessGoneErr(findProcessErr)
	}
	_ = proc.Release()
	return false
}
