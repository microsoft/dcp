//go:build darwin || linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const controllerCleanupHelperMode = "DCP_CONTROLLER_CLEANUP_HELPER_MODE"

type processTreeCleanupRunner struct {
	executor process.Executor
	handle   process.ProcessHandle
	stopErr  error
}

func (*processTreeCleanupRunner) StartRun(context.Context, *apiv1.Executable, RunChangeHandler, logr.Logger) *ExecutableStartResult {
	return NewExecutableStartResult()
}

func (runner *processTreeCleanupRunner) StopRun(ctx context.Context, _ RunID, _ logr.Logger) error {
	runner.stopErr = runner.executor.StopProcess(ctx, runner.handle)
	return nil
}

func (*processTreeCleanupRunner) ReleaseRun(context.Context, RunID, logr.Logger) error {
	return nil
}

func TestControllerCleanupProcessHelper(t *testing.T) {
	mode := os.Getenv(controllerCleanupHelperMode)
	switch mode {
	case "":
		return

	case "child":
		time.Sleep(30 * time.Second)

	case "root":
		signalCh := make(chan os.Signal, 1)
		signal.Notify(signalCh, syscall.SIGTERM)
		defer signal.Stop(signalCh)

		childCmd := exec.Command(os.Args[0], "-test.run=^TestControllerCleanupProcessHelper$")
		childCmd.Env = append(os.Environ(), controllerCleanupHelperMode+"=child")
		childStartErr := childCmd.Start()
		require.NoError(t, childStartErr)
		go func() {
			_ = childCmd.Wait()
		}()

		pidWriter := os.NewFile(uintptr(3), "child-pid")
		require.NotNil(t, pidWriter)
		_, pidWriteErr := fmt.Fprintln(pidWriter, childCmd.Process.Pid)
		require.NoError(t, pidWriteErr)
		require.NoError(t, pidWriter.Close())

		<-signalCh

		exitNoticeWriter := os.NewFile(uintptr(4), "exit-notice")
		require.NotNil(t, exitNoticeWriter)
		_, noticeWriteErr := fmt.Fprintln(exitNoticeWriter, "terminating")
		require.NoError(t, noticeWriteErr)
		require.NoError(t, exitNoticeWriter.Close())

		exitAckReader := os.NewFile(uintptr(5), "exit-ack")
		require.NotNil(t, exitAckReader)
		_, ackReadErr := io.ReadFull(exitAckReader, make([]byte, 1))
		require.NoError(t, ackReadErr)
		require.NoError(t, exitAckReader.Close())
		// Keep the root alive long enough for the stop path to observe queue cancellation
		// before it can move from the root phase to descendant dispatch.
		time.Sleep(100 * time.Millisecond)

	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func TestPhysicalProcessCleanupContinuesAfterQueueCancellation(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := process.NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	rootHandle, childHandle, operationCtx, cancellationResult := startControllerCleanupTree(t, testCtx, executor)

	reconciler := NewPhysicalProcessReconciler(testCtx, nil, nil, logr.Discard(), executor)
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cancelled-stop",
			Namespace: "test",
			UID:       types.UID("cancelled-stop"),
		},
	}
	stateKey := physicalProcessHandleDataKey(rootHandle)
	data := &physicalProcessData{
		resourceUID: physicalProcess.UID,
		state:       physicalProcessStateStop,
		progress:    physicalResourceProgressInProgress,
		handle:      rootHandle,
	}
	reconciler.processData.Store(physicalProcess.NamespacedName(), stateKey, data.Clone())

	reconciler.stopPhysicalProcess(operationCtx, physicalProcess, stateKey, data.Clone(), logr.Discard())

	require.NoError(t, <-cancellationResult)
	requireProcessStoppedByController(t, testCtx, executor, childHandle)
}

