//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process_test

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	wait "k8s.io/apimachinery/pkg/util/wait"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Tests that processes that ignore SIGTERM can still be terminated.
// Run on Unix-like systems only, because Windows does not have signals.
func TestStopProcessIgnoreSigterm(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	delayToolDir, err := getDelayToolDir()
	require.NoError(t, err)

	const delay = 20 * time.Second
	cmd := exec.Command("./delay", fmt.Sprintf("--delay=%s", delay.String()), "--ignore-sigterm")
	cmd.Dir = delayToolDir

	err = cmd.Start()
	require.NoError(t, err, "could not start the 'delay' test program")
	defer func() {
		_ = cmd.Wait()
	}()

	rootP, handleErr := process.ProcessHandleFromCmd(cmd)
	require.NoError(t, handleErr)
	require.False(t, rootP.IdentityTime.IsZero(), "process start time should not be zero")

	// Only one process should be running, so the "tree" size is 1.
	int_testutil.EnsureProcessTree(t, rootP, 1, 5*time.Second)

	executor := process.NewOSExecutor(log)
	start := time.Now()
	err = executor.StopProcess(testCtx, rootP)
	require.NoError(t, err)
	elapsed := time.Since(start)
	elapsedStr := osutil.FormatDuration(elapsed)
	if elapsed > delay {
		// It is expected that the process will not exit immediately, because it will ignore SIGTERM.
		// It should not take more than `signalAndWaitTimeout` though.
		t.Fatal("Process was not terminated timely, elapsed time was ", elapsedStr)
	}
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
