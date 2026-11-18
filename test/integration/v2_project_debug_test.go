/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientrest "k8s.io/client-go/rest"
	clientwebsocket "k8s.io/client-go/transport/websocket"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dap"
	"github.com/microsoft/dcp/internal/projectdebug"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that the production API-server process launches a real host adapter only on upgrade,
// reports its identity, drains graceful DAP shutdown, fails unexpected disconnect, and bounds
// ProjectProcess deletion when an adapter ignores disconnect without changing application lifetime.
func TestV2ProjectProcessDebugTransportLifecycle(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"disconnect", "graceful", "delete", "unresponsive-delete"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createV2ExecNamespace(t, ctx, client, "v2-project-debug-runtime-"+name)
			tool, toolErr := internaltest.GetTestToolPath("delay")
			require.NoError(t, toolErr)
			args := []string{tool, "--dap-stdio"}
			if name == "unresponsive-delete" {
				args = args[:1]
			}
			project := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-application"}},
					DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: args}}}
			require.NoError(t, client.Create(ctx, project))
			running := waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)
			require.Nil(t, running.Status.Debug)
			connection, connectErr := connectProjectDebug(t, ctx, clientConfig, project)
			require.NoError(t, connectErr)
			defer connection.Close()
			if deadline, exists := ctx.Deadline(); exists {
				require.NoError(t, connection.SetReadDeadline(deadline))
			}
			active := waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
				return current.Status.Debug != nil && len(current.Status.Debug.Executions) == 1 &&
					current.Status.Debug.Executions[0].Phase == apiv2.ProjectDebugPhaseRunning &&
					len(current.Status.Debug.Sessions) == 1 && current.Status.Debug.Sessions[0].Phase == apiv2.ProjectDebugPhaseRunning, nil
			})
			adapter := active.Status.Debug.Executions[0]
			waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseActive)
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
				require.NoError(t, client.Delete(ctx, project))
				if name == "unresponsive-delete" {
					require.NoError(t, client.Get(ctx, project.NamespacedName(), project))
					require.False(t, project.DeletionTimestamp.IsZero())
				}
				if name == "delete" {
					event := readProjectDebugMessage(t, connection)
					require.JSONEq(t, `"terminated"`, string(event["event"]), "private disconnect response must not reach Aspire")
				}
				ctrl_testutil.WaitObjectDeleted(t, ctx, client, project)
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
				completed := waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
					return current.Status.Debug.Sessions[0].Phase == apiv2.ProjectDebugPhaseExited &&
						current.Status.Debug.Executions[0].Phase == apiv2.ProjectDebugPhaseExited, nil
				})
				require.Equal(t, running.Status.EffectiveArgs, completed.Status.EffectiveArgs)
				require.NotNil(t, completed.Status.Debug.Executions[0].ExitCode)
				require.Zero(t, *completed.Status.Debug.Executions[0].ExitCode)
				waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)
			default:
				require.NoError(t, connection.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")))
				require.NoError(t, connection.Close())
				completed := waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
					return current.Status.Debug != nil && current.Status.Debug.Sessions[0].Phase == apiv2.ProjectDebugPhaseFailed &&
						current.Status.Debug.Executions[0].Phase == apiv2.ProjectDebugPhaseExited, nil
				})
				require.Equal(t, running.Status.EffectiveArgs, completed.Status.EffectiveArgs)
				require.True(t, reflect.DeepEqual(running.Status.EffectiveEnv, completed.Status.EffectiveEnv))
				waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)
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

