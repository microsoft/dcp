//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	windowsDescendantStopHelper  = "DCP_WINDOWS_DESCENDANT_STOP_HELPER"
	windowsDescendantStopAction  = "DCP_WINDOWS_DESCENDANT_STOP_ACTION"
	windowsDescendantStopMarkers = "DCP_WINDOWS_DESCENDANT_STOP_MARKERS"
)

// Verifies that explicit and concurrent Windows stops, cancellation cleanup, and disposal
// give descendants the remaining graceful budget without interrupting the caller.
// Signal-resistant descendants are force-killed within the bounded cleanup window.
func TestWindowsDescendantStopsDoNotSignalCaller(t *testing.T) {
	t.Parallel()

	for _, action := range []string{"stop", "concurrent", "cancel", "dispose", "force"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			testCtx, testCancel := testutil.GetTestContext(t, time.Minute)
			defer testCancel()

			coordinator := exec.CommandContext(testCtx, os.Args[0], "-test.run=^TestWindowsDescendantStopHelper$")
			coordinator.Env = append(os.Environ(),
				windowsDescendantStopHelper+"=coordinator",
				windowsDescendantStopAction+"="+action,
				windowsDescendantStopMarkers+"="+t.TempDir(),
			)
			ForkFromParent(coordinator)
			output, runErr := coordinator.CombinedOutput()
			require.NoError(t, runErr, "isolated coordinator failed:\n%s", output)
		})
	}
}

// Runs isolated coordinator and inherited-group process fixtures to verify graceful descendant
// cleanup, bounded force-kill escalation, and caller signal isolation.
func TestWindowsDescendantStopHelper(t *testing.T) {
	mode := os.Getenv(windowsDescendantStopHelper)
	if mode == "" {
		return
	}
	testCtx, testCancel := testutil.GetTestContext(t, 45*time.Second)
	defer testCancel()
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt)
	defer signal.Stop(signalCh)

	switch mode {
	case "coordinator":
		runWindowsDescendantStopCoordinator(t, testCtx, signalCh)
	case "root":
		child := windowsDescendantHelperCommand("child")
		childOutput, childOutputErr := child.StdoutPipe()
		require.NoError(t, childOutputErr)
		defer func() { require.NoError(t, childOutput.Close()) }()
		require.NoError(t, child.Start())
		defer func() { require.NoError(t, child.Process.Release()) }()
		grandchildText, grandchildReadErr := bufio.NewReader(childOutput).ReadString('\n')
		require.NoError(t, grandchildReadErr)
		grandchildPID, grandchildParseErr := strconv.ParseInt(strings.TrimSpace(grandchildText), 10, 64)
		require.NoError(t, grandchildParseErr)
		_, readyWriteErr := fmt.Fprintln(os.Stdout, child.Process.Pid, grandchildPID)
		require.NoError(t, readyWriteErr)
		awaitWindowsDescendantSignal(t, testCtx, signalCh)
		if os.Getenv(windowsDescendantStopAction) == "concurrent" {
			_, signalWriteErr := fmt.Fprintln(os.Stdout, "signaled")
			require.NoError(t, signalWriteErr)
			_, inputReadErr := io.Copy(io.Discard, os.Stdin)
			require.NoError(t, inputReadErr)
		}
	case "child":
		grandchild := windowsDescendantHelperCommand("grandchild")
		grandchildOutput, grandchildOutputErr := grandchild.StdoutPipe()
		require.NoError(t, grandchildOutputErr)
		defer func() { require.NoError(t, grandchildOutput.Close()) }()
		require.NoError(t, grandchild.Start())
		defer func() { require.NoError(t, grandchild.Process.Release()) }()
		readyText, readyReadErr := bufio.NewReader(grandchildOutput).ReadString('\n')
		require.NoError(t, readyReadErr)
		require.Equal(t, "ready\n", readyText)
		_, pidWriteErr := fmt.Fprintln(os.Stdout, grandchild.Process.Pid)
		require.NoError(t, pidWriteErr)
		finishWindowsDescendantGracefully(t, testCtx, signalCh, mode)
	case "grandchild":
		_, readyWriteErr := fmt.Fprintln(os.Stdout, "ready")
		require.NoError(t, readyWriteErr)
		finishWindowsDescendantGracefully(t, testCtx, signalCh, mode)
	default:
		t.Fatalf("unknown descendant-stop helper mode %q", mode)
	}
}

func windowsDescendantHelperCommand(mode string) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestWindowsDescendantStopHelper$")
	command.Env = append(os.Environ(), windowsDescendantStopHelper+"="+mode)
	return command
}

func awaitWindowsDescendantSignal(t *testing.T, ctx context.Context, signalCh <-chan os.Signal) {
	t.Helper()
	select {
	case delivered, received := <-signalCh:
		require.True(t, received)
		require.Equal(t, os.Interrupt, delivered)
	case <-ctx.Done():
		t.Fatal("helper did not receive the root group signal")
	}
}

func finishWindowsDescendantGracefully(t *testing.T, ctx context.Context, signalCh <-chan os.Signal, mode string) {
	t.Helper()
	awaitWindowsDescendantSignal(t, ctx, signalCh)
	if os.Getenv(windowsDescendantStopAction) == "force" {
		<-ctx.Done()
		t.Fatal("signal-resistant descendant was not force-killed")
	}

	timer := time.NewTimer(signalAndWaitTimeout + time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		marker := filepath.Join(os.Getenv(windowsDescendantStopMarkers), mode)
		require.NoError(t, usvc_io.WriteFile(marker, []byte("graceful"), 0o600))
	case <-ctx.Done():
		t.Fatal("descendant did not receive its remaining graceful-stop budget")
	}
}

