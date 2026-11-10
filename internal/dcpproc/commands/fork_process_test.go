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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	cmds "github.com/microsoft/dcp/internal/commands"
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
	stopCalls        atomic.Int32
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
	executor.stopCalls.Add(1)
	if executor.stopFailure != nil {
		return executor.stopFailure
	}
	return executor.Executor.StopProcess(ctx, handle, options...)
}

type recordingForkLogSink struct {
	lock     sync.Mutex
	messages []string
}

func (sink *recordingForkLogSink) Init(logr.RuntimeInfo) {
}

func (sink *recordingForkLogSink) Enabled(int) bool {
	return true
}

func (sink *recordingForkLogSink) Info(_ int, message string, _ ...any) {
	sink.record(message)
}

func (sink *recordingForkLogSink) Error(_ error, message string, _ ...any) {
	sink.record(message)
}

func (sink *recordingForkLogSink) WithValues(...any) logr.LogSink {
	return sink
}

func (sink *recordingForkLogSink) WithName(string) logr.LogSink {
	return sink
}

func (sink *recordingForkLogSink) record(message string) {
	sink.lock.Lock()
	defer sink.lock.Unlock()
	sink.messages = append(sink.messages, message)
}

func (sink *recordingForkLogSink) recordedMessages() []string {
	sink.lock.Lock()
	defer sink.lock.Unlock()
	return append([]string(nil), sink.messages...)
}

func (sink *recordingForkLogSink) hasMessagePrefix(prefix string) bool {
	for _, message := range sink.recordedMessages() {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}
	return false
}

const ownerExitLogPrefix = "Monitored process exited"

type forkedProcessSupervisor func(*cobra.Command, *exec.Cmd, context.Context, process.Executor, logr.Logger) error

func supervisorForMode(cleanupEnabled bool) forkedProcessSupervisor {
	if cleanupEnabled {
		return superviseForkedProcessWithCleanup
	}
	return superviseForkedProcess
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
	payload []byte
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
	writer.payload = append([]byte(nil), payload...)
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

// Verifies that the fork-process supervisors stop the child (in cleanup mode only) when owner exit or command
// cancellation occurs during real process creation, and that owner exit is logged only if the owner actually exited.
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
				ownerCtx, ownerCancel, monitorErr := cmds.MonitorPidFromFlags(commandCtx, logr.Discard())
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
				sink := &recordingForkLogSink{}
				runErr := supervisorForMode(cleanupEnabled)(command, childCmd, ownerCtx, startExecutor, logr.New(sink))
				if cancellation {
					require.ErrorIs(t, runErr, context.Canceled)
					require.False(t, sink.hasMessagePrefix(ownerExitLogPrefix), "cancellation must not be logged as owner exit: %v", sink.recordedMessages())
				} else {
					require.NoError(t, runErr)
					require.True(t, sink.hasMessagePrefix(ownerExitLogPrefix), "owner exit was not logged: %v", sink.recordedMessages())
				}
				if cleanupEnabled {
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

// Verifies that superviseForkedProcessWithCleanup stops its child when owner exit or command cancellation occurs
// while PID output is blocked, logs owner exit only if the owner actually exited, and lets the best-effort write
// finish later.
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
			ownerCtx, ownerCancel, monitorErr := cmds.MonitorPidFromFlags(commandCtx, logr.Discard())
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
			sink := &recordingForkLogSink{}
			runResultCh := make(chan error, 1)
			go func() {
				runResultCh <- superviseForkedProcessWithCleanup(command, childCmd, ownerCtx, startExecutor, logr.New(sink))
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
				require.False(t, sink.hasMessagePrefix(ownerExitLogPrefix), "cancellation must not be logged as owner exit: %v", sink.recordedMessages())
			} else {
				require.NoError(t, runErr)
				require.True(t, sink.hasMessagePrefix(ownerExitLogPrefix), "owner exit was not logged: %v", sink.recordedMessages())
			}
			require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(child)))
			close(writer.release)
			select {
			case <-writer.done:
			case <-testCtx.Done():
				t.Fatal(testCtx.Err())
			}
			require.Equal(t, strconv.FormatInt(int64(child.Pid), 10)+"\n", string(writer.payload))
		})
	}
}

