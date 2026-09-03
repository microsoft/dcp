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
		data,
		logr.Discard(),
	)

	require.Equal(t, additionalReconciliationNeeded, change)
	require.Zero(t, executor.findProcessHandleCalls.Load())
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
		data,
		logr.Discard(),
	)

	require.Equal(t, noChange, change)
	_, currentData := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
	require.NotNil(t, currentData)
	require.Equal(t, physicalResourceProgressMissing, currentData.progress)
	require.Empty(t, currentData.failureMessage)
}
