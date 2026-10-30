/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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

// Verifies that PhysicalProcess accepts identity differences within the shared process tolerance,
// and rejects a stale identity without stopping or claiming the current PID.
func TestV2PhysicalProcessControllerChecksExpectedIdentity(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-expected-identity")
	handle := seedAdoptablePhysicalProcess(t, ctx, "v2-pproc-expected-identity-command")
	pid := int64(handle.Pid)
	stale := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "stale", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{PID: &pid, IdentityTimestamp: metav1.NewMicroTime(handle.IdentityTime.Add(-time.Hour)), Stop: true}}
	require.NoError(t, client.Create(ctx, stale))
	waitPhysicalProcessPhase(t, ctx, stale.NamespacedName(), apiv2.PhysicalProcessPhaseExited)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
	withinTolerance := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "matching", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{PID: &pid, IdentityTimestamp: metav1.NewMicroTime(handle.IdentityTime.Add(time.Millisecond))}}
	require.NoError(t, client.Create(ctx, withinTolerance))
	waitPhysicalProcessPhase(t, ctx, withinTolerance.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)
	require.NoError(t, client.Delete(ctx, stale))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, stale)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
}

func TestV2PhysicalProcessControllerLaunchesProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-launch")
	executablePath := "v2-pproc-launch-command"
	inheritEnvironment := false
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "launched-process", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{
			Process: &apiv2.PhysicalProcessConfig{
				ExecutablePath:     executablePath,
				Args:               []string{"one", "two"},
				WorkingDirectory:   "/tmp",
				InheritEnvironment: &inheritEnvironment,
				Env:                []commonapi.EnvVar{{Name: "TEST_VALUE", Value: "expected"}},
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
	handle := seedAdoptablePhysicalProcess(t, ctx, "v2-pproc-existing-command")
	pid := int64(handle.Pid)
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-process", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))

	runningProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)
	require.Equal(t, pid, *runningProcess.Status.PID)
	require.Equal(t, handle.IdentityTime.UTC().Truncate(time.Second), runningProcess.Status.IdentityTimestamp.Time.UTC().Truncate(time.Second))
	require.NoError(t, client.Delete(ctx, runningProcess))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalProcess](t, ctx, client, runningProcess)

	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle), "adopted process must survive deletion of the PhysicalProcess")
}

func TestV2PhysicalProcessControllerReportsAdoptedProcessExit(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-adopted-exit")
	handle := seedAdoptablePhysicalProcess(t, ctx, "v2-pproc-adopted-exit-command")
	pid := int64(handle.Pid)
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "adopted-process", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))
	runningProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)

	testProcessExecutor.SimulateProcessExit(t, handle.Pid, 0)
	require.NoError(t, retryOnConflict[apiv2.PhysicalProcess](ctx, runningProcess.NamespacedName(), func(ctx context.Context, current *apiv2.PhysicalProcess) error {
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations["test"] = "reconcile"
		return client.Update(ctx, current)
	}))

	exitedProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseExited)
	requireReadyCondition(t, exitedProcess.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalProcessReasonRuntimeProcessMissing)
}

func TestV2PhysicalProcessControllerReportsMissingExistingProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-missing")
	handle := seedAdoptablePhysicalProcess(t, ctx, "v2-pproc-missing-command")
	require.NoError(t, testProcessExecutor.StopProcess(ctx, handle), "could not retire the seeded process")

	pid := int64(handle.Pid)
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-process", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))

	exitedProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseExited)
	require.Equal(t, pid, *exitedProcess.Status.PID)
	requireReadyCondition(t, exitedProcess.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalProcessReasonRuntimeProcessMissing)

	// Deletion must not be blocked by the inability to identify the runtime process.
	require.NoError(t, client.Delete(ctx, exitedProcess))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalProcess](t, ctx, client, exitedProcess)
}

