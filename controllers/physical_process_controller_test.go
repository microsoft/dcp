/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

type missingPhysicalProcessExecutor struct {
	process.Executor
}

type failingPhysicalProcessInspectionExecutor struct {
	process.Executor
	err error
}

type recordingPhysicalProcessExecutor struct {
	process.Executor
	findProcessHandleCalls atomic.Int32
	startProcessCalls      atomic.Int32
	stopProcessCalls       atomic.Int32
}

// Verifies that PhysicalProcess environment construction defaults to inheritance, honors explicit
// inheritance behavior, applies explicit overrides, and excludes inherited and explicit internal variables.
func TestPhysicalProcessEnvironment(t *testing.T) {
	t.Setenv("PHYSICAL_PROCESS_ENV_TEST", "inherited")
	t.Setenv("PHYSICAL_PROCESS_AMBIENT_ONLY", "ambient")
	t.Setenv("DCP_PHYSICAL_PROCESS_INHERITED", "not for child")
	t.Setenv("DEBUG_SESSION_PHYSICAL_PROCESS_INHERITED", "not for child")

	explicitEnvironment := []commonapi.EnvVar{
		{Name: "PHYSICAL_PROCESS_ENV_TEST", Value: "configured"},
		{Name: "DCP_PHYSICAL_PROCESS_DECLARED", Value: "not for child"},
		{Name: "DEBUG_SESSION_PHYSICAL_PROCESS_DECLARED", Value: "not for child"},
	}
	testCases := []struct {
		name            string
		behavior        apiv2.EnvironmentBehavior
		expectedAmbient []string
	}{
		{
			name:            "defaults to inherited environment",
			expectedAmbient: []string{"ambient"},
		},
		{
			name:            "inherits environment explicitly",
			behavior:        apiv2.EnvironmentBehaviorInherit,
			expectedAmbient: []string{"ambient"},
		},
		{
			name:     "does not inherit environment explicitly",
			behavior: apiv2.EnvironmentBehaviorDoNotInherit,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			environment := physicalProcessEnvironment(&apiv2.PhysicalProcessConfig{
				AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: testCase.behavior},
				Env:                explicitEnvironment,
			})

			var values, ambientValues, internalNames []string
			for _, entry := range environment {
				name, value, _ := strings.Cut(entry, "=")
				switch name {
				case "PHYSICAL_PROCESS_ENV_TEST":
					values = append(values, value)
				case "PHYSICAL_PROCESS_AMBIENT_ONLY":
					ambientValues = append(ambientValues, value)
				case "DCP_PHYSICAL_PROCESS_INHERITED", "DEBUG_SESSION_PHYSICAL_PROCESS_INHERITED",
					"DCP_PHYSICAL_PROCESS_DECLARED", "DEBUG_SESSION_PHYSICAL_PROCESS_DECLARED":
					internalNames = append(internalNames, name)
				}
			}
			require.Equal(t, []string{"configured"}, values)
			require.Equal(t, testCase.expectedAmbient, ambientValues)
			require.Empty(t, internalNames)
		})
	}
	t.Run("does not restore inheritance when all variables are suppressed", func(t *testing.T) {
		environment := physicalProcessEnvironment(&apiv2.PhysicalProcessConfig{
			AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
			Env:                explicitEnvironment[1:],
		})
		require.NotNil(t, environment)
		require.Empty(t, environment)
	})
}

// Verifies that processLaunchEnvironment preserves literal inherited values with explicit overrides,
// honors disabled inheritance, and excludes internal variables.
func TestProcessLaunchEnvironment(t *testing.T) {
	t.Setenv("LOGICAL_PROCESS_AMBIENT_ONLY", "{{literal inherited value}}")
	t.Setenv("LOGICAL_PROCESS_OVERRIDE", "inherited")
	t.Setenv("DCP_LOGICAL_PROCESS_INTERNAL", "hidden")
	t.Setenv("DEBUG_SESSION_LOGICAL_PROCESS_INTERNAL", "hidden")
	declared := []commonapi.EnvVar{
		{Name: "LOGICAL_PROCESS_OVERRIDE", Value: "configured"},
		{Name: "DCP_LOGICAL_PROCESS_INTERNAL", Value: "configured hidden"},
	}
	inherited := processLaunchEnvironment(apiv2.AmbientEnvironment{}, declared)
	require.Contains(t, inherited, commonapi.EnvVar{Name: "LOGICAL_PROCESS_AMBIENT_ONLY", Value: "{{literal inherited value}}"})
	require.Contains(t, inherited, declared[0])
	require.NotContains(t, inherited, commonapi.EnvVar{Name: "LOGICAL_PROCESS_OVERRIDE", Value: "inherited"})
	require.NotContains(t, inherited, declared[1])
	require.NotContains(t, inherited, commonapi.EnvVar{Name: "DEBUG_SESSION_LOGICAL_PROCESS_INTERNAL", Value: "hidden"})
	isolated := processLaunchEnvironment(
		apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit}, declared)
	require.Equal(t, declared[:1], isolated)
}

