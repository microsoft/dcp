/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientwebsocket "k8s.io/client-go/transport/websocket"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dap"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
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
					DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: args}}}
			require.NoError(t, apiClient.Create(ctx, project))
			running := waitProjectPhase(t, ctx, apiClient, project, apiv2.ProjectPhaseReady)
			require.Nil(t, running.Status.Debug)
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

// Verifies that Project installs adapter files at the default or overridden destination before readiness,
// reports missing sources as preparation failures, and does not launch an adapter during installation.
func TestV2ProjectContainerAdapterDirectoryPreparation(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"default", "custom", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			server, _, startupErr := StartTestEnvironment(t, ctx, projectTestControllers, t.Name(), t.TempDir())
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-adapter-directory-"+scenario)
			source := t.TempDir()
			require.NoError(t, usvc_io.WriteFile(filepath.Join(source, "dependency.dll"), []byte("dependency"), 0644))
			project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(),
					Container: &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
						Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
					}},
					DebugAdapter: &apiv2.DebugAdapterConfig{Directory: source, Args: []string{"{{adapterDirectory}}/adapter"}},
				}}
			destination := apiv2.DefaultContainerDebugAdapterDirectory
			if scenario == "custom" {
				destination = "/custom/adapter"
				project.Spec.DebugAdapter.ContainerDirectory = destination
			} else if scenario == "missing" {
				project.Spec.DebugAdapter.Directory = filepath.Join(source, "missing")
			}
			require.NoError(t, server.Client.Create(ctx, project))
			expectedPhase := apiv2.ProjectPhaseReady
			if scenario == "missing" {
				expectedPhase = apiv2.ProjectPhaseFailed
			}
			observed := waitProjectPhase(t, ctx, server.Client, project, expectedPhase)
			require.Nil(t, observed.Status.Debug)
			if scenario == "missing" {
				requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
				require.Contains(t, observed.Status.Conditions[0].Message, "directory source")
			} else {
				physical := &apiv2.PhysicalContainer{}
				require.NoError(t, server.Client.Get(ctx, ctrlclient.ObjectKey{Namespace: project.Namespace, Name: observed.Status.PhysicalContainerRef}, physical))
				require.Empty(t, physical.Spec.Container.CreateFiles)
				require.Len(t, physical.Spec.Container.CopyFiles, 1)
				require.Equal(t, source, physical.Spec.Container.CopyFiles[0].Directory)
				require.Equal(t, destination, physical.Spec.Container.CopyFiles[0].Destination)
				require.Empty(t, physical.Spec.Container.CopyFiles[0].Source)
				require.False(t, physical.Spec.Container.CopyFiles[0].DeleteAfterCopy)
				require.DirExists(t, source)
				orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
				copied, copiedErr := orchestrator.GetCreatedFiles(observed.Status.ContainerID)
				require.NoError(t, copiedErr)
				require.Len(t, copied, 1)
				reader := tar.NewReader(bytes.NewReader(copied[0].Tar.Bytes()))
				found := false
				for {
					header, nextErr := reader.Next()
					if nextErr == io.EOF {
						break
					}
					require.NoError(t, nextErr)
					if header.Name == strings.TrimPrefix(destination, "/")+"/dependency.dll" {
						payload, readErr := io.ReadAll(reader)
						require.NoError(t, readErr)
						require.Equal(t, "dependency", string(payload))
						found = true
					}
				}
				require.True(t, found)
			}
			require.NoError(t, server.Client.Delete(ctx, project))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, project)
		})
	}
}

// Verifies through each enabled real runtime that container stdio adapter sessions use the
// prepared context without host PIDs and retain that context across connections until Project deletion.
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
		project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
			Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(),
				Container: &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
					Image: &apiv2.PhysicalContainerImageConfig{Image: "busybox:latest"}, Labels: labels,
				}},
				DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"cat"}},
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
			ready := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
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
