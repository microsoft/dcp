/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type forkProcessRun struct {
	command *exec.Cmd
	output  *dcpProcOutput
	exit    <-chan error
}

// Verifies that fork-process cleans up its real child after owner exit only when enabled,
// while both cleanup policies retain the existing PID-only stdout contract.
func TestForkProcessChildCleanupWhenOwnerExits(t *testing.T) {
	for _, cleanupMode := range []string{"disabled", "tree", "root"} {
		t.Run(cleanupMode, func(t *testing.T) {
			t.Parallel()
			testCtx, testCancel := testutil.GetTestContext(t, 0)
			defer testCancel()
			ownerCmd, owner := startCleanupOwner(t, testCtx)
			readyPath := filepath.Join(t.TempDir(), "child-ready")
			fork := startCleanupFork(t, testCtx, owner, cleanupMode, "--delay=2m", "--ready-file="+readyPath)
			child := waitCleanupForkChild(t, testCtx, fork)
			waitCleanupFile(t, testCtx, readyPath)
			require.NoError(t, ownerCmd.Process.Kill())
			require.NoError(t, waitCleanupForkExit(t, testCtx, fork), fork.output.Stderr())
			executor := process.NewOSExecutor(logr.Discard())
			defer executor.Dispose()
			if cleanupMode == "disabled" {
				require.NoError(t, executor.CheckProcessRunning(child))
			} else {
				require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(child)))
			}
			require.Equal(t, strconv.FormatInt(int64(child.Pid), 10)+"\n", fork.stdout())
		})
	}
}

// Verifies that fork-process never starts a child for an already-gone or reused owner,
// with or without cleanup, and does not interfere with the unrelated live owner PID.
func TestForkProcessCleanupRejectsGoneOrReusedOwner(t *testing.T) {
	for _, cleanupMode := range []string{"disabled", "root"} {
		for _, reused := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s,reused=%t", cleanupMode, reused), func(t *testing.T) {
				t.Parallel()
				testCtx, testCancel := testutil.GetTestContext(t, 0)
				defer testCancel()
				ownerCmd, owner := startCleanupOwner(t, testCtx)
				originalOwner := owner
				if reused {
					owner.IdentityTime = owner.IdentityTime.Add(time.Hour)
				} else {
					require.NoError(t, ownerCmd.Process.Kill())
					waitCleanupProcessGone(t, testCtx, owner)
				}
				readyPath := filepath.Join(t.TempDir(), "must-not-start")
				fork := startCleanupFork(t, testCtx, owner, cleanupMode, "--delay=2m", "--ready-file="+readyPath)
				require.NoError(t, waitCleanupForkExit(t, testCtx, fork), fork.output.Stderr())
				require.Empty(t, fork.stdout())
				_, readyErr := os.Stat(readyPath)
				require.ErrorIs(t, readyErr, os.ErrNotExist)
				if reused {
					executor := process.NewOSExecutor(logr.Discard())
					defer executor.Dispose()
					require.NoError(t, executor.CheckProcessRunning(originalOwner))
				}
			})
		}
	}
}

// Verifies that fork-process surfaces failed native creation with cleanup enabled,
// without printing a child PID or launching a second process.
func TestForkProcessCleanupReportsFailedStart(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()
	dcpPath, dcpPathErr := getDcpProcExecutablePath()
	require.NoError(t, dcpPathErr)
	commandArgs := forkProcessArgsForCurrentProcess(t, filepath.Join(t.TempDir(), "missing-command"))
	commandArgs = append([]string{"fork-process", "--enable-child-cleanup"}, commandArgs[1:]...)
	command := exec.CommandContext(testCtx, dcpPath, commandArgs...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, runErr, &exitErr)
	require.NotZero(t, exitErr.ExitCode())
	require.Contains(t, stderr.String(), "could not start forked process")
	require.Empty(t, stdout.String())
}

