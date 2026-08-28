/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type failOncePhysicalProcessStatusClient struct {
	ctrl_client.Client

	lock      sync.Mutex
	triggered bool
}

func (client *failOncePhysicalProcessStatusClient) Status() ctrl_client.SubResourceWriter {
	return &failOncePhysicalProcessStatusWriter{
		SubResourceWriter: client.Client.Status(),
		client:            client,
	}
}

func (client *failOncePhysicalProcessStatusClient) failStatusPatch(obj ctrl_client.Object) error {
	physicalProcess, isPhysicalProcess := obj.(*apiv2.PhysicalProcess)
	if !isPhysicalProcess || physicalProcess.Status.PID == nil {
		return nil
	}

	client.lock.Lock()
	defer client.lock.Unlock()
	if client.triggered {
		return nil
	}
	client.triggered = true
	return apierrors.NewConflict(
		schema.GroupResource{Group: apiv2.GroupVersion.Group, Resource: "physicalprocesses"},
		physicalProcess.Name,
		context.DeadlineExceeded,
	)
}

func (client *failOncePhysicalProcessStatusClient) failureTriggered() bool {
	client.lock.Lock()
	defer client.lock.Unlock()
	return client.triggered
}

type failOncePhysicalProcessStatusWriter struct {
	ctrl_client.SubResourceWriter
	client *failOncePhysicalProcessStatusClient
}

func (writer *failOncePhysicalProcessStatusWriter) Patch(
	ctx context.Context,
	obj ctrl_client.Object,
	patch ctrl_client.Patch,
	opts ...ctrl_client.SubResourcePatchOption,
) error {
	if statusErr := writer.client.failStatusPatch(obj); statusErr != nil {
		return statusErr
	}
	return writer.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

type blockingStartProcessExecutor struct {
	process.Executor

	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
}

type invalidIdentityOnceExecutor struct {
	process.Executor

	lock     sync.Mutex
	returned bool
}

func (executor *invalidIdentityOnceExecutor) StartProcess(
	ctx context.Context,
	cmd *exec.Cmd,
	exitHandler process.ProcessExitHandler,
	creationFlags process.ProcessCreationFlag,
	sysCreateProcess process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	handle, startWaitForExit, startErr := executor.Executor.StartProcess(ctx, cmd, exitHandler, creationFlags, sysCreateProcess)
	if startErr != nil {
		return handle, startWaitForExit, startErr
	}

	executor.lock.Lock()
	defer executor.lock.Unlock()
	if !executor.returned {
		executor.returned = true
		handle.IdentityTime = time.Time{}
	}
	return handle, startWaitForExit, nil
}

func (executor *blockingStartProcessExecutor) StartProcess(
	ctx context.Context,
	cmd *exec.Cmd,
	exitHandler process.ProcessExitHandler,
	creationFlags process.ProcessCreationFlag,
	sysCreateProcess process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	executor.startedOnce.Do(func() {
		close(executor.started)
	})
	select {
	case <-ctx.Done():
		return process.ProcessHandle{Pid: process.UnknownPID}, nil, ctx.Err()
	case <-executor.release:
	}
	return executor.Executor.StartProcess(ctx, cmd, exitHandler, creationFlags, sysCreateProcess)
}

func TestV2PhysicalProcessControllerDoesNotRelaunchAfterStatusConflict(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "v2-pproc-status-conflict",
			Finalizers: []string{apiv2.NamespaceFinalizer},
		},
		Status: apiv2.NamespaceStatus{Phase: apiv2.NamespacePhaseActive},
	}
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "conflict-process", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{ExecutablePath: "v2-pproc-conflict-command"},
		},
	}
	baseClient := fake.NewClientBuilder().
		WithScheme(client.Scheme()).
		WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalProcess{}).
		WithObjects(namespace, physicalProcess).
		Build()
	statusClient := &failOncePhysicalProcessStatusClient{Client: baseClient}
	processExecutor := internal_testutil.NewTestProcessExecutor(ctx)
	reconciler := controllers.NewPhysicalProcessReconciler(ctx, statusClient, baseClient, testutil.NewLogForTesting(t.Name()), processExecutor)
	request := ctrl.Request{NamespacedName: physicalProcess.NamespacedName()}

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		if reconcileErr != nil {
			return false, reconcileErr
		}
		current := apiv2.PhysicalProcess{}
		getErr := baseClient.Get(ctx, request.NamespacedName, &current)
		if getErr != nil {
			return false, getErr
		}
		return current.Status.Phase == apiv2.PhysicalProcessPhaseRunning && statusClient.failureTriggered(), nil
	})
	require.NoError(t, waitErr)
	require.Len(t, processExecutor.FindAll([]string{physicalProcess.Spec.Process.ExecutablePath}, "", nil), 1)
}

