//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	wait "k8s.io/apimachinery/pkg/util/wait"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Tests that processes that ignore SIGTERM can still be terminated.
// Run on Unix-like systems only, because Windows does not have signals.
func TestStopProcessIgnoreSigterm(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 35*time.Second)
	defer testCancel()

	delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	cmd := exec.Command(delayPath, "--delay=60s", "--ignore-sigterm")
	startErr := cmd.Start()
	require.NoError(t, startErr, "could not start delay")
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	rootP, handleErr := process.ProcessHandleFromCmd(cmd)
	require.NoError(t, handleErr)
	require.False(t, rootP.IdentityTime.IsZero(), "process start time should not be zero")

	// Allow delay to install its signal handlers before requesting a stop.
	time.Sleep(2 * time.Second)
	executor := process.NewOSExecutor(log)
	defer executor.Dispose()
	start := time.Now()
	stopErr := executor.StopProcess(testCtx, rootP)
	require.NoError(t, stopErr)
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, 14*time.Second,
		"delay must remain alive through the graceful SIGTERM phase")
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
	proc, findProcessErr := pp.OsProcess()
	if findProcessErr != nil {
		return process.IsProcessGoneErr(findProcessErr)
	}
	_ = proc.Release()
	return false
}