func (*missingPhysicalProcessExecutor) CheckProcessRunning(process.ProcessHandle) error {
	return process.ErrorProcessNotFound
}

func (executor *failingPhysicalProcessInspectionExecutor) CheckProcessRunning(process.ProcessHandle) error {
	return executor.err
}

func (e *recordingPhysicalProcessExecutor) FindProcessHandle(process.Pid_t) (process.ProcessHandle, error) {
	e.findProcessHandleCalls.Add(1)
	return process.ProcessHandle{}, process.ErrorProcessNotFound
}

func (e *recordingPhysicalProcessExecutor) StartProcess(
	context.Context,
	*exec.Cmd,
	process.ProcessExitHandler,
	process.ProcessCreationFlag,
	process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	e.startProcessCalls.Add(1)
	return process.ProcessHandle{}, nil, errors.New("unexpected process start")
}

func (e *recordingPhysicalProcessExecutor) StopProcess(
	context.Context,
	process.ProcessHandle,
	...process.ProcessStopOption,
) error {
	e.stopProcessCalls.Add(1)
	return errors.New("unexpected process stop")
}

// Verifies that a canceled launch performs no process start and records a retry without outstanding work.
func TestPhysicalProcessLaunchCanceledBeforeStart(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                 string
		retainRuntimeProcess bool
	}{
		{
			name: "managed process",
		},
		{
			name:                 "retained process",
			retainRuntimeProcess: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			lifetimeCtx, cancelLifetime := testutil.GetTestContext(t, 30*time.Second)
			defer cancelLifetime()
			operationCtx, cancelOperation := context.WithCancel(lifetimeCtx)
			cancelOperation()

			physicalProcess := &apiv2.PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-process",
					Namespace: "test",
					UID:       types.UID("test-process"),
				},
				Spec: apiv2.PhysicalProcessSpec{
					Process: &apiv2.PhysicalProcessConfig{
						ExecutablePath:       "test-executable",
						RetainRuntimeProcess: testCase.retainRuntimeProcess,
					},
				},
			}
			stateKey := physicalProcessDataKey(physicalProcess)
			initialData := &physicalProcessData{
				resourceUID:    physicalProcess.UID,
				state:          physicalProcessStateLaunch,
				workInProgress: true,
			}
			executor := &recordingPhysicalProcessExecutor{}
			reconciler := NewPhysicalProcessReconciler(
				lifetimeCtx,
				nil,
				nil,
				logr.Discard(),
				executor,
				resiliency.NewWorkQueue(lifetimeCtx, 1),
			)

			reconciler.processData.Store(physicalProcess.NamespacedName(), stateKey, initialData)

			reconciler.launchPhysicalProcess(
				operationCtx,
				physicalProcess,
				stateKey,
				initialData.Clone(),
				logr.Discard(),
			)

			require.Zero(t, executor.startProcessCalls.Load())
			reconciler.processData.RunDeferredOps(physicalProcess.NamespacedName(), physicalProcess)
			currentStateKey, currentData := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
			require.Equal(t, stateKey, currentStateKey)
			require.NotNil(t, currentData)
			require.Equal(t, physicalProcessStateLaunchRetry, currentData.state)
			require.False(t, currentData.workInProgress)
			require.Contains(t, currentData.failureMessage, context.Canceled.Error())
			require.False(t, currentData.retryAfter.IsZero())
		})
	}
}

// Verifies that process resolution waits for its retry deadline without probing the executor.
func TestHandlePhysicalProcessResolveWaitsForRetryDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	pid := int64(42)
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-process",
			Namespace: "test",
			UID:       types.UID("test-process"),
		},
		Spec: apiv2.PhysicalProcessSpec{
			PID: &pid,
		},
	}
	data := &physicalProcessData{
		resourceUID: physicalProcess.UID,
		state:       physicalProcessStateResolveRetry,
		retryAfter:  time.Now().Add(time.Minute),
	}
	executor := &recordingPhysicalProcessExecutor{}
	reconciler := NewPhysicalProcessReconciler(
		ctx,
		nil,
		nil,
		logr.Discard(),
		executor,
		resiliency.NewWorkQueue(ctx, 1),
	)

	change := handlePhysicalProcessResolve(
		ctx,
		reconciler,
		physicalProcess,
		physicalProcessStateResolve,
		data,
		logr.Discard(),
	)

	require.Equal(t, additionalReconciliationNeeded, change)
	require.Zero(t, executor.findProcessHandleCalls.Load())
}