// Verifies through each enabled real runtime that container stdio adapter sessions use the
// prepared context without host PIDs and retain that context across connections until ProjectContainer deletion.
func TestV2ProjectContainerAdapterWithRealOrchestrator(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 3*time.Minute)
	t.Cleanup(testCancel)
	containertest.ForEachHealthyRuntime(t, testCtx, func(t *testing.T, runtimeCtx context.Context, runtime containertest.Runtime) {
		ctx, cancel := context.WithCancel(runtimeCtx)
		defer cancel()
		tracker := containertest.NewResourceTracker(t, runtime)
		server, environment, startupErr := StartAdvancedTestEnvironmentWithOptions(ctx,
			projectTestControllers, containertest.UniqueName(t, "project-adapter-environment"), t.TempDir(),
			AdvancedTestEnvironmentOptions{ApiServerFlags: ctrl_testutil.ApiServerUseTrueContainerOrchestrator, ContainerOrchestrator: runtime.Orchestrator})
		require.NoError(t, startupErr)
		defer environment.ProcessExecutor.Dispose()
		defer shutdownAdvancedTestEnvironment(t, ctx, cancel, server)
		namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: containertest.UniqueName(t, "project-adapter-namespace")}}
		require.NoError(t, server.Client.Create(ctx, namespace))
		var labels []commonapi.Label
		for name, value := range tracker.MapLabels() {
			labels = append(labels, commonapi.Label{Key: name, Value: value})
		}
		project := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
			Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: "busybox:latest"}, Labels: labels,
			}},
				DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"cat"}}},
			}}
		require.NoError(t, server.Client.Create(ctx, project))
		prepared := waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseReady)
		require.NoError(t, tracker.TrackContainer(prepared.Status.ContainerName))
		require.Equal(t, runtime.Orchestrator.Name(), prepared.Status.ContainerRuntime)
		for range 2 {
			connection, connectErr := connectProjectDebug(t, ctx, server.ClientConfig, project)
			require.NoError(t, connectErr)
			sendProjectProtocolRequest(t, connection, `{"seq":41,"type":"request","command":"initialize","arguments":{"extension":"unchanged"}}`)
			echo := readProjectDebugMessage(t, connection)
			require.JSONEq(t, `"initialize"`, string(echo["command"]))
			require.JSONEq(t, `{"extension":"unchanged"}`, string(echo["arguments"]))
			require.NoError(t, connection.Close())
			ready := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
				if current.Status.Phase != apiv2.ProjectPhaseReady {
					return false, nil
				}
				for _, session := range current.Status.Debug.Sessions {
					if session.Phase == apiv2.ProjectDebugPhasePending || session.Phase == apiv2.ProjectDebugPhaseRunning {
						return false, nil
					}
				}
				return true, nil
			})
			require.Equal(t, prepared.Status.ContainerID, ready.Status.ContainerID)
			for _, execution := range ready.Status.Debug.Executions {
				require.Nil(t, execution.PID)
			}
			require.Equal(t, prepared.Status.ContainerRuntime, ready.Status.ContainerRuntime)
		}
		require.NoError(t, server.Client.Delete(ctx, project))
		ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, project)
		require.NoError(t, server.Client.Delete(ctx, namespace))
		ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
	})
}

