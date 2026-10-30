/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that physical process cleanup retains its finalizer and capture ownership after
// a removal failure, then normal reconciliation completes deletion when removal becomes possible.
func TestV2ProcessCleanupRetriesOutputRemoval(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-process-output-cleanup")
	logical := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-process-output-cleanup-command"}}}
	require.NoError(t, client.Create(ctx, logical))
	running := waitLogicalProcessPhase(t, ctx, logical.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.NoError(t, retryOnConflict(ctx, logical.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalProcess) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	}))
	completed := waitObjectAssumesState(t, ctx, logical.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.RuntimeLogs != nil && current.Status.RuntimeLogs.Complete, nil
	})
	capturePath := completed.Status.RuntimeLogs.StdoutFile
	savedPath := capturePath + ".saved"
	obstruction := filepath.Join(capturePath, "busy")
	t.Cleanup(func() {
		_ = os.Remove(obstruction)
		_ = os.Remove(capturePath)
		_ = os.Remove(savedPath)
	})
	require.NoError(t, os.Rename(capturePath, savedPath))
	require.NoError(t, os.Mkdir(capturePath, 0700))
	require.NoError(t, usvc_io.WriteFile(obstruction, []byte("busy"), osutil.PermissionOnlyOwnerReadWrite))
	require.NoError(t, client.Delete(ctx, logical))
	physicalName := types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalProcessRef}
	failed := waitObjectAssumesState(t, ctx, physicalName, func(current *apiv2.PhysicalProcess) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && strings.Contains(ready.Message, "Failed to release process output capture"), nil
	})
	require.NotEmpty(t, failed.Finalizers)
	require.False(t, failed.DeletionTimestamp.IsZero())
	// Let the controller remove the emptied directory; never recreate the path after disposal can succeed.
	require.NoError(t, os.Remove(obstruction))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, logical)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, failed)
	require.NoFileExists(t, capturePath)
	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
}

// Verifies that retained raw process capture preserves application numbers, timestamps, and
// a final fragment through logical/physical endpoints while retaining line selection behavior.
func TestV2ProcessRetainedLogsPreserveRawOutput(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-process-raw-output")
	logical := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
			Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-process-raw-output-command"}}}
	require.NoError(t, client.Create(ctx, logical))
	defer func() {
		require.NoError(t, retryOnConflict(ctx, logical.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalProcess) error {
			current.Spec.Stop = true
			return client.Update(updateCtx, current)
		}))
		waitLogicalProcessPhase(t, ctx, logical.NamespacedName(), apiv2.LogicalProcessPhaseExited)
		require.NoError(t, client.Delete(ctx, logical))
		ctrl_testutil.WaitObjectDeleted(t, ctx, client, logical)
	}()
	running := waitLogicalProcessPhase(t, ctx, logical.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.True(t, running.Status.RuntimeLogs.Raw)
	physical := &apiv2.PhysicalProcess{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalProcessRef}, physical))
	require.True(t, physical.GetLogSource().Raw)
	const payload = "123 items\n2026-10-22T12:00:00Z message\nhello"
	for _, filePath := range []string{running.Status.RuntimeLogs.StdoutFile, running.Status.RuntimeLogs.StderrFile} {
		output, openErr := usvc_io.OpenOrCreateFileForAppending(filePath, osutil.PermissionOnlyOwnerReadWrite)
		require.NoError(t, openErr)
		_, writeErr := io.WriteString(output, payload)
		require.NoError(t, writeErr)
		require.NoError(t, output.Close())
	}
	for _, source := range []string{"stdout", "stderr"} {
		logicalOutput, logicalErr := readV2ContainerLog(ctx, logical, apiv2.ResourceLogOptions{Source: source})
		require.NoError(t, logicalErr)
		require.Equal(t, payload, logicalOutput)
		physicalOutput, physicalErr := readV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Source: source})
		require.NoError(t, physicalErr)
		require.Equal(t, payload, physicalOutput)
	}
	skip, limit := int64(1), int64(1)
	selected, selectedErr := readV2ContainerLog(ctx, logical, apiv2.ResourceLogOptions{Skip: &skip, Limit: &limit})
	require.NoError(t, selectedErr)
	require.Equal(t, "2026-10-22T12:00:00Z message\n", selected)
	preserved, preservedErr := readV2ContainerLog(ctx, logical, apiv2.ResourceLogOptions{Timestamps: true, LineNumbers: true})
	require.NoError(t, preservedErr)
	require.Equal(t, payload, preserved)
}

type processOutputDrainExecutor struct {
	process.Executor
	testContext context.Context
}