// Verifies that the fork-process supervisors return terminal owner-monitoring failures, without logging them
// as owner exit, only when cleanup is enabled, preserving the existing successful default-mode behavior.
func TestForkProcessReturnsOwnerMonitoringFailure(t *testing.T) {
	for _, cleanupEnabled := range []bool{false, true} {
		t.Run("cleanup="+strconv.FormatBool(cleanupEnabled), func(t *testing.T) {
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
			defer func() { stopForkTestProcess(testCtx, executor, child) }()
			startExecutor := &forkStartExecutor{
				Executor:   executor,
				afterStart: func(handle process.ProcessHandle) { child = handle },
			}
			childCmd := forkTestDelayCommand(t, "--delay=2m")
			sink := &recordingForkLogSink{}
			runResultCh := make(chan error, 1)
			go func() {
				runResultCh <- supervisorForMode(cleanupEnabled)(command, childCmd, monitorCtx, startExecutor, logr.New(sink))
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
			if cleanupEnabled {
				require.ErrorIs(t, runErr, monitorErr)
				require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(child)))
				require.False(t, sink.hasMessagePrefix(ownerExitLogPrefix), "monitoring failure must not be logged as owner exit: %v", sink.recordedMessages())
			} else {
				require.NoError(t, runErr)
				require.NoError(t, executor.CheckProcessRunning(child))
			}
		})
	}
}

// Verifies that the fork-process supervisors roll back a failed PID handoff only when cleanup is enabled,
// leave descendants alone, and surface both output and cleanup failures.
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
			runErr := supervisorForMode(cleanupMode != "disabled")(command, childCmd, testCtx, startExecutor, logr.Discard())
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

// Verifies that superviseForkedProcessWithCleanup keeps the original child identity when cleanup runs,
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
	runErr := superviseForkedProcessWithCleanup(command, forkTestDelayCommand(t, "--delay=2m"), testCtx, startExecutor, logr.Discard())
	require.ErrorIs(t, runErr, writeErr)
	require.NoError(t, executor.CheckProcessRunning(actualChild), "cleanup must not recapture identity from a bare PID")
}