func TestV2PhysicalProcessControllerRejectsAlreadyTrackedProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-tracked")
	handle := seedAdoptablePhysicalProcess(t, ctx, "v2-pproc-tracked-command")
	pid := int64(handle.Pid)

	owner := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "tracking-owner", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
	}
	require.NoError(t, client.Create(ctx, owner))
	waitPhysicalProcessPhase(t, ctx, owner.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)

	duplicate := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "tracking-duplicate", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
	}
	require.NoError(t, client.Create(ctx, duplicate))

	pendingProcess := waitObjectAssumesState(t, ctx, duplicate.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return condition != nil && condition.Reason == string(apiv2.PhysicalProcessReasonRuntimeProcessAlreadyTracked), nil
	})
	require.Equal(t, apiv2.PhysicalProcessPhasePending, pendingProcess.Status.Phase)
	readyCondition := apimeta.FindStatusCondition(pendingProcess.Status.Conditions, string(apiv2.ConditionReady))
	require.Contains(t, readyCondition.Message, owner.Name, "diagnostics should name the owning PhysicalProcess")

	// The duplicate must not disturb the process it failed to claim.
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
	require.NoError(t, client.Delete(ctx, duplicate))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalProcess](t, ctx, client, duplicate)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
}

func TestV2PhysicalProcessControllerStopsExistingProcessOnRequest(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-existing-stop")
	handle := seedAdoptablePhysicalProcess(t, ctx, "v2-pproc-existing-stop-command")
	pid := int64(handle.Pid)
	physicalProcess := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-stopping-process", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
	}
	require.NoError(t, client.Create(ctx, physicalProcess))
	waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)

	require.NoError(t, retryOnConflict[apiv2.PhysicalProcess](ctx, physicalProcess.NamespacedName(), func(ctx context.Context, current *apiv2.PhysicalProcess) error {
		current.Spec.Stop = true
		return client.Update(ctx, current)
	}))

	exitedProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseExited)
	requireReadyCondition(t, exitedProcess.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalProcessReasonRuntimeProcessExited)

	execution, found := testProcessExecutor.FindByPid(handle.Pid)
	require.True(t, found)
	require.True(t, execution.Finished(), "an adopted process must be terminated when a stop is requested")
	if osutil.IsWindows() {
		dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
		require.NoError(t, dcpPathErr)
		stopExecutions := testProcessExecutor.FindAll(
			[]string{dcpPath, "stop-process-tree", "--pid", strconv.FormatInt(int64(handle.Pid), 10)},
			"",
			nil,
		)
		require.Len(t, stopExecutions, 1)
	}
}

