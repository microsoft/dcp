/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that PhysicalContainerExec uses real runtime arguments, environment, working directory,
// exit results, and output capture, and settles attached execution deletion and target exit.
func TestV2PhysicalContainerExecWithRealOrchestrator(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 3*time.Minute)
	t.Cleanup(testCancel)
	containertest.ForEachHealthyRuntime(t, testCtx, func(t *testing.T, runtimeCtx context.Context, runtime containertest.Runtime) {
		ctx, cancel := context.WithCancel(runtimeCtx)
		defer cancel()
		tracker := containertest.NewResourceTracker(t, runtime)
		containerName := containertest.UniqueName(t, "physical-exec")
		require.NoError(t, tracker.TrackContainer(containerName))
		server, environment, startupErr := StartAdvancedTestEnvironmentWithOptions(ctx,
			PhysicalContainerController|PhysicalContainerExecController,
			containertest.UniqueName(t, "physical-exec-environment"), t.TempDir(),
			AdvancedTestEnvironmentOptions{
				ApiServerFlags: ctrl_testutil.ApiServerUseTrueContainerOrchestrator, ContainerOrchestrator: runtime.Orchestrator,
			})
		require.NoError(t, startupErr)
		defer environment.ProcessExecutor.Dispose()
		defer shutdownAdvancedTestEnvironment(t, ctx, cancel, server)
		namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: containertest.UniqueName(t, "physical-exec-namespace")}}
		require.NoError(t, server.Client.Create(ctx, namespace))
		waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
			return current.Status.Phase == apiv2.NamespacePhaseActive, nil
		})
		imageID, pullErr := runtime.Orchestrator.PullImage(ctx, containers.PullImageOptions{
			Image: "mcr.microsoft.com/azurelinux/distroless/minimal:3.0.20260809",
		})
		require.NoError(t, pullErr)
		const probePath = "/dcp-exec-probe"
		containerID, createErr := runtime.Orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
			Name: containerName, Image: imageID, Entrypoint: probePath, Command: []string{"wait", "10m"},
			Labels: tracker.MapLabels(), PullPolicy: containers.PullPolicyNever,
		})
		require.NoError(t, createErr)
		probeBinary, probeErr := internal_testutil.GetTestContainerToolPath("container_probe_c")
		require.NoError(t, probeErr)
		require.NoError(t, runtime.Orchestrator.CreateFiles(ctx, containers.CreateFilesOptions{
			Container: containerID, Destination: "/",
			Entries: []containers.FileSystemEntry{{Name: "dcp-exec-probe", Source: probeBinary, Mode: 0755}},
		}))
		_, startErr := runtime.Orchestrator.StartContainers(ctx, containers.StartContainersOptions{Containers: []string{containerID}})
		require.NoError(t, startErr)
		target := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: namespace.Name},
			Spec: apiv2.PhysicalContainerSpec{ContainerID: containerID}}
		require.NoError(t, server.Client.Create(ctx, target))
		waitObjectAssumesStateEx(t, ctx, server.Client, target.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
			return current.Status.Phase == apiv2.PhysicalContainerPhaseRunning, nil
		})
		createExecution := func(t *testing.T, name string, config apiv2.ContainerExecConfig) *apiv2.PhysicalContainerExec {
			t.Helper()
			execution := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name},
				Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: target.Name, Execution: config}}
			require.NoError(t, server.Client.Create(ctx, execution))
			return execution
		}
		waitPhase := func(t *testing.T, execution *apiv2.PhysicalContainerExec, phase apiv2.ContainerExecPhase) *apiv2.PhysicalContainerExec {
			t.Helper()
			return waitObjectAssumesStateEx(t, ctx, server.Client, execution.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
				return current.Status.Phase == phase, nil
			})
		}
		readOutput := func(t *testing.T, execution *apiv2.PhysicalContainerExec, source string) string {
			t.Helper()
			gvr := execution.GetGroupVersionResource()
			stream, streamErr := server.RestClient.Get().
				AbsPath("/apis", gvr.Group, gvr.Version, "namespaces", namespace.Name, gvr.Resource, execution.Name, apiv2.LogSubresourceName).
				Param("source", source).Stream(ctx)
			require.NoError(t, streamErr)
			output, readErr := io.ReadAll(stream)
			require.NoError(t, stream.Close())
			require.NoError(t, readErr)
			return string(output)
		}
		for _, testCase := range []struct {
			name       string
			config     apiv2.ContainerExecConfig
			exitCode   int32
			stdout     string
			stderr     string
			anyNonzero bool
		}{
			{name: "inputs", config: apiv2.ContainerExecConfig{
				Command: probePath, Args: []string{"report", "DCP_EXEC_VALUE", "stderr with spaces\n"},
				WorkingDirectory: "/tmp", Env: []commonapi.EnvVar{{Name: "DCP_EXEC_VALUE", Value: "literal {{value}}"}},
			}, stdout: "literal {{value}}:/tmp", stderr: "stderr with spaces\n"},
			{name: "nonzero", config: apiv2.ContainerExecConfig{Command: probePath, Args: []string{"invalid-command"}},
				exitCode: 1, stderr: `unknown command "invalid-command"`},
			{name: "command-not-found", config: apiv2.ContainerExecConfig{Command: "/dcp-missing-executable"}, anyNonzero: true},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				execution := createExecution(t, testCase.name, testCase.config)
				exited := waitPhase(t, execution, apiv2.ContainerExecPhaseExited)
				require.NotNil(t, exited.Status.ExitCode)
				if testCase.anyNonzero {
					require.NotZero(t, *exited.Status.ExitCode)
				} else {
					require.Equal(t, testCase.exitCode, *exited.Status.ExitCode)
				}
				require.True(t, exited.Status.LogsComplete)
				require.False(t, exited.Status.StartedAt.IsZero())
				require.False(t, exited.Status.FinishedAt.IsZero())
				if testCase.stdout != "" {
					require.Equal(t, testCase.stdout, strings.TrimSuffix(readOutput(t, execution, "stdout"), "\n"))
				}
				if testCase.stderr != "" {
					require.Contains(t, readOutput(t, execution, "stderr"), testCase.stderr)
				}
				require.NoError(t, server.Client.Delete(ctx, execution))
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, execution)
			})
		}
		t.Run("delete-running-execution", func(t *testing.T) {
			execution := createExecution(t, "delete-running", apiv2.ContainerExecConfig{Command: probePath, Args: []string{"wait", "10m"}})
			waitPhase(t, execution, apiv2.ContainerExecPhaseRunning)
			require.NoError(t, server.Client.Delete(ctx, execution))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, execution)
			inspected, inspectErr := runtime.Orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
			require.NoError(t, inspectErr)
			require.Len(t, inspected, 1)
			require.Equal(t, containers.ContainerStatusRunning, inspected[0].Status)
		})
		t.Run("target-exit", func(t *testing.T) {
			execution := createExecution(t, "target-exit", apiv2.ContainerExecConfig{Command: probePath, Args: []string{"wait", "10m"}})
			waitPhase(t, execution, apiv2.ContainerExecPhaseRunning)
			require.NoError(t, retryOnConflictEx(ctx, server.Client, target.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainer) error {
				current.Spec.Stop = true
				return server.Client.Update(updateCtx, current)
			}))
			waitObjectAssumesStateEx(t, ctx, server.Client, target.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
				return current.Status.Phase == apiv2.PhysicalContainerPhaseExited, nil
			})
			exited := waitPhase(t, execution, apiv2.ContainerExecPhaseExited)
			require.True(t, exited.Status.LogsComplete)
			lateExecution := createExecution(t, "exited-target", apiv2.ContainerExecConfig{Command: probePath, Args: []string{"exit"}})
			failed := waitPhase(t, lateExecution, apiv2.ContainerExecPhaseFailed)
			requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, "ContainerUnavailable")
			require.Nil(t, failed.Status.ExitCode)
		})
		require.NoError(t, server.Client.Delete(ctx, namespace))
		ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
	})
}
