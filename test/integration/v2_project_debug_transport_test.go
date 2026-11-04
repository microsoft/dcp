/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientwebsocket "k8s.io/client-go/transport/websocket"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dap"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that the production API-server process launches a real host adapter only on upgrade,
// reports its identity, drains graceful DAP shutdown, fails unexpected disconnect, and bounds
// Project deletion when an adapter ignores disconnect without changing application lifetime.
func TestV2ProjectDebugTransportLifecycle(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"disconnect", "graceful", "delete", "unresponsive-delete"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			serverInfo, _, startupErr := StartTestEnvironment(t, ctx, ProjectController|PhysicalProcessController, t.Name(), t.TempDir())
			require.NoError(t, startupErr)
			apiClient := serverInfo.Client
			namespace := createV2ExecNamespace(t, ctx, apiClient, "v2-project-debug-runtime-"+name)
			tool, toolErr := internaltest.GetTestToolPath("delay")
			require.NoError(t, toolErr)
			args := []string{tool, "--dap-stdio"}
			if name == "unresponsive-delete" {
				args = args[:1]
			}
			project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-application"}},
					DebugAdapter: &apiv2.DebugAdapterConfig{Args: args}}}
			require.NoError(t, apiClient.Create(ctx, project))
			running := waitProjectPhase(t, ctx, apiClient, project, apiv2.ProjectPhaseReady)
			require.NotNil(t, running.Status.Debug)
			require.True(t, running.Status.Debug.PrerequisitesReady)
			require.Empty(t, running.Status.Debug.Executions)
			transport, holder, transportErr := clientwebsocket.RoundTripperFor(serverInfo.ClientConfig)
			require.NoError(t, transportErr)
			request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet,
				serverInfo.ClientConfig.Host+"/apis/"+apiv2.GroupVersion.String()+"/namespaces/"+namespace.Name+"/projects/"+project.Name+"/debug", nil)
			require.NoError(t, requestErr)
			connection, connectErr := clientwebsocket.Negotiate(transport, holder, request, "dap")
			require.NoError(t, connectErr)
			defer connection.Close()
			if deadline, exists := ctx.Deadline(); exists {
				require.NoError(t, connection.SetReadDeadline(deadline))
			}
			active := waitObjectAssumesStateEx(t, ctx, apiClient, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
				return current.Status.Debug != nil && len(current.Status.Debug.Executions) == 1 &&
					current.Status.Debug.Executions[0].Phase == apiv2.ProjectDebugPhaseRunning &&
					len(current.Status.Debug.Sessions) == 1 && current.Status.Debug.Sessions[0].Phase == apiv2.ProjectDebugPhaseRunning, nil
			})
			adapter := active.Status.Debug.Executions[0]
			waitProjectPhase(t, ctx, apiClient, project, apiv2.ProjectPhaseActive)
			require.NotNil(t, adapter.PID)
			pid, pidErr := process.Int64_ToPidT(*adapter.PID)
			require.NoError(t, pidErr)
			handle := process.NewHandle(pid, adapter.IdentityTimestamp.Time)
			executor := process.NewOSExecutor(logr.Discard())
			t.Cleanup(executor.Dispose)
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := process.WithDetachedStopTimeout(ctx)
				defer cleanupCancel()
				stopErr := executor.StopProcess(cleanupCtx, handle)
				require.True(t, stopErr == nil || process.IsProcessGoneErr(stopErr), "%v", stopErr)
			})
			require.NoError(t, executor.CheckProcessRunning(handle))
			switch name {
			case "delete", "unresponsive-delete":
				require.NoError(t, apiClient.Delete(ctx, project))
				if name == "unresponsive-delete" {
					require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
					require.False(t, project.DeletionTimestamp.IsZero())
				}
				if name == "delete" {
					event := readProjectDebugMessage(t, connection)
					require.JSONEq(t, `"terminated"`, string(event["event"]), "private disconnect response must not reach Aspire")
				}
				ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, project)
				if name == "unresponsive-delete" {
					require.False(t, time.Now().Before(project.DeletionTimestamp.Add(apiv2.ProjectDebugShutdownGrace)))
				}
			case "graceful":
				body := `{"seq":93,"type":"request","command":"disconnect","arguments":{}}`
				require.NoError(t, connection.WriteMessage(websocket.BinaryMessage, []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body))))
				response := readProjectDebugMessage(t, connection)
				require.JSONEq(t, "93", string(response["request_seq"]))
				event := readProjectDebugMessage(t, connection)
				require.JSONEq(t, `"terminated"`, string(event["event"]))
				completed := waitObjectAssumesStateEx(t, ctx, apiClient, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
					return current.Status.Debug.Sessions[0].Phase == apiv2.ProjectDebugPhaseExited &&
						current.Status.Debug.Executions[0].Phase == apiv2.ProjectDebugPhaseExited, nil
				})
				require.Equal(t, running.Status.EffectiveArgs, completed.Status.EffectiveArgs)
				require.NotNil(t, completed.Status.Debug.Executions[0].ExitCode)
				require.Zero(t, *completed.Status.Debug.Executions[0].ExitCode)
				waitProjectPhase(t, ctx, apiClient, project, apiv2.ProjectPhaseReady)
			default:
				require.NoError(t, connection.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")))
				require.NoError(t, connection.Close())
				completed := waitObjectAssumesStateEx(t, ctx, apiClient, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
					return current.Status.Debug != nil && current.Status.Debug.Sessions[0].Phase == apiv2.ProjectDebugPhaseFailed &&
						current.Status.Debug.Executions[0].Phase == apiv2.ProjectDebugPhaseExited, nil
				})
				require.Equal(t, running.Status.EffectiveArgs, completed.Status.EffectiveArgs)
				require.True(t, reflect.DeepEqual(running.Status.EffectiveEnv, completed.Status.EffectiveEnv))
				waitProjectPhase(t, ctx, apiClient, project, apiv2.ProjectPhaseReady)
			}
			require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
				return process.IsProcessGoneErr(executor.CheckProcessRunning(handle)), nil
			}))
		})
	}
}

func readProjectDebugMessage(t *testing.T, connection *websocket.Conn) dap.Message {
	t.Helper()
	_, payload, readErr := connection.ReadMessage()
	require.NoError(t, readErr)
	message, decodeErr := dap.ReadMessage(bufio.NewReader(bytes.NewReader(payload)))
	require.NoError(t, decodeErr)
	return message
}