// Verifies that PhysicalProcess completes policy-specific cleanup and publishes its final
// outcome while a logical hold retains the API object, including monitor-scoped retention.
func TestV2PhysicalProcessControllerDeletesOrRetainsCreatedProcess(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr)
	customMonitorPID := int64(12345)

	testCases := []struct {
		name             string
		slug             string
		retain           bool
		customMonitorPID *int64
	}{
		{name: "deletes", slug: "deletes", retain: false},
		{name: "retains", slug: "retains", retain: true},
		{name: "retains with monitor", slug: "retains-with-monitor", retain: true, customMonitorPID: &customMonitorPID},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			namespace := createActiveV2Namespace(t, ctx, "v2-pproc-"+testCase.slug)
			executablePath := "v2-pproc-" + testCase.slug + "-command"
			monitorTimestamp := metav1.NewMicroTime(time.Now().Add(-time.Minute))
			physicalProcess := &apiv2.PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{Name: testCase.slug + "-process", Namespace: namespace.Name,
					Finalizers: []string{controllers.LogicalBackingFinalizer}},
				Spec: apiv2.PhysicalProcessSpec{
					Process: &apiv2.PhysicalProcessConfig{
						ExecutablePath:       executablePath,
						RetainRuntimeProcess: testCase.retain,
						MonitorPID:           testCase.customMonitorPID,
						MonitorTimestamp:     monitorTimestamp,
					},
				},
			}
			if testCase.customMonitorPID == nil {
				physicalProcess.Spec.Process.MonitorTimestamp = metav1.MicroTime{}
			}
			require.NoError(t, client.Create(ctx, physicalProcess))
			removeDependencyHoldOnCleanup(t, ctx, physicalProcess, controllers.LogicalBackingFinalizer)
			runningProcess := waitPhysicalProcessPhase(t, ctx, physicalProcess.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)
			if testCase.customMonitorPID != nil {
				require.NotNil(t, runningProcess.Spec.Process.MonitorPID)
				require.Equal(t, *testCase.customMonitorPID, *runningProcess.Spec.Process.MonitorPID)
				require.True(t, osutil.Within(monitorTimestamp.Time, runningProcess.Spec.Process.MonitorTimestamp.Time, 2*time.Microsecond))
			}
			pid, convertErr := process.Int64_ToPidT(*runningProcess.Status.PID)
			require.NoError(t, convertErr)
			runningExecution, runningExecutionFound := testProcessExecutor.FindByPid(pid)
			require.True(t, runningExecutionFound)
			require.True(t, runningExecution.Running())
			monitorExecutions := testProcessExecutor.FindAll(
				[]string{dcpPath, "monitor-process", "--child", strconv.FormatInt(int64(pid), 10)},
				"",
				nil,
			)
			if testCase.customMonitorPID != nil {
				require.Len(t, monitorExecutions, 1)
				require.Contains(t, monitorExecutions[0].Cmd.Args, strconv.FormatInt(*testCase.customMonitorPID, 10))
				require.Contains(t, monitorExecutions[0].Cmd.Args, monitorTimestamp.Time.Format(osutil.RFC3339MiliTimestampFormat))
			} else if testCase.retain {
				require.Empty(t, monitorExecutions)
			} else {
				require.Len(t, monitorExecutions, 1)
			}

			require.NoError(t, client.Delete(ctx, runningProcess))
			cleaned := waitObjectAssumesState(t, ctx, physicalProcess.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
				return len(current.Finalizers) == 1 && current.Finalizers[0] == controllers.LogicalBackingFinalizer, nil
			})
			if !testCase.retain {
				require.Equal(t, apiv2.PhysicalProcessPhaseExited, cleaned.Status.Phase)
			}

			execution, found := testProcessExecutor.FindByPid(pid)
			require.True(t, found)
			require.Equal(t, testCase.retain, execution.Running())
			if testCase.retain {
				testProcessExecutor.SimulateProcessExit(t, pid, 0)
			}
			require.NoError(t, controllers.ReleaseLogicalBackingFinalizer(ctx, client, cleaned))
			ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalProcess](t, ctx, client, physicalProcess)
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
	t.Parallel()
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

func TestV2PhysicalProcessControllerReportsStopFailure(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-stop-failure")
	executablePath := "v2-pproc-stop-failure-command"
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{executablePath}}
	finishExecution := make(chan struct{})
	testProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria,
		RunCommand: func(*internal_testutil.ProcessExecution) int32 {
			<-finishExecution
			return 0
		},
		StopError: func(*internal_testutil.ProcessExecution) error {
			return errors.New("simulated stop failure")
		},
	})
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			testProcessExecutor.RemoveAutoExecution(criteria)
			close(finishExecution)
		})
	}
	defer cleanup()

	physicalProcess := createRunningPhysicalProcess(t, ctx, namespace.Name, "stop-failure-process", executablePath)
	require.NoError(t, retryOnConflict[apiv2.PhysicalProcess](ctx, physicalProcess.NamespacedName(), func(ctx context.Context, current *apiv2.PhysicalProcess) error {
		current.Spec.Stop = true
		return client.Update(ctx, current)
	}))

	failedProcess := waitObjectAssumesState(t, ctx, physicalProcess.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		readyCondition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return readyCondition != nil &&
			apiv2.ConditionReason(readyCondition.Reason) == apiv2.PhysicalProcessReasonStopFailed, nil
	})
	require.Equal(t, apiv2.PhysicalProcessPhaseUnknown, failedProcess.Status.Phase)
	requireReadyCondition(t, failedProcess.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalProcessReasonStopFailed)
	cleanup()
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

