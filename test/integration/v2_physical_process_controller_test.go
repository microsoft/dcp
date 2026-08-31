/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/dcppaths"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/osutil"
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

func TestV2PhysicalProcessControllerLaunchesProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-launch")
	executablePath := "v2-pproc-launch-command"
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "launched-process", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{
				ExecutablePath:   executablePath,
				Args:             []string{"one", "two"},
				WorkingDirectory: "/tmp",
				Env:              []commonapi.EnvVar{{Name: "TEST_VALUE", Value: "expected"}},
			},
		},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))

	runningProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)
	require.NotNil(t, runningProcess.Status.PID)
	require.False(t, runningProcess.Status.IdentityTimestamp.IsZero())
	requireReadyCondition(t, runningProcess.Status.Conditions, metav1.ConditionTrue, apiv2.PhysicalProcessReasonRuntimeProcessRunning)

	executions := testProcessExecutor.FindAll([]string{executablePath}, "", nil)
	require.Len(t, executions, 1)
	require.Equal(t, []string{executablePath, "one", "two"}, executions[0].Cmd.Args)
	require.Equal(t, "/tmp", executions[0].Cmd.Dir)
	require.Equal(t, []string{"TEST_VALUE=expected"}, executions[0].Cmd.Env)
}

func TestV2PhysicalProcessControllerObservesAndPreservesExistingProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-existing")
	pid := int64(os.Getpid())
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-process", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))

	runningProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)
	require.Equal(t, pid, *runningProcess.Status.PID)
	require.NoError(t, client.Delete(ctx, runningProcess))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalProcess](t, ctx, client, runningProcess)

	handlePID, convertErr := process.Int64_ToPidT(pid)
	require.NoError(t, convertErr)
	osProcess, findErr := process.FindProcess(process.NewHandle(handlePID, runningProcess.Status.IdentityTimestamp.Time))
	require.NoError(t, findErr)
	require.NoError(t, osProcess.Release())
}

func TestV2PhysicalProcessControllerDeletesOrRetainsCreatedProcess(t *testing.T) {
	dcppaths.EnableTestPathProbing()
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr)

	testCases := []struct {
		name   string
		retain bool
	}{
		{name: "deletes", retain: false},
		{name: "retains", retain: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			namespace := createActiveV2Namespace(t, ctx, "v2-pproc-"+testCase.name)
			executablePath := "v2-pproc-" + testCase.name + "-command"
			physicalProcess := &apiv2.PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{Name: testCase.name + "-process", Namespace: namespace.Name},
				Spec: apiv2.PhysicalProcessSpec{
					Process: &apiv2.PhysicalProcessConfig{
						ExecutablePath:       executablePath,
						RetainRuntimeProcess: testCase.retain,
					},
				},
			}
			require.NoError(t, client.Create(ctx, physicalProcess))
			runningProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)
			pid, convertErr := process.Int64_ToPidT(*runningProcess.Status.PID)
			require.NoError(t, convertErr)
			monitorExecutions := testProcessExecutor.FindAll(
				[]string{dcpPath, "monitor-process", "--child", strconv.FormatInt(int64(pid), 10)},
				"",
				nil,
			)
			if testCase.retain {
				require.Empty(t, monitorExecutions)
			} else {
				require.Len(t, monitorExecutions, 1)
			}

			require.NoError(t, client.Delete(ctx, runningProcess))
			ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalProcess](t, ctx, client, runningProcess)

			execution, found := testProcessExecutor.FindByPid(pid)
			require.True(t, found)
			require.Equal(t, testCase.retain, execution.Running())
			if testCase.retain {
				testProcessExecutor.SimulateProcessExit(t, pid, 0)
			}
		})
	}
}

func TestV2PhysicalProcessControllerReportsExit(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-exit")
	physicalProcess := createRunningPhysicalProcess(t, ctx, namespace.Name, "exiting-process", "v2-pproc-exit-command")
	pid, convertErr := process.Int64_ToPidT(*physicalProcess.Status.PID)
	require.NoError(t, convertErr)

	const exitCode int32 = 17
	testProcessExecutor.SimulateProcessExit(t, pid, exitCode)
	exitedProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseExited)
	require.NotNil(t, exitedProcess.Status.ExitCode)
	require.Equal(t, exitCode, *exitedProcess.Status.ExitCode)
	require.False(t, exitedProcess.Status.FinishedAt.IsZero())
	requireReadyCondition(t, exitedProcess.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalProcessReasonRuntimeProcessExited)
}