// Verifies through each enabled real runtime that overlapping container TCP-connect sessions share one
// in-container adapter provider through the runtime-assigned host binding, and that the idle adapter is retained for a later session.
func TestV2ProjectContainerTCPAdapterWithRealOrchestrator(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 3*time.Minute)
	t.Cleanup(testCancel)
	containertest.ForEachHealthyRuntime(t, testCtx, func(t *testing.T, runtimeCtx context.Context, runtime containertest.Runtime) {
		ctx, cancel := context.WithCancel(runtimeCtx)
		defer cancel()
		tracker := containertest.NewResourceTracker(t, runtime)
		server, environment, startupErr := StartAdvancedTestEnvironmentWithOptions(ctx,
			projectTestControllers, containertest.UniqueName(t, "project-tcp-adapter-environment"), t.TempDir(),
			AdvancedTestEnvironmentOptions{ApiServerFlags: ctrl_testutil.ApiServerUseTrueContainerOrchestrator, ContainerOrchestrator: runtime.Orchestrator})
		require.NoError(t, startupErr)
		defer environment.ProcessExecutor.Dispose()
		defer shutdownAdvancedTestEnvironment(t, ctx, cancel, server)
		namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: containertest.UniqueName(t, "project-tcp-adapter-namespace")}}
		require.NoError(t, server.Client.Create(ctx, namespace))
		var labels []commonapi.Label
		for name, value := range tracker.MapLabels() {
			labels = append(labels, commonapi.Label{Key: name, Value: value})
		}

		// Busybox nc with a repeated -l forks a cat echo server per accepted connection.
		project := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
			Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: "busybox:latest"}, Labels: labels,
			}},
				DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Mode: apiv2.DebugAdapterModeTCPConnect,
					Args: []string{"nc", "-ll", "-p", "{{port}}", "-e", "cat"}}},
			}}
		require.NoError(t, server.Client.Create(ctx, project))
		prepared := waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseReady)
		require.NoError(t, tracker.TrackContainer(prepared.Status.ContainerName))
		require.Equal(t, apiv2.DefaultContainerDebugAdapterPort, prepared.Status.DebugAdapterPort)
		require.NotEmpty(t, prepared.Status.DebugAdapterAddress)

		var connections []*websocket.Conn
		for index := range 2 {
			connection, connectErr := connectProjectDebug(t, ctx, server.ClientConfig, project)
			require.NoError(t, connectErr)
			defer connection.Close()
			if deadline, exists := ctx.Deadline(); exists {
				require.NoError(t, connection.SetReadDeadline(deadline))
			}
			sendProjectProtocolRequest(t, connection, fmt.Sprintf(`{"seq":%d,"type":"request","command":"initialize","arguments":{"session":%d}}`, 41+index, index))
			echo := readProjectDebugMessage(t, connection)
			require.JSONEq(t, `"initialize"`, string(echo["command"]))
			require.JSONEq(t, fmt.Sprintf(`{"session":%d}`, index), string(echo["arguments"]))
			connections = append(connections, connection)
		}
		shared := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
			if current.Status.Debug == nil || len(current.Status.Debug.Sessions) != 2 {
				return false, nil
			}
			for _, session := range current.Status.Debug.Sessions {
				if session.Phase != apiv2.ProjectDebugPhaseRunning {
					return false, nil
				}
			}
			return true, nil
		})
		require.Len(t, shared.Status.Debug.Executions, 1, "overlapping TCP-connect sessions must share one adapter provider")
		execution := shared.Status.Debug.Executions[0]
		require.Equal(t, apiv2.ProjectDebugPhaseRunning, execution.Phase)
		require.Nil(t, execution.PID)
		for _, session := range shared.Status.Debug.Sessions {
			require.Equal(t, execution.ID, session.ExecutionID)
		}

		for _, connection := range connections {
			require.NoError(t, connection.Close())
		}
		released := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
			if current.Status.Phase != apiv2.ProjectPhaseReady || len(current.Status.Debug.Executions) != 1 {
				return false, nil
			}
			for _, session := range current.Status.Debug.Sessions {
				if session.Phase == apiv2.ProjectDebugPhasePending || session.Phase == apiv2.ProjectDebugPhaseRunning {
					return false, nil
				}
			}
			return true, nil
		})
		require.Equal(t, prepared.Status.ContainerID, released.Status.ContainerID)
		require.Equal(t, prepared.Status.DebugAdapterAddress, released.Status.DebugAdapterAddress)
		require.Equal(t, execution.ID, released.Status.Debug.Executions[0].ID)
		require.Equal(t, apiv2.ProjectDebugPhaseRunning, released.Status.Debug.Executions[0].Phase,
			"final release must leave the in-container adapter running")

		reused, reuseErr := connectProjectDebug(t, ctx, server.ClientConfig, project)
		require.NoError(t, reuseErr)
		defer reused.Close()
		if deadline, exists := ctx.Deadline(); exists {
			require.NoError(t, reused.SetReadDeadline(deadline))
		}
		sendProjectProtocolRequest(t, reused, `{"seq":43,"type":"request","command":"initialize","arguments":{"session":2}}`)
		reusedEcho := readProjectDebugMessage(t, reused)
		require.JSONEq(t, `{"session":2}`, string(reusedEcho["arguments"]))
		current := &apiv2.ProjectContainer{}
		require.NoError(t, server.Client.Get(ctx, project.NamespacedName(), current))
		require.Len(t, current.Status.Debug.Executions, 1, "a later session must reuse the retained adapter")
		for _, session := range current.Status.Debug.Sessions {
			require.Equal(t, execution.ID, session.ExecutionID)
		}
		require.NoError(t, reused.Close())

		require.NoError(t, server.Client.Delete(ctx, project))
		ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, project)
		require.NoError(t, server.Client.Delete(ctx, namespace))
		ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
	})
}

