/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that logical and physical process endpoints share immediate captured stdout/stderr,
// support standard formatting and filtering, and finish followers when the process exits.
func TestV2ProcessLogEndpointsShareOutputAndCompleteOnExit(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-process-log-output")
	resource := &apiv2.LogicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec:       apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-process-log-output-command"}},
	}
	require.NoError(t, client.Create(ctx, resource))
	running := waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	physical := &apiv2.PhysicalProcess{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalProcessRef}, physical))
	require.Equal(t, physical.GetLogSource(), running.Status.RuntimeLogs)
	require.True(t, running.Status.RuntimeLogs.Available)
	pid, pidErr := process.Int64_ToPidT(*running.Status.PID)
	require.NoError(t, pidErr)
	execution, found := testProcessExecutor.FindByPid(pid)
	require.True(t, found)
	_, stdoutErr := io.WriteString(execution.Cmd.Stdout, "first-line\nsecond-line\nthird-line\n")
	require.NoError(t, stdoutErr)
	_, stderrErr := io.WriteString(execution.Cmd.Stderr, "stderr-line\n")
	require.NoError(t, stderrErr)
	for _, source := range []string{"", "stdout", "stderr"} {
		logicalOutput, logicalErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Source: source})
		require.NoError(t, logicalErr)
		physicalOutput, physicalErr := readV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Source: source})
		require.NoError(t, physicalErr)
		require.Equal(t, logicalOutput, physicalOutput)
		if source == "stderr" {
			require.Equal(t, "stderr-line\n", logicalOutput)
		} else {
			require.Equal(t, "first-line\nsecond-line\nthird-line\n", logicalOutput)
		}
	}
	skip, limit := int64(1), int64(1)
	filtered, filteredErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Skip: &skip, Limit: &limit})
	require.NoError(t, filteredErr)
	require.Equal(t, "second-line\n", filtered)
	tail := int64(1)
	lastLine, tailErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Tail: &tail})
	require.NoError(t, tailErr)
	require.Equal(t, "third-line\n", lastLine)
	timestamped, timestampErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Timestamps: true})
	require.NoError(t, timestampErr)
	require.Contains(t, timestamped, "T")
	require.Contains(t, timestamped, "first-line\n")
	follow, followErr := openV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Follow: true})
	require.NoError(t, followErr)
	defer func() { require.NoError(t, follow.Close()) }()
	initial := make([]byte, len("first-line\nsecond-line\nthird-line\n"))
	_, initialErr := io.ReadFull(follow, initial)
	require.NoError(t, initialErr)
	testProcessExecutor.SimulateProcessExit(t, pid, 0)
	remaining, remainingErr := io.ReadAll(follow)
	require.NoError(t, remainingErr)
	require.Empty(t, remaining)
	exited := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.RuntimeLogs != nil && current.Status.RuntimeLogs.Complete, nil
	})
	require.Equal(t, apiv2.LogicalProcessPhaseExited, exited.Status.Phase)
	for _, source := range []string{"startup_stdout", "startup_stderr", "invalid"} {
		stream, streamErr := openV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Source: source})
		if stream != nil {
			require.NoError(t, stream.Close())
		}
		require.True(t, apierrors.IsBadRequest(streamErr), "%v", streamErr)
		stream, streamErr = openV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Source: source})
		if stream != nil {
			require.NoError(t, stream.Close())
		}
		require.True(t, apierrors.IsBadRequest(streamErr), "%v", streamErr)
	}
}

