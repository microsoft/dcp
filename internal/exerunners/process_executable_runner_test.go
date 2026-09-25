/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/internal/termpty"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	defaultExerunnerTestTimeout = 20 * time.Second
)

func TestProcessExecutableRunnerStartsLifecycleMonitor(t *testing.T) {
	monitorPID := int64(12345)
	monitorTimestamp := metav1.NewMicroTime(time.Now().Add(-time.Minute))
	testCases := []struct {
		name                  string
		persistent            bool
		monitorPID            *int64
		monitorTimestamp      metav1.MicroTime
		expectedMonitorStarts int
	}{
		{
			name:                  "non-persistent executable starts monitor",
			expectedMonitorStarts: 1,
		},
		{
			name:       "persistent executable skips monitor",
			persistent: true,
		},
		{
			name:                  "persistent executable with monitor starts monitor",
			persistent:            true,
			monitorPID:            &monitorPID,
			monitorTimestamp:      monitorTimestamp,
			expectedMonitorStarts: 1,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			dcppaths.EnableTestPathProbing()

			ctx, cancel := testutil.GetTestContext(t, defaultExerunnerTestTimeout)
			defer cancel()

			processExecutor := internal_testutil.NewTestProcessExecutor(ctx)
			runner := NewProcessExecutableRunner(processExecutor)
			persistentOutputDir := ""
			if testCase.persistent {
				persistentOutputDir = overridePersistentExecutableOutputDir(t)
			}
			exe := &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "api",
					Namespace: "default",
					UID:       "api-uid",
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath:   "/test/app",
					Persistent:       testCase.persistent,
					MonitorPID:       testCase.monitorPID,
					MonitorTimestamp: testCase.monitorTimestamp,
				},
			}

			result := runner.StartRun(ctx, exe, newRecordingRunChangeHandler(), logr.Discard())

			require.Equal(t, apiv1.ExecutableStateRunning, result.ExeState)
			t.Cleanup(func() {
				require.NoError(t, runner.ReleaseRun(context.Background(), result.RunID, logr.Discard()))
				removeFileIfExists(t, result.StdOutFile)
				removeFileIfExists(t, result.StdErrFile)
			})
			if testCase.persistent {
				require.Equal(t, persistentOutputDir, filepath.Dir(result.StdOutFile))
				require.Equal(t, persistentOutputDir, filepath.Dir(result.StdErrFile))
			}
			require.Len(t, processExecutor.FindAll([]string{"/test/app"}, "", nil), 1)
			monitorProcesses := processExecutor.FindAll([]string{"dcp", "monitor-process"}, "", nil)
			require.Len(t, monitorProcesses, testCase.expectedMonitorStarts)
			if testCase.monitorPID != nil {
				require.Contains(t, monitorProcesses[0].Cmd.Args, "--monitor")
				require.Contains(t, monitorProcesses[0].Cmd.Args, strconv.FormatInt(*testCase.monitorPID, 10))
				require.Contains(t, monitorProcesses[0].Cmd.Args, "--monitor-identity-time")
				require.Contains(t, monitorProcesses[0].Cmd.Args, testCase.monitorTimestamp.Time.Format(osutil.RFC3339MiliTimestampFormat))
			}
		})
	}
}

func TestPersistentExecutableOutputFileUsesPersistentOutputDir(t *testing.T) {
	persistentOutputDir := overridePersistentExecutableOutputDir(t)
	exe := &apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api/name",
			Namespace: "default",
			UID:       "api-uid",
		},
		Spec: apiv1.ExecutableSpec{
			LifecycleKey: "life/key",
			Persistent:   true,
		},
	}

	file, fileErr := openExecutableOutputFile(exe, "out")
	require.NoError(t, fileErr)
	t.Cleanup(func() {
		require.NoError(t, file.Close())
		removeFileIfExists(t, file.Name())
	})

	require.Equal(t, persistentOutputDir, filepath.Dir(file.Name()))
	require.Equal(t, "api-uid_out", filepath.Base(file.Name()))
}

func TestPersistentExecutableOutputBaseDirCanBeConfigured(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "custom-peo")
	t.Setenv(DCP_PERSISTENT_EXECUTABLE_OUTPUT_DIR, outputDir)

	require.Equal(t, outputDir, persistentExecutableOutputBaseDir())
}

func TestPersistentExecutableOutputBaseDirDefaultsForEmptyEnvVar(t *testing.T) {
	t.Setenv(DCP_PERSISTENT_EXECUTABLE_OUTPUT_DIR, " ")

	require.Equal(t, filepath.Join(os.TempDir(), persistentExecutableOutputDirName), persistentExecutableOutputBaseDir())
}