// Verifies that an invalid process-state dispatch clears stale diagnostics and reports the state by name.
func TestHandleUnknownPhysicalProcessStateUsesReadableMessage(t *testing.T) {
	t.Parallel()

	physicalProcess := &apiv2.PhysicalProcess{}
	data := &physicalProcessData{
		state:          physicalProcessStateStop,
		failureReason:  apiv2.PhysicalProcessReasonStopFailed,
		failureMessage: "old failure",
	}

	change := handleUnknownPhysicalProcessState(
		t.Context(),
		nil,
		physicalProcess,
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, noChange, change)
	require.Equal(t, physicalProcessStateInvalid, data.state)
	require.False(t, data.workInProgress)
	require.Empty(t, data.failureReason)
	require.Equal(t, "Physical process reached invalid reconciliation state Stop.", data.failureMessage)
}

// Verifies that a conflicting launch result preserves the existing process owner and returns to resolution.
func TestPhysicalProcessLaunchResultDoesNotReplaceExistingOwner(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	handle := process.NewHandle(42, time.Now())
	launchedProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "launched-process",
			Namespace: "test",
			UID:       types.UID("launched-process"),
		},
	}
	existingOwner := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-owner",
			Namespace: "test",
			UID:       types.UID("existing-owner"),
		},
	}
	reconciler := NewPhysicalProcessReconciler(
		ctx,
		nil,
		nil,
		logr.Discard(),
		&missingPhysicalProcessExecutor{},
		resiliency.NewWorkQueue(ctx, 1),
	)

	launchStateKey := physicalProcessDataKey(launchedProcess)
	reconciler.processData.Store(launchedProcess.NamespacedName(), launchStateKey, &physicalProcessData{
		resourceUID:    launchedProcess.UID,
		state:          physicalProcessStateLaunch,
		workInProgress: true,
	})
	reconciler.processData.Store(existingOwner.NamespacedName(), physicalProcessHandleDataKey(handle), &physicalProcessData{
		resourceUID: existingOwner.UID,
		state:       physicalProcessStateRunning,
		handle:      handle,
	})

	reconciler.queuePhysicalProcessDataResult(launchedProcess, launchStateKey, &physicalProcessData{
		resourceUID: launchedProcess.UID,
		state:       physicalProcessStateRunning,
		handle:      handle,
	})
	reconciler.processData.RunDeferredOps(launchedProcess.NamespacedName(), launchedProcess)

	ownerName, ownerData := reconciler.processData.BorrowByStateKey(physicalProcessHandleDataKey(handle))
	require.Equal(t, existingOwner.NamespacedName(), ownerName)
	require.NotNil(t, ownerData)
	require.Equal(t, existingOwner.UID, ownerData.resourceUID)

	currentStateKey, launchedData := reconciler.processData.BorrowByNamespacedName(launchedProcess.NamespacedName())
	require.Equal(t, launchStateKey, currentStateKey)
	require.NotNil(t, launchedData)
	require.Equal(t, physicalProcessStateResolveRetry, launchedData.state)
	require.False(t, launchedData.workInProgress)
	require.Equal(t, handle, launchedData.handle)
	require.Equal(t, apiv2.PhysicalProcessReasonRuntimeProcessAlreadyTracked, launchedData.failureReason)
	require.Contains(t, launchedData.failureMessage, existingOwner.Name)
}

