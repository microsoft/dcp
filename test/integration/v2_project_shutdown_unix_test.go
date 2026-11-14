//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"errors"
	"sync"
	"syscall"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that Project waits for adapter cleanup before stopping surviving targets, even when
// a suspended adapter cannot finish DAP shutdown and must outlive the protocol grace window.
func TestV2ProjectAdapterCleanupPrecedesTargets(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var tracked *projectTargetExecutor
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, ProjectController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateProjectProcessExecutor: func(executor process.Executor) process.Executor {
			tracked = &projectTargetExecutor{Executor: executor, stops: make(map[process.ProcessHandle]int)}
			return tracked
		}})
	require.NoError(t, startupErr)
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-shutdown-order")
	tool, toolErr := internaltest.GetTestToolPath("delay")
	require.NoError(t, toolErr)
	ownerExecutor := process.NewOSExecutor(logr.Discard())
	t.Cleanup(ownerExecutor.Dispose)
	target := startProjectTarget(t, ctx, ownerExecutor, tool)
	project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(),
			Process:      &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "adapter-started"}},
			DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{tool, "--dap-stdio"}}}}
	require.NoError(t, server.Client.Create(ctx, project))
	waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseReady)
	connection, connectionErr := connectProjectDebug(t, ctx, server.ClientConfig, project)
	require.NoError(t, connectionErr)
	defer connection.Close()
	reportProjectTarget(t, connection, target.Pid)
	running := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return current.Status.Debug != nil && len(current.Status.Debug.Executions) == 1 &&
			current.Status.Debug.Executions[0].PID != nil && len(current.Status.Debug.Sessions) == 1 &&
			len(current.Status.Debug.Sessions[0].Processes) == 1, nil
	})
	entry := running.Status.Debug.Executions[0]
	pid, pidErr := process.Int64_ToPidT(*entry.PID)
	require.NoError(t, pidErr)
	adapter := process.NewHandle(pid, entry.IdentityTimestamp.Time)
	tracked.adapter.Store(&adapter)
	proc, procErr := adapter.OsProcess()
	require.NoError(t, procErr)
	require.NoError(t, proc.Signal(syscall.SIGSTOP))
	require.NoError(t, proc.Release())
	resume := sync.OnceFunc(func() {
		if process.IsProcessGoneErr(ownerExecutor.CheckProcessRunning(adapter)) {
			return
		}
		resumable, resumeErr := adapter.OsProcess()
		if process.IsProcessGoneErr(resumeErr) {
			return
		}
		require.NoError(t, resumeErr)
		if resumeErr == nil {
			signalErr := resumable.Signal(syscall.SIGCONT)
			require.True(t, signalErr == nil || errors.Is(signalErr, syscall.ESRCH), "could not resume adapter: %v", signalErr)
			require.NoError(t, resumable.Release())
		}
	})
	t.Cleanup(resume)
	require.NoError(t, server.Client.Patch(ctx, project, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	stopped := waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseStopped)
	require.False(t, tracked.orderFailed.Load(), "target cleanup started before adapter termination settled")
	require.True(t, process.IsProcessGoneErr(ownerExecutor.CheckProcessRunning(adapter)))
	require.True(t, process.IsProcessGoneErr(ownerExecutor.CheckProcessRunning(target)))
	require.Equal(t, apiv2.ProjectDebugProcessCleanupComplete, stopped.Status.Debug.Sessions[0].Processes[0].Observation.Cleanup)
	require.NoError(t, server.Client.Delete(ctx, project))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, project)
}
