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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dcppaths"
	usvc_io "github.com/microsoft/dcp/pkg/io"
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

type outputCleanupRunner struct {
	cleanupContextRunner
	deleteCalls int
	deleteErr   error
	runID       RunID
	stdOutPath  string
	stdErrPath  string
	contextErr  error
}

func (runner *outputCleanupRunner) DeleteRunOutput(
	ctx context.Context,
	runID RunID,
	stdOutPath string,
	stdErrPath string,
) error {
	runner.deleteCalls++
	runner.runID = runID
	runner.stdOutPath = stdOutPath
	runner.stdErrPath = stdErrPath
	runner.contextErr = ctx.Err()
	return runner.deleteErr
}

type cleanupContextProcessExecutor struct {
	process.Executor
	observation cleanupContextObservation
	stopErr     error
	stopErrors  []error
	stopCalls   atomic.Int32
	checkErr    error
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
		stopErr := executor.nextStopError()
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
	return executor.nextStopError()
}

func (executor *cleanupContextProcessExecutor) CheckProcessRunning(process.ProcessHandle) error {
	return executor.checkErr
}

func (executor *cleanupContextProcessExecutor) nextStopError() error {
	callIndex := int(executor.stopCalls.Add(1)) - 1
	if callIndex < len(executor.stopErrors) {
		return executor.stopErrors[callIndex]
	}
	return executor.stopErr
}