// Verifies that PhysicalProcess preserves a terminal launch failure after stop is requested,
// even when the original cause of the launch failure has been removed.
func TestV2PhysicalProcessControllerPreservesLaunchFailureAfterStopRequest(t *testing.T) {
	t.Parallel()
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

	stoppedProcess := waitObjectAssumesState(t, ctx, physicalProcess.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.PhysicalProcessPhaseFailed &&
			condition != nil && condition.ObservedGeneration == current.Generation && current.Spec.Stop, nil
	})
	requireReadyCondition(t, stoppedProcess.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalProcessReasonLaunchFailed)
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

// Verifies that PhysicalProcess treats launch errors as terminal and a new resource
// can explicitly attempt launch again after the original resource is deleted.
func TestV2PhysicalProcessControllerLaunchFailureRequiresNewResource(t *testing.T) {
	t.Parallel()
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
	require.Equal(t, apiv2.PhysicalProcessPhaseFailed, failedProcess.Status.Phase)

	testProcessExecutor.RemoveAutoExecution(criteria)
	require.NoError(t, client.Delete(ctx, physicalProcess))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physicalProcess)
	replacement := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: physicalProcess.Name, Namespace: namespace.Name}, Spec: *physicalProcess.Spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, replacement))
	waitPhysicalProcessPhase(t, ctx, replacement.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)
	require.Len(t, testProcessExecutor.FindAll([]string{executablePath}, "", nil), 2)
}

// Verifies that PhysicalProcess retries an incomplete stop and preserves the cleanup warning
// after the retry successfully stops the root.
func TestV2PhysicalProcessControllerPreservesCleanupWarningAfterRetry(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-pproc-cleanup-retry")
	physicalProcess := createRunningPhysicalProcess(t, ctx, namespace.Name, "cleanup-process", "v2-pproc-cleanup-retry-command")
	pid, convertErr := process.Int64_ToPidT(*physicalProcess.Status.PID)
	require.NoError(t, convertErr)
	handle, handleErr := testProcessExecutor.FindProcessHandle(pid)
	require.NoError(t, handleErr)
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{physicalProcess.Spec.Process.ExecutablePath}}
	testProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria,
		StopError: func(*internal_testutil.ProcessExecution) error {
			return process.ErrIncompleteProcessTree
		},
	})
	t.Cleanup(func() { testProcessExecutor.RemoveAutoExecution(criteria) })
	require.NoError(t, retryOnConflict[apiv2.PhysicalProcess](ctx, physicalProcess.NamespacedName(), func(ctx context.Context, current *apiv2.PhysicalProcess) error {
		current.Spec.Stop = true
		return client.Update(ctx, current)
	}))
	failed := waitObjectAssumesState(t, ctx, physicalProcess.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return condition != nil && condition.Reason == string(apiv2.PhysicalProcessReasonStopFailed), nil
	})
	require.Equal(t, apiv2.PhysicalProcessPhaseUnknown, failed.Status.Phase)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))

	testProcessExecutor.RemoveAutoExecution(criteria)
	completed := waitObjectAssumesState(t, ctx, physicalProcess.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return condition != nil && condition.Reason == string(apiv2.PhysicalProcessReasonDescendantCleanupUnconfirmed), nil
	})
	require.Equal(t, apiv2.PhysicalProcessPhaseExited, completed.Status.Phase)
	require.True(t, process.IsProcessGoneErr(testProcessExecutor.CheckProcessRunning(handle)))
	require.NoError(t, client.Delete(ctx, completed))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, completed)
}

// seedAdoptablePhysicalProcess registers a process execution with the shared test process executor so
// that PhysicalProcess objects can adopt it the way they would adopt a process started outside of DCP.
func seedAdoptablePhysicalProcess(t *testing.T, ctx context.Context, executablePath string) process.ProcessHandle {
	t.Helper()
	handle, _, startErr := testProcessExecutor.StartProcess(ctx, exec.Command(executablePath), nil, process.CreationFlagsNone, nil)
	require.NoError(t, startErr, "could not seed process execution")
	t.Cleanup(func() {
		_ = testProcessExecutor.StopProcess(context.WithoutCancel(ctx), handle)
	})
	return handle
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