func (executor *processOutputDrainExecutor) StartProcess(
	ctx context.Context, command *exec.Cmd, handler process.ProcessExitHandler,
	flags process.ProcessCreationFlag, create process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	// Keep the drain open until the test releases the child instead of racing API requests against WaitDelay.
	if deadline, bounded := executor.testContext.Deadline(); bounded && command.WaitDelay != 0 {
		command.WaitDelay = time.Until(deadline)
	}
	return executor.Executor.StartProcess(ctx, command, handler, flags, create)
}

// Verifies that physical process output completes after descendant-held pipes drain,
// rather than when the root process disappears, including for a newly opened follower.
func TestV2PhysicalProcessLogsWaitForOutputDrain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix root-only termination to leave a descendant writing to the inherited pipe")
	}
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	executor := process.NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, PhysicalProcessController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecoratePhysicalProcessExecutor: func(process.Executor) process.Executor {
			return &processOutputDrainExecutor{Executor: executor, testContext: ctx}
		}})
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-process-output-drain"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	tool, toolErr := internal_testutil.GetTestToolPath("delay")
	require.NoError(t, toolErr)
	physical := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{Process: &apiv2.PhysicalProcessConfig{
			ExecutablePath: tool, Args: []string{"--child-spec", "1", "--hold-pipe", "--report-ready"},
		}}}
	require.NoError(t, server.Client.Create(ctx, physical))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalProcessPhaseRunning, nil
	})
	pid, pidErr := process.Int64_ToPidT(*running.Status.PID)
	require.NoError(t, pidErr)
	handle := process.NewHandle(pid, running.Status.IdentityTimestamp.Time)
	var tree []process.ProcessHandle
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, pollImmediately, func(treeCtx context.Context) (bool, error) {
		var treeErr error
		tree, treeErr = process.GetProcessTree(treeCtx, handle)
		return len(tree) > 1, treeErr
	}))
	defer func() {
		for _, child := range tree {
			stopErr := executor.StopProcess(context.WithoutCancel(ctx), child)
			require.True(t, stopErr == nil || process.IsProcessGoneErr(stopErr), "%v", stopErr)
		}
	}()
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, pollImmediately, func(context.Context) (bool, error) {
		captured, captureErr := os.ReadFile(running.Status.StdoutFile)
		return strings.Contains(string(captured), fmt.Sprintf("Ready: %d\n", tree[1].Pid)), captureErr
	}))
	root, rootErr := handle.OsProcess()
	require.NoError(t, rootErr)
	require.NoError(t, root.Kill())
	require.NoError(t, root.Release())
	require.NoError(t, retryOnConflictEx(ctx, server.Client, physical.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalProcess) error {
		current.Annotations = map[string]string{"test.dcp.microsoft.com/reconcile": "observe-root-exit"}
		return server.Client.Update(updateCtx, current)
	}))
	exited := waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalProcessPhaseExited, nil
	})
	require.False(t, exited.Status.LogsComplete, "root disappearance must not finish the capture before pipe drain")
	stream, streamErr := server.RestClient.Get().AbsPath("/apis", apiv2.GroupVersion.Group, apiv2.GroupVersion.Version,
		"namespaces", namespace.Name, "physicalprocesses", physical.Name, apiv2.LogSubresourceName).Param("follow", "true").Stream(ctx)
	require.NoError(t, streamErr)
	defer func() { require.NoError(t, stream.Close()) }()
	require.NoError(t, executor.StopProcess(ctx, tree[1], process.StopRootOnly()))
	output, readErr := io.ReadAll(stream)
	require.NoError(t, readErr)
	require.Contains(t, string(output), "Received request to exit\n")
	waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		return current.Status.LogsComplete, nil
	})
	require.NoError(t, server.Client.Delete(ctx, physical))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
}

type processBackingDeletionConflictReader struct {
	ctrl_client.Reader
	client      ctrl_client.Client
	logicalName types.NamespacedName
	armed       atomic.Bool
	injected    atomic.Bool
}

