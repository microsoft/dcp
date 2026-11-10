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
	"sync"
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
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/internal/statestore"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that output removal failure does not block LogicalProcess, PhysicalProcess,
// or namespace deletion after the process has stopped.
func TestV2ProcessCleanupDoesNotBlockOnOutputRemovalFailure(t *testing.T) {
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
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, logical)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: physicalName.Name, Namespace: physicalName.Namespace},
	})
	require.FileExists(t, obstruction)
	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
}

// Verifies that retained raw process capture preserves application numbers, timestamps, and
// a final fragment through LogicalProcess and PhysicalProcess endpoints while retaining line selection behavior.
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

// Verifies that LogicalProcess and PhysicalProcess preserve incomplete output after root exit
// until descendant-held pipes drain, including for newly opened followers of terminal resources.
func TestV2ProcessLogFollowersWaitForOutputDrain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix root-only termination to leave a descendant writing to the inherited pipe")
	}
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	executor := process.NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, LogicalProcessController|PhysicalProcessController, t.Name(), t.TempDir(),
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
	logical := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{
			ExecutablePath: tool, Args: []string{"--child-spec", "1", "--hold-pipe", "--report-ready"},
		}}}
	require.NoError(t, server.Client.Create(ctx, logical))
	logicalRunning := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseRunning, nil
	})
	physical := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: logicalRunning.Status.PhysicalProcessRef, Namespace: namespace.Name}}
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
	logicalExited := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseExited, nil
	})
	require.Equal(t, exited.GetLogSource(), logicalExited.Status.RuntimeLogs)
	require.False(t, logicalExited.Status.RuntimeLogs.Complete)
	var followers []io.ReadCloser
	for _, resource := range []commonapi.DcpModelObject{logical, physical} {
		gvr := resource.GetGroupVersionResource()
		stream, streamErr := server.RestClient.Get().AbsPath("/apis", gvr.Group, gvr.Version,
			"namespaces", namespace.Name, gvr.Resource, resource.GetName(), apiv2.LogSubresourceName).Param("follow", "true").Stream(ctx)
		require.NoError(t, streamErr)
		defer func() { require.NoError(t, stream.Close()) }()
		followers = append(followers, stream)
	}
	require.NoError(t, executor.StopProcess(ctx, tree[1], process.StopRootOnly()))
	for _, follower := range followers {
		output, readErr := io.ReadAll(follower)
		require.NoError(t, readErr)
		require.Contains(t, string(output), "Received request to exit\n")
	}
	waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		return current.Status.LogsComplete, nil
	})
	waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.RuntimeLogs != nil && current.Status.RuntimeLogs.Complete, nil
	})
	require.NoError(t, server.Client.Delete(ctx, logical))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
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

// Verifies that LogicalProcess log followers finish when the backing PhysicalProcess disappears,
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
			// No PhysicalProcess log request is made: its lazily started log lifecycle watch must not be required.
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
			forceRemoveLogicalProcessBacking(t, ctx, apiClient, physical)
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
					ready != nil && ready.Reason == string(apiv2.LogicalProcessReasonMissing), nil
			})
			require.NoError(t, apiClient.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, resource)
		})
	}
}