func runWindowsDescendantStopCoordinator(t *testing.T, ctx context.Context, signalCh <-chan os.Signal) {
	t.Helper()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	processCtx, processCancel := context.WithCancel(ctx)
	defer processCancel()
	exitResults := make(chan ProcessExitInfo, 1)
	root := windowsDescendantHelperCommand("root")
	rootOutput, rootOutputErr := root.StdoutPipe()
	require.NoError(t, rootOutputErr)
	rootInput, rootInputErr := root.StdinPipe()
	require.NoError(t, rootInputErr)
	defer func() { _ = rootInput.Close() }()
	rootHandle, startWaiting, rootStartErr := executor.StartProcess(
		processCtx,
		root,
		ProcessExitHandlerFunc(func(pid Pid_t, exitCode int32, exitErr error) {
			exitResults <- ProcessExitInfo{PID: pid, ExitCode: exitCode, Err: exitErr}
		}),
		CreationFlagEnsureKillOnDispose,
		nil,
	)
	require.NoError(t, rootStartErr)
	reader := bufio.NewReader(rootOutput)
	readyText, readyReadErr := reader.ReadString('\n')
	require.NoError(t, readyReadErr)
	fields := strings.Fields(readyText)
	require.Len(t, fields, 2)
	descendants := make([]ProcessHandle, 0, len(fields))
	for _, field := range fields {
		pid, pidParseErr := StringToPidT(field)
		require.NoError(t, pidParseErr)
		handle, handleErr := FindProcessHandle(pid)
		require.NoError(t, handleErr)
		descendants = append(descendants, handle)
	}
	tree, treeErr := GetProcessTree(ctx, rootHandle)
	require.NoError(t, treeErr)
	for _, handle := range descendants {
		require.Contains(t, tree, handle)
	}
	startWaiting()
	startedAt := time.Now()
	action := os.Getenv(windowsDescendantStopAction)
	switch action {
	case "stop", "force":
		require.NoError(t, executor.StopProcess(ctx, rootHandle))
	case "dispose":
		executor.Dispose()
	case "cancel":
		processCancel()
		select {
		case exitResult, received := <-exitResults:
			require.True(t, received)
			require.Equal(t, rootHandle.Pid, exitResult.PID)
			require.ErrorIs(t, exitResult.Err, context.Canceled)
		case <-ctx.Done():
			t.Fatal("automatic process cleanup did not finish")
		}
	case "concurrent":
		stopResults := make(chan error, 2)
		go func() { stopResults <- executor.StopProcess(ctx, rootHandle) }()
		signaledText, signaledReadErr := reader.ReadString('\n')
		require.NoError(t, signaledReadErr)
		require.Equal(t, "signaled\n", signaledText)
		go func() { stopResults <- executor.StopProcess(ctx, rootHandle) }()
		descendantWaitErr := wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
			executor.acquireLock()
			defer executor.releaseLock()
			state := executor.procsWaiting[descendants[0]]
			return state != nil && state.reason&waitReasonStopping != 0, nil
		})
		require.NoError(t, descendantWaitErr)
		require.NoError(t, rootInput.Close())
		for range 2 {
			select {
			case stopErr, received := <-stopResults:
				require.True(t, received)
				require.NoError(t, stopErr)
				for _, handle := range descendants {
					require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)),
						"each concurrent stop must confirm descendant exit")
				}
			case <-ctx.Done():
				t.Fatal("concurrent process stop did not finish")
			}
		}
	default:
		t.Fatalf("unknown stop action %q", action)
	}
	elapsed := time.Since(startedAt)
	require.Less(t, elapsed, processStopTimeout)
	if action == "force" {
		for _, handle := range descendants {
			require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(handle)),
				"force stop must confirm descendant exit before returning")
		}
	}
	if action == "force" {
		require.GreaterOrEqual(t, elapsed, gracefulProcessStopTimeout)
	} else {
		require.GreaterOrEqual(t, elapsed, signalAndWaitTimeout)
		for _, mode := range []string{"child", "grandchild"} {
			marker, markerOpenErr := usvc_io.OpenFileReadOnly(filepath.Join(os.Getenv(windowsDescendantStopMarkers), mode))
			require.NoError(t, markerOpenErr)
			contents, markerReadErr := io.ReadAll(marker)
			require.NoError(t, errors.Join(markerReadErr, marker.Close()))
			require.Equal(t, "graceful", string(contents))
		}
	}
	handles := append([]ProcessHandle{rootHandle}, descendants...)
	allGoneErr := wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		for _, handle := range handles {
			runningErr := executor.CheckProcessRunning(handle)
			if runningErr == nil {
				return false, nil
			}
			if !IsProcessGoneErr(runningErr) {
				return false, runningErr
			}
		}
		return true, nil
	})
	require.NoError(t, allGoneErr)
	select {
	case delivered, received := <-signalCh:
		require.True(t, received)
		t.Fatalf("stop signaled its caller: %v", delivered)
	default:
	}
	require.NoError(t, ctx.Err())
}
