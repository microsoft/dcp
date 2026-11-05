/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that running and completed logical/direct exec captures honor executable-log
// preservation after resource deletion and queued cleanup. Runs serially because the switch is process-wide.
func TestV2ContainerExecCapturesHonorPreservation(t *testing.T) {
	t.Setenv(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS, "1")
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	server, environment, startupErr := StartTestEnvironment(t, ctx, containerExecTestControllers, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	orchestrator, simulated := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, simulated)
	target := createV2ExecTarget(t, ctx, server.Client, "v2-exec-preserved-capture")
	var paths []string
	t.Cleanup(func() {
		for _, path := range paths {
			removeErr := os.Remove(path)
			require.True(t, removeErr == nil || errors.Is(removeErr, os.ErrNotExist), "%v", removeErr)
		}
	})
	for _, logical := range []bool{false, true} {
		for _, completed := range []bool{false, true} {
			name := "physical"
			if logical {
				name = "logical"
			}
			name += "-running"
			if completed {
				name += "-completed"
			}
			t.Run(name, func(t *testing.T) {
				physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: target.Namespace},
					Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: target.Status.PhysicalContainerRef,
						Execution: apiv2.ContainerExecConfig{Command: name}}}
				var owner *apiv2.LogicalContainerExec
				if logical {
					owner = &apiv2.LogicalContainerExec{ObjectMeta: physical.ObjectMeta,
						Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: physical.Spec.Execution}}
					require.NoError(t, server.Client.Create(ctx, owner))
					running := waitObjectAssumesStateEx(t, ctx, server.Client, owner.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
						return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
					})
					require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: target.Namespace, Name: running.Status.PhysicalExecRef}, physical))
				} else {
					require.NoError(t, server.Client.Create(ctx, physical))
				}
				running := waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
					return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
				})
				paths = append(paths, running.Status.StdoutFile, running.Status.StderrFile)
				for _, source := range []apiv1.LogStreamSource{apiv1.LogStreamSourceStdout, apiv1.LogStreamSourceStderr} {
					require.NoError(t, orchestrator.SimulateContainerExecCommandLogging(target.Status.ContainerID, name, source, []byte("preserved exec output\n")))
				}
				if completed {
					require.NoError(t, orchestrator.SimulateContainerExecExit(ctx, target.Status.ContainerID, name, 23))
					waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
						return current.Status.Phase == apiv2.ContainerExecPhaseExited, nil
					})
				}
				if owner != nil {
					require.NoError(t, server.Client.Delete(ctx, owner))
					ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, owner)
				} else {
					require.NoError(t, server.Client.Delete(ctx, physical))
				}
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
			})
		}
	}
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
		waitContainerExecSignal(t, ctx, started)
	}
	for _, path := range paths {
		output, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		require.Contains(t, string(output), "preserved exec output\n")
	}
}