// Verifies that LogicalProcess preserves a captured exit outcome after backing deletion while
// publishing unavailable logs, including when a real API conflict rejects the first status update.
func TestV2ProcessTerminalOutcomeSurvivesBackingDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	logicalName := types.NamespacedName{Namespace: "v2-terminal-process-backing-loss", Name: "process"}
	var conflictReader *processBackingDeletionConflictReader
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalProcessController|PhysicalProcessController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIReader: func(reader ctrl_client.Reader) ctrl_client.Reader {
			conflictReader = &processBackingDeletionConflictReader{Reader: reader, logicalName: logicalName}
			return conflictReader
		}})
	require.NoError(t, startupErr)
	conflictReader.client = server.Client
	createV2ExecNamespace(t, ctx, server.Client, logicalName.Namespace)
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: logicalName.Name, Namespace: logicalName.Namespace},
		Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: logicalName.Namespace}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, logicalName, func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseRunning, nil
	})
	pid, pidErr := process.Int64_ToPidT(*running.Status.PID)
	require.NoError(t, pidErr)
	environment.TestProcessExecutor.SimulateProcessExit(t, pid, 17)
	exited := waitObjectAssumesStateEx(t, ctx, server.Client, logicalName, func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseExited, nil
	})
	require.NotNil(t, exited.Status.ExitCode)
	require.Equal(t, int32(17), *exited.Status.ExitCode)
	require.False(t, exited.Status.FinishedAt.IsZero())
	require.NotNil(t, exited.Status.RuntimeLogs)
	require.True(t, exited.Status.RuntimeLogs.Available)
	physical := &apiv2.PhysicalProcess{}
	require.NoError(t, server.Client.Get(ctx,
		types.NamespacedName{Name: exited.Status.PhysicalProcessRef, Namespace: logicalName.Namespace}, physical))
	conflictReader.armed.Store(true)
	forceRemoveLogicalProcessBacking(t, ctx, server.Client, physical)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
	completed := waitObjectAssumesStateEx(t, ctx, server.Client, logicalName, func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.RuntimeLogs != nil && current.Status.RuntimeLogs.Complete && !current.Status.RuntimeLogs.Available, nil
	})
	require.True(t, conflictReader.injected.Load())
	require.Equal(t, exited.Status.Phase, completed.Status.Phase)
	require.Equal(t, exited.Status.Conditions, completed.Status.Conditions)
	require.Equal(t, exited.Status.PID, completed.Status.PID)
	require.Equal(t, exited.Status.IdentityTimestamp, completed.Status.IdentityTimestamp)
	require.Equal(t, exited.Status.ExitCode, completed.Status.ExitCode)
	require.Equal(t, exited.Status.FinishedAt, completed.Status.FinishedAt)
	require.Equal(t, exited.Status.PhysicalProcessRef, completed.Status.PhysicalProcessRef)
	require.Equal(t, physical.Name, completed.Status.RuntimeLogs.Name)
	require.Equal(t, physical.UID, completed.Status.RuntimeLogs.UID)
	require.NoError(t, server.Client.Patch(ctx, resource,
		ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	unchanged := waitObjectAssumesStateEx(t, ctx, server.Client, logicalName, func(current *apiv2.LogicalProcess) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Spec.Stop && ready != nil && ready.ObservedGeneration == current.Generation, nil
	})
	require.Equal(t, apiv2.LogicalProcessPhaseExited, unchanged.Status.Phase)
	requireReadyCondition(t, unchanged.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalProcessReasonCompleted)
	require.Equal(t, completed.Status.RuntimeLogs, unchanged.Status.RuntimeLogs)
}

// Verifies that LogicalProcess and PhysicalProcess endpoints share captured stdout/stderr,
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

// Verifies that namespace deletion completes and LogicalProcess log followers finish
// even when output capture removal fails.
func TestV2ProcessLogicalOnlyFollowerCompletesDuringNamespaceDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, environment, startupErr := StartTestEnvironment(t, ctx,
		LogicalProcessController|PhysicalProcessController, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-only-deletion-follow"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	logical := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-logical-only-deletion-command"}}}
	require.NoError(t, server.Client.Create(ctx, logical))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseRunning, nil
	})
	pid, pidErr := process.Int64_ToPidT(*running.Status.PID)
	require.NoError(t, pidErr)
	execution, found := environment.TestProcessExecutor.FindByPid(pid)
	require.True(t, found)
	const payload = "logical-only-before-deletion\n"
	_, writeErr := io.WriteString(execution.Cmd.Stdout, payload)
	require.NoError(t, writeErr)
	stream, streamErr := server.RestClient.Get().
		AbsPath("/apis", apiv2.GroupVersion.Group, apiv2.GroupVersion.Version,
			"namespaces", namespace.Name, "logicalprocesses", logical.Name, apiv2.LogSubresourceName).
		Param("source", "stdout").Param("follow", "true").Stream(ctx)
	require.NoError(t, streamErr)
	defer func() { require.NoError(t, stream.Close()) }()
	initial := make([]byte, len(payload))
	_, readErr := io.ReadFull(stream, initial)
	require.NoError(t, readErr)
	require.Equal(t, payload, string(initial))
	require.False(t, running.Status.RuntimeLogs.Complete)
	t.Cleanup(func() {
		_ = logs.RemoveOutputFiles(context.WithoutCancel(ctx),
			running.Status.RuntimeLogs.StdoutFile, running.Status.RuntimeLogs.StderrFile)
	})
	if runtime.GOOS != "windows" {
		// Model Windows' open-reader removal failure without opening the PhysicalProcess log endpoint.
		path := running.Status.RuntimeLogs.StdoutFile
		saved := path + ".saved"
		obstruction := filepath.Join(path, "busy")
		require.NoError(t, os.Rename(path, saved))
		require.NoError(t, os.Mkdir(path, 0700))
		require.NoError(t, usvc_io.WriteFile(obstruction, []byte("busy"), osutil.PermissionOnlyOwnerReadWrite))
		t.Cleanup(func() {
			_ = os.Remove(obstruction)
			_ = os.Remove(path)
			_ = os.Remove(saved)
		})
	}
	require.NoError(t, server.Client.Delete(ctx, namespace))
	physicalName := types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalProcessRef}
	remaining, remainingErr := io.ReadAll(stream)
	require.NoError(t, remainingErr)
	require.Empty(t, remaining)
	require.NoError(t, stream.Close())
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: physicalName.Name, Namespace: physicalName.Namespace},
	})
}

