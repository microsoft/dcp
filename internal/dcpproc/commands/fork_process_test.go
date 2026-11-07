/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
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

type forkStartExecutor struct {
	process.Executor
	afterStart       func(process.ProcessHandle)
	mismatchIdentity bool
	stopFailure      error
}

func (executor *forkStartExecutor) StartProcess(
	ctx context.Context, cmd *exec.Cmd, handler process.ProcessExitHandler, creationFlags process.ProcessCreationFlag, creator process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	handle, startWait, startErr := executor.Executor.StartProcess(ctx, cmd, handler, creationFlags, creator)
	if startErr == nil {
		executor.afterStart(handle)
		if executor.mismatchIdentity {
			handle.IdentityTime = handle.IdentityTime.Add(time.Hour)
		}
	}
	return handle, startWait, startErr
}

func (executor *forkStartExecutor) StopProcess(ctx context.Context, handle process.ProcessHandle, options ...process.ProcessStopOption) error {
	if executor.stopFailure != nil {
		return executor.stopFailure
	}
	return executor.Executor.StopProcess(ctx, handle, options...)
}

type failedForkPidWriter struct {
	err error
}

func (writer failedForkPidWriter) Write([]byte) (int, error) {
	return 0, writer.err
}

type blockingForkPidWriter struct {
	started chan struct{}
	release chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newBlockingForkPidWriter() *blockingForkPidWriter {
	return &blockingForkPidWriter{
		started: make(chan struct{}),
		release: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (writer *blockingForkPidWriter) Write(payload []byte) (int, error) {
	writer.once.Do(func() {
		close(writer.started)
	})
	<-writer.release
	close(writer.done)
	return len(payload), nil
}

type observedForkPidWriter struct {
	written chan struct{}
	once    sync.Once
}

func (writer *observedForkPidWriter) Write(payload []byte) (int, error) {
	writer.once.Do(func() {
		close(writer.written)
	})
	return len(payload), nil
}

// Verifies that runForkedProcess applies opt-in root cleanup when owner exit or command
// cancellation occurs during real process creation, without changing default fork behavior.
func TestForkProcessCleanupDuringStartup(t *testing.T) {
	for _, cancellation := range []bool{false, true} {
		for _, cleanupEnabled := range []bool{false, true} {
			t.Run(strconv.FormatBool(cancellation)+"/cleanup="+strconv.FormatBool(cleanupEnabled), func(t *testing.T) {
				testCtx, testCancel := testutil.GetTestContext(t, 0)
				defer testCancel()
				commandCtx, commandCancel := context.WithCancel(testCtx)
				defer commandCancel()
				executor := process.NewOSExecutor(logr.Discard())
				defer executor.Dispose()
				ownerCmd, owner := startForkTestDelay(t, executor, "--delay=2m")
				defer stopForkTestProcess(testCtx, executor, owner)
				command, commandErr := NewForkProcessCommand(logr.Discard())
				require.NoError(t, commandErr)
				command.SetContext(commandCtx)
				require.NoError(t, command.Flags().Set("monitor", strconv.FormatInt(int64(owner.Pid), 10)))
				require.NoError(t, command.Flags().Set("monitor-identity-time", owner.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat)))
				require.NoError(t, command.Flags().Set("monitor-interval", "1"))
				ownerCtx, ownerCancel, monitorErr := monitorForkOwner(command, logr.Discard())
				require.NoError(t, monitorErr)
				defer ownerCancel()
				var output bytes.Buffer
				command.SetOut(&output)
				readyPath := filepath.Join(t.TempDir(), "child-ready")
				childCmd := forkTestDelayCommand(t, "--delay=2m", "--child-spec=1", "--fork-children", "--ready-file="+readyPath)
				var child process.ProcessHandle
				var tree []process.ProcessHandle
				defer func() {
					for _, handle := range tree {
						stopForkTestProcess(testCtx, executor, handle)
					}
				}()
				startExecutor := &forkStartExecutor{Executor: executor, afterStart: func(handle process.ProcessHandle) {
					child = handle
					waitForkTestReady(t, testCtx, readyPath)
					var treeErr error
					tree, treeErr = process.GetProcessTree(testCtx, handle)
					require.NoError(t, treeErr)
					if cancellation {
						commandCancel()
					} else {
						require.NoError(t, ownerCmd.Process.Kill())
					}
					<-ownerCtx.Done()
				}}
				options := &forkProcessOptions{enableChildCleanup: cleanupEnabled}
				runErr := runForkedProcess(command, childCmd, ownerCtx, options, startExecutor, logr.Discard())
				if cancellation {
					require.ErrorIs(t, runErr, context.Canceled)
				} else {
					require.NoError(t, runErr)
				}
				if options.enableChildCleanup {
					require.Empty(t, output.String(), "a child retired during startup should not be handed off")
					require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(child)))
				} else {
					require.Equal(t, strconv.FormatInt(int64(child.Pid), 10)+"\n", output.String())
					require.NoError(t, executor.CheckProcessRunning(child))
				}
				if !osutil.IsWindows() {
					require.Len(t, tree, 2)
					require.NoError(t, executor.CheckProcessRunning(tree[1]))
				}
			})
		}
	}
}