func (reader *processBackingDeletionConflictReader) Get(
	ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption,
) error {
	getErr := reader.Reader.Get(ctx, key, obj, options...)
	if _, physical := obj.(*apiv2.PhysicalProcess); !physical || !apierrors.IsNotFound(getErr) ||
		key.Namespace != reader.logicalName.Namespace || !reader.armed.CompareAndSwap(true, false) {
		return getErr
	}
	// Change the real API resource version after reconciliation has read it but before its status write.
	updateErr := retryOnConflictEx(ctx, reader.client, reader.logicalName, func(updateCtx context.Context, current *apiv2.LogicalProcess) error {
		patch := ctrl_client.MergeFrom(current.DeepCopy())
		if current.Annotations == nil {
			current.Annotations = make(map[string]string)
		}
		current.Annotations["test.dcp.microsoft.com/backing-deletion-conflict"] = "injected"
		return reader.client.Patch(updateCtx, current, patch)
	})
	if updateErr != nil {
		return updateErr
	}
	reader.injected.Store(true)
	return getErr
}

// Verifies that logical-only process log followers finish when physical backing disappears,
// and completion remains projected when a real API resource-version conflict rejects the first status write.
func TestV2ProcessLogicalOnlyFollowerCompletesAfterBackingDeletion(t *testing.T) {
	t.Parallel()
	for _, withConflict := range []bool{false, true} {
		name := "normal"
		if withConflict {
			name = "status-conflict"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			apiClient, logsClient, executor := client, restClient, testProcessExecutor
			namespaceName := "v2-process-logical-only-" + name
			var conflictReader *processBackingDeletionConflictReader
			if withConflict {
				serverInfo, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
					LogicalProcessController|PhysicalProcessController, t.Name(), t.TempDir(),
					TestEnvironmentOptions{DecorateAPIReader: func(reader ctrl_client.Reader) ctrl_client.Reader {
						conflictReader = &processBackingDeletionConflictReader{Reader: reader,
							logicalName: types.NamespacedName{Namespace: namespaceName, Name: "process"}}
						return conflictReader
					}})
				require.NoError(t, startupErr)
				apiClient, logsClient, executor = serverInfo.Client, serverInfo.RestClient, environment.TestProcessExecutor
				conflictReader.client = apiClient
			}
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}
			require.NoError(t, apiClient.Create(ctx, namespace))
			waitObjectAssumesStateEx(t, ctx, apiClient, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
				return current.Status.Phase == apiv2.NamespacePhaseActive, nil
			})
			resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespaceName},
				Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-logical-only-follow-" + name}}}
			require.NoError(t, apiClient.Create(ctx, resource))
			running := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
				return current.Status.Phase == apiv2.LogicalProcessPhaseRunning &&
					current.Status.RuntimeLogs != nil && current.Status.RuntimeLogs.Available, nil
			})
			physical := &apiv2.PhysicalProcess{}
			require.NoError(t, apiClient.Get(ctx, types.NamespacedName{Namespace: namespaceName, Name: running.Status.PhysicalProcessRef}, physical))
			pid, pidErr := process.Int64_ToPidT(*running.Status.PID)
			require.NoError(t, pidErr)
			execution, found := executor.FindByPid(pid)
			require.True(t, found)
			_, writeErr := io.WriteString(execution.Cmd.Stdout, "logical-only-output\n")
			require.NoError(t, writeErr)
			// No physical log request is made: its lazily started log lifecycle watch must not be required.
			stream, streamErr := logsClient.Get().
				AbsPath("/apis", apiv2.GroupVersion.Group, apiv2.GroupVersion.Version, "namespaces",
					namespaceName, "logicalprocesses", resource.Name, apiv2.LogSubresourceName).
				Param("source", "stdout").Param("follow", "true").Stream(ctx)
			require.NoError(t, streamErr)
			defer func() { require.NoError(t, stream.Close()) }()
			firstLine := make([]byte, len("logical-only-output\n"))
			_, readErr := io.ReadFull(stream, firstLine)
			require.NoError(t, readErr)
			require.Equal(t, "logical-only-output\n", string(firstLine))
			if conflictReader != nil {
				conflictReader.armed.Store(true)
			}
			require.NoError(t, apiClient.Delete(ctx, physical))
			ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
			completed := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
				return current.Status.RuntimeLogs != nil && current.Status.RuntimeLogs.Complete &&
					!current.Status.RuntimeLogs.Available, nil
			})
			require.Equal(t, physical.UID, completed.Status.RuntimeLogs.UID)
			require.Equal(t, physical.Name, completed.Status.RuntimeLogs.Name)
			if conflictReader != nil {
				require.True(t, conflictReader.injected.Load())
			}
			remaining, remainingErr := io.ReadAll(stream)
			require.NoError(t, remainingErr)
			require.Empty(t, remaining)
			waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return current.Status.Phase == apiv2.LogicalProcessPhaseFailed &&
					ready != nil && ready.Reason == "PhysicalProcessMissing", nil
			})
			require.NoError(t, apiClient.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, resource)
		})
	}
}

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