func TestProcessExecutableRunnerSkipsTimestampsForPersistentOutput(t *testing.T) {
	testCases := []struct {
		name       string
		persistent bool
	}{
		{
			name: "non-persistent output is timestamped",
		},
		{
			name:       "persistent output is written directly",
			persistent: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			dcppaths.EnableTestPathProbing()

			ctx, cancel := testutil.GetTestContext(t, defaultExerunnerTestTimeout)
			defer cancel()

			if testCase.persistent {
				overridePersistentExecutableOutputDir(t)
			}
			processExecutor := internal_testutil.NewTestProcessExecutor(ctx)
			runner := NewProcessExecutableRunner(processExecutor)
			exe := &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "api",
					Namespace: "default",
					UID:       types.UID(fmt.Sprintf("api-uid-%d", time.Now().UnixNano())),
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "/test/app",
					Persistent:     testCase.persistent,
				},
			}

			result := runner.StartRun(ctx, exe, newRecordingRunChangeHandler(), logr.Discard())
			require.Equal(t, apiv1.ExecutableStateRunning, result.ExeState)
			t.Cleanup(func() {
				require.NoError(t, runner.ReleaseRun(context.Background(), result.RunID, logr.Discard()))
				removeFileIfExists(t, result.StdOutFile)
				removeFileIfExists(t, result.StdErrFile)
			})

			executions := processExecutor.FindAll([]string{"/test/app"}, "", nil)
			require.Len(t, executions, 1)
			_, writeErr := executions[0].Cmd.Stdout.Write([]byte("hello\n"))
			require.NoError(t, writeErr)
			if syncer, ok := executions[0].Cmd.Stdout.(interface{ Sync() error }); ok {
				require.NoError(t, syncer.Sync())
			}

			output, readErr := os.ReadFile(result.StdOutFile)
			require.NoError(t, readErr)
			if testCase.persistent {
				require.Equal(t, "hello\n", string(output))
			} else {
				require.True(t, strings.HasPrefix(string(output), "1 "), "expected timestamped output, got %q", string(output))
				require.Contains(t, string(output), "hello\n")
			}
		})
	}
}

func TestAdoptedProcessStopUsesAdoptedPID(t *testing.T) {
	t.Parallel()

	dcppaths.EnableTestPathProbing()

	ctx, cancel := testutil.GetTestContext(t, defaultExerunnerTestTimeout)
	defer cancel()

	processExecutor := internal_testutil.NewTestProcessExecutor(ctx)
	runner := NewProcessExecutableRunner(processExecutor)
	runner.disableConsoleStop = true // We are just simulating the run, so stopping via dcpproc/console would fail.
	handle, _, startErr := processExecutor.StartProcess(ctx, exec.Command("./delay", "--delay=1s"), nil, process.CreationFlagsNone, nil)
	require.NoError(t, startErr)
	pid := handle.Pid
	adoptedRunID := controllers.RunID(pidToRunID(pid + 1))
	changeHandler := newRecordingRunChangeHandler()
	runner.runningProcesses.Store(adoptedRunID, &processRunState{
		handle:           handle,
		cmdInfo:          "./delay --delay=1s",
		adopted:          true,
		runChangeHandler: changeHandler,
	})

	require.NoError(t, runner.StopRun(ctx, adoptedRunID, logr.Discard()))

	execution, found := processExecutor.FindByPid(pid)
	require.True(t, found)
	require.True(t, execution.Finished())
	require.Equal(t, int32(internal_testutil.KilledProcessExitCode), execution.ExitCode)
}

func TestStopRunDoesNotRestoreCompletedRun(t *testing.T) {
	t.Parallel()

	stopExecutor := &stopRunTestExecutor{
		handle:         process.NewHandle(4242, time.Unix(1000, 0).UTC()),
		stopErr:        process.ErrIncompleteProcessTree,
		exitDuringStop: true,
	}
	runner, result, changeHandler := startStopRunTest(t, stopExecutor)

	stopErr := runner.StopRun(context.Background(), result.RunID, logr.Discard())
	require.ErrorIs(t, stopErr, process.ErrIncompleteProcessTree)

	_, found := runner.runningProcesses.Load(result.RunID)
	require.False(t, found)
	select {
	case completed := <-changeHandler.completedRuns:
		require.Equal(t, result.RunID, completed.runID)
	default:
		require.Fail(t, "expected process completion notification")
	}
}