func TestV2PhysicalProcessControllerReportsExitBeforeRunningStatus(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-early-exit")
	executablePath := "v2-pproc-early-exit-command"
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{executablePath}}
	testProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria,
		RunCommand: func(*internal_testutil.ProcessExecution) int32 {
			return 23
		},
	})
	defer testProcessExecutor.RemoveAutoExecution(criteria)

	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "early-exit-process", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{ExecutablePath: executablePath},
		},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))
	exitedProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseExited)
	require.NotNil(t, exitedProcess.Status.ExitCode)
	require.Equal(t, int32(23), *exitedProcess.Status.ExitCode)
	require.Len(t, testProcessExecutor.FindAll([]string{executablePath}, "", nil), 1)
}

func TestV2PhysicalProcessControllerStopsProcessOnRequest(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-stop")
	physicalProcess := createRunningPhysicalProcess(t, ctx, namespace.Name, "stopping-process", "v2-pproc-stop-command")
	pid, convertErr := process.Int64_ToPidT(*physicalProcess.Status.PID)
	require.NoError(t, convertErr)

	require.NoError(t, retryOnConflict[apiv2.PhysicalProcess](ctx, physicalProcess.NamespacedName(), func(ctx context.Context, current *apiv2.PhysicalProcess) error {
		current.Spec.Stop = true
		return client.Update(ctx, current)
	}))
	exitedProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseExited)
	requireReadyCondition(t, exitedProcess.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalProcessReasonRuntimeProcessExited)

	execution, found := testProcessExecutor.FindByPid(pid)
	require.True(t, found)
	require.True(t, execution.Finished())
	if osutil.IsWindows() {
		dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
		require.NoError(t, dcpPathErr)
		stopExecutions := testProcessExecutor.FindAll(
			[]string{dcpPath, "stop-process-tree", "--pid", strconv.FormatInt(int64(pid), 10)},
			"",
			nil,
		)
		require.Len(t, stopExecutions, 1)
	}
}

func TestV2PhysicalProcessControllerDoesNotLaunchStoppedProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-initially-stopped")
	executablePath := "v2-pproc-initially-stopped-command"
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "initially-stopped-process", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{ExecutablePath: executablePath},
			Stop:    true,
		},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))

	stoppedProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseExited)
	require.Nil(t, stoppedProcess.Status.PID)
	requireReadyCondition(t, stoppedProcess.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalProcessReasonStopRequested)
	require.Empty(t, testProcessExecutor.FindAll([]string{executablePath}, "", nil))
}

func TestV2PhysicalProcessControllerDoesNotRetryLaunchAfterStopRequest(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-stop-launch-retry")
	executablePath := "v2-pproc-stop-launch-retry-command"
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{executablePath}}
	testProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria,
		StartupError: func(*internal_testutil.ProcessExecution) error {
			return errors.New("simulated launch failure")
		},
	})
	defer testProcessExecutor.RemoveAutoExecution(criteria)

	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "stopped-retry-process", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{ExecutablePath: executablePath},
		},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))
	failedProcess := waitObjectAssumesState(t, ctx, physicalProcess.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return condition != nil && apiv2.ConditionReason(condition.Reason) == apiv2.PhysicalProcessReasonLaunchFailed, nil
	})

	require.NoError(t, retryOnConflict[apiv2.PhysicalProcess](ctx, failedProcess.NamespacedName(), func(ctx context.Context, current *apiv2.PhysicalProcess) error {
		current.Spec.Stop = true
		return client.Update(ctx, current)
	}))
	testProcessExecutor.RemoveAutoExecution(criteria)

	stoppedProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseExited)
	requireReadyCondition(t, stoppedProcess.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalProcessReasonStopRequested)
	require.Len(t, testProcessExecutor.FindAll([]string{executablePath}, "", nil), 1)
}

func TestV2PhysicalProcessControllerCleansUpOnNamespaceDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-namespace-delete")
	physicalProcess := createRunningPhysicalProcess(t, ctx, namespace.Name, "namespace-process", "v2-pproc-namespace-command")
	pid, convertErr := process.Int64_ToPidT(*physicalProcess.Status.PID)
	require.NoError(t, convertErr)

	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalProcess](t, ctx, client, physicalProcess)
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
	execution, found := testProcessExecutor.FindByPid(pid)
	require.True(t, found)
	require.True(t, execution.Finished())
}

