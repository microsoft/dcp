/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type missingPhysicalProcessExecutor struct {
	process.Executor
}

type recordingPhysicalProcessExecutor struct {
	process.Executor
	findProcessHandleCalls atomic.Int32
}

func (*missingPhysicalProcessExecutor) CheckProcessRunning(process.ProcessHandle) error {
	return process.ErrorProcessNotFound
}

func (e *recordingPhysicalProcessExecutor) FindProcessHandle(process.Pid_t) (process.ProcessHandle, error) {
	e.findProcessHandleCalls.Add(1)
	return process.ProcessHandle{}, process.ErrorProcessNotFound
}

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
		state:       physicalProcessStateResolve,
		progress:    physicalResourceProgressRetryPending,
		retryAfter:  time.Now().Add(time.Minute),
	}
	executor := &recordingPhysicalProcessExecutor{}
	reconciler := NewPhysicalProcessReconciler(
		ctx,
		nil,
		nil,
		logr.Discard(),
		executor,
	)

	change := handlePhysicalProcessResolve(
		ctx,
		reconciler,
		physicalProcess,
		physicalProcessStateResolve,
		physicalProcessDataKey(physicalProcess),
		data,
		logr.Discard(),
	)

	require.Equal(t, additionalReconciliationNeeded, change)
	require.Zero(t, executor.findProcessHandleCalls.Load())
}

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
	)
	launchStateKey := physicalProcessDataKey(launchedProcess)
	reconciler.processData.Store(launchedProcess.NamespacedName(), launchStateKey, &physicalProcessData{
		resourceUID: launchedProcess.UID,
		state:       physicalProcessStateLaunch,
		progress:    physicalResourceProgressInProgress,
	})
	reconciler.processData.Store(existingOwner.NamespacedName(), physicalProcessHandleDataKey(handle), &physicalProcessData{
		resourceUID: existingOwner.UID,
		state:       physicalProcessStateRuntime,
		progress:    physicalResourceProgressRunning,
		handle:      handle,
	})

	reconciler.queuePhysicalProcessDataResult(launchedProcess, launchStateKey, &physicalProcessData{
		resourceUID: launchedProcess.UID,
		state:       physicalProcessStateRuntime,
		progress:    physicalResourceProgressRunning,
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
	require.Equal(t, physicalProcessStateResolve, launchedData.state)
	require.Equal(t, physicalResourceProgressRetryPending, launchedData.progress)
	require.Equal(t, handle, launchedData.handle)
	require.Equal(t, apiv2.PhysicalProcessReasonRuntimeProcessAlreadyTracked, launchedData.failureReason)
	require.Contains(t, launchedData.failureMessage, existingOwner.Name)
}

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
	)
	launchStateKey := physicalProcessDataKey(launchedProcess)
	reconciler.processData.Store(launchedProcess.NamespacedName(), launchStateKey, &physicalProcessData{
		resourceUID: launchedProcess.UID,
		state:       physicalProcessStateStop,
		progress:    physicalResourceProgressInProgress,
		handle:      handle,
	})
	reconciler.processData.Store(existingOwner.NamespacedName(), physicalProcessHandleDataKey(handle), &physicalProcessData{
		resourceUID: existingOwner.UID,
		state:       physicalProcessStateRuntime,
		progress:    physicalResourceProgressRunning,
		handle:      handle,
	})

	finishedAt := time.Now()
	reconciler.queuePhysicalProcessDataResult(launchedProcess, launchStateKey, &physicalProcessData{
		resourceUID: launchedProcess.UID,
		state:       physicalProcessStateRuntime,
		progress:    physicalResourceProgressExited,
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
	require.Equal(t, physicalProcessStateRuntime, launchedData.state)
	require.Equal(t, physicalResourceProgressExited, launchedData.progress)
	require.Equal(t, finishedAt, launchedData.finishedAt)
}

func TestHandlePhysicalProcessRuntimeClearsFailureWhenProcessIsMissing(t *testing.T) {
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
		state:          physicalProcessStateRuntime,
		progress:       physicalResourceProgressRetryPending,
		handle:         handle,
		failureMessage: "stale inspection failure",
		retryAfter:     time.Now().Add(-time.Second),
	}
	reconciler := NewPhysicalProcessReconciler(
		ctx,
		nil,
		nil,
		logr.Discard(),
		&missingPhysicalProcessExecutor{},
	)
	reconciler.processData.Store(
		physicalProcess.NamespacedName(),
		physicalProcessHandleDataKey(handle),
		data.Clone(),
	)

	change := handlePhysicalProcessRuntime(
		ctx,
		reconciler,
		physicalProcess,
		physicalProcessStateRuntime,
		physicalProcessHandleDataKey(handle),
		data,
		logr.Discard(),
	)

	require.Equal(t, noChange, change)
	require.Equal(t, physicalResourceProgressMissing, data.progress)
	require.Empty(t, data.failureMessage)
}