func TestStopRunPreservesStateAfterStopFailure(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("stop failed")
	stopExecutor := &stopRunTestExecutor{
		handle:  process.NewHandle(4243, time.Unix(1001, 0).UTC()),
		stopErr: expectedErr,
	}
	runner, result, _ := startStopRunTest(t, stopExecutor)

	stopErr := runner.StopRun(context.Background(), result.RunID, logr.Discard())
	require.ErrorIs(t, stopErr, expectedErr)

	stored, found := runner.runningProcesses.Load(result.RunID)
	require.True(t, found)
	require.Equal(t, stopExecutor.handle, stored.handle)
}

func TestStopRunCleanupErrorDoesNotRestoreState(t *testing.T) {
	t.Parallel()

	cleanupErr := errors.New("cleanup failed")
	stopExecutor := &stopRunTestExecutor{
		handle: process.NewHandle(4244, time.Unix(1002, 0).UTC()),
	}
	runner := NewProcessExecutableRunner(stopExecutor)
	runner.disableConsoleStop = true
	runID := pidToRunID(stopExecutor.handle.Pid)
	runner.runningProcesses.Store(runID, &processRunState{
		handle:  stopExecutor.handle,
		cmdInfo: "cleanup-error",
		ptp: &termpty.PseudoTerminalProcess{
			PTY: &stopRunErrorPTY{closeErr: cleanupErr},
		},
	})

	stopErr := runner.StopRun(context.Background(), runID, logr.Discard())
	require.ErrorIs(t, stopErr, cleanupErr)
	_, found := runner.runningProcesses.Load(runID)
	require.False(t, found)
}

func TestAdoptedProcessStartsLifecycleMonitor(t *testing.T) {
	t.Parallel()

	dcppaths.EnableTestPathProbing()

	monitorPID := int64(12345)
	monitorTimestamp := metav1.NewMicroTime(time.Now().Add(-time.Minute))
	testCases := []struct {
		name             string
		spec             apiv1.ExecutableSpec
		expectedMonitor  int
		expectedMonitorP *int64
	}{
		{
			name: "cleanup executable starts DCP cleanup monitor",
			spec: apiv1.ExecutableSpec{
				ExecutablePath: "/test/app",
				Mode:           apiv1.ExecutableModeCleanup,
			},
			expectedMonitor: 1,
		},
		{
			name: "persistent executable without monitor skips monitor",
			spec: apiv1.ExecutableSpec{
				ExecutablePath: "/test/app",
				Persistent:     true,
			},
		},
		{
			name: "persistent executable with monitor starts scoped monitor",
			spec: apiv1.ExecutableSpec{
				ExecutablePath:   "/test/app",
				Persistent:       true,
				MonitorPID:       &monitorPID,
				MonitorTimestamp: monitorTimestamp,
			},
			expectedMonitor:  1,
			expectedMonitorP: &monitorPID,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultExerunnerTestTimeout)
			defer cancel()

			processExecutor := internal_testutil.NewTestProcessExecutor(ctx)
			runner := NewProcessExecutableRunner(processExecutor)
			handle, _, startErr := processExecutor.StartProcess(ctx, exec.Command("/test/app"), nil, process.CreationFlagsNone, nil)
			require.NoError(t, startErr)
			pid := handle.Pid
			runID := pidToRunID(pid)
			t.Cleanup(func() {
				_ = runner.ReleaseRun(context.Background(), runID, logr.Discard())
				_ = processExecutor.StopProcess(context.Background(), handle)
			})

			exe := &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "api",
					Namespace: "default",
					UID:       types.UID(fmt.Sprintf("api-uid-%d", time.Now().UnixNano())),
				},
				Spec: testCase.spec,
			}
			record := &statestore.PersistentProcessRecord{
				RunID:        string(runID),
				PID:          pid,
				IdentityTime: handle.IdentityTime,
			}

			adoptErr := runner.AdoptRun(ctx, exe, record, newRecordingRunChangeHandler(), logr.Discard())
			require.NoError(t, adoptErr)

			monitorProcesses := processExecutor.FindAll([]string{"dcp", "monitor-process"}, "", nil)
			require.Len(t, monitorProcesses, testCase.expectedMonitor)
			if testCase.expectedMonitorP != nil {
				require.Contains(t, monitorProcesses[0].Cmd.Args, "--monitor")
				require.Contains(t, monitorProcesses[0].Cmd.Args, strconv.FormatInt(*testCase.expectedMonitorP, 10))
				require.Contains(t, monitorProcesses[0].Cmd.Args, "--monitor-identity-time")
				require.Contains(t, monitorProcesses[0].Cmd.Args, testCase.spec.MonitorTimestamp.Time.Format(osutil.RFC3339MiliTimestampFormat))
			}
		})
	}
}

