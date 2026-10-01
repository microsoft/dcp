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
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	windowsProcessLifecycleFixtureMode = "DCP_WINDOWS_PROCESS_LIFECYCLE_FIXTURE_MODE"
	delayedRootExitDuration            = 4 * time.Second
)

// Verifies the Windows subprocess modes used by process lifecycle tests.
func TestWindowsProcessLifecycleFixture(t *testing.T) {
	mode := os.Getenv(windowsProcessLifecycleFixtureMode)
	if mode == "" {
		return
	}

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt)
	defer signal.Stop(signalCh)

	switch mode {
	case "console-root", "delayed-console-root":
		childMode := "console-child"
		if mode == "delayed-console-root" {
			childMode = "delayed-console-child"
		}
		childCmd := exec.Command(os.Args[0], "-test.run=^TestWindowsProcessLifecycleFixture$")
		childCmd.Env = append(os.Environ(), windowsProcessLifecycleFixtureMode+"="+childMode)
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
		if mode == "delayed-console-root" {
			time.Sleep(delayedRootExitDuration)
		}

	case "console-child":
		_, readyErr := fmt.Fprintln(os.Stdout, "ready")
		require.NoError(t, readyErr)
		<-signalCh
		time.Sleep(signalAndWaitTimeout + time.Second)
		markerPath := os.Getenv("DCP_WINDOWS_PROCESS_LIFECYCLE_MARKER")
		require.NotEmpty(t, markerPath)
		require.NoError(t, usvc_io.WriteFile(markerPath, []byte("graceful"), 0o600))

	case "delayed-console-child":
		_, readyErr := fmt.Fprintln(os.Stdout, "ready")
		require.NoError(t, readyErr)
		<-signalCh
		time.Sleep(30 * time.Second)

	case "signal-resistant-root":
		_, readyErr := fmt.Fprintln(os.Stdout, "ready")
		require.NoError(t, readyErr)
		<-signalCh
		time.Sleep(30 * time.Second)

	default:
		t.Fatalf("unknown fixture mode %q", mode)
	}
}

// Verifies with real Windows process identities that a root exiting immediately
// after the snapshot does not discard an already verified descendant.
func TestGetProcessTreePreservesWindowsDescendantAfterRootExitPostSnapshot(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	rootCmd := exec.Command(os.Args[0], "-test.run=^TestWindowsProcessLifecycleFixture$")
	rootCmd.Env = append(
		os.Environ(),
		windowsProcessLifecycleFixtureMode+"=console-root",
		"DCP_WINDOWS_PROCESS_LIFECYCLE_MARKER="+filepath.Join(t.TempDir(), "unused-marker"),
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

	childPIDText, childReadErr := bufio.NewReader(rootStdout).ReadString('\n')
	require.NoError(t, childReadErr)
	childPID, childParseErr := strconv.ParseInt(strings.TrimSpace(childPIDText), 10, 64)
	require.NoError(t, childParseErr)
	rootHandle, rootHandleErr := ProcessHandleFromCmd(rootCmd)
	require.NoError(t, rootHandleErr)
	childHandle, childHandleErr := FindProcessHandle(Pid_t(childPID))
	require.NoError(t, childHandleErr)
	t.Cleanup(func() {
		childProcess, findErr := FindProcess(childHandle)
		if findErr == nil {
			_ = childProcess.Kill()
			_ = childProcess.Release()
		}
	})

	tree, treeErr := getProcessTree(
		testCtx,
		rootHandle,
		findProcessInfo,
		func(snapshotCtx context.Context) ([]processInfo, error) {
			snapshot, snapshotErr := snapshotProcesses(snapshotCtx)
			require.NoError(t, rootCmd.Process.Kill())
			rootGoneErr := wait.PollUntilContextCancel(
				snapshotCtx,
				time.Millisecond,
				true,
				func(context.Context) (bool, error) {
					_, rootInfoErr := findProcessInfo(rootHandle)
					return IsProcessGoneErr(rootInfoErr), nil
				},
			)
			require.NoError(t, rootGoneErr)
			return snapshot, snapshotErr
		},
	)

	require.ErrorIs(t, treeErr, ErrIncompleteProcessTree)
	require.NotEmpty(t, tree)
	require.Equal(t, rootHandle, tree[0])
	require.Contains(t, tree, childHandle)
	rootWaitErr := rootCmd.Wait()
	require.True(t, rootWaitErr == nil || IsEarlyProcessExitError(rootWaitErr))
}

// Verifies that a descendant whose receipt of the root's CTRL_C_EVENT is not represented
// in executor runtime state receives only the six-second passive fallback.
func TestStopViaConsoleUsesPassiveFallbackForUnknownDescendant(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	markerPath := filepath.Join(t.TempDir(), "graceful-exit")
	rootCmd := exec.Command(os.Args[0], "-test.run=^TestWindowsProcessLifecycleFixture$")
	rootCmd.Env = append(
		os.Environ(),
		windowsProcessLifecycleFixtureMode+"=console-root",
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
	require.GreaterOrEqual(t, elapsed, signalAndWaitTimeout-time.Second)
	require.Less(t, elapsed, signalAndWaitTimeout+3*time.Second)
	_, markerErr := os.Stat(markerPath)
	require.ErrorIs(t, markerErr, os.ErrNotExist,
		"unknown descendant should be force-killed when it outlives the six-second passive fallback")
	require.NoError(t, rootCmd.Wait())
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(childHandle)))
}

