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

// Verifies that a real adapter's initialization and launch outcomes stay separate from transport
// and Project readiness, retain rejection diagnostics, and interrupt pending requests on shutdown.
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
					DebugAdapter: &apiv2.DebugAdapterConfig{Args: []string{tool, "--dap-stdio"}},
				}}
			if ending == "delete" {
				project.Finalizers = []string{"test/inspect-target-cleanup"}
			}
			require.NoError(t, server.Client.Create(ctx, project))
			waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseReady)
			connection, connectErr := connectProjectDebug(t, ctx, server.ClientConfig, project)
			require.NoError(t, connectErr)
			defer connection.Close()
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
			}
			sendProjectProtocolRequest(t, connection, `{"seq":26,"type":"request","command":"attach","arguments":{"_testResponse":"defer"}}`)
			active := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
				return current.Status.Phase == apiv2.ProjectPhaseActive && len(current.Status.Debug.Sessions[0].ProtocolRequests) == 5, nil
			})
			protocol := active.Status.Debug.Sessions[0].ProtocolRequests
			require.Equal(t, apiv2.ProjectDebugRequestFailed, protocol[0].Phase)
			require.Equal(t, apiv2.ProjectDebugRequestSucceeded, protocol[1].Phase)
			require.Equal(t, apiv2.ProjectDebugRequestFailed, protocol[2].Phase)
			require.Equal(t, "test adapter rejected request", protocol[2].Message)
			require.Equal(t, apiv2.ProjectDebugRequestSucceeded, protocol[3].Phase)
			require.Equal(t, apiv2.ProjectDebugRequestPending, protocol[4].Phase)
			require.Equal(t, apiv2.ProjectDebugPhaseRunning, active.Status.Debug.Sessions[0].Phase)
			require.True(t, active.Status.Debug.PrerequisitesReady)
			require.True(t, active.Status.Debug.Sessions[0].ProcessTrackingUnconfirmed)
			encoded, encodeErr := json.Marshal(active.Status.Debug.Sessions[0])
			require.NoError(t, encodeErr)
			require.NotContains(t, string(encoded), "not-a-real-secret")
			require.NotContains(t, string(encoded), "futureCapability")
			require.Empty(t, active.Status.Debug.Sessions[0].Processes)
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
					session.ProtocolRequests[4].Phase == apiv2.ProjectDebugRequestInterrupted, nil
			})
			retained := completed.Status.Debug.Sessions[0].ProtocolRequests
			require.Equal(t, protocol[:4], retained[:4])
			require.False(t, retained[4].FinishedAt.IsZero())
			require.NotEmpty(t, retained[4].Message)
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