// Verifies that PhysicalProcess launch diagnostics are routed into both its own and the owning
// LogicalProcess system streams, while failed launch output completes without hanging.
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

// Verifies that an identity-bound PhysicalProcess reads a supplied stdout file independent of
// resource naming, keeps it while detached, and completes deletion after exit even if output removal fails.
func TestV2PhysicalProcessReusesExistingOutputCapture(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-physical-process-existing-output")
	handle := seedAdoptablePhysicalProcess(t, ctx, "v2-physical-process-existing-output-command")
	pid := int64(handle.Pid)
	outputPath := filepath.Join(t.TempDir(), "unrelated-original-output.log")
	const output = "123 items\noriginal capture"
	require.NoError(t, usvc_io.WriteFile(outputPath, []byte(output), osutil.PermissionOnlyOwnerReadWrite))
	spec := apiv2.PhysicalProcessSpec{PID: &pid, IdentityTimestamp: metav1.NewMicroTime(handle.IdentityTime), StdoutFile: outputPath}
	first := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: namespace.Name}, Spec: *spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, first))
	waitPhysicalProcessPhase(t, ctx, first.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)
	readOutput, readErr := readV2ContainerLog(ctx, first, apiv2.ResourceLogOptions{})
	require.NoError(t, readErr)
	require.Equal(t, output, readOutput)
	noStderr, stderrErr := readV2ContainerLog(ctx, first, apiv2.ResourceLogOptions{Source: "stderr", Follow: true})
	require.NoError(t, stderrErr)
	require.Empty(t, noStderr)
	require.NoError(t, client.Delete(ctx, first))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, first)
	require.FileExists(t, outputPath)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
	second := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: namespace.Name}, Spec: *spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, second))
	waitPhysicalProcessPhase(t, ctx, second.NamespacedName(), apiv2.PhysicalProcessPhaseRunning)
	require.NoError(t, retryOnConflict(ctx, second.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalProcess) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	}))
	exited := waitPhysicalProcessPhase(t, ctx, second.NamespacedName(), apiv2.PhysicalProcessPhaseExited)
	require.True(t, exited.Status.LogsComplete)
	finalOutput, finalErr := readV2ContainerLog(ctx, second, apiv2.ResourceLogOptions{Follow: true})
	require.NoError(t, finalErr)
	require.Equal(t, output, finalOutput)
	require.NoError(t, os.Rename(outputPath, outputPath+".saved"))
	require.NoError(t, os.Mkdir(outputPath, 0700))
	obstruction := filepath.Join(outputPath, "busy")
	require.NoError(t, usvc_io.WriteFile(obstruction, []byte("busy"), osutil.PermissionOnlyOwnerReadWrite))
	require.NoError(t, client.Delete(ctx, second))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, second)
	require.FileExists(t, obstruction)
}

// Verifies that LogicalProcess and PhysicalProcess log followers drain and finish on resource deletion,
// and a recreated logical name gets a distinct capture without old output.
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
	// Clean up any captures left by best-effort deletion after both followers have closed.
	t.Cleanup(func() {
		cleanupErr := logs.RemoveOutputFiles(context.WithoutCancel(ctx), physical.Status.StdoutFile, physical.Status.StderrFile)
		if cleanupErr != nil {
			t.Logf("Could not remove test output files: %v", cleanupErr)
		}
	})
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
	replacement := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: resource.Name, Namespace: namespace.Name}, Spec: *resource.Spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, replacement))
	recreated := waitLogicalProcessPhase(t, ctx, replacement.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.NotEqual(t, running.Status.RuntimeLogs.UID, recreated.Status.RuntimeLogs.UID)
	require.NotEqual(t, running.Status.RuntimeLogs.StdoutFile, recreated.Status.RuntimeLogs.StdoutFile)
	output, outputErr := readV2ContainerLog(ctx, replacement, apiv2.ResourceLogOptions{})
	require.NoError(t, outputErr)
	require.Empty(t, output)
}