// Verifies that an exited process result is retained without claiming another resource's process identity.
func TestPhysicalProcessTerminalResultDoesNotRequireHandleOwnership(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	handle := process.NewHandle(42, time.Now())
	launchedProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "launched-process",
			Namespace: "test",
			UID:       types.UID("launched-process"),
		},
	}
	existingOwner := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-owner",
			Namespace: "test",
			UID:       types.UID("existing-owner"),
		},
	}
	reconciler := NewPhysicalProcessReconciler(
		ctx,
		nil,
		nil,
		logr.Discard(),
		&missingPhysicalProcessExecutor{},
		resiliency.NewWorkQueue(ctx, 1),
	)

	launchStateKey := physicalProcessDataKey(launchedProcess)
	reconciler.processData.Store(launchedProcess.NamespacedName(), launchStateKey, &physicalProcessData{
		resourceUID:    launchedProcess.UID,
		state:          physicalProcessStateStop,
		workInProgress: true,
		handle:         handle,
	})
	reconciler.processData.Store(existingOwner.NamespacedName(), physicalProcessHandleDataKey(handle), &physicalProcessData{
		resourceUID: existingOwner.UID,
		state:       physicalProcessStateRunning,
		handle:      handle,
	})

	finishedAt := time.Now()
	reconciler.queuePhysicalProcessDataResult(launchedProcess, launchStateKey, &physicalProcessData{
		resourceUID: launchedProcess.UID,
		state:       physicalProcessStateExited,
		handle:      handle,
		finishedAt:  finishedAt,
	})
	reconciler.processData.RunDeferredOps(launchedProcess.NamespacedName(), launchedProcess)

	ownerName, ownerData := reconciler.processData.BorrowByStateKey(physicalProcessHandleDataKey(handle))
	require.Equal(t, existingOwner.NamespacedName(), ownerName)
	require.NotNil(t, ownerData)
	require.Equal(t, existingOwner.UID, ownerData.resourceUID)

	currentStateKey, launchedData := reconciler.processData.BorrowByNamespacedName(launchedProcess.NamespacedName())
	require.Equal(t, launchStateKey, currentStateKey)
	require.NotNil(t, launchedData)
	require.Equal(t, physicalProcessStateExited, launchedData.state)
	require.False(t, launchedData.workInProgress)
	require.Equal(t, finishedAt, launchedData.finishedAt)
}

// Verifies that cleanup uncertainty remains monotonic for the same process identity
// but does not contaminate a replacement process that reuses the PID.
func TestPhysicalProcessCleanupUncertaintyIsScopedToProcessIdentity(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                      string
		replacementIdentityOffset time.Duration
		expectCleanupUnconfirmed  bool
	}{
		{
			name:                     "same identity preserves uncertainty",
			expectCleanupUnconfirmed: true,
		},
		{
			name:                      "replacement identity clears uncertainty",
			replacementIdentityOffset: time.Second,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
			defer cancel()
			physicalProcess := &apiv2.PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "identity-scope",
					Namespace: "test",
					UID:       types.UID("identity-scope-" + testCase.name),
				},
			}
			originalHandle := process.NewHandle(44, time.Unix(1200, 0).UTC())
			claimedHandle := process.NewHandle(
				originalHandle.Pid,
				originalHandle.IdentityTime.Add(testCase.replacementIdentityOffset),
			)
			data := &physicalProcessData{
				resourceUID:        physicalProcess.UID,
				state:              physicalProcessStateResolveRetry,
				handle:             originalHandle,
				failureReason:      apiv2.PhysicalProcessReasonStopFailed,
				failureMessage:     "cleanup was not confirmed",
				cleanupUnconfirmed: true,
			}
			reconciler := NewPhysicalProcessReconciler(
				ctx,
				nil,
				nil,
				logr.Discard(),
				&missingPhysicalProcessExecutor{},
				resiliency.NewWorkQueue(ctx, 1),
			)

			reconciler.processData.Store(
				physicalProcess.NamespacedName(),
				physicalProcessDataKey(physicalProcess),
				data.Clone(),
			)

			change := reconciler.claimPhysicalProcessTracking(physicalProcess, data, claimedHandle)

			require.Equal(t, noChange, change)
			currentStateKey, claimedData := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
			require.Equal(t, physicalProcessHandleDataKey(claimedHandle), currentStateKey)
			require.NotNil(t, claimedData)
			require.Equal(t, claimedHandle, claimedData.handle)
			require.Equal(t, testCase.expectCleanupUnconfirmed, claimedData.cleanupUnconfirmed)
			if testCase.expectCleanupUnconfirmed {
				require.Equal(t, apiv2.PhysicalProcessReasonDescendantCleanupUnconfirmed, claimedData.failureReason)
				require.Equal(t, data.failureMessage, claimedData.failureMessage)
			} else {
				require.Empty(t, claimedData.failureReason)
				require.Empty(t, claimedData.failureMessage)
			}
		})
	}
}

