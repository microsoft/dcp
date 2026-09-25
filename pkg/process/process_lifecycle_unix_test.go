//go:build darwin || linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

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

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"
)

func TestProcessStopContextHelper(t *testing.T) {
	if os.Getenv("DCP_PROCESS_STOP_CONTEXT_HELPER") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	_, readyErr := fmt.Fprintln(os.Stdout, "ready")
	require.NoError(t, readyErr)
	_, readErr := io.Copy(io.Discard, os.Stdin)
	require.NoError(t, readErr)
}

func TestCancelledStopCanBeRetriedWithoutAnotherWaiter(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessStopContextHelper$")
	cmd.Env = append(os.Environ(), "DCP_PROCESS_STOP_CONTEXT_HELPER=1")
	stdout, stdoutErr := cmd.StdoutPipe()
	require.NoError(t, stdoutErr)
	stdin, stdinErr := cmd.StdinPipe()
	require.NoError(t, stdinErr)
	defer func() { _ = stdin.Close() }()
	handle, _, startErr := executor.StartProcess(testCtx, cmd, nil, CreationFlagEnsureKillOnDispose, nil)
	require.NoError(t, startErr)
	ready, readyErr := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, readyErr)
	require.Equal(t, "ready\n", ready)

	stopCtx, stopCancel := context.WithCancel(testCtx)
	defer stopCancel()
	stopped := make(chan error, 1)
	go func() { stopped <- executor.StopProcess(stopCtx, handle) }()
	ownershipErr := wait.PollUntilContextCancel(testCtx, time.Millisecond, true, func(context.Context) (bool, error) {
		executor.acquireLock()
		defer executor.releaseLock()
		state := executor.procsWaiting[handle]
		return state != nil && state.reason&waitReasonStopping != 0, nil
	})
	require.NoError(t, ownershipErr)
	stopCancel()
	select {
	case stopErr, received := <-stopped:
		require.True(t, received)
		require.ErrorIs(t, stopErr, context.Canceled)
	case <-testCtx.Done():
		t.Fatal("cancelled stop did not return")
	}
	require.NoError(t, executor.CheckProcessRunning(handle))
	require.NoError(t, executor.StopProcess(testCtx, handle))
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)))
}