// Verifies that created session and exited persistent process captures honor executable-log
// preservation while their LogicalProcess and PhysicalProcess resources complete normal deletion.
// Runs serially because log preservation is controlled by a process-wide environment variable.
func TestV2ProcessCreatedCapturesHonorPreservation(t *testing.T) {
	t.Setenv(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS, "1")
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	for _, mode := range []apiv2.LogicalProcessMode{apiv2.LogicalProcessModeSession, apiv2.LogicalProcessModePersistent} {
		t.Run(string(mode), func(t *testing.T) {
			namespace := createActiveV2Namespace(t, ctx, "v2-process-preserved-capture-"+string(mode))
			resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
				Spec: apiv2.LogicalProcessSpec{Mode: mode,
					Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-process-preserved-capture-" + string(mode)}}}
			require.NoError(t, client.Create(ctx, resource))
			running := waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
			paths := []string{running.Status.RuntimeLogs.StdoutFile, running.Status.RuntimeLogs.StderrFile}
			t.Cleanup(func() {
				for _, path := range paths {
					_ = os.Remove(path)
				}
			})
			for _, path := range paths {
				file, openErr := usvc_io.OpenOrCreateFileForAppending(path, osutil.PermissionOnlyOwnerReadWrite)
				require.NoError(t, openErr)
				_, writeErr := file.WriteString("preserved process output\n")
				require.NoError(t, writeErr)
				require.NoError(t, file.Close())
			}
			require.NoError(t, retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalProcess) error {
				current.Spec.Stop = true
				return client.Update(updateCtx, current)
			}))
			waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseExited)
			require.NoError(t, client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
			for _, path := range paths {
				output, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				require.Contains(t, string(output), "preserved process output\n")
			}
		})
	}
}

// Verifies that a blocked shared output cleanup queue does not block process/image deletion or
// stale persistent-process replacement, and queued files are removed after the workers are released.
func TestV2OutputCleanupDoesNotBlockReconciliation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, environment, startupErr := StartTestEnvironment(t, ctx,
		LogicalProcessController|PhysicalProcessController|PhysicalContainerImageController, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-queued-output-cleanup"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})

	started := make(chan struct{}, controllers.MaxConcurrentReconciles)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	for range controllers.MaxConcurrentReconciles {
		require.NoError(t, environment.OutputCleanupQueue.Enqueue(func(cleanupCtx context.Context) {
			started <- struct{}{}
			select {
			case <-release:
			case <-cleanupCtx.Done():
			}
		}))
	}
	for range controllers.MaxConcurrentReconciles {
		select {
		case _, received := <-started:
			require.True(t, received)
		case <-ctx.Done():
			t.Fatal("cleanup workers did not start")
		}
	}

	session := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-queued-cleanup-session"}}}
	require.NoError(t, server.Client.Create(ctx, session))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, session.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseRunning, nil
	})
	paths := []string{running.Status.RuntimeLogs.StdoutFile, running.Status.RuntimeLogs.StderrFile}
	require.NoError(t, server.Client.Delete(ctx, session))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, session)

	image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: "v2-queued-cleanup-image"}}}
	require.NoError(t, server.Client.Create(ctx, image))
	readyImage := waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
	})
	paths = append(paths, readyImage.Status.StdoutFile, readyImage.Status.StderrFile)
	require.NoError(t, server.Client.Delete(ctx, image))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, image)

	persistent := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "persistent", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
			Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-queued-cleanup-persistent"}}}
	staleHandle, _, startErr := environment.TestProcessExecutor.StartProcess(ctx, exec.Command("v2-queued-cleanup-stale"),
		nil, process.CreationFlagsNone, nil)
	require.NoError(t, startErr)
	require.NoError(t, environment.TestProcessExecutor.StopProcess(ctx, staleHandle))
	staleOutput := filepath.Join(t.TempDir(), "stale-output")
	require.NoError(t, usvc_io.WriteFile(staleOutput, []byte("retired output"), osutil.PermissionOnlyOwnerReadWrite))
	require.NoError(t, environment.StateStore.UpsertPersistentProcess(ctx, statestore.PersistentProcessRecord{
		ResourceKey: persistent.GetLeaseKey(), PID: staleHandle.Pid, IdentityTime: staleHandle.IdentityTime,
		RunID: "stale", StdOutFile: staleOutput,
	}))
	require.NoError(t, server.Client.Create(ctx, persistent))
	replacement := waitObjectAssumesStateEx(t, ctx, server.Client, persistent.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseRunning, nil
	})
	require.NotEqual(t, int64(staleHandle.Pid), *replacement.Status.PID)
	paths = append(paths, staleOutput, replacement.Status.RuntimeLogs.StdoutFile, replacement.Status.RuntimeLogs.StderrFile)
	require.NoError(t, server.Client.Patch(ctx, persistent,
		ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	waitObjectAssumesStateEx(t, ctx, server.Client, persistent.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseExited, nil
	})
	require.NoError(t, server.Client.Delete(ctx, persistent))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, persistent)
	require.NoError(t, server.Client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
	for _, path := range paths {
		require.FileExists(t, path)
	}
	unblock()
	waitOutputFilesRemoved(t, ctx, paths...)
}

func waitOutputFilesRemoved(t *testing.T, ctx context.Context, paths ...string) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		for _, path := range paths {
			_, statErr := os.Stat(path)
			if statErr == nil {
				return false, nil
			}
			if !os.IsNotExist(statErr) {
				return false, statErr
			}
		}
		return true, nil
	}))
}