// Verifies that a confirmed CTRL_C_EVENT lets the root exit on its own, after which an
// unknown descendant gets a separate six-second passive fallback rather than 15 seconds.
func TestStopViaConsoleUsesPassiveFallbackAfterDelayedRootExit(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	rootCmd := exec.Command(os.Args[0], "-test.run=^TestWindowsProcessLifecycleFixture$")
	rootCmd.Env = append(os.Environ(), windowsProcessLifecycleFixtureMode+"=delayed-console-root")
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
	require.GreaterOrEqual(t, elapsed, delayedRootExitDuration+signalAndWaitTimeout-time.Second)
	require.Less(t, elapsed, gracefulProcessStopTimeout)
	require.NoError(t, rootCmd.Wait())
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(childHandle)))
}

// Verifies that a new-console process stopped without an attach helper, and a detached
// process for which AttachConsole cannot succeed, both use the six-second passive fallback.
func TestWindowsUnconfirmedConsoleDeliveryUsesPassiveFallback(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*exec.Cmd)
		stop      func(context.Context, *OSExecutor, ProcessHandle) error
	}{
		{
			name: "new console without helper",
			configure: func(command *exec.Cmd) {
				ForkFromParent(command)
			},
			stop: func(ctx context.Context, executor *OSExecutor, handle ProcessHandle) error {
				return executor.StopProcess(ctx, handle, StopRootOnly())
			},
		},
		{
			name: "detached process attach failure",
			configure: func(command *exec.Cmd) {
				command.SysProcAttr = &syscall.SysProcAttr{
					CreationFlags: windows.DETACHED_PROCESS,
				}
			},
			stop: func(ctx context.Context, executor *OSExecutor, handle ProcessHandle) error {
				return StopViaConsole(ctx, logr.Discard(), executor, handle, StopRootOnly())
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			testCtx, testCancel := testutil.GetTestContext(t, 20*time.Second)
			defer testCancel()
			executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
			defer executor.Dispose()
			rootCmd := exec.Command(os.Args[0], "-test.run=^TestWindowsProcessLifecycleFixture$")
			rootCmd.Env = append(os.Environ(), windowsProcessLifecycleFixtureMode+"=signal-resistant-root")
			testCase.configure(rootCmd)
			rootStdout, stdoutErr := rootCmd.StdoutPipe()
			require.NoError(t, stdoutErr)
			rootHandle, startWaiting, startErr := executor.StartProcess(
				testCtx,
				rootCmd,
				nil,
				CreationFlagsNone,
				nil,
			)
			require.NoError(t, startErr)
			startWaiting()
			readyText, readyErr := bufio.NewReader(rootStdout).ReadString('\n')
			require.NoError(t, readyErr)
			require.Equal(t, "ready\n", readyText)

			startedAt := time.Now()
			stopErr := testCase.stop(testCtx, executor, rootHandle)
			elapsed := time.Since(startedAt)

			require.NoError(t, stopErr)
			require.GreaterOrEqual(t, elapsed, signalAndWaitTimeout-time.Second)
			require.Less(t, elapsed, signalAndWaitTimeout+3*time.Second)
			require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(rootHandle)))
		})
	}
}

// Verifies that a root-only console stop force-kills a signal-resistant root
// and confirms its exit within the bounded full stop window.
func TestStopViaConsoleRootOnlyBoundsForceConfirmation(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	rootCmd := exec.Command(os.Args[0], "-test.run=^TestWindowsProcessLifecycleFixture$")
	rootCmd.Env = append(os.Environ(), windowsProcessLifecycleFixtureMode+"=signal-resistant-root")
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
	require.GreaterOrEqual(t, elapsed, gracefulProcessStopTimeout)
	require.Less(t, elapsed, processStopTimeout)
	waitErr := rootCmd.Wait()
	require.True(t, waitErr == nil || IsEarlyProcessExitError(waitErr))
	require.True(t, IsProcessGoneErr(executor.CheckProcessRunning(rootHandle)))
}