// Verifies that runForkedProcess stops its child when owner exit or command cancellation occurs
// while PID output is blocked, without waiting for the writer to become writable.
func TestForkProcessCleanupDuringBlockedPidHandoff(t *testing.T) {
	for _, cancellation := range []bool{false, true} {
		t.Run("cancel="+strconv.FormatBool(cancellation), func(t *testing.T) {
			testCtx, testCancel := testutil.GetTestContext(t, 0)
			defer testCancel()
			commandCtx, commandCancel := context.WithCancel(testCtx)
			defer commandCancel()
			executor := process.NewOSExecutor(logr.Discard())
			defer executor.Dispose()
			ownerCmd, owner := startForkTestDelay(t, executor, "--delay=2m")
			defer stopForkTestProcess(testCtx, executor, owner)
			command, commandErr := NewForkProcessCommand(logr.Discard())
			require.NoError(t, commandErr)
			command.SetContext(commandCtx)
			require.NoError(t, command.Flags().Set("monitor", strconv.FormatInt(int64(owner.Pid), 10)))
			require.NoError(t, command.Flags().Set("monitor-identity-time", owner.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat)))
			require.NoError(t, command.Flags().Set("monitor-interval", "1"))
			ownerCtx, ownerCancel, monitorErr := monitorForkOwner(command, logr.Discard())
			require.NoError(t, monitorErr)
			defer ownerCancel()
			writer := newBlockingForkPidWriter()
			command.SetOut(writer)
			var child process.ProcessHandle
			startExecutor := &forkStartExecutor{
				Executor:   executor,
				afterStart: func(handle process.ProcessHandle) { child = handle },
			}
			childCmd := forkTestDelayCommand(t, "--delay=2m")
			runResultCh := make(chan error, 1)
			go func() {
				runResultCh <- runForkedProcess(
					command,
					childCmd,
					ownerCtx,
					&forkProcessOptions{enableChildCleanup: true},
					startExecutor,
					logr.Discard(),
				)
			}()
			select {
			case <-writer.started:
			case <-testCtx.Done():
				t.Fatal(testCtx.Err())
			}
			if cancellation {
				commandCancel()
			} else {
				require.NoError(t, ownerCmd.Process.Kill())
			}
			var runErr error
			select {
			case runErr = <-runResultCh:
			case <-testCtx.Done():
				t.Fatal(testCtx.Err())
			}
			if cancellation {
				require.ErrorIs(t, runErr, context.Canceled)
			} else {
				require.NoError(t, runErr)
			}
			require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(child)))
			close(writer.release)
			select {
			case <-writer.done:
			case <-testCtx.Done():
				t.Fatal(testCtx.Err())
			}
		})
	}
}

// Verifies that runForkedProcess cleans up its child and returns a terminal owner-monitoring failure.
func TestForkProcessReturnsOwnerMonitoringFailure(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()
	command, commandErr := NewForkProcessCommand(logr.Discard())
	require.NoError(t, commandErr)
	command.SetContext(testCtx)
	writer := &observedForkPidWriter{written: make(chan struct{})}
	command.SetOut(writer)
	monitorCtx, monitorCancel := context.WithCancelCause(testCtx)
	defer monitorCancel(context.Canceled)
	executor := process.NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	var child process.ProcessHandle
	startExecutor := &forkStartExecutor{
		Executor:   executor,
		afterStart: func(handle process.ProcessHandle) { child = handle },
	}
	childCmd := forkTestDelayCommand(t, "--delay=2m")
	runResultCh := make(chan error, 1)
	go func() {
		runResultCh <- runForkedProcess(
			command,
			childCmd,
			monitorCtx,
			&forkProcessOptions{enableChildCleanup: true},
			startExecutor,
			logr.Discard(),
		)
	}()
	select {
	case <-writer.written:
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	monitorErr := errors.New("injected owner monitoring failure")
	monitorCancel(monitorErr)
	var runErr error
	select {
	case runErr = <-runResultCh:
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	require.ErrorIs(t, runErr, monitorErr)
	require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(child)))
}

