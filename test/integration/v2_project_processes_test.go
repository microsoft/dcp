/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

func startProjectTarget(t *testing.T, ctx context.Context, executor process.Executor, tool string, arguments ...string) process.ProcessHandle {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(tool, append([]string{"--ready-file", ready}, arguments...)...)
	handle, enableWait, startErr := executor.StartProcess(ctx, cmd, nil, process.CreationFlagEnsureKillOnDispose, nil)
	require.NoError(t, startErr)
	enableWait()
	t.Cleanup(func() {
		cleanupCtx, cancel := process.WithDetachedStopTimeout(ctx)
		defer cancel()
		stopErr := executor.StopProcess(cleanupCtx, handle)
		require.True(t, stopErr == nil || process.IsProcessGoneErr(stopErr), "%v", stopErr)
	})
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		_, statErr := os.Stat(ready)
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return statErr == nil, statErr
	}))
	return handle
}

func reportProjectTarget(t *testing.T, connection *websocket.Conn, pid process.Pid_t) {
	t.Helper()
	body := fmt.Sprintf(`{"seq":12,"type":"request","command":"reportProcess","arguments":{"name":"target","systemProcessId":%d}}`, pid)
	require.NoError(t, connection.WriteMessage(websocket.BinaryMessage, []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body))))
	event := readProjectDebugMessage(t, connection)
	require.JSONEq(t, `"process"`, string(event["event"]))
	response := readProjectDebugMessage(t, connection)
	require.JSONEq(t, `"response"`, string(response["type"]))
}

func releaseProjectInspection(t *testing.T, ctx context.Context, client ctrl_client.Client, project *apiv2.ProjectProcess) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if getErr := client.Get(ctx, project.NamespacedName(), project); getErr != nil {
			return ctrl_client.IgnoreNotFound(getErr)
		}
		project.Finalizers = slices.DeleteFunc(project.Finalizers, func(finalizer string) bool { return finalizer == "test/inspect-target-cleanup" })
		return client.Update(ctx, project)
	}))
}

// Verifies that ProjectProcess retains informational target observations after session closure and preserves
// reported targets through stop, ProjectProcess deletion and namespace deletion, regardless of launch or attach intent.
func TestV2ProjectProcessObservesTargetsWithoutCleanupOwnership(t *testing.T) {
	t.Parallel()
	for _, ending := range []string{"stop", "delete", "namespace"} {
		t.Run(ending, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createV2ExecNamespace(t, ctx, client, "v2-project-targets-"+ending)
			tool, toolErr := internaltest.GetTestToolPath("delay")
			require.NoError(t, toolErr)
			owner := process.NewOSExecutor(logr.Discard())
			t.Cleanup(owner.Dispose)
			methods := []string{"", "launch", "attach", "attachForSuspendedLaunch"}
			handles := make([]process.ProcessHandle, len(methods))
			for index := range handles {
				handles[index] = startProjectTarget(t, ctx, owner, tool)
			}
			project := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"}},
					DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{tool, "--dap-stdio"}},
				}}
			require.NoError(t, client.Create(ctx, project))
			waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)
			connection, connectErr := connectProjectDebug(t, ctx, clientConfig, project)
			require.NoError(t, connectErr)
			defer connection.Close()
			for index, method := range methods {
				sendProjectProtocolRequest(t, connection, fmt.Sprintf(
					`{"seq":%d,"type":"request","command":"reportProcess","arguments":{"systemProcessId":%d,"startMethod":%q}}`,
					index+1, handles[index].Pid, method))
				readProjectDebugMessage(t, connection)
				readProjectDebugMessage(t, connection)
			}
			reportProjectTarget(t, connection, handles[0].Pid)
			observed := waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
				if current.Status.Debug == nil || len(current.Status.Debug.Sessions) != 1 ||
					len(current.Status.Debug.Sessions[0].Processes) != len(handles) {
					return false, nil
				}
				for _, target := range current.Status.Debug.Sessions[0].Processes {
					if target.Observation.Phase != apiv2.ProjectDebugProcessRunning {
						return false, nil
					}
				}
				return true, nil
			})
			require.False(t, observed.Status.Debug.Sessions[0].Processes[1].Attached)
			require.True(t, observed.Status.Debug.Sessions[0].Processes[2].Attached)
			require.True(t, observed.Status.Debug.Sessions[0].Processes[3].Attached)
			require.NoError(t, connection.Close())
			idle := waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)
			require.Len(t, idle.Status.Debug.Sessions[0].Processes, len(handles))

			// Independently observed target exit must not depend on adapter or session lifetime.
			require.NoError(t, owner.StopProcess(ctx, handles[0]))
			waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
				observation := current.Status.Debug.Sessions[0].Processes[0].Observation
				return current.Status.Phase == apiv2.ProjectPhaseReady &&
					observation.Phase == apiv2.ProjectDebugProcessExited && !observation.FinishedAt.IsZero(), nil
			})
			switch ending {
			case "stop":
				require.NoError(t, client.Patch(ctx, project, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
				stopped := waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseStopped)
				for _, target := range stopped.Status.Debug.Sessions[0].Processes[1:] {
					require.Equal(t, apiv2.ProjectDebugProcessRunning, target.Observation.Phase)
				}
				require.NoError(t, client.Delete(ctx, project))
				ctrl_testutil.WaitObjectDeleted(t, ctx, client, project)
			case "delete":
				require.NoError(t, client.Delete(ctx, project))
				ctrl_testutil.WaitObjectDeleted(t, ctx, client, project)
			case "namespace":
				require.NoError(t, client.Delete(ctx, namespace))
				ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
			}
			for _, handle := range handles[1:] {
				require.NoError(t, owner.CheckProcessRunning(handle), "reported targets must survive Project cleanup")
			}
		})
	}
}