// Verifies that observing a tracked process's exit after an inspection failure clears stale diagnostics
// and reports RuntimeProcessExited without fabricating an exit code.
func TestHandlePhysicalProcessInspectFailedClearsFailureWhenTrackedProcessExits(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	handle := process.NewHandle(42, time.Now())
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-process",
			Namespace: "test",
			UID:       types.UID("test-process"),
		},
	}
	data := &physicalProcessData{
		resourceUID:    physicalProcess.UID,
		state:          physicalProcessStateInspectFailed,
		handle:         handle,
		failureReason:  apiv2.PhysicalProcessReasonStopFailed,
		failureMessage: "stale inspection failure",
		retryAfter:     time.Now().Add(-time.Second),
	}
	reconciler := NewPhysicalProcessReconciler(
		ctx,
		nil,
		nil,
		logr.Discard(),
		&missingPhysicalProcessExecutor{},
		resiliency.NewWorkQueue(ctx, 1),
	)

	reconciler.processData.Store(
		physicalProcess.NamespacedName(),
		physicalProcessHandleDataKey(handle),
		data.Clone(),
	)

	change := handlePhysicalProcessInspectFailed(
		ctx,
		reconciler,
		physicalProcess,
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, additionalReconciliationNeeded, change)
	require.Equal(t, physicalProcessStateExited, data.state)
	require.Empty(t, data.failureReason)
	require.Empty(t, data.failureMessage)
	_, _, valid := data.applyTo(physicalProcess)
	require.True(t, valid)
	require.Equal(t, apiv2.PhysicalProcessPhaseExited, physicalProcess.Status.Phase)
	ready := apimeta.FindStatusCondition(physicalProcess.Status.Conditions, string(apiv2.ConditionReady))
	require.NotNil(t, ready)
	require.Equal(t, string(apiv2.PhysicalProcessReasonRuntimeProcessExited), ready.Reason)
	require.Nil(t, physicalProcess.Status.ExitCode)
	require.False(t, physicalProcess.Status.FinishedAt.IsZero())
}

// Verifies that runtime inspection failures replace any stale condition reason
// with RuntimeProcessInspectFailed.
func TestHandlePhysicalProcessInspectFailedSetsInspectionFailureReason(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	inspectionErr := errors.New("inspection failed")
	handle := process.NewHandle(42, time.Now())
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-process",
			Namespace: "test",
			UID:       types.UID("test-process"),
		},
	}
	data := &physicalProcessData{
		resourceUID:   physicalProcess.UID,
		state:         physicalProcessStateInspectFailed,
		handle:        handle,
		failureReason: apiv2.PhysicalProcessReasonStopFailed,
		retryAfter:    time.Now().Add(-time.Second),
	}
	reconciler := NewPhysicalProcessReconciler(
		ctx,
		nil,
		nil,
		logr.Discard(),
		&failingPhysicalProcessInspectionExecutor{err: inspectionErr},
		resiliency.NewWorkQueue(ctx, 1),
	)

	change := handlePhysicalProcessInspectFailed(
		ctx,
		reconciler,
		physicalProcess,
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, noChange, change)
	require.Equal(t, physicalProcessStateInspectFailed, data.state)
	require.Equal(t, apiv2.PhysicalProcessReasonRuntimeProcessInspectFailed, data.failureReason)
	require.Contains(t, data.failureMessage, inspectionErr.Error())
}

// Verifies that starting a new stop attempt clears stale failure diagnostics
// so the in-progress projection reports the Stopping reason.
func TestSchedulePhysicalProcessStopClearsStaleFailureReason(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	handle := process.NewHandle(43, time.Now())
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-process",
			Namespace: "test",
			UID:       types.UID("test-process"),
		},
	}
	data := &physicalProcessData{
		resourceUID:    physicalProcess.UID,
		state:          physicalProcessStateRunning,
		handle:         handle,
		failureReason:  apiv2.PhysicalProcessReasonStopFailed,
		failureMessage: "stale stop failure",
	}
	reconciler := NewPhysicalProcessReconciler(
		ctx,
		nil,
		nil,
		logr.Discard(),
		&recordingPhysicalProcessExecutor{},
		resiliency.NewWorkQueue(ctx, 1),
	)

	reconciler.processData.Store(
		physicalProcess.NamespacedName(),
		physicalProcessHandleDataKey(handle),
		data.Clone(),
	)

	_, _ = reconciler.schedulePhysicalProcessStop(physicalProcess, data, logr.Discard())

	require.Equal(t, physicalProcessStateStop, data.state)
	require.True(t, data.workInProgress)
	require.Empty(t, data.failureReason)
	require.Empty(t, data.failureMessage)
}
