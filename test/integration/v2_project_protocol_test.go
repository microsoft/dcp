/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

func sendProjectProtocolRequest(t *testing.T, connection *websocket.Conn, body string) {
	t.Helper()
	require.NoError(t, connection.WriteMessage(websocket.BinaryMessage, []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body))))
}

// Verifies that a real adapter's DAP messages establish session milestones without request history,
// publish debuggee exit independently, and retain terminal connection observations after shutdown.
func TestV2ProjectProtocolProgress(t *testing.T) {
	t.Parallel()
	for _, ending := range []string{"disconnect", "stop", "delete"} {
		t.Run(ending, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			server, _, startupErr := StartTestEnvironment(t, ctx, ProjectController, t.Name(), t.TempDir())
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-protocol-"+ending)
			tool, toolErr := internaltest.GetTestToolPath("delay")
			require.NoError(t, toolErr)
			project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(),
					Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{
						ExecutablePath: "adapter-started", AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit}}},
					DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{tool, "--dap-stdio"}},
				}}
			if ending == "delete" {
				project.Finalizers = []string{"test/inspect-target-cleanup"}
			}
			require.NoError(t, server.Client.Create(ctx, project))
			waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseReady)
			connection, connectErr := connectProjectDebug(t, ctx, server.ClientConfig, project)
			require.NoError(t, connectErr)
			defer connection.Close()
			connected := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
				return current.Status.Debug != nil && len(current.Status.Debug.Sessions) != 0 &&
					current.Status.Debug.Sessions[0].Progress != nil &&
					current.Status.Debug.Sessions[0].Progress.StreamsConnected, nil
			})
			require.Equal(t, apiv2.ProjectDebugPhaseRunning, connected.Status.Debug.Sessions[0].Phase)
			require.Equal(t, metav1.ConditionFalse, connected.Status.Debug.Sessions[0].Conditions[0].Status)
			for index, request := range []string{
				`{"seq":22,"type":"request","command":"initialize","arguments":{"_testResponse":"reject"}}`,
				`{"seq":23,"type":"request","command":"initialize","arguments":{"futureCapability":true}}`,
				`{"seq":24,"type":"request","command":"launch","arguments":{"_testResponse":"reject","env":{"TOKEN":"not-a-real-secret"}}}`,
				`{"seq":25,"type":"request","command":"launch","arguments":{"noDebug":true,"extension":{"keep":true}}}`,
			} {
				sendProjectProtocolRequest(t, connection, request)
				response := readProjectDebugMessage(t, connection)
				require.JSONEq(t, fmt.Sprint(22+index), string(response["request_seq"]))
				require.JSONEq(t, fmt.Sprint(index%2 == 1), string(response["success"]))
				if index%2 == 0 {
					rejected := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
						return current.Status.Debug != nil && len(current.Status.Debug.Sessions) != 0 &&
							current.Status.Debug.Sessions[0].Progress != nil &&
							current.Status.Debug.Sessions[0].Progress.Message != "", nil
					})
					require.Contains(t, rejected.Status.Debug.Sessions[0].Progress.Message, "test adapter rejected request")
				}
			}
			sendProjectProtocolRequest(t, connection, `{"seq":26,"type":"request","command":"reportInitialized","arguments":{}}`)
			initialized := readProjectDebugMessage(t, connection)
			require.JSONEq(t, `"initialized"`, string(initialized["event"]))
			readProjectDebugMessage(t, connection)
			sendProjectProtocolRequest(t, connection, `{"seq":27,"type":"request","command":"reportExit","arguments":{"exitCode":7}}`)
			exited := readProjectDebugMessage(t, connection)
			require.JSONEq(t, `"exited"`, string(exited["event"]))
			readProjectDebugMessage(t, connection)
			sendProjectProtocolRequest(t, connection, `{"seq":28,"type":"request","command":"reportProcess","arguments":{"name":"reported-target"}}`)
			reported := readProjectDebugMessage(t, connection)
			require.JSONEq(t, `"process"`, string(reported["event"]))
			readProjectDebugMessage(t, connection)
			sendProjectProtocolRequest(t, connection, `{"seq":29,"type":"request","command":"attach","arguments":{"_testResponse":"defer"}}`)
			active := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
				if current.Status.Debug == nil || len(current.Status.Debug.Sessions) == 0 {
					return false, nil
				}
				progress := current.Status.Debug.Sessions[0].Progress
				return current.Status.Phase == apiv2.ProjectPhaseActive && progress != nil &&
					progress.InitializeRequested && progress.LaunchRequested && progress.AttachRequested &&
					progress.AdapterInitialized && progress.DebuggeeExited && progress.ProcessReported &&
					len(current.Status.Debug.Sessions[0].Processes) == 1, nil
			})
			progress := active.Status.Debug.Sessions[0].Progress
			require.Equal(t, apiv2.ProjectDebugConnectionConnected, progress.Phase)
			require.True(t, progress.StreamsConnected)
			require.EqualValues(t, 7, *progress.DebuggeeExitCode)
			require.Equal(t, metav1.ConditionTrue, active.Status.Debug.Sessions[0].Conditions[0].Status)
			require.Equal(t, apiv2.ProjectDebugPhaseRunning, active.Status.Debug.Sessions[0].Phase)
			require.True(t, active.Status.Debug.Sessions[0].ProcessTrackingUnconfirmed)
			encoded, encodeErr := json.Marshal(active.Status.Debug.Sessions[0])
			require.NoError(t, encodeErr)
			require.NotContains(t, string(encoded), "not-a-real-secret")
			require.NotContains(t, string(encoded), "futureCapability")
			require.NotContains(t, string(encoded), "protocolRequests")
			require.Equal(t, "reported-target", active.Status.Debug.Sessions[0].Processes[0].Name)
			require.Nil(t, active.Status.Debug.Sessions[0].Processes[0].PID)
			if ending == "disconnect" {
				require.NoError(t, connection.Close())
			} else {
				if ending == "stop" {
					require.NoError(t, server.Client.Patch(ctx, project, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
				} else {
					require.NoError(t, server.Client.Delete(ctx, project))
				}
				event := readProjectDebugMessage(t, connection)
				require.JSONEq(t, `"terminated"`, string(event["event"]))
			}
			completed := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
				session := current.Status.Debug.Sessions[0]
				return (session.Phase == apiv2.ProjectDebugPhaseExited || session.Phase == apiv2.ProjectDebugPhaseFailed) &&
					session.Progress != nil && session.Progress.Phase == apiv2.ProjectDebugConnectionDisconnected &&
					session.Progress.ClientDisconnected && session.Progress.AdapterDisconnected, nil
			})
			retained := completed.Status.Debug.Sessions[0].Progress
			require.True(t, retained.InitializeRequested)
			require.True(t, retained.AdapterInitialized)
			require.True(t, retained.LaunchRequested)
			require.True(t, retained.AttachRequested)
			require.Equal(t, progress.DebuggeeExitCode, retained.DebuggeeExitCode)
			require.NotEmpty(t, retained.ClientDisconnectMessage)
			require.NotEmpty(t, retained.AdapterDisconnectMessage)
			if ending != "disconnect" {
				require.True(t, retained.ResourceTerminating)
				require.True(t, retained.AdapterTerminating)
			}
			switch ending {
			case "disconnect":
				waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseReady)
			case "stop":
				waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseStopped)
			case "delete":
				releaseProjectInspection(t, ctx, server.Client, project)
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, project)
			}
		})
	}
}
