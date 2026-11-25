/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type adoptedProcessWatchExecutor struct {
	process.Executor
	failed   atomic.Bool
	started  chan struct{}
	canceled chan struct{}
}

func (executor *adoptedProcessWatchExecutor) WaitForProcessExit(ctx context.Context, handle process.ProcessHandle) error {
	if executor.failed.CompareAndSwap(false, true) {
		return errors.New("injected process watch inspection failure")
	}
	close(executor.started)
	waitErr := executor.Executor.WaitForProcessExit(ctx, handle)
	if ctx.Err() != nil {
		close(executor.canceled)
	}
	return waitErr
}

// Verifies that adopted-process watch errors report uncertainty rather than exit, observation
// restarts after inspection recovery, and deleting the resource cancels its watch without stopping the process.
func TestV2PhysicalProcessWatchRecoveryAndCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var observer *adoptedProcessWatchExecutor
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx, PhysicalProcessController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecoratePhysicalProcessExecutor: func(executor process.Executor) process.Executor {
			observer = &adoptedProcessWatchExecutor{Executor: executor, started: make(chan struct{}), canceled: make(chan struct{})}
			return observer
		}})
	require.NoError(t, startupErr)
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-process-watch-recovery")
	handle, startWait, startErr := environment.TestProcessExecutor.StartProcess(ctx, exec.Command("adopted-watch-target"), nil, process.CreationFlagsNone, nil)
	require.NoError(t, startErr)
	startWait()
	pid := int64(handle.Pid)
	resource := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.PhysicalProcessSpec{PID: &pid}}
	require.NoError(t, server.Client.Create(ctx, resource))
	unknown := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalProcessPhaseUnknown, nil
	})
	require.Contains(t, unknown.Status.Conditions[0].Message, "injected process watch inspection failure")
	require.Nil(t, unknown.Status.ExitCode)
	select {
	case <-observer.started:
	case <-ctx.Done():
		t.Fatal("adopted-process observation did not restart after inspection recovery")
	}
	waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalProcessPhaseRunning, nil
	})
	require.NoError(t, server.Client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
	select {
	case <-observer.canceled:
	case <-ctx.Done():
		t.Fatal("resource deletion did not cancel adopted-process observation")
	}
	require.NoError(t, environment.TestProcessExecutor.CheckProcessRunning(handle))
}