func TestAdoptedProcessReportsCompletionWhenProcessExits(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultExerunnerTestTimeout)
	defer cancel()

	processExecutor := internal_testutil.NewTestProcessExecutor(ctx)
	cmd := exec.Command("./delay", "--delay=1s")
	handle, _, startProcessErr := processExecutor.StartProcess(ctx, cmd, nil, process.CreationFlagsNone, nil)
	require.NoError(t, startProcessErr)
	runner := NewProcessExecutableRunner(processExecutor)
	runID := pidToRunID(handle.Pid)
	changeHandler := newRecordingRunChangeHandler()
	exe := &apiv1.Executable{
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "./delay",
		},
	}
	record := &statestore.PersistentProcessRecord{
		RunID:        string(runID),
		PID:          handle.Pid,
		IdentityTime: handle.IdentityTime,
	}

	adoptErr := runner.AdoptRun(ctx, exe, record, changeHandler, logr.Discard())
	require.NoError(t, adoptErr)

	processExecutor.SimulateProcessExit(t, handle.Pid, 0)

	select {
	case completedRun := <-changeHandler.completedRuns:
		require.Equal(t, runID, completedRun.runID)
		require.Equal(t, apiv1.UnknownExitCode, completedRun.exitCode)
		require.NoError(t, completedRun.err)
	case <-ctx.Done():
		require.Fail(t, "timed out waiting for adopted process completion notification")
	}

	_, found := runner.runningProcesses.Load(runID)
	require.False(t, found)
}

func TestAdoptedProcessWatcherDoesNotDeleteReusedRunID(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultExerunnerTestTimeout)
	defer cancel()
	runner := NewProcessExecutableRunner(internal_testutil.NewTestProcessExecutor(ctx))
	runID := controllers.RunID("42")
	watchedPID := process.Pid_t(42)
	watchedIdentityTime := time.Unix(1, 0).UTC()
	reusedIdentityTime := watchedIdentityTime.Add(time.Minute)
	changeHandler := newRecordingRunChangeHandler()
	watchedHandle := process.NewHandle(watchedPID, watchedIdentityTime)
	reusedRunState := &processRunState{
		handle:           process.NewHandle(watchedPID, reusedIdentityTime),
		runChangeHandler: changeHandler,
	}
	runner.runningProcesses.Store(runID, reusedRunState)

	runner.watchAdoptedProcess(runID, watchedHandle, make(chan struct{}), logr.Discard())

	storedRunState, found := runner.runningProcesses.Load(runID)
	require.True(t, found)
	require.Same(t, reusedRunState, storedRunState)
	select {
	case completedRun := <-changeHandler.completedRuns:
		require.Failf(t, "unexpected completion notification", "received completion for run %s", completedRun.runID)
	default:
	}
}

func TestReleaseRunClosesProcessRunFiles(t *testing.T) {
	t.Parallel()

	stdOutFile, stdOutFileErr := usvc_io.CreateNewTempFile(fmt.Sprintf("stdout_%d", time.Now().UnixNano()), osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, stdOutFileErr)
	t.Cleanup(func() {
		require.NoError(t, os.Remove(stdOutFile.Name()))
	})
	stdErrFile, stdErrFileErr := usvc_io.CreateNewTempFile(fmt.Sprintf("stderr_%d", time.Now().UnixNano()), osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, stdErrFileErr)
	t.Cleanup(func() {
		require.NoError(t, os.Remove(stdErrFile.Name()))
	})

	ctx, cancel := testutil.GetTestContext(t, defaultExerunnerTestTimeout)
	defer cancel()
	runner := NewProcessExecutableRunner(internal_testutil.NewTestProcessExecutor(ctx))
	runID := controllers.RunID("run-1")
	runner.runningProcesses.Store(runID, &processRunState{
		stdOutFile: stdOutFile,
		stdErrFile: stdErrFile,
	})

	require.NoError(t, runner.ReleaseRun(context.Background(), runID, logr.Discard()))

	require.ErrorIs(t, stdOutFile.Close(), os.ErrClosed)
	require.ErrorIs(t, stdErrFile.Close(), os.ErrClosed)
	_, found := runner.runningProcesses.Load(runID)
	require.False(t, found)
}