func sendProjectProtocolRequest(t *testing.T, connection *websocket.Conn, body string) {
	t.Helper()
	require.NoError(t, connection.WriteMessage(websocket.BinaryMessage, []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body))))
}

// Verifies that a real adapter's DAP messages establish session milestones without request history,
// publish debuggee exit independently, and retain terminal connection observations after shutdown.
func TestV2ProjectProcessProtocolProgress(t *testing.T) {
	t.Parallel()
	for _, ending := range []string{"disconnect", "stop", "delete"} {
		t.Run(ending, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createV2ExecNamespace(t, ctx, client, "v2-project-protocol-"+ending)
			tool, toolErr := internaltest.GetTestToolPath("delay")
			require.NoError(t, toolErr)
			project := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{
					ExecutablePath: "adapter-started", AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit}}},
					DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{tool, "--dap-stdio"}},
				}}
			if ending == "delete" {
				project.Finalizers = []string{"test/inspect-target-cleanup"}
			}
			require.NoError(t, client.Create(ctx, project))
			if ending == "delete" {
				t.Cleanup(func() {
					cleanupCtx, cleanupCancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
					defer cleanupCancel()
					releaseProjectInspection(t, cleanupCtx, client, project)
				})
			}
			waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)
			connection, connectErr := connectProjectDebug(t, ctx, clientConfig, project)
			require.NoError(t, connectErr)
			defer connection.Close()
			connected := waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
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
					rejected := waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
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
			active := waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
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
					require.NoError(t, client.Patch(ctx, project, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
				} else {
					require.NoError(t, client.Delete(ctx, project))
				}
				event := readProjectDebugMessage(t, connection)
				require.JSONEq(t, `"terminated"`, string(event["event"]))
			}
			completed := waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
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
				waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)
			case "stop":
				waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseStopped)
			case "delete":
				releaseProjectInspection(t, ctx, client, project)
				ctrl_testutil.WaitObjectDeleted(t, ctx, client, project)
			}
		})
	}
}

// Verifies that debug sessions remain usable when Projects with the same name in different
// namespaces are created, updated, and disconnected.
func TestV2ProjectProcessDebugSessionsAcrossNamespaces(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	firstNamespace := createV2ExecNamespace(t, ctx, client, "v2-project-debug-watch-first")
	secondNamespace := createV2ExecNamespace(t, ctx, client, "v2-project-debug-watch-second")
	tool, toolErr := internaltest.GetTestToolPath("delay")
	require.NoError(t, toolErr)

	startSession := func(namespace string) (*apiv2.ProjectProcess, *websocket.Conn) {
		project := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace},
			Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace + "-application"}},
				DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{tool, "--dap-stdio"}}}}
		require.NoError(t, client.Create(ctx, project))
		waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)
		connection, connectErr := connectProjectDebug(t, ctx, clientConfig, project)
		require.NoError(t, connectErr)
		t.Cleanup(func() { _ = connection.Close() })
		sendProjectProtocolRequest(t, connection, `{"seq":1,"type":"request","command":"initialize","arguments":{}}`)
		readProjectDebugMessage(t, connection)
		sendProjectProtocolRequest(t, connection, `{"seq":2,"type":"request","command":"launch","arguments":{"noDebug":true}}`)
		readProjectDebugMessage(t, connection)
		waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
			return current.Status.Debug != nil && len(current.Status.Debug.Sessions) == 1 &&
				current.Status.Debug.Sessions[0].Progress != nil && current.Status.Debug.Sessions[0].Progress.LaunchSucceeded &&
				current.Status.Debug.Sessions[0].Phase == apiv2.ProjectDebugPhaseRunning, nil
		})
		return project, connection
	}

	first, firstConnection := startSession(firstNamespace.Name)
	second, secondConnection := startSession(secondNamespace.Name)
	require.NoError(t, secondConnection.Close())
	waitProjectPhase(t, ctx, client, second, apiv2.ProjectPhaseReady)

	sendProjectProtocolRequest(t, firstConnection, `{"seq":3,"type":"request","command":"reportProcess","arguments":{"name":"target"}}`)
	event := readProjectDebugMessage(t, firstConnection)
	require.JSONEq(t, `"process"`, string(event["event"]))
	readProjectDebugMessage(t, firstConnection)
	waitObjectAssumesStateEx(t, ctx, client, first.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
		return current.Status.Debug != nil && len(current.Status.Debug.Sessions) == 1 &&
			current.Status.Debug.Sessions[0].Progress != nil && current.Status.Debug.Sessions[0].Progress.ProcessReported &&
			current.Status.Debug.Sessions[0].Phase == apiv2.ProjectDebugPhaseRunning, nil
	})
}