func TestV2PhysicalProcessControllerStopsProcessWhenDeletedDuringLaunch(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "v2-pproc-delete-launch",
			Finalizers: []string{apiv2.NamespaceFinalizer},
		},
		Status: apiv2.NamespaceStatus{Phase: apiv2.NamespacePhaseActive},
	}
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "deleting-process", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{ExecutablePath: "v2-pproc-delete-launch-command"},
		},
	}
	baseClient := fake.NewClientBuilder().
		WithScheme(client.Scheme()).
		WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalProcess{}).
		WithObjects(namespace, physicalProcess).
		Build()
	testExecutor := internal_testutil.NewTestProcessExecutor(ctx)
	blockingExecutor := &blockingStartProcessExecutor{
		Executor: testExecutor,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	reconciler := controllers.NewPhysicalProcessReconciler(ctx, baseClient, baseClient, testutil.NewLogForTesting(t.Name()), blockingExecutor)
	request := ctrl.Request{NamespacedName: physicalProcess.NamespacedName()}

	_, firstReconcileErr := reconciler.Reconcile(ctx, request)
	require.NoError(t, firstReconcileErr)
	_, launchReconcileErr := reconciler.Reconcile(ctx, request)
	require.NoError(t, launchReconcileErr)
	select {
	case <-blockingExecutor.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	current := apiv2.PhysicalProcess{}
	require.NoError(t, baseClient.Get(ctx, request.NamespacedName, &current))
	require.NoError(t, baseClient.Delete(ctx, &current))
	_, deletionReconcileErr := reconciler.Reconcile(ctx, request)
	require.NoError(t, deletionReconcileErr)
	close(blockingExecutor.release)

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		if reconcileErr != nil {
			return false, reconcileErr
		}
		getErr := baseClient.Get(ctx, request.NamespacedName, &apiv2.PhysicalProcess{})
		return apierrors.IsNotFound(getErr), nil
	})
	require.NoError(t, waitErr)

	executions := testExecutor.FindAll([]string{physicalProcess.Spec.Process.ExecutablePath}, "", nil)
	require.Len(t, executions, 1)
	require.True(t, executions[0].Finished())
}

func TestV2PhysicalProcessControllerStopsUndurableRetainedProcessAtShutdown(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	reconcilerCtx, stopReconciler := context.WithCancel(ctx)

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "v2-pproc-retained-shutdown",
			Finalizers: []string{apiv2.NamespaceFinalizer},
		},
		Status: apiv2.NamespaceStatus{Phase: apiv2.NamespacePhaseActive},
	}
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "retained-process", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{
				ExecutablePath:       "v2-pproc-retained-shutdown-command",
				RetainRuntimeProcess: true,
			},
		},
	}
	baseClient := fake.NewClientBuilder().
		WithScheme(client.Scheme()).
		WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalProcess{}).
		WithObjects(namespace, physicalProcess).
		Build()
	statusClient := &failOncePhysicalProcessStatusClient{Client: baseClient}
	processExecutor := internal_testutil.NewTestProcessExecutor(ctx)
	reconciler := controllers.NewPhysicalProcessReconciler(reconcilerCtx, statusClient, baseClient, testutil.NewLogForTesting(t.Name()), processExecutor)
	request := ctrl.Request{NamespacedName: physicalProcess.NamespacedName()}

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		if reconcileErr != nil {
			return false, reconcileErr
		}
		return statusClient.failureTriggered(), nil
	})
	require.NoError(t, waitErr)

	executions := processExecutor.FindAll([]string{physicalProcess.Spec.Process.ExecutablePath}, "", nil)
	require.Len(t, executions, 1)
	require.True(t, executions[0].Running())
	handle := process.NewHandle(executions[0].PID, executions[0].StartedAt)
	stopReconciler()

	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		return processExecutor.CheckProcessRunning(handle) != nil, nil
	})
	require.NoError(t, waitErr)
}

func TestV2PhysicalProcessControllerCleansInvalidLaunchIdentityBeforeRetry(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "v2-pproc-invalid-identity",
			Finalizers: []string{apiv2.NamespaceFinalizer},
		},
		Status: apiv2.NamespaceStatus{Phase: apiv2.NamespacePhaseActive},
	}
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-identity-process", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{ExecutablePath: "v2-pproc-invalid-identity-command"},
		},
	}
	baseClient := fake.NewClientBuilder().
		WithScheme(client.Scheme()).
		WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalProcess{}).
		WithObjects(namespace, physicalProcess).
		Build()
	testExecutor := internal_testutil.NewTestProcessExecutor(ctx)
	processExecutor := &invalidIdentityOnceExecutor{Executor: testExecutor}
	reconciler := controllers.NewPhysicalProcessReconciler(ctx, baseClient, baseClient, testutil.NewLogForTesting(t.Name()), processExecutor)
	request := ctrl.Request{NamespacedName: physicalProcess.NamespacedName()}

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		if reconcileErr != nil {
			return false, reconcileErr
		}
		current := apiv2.PhysicalProcess{}
		getErr := baseClient.Get(ctx, request.NamespacedName, &current)
		return current.Status.Phase == apiv2.PhysicalProcessPhaseRunning, getErr
	})
	require.NoError(t, waitErr)

	executions := testExecutor.FindAll([]string{physicalProcess.Spec.Process.ExecutablePath}, "", nil)
	require.Len(t, executions, 2)
	require.True(t, executions[0].Finished())
	require.True(t, executions[1].Running())
}
