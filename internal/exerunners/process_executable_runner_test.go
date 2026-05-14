/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/testutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
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

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			processExecutor := testutil.NewTestProcessExecutor(ctx)
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

			result := runner.StartRun(ctx, exe, &testRunChangeHandler{}, logr.Discard())

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

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if testCase.persistent {
				overridePersistentExecutableOutputDir(t)
			}
			processExecutor := testutil.NewTestProcessExecutor(ctx)
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

			result := runner.StartRun(ctx, exe, &testRunChangeHandler{}, logr.Discard())
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	processExecutor := &recordingProcessExecutor{}
	runner := NewProcessExecutableRunner(processExecutor)
	pid := process.Pid_t(42)
	identityTime := time.Now().UTC()
	originalRunID := pidToRunID(pid)
	adoptedRunID := controllers.RunID(fmt.Sprintf("%s-adopted", originalRunID))
	runner.runningProcesses.Store(adoptedRunID, &processRunState{
		pid:          pid,
		identityTime: identityTime,
		cmdInfo:      "/test/app",
		adopted:      true,
	})

	require.NoError(t, runner.StopRun(ctx, adoptedRunID, logr.Discard()))

	require.Equal(t, pid, processExecutor.stoppedPID)
	require.Equal(t, identityTime, processExecutor.stoppedIdentityTime)
}

func TestAdoptedProcessReportsCompletionWhenProcessExits(t *testing.T) {
	t.Parallel()

	delayToolDir, delayToolDirErr := testutil.GetTestToolDir("delay")
	require.NoError(t, delayToolDirErr)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "./delay", "--delay=1s")
	cmd.Dir = delayToolDir
	require.NoError(t, cmd.Start())
	go func() {
		_ = cmd.Wait()
	}()

	pid := process.Uint32_ToPidT(uint32(cmd.Process.Pid))
	identityTime := process.ProcessIdentityTime(pid)
	require.False(t, identityTime.IsZero())

	runner := NewProcessExecutableRunner(&recordingProcessExecutor{})
	runID := pidToRunID(pid)
	changeHandler := newRecordingRunChangeHandler()

	adoptErr := runner.AdoptRun(ctx, controllers.ExecutableRunAdoptionInfo{
		RunID:               runID,
		Pid:                 pid,
		ProcessIdentityTime: identityTime,
		CommandInfo:         "./delay --delay=1s",
	}, changeHandler, logr.Discard())
	require.NoError(t, adoptErr)

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

	runner := NewProcessExecutableRunner(&recordingProcessExecutor{})
	runID := controllers.RunID("42")
	watchedPID := process.Pid_t(42)
	watchedIdentityTime := time.Unix(1, 0).UTC()
	reusedIdentityTime := watchedIdentityTime.Add(time.Minute)
	changeHandler := newRecordingRunChangeHandler()
	reusedRunState := &processRunState{
		pid:              watchedPID,
		identityTime:     reusedIdentityTime,
		runChangeHandler: changeHandler,
	}
	runner.runningProcesses.Store(runID, reusedRunState)

	runner.watchAdoptedProcess(runID, watchedPID, watchedIdentityTime, make(chan struct{}), logr.Discard())

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

	stdOutFile, stdOutFileErr := usvc_io.OpenTempFile(fmt.Sprintf("stdout_%d", time.Now().UnixNano()), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, stdOutFileErr)
	t.Cleanup(func() {
		require.NoError(t, os.Remove(stdOutFile.Name()))
	})
	stdErrFile, stdErrFileErr := usvc_io.OpenTempFile(fmt.Sprintf("stderr_%d", time.Now().UnixNano()), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, stdErrFileErr)
	t.Cleanup(func() {
		require.NoError(t, os.Remove(stdErrFile.Name()))
	})

	runner := NewProcessExecutableRunner(&recordingProcessExecutor{})
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

type recordingProcessExecutor struct {
	stoppedPID          process.Pid_t
	stoppedIdentityTime time.Time
}

func (e *recordingProcessExecutor) StartProcess(_ context.Context, _ *exec.Cmd, _ process.ProcessExitHandler, _ process.ProcessCreationFlag) (process.Pid_t, time.Time, func(), error) {
	return process.UnknownPID, time.Time{}, nil, fmt.Errorf("process start is not supported by recordingProcessExecutor")
}

func (e *recordingProcessExecutor) StopProcess(pid process.Pid_t, processStartTime time.Time, _ ...process.ProcessStopOption) error {
	e.stoppedPID = pid
	e.stoppedIdentityTime = processStartTime
	return nil
}

func (e *recordingProcessExecutor) StartAndForget(*exec.Cmd, process.ProcessCreationFlag) (process.Pid_t, time.Time, error) {
	return process.UnknownPID, time.Time{}, fmt.Errorf("process start is not supported by recordingProcessExecutor")
}

func (e *recordingProcessExecutor) Dispose() {}

type testRunChangeHandler struct{}

func (*testRunChangeHandler) OnMainProcessChanged(controllers.RunID, process.Pid_t) {}

func (*testRunChangeHandler) OnRunCompleted(controllers.RunID, *int32, error) {}

func (*testRunChangeHandler) OnStartupCompleted(types.NamespacedName, *controllers.ExecutableStartResult) {
}

func (*testRunChangeHandler) OnRunMessage(controllers.RunID, controllers.RunMessageLevel, string) {}

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