// Verifies that the ProjectProcess controller publishes service endpoints only after a ready debug session
// requests launch, including a noDebug launch whose adapter omits initialized, and withdraws them when
// that session disconnects or the ProjectProcess is deleted.
func TestV2ProjectProcessEndpointsFollowLaunchedSession(t *testing.T) {
	t.Parallel()
	for _, ending := range []string{"disconnect", "delete", "uninitialized-launch"} {
		t.Run(ending, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createV2ExecNamespace(t, ctx, client, "v2-project-endpoints-"+ending)
			tool, toolErr := internaltest.GetTestToolPath("delay")
			require.NoError(t, toolErr)
			project := serviceProjectProcess("project", namespace.Name, t.TempDir(), nil, nil)
			project.Spec.DebugAdapter = &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{tool, "--dap-stdio"}}
			require.NoError(t, client.Create(ctx, project))
			prepared := waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)
			require.Len(t, prepared.Status.Ports, 1)
			connection, connectErr := connectProjectDebug(t, ctx, clientConfig, project)
			require.NoError(t, connectErr)
			defer connection.Close()

			sendProjectProtocolRequest(t, connection, `{"seq":1,"type":"request","command":"initialize","arguments":{}}`)
			readProjectDebugMessage(t, connection)
			expectedReady := metav1.ConditionFalse
			if ending != "uninitialized-launch" {
				sendProjectProtocolRequest(t, connection, `{"seq":2,"type":"request","command":"reportInitialized","arguments":{}}`)
				readProjectDebugMessage(t, connection)
				readProjectDebugMessage(t, connection)
				expectedReady = metav1.ConditionTrue
			}
			waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
				return current.Status.Phase == apiv2.ProjectPhaseActive && current.Status.Debug != nil && len(current.Status.Debug.Sessions) == 1 &&
					current.Status.Debug.Sessions[0].Progress != nil && current.Status.Debug.Sessions[0].Progress.InitializeRequested &&
					len(current.Status.Debug.Sessions[0].Conditions) == 1 && current.Status.Debug.Sessions[0].Conditions[0].Status == expectedReady, nil
			})
			var endpoints apiv2.ServiceEndpointList
			require.NoError(t, client.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
			require.Empty(t, endpoints.Items, "endpoints must not be published before launch")

			sendProjectProtocolRequest(t, connection, `{"seq":3,"type":"request","command":"launch","arguments":{"noDebug":true}}`)
			readProjectDebugMessage(t, connection)
			require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
				listErr := client.List(pollCtx, &endpoints, ctrl_client.InNamespace(namespace.Name))
				return listErr == nil && len(endpoints.Items) == 1, nil
			}))
			endpoint := endpoints.Items[0]
			require.True(t, metav1.IsControlledBy(&endpoint, prepared))
			require.Equal(t, prepared.Status.Ports[0].ServiceRef, endpoint.Spec.ServiceRef)
			require.Equal(t, []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: prepared.Status.Ports[0].Port}}, endpoint.Spec.Addresses)

			if ending == "delete" {
				require.NoError(t, client.Delete(ctx, project))
			} else {
				require.NoError(t, connection.Close())
			}
			require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
				listErr := client.List(pollCtx, &endpoints, ctrl_client.InNamespace(namespace.Name))
				return listErr == nil && len(endpoints.Items) == 0, nil
			}))
			if ending != "delete" {
				waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)
				require.NoError(t, client.Delete(ctx, project))
			}
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, project)
		})
	}
}

