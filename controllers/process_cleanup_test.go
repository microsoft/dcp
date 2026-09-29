/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type cleanupContextKey struct{}

type cleanupContextObservation struct {
	err         error
	value       any
	deadline    time.Time
	hasDeadline bool
}

type cleanupContextRunner struct {
	observation cleanupContextObservation
}

func (*cleanupContextRunner) StartRun(context.Context, *apiv1.Executable, RunChangeHandler, logr.Logger) *ExecutableStartResult {
	return NewExecutableStartResult()
}

func (runner *cleanupContextRunner) StopRun(ctx context.Context, _ RunID, _ logr.Logger) error {
	runner.observation.err = ctx.Err()
	runner.observation.value = ctx.Value(cleanupContextKey{})
	runner.observation.deadline, runner.observation.hasDeadline = ctx.Deadline()
	return nil
}

func (*cleanupContextRunner) ReleaseRun(context.Context, RunID, logr.Logger) error {
	return nil
}

type cleanupContextProcessExecutor struct {
	process.Executor
	observation cleanupContextObservation
	stopErr     error
}

func (executor *cleanupContextProcessExecutor) observeContext(ctx context.Context) {
	executor.observation.err = ctx.Err()
	executor.observation.value = ctx.Value(cleanupContextKey{})
	executor.observation.deadline, executor.observation.hasDeadline = ctx.Deadline()
}

func (executor *cleanupContextProcessExecutor) StartProcess(
	ctx context.Context,
	_ *exec.Cmd,
	handler process.ProcessExitHandler,
	_ process.ProcessCreationFlag,
	_ process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	executor.observeContext(ctx)
	handle := process.NewHandle(4301, time.Unix(1101, 0).UTC())
	startWaitForExit := func() {
		handler.OnProcessExited(handle.Pid, process.UnknownExitCode, executor.stopErr)
	}
	return handle, startWaitForExit, nil
}

func (executor *cleanupContextProcessExecutor) StopProcess(
	ctx context.Context,
	_ process.ProcessHandle,
	_ ...process.ProcessStopOption,
) error {
	executor.observeContext(ctx)
	return executor.stopErr
}

// Verifies that queued executable stops and persistent-start rollback detach parent cancellation,
// retain context values, and apply a bounded cleanup deadline.
func TestExecutableCleanupBoundariesDetachCancellation(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		invoke func(*ExecutableReconciler, *apiv1.Executable, *cleanupContextRunner, context.Context)
	}{
		{
			name: "queued stop",
			invoke: func(reconciler *ExecutableReconciler, executable *apiv1.Executable, _ *cleanupContextRunner, ctx context.Context) {
				runInfo := &ExecutableRunInfo{
					RunID:        RunID("queued-stop"),
					startupStage: StartupStageDefaultRunner,
				}
				reconciler.stopExecutableFunc(executable, runInfo, nil, logr.Discard())(ctx)
			},
		},
		{
			name: "persistent start rollback",
			invoke: func(reconciler *ExecutableReconciler, executable *apiv1.Executable, _ *cleanupContextRunner, ctx context.Context) {
				reconciler.cleanUpPersistentStartAfterRecordFailure(
					ctx,
					executable,
					StartupStageDefaultRunner,
					&ExecutableStartResult{RunID: RunID("persistent-start")},
					logr.Discard(),
				)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			runner := &cleanupContextRunner{}
			reconciler := &ExecutableReconciler{
				ExecutableRunners: map[apiv1.ExecutionType]ExecutableRunner{
					apiv1.ExecutionTypeProcess: runner,
				},
			}
			executable := &apiv1.Executable{}
			parentCtx := context.WithValue(context.Background(), cleanupContextKey{}, "retained")
			cancelledCtx, cancel := context.WithCancel(parentCtx)
			cancel()

			testCase.invoke(reconciler, executable, runner, cancelledCtx)

			require.NoError(t, runner.observation.err)
			require.Equal(t, "retained", runner.observation.value)
			require.True(t, runner.observation.hasDeadline)
			require.WithinDuration(t, time.Now().Add(15*time.Second), runner.observation.deadline, time.Second)
		})
	}
}

// Verifies that physical-process cleanup detaches queue cancellation, retains context values,
// and leaves an incomplete-tree stop in retry-pending state with its failure recorded.
func TestPhysicalProcessIncompleteTreeStopRemainsRetryable(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	parentCtx := context.WithValue(testCtx, cleanupContextKey{}, "retained")
	cancelledCtx, cancel := context.WithCancel(parentCtx)
	cancel()

	handle := process.NewHandle(4300, time.Unix(1100, 0).UTC())
	executor := &cleanupContextProcessExecutor{
		stopErr: process.ErrIncompleteProcessTree,
	}
	reconciler := NewPhysicalProcessReconciler(
		testCtx,
		nil,
		nil,
		logr.Discard(),
		executor,
	)
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "incomplete-tree",
			Namespace: "test",
			UID:       types.UID("incomplete-tree"),
		},
	}
	stateKey := physicalProcessHandleDataKey(handle)
	data := &physicalProcessData{
		resourceUID: physicalProcess.UID,
		state:       physicalProcessStateStop,
		progress:    physicalResourceProgressInProgress,
		handle:      handle,
	}
	reconciler.processData.Store(physicalProcess.NamespacedName(), stateKey, data.Clone())

	reconciler.stopPhysicalProcess(cancelledCtx, physicalProcess, stateKey, data.Clone(), logr.Discard())
	reconciler.processData.RunDeferredOps(physicalProcess.NamespacedName(), physicalProcess)

	require.NoError(t, executor.observation.err)
	require.Equal(t, "retained", executor.observation.value)
	require.True(t, executor.observation.hasDeadline)
	_, currentData := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
	require.NotNil(t, currentData)
	require.Equal(t, physicalProcessStateStop, currentData.state)
	require.Equal(t, physicalResourceProgressRetryPending, currentData.progress)
	require.Contains(t, currentData.failureMessage, process.ErrIncompleteProcessTree.Error())
	require.False(t, currentData.retryAfter.IsZero())
}