// Verifies that superviseForkedProcessWithCleanup does not try to stop a child process whose exit it observed,
// including a child process killed by a signal, and returns the child exit code.
func TestForkProcessCleanupSkipsStopAfterObservedChildExit(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()
	command, commandErr := NewForkProcessCommand(logr.Discard())
	require.NoError(t, commandErr)
	command.SetContext(testCtx)
	writer := &observedForkPidWriter{written: make(chan struct{})}
	command.SetOut(writer)
	executor := process.NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	var child process.ProcessHandle
	defer func() { stopForkTestProcess(testCtx, executor, child) }()
	stopErr := errors.New("injected process stop failure")
	startExecutor := &forkStartExecutor{
		Executor:    executor,
		afterStart:  func(handle process.ProcessHandle) { child = handle },
		stopFailure: stopErr,
	}
	childCmd := forkTestDelayCommand(t, "--delay=2m")
	runResultCh := make(chan error, 1)
	go func() {
		runResultCh <- superviseForkedProcessWithCleanup(command, childCmd, testCtx, startExecutor, logr.Discard())
	}()
	select {
	case <-writer.written:
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	childProcess, childProcessErr := child.OsProcess()
	require.NoError(t, childProcessErr)
	require.NoError(t, childProcess.Kill())
	var runErr error
	select {
	case runErr = <-runResultCh:
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	var exitCodeErr interface{ ExitCode() int }
	require.ErrorAs(t, runErr, &exitCodeErr)
	expectedExitCode := int(process.UnknownExitCode) // A process killed by a signal has no exit code.
	if osutil.IsWindows() {
		expectedExitCode = 1 // Process.Kill() terminates the process with exit code 1.
	}
	require.Equal(t, expectedExitCode, exitCodeErr.ExitCode())
	require.NotErrorIs(t, runErr, stopErr)
	require.Zero(t, startExecutor.stopCalls.Load(), "an exited child process must not be stopped")
}

// Verifies that fork-process accepts cleanup only with a monitor, fails without starting the child process when
// the --monitor value is unusable (with or without cleanup), and that rejected watch, status, and supervisor flags
// remain absent.
func TestForkProcessFlagValidation(t *testing.T) {
	for _, testCase := range []struct {
		args []string
		err  string
	}{
		{args: []string{"--enable-child-cleanup"}, err: "--enable-child-cleanup requires --monitor"},
		{args: []string{"--monitor=0"}, err: "invalid process handle"},
		{args: []string{"--monitor=-1"}, err: "invalid process handle"},
		{args: []string{"--monitor=0", "--enable-child-cleanup"}, err: "invalid process handle"},
		{args: []string{"--monitor=-1", "--enable-child-cleanup"}, err: "invalid process handle"},
	} {
		command, commandErr := NewForkProcessCommand(logr.Discard())
		require.NoError(t, commandErr)
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(io.Discard)
		command.SetArgs(append(testCase.args, "--", "unused-command"))
		runErr := command.ExecuteContext(context.Background())
		require.ErrorContains(t, runErr, testCase.err, "args: %v", testCase.args)
		require.Empty(t, output.String(), "args: %v", testCase.args)
		for _, removedFlag := range []string{"watch", "status-file", "watch-supervisor"} {
			require.Nil(t, command.Flags().Lookup(removedFlag))
		}
	}
}

// Verifies that fork-process returns the cancellation error without starting the child process when it is
// cancelled before the child process starts, with or without cleanup.
func TestForkProcessCancelledBeforeStart(t *testing.T) {
	owner, ownerErr := process.FindProcessHandle(process.Pid_t(os.Getpid()))
	require.NoError(t, ownerErr)
	for _, cleanupEnabled := range []bool{false, true} {
		t.Run("cleanup="+strconv.FormatBool(cleanupEnabled), func(t *testing.T) {
			commandCtx, commandCancel := context.WithCancel(context.Background())
			commandCancel()
			command, commandErr := NewForkProcessCommand(logr.Discard())
			require.NoError(t, commandErr)
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(io.Discard)
			args := []string{
				"--monitor", strconv.FormatInt(int64(owner.Pid), 10),
				"--monitor-identity-time", owner.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
			}
			if cleanupEnabled {
				args = append(args, "--enable-child-cleanup")
			}
			// Starting a missing command would fail with a different error.
			command.SetArgs(append(args, "--", filepath.Join(t.TempDir(), "missing-command")))
			runErr := command.ExecuteContext(commandCtx)
			require.ErrorIs(t, runErr, context.Canceled)
			require.Empty(t, output.String())
		})
	}
}

// Verifies that forkOwnerMonitorResult and ownerExitedOutcome report command cancellation and owner monitoring
// failures as errors without an owner exit diagnostic, and treat owner exit, including a gone or reused owner PID,
// as success that is logged.
func TestForkOwnerMonitorResult(t *testing.T) {
	monitorFailure := errors.New("injected owner monitoring failure")
	for _, testCase := range []struct {
		name          string
		cancelCommand bool
		ownerCause    error
		expectedErr   error
	}{
		{name: "owner-exited", ownerCause: context.Canceled},
		{name: "owner-gone", ownerCause: &process.ErrProcessNotFound{Pid: 1234}},
		{name: "owner-pid-reused", ownerCause: process.ErrProcessIdentityMismatch},
		{name: "monitoring-failed", ownerCause: monitorFailure, expectedErr: monitorFailure},
		{name: "command-cancelled", cancelCommand: true, expectedErr: context.Canceled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			commandCtx, commandCancel := context.WithCancel(context.Background())
			defer commandCancel()
			ownerCtx, ownerCancel := context.WithCancelCause(commandCtx)
			defer ownerCancel(nil)
			if testCase.cancelCommand {
				commandCancel()
			} else {
				ownerCancel(testCase.ownerCause)
			}

			resultErr := forkOwnerMonitorResult(commandCtx, ownerCtx)
			outcome := ownerExitedOutcome(commandCtx, ownerCtx, 1234, "Monitored process exited")
			sink := &recordingForkLogSink{}
			outcome.logDiagnostic(logr.New(sink))
			require.False(t, outcome.childExited)
			if testCase.expectedErr == nil {
				require.NoError(t, resultErr)
				require.NoError(t, outcome.err)
				require.Equal(t, []string{"Monitored process exited"}, sink.recordedMessages())
			} else {
				require.ErrorIs(t, resultErr, testCase.expectedErr)
				require.ErrorIs(t, outcome.err, testCase.expectedErr)
				require.Empty(t, sink.recordedMessages())
			}
		})
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
