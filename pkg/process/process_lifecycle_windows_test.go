//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"bufio"
	"context"
	"fmt"
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

const windowsProcessLifecycleHelperMode = "DCP_WINDOWS_PROCESS_LIFECYCLE_HELPER_MODE"

func TestWindowsProcessLifecycleHelper(t *testing.T) {
	mode := os.Getenv(windowsProcessLifecycleHelperMode)
	if mode == "" {
		return
	}

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt)
	defer signal.Stop(signalCh)

	switch mode {
	case "console-root":
		childCmd := exec.Command(os.Args[0], "-test.run=^TestWindowsProcessLifecycleHelper$")
		childCmd.Env = append(os.Environ(), windowsProcessLifecycleHelperMode+"=console-child")
		childStdout, stdoutErr := childCmd.StdoutPipe()
		require.NoError(t, stdoutErr)
		childStartErr := childCmd.Start()
		require.NoError(t, childStartErr)
		readyText, readyErr := bufio.NewReader(childStdout).ReadString('\n')
		require.NoError(t, readyErr)
		require.Equal(t, "ready\n", readyText)
		_, writeErr := fmt.Fprintln(os.Stdout, childCmd.Process.Pid)
		require.NoError(t, writeErr)
		<-signalCh

	case "console-child":
		_, readyErr := fmt.Fprintln(os.Stdout, "ready")
		require.NoError(t, readyErr)
		<-signalCh
		time.Sleep(signalAndWaitTimeout + time.Second)
		markerPath := os.Getenv("DCP_WINDOWS_PROCESS_LIFECYCLE_MARKER")
		require.NotEmpty(t, markerPath)
		require.NoError(t, usvc_io.WriteFile(markerPath, []byte("graceful"), 0o600))

	case "signal-resistant-root":
		_, readyErr := fmt.Fprintln(os.Stdout, "ready")
		require.NoError(t, readyErr)
		<-signalCh
		time.Sleep(30 * time.Second)

	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

// Verifies that StopViaConsole gives descendants the remaining shared graceful budget
// instead of force-killing them after the per-process signal timeout.
func TestStopViaConsoleUsesRemainingTreeGracePeriod(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	markerPath := filepath.Join(t.TempDir(), "graceful-exit")
	rootCmd := exec.Command(os.Args[0], "-test.run=^TestWindowsProcessLifecycleHelper$")
	rootCmd.Env = append(
		os.Environ(),
		windowsProcessLifecycleHelperMode+"=console-root",
		"DCP_WINDOWS_PROCESS_LIFECYCLE_MARKER="+markerPath,
	)
	ForkFromParent(rootCmd)
	rootStdout, stdoutErr := rootCmd.StdoutPipe()
	require.NoError(t, stdoutErr)
	require.NoError(t, rootCmd.Start())
	t.Cleanup(func() {
		if rootCmd.Process != nil {
			_ = rootCmd.Process.Kill()
		}
	})

	childPIDText, readErr := bufio.NewReader(rootStdout).ReadString('\n')
	require.NoError(t, readErr)
	childPID, parseErr := strconv.ParseInt(strings.TrimSpace(childPIDText), 10, 64)
	require.NoError(t, parseErr)

	rootHandle, rootHandleErr := ProcessHandleFromCmd(rootCmd)
	require.NoError(t, rootHandleErr)
	childHandle, childHandleErr := FindProcessHandle(Pid_t(childPID))
	require.NoError(t, childHandleErr)
	t.Cleanup(func() {
		childProc, findErr := FindProcess(childHandle)
		if findErr == nil {
			_ = childProc.Kill()
			_ = childProc.Release()
		}
	})

	treeReadyErr := wait.PollUntilContextCancel(testCtx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		tree, treeErr := GetProcessTree(testCtx, rootHandle)
		return treeErr == nil && len(tree) >= 2, nil
	})
	require.NoError(t, treeReadyErr)

	executor := NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	startedAt := time.Now()
	stopErr := StopViaConsole(testCtx, logr.Discard(), executor, rootHandle)
	elapsed := time.Since(startedAt)

	require.NoError(t, stopErr)
	require.GreaterOrEqual(t, elapsed, signalAndWaitTimeout)
	require.Less(t, elapsed, signalAndWaitTimeout+4*time.Second)
	_, markerErr := os.Stat(markerPath)
	require.NoError(t, markerErr, "descendant should exit naturally after the console signal")
	require.NoError(t, rootCmd.Wait())
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(childHandle)))
}

// Verifies that a root-only console stop force-kills a signal-resistant root
// and confirms its exit within the bounded full stop window.
func TestStopViaConsoleRootOnlyBoundsForceConfirmation(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	rootCmd := exec.Command(os.Args[0], "-test.run=^TestWindowsProcessLifecycleHelper$")
	rootCmd.Env = append(os.Environ(), windowsProcessLifecycleHelperMode+"=signal-resistant-root")
	ForkFromParent(rootCmd)
	rootStdout, stdoutErr := rootCmd.StdoutPipe()
	require.NoError(t, stdoutErr)
	require.NoError(t, rootCmd.Start())
	t.Cleanup(func() {
		if rootCmd.Process != nil {
			_ = rootCmd.Process.Kill()
		}
	})
	readyText, readyErr := bufio.NewReader(rootStdout).ReadString('\n')
	require.NoError(t, readyErr)
	require.Equal(t, "ready\n", readyText)

	rootHandle, rootHandleErr := ProcessHandleFromCmd(rootCmd)
	require.NoError(t, rootHandleErr)
	executor := NewOSExecutor(logr.Discard())
	defer executor.Dispose()

	startedAt := time.Now()
	stopErr := StopViaConsole(testCtx, logr.Discard(), executor, rootHandle, StopRootOnly())
	elapsed := time.Since(startedAt)

	require.NoError(t, stopErr)
	require.GreaterOrEqual(t, elapsed, signalAndWaitTimeout)
	require.Less(t, elapsed, processStopTimeout)
	waitErr := rootCmd.Wait()
	require.True(t, waitErr == nil || IsEarlyProcessExitError(waitErr))
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(rootHandle)))
}
