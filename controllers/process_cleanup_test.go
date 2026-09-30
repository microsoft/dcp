/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/osutil"
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
	waitForStop func(context.Context) error
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
		stopErr := executor.stopErr
		if executor.waitForStop != nil {
			stopErr = executor.waitForStop(ctx)
		}
		handler.OnProcessExited(handle.Pid, 0, stopErr)
	}
	return handle, startWaitForExit, nil
}

func (executor *cleanupContextProcessExecutor) StopProcess(
	ctx context.Context,
	_ process.ProcessHandle,
	_ ...process.ProcessStopOption,
) error {
	executor.observeContext(ctx)
	if executor.waitForStop != nil {
		return executor.waitForStop(ctx)
	}
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
			expectedTimeout := 21 * time.Second
			if osutil.IsWindows() {
				expectedTimeout = 26 * time.Second
			}
			require.WithinDuration(t, time.Now().Add(expectedTimeout), runner.observation.deadline, time.Second)
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
	descendantStopErr := errors.New("descendant stop failed")
	executor := &cleanupContextProcessExecutor{
		stopErr: errors.Join(process.ErrIncompleteProcessTree, descendantStopErr),
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
	expectedTimeout := 21 * time.Second
	if osutil.IsWindows() {
		expectedTimeout = 26 * time.Second
	}
	require.WithinDuration(t, time.Now().Add(expectedTimeout), executor.observation.deadline, time.Second)
	_, currentData := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
	require.NotNil(t, currentData)
	require.Equal(t, physicalProcessStateStop, currentData.state)
	require.Equal(t, physicalResourceProgressRetryPending, currentData.progress)
	require.True(t, currentData.cleanupUnconfirmed)
	require.Equal(t, apiv2.PhysicalProcessReasonDescendantCleanupUnconfirmed, currentData.failureReason)
	require.Contains(t, currentData.failureMessage, process.ErrIncompleteProcessTree.Error())
	require.Contains(t, currentData.failureMessage, descendantStopErr.Error())
	require.False(t, currentData.retryAfter.IsZero())

	executor.stopErr = &process.ErrProcessNotFound{Pid: handle.Pid}
	reconciler.stopPhysicalProcess(cancelledCtx, physicalProcess, stateKey, currentData.Clone(), logr.Discard())
	reconciler.processData.RunDeferredOps(physicalProcess.NamespacedName(), physicalProcess)

	_, missingData := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
	require.NotNil(t, missingData)
	require.Equal(t, physicalProcessStateRuntime, missingData.state)
	require.Equal(t, physicalResourceProgressMissing, missingData.progress)
	require.True(t, missingData.cleanupUnconfirmed)
	require.Equal(t, apiv2.PhysicalProcessReasonDescendantCleanupUnconfirmed, missingData.failureReason)
	require.Equal(t, descendantCleanupUnconfirmedMessage, missingData.failureMessage)

	now := metav1.Now()
	physicalProcess.DeletionTimestamp = &now
	physicalProcess.Finalizers = []string{physicalProcessFinalizer}
	firstDeleteChange, _ := reconciler.handleDeletionRequest(physicalProcess, missingData, logr.Discard())
	require.Equal(t, noChange, firstDeleteChange)
	require.Contains(t, physicalProcess.Finalizers, physicalProcessFinalizer)

	statusChange, _, valid := missingData.applyTo(physicalProcess)
	require.True(t, valid)
	require.NotEqual(t, noChange, statusChange)
	readyCondition := apimeta.FindStatusCondition(
		physicalProcess.Status.Conditions,
		string(apiv2.ConditionReady),
	)
	require.NotNil(t, readyCondition)
	require.Equal(t, string(apiv2.PhysicalProcessReasonDescendantCleanupUnconfirmed), readyCondition.Reason)
	require.Equal(t, descendantCleanupUnconfirmedMessage, readyCondition.Message)

	secondDeleteChange, _ := reconciler.handleDeletionRequest(physicalProcess, missingData, logr.Discard())
	require.NotEqual(t, noChange, secondDeleteChange)
	require.NotContains(t, physicalProcess.Finalizers, physicalProcessFinalizer)
}

// Verifies that PhysicalProcess initialization ignores diagnostic status, including JSON-round-tripped timestamps.
// Deletion by a fresh controller must not claim, inspect, start, or stop a runtime process.
func TestPhysicalProcessDiagnosticStatusDoesNotRestoreOwnership(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	pid := int64(4300)
	exitCode := int32(0)
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "diagnostic-status",
			Namespace: "test",
			UID:       types.UID("diagnostic-status"),
		},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{ExecutablePath: "unused"},
		},
		Status: apiv2.PhysicalProcessStatus{
			Phase:             apiv2.PhysicalProcessPhaseExited,
			PID:               &pid,
			IdentityTimestamp: metav1.NewMicroTime(time.Unix(1100, 0).UTC()),
			ExitCode:          &exitCode,
			Conditions: []metav1.Condition{{
				Type:   string(apiv2.ConditionReady),
				Status: metav1.ConditionFalse,
				Reason: string(apiv2.PhysicalProcessReasonDescendantCleanupUnconfirmed),
			}},
		},
	}
	encoded, marshalErr := json.Marshal(physicalProcess)
	require.NoError(t, marshalErr)
	var decoded apiv2.PhysicalProcess
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	for _, fixture := range []*apiv2.PhysicalProcess{physicalProcess, &decoded} {
		stateKey, data := initialPhysicalProcessData(fixture)
		require.Equal(t, physicalProcessDataKey(fixture), stateKey)
		require.Equal(t, &physicalProcessData{
			resourceUID: fixture.UID,
			state:       physicalProcessStateNamespace,
			progress:    physicalResourceProgressNotReady,
		}, data)

		deleting := fixture.DeepCopy()
		deletedAt := metav1.Now()
		deleting.DeletionTimestamp = &deletedAt
		deleting.Finalizers = []string{physicalProcessFinalizer}
		executor := &recordingPhysicalProcessExecutor{}
		reconciler := NewPhysicalProcessReconciler(testCtx, nil, nil, logr.Discard(), executor)
		change, _ := reconciler.managePhysicalProcess(testCtx, deleting, logr.Discard())

		require.NotEqual(t, noChange, change)
		require.NotContains(t, deleting.Finalizers, physicalProcessFinalizer)
		require.Zero(t, executor.startProcessCalls.Load())
		require.Zero(t, executor.stopProcessCalls.Load())
		require.Zero(t, executor.findProcessHandleCalls.Load())
		_, remaining := reconciler.processData.BorrowByNamespacedName(deleting.NamespacedName())
		require.Nil(t, remaining)
	}
}