// Verifies that the ProjectProcess controller keeps service endpoints published while any launched debug session
// remains ready, and withdraws them only after the last ready launched session ends.
func TestV2ProjectProcessEndpointsFollowLastReadySession(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	namespace := createV2ExecNamespace(t, ctx, client, "v2-project-endpoints-last-session")
	tool, toolErr := internaltest.GetTestToolPath("delay")
	require.NoError(t, toolErr)
	project := serviceProjectProcess("project", namespace.Name, t.TempDir(), nil, nil)
	project.Spec.DebugAdapter = &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{tool, "--dap-stdio"}}
	require.NoError(t, client.Create(ctx, project))
	waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)

	readySessions := func(current *apiv2.ProjectProcess) (running int, ready int) {
		if current.Status.Debug == nil {
			return 0, 0
		}
		for _, session := range current.Status.Debug.Sessions {
			if session.Phase == apiv2.ProjectDebugPhaseRunning {
				running++
				if session.Progress != nil && session.Progress.LaunchSucceeded &&
					len(session.Conditions) == 1 && session.Conditions[0].Status == metav1.ConditionTrue {
					ready++
				}
			}
		}
		return running, ready
	}
	connections := make([]*websocket.Conn, 2)
	for index := range connections {
		connection, connectErr := connectProjectDebug(t, ctx, clientConfig, project)
		require.NoError(t, connectErr)
		defer connection.Close()
		connections[index] = connection
		sendProjectProtocolRequest(t, connection, `{"seq":1,"type":"request","command":"initialize","arguments":{}}`)
		readProjectDebugMessage(t, connection)
		sendProjectProtocolRequest(t, connection, `{"seq":2,"type":"request","command":"launch","arguments":{"noDebug":true}}`)
		readProjectDebugMessage(t, connection)
	}
	waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
		_, ready := readySessions(current)
		return ready == 2, nil
	})
	var endpoints apiv2.ServiceEndpointList
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		listErr := client.List(pollCtx, &endpoints, ctrl_client.InNamespace(namespace.Name))
		return listErr == nil && len(endpoints.Items) == 1, nil
	}))
	publishedUID := endpoints.Items[0].UID

	require.NoError(t, connections[0].Close())
	waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
		running, ready := readySessions(current)
		return running == 1 && ready == 1, nil
	})
	require.NoError(t, client.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
	require.Len(t, endpoints.Items, 1, "endpoints must remain while another launched session is ready")
	require.Equal(t, publishedUID, endpoints.Items[0].UID)

	require.NoError(t, connections[1].Close())
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		listErr := client.List(pollCtx, &endpoints, ctrl_client.InNamespace(namespace.Name))
		return listErr == nil && len(endpoints.Items) == 0, nil
	}))
	waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)
	require.NoError(t, client.Delete(ctx, project))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, project)
}

func connectProjectDebug(t *testing.T, ctx context.Context, config *clientrest.Config, project apiv2.ProjectObject) (*websocket.Conn, error) {
	t.Helper()
	transport, holder, transportErr := clientwebsocket.RoundTripperFor(config)
	if transportErr != nil {
		return nil, transportErr
	}
	plural := "projectprocesses"
	if _, container := project.(*apiv2.ProjectContainer); container {
		plural = "projectcontainers"
	}
	request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet,
		config.Host+"/apis/"+apiv2.GroupVersion.String()+"/namespaces/"+project.GetNamespace()+"/"+plural+"/"+project.GetName()+"/debug", nil)
	if requestErr != nil {
		return nil, requestErr
	}
	if token := os.Getenv(projectdebug.DCP_DEBUG_SESSION_TOKEN); token != "" {
		request.Header.Set("X-DCP-Debug-Session-Token", token)
	}
	connection, connectionErr := clientwebsocket.Negotiate(transport, holder, request, "dap")
	if connectionErr == nil {
		if deadline, exists := ctx.Deadline(); exists {
			if deadlineErr := connection.SetReadDeadline(deadline); deadlineErr != nil {
				_ = connection.Close()
				return nil, deadlineErr
			}
		}
	}
	return connection, connectionErr
}