func startCleanupOwner(t *testing.T, ctx context.Context) (*exec.Cmd, process.ProcessHandle) {
	t.Helper()
	delayPath, delayErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayErr)
	command := exec.Command(delayPath, "--delay=2m")
	process.ForkFromParent(command)
	require.NoError(t, command.Start())
	handle, handleErr := process.ProcessHandleFromCmd(command)
	require.NoError(t, handleErr)
	exited := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		stopCleanupProcess(ctx, handle)
		select {
		case <-exited:
		case <-ctx.Done():
		}
	})
	return command, handle
}

func startCleanupFork(t *testing.T, ctx context.Context, owner process.ProcessHandle, cleanupMode string, childArgs ...string) *forkProcessRun {
	t.Helper()
	dcpPath, dcpPathErr := getDcpProcExecutablePath()
	require.NoError(t, dcpPathErr)
	delayPath, delayErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayErr)
	commandArgs := []string{"fork-process", "--monitor", strconv.FormatInt(int64(owner.Pid), 10),
		"--monitor-identity-time", owner.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		"--monitor-interval", "1"}
	if cleanupMode != "disabled" {
		commandArgs = append(commandArgs, "--enable-child-cleanup")
	}
	if cleanupMode == "root" {
		commandArgs = append(commandArgs, "--skip-descendants")
	}
	commandArgs = append(commandArgs, "--", delayPath)
	commandArgs = append(commandArgs, childArgs...)
	command := exec.CommandContext(ctx, dcpPath, commandArgs...)
	process.ForkFromParent(command)
	output := &dcpProcOutput{}
	command.Stdout = output.StdoutWriter()
	command.Stderr = output.StderrWriter()
	require.NoError(t, command.Start())
	handle, handleErr := process.ProcessHandleFromCmd(command)
	require.NoError(t, handleErr)
	exit := make(chan error, 1)
	go func() { exit <- command.Wait() }()
	t.Cleanup(func() {
		stopCleanupProcess(ctx, handle)
		output.DumpOnFailure(t, "fork-process")
	})
	return &forkProcessRun{command: command, output: output, exit: exit}
}

func (fork *forkProcessRun) stdout() string {
	fork.output.mu.Lock()
	defer fork.output.mu.Unlock()
	return fork.output.stdout.String()
}

func waitCleanupForkChild(t *testing.T, ctx context.Context, fork *forkProcessRun) process.ProcessHandle {
	t.Helper()
	var child process.ProcessHandle
	pollErr := wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		output := fork.stdout()
		if !strings.HasSuffix(output, "\n") {
			return false, nil
		}
		childPID := parseForkedPid(t, output)
		var handleErr error
		child, handleErr = process.FindProcessHandle(childPID)
		return handleErr == nil, handleErr
	})
	require.NoError(t, pollErr, fork.output.Stderr())
	t.Cleanup(func() { stopCleanupProcess(ctx, child) })
	return child
}

func waitCleanupForkExit(t *testing.T, ctx context.Context, fork *forkProcessRun) error {
	t.Helper()
	select {
	case runErr, received := <-fork.exit:
		require.True(t, received)
		return runErr
	case <-ctx.Done():
		t.Fatalf("fork-process did not exit: %v; stderr: %s", ctx.Err(), fork.output.Stderr())
		return ctx.Err()
	}
}

func waitCleanupFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	pollErr := wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		_, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		return statErr == nil, statErr
	})
	require.NoError(t, pollErr)
}

func waitCleanupProcessGone(t *testing.T, ctx context.Context, handle process.ProcessHandle) {
	t.Helper()
	executor := process.NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	pollErr := wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		checkErr := executor.CheckProcessRunning(handle)
		if process.IsProcessGoneErr(checkErr) {
			return true, nil
		}
		return false, checkErr
	})
	require.NoError(t, pollErr)
}

func stopCleanupProcess(parent context.Context, handle process.ProcessHandle) {
	executor := process.NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	stopCtx, stopCancel := process.WithDetachedStopTimeout(parent)
	defer stopCancel()
	if executor.CheckProcessRunning(handle) == nil {
		_ = executor.StopProcess(stopCtx, handle, process.StopRootOnly())
	}
}