func overridePersistentExecutableOutputDir(t *testing.T) string {
	t.Helper()

	outputDir := t.TempDir()
	originalOutputDir := persistentExecutableOutputDir
	persistentExecutableOutputDir = func() (string, error) {
		return outputDir, nil
	}
	t.Cleanup(func() {
		persistentExecutableOutputDir = originalOutputDir
	})
	return outputDir
}

func removeFileIfExists(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		return
	}
	removeErr := os.Remove(path)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		require.NoError(t, removeErr)
	}
}

// stopRunTestExecutor synchronously reports process exit before returning a configured stop error.
// TestProcessExecutor cannot model this ordering because its exit callbacks are asynchronous and StopError returns before exit.
type stopRunTestExecutor struct {
	handle         process.ProcessHandle
	handler        process.ProcessExitHandler
	stopErr        error
	exitDuringStop bool
}

func (executor *stopRunTestExecutor) StartProcess(
	_ context.Context,
	_ *exec.Cmd,
	handler process.ProcessExitHandler,
	_ process.ProcessCreationFlag,
	_ process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	executor.handler = handler
	return executor.handle, func() {}, nil
}

func (executor *stopRunTestExecutor) StopProcess(
	_ context.Context,
	handle process.ProcessHandle,
	_ ...process.ProcessStopOption,
) error {
	if executor.exitDuringStop && executor.handler != nil {
		executor.handler.OnProcessExited(handle.Pid, 0, nil)
	}
	return executor.stopErr
}

func (*stopRunTestExecutor) CheckProcessRunning(process.ProcessHandle) error {
	return nil
}

func (executor *stopRunTestExecutor) FindProcessHandle(process.Pid_t) (process.ProcessHandle, error) {
	return executor.handle, nil
}

func (executor *stopRunTestExecutor) StartAndForget(*exec.Cmd, process.ProcessCreationFlag) (process.ProcessHandle, error) {
	return executor.handle, nil
}

func (*stopRunTestExecutor) Dispose() {}

type stopRunErrorPTY struct {
	closeErr error
}

func (*stopRunErrorPTY) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (*stopRunErrorPTY) Write(data []byte) (int, error) {
	return len(data), nil
}

func (pty *stopRunErrorPTY) Close() error {
	return pty.closeErr
}

func (*stopRunErrorPTY) Resize(uint16, uint16) error {
	return nil
}

func startStopRunTest(
	t *testing.T,
	executor *stopRunTestExecutor,
) (*ProcessExecutableRunner, *controllers.ExecutableStartResult, *recordingRunChangeHandler) {
	t.Helper()

	runner := NewProcessExecutableRunner(executor)
	runner.disableConsoleStop = true
	changeHandler := newRecordingRunChangeHandler()
	result := runner.StartRun(
		context.Background(),
		&apiv1.Executable{
			ObjectMeta: metav1.ObjectMeta{
				Name: "stop-run-test",
				UID:  types.UID(fmt.Sprintf("stop-run-test-%d", time.Now().UnixNano())),
			},
			Spec: apiv1.ExecutableSpec{ExecutablePath: "unused"},
		},
		changeHandler,
		logr.Discard(),
	)
	t.Cleanup(func() {
		_ = runner.ReleaseRun(context.Background(), result.RunID, logr.Discard())
		removeFileIfExists(t, result.StdOutFile)
		removeFileIfExists(t, result.StdErrFile)
	})
	return runner, result, changeHandler
}

type completedRunNotification struct {
	runID    controllers.RunID
	exitCode *int32
	err      error
}

type recordingRunChangeHandler struct {
	completedRuns chan completedRunNotification
}

func newRecordingRunChangeHandler() *recordingRunChangeHandler {
	return &recordingRunChangeHandler{
		completedRuns: make(chan completedRunNotification, 1),
	}
}

func (*recordingRunChangeHandler) OnMainProcessChanged(controllers.RunID, process.Pid_t) {}

func (h *recordingRunChangeHandler) OnRunCompleted(runID controllers.RunID, exitCode *int32, err error) {
	h.completedRuns <- completedRunNotification{
		runID:    runID,
		exitCode: exitCode,
		err:      err,
	}
}

func (*recordingRunChangeHandler) OnStartupCompleted(types.NamespacedName, *controllers.ExecutableStartResult) {
}

func (*recordingRunChangeHandler) OnRunMessage(controllers.RunID, controllers.RunMessageLevel, string) {
}