// Verifies that deletion after an unconfirmed stop transfers output ownership to the runner before finalization.
// The handoff uses a detached context so eventual handle closure can remove Windows output files.
func TestExecutableDeletionTransfersOutputCleanupToRunnerAfterFailedStop(t *testing.T) {
	t.Setenv(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS, "")

	runner := &outputCleanupRunner{}
	reconciler := &ExecutableReconciler{
		ExecutableRunners: map[apiv1.ExecutionType]ExecutableRunner{
			apiv1.ExecutionTypeProcess: runner,
		},
	}
	executable := &apiv1.Executable{
		Status: apiv1.ExecutableStatus{
			StdOutFile: "stdout.log",
			StdErrFile: "stderr.log",
		},
	}
	runInfo := &ExecutableRunInfo{
		RunID:        "failed-stop",
		ExeState:     apiv1.ExecutableStateUnknown,
		startupStage: StartupStageDefaultRunner,
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	require.True(t, reconciler.runnerOwnsOutputDeletion(cancelledCtx, executable, runInfo, logr.Discard()))
	require.Equal(t, 1, runner.deleteCalls)
	require.Equal(t, runInfo.RunID, runner.runID)
	require.Equal(t, executable.Status.StdOutFile, runner.stdOutPath)
	require.Equal(t, executable.Status.StdErrFile, runner.stdErrPath)
	require.NoError(t, runner.contextErr)
}

// Verifies that the controller does not request runner-owned deletion when executable logs must be preserved.
func TestExecutableDeletionPreservesOutputWhenConfigured(t *testing.T) {
	t.Setenv(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS, "1")

	runner := &outputCleanupRunner{}
	reconciler := &ExecutableReconciler{
		ExecutableRunners: map[apiv1.ExecutionType]ExecutableRunner{
			apiv1.ExecutionTypeProcess: runner,
		},
	}
	executable := &apiv1.Executable{
		Status: apiv1.ExecutableStatus{
			StdOutFile: "stdout.log",
			StdErrFile: "stderr.log",
		},
	}
	runInfo := &ExecutableRunInfo{
		RunID:        "preserved-output",
		ExeState:     apiv1.ExecutableStateUnknown,
		startupStage: StartupStageDefaultRunner,
	}

	require.False(t, reconciler.runnerOwnsOutputDeletion(context.Background(), executable, runInfo, logr.Discard()))
	require.Zero(t, runner.deleteCalls)
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
// and publishes a terminal warning when the root is already gone after incomplete cleanup.
func TestPhysicalProcessIncompleteTreeStopFinalizesWhenRootIsGone(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	parentCtx := context.WithValue(testCtx, cleanupContextKey{}, "retained")
	cancelledCtx, cancel := context.WithCancel(parentCtx)
	cancel()

	handle := process.NewHandle(4300, time.Unix(1100, 0).UTC())
	descendantStopErr := errors.New("descendant stop failed")
	executor := &cleanupContextProcessExecutor{
		stopErr:  errors.Join(process.ErrIncompleteProcessTree, descendantStopErr),
		checkErr: &process.ErrProcessNotFound{Pid: handle.Pid},
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
	require.Equal(t, physicalProcessStateRuntime, currentData.state)
	require.Equal(t, physicalResourceProgressMissing, currentData.progress)
	require.True(t, currentData.cleanupUnconfirmed)
	require.Equal(t, apiv2.PhysicalProcessReasonDescendantCleanupUnconfirmed, currentData.failureReason)
	require.Contains(t, currentData.failureMessage, process.ErrIncompleteProcessTree.Error())
	require.Contains(t, currentData.failureMessage, descendantStopErr.Error())
	require.True(t, currentData.retryAfter.IsZero())

	now := metav1.Now()
	physicalProcess.DeletionTimestamp = &now
	physicalProcess.Finalizers = []string{physicalProcessFinalizer}
	firstDeleteChange, _ := reconciler.handleDeletionRequest(physicalProcess, currentData, logr.Discard())
	require.Equal(t, noChange, firstDeleteChange)
	require.Contains(t, physicalProcess.Finalizers, physicalProcessFinalizer)

	statusChange, _, valid := currentData.applyTo(physicalProcess)
	require.True(t, valid)
	require.NotEqual(t, noChange, statusChange)
	readyCondition := apimeta.FindStatusCondition(
		physicalProcess.Status.Conditions,
		string(apiv2.ConditionReady),
	)
	require.NotNil(t, readyCondition)
	require.Equal(t, string(apiv2.PhysicalProcessReasonDescendantCleanupUnconfirmed), readyCondition.Reason)
	require.Contains(t, readyCondition.Message, process.ErrIncompleteProcessTree.Error())

	secondDeleteChange, _ := reconciler.handleDeletionRequest(physicalProcess, currentData, logr.Discard())
	require.NotEqual(t, noChange, secondDeleteChange)
	require.NotContains(t, physicalProcess.Finalizers, physicalProcessFinalizer)
}

// Verifies that incomplete cleanup remains retryable with a StopFailed reason
// while the root process is still confirmed running.
func TestPhysicalProcessIncompleteTreeStopRetriesWhileRootIsRunning(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	handle := process.NewHandle(4300, time.Unix(1100, 0).UTC())
	executor := &cleanupContextProcessExecutor{
		stopErr: errors.Join(process.ErrIncompleteProcessTree, errors.New("descendant stop failed")),
	}
	reconciler := NewPhysicalProcessReconciler(testCtx, nil, nil, logr.Discard(), executor)
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "incomplete-tree-running",
			Namespace: "test",
			UID:       types.UID("incomplete-tree-running"),
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

	reconciler.stopPhysicalProcess(testCtx, physicalProcess, stateKey, data.Clone(), logr.Discard())
	reconciler.processData.RunDeferredOps(physicalProcess.NamespacedName(), physicalProcess)

	_, currentData := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
	require.NotNil(t, currentData)
	require.Equal(t, physicalProcessStateStop, currentData.state)
	require.Equal(t, physicalResourceProgressRetryPending, currentData.progress)
	require.True(t, currentData.cleanupUnconfirmed)
	require.Equal(t, apiv2.PhysicalProcessReasonStopFailed, currentData.failureReason)
	require.Contains(t, currentData.failureMessage, process.ErrIncompleteProcessTree.Error())
	require.False(t, currentData.retryAfter.IsZero())
}

// Verifies that any stop failure latches cleanup uncertainty and that a later successful
// or process-gone retry publishes the warning before deletion can remove the finalizer.
func TestPhysicalProcessStopFailureRemainsUnconfirmedAfterRetry(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()

	firstStopErrors := map[string]error{
		"helper timeout":       context.DeadlineExceeded,
		"helper abnormal exit": errors.New("stop helper exited abnormally"),
	}
	finalStopErrors := map[string]error{
		"success":      nil,
		"process gone": &process.ErrProcessNotFound{Pid: 4303},
	}
	for firstName, firstStopErr := range firstStopErrors {
		for finalName, finalStopErr := range finalStopErrors {
			t.Run(firstName+"/"+finalName, func(t *testing.T) {
				t.Parallel()

				testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
				defer testCancel()
				handle := process.NewHandle(4303, time.Unix(1103, 0).UTC())
				executor := &cleanupContextProcessExecutor{
					stopErrors: []error{firstStopErr, finalStopErr},
				}
				reconciler := NewPhysicalProcessReconciler(testCtx, nil, nil, logr.Discard(), executor)
				physicalProcess := &apiv2.PhysicalProcess{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "retry-unconfirmed",
						Namespace: "test",
						UID:       types.UID("retry-unconfirmed-" + firstName + "-" + finalName),
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

				reconciler.stopPhysicalProcess(testCtx, physicalProcess, stateKey, data.Clone(), logr.Discard())
				reconciler.processData.RunDeferredOps(physicalProcess.NamespacedName(), physicalProcess)
				_, retryData := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
				require.NotNil(t, retryData)
				require.Equal(t, physicalProcessStateStop, retryData.state)
				require.Equal(t, physicalResourceProgressRetryPending, retryData.progress)
				require.True(t, retryData.cleanupUnconfirmed)
				require.Equal(t, apiv2.PhysicalProcessReasonStopFailed, retryData.failureReason)
				require.Contains(t, retryData.failureMessage, firstStopErr.Error())

				retryData.progress = physicalResourceProgressInProgress
				retryData.retryAfter = time.Time{}
				require.True(t, reconciler.processData.Update(
					physicalProcess.NamespacedName(),
					stateKey,
					retryData,
				))
				reconciler.stopPhysicalProcess(testCtx, physicalProcess, stateKey, retryData.Clone(), logr.Discard())
				reconciler.processData.RunDeferredOps(physicalProcess.NamespacedName(), physicalProcess)

				_, completedData := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
				require.NotNil(t, completedData)
				require.Equal(t, physicalProcessStateRuntime, completedData.state)
				if finalStopErr == nil {
					require.Equal(t, physicalResourceProgressExited, completedData.progress)
				} else {
					require.Equal(t, physicalResourceProgressMissing, completedData.progress)
				}
				require.True(t, completedData.cleanupUnconfirmed)
				require.Equal(t, apiv2.PhysicalProcessReasonDescendantCleanupUnconfirmed, completedData.failureReason)
				require.Equal(t, descendantCleanupUnconfirmedMessage, completedData.failureMessage)
				require.Equal(t, int32(2), executor.stopCalls.Load())

				deletedAt := metav1.Now()
				physicalProcess.DeletionTimestamp = &deletedAt
				physicalProcess.Finalizers = []string{physicalProcessFinalizer}
				firstDeleteChange, _ := reconciler.handleDeletionRequest(physicalProcess, completedData, logr.Discard())
				require.Equal(t, noChange, firstDeleteChange)
				require.Contains(t, physicalProcess.Finalizers, physicalProcessFinalizer)

				statusChange, _, valid := completedData.applyTo(physicalProcess)
				require.True(t, valid)
				require.NotEqual(t, noChange, statusChange)
				require.True(t, physicalProcessReportsDescendantCleanupUnconfirmed(physicalProcess))

				secondDeleteChange, _ := reconciler.handleDeletionRequest(physicalProcess, completedData, logr.Discard())
				require.NotEqual(t, noChange, secondDeleteChange)
				require.NotContains(t, physicalProcess.Finalizers, physicalProcessFinalizer)
			})
		}
	}
}

// Verifies that deletion honors RetainRuntimeProcess even when a prior stop
// left descendant cleanup unconfirmed.
func TestRetainedPhysicalProcessDeletionDoesNotRetryUnconfirmedCleanup(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := &recordingPhysicalProcessExecutor{}
	reconciler := NewPhysicalProcessReconciler(testCtx, nil, nil, logr.Discard(), executor)
	handle := process.NewHandle(4302, time.Unix(1102, 0).UTC())
	deletedAt := metav1.Now()
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "retained-incomplete",
			Namespace:         "test",
			UID:               types.UID("retained-incomplete"),
			DeletionTimestamp: &deletedAt,
			Finalizers:        []string{physicalProcessFinalizer},
		},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{
				ExecutablePath:       "unused",
				RetainRuntimeProcess: true,
			},
		},
		Status: apiv2.PhysicalProcessStatus{
			Conditions: []metav1.Condition{{
				Type:   string(apiv2.ConditionReady),
				Status: metav1.ConditionFalse,
				Reason: string(apiv2.PhysicalProcessReasonDescendantCleanupUnconfirmed),
			}},
		},
	}
	data := &physicalProcessData{
		resourceUID:        physicalProcess.UID,
		state:              physicalProcessStateStop,
		progress:           physicalResourceProgressRetryPending,
		handle:             handle,
		failureReason:      apiv2.PhysicalProcessReasonDescendantCleanupUnconfirmed,
		failureMessage:     descendantCleanupUnconfirmedMessage,
		cleanupUnconfirmed: true,
		retryAfter:         time.Now().Add(-time.Second),
	}
	reconciler.processData.Store(
		physicalProcess.NamespacedName(),
		physicalProcessHandleDataKey(handle),
		data,
	)

	change, _ := reconciler.handleDeletionRequest(physicalProcess, data, logr.Discard())

	require.NotEqual(t, noChange, change)
	require.NotContains(t, physicalProcess.Finalizers, physicalProcessFinalizer)
	require.Zero(t, executor.stopProcessCalls.Load())
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
			if incomplete {
				executor.checkErr = &process.ErrProcessNotFound{Pid: 4300}
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
				require.Equal(t, physicalResourceProgressExited, completed.progress)
				require.True(t, completed.cleanupUnconfirmed)
				require.Equal(t, rootExited.finishedAt, completed.finishedAt)
				_, _, valid := completed.applyTo(physicalProcess)
				require.True(t, valid)
				require.True(t, physicalProcessReportsDescendantCleanupUnconfirmed(physicalProcess))
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