// Verifies that physical process launch diagnostics are routed into both physical and owning
// logical system streams, while failed launch output completes without hanging.
func TestV2ProcessSystemLogsFollowLogicalOwnership(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-process-system-owner")
	command := "v2-process-system-owner-command"
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{command}}
	testProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition:    criteria,
		StartupError: func(*internal_testutil.ProcessExecution) error { return errors.New("owned-process-log-failure") },
	})
	defer testProcessExecutor.RemoveAutoExecution(criteria)
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: command}}}
	require.NoError(t, client.Create(ctx, resource))
	failed := waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseFailed)
	physical := &apiv2.PhysicalProcess{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: failed.Status.PhysicalProcessRef}, physical))
	logicalSystem, logicalErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Source: "system"})
	require.NoError(t, logicalErr)
	physicalSystem, physicalErr := readV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Source: "system"})
	require.NoError(t, physicalErr)
	require.Contains(t, logicalSystem, "owned-process-log-failure")
	require.Contains(t, physicalSystem, "owned-process-log-failure")
	require.Contains(t, logicalSystem, "PhysicalProcessReconciler")
	output, outputErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Follow: true})
	require.NoError(t, outputErr)
	require.Empty(t, output)
}

// Verifies that referenced processes without attached capture expose completed empty output,
// without launching a second process or hiding their system log endpoint.
func TestV2ProcessLogsForExistingProcessWithoutCapture(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-process-log-existing")
	handle := seedAdoptablePhysicalProcess(t, ctx, "v2-process-log-existing-command")
	pid := int64(handle.Pid)
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModeExisting, PID: &pid,
			IdentityTimestamp: metav1.NewMicroTime(handle.IdentityTime)}}
	require.NoError(t, client.Create(ctx, resource))
	running := waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.True(t, running.Status.RuntimeLogs.Complete)
	require.False(t, running.Status.RuntimeLogs.Available)
	for _, source := range []string{"stdout", "stderr"} {
		output, outputErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Source: source, Follow: true})
		require.NoError(t, outputErr)
		require.Empty(t, output)
	}
	_, systemErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Source: "system"})
	require.NoError(t, systemErr)
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
}

// Verifies that logical and physical followers drain and finish on resource deletion,
// captured files are cleaned up, and a recreated logical name gets a distinct capture.
func TestV2ProcessLogsFinishOnDeletionAndIsolateRecreatedResources(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-process-log-deletion")
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-process-log-deletion-command"}}}
	require.NoError(t, client.Create(ctx, resource))
	running := waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	physical := &apiv2.PhysicalProcess{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalProcessRef}, physical))
	pid, pidErr := process.Int64_ToPidT(*running.Status.PID)
	require.NoError(t, pidErr)
	execution, found := testProcessExecutor.FindByPid(pid)
	require.True(t, found)
	_, writeErr := io.WriteString(execution.Cmd.Stdout, "before-deletion\n")
	require.NoError(t, writeErr)
	logicalFollow, logicalErr := openV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Follow: true})
	require.NoError(t, logicalErr)
	defer func() { require.NoError(t, logicalFollow.Close()) }()
	physicalFollow, physicalErr := openV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Follow: true})
	require.NoError(t, physicalErr)
	defer func() { require.NoError(t, physicalFollow.Close()) }()
	for _, stream := range []io.Reader{logicalFollow, physicalFollow} {
		output := make([]byte, len("before-deletion\n"))
		_, readErr := io.ReadFull(stream, output)
		require.NoError(t, readErr)
	}
	require.NoError(t, client.Delete(ctx, resource))
	for _, stream := range []io.Reader{logicalFollow, physicalFollow} {
		remaining, readErr := io.ReadAll(stream)
		require.NoError(t, readErr)
		require.Empty(t, remaining)
	}
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	require.NoFileExists(t, physical.Status.StdoutFile)
	require.NoFileExists(t, physical.Status.StderrFile)
	replacement := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: resource.Name, Namespace: namespace.Name}, Spec: *resource.Spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, replacement))
	recreated := waitLogicalProcessPhase(t, ctx, replacement.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.NotEqual(t, running.Status.RuntimeLogs.UID, recreated.Status.RuntimeLogs.UID)
	require.NotEqual(t, running.Status.RuntimeLogs.StdoutFile, recreated.Status.RuntimeLogs.StdoutFile)
	output, outputErr := readV2ContainerLog(ctx, replacement, apiv2.ResourceLogOptions{})
	require.NoError(t, outputErr)
	require.Empty(t, output)
}