// Verifies that root-exit notifications preserve in-flight tree cleanup and the root's exit metadata.
// Finalization waits for confirmed cleanup or publication of an incomplete-cleanup warning after root disappearance.
func TestPhysicalProcessRootExitDoesNotFinishTreeCleanup(t *testing.T) {
	t.Parallel()

	for _, incomplete := range []bool{false, true} {
		name := "confirmed"
		if incomplete {
			name = "incomplete"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
			defer testCancel()

			stopStarted := make(chan struct{})
			releaseStop := make(chan struct{})
			stopFinished := make(chan struct{})
			var releaseOnce sync.Once
			finishStop := func() { releaseOnce.Do(func() { close(releaseStop) }) }
			executor := &cleanupContextProcessExecutor{
				waitForStop: func(ctx context.Context) error {
					close(stopStarted)
					select {
					case <-releaseStop:
						if incomplete {
							return process.ErrIncompleteProcessTree
						}
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			}
			reconciler := NewPhysicalProcessReconciler(testCtx, nil, nil, logr.Discard(), executor)
			deletedAt := metav1.Now()
			physicalProcess := &apiv2.PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{
					Name:              name,
					Namespace:         "test",
					UID:               types.UID(name),
					DeletionTimestamp: &deletedAt,
					Finalizers:        []string{physicalProcessFinalizer},
				},
				Spec: apiv2.PhysicalProcessSpec{
					Process: &apiv2.PhysicalProcessConfig{ExecutablePath: "unused"},
				},
			}
			handle := process.NewHandle(4300, time.Unix(1100, 0).UTC())
			stateKey := physicalProcessHandleDataKey(handle)
			stopping := &physicalProcessData{
				resourceUID: physicalProcess.UID,
				state:       physicalProcessStateStop,
				progress:    physicalResourceProgressInProgress,
				handle:      handle,
			}
			reconciler.processData.Store(physicalProcess.NamespacedName(), stateKey, stopping.Clone())
			go func() {
				reconciler.stopPhysicalProcess(testCtx, physicalProcess.DeepCopy(), stateKey, stopping.Clone(), logr.Discard())
				close(stopFinished)
			}()
			defer func() {
				finishStop()
				select {
				case <-stopFinished:
				case <-testCtx.Done():
					t.Error("cleanup worker did not finish")
				}
			}()
			select {
			case <-stopStarted:
			case <-testCtx.Done():
				t.Fatal("cleanup worker did not start")
			}

			const rootExitCode int32 = 17
			reconciler.processExited(physicalProcess.NamespacedName(), physicalProcess.UID, handle, handle.Pid, rootExitCode, nil)
			reconciler.processData.RunDeferredOps(physicalProcess.NamespacedName(), physicalProcess)
			_, rootExited := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
			require.NotNil(t, rootExited)
			change, _ := reconciler.handleDeletionRequest(physicalProcess, rootExited, logr.Discard())
			require.Contains(t, physicalProcess.Finalizers, physicalProcessFinalizer)
			require.Equal(t, additionalReconciliationNeeded, change)
			require.Equal(t, physicalProcessStateStop, rootExited.state)
			require.True(t, rootExited.operationInProgress())
			require.NotNil(t, rootExited.exitCode)
			require.Equal(t, rootExitCode, *rootExited.exitCode)
			require.False(t, rootExited.finishedAt.IsZero())

			finishStop()
			select {
			case <-stopFinished:
			case <-testCtx.Done():
				t.Fatal("cleanup result did not arrive")
			}
			reconciler.processData.RunDeferredOps(physicalProcess.NamespacedName(), physicalProcess)
			_, completed := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
			require.NotNil(t, completed)
			require.NotNil(t, completed.exitCode)
			require.Equal(t, rootExitCode, *completed.exitCode)
			if incomplete {
				require.Equal(t, physicalResourceProgressRetryPending, completed.progress)
				require.True(t, completed.cleanupUnconfirmed)
				require.Equal(t, rootExited.finishedAt, completed.finishedAt)
				_, _, valid := completed.applyTo(physicalProcess)
				require.True(t, valid)
				require.True(t, physicalProcessReportsDescendantCleanupUnconfirmed(physicalProcess))

				executor.waitForStop = nil
				executor.stopErr = &process.ErrProcessNotFound{Pid: handle.Pid}
				reconciler.stopPhysicalProcess(testCtx, physicalProcess, stateKey, completed.Clone(), logr.Discard())
				reconciler.processData.RunDeferredOps(physicalProcess.NamespacedName(), physicalProcess)
				_, completed = reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
				require.NotNil(t, completed)
				require.True(t, completed.cleanupUnconfirmed)
			} else {
				require.Equal(t, physicalResourceProgressExited, completed.progress)
				require.False(t, completed.cleanupUnconfirmed)
			}
			finalChange, _ := reconciler.handleDeletionRequest(physicalProcess, completed, logr.Discard())
			require.NotEqual(t, noChange, finalChange)
			require.NotContains(t, physicalProcess.Finalizers, physicalProcessFinalizer)
		})
	}
}