// Verifies that runForkedProcess rolls back a failed PID handoff only when cleanup is enabled,
// leaves descendants alone, and surfaces both output and cleanup failures.
func TestForkProcessFailedPidHandoffCleanup(t *testing.T) {
	for _, cleanupMode := range []string{"disabled", "enabled", "stop-failed"} {
		t.Run(cleanupMode, func(t *testing.T) {
			testCtx, testCancel := testutil.GetTestContext(t, 0)
			defer testCancel()
			executor := process.NewOSExecutor(logr.Discard())
			defer executor.Dispose()
			command, commandErr := NewForkProcessCommand(logr.Discard())
			require.NoError(t, commandErr)
			command.SetContext(testCtx)
			writeErr := errors.New("injected PID output failure")
			command.SetOut(failedForkPidWriter{err: writeErr})
			readyPath := filepath.Join(t.TempDir(), "child-ready")
			childCmd := forkTestDelayCommand(t, "--delay=2m", "--child-spec=1", "--fork-children", "--ready-file="+readyPath)
			var child process.ProcessHandle
			var tree []process.ProcessHandle
			defer func() {
				for _, handle := range tree {
					stopForkTestProcess(testCtx, executor, handle)
				}
			}()
			startExecutor := &forkStartExecutor{Executor: executor, afterStart: func(handle process.ProcessHandle) {
				child = handle
				waitForkTestReady(t, testCtx, readyPath)
				var treeErr error
				tree, treeErr = process.GetProcessTree(testCtx, handle)
				require.NoError(t, treeErr)
			}}
			stopErr := errors.New("injected process stop failure")
			if cleanupMode == "stop-failed" {
				startExecutor.stopFailure = stopErr
			}
			options := &forkProcessOptions{enableChildCleanup: cleanupMode != "disabled"}
			runErr := runForkedProcess(command, childCmd, testCtx, options, startExecutor, logr.Discard())
			require.ErrorIs(t, runErr, writeErr)
			if cleanupMode == "stop-failed" {
				require.ErrorIs(t, runErr, stopErr)
			}
			if cleanupMode == "disabled" || cleanupMode == "stop-failed" {
				require.NoError(t, executor.CheckProcessRunning(child))
			} else {
				require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(child)))
			}
			if !osutil.IsWindows() {
				require.Len(t, tree, 2)
				require.NoError(t, executor.CheckProcessRunning(tree[1]))
			}
		})
	}
}

// Verifies that runForkedProcess keeps the original child identity when cleanup runs,
// so a live process using the same PID with a different identity is not stopped.
func TestForkProcessCleanupPreservesChildIdentity(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()
	executor := process.NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	command, commandErr := NewForkProcessCommand(logr.Discard())
	require.NoError(t, commandErr)
	command.SetContext(testCtx)
	writeErr := errors.New("injected PID output failure")
	command.SetOut(failedForkPidWriter{err: writeErr})
	var actualChild process.ProcessHandle
	defer func() { stopForkTestProcess(testCtx, executor, actualChild) }()
	startExecutor := &forkStartExecutor{
		Executor: executor, mismatchIdentity: true,
		afterStart: func(handle process.ProcessHandle) { actualChild = handle },
	}
	runErr := runForkedProcess(command, forkTestDelayCommand(t, "--delay=2m"), testCtx,
		&forkProcessOptions{enableChildCleanup: true}, startExecutor, logr.Discard())
	require.ErrorIs(t, runErr, writeErr)
	require.NoError(t, executor.CheckProcessRunning(actualChild), "cleanup must not recapture identity from a bare PID")
}

// Verifies that fork-process accepts cleanup only with a monitor and that rejected watch,
// status, and supervisor flags remain absent.
func TestForkProcessCleanupFlagValidation(t *testing.T) {
	for _, testCase := range []struct {
		args []string
		err  string
	}{
		{args: []string{"--enable-child-cleanup"}, err: "--enable-child-cleanup requires --monitor"},
		{args: []string{"--monitor=0", "--enable-child-cleanup"}, err: "invalid process handle"},
	} {
		command, commandErr := NewForkProcessCommand(logr.Discard())
		require.NoError(t, commandErr)
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		command.SetArgs(append(testCase.args, "--", "unused-command"))
		runErr := command.ExecuteContext(context.Background())
		require.ErrorContains(t, runErr, testCase.err)
		for _, removedFlag := range []string{"watch", "status-file", "watch-supervisor"} {
			require.Nil(t, command.Flags().Lookup(removedFlag))
		}
	}
}

func forkTestDelayCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	delayPath, delayErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayErr)
	command := exec.Command(delayPath, args...)
	process.ForkFromParent(command)
	return command
}

func startForkTestDelay(t *testing.T, executor process.Executor, args ...string) (*exec.Cmd, process.ProcessHandle) {
	t.Helper()
	command := forkTestDelayCommand(t, args...)
	handle, startWait, startErr := executor.StartProcess(context.Background(), command, nil, process.CreationFlagsNone, nil)
	require.NoError(t, startErr)
	startWait()
	return command, handle
}

func stopForkTestProcess(parent context.Context, executor process.Executor, handle process.ProcessHandle) {
	stopCtx, stopCancel := process.WithDetachedStopTimeout(parent)
	defer stopCancel()
	if handle.Validate() == nil && executor.CheckProcessRunning(handle) == nil {
		_ = executor.StopProcess(stopCtx, handle, process.StopRootOnly())
	}
}

func waitForkTestReady(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	readyErr := wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		_, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		return statErr == nil, statErr
	})
	require.NoError(t, readyErr)
}

var _ process.Executor = (*forkStartExecutor)(nil)