func TestExecutableCleanupContinuesAfterQueueCancellation(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		invoke func(*ExecutableReconciler, *apiv1.Executable, *ExecutableRunInfo, context.Context)
	}{
		{
			name: "queued stop",
			invoke: func(reconciler *ExecutableReconciler, executable *apiv1.Executable, runInfo *ExecutableRunInfo, ctx context.Context) {
				reconciler.stopExecutableFunc(executable, runInfo, nil, logr.Discard())(ctx)
			},
		},
		{
			name: "persistent start rollback",
			invoke: func(reconciler *ExecutableReconciler, executable *apiv1.Executable, runInfo *ExecutableRunInfo, ctx context.Context) {
				reconciler.cleanUpPersistentStartAfterRecordFailure(
					ctx,
					executable,
					runInfo.startupStage,
					&ExecutableStartResult{RunID: runInfo.RunID},
					logr.Discard(),
				)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
			defer testCancel()
			executor := process.NewOSExecutor(logr.Discard())
			defer executor.Dispose()
			rootHandle, childHandle, operationCtx, cancellationResult := startControllerCleanupTree(t, testCtx, executor)
			runner := &processTreeCleanupRunner{
				executor: executor,
				handle:   rootHandle,
			}
			reconciler := &ExecutableReconciler{
				ExecutableRunners: map[apiv1.ExecutionType]ExecutableRunner{
					apiv1.ExecutionTypeProcess: runner,
				},
			}
			executable := &apiv1.Executable{}
			runInfo := &ExecutableRunInfo{
				RunID:        RunID(strconv.FormatInt(int64(rootHandle.Pid), 10)),
				startupStage: StartupStageDefaultRunner,
			}

			testCase.invoke(reconciler, executable, runInfo, operationCtx)

			require.NoError(t, <-cancellationResult)
			require.NoError(t, runner.stopErr)
			requireProcessStoppedByController(t, testCtx, executor, childHandle)
		})
	}
}

func startControllerCleanupTree(
	t *testing.T,
	testCtx context.Context,
	executor process.Executor,
) (process.ProcessHandle, process.ProcessHandle, context.Context, <-chan error) {
	t.Helper()

	pidReader, pidWriter, pidPipeErr := os.Pipe()
	require.NoError(t, pidPipeErr)
	exitNoticeReader, exitNoticeWriter, noticePipeErr := os.Pipe()
	require.NoError(t, noticePipeErr)
	exitAckReader, exitAckWriter, ackPipeErr := os.Pipe()
	require.NoError(t, ackPipeErr)
	rootCmd := exec.Command(os.Args[0], "-test.run=^TestControllerCleanupProcessHelper$")
	rootCmd.Env = append(os.Environ(), controllerCleanupHelperMode+"=root")
	rootCmd.ExtraFiles = []*os.File{pidWriter, exitNoticeWriter, exitAckReader}
	rootHandle, startWaitForExit, startErr := executor.StartProcess(
		testCtx,
		rootCmd,
		nil,
		process.CreationFlagEnsureKillOnDispose,
		nil,
	)
	require.NoError(t, startErr)
	require.NoError(t, pidWriter.Close())
	require.NoError(t, exitNoticeWriter.Close())
	require.NoError(t, exitAckReader.Close())
	startWaitForExit()

	childPIDText, childPIDReadErr := bufio.NewReader(pidReader).ReadString('\n')
	require.NoError(t, childPIDReadErr)
	require.NoError(t, pidReader.Close())
	childPID, childPIDParseErr := strconv.ParseInt(strings.TrimSpace(childPIDText), 10, 64)
	require.NoError(t, childPIDParseErr)
	childHandle, childHandleErr := executor.FindProcessHandle(process.Pid_t(childPID))
	require.NoError(t, childHandleErr)

	operationCtx, cancelOperation := context.WithCancel(testCtx)
	cancellationResult := make(chan error, 1)
	go func() {
		notice, noticeReadErr := bufio.NewReader(exitNoticeReader).ReadString('\n')
		closeNoticeErr := exitNoticeReader.Close()
		cancelOperation()
		_, ackWriteErr := exitAckWriter.Write([]byte{1})
		closeAckErr := exitAckWriter.Close()
		var noticeErr error
		if noticeReadErr != nil {
			noticeErr = noticeReadErr
		} else if strings.TrimSpace(notice) != "terminating" {
			noticeErr = fmt.Errorf("unexpected exit notice %q", notice)
		}
		cancellationResult <- errors.Join(noticeErr, ackWriteErr, closeAckErr, closeNoticeErr)
	}()

	t.Cleanup(func() {
		cancelOperation()
		_ = exitNoticeReader.Close()
		_ = exitAckWriter.Close()
		cleanupProcessForControllerTest(childHandle)
		cleanupProcessForControllerTest(rootHandle)
	})
	return rootHandle, childHandle, operationCtx, cancellationResult
}

func requireProcessStoppedByController(
	t *testing.T,
	ctx context.Context,
	executor process.Executor,
	handle process.ProcessHandle,
) {
	t.Helper()

	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Second)
	defer waitCancel()
	waitErr := wait.PollUntilContextCancel(waitCtx, 25*time.Millisecond, true, func(context.Context) (bool, error) {
		return process.IsProcessGoneErr(executor.CheckProcessRunning(handle)), nil
	})
	require.NoError(t, waitErr)
}

func cleanupProcessForControllerTest(handle process.ProcessHandle) {
	proc, findErr := process.FindProcess(handle)
	if findErr != nil {
		return
	}
	_ = proc.Kill()
	_ = proc.Release()
}
