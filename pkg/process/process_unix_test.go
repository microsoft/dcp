//go:build !windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package process_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	wait "k8s.io/apimachinery/pkg/util/wait"

	int_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

// Tests that processes that ignore SIGTERM can still be terminated.
// Run on Unix-like systems only, because Windows does not have signals.
func TestStopProcessIgnoreSigterm(t *testing.T) {
	t.Parallel()

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

	pid := process.Uint32_ToPidT(uint32(cmd.Process.Pid))
	createTime := process.StartTimeForProcess(pid)
	require.False(t, createTime.IsZero(), "process start time should not be zero")
	rootP := process.ProcessTreeItem{pid, createTime}

	// Only one process should be running, so the "tree" size is 1.
	int_testutil.EnsureProcessTree(t, rootP, 1, 5*time.Second)

	executor := process.NewOSExecutor(log)
	start := time.Now()
	err = executor.StopProcess(pid, time.Time{})
	require.NoError(t, err)
	elapsed := time.Since(start)
	elapsedStr := osutil.FormatDuration(elapsed)
	if elapsed > delay {
		// It is expected that the process will not exit immediately, because it will ignore SIGTERM.
		// It should not take more than `signalAndWaitTimeout` though.
		t.Fatal("Process was not terminated timely, elapsed time was ", elapsedStr)
	}
	ensureAllStopped(t, []process.ProcessTreeItem{rootP}, 5*time.Second)
}

func ensureAllStopped(t *testing.T, processes []process.ProcessTreeItem, timeout time.Duration) {
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

func isStopped(pp process.ProcessTreeItem) bool {
	// On Unix-like systems FindProcess() always succeeds, so it is not a reliable way of checking
	// if the process is still running.
	osPid, err := process.PidT_ToInt(pp.Pid)
	if err != nil {
		panic(err)
	}

	proc, findProcessErr := os.FindProcess(osPid)
	if findProcessErr != nil {
		return true
	}
	// The SIGWINCH (window resize) is ignored by default, so it is a good one to use
	// as a "Are you there?" query
	signalSendErr := proc.Signal(syscall.SIGWINCH)
	return errors.Is(signalSendErr, os.ErrProcessDone)
}