func TestV2PhysicalProcessControllerWaitsForNamespace(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespaceName := "v2-pproc-wait-namespace"
	executablePath := "v2-pproc-wait-command"
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "waiting-process", Namespace: namespaceName},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{ExecutablePath: executablePath},
		},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))
	pendingProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhasePending)
	requireReadyCondition(t, pendingProcess.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalResourceReasonNamespaceNotFound)
	require.Empty(t, testProcessExecutor.FindAll([]string{executablePath}, "", nil))

	createActiveV2Namespace(t, ctx, namespaceName)
	waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)
	require.Len(t, testProcessExecutor.FindAll([]string{executablePath}, "", nil), 1)
}

func TestV2PhysicalProcessControllerDoesNotDuplicateLaunch(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-no-duplicate")
	executablePath := "v2-pproc-no-duplicate-command"
	physicalProcess := createRunningPhysicalProcess(t, ctx, namespace.Name, "single-process", executablePath)

	require.NoError(t, retryOnConflict[apiv2.PhysicalProcess](ctx, physicalProcess.NamespacedName(), func(ctx context.Context, current *apiv2.PhysicalProcess) error {
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations["test.dcp.microsoft.com/reconcile"] = "again"
		return client.Update(ctx, current)
	}))
	require.Never(t, func() bool {
		return len(testProcessExecutor.FindAll([]string{executablePath}, "", nil)) > 1
	}, 3*time.Second, 250*time.Millisecond)
}

func TestV2PhysicalProcessControllerRetriesLaunchFailure(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-launch-retry")
	executablePath := "v2-pproc-launch-retry-command"
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{executablePath}}
	testProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria,
		StartupError: func(*internal_testutil.ProcessExecution) error {
			return errors.New("simulated launch failure")
		},
	})
	defer testProcessExecutor.RemoveAutoExecution(criteria)

	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-process", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{ExecutablePath: executablePath},
		},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))
	failedProcess := waitObjectAssumesState(t, ctx, physicalProcess.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return condition != nil && apiv2.ConditionReason(condition.Reason) == apiv2.PhysicalProcessReasonLaunchFailed, nil
	})
	requireReadyCondition(t, failedProcess.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalProcessReasonLaunchFailed)

	testProcessExecutor.RemoveAutoExecution(criteria)
	waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)
	require.Len(t, testProcessExecutor.FindAll([]string{executablePath}, "", nil), 2)
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
	reconciler := controllers.NewPhysicalProcessReconciler(ctx, statusClient, baseClient, testutil.NewLogForTesting(t.Name()), testProcessExecutor)
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
	require.Len(t, testProcessExecutor.FindAll([]string{physicalProcess.Spec.Process.ExecutablePath}, "", nil), 1)
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
	blockingExecutor := &blockingStartProcessExecutor{
		Executor: testProcessExecutor,
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

	executions := testProcessExecutor.FindAll([]string{physicalProcess.Spec.Process.ExecutablePath}, "", nil)
	require.Len(t, executions, 1)
	require.True(t, executions[0].Finished())
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
	processExecutor := &invalidIdentityOnceExecutor{Executor: testProcessExecutor}
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

	executions := testProcessExecutor.FindAll([]string{physicalProcess.Spec.Process.ExecutablePath}, "", nil)
	require.Len(t, executions, 2)
	require.True(t, executions[0].Finished())
	require.True(t, executions[1].Running())
}

func waitPhysicalProcessPhase(
	t *testing.T,
	ctx context.Context,
	name types.NamespacedName,
	phase apiv2.PhysicalProcessPhase,
) *apiv2.PhysicalProcess {
	t.Helper()
	return waitObjectAssumesState(t, ctx, name, func(physicalProcess *apiv2.PhysicalProcess) (bool, error) {
		return physicalProcess.Status.Phase == phase, nil
	})
}

func createRunningPhysicalProcess(
	t *testing.T,
	ctx context.Context,
	namespace string,
	name string,
	executablePath string,
) *apiv2.PhysicalProcess {
	t.Helper()
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{ExecutablePath: executablePath},
		},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))
	return waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)
}
