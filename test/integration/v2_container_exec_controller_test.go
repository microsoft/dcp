/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcpclient"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

type incompleteContainerExecOrchestrator struct {
	containers.ContainerOrchestrator
}

func (orchestrator *incompleteContainerExecOrchestrator) ExecContainer(ctx context.Context, options containers.ExecContainerOptions) (<-chan int32, error) {
	switch options.Command {
	case "nil-result":
		return nil, nil
	case "closed-result":
		result := make(chan int32)
		close(result)
		return result, nil
	default:
		return orchestrator.ContainerOrchestrator.ExecContainer(ctx, options)
	}
}

// Verifies that LogicalContainerExec delegates literal inputs, shares physical capture,
// records nonzero completion, and leaves the target container running.
func TestV2ContainerExecRunsWithSharedOutput(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	target := createV2ExecTarget(t, ctx, client, "v2-exec-output")
	resource := &apiv2.LogicalContainerExec{
		ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Namespace + "/" + target.Name,
			Execution: apiv2.ContainerExecConfig{
				Command: "v2-exec-output-command", Args: []string{"{{literal-argument}}"},
				WorkingDirectory: "/work", Env: []commonapi.EnvVar{{Name: "VALUE", Value: "{{literal-env}}"}},
				EnvFiles: []string{"/host/env-file"},
			}},
	}
	require.NoError(t, client.Create(ctx, resource))
	running := waitV2LogicalExecPhase(t, ctx, resource, apiv2.ContainerExecPhaseRunning)
	var physical apiv2.PhysicalContainerExec
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: target.Namespace, Name: running.Status.PhysicalExecRef}, &physical))
	require.Equal(t, resource.Spec.Execution, physical.Spec.Execution)
	require.Equal(t, target.Status.PhysicalContainerRef, physical.Spec.ContainerRef)
	require.Equal(t, physical.GetLogSource(), running.Status.RuntimeLogs)
	require.Equal(t, target.Status.ContainerID, running.Status.ContainerID)
	invocation, invocationErr := containerOrchestrator.GetContainerExecOptions(target.Status.ContainerID, resource.Spec.Execution.Command)
	require.NoError(t, invocationErr)
	require.Equal(t, resource.Spec.Execution.Command, invocation.Command)
	require.Equal(t, resource.Spec.Execution.Args, invocation.Args)
	require.Equal(t, resource.Spec.Execution.Env, invocation.Env)
	require.Equal(t, resource.Spec.Execution.EnvFiles, invocation.EnvFiles)
	require.Equal(t, resource.Spec.Execution.WorkingDirectory, invocation.WorkingDirectory)
	require.NoError(t, containerOrchestrator.SimulateContainerExecCommandLogging(target.Status.ContainerID, resource.Spec.Execution.Command, apiv1.LogStreamSourceStdout, []byte("first\nsecond\n")))
	require.NoError(t, containerOrchestrator.SimulateContainerExecCommandLogging(target.Status.ContainerID, resource.Spec.Execution.Command, apiv1.LogStreamSourceStderr, []byte("error-output\n")))
	for _, source := range []string{"stdout", "stderr"} {
		logicalOutput, logicalErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Source: source})
		require.NoError(t, logicalErr)
		physicalOutput, physicalErr := readV2ContainerLog(ctx, &physical, apiv2.ResourceLogOptions{Source: source})
		require.NoError(t, physicalErr)
		require.Equal(t, logicalOutput, physicalOutput)
		if source == "stdout" {
			require.Equal(t, "first\nsecond\n", logicalOutput)
		} else {
			require.Equal(t, "error-output\n", logicalOutput)
		}
	}
	follow, followErr := openV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Follow: true})
	require.NoError(t, followErr)
	defer func() { require.NoError(t, follow.Close()) }()
	initial := make([]byte, len("first\nsecond\n"))
	_, readErr := io.ReadFull(follow, initial)
	require.NoError(t, readErr)
	require.NoError(t, containerOrchestrator.SimulateContainerExecExit(ctx, target.Status.ContainerID, resource.Spec.Execution.Command, 23))
	remaining, remainingErr := io.ReadAll(follow)
	require.NoError(t, remainingErr)
	require.Empty(t, remaining)
	exited := waitV2LogicalExecPhase(t, ctx, resource, apiv2.ContainerExecPhaseExited)
	require.Equal(t, int32(23), *exited.Status.ExitCode)
	require.False(t, exited.Status.StartedAt.IsZero())
	require.False(t, exited.Status.FinishedAt.IsZero())
	require.True(t, exited.Status.RuntimeLogs.Complete)
	requireReadyCondition(t, exited.Status.Conditions, metav1.ConditionFalse, "ExecutionExited")
	require.Equal(t, 1, containerOrchestrator.ExecContainerCallCount(resource.Spec.Execution.Command))
	waitPhysicalContainerPhase(t, ctx, types.NamespacedName{Namespace: target.Namespace, Name: target.Status.PhysicalContainerRef}, apiv2.PhysicalContainerPhaseRunning)
}

// Verifies that PhysicalContainerExec preserves completion reported before launch returns
// without launching a second command or manufacturing a running observation.
func TestV2PhysicalContainerExecImmediateCompletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	target := createV2ExecTarget(t, ctx, client, "v2-exec-immediate")
	const command = "v2-exec-immediate-command"
	containerOrchestrator.CompleteNextExecContainer(command, 0)
	resource := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: target.Status.PhysicalContainerRef, Execution: apiv2.ContainerExecConfig{Command: command}}}
	require.NoError(t, client.Create(ctx, resource))
	exited := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseExited, nil
	})
	require.NotNil(t, exited.Status.ExitCode)
	require.Zero(t, *exited.Status.ExitCode)
	require.True(t, exited.Status.LogsComplete)
	require.False(t, exited.Status.StartedAt.IsZero())
	require.False(t, exited.Status.FinishedAt.IsZero())
	require.Equal(t, 1, containerOrchestrator.ExecContainerCallCount(command))
}

// Verifies that LogicalContainerExec waits for namespace, target, and image readiness
// before creating its physical execution, then progresses through dependency watches.
func TestV2LogicalContainerExecWaitsForPrerequisites(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	const namespace = "v2-exec-prerequisites"
	resource := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: "target", Execution: apiv2.ContainerExecConfig{Command: "v2-exec-prerequisites-command"}}}
	require.NoError(t, client.Create(ctx, resource))
	waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return condition != nil && condition.Reason == string(apiv2.PhysicalResourceReasonNamespaceNotFound), nil
	})
	createActiveV2Namespace(t, ctx, namespace)
	waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return condition != nil && condition.Reason == "ContainerNotFound", nil
	})
	target := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: namespace},
		Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{ImageRef: "pending-image"}}}
	require.NoError(t, client.Create(ctx, target))
	pending := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return condition != nil && condition.Reason == "ContainerNotReady", nil
	})
	require.Empty(t, pending.Status.PhysicalExecRef)
	require.Zero(t, containerOrchestrator.ExecContainerCallCount(resource.Spec.Execution.Command))
	var executions apiv2.PhysicalContainerExecList
	require.NoError(t, client.List(ctx, &executions, ctrl_client.InNamespace(namespace)))
	require.Empty(t, executions.Items)
	createReadyV2PhysicalContainerImage(t, ctx, namespace, "pending-image", namespace+"-image")
	waitV2LogicalExecPhase(t, ctx, resource, apiv2.ContainerExecPhaseRunning)
}

// Verifies that terminal container execution launch failure remains terminal across updates,
// completes output, and forwards the physical diagnostic to the logical system stream.
func TestV2ContainerExecLaunchFailureIsTerminal(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var observer *containerExecReconcileObserver
	serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, containerExecTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
			observer = &containerExecReconcileObserver{Client: apiClient, first: make(chan struct{}), second: make(chan struct{})}
			return observer
		}})
	require.NoError(t, startupErr)
	apiClient := serverInfo.Client
	orchestrator, isSimulated := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isSimulated)
	target := createV2ExecTarget(t, ctx, apiClient, "v2-exec-failure")
	const command = "v2-exec-failure-command"
	orchestrator.FailNextExecContainer(command, errors.New("execution launch denied"))
	resource := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: command}}}
	require.NoError(t, apiClient.Create(ctx, resource))
	failed := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseFailed, nil
	})
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, "LaunchFailed")
	require.True(t, failed.Status.RuntimeLogs.Complete)
	require.Nil(t, failed.Status.ExitCode)
	for _, source := range []string{"system", "stdout"} {
		gvr := resource.GetGroupVersionResource()
		options := apiv2.ResourceLogOptions{Source: source, Follow: source == "stdout"}
		stream, streamErr := serverInfo.RestClient.Get().
			AbsPath("/apis", gvr.Group, gvr.Version, "namespaces", resource.Namespace, gvr.Resource, resource.Name, apiv2.LogSubresourceName).
			SpecificallyVersionedParams(&options, runtime.NewParameterCodec(dcpclient.NewScheme()), apiv2.GroupVersion).Stream(ctx)
		require.NoError(t, streamErr)
		output, outputErr := io.ReadAll(stream)
		require.NoError(t, stream.Close())
		require.NoError(t, outputErr)
		if source == "system" {
			require.Contains(t, string(output), "execution launch denied")
		} else {
			require.Empty(t, output)
		}
	}
	physical := &apiv2.PhysicalContainerExec{}
	physicalName := types.NamespacedName{Namespace: resource.Namespace, Name: failed.Status.PhysicalExecRef}
	require.NoError(t, apiClient.Get(ctx, physicalName, physical))
	physical.Annotations = map[string]string{containerExecReconcileStep: "first"}
	require.NoError(t, apiClient.Update(ctx, physical))
	waitContainerExecSignal(t, ctx, observer.first)
	require.NoError(t, apiClient.Get(ctx, physicalName, physical))
	physical.Annotations[containerExecReconcileStep] = "second"
	require.NoError(t, apiClient.Update(ctx, physical))
	waitContainerExecSignal(t, ctx, observer.second)
	require.NoError(t, apiClient.Get(ctx, physicalName, physical))
	require.Equal(t, apiv2.ContainerExecPhaseFailed, physical.Status.Phase)
	require.Equal(t, 1, orchestrator.ExecContainerCallCount(command))
}

// Verifies that deleting a running or in-flight LogicalContainerExec cancels its attached
// execution and removes the child before completing deletion, without stopping the container.
func TestV2ContainerExecDeletionSettlesAttachedExecution(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		name := "v2-exec-delete-running"
		if blocked {
			name = "v2-exec-delete-launch"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			target := createV2ExecTarget(t, ctx, client, name)
			command := name + "-command"
			if blocked {
				release := containerOrchestrator.BlockExecContainer(command)
				defer release()
			}
			resource := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
				Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: command}}}
			require.NoError(t, client.Create(ctx, resource))
			if blocked {
				waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
					return containerOrchestrator.ExecContainerCallCount(command) == 1, nil
				})
				require.NoError(t, waitErr)
			} else {
				waitV2LogicalExecPhase(t, ctx, resource, apiv2.ContainerExecPhaseRunning)
			}
			require.NoError(t, client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted[apiv2.LogicalContainerExec](t, ctx, client, resource)
			var executions apiv2.PhysicalContainerExecList
			require.NoError(t, client.List(ctx, &executions, ctrl_client.InNamespace(target.Namespace)))
			require.Empty(t, executions.Items)
			waitPhysicalContainerPhase(t, ctx, types.NamespacedName{Namespace: target.Namespace, Name: target.Status.PhysicalContainerRef}, apiv2.PhysicalContainerPhaseRunning)
			require.Equal(t, 1, containerOrchestrator.ExecContainerCallCount(command))
		})
	}
}

// Verifies that deleting a backing physical execution fails its logical owner without
// recreating the child or launching the command again.
func TestV2LogicalContainerExecDoesNotRecreateMissingBacking(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	target := createV2ExecTarget(t, ctx, client, "v2-exec-missing-backing")
	resource := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: "v2-exec-missing-backing-command"}}}
	require.NoError(t, client.Create(ctx, resource))
	running := waitV2LogicalExecPhase(t, ctx, resource, apiv2.ContainerExecPhaseRunning)
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: running.Status.PhysicalExecRef, Namespace: target.Namespace}}
	require.NoError(t, client.Delete(ctx, physical))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerExec](t, ctx, client, physical)
	failed := waitV2LogicalExecPhase(t, ctx, resource, apiv2.ContainerExecPhaseFailed)
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, "PhysicalExecutionChanged")
	require.NotNil(t, failed.Status.RuntimeLogs)
	require.Equal(t, running.Status.RuntimeLogs.UID, failed.Status.RuntimeLogs.UID)
	require.True(t, failed.Status.RuntimeLogs.Complete)
	require.False(t, failed.Status.RuntimeLogs.Available)
	require.Equal(t, 1, containerOrchestrator.ExecContainerCallCount(resource.Spec.Execution.Command))
}

// Verifies that namespace cleanup settles active logical and directly created physical
// executions before their container dependencies disappear.
func TestV2ContainerExecNamespaceCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	target := createV2ExecTarget(t, ctx, client, "v2-exec-namespace")
	logical := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: "v2-exec-namespace-logical"}}}
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "physical", Namespace: target.Namespace},
		Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: target.Status.PhysicalContainerRef, Execution: apiv2.ContainerExecConfig{Command: "v2-exec-namespace-physical"}}}
	require.NoError(t, client.Create(ctx, logical))
	require.NoError(t, client.Create(ctx, physical))
	waitV2LogicalExecPhase(t, ctx, logical, apiv2.ContainerExecPhaseRunning)
	waitObjectAssumesState(t, ctx, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
	})
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: target.Namespace}}
	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
	ctrl_testutil.WaitObjectDeleted[apiv2.LogicalContainerExec](t, ctx, client, logical)
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerExec](t, ctx, client, physical)
}

// Verifies that PhysicalContainerExec treats missing completion channels and closed channels
// without a result as failures rather than fabricating a successful zero exit code.
func TestV2PhysicalContainerExecRejectsIncompleteRuntimeResults(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		PhysicalContainerController|PhysicalContainerExecController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
			return &incompleteContainerExecOrchestrator{ContainerOrchestrator: orchestrator}
		}})
	require.NoError(t, startupErr)
	envClient := serverInfo.Client
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "incomplete-execution"}}
	require.NoError(t, envClient.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, envClient, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	containerID, runErr := serverInfo.ContainerOrchestrator.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{Name: "incomplete-execution-runtime", Image: "incomplete-execution-image"},
	})
	require.NoError(t, runErr)
	target := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerSpec{ContainerID: containerID}}
	require.NoError(t, envClient.Create(ctx, target))
	waitObjectAssumesStateEx(t, ctx, envClient, target.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerPhaseRunning, nil
	})
	for _, command := range []string{"nil-result", "closed-result"} {
		resource := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: command, Namespace: namespace.Name},
			Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: command}}}
		require.NoError(t, envClient.Create(ctx, resource))
		failed := waitObjectAssumesStateEx(t, ctx, envClient, resource.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
			return current.Status.Phase == apiv2.ContainerExecPhaseFailed, nil
		})
		require.Nil(t, failed.Status.ExitCode)
		require.True(t, failed.Status.LogsComplete)
		reason := apiv2.ConditionReason("LaunchFailed")
		if command == "closed-result" {
			reason = "CompletionUnknown"
		}
		requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, reason)
		require.NoError(t, envClient.Delete(ctx, resource))
		ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerExec](t, ctx, envClient, resource)
	}
	require.NoError(t, envClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, envClient, namespace)
}

// Verifies that logical and physical executions fail without a launch when the target
// container has already stopped, rather than waiting forever or restarting the container.
func TestV2ContainerExecRejectsExitedTargets(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	target := createV2ExecTarget(t, ctx, client, "v2-exec-exited-target")
	require.NoError(t, retryOnConflict(ctx, target.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	}))
	waitObjectAssumesState(t, ctx, target.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
	})
	logical := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: "v2-exec-exited-logical"}}}
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "physical", Namespace: target.Namespace},
		Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: target.Status.PhysicalContainerRef, Execution: apiv2.ContainerExecConfig{Command: "v2-exec-exited-physical"}}}
	require.NoError(t, client.Create(ctx, logical))
	require.NoError(t, client.Create(ctx, physical))
	failed := waitV2LogicalExecPhase(t, ctx, logical, apiv2.ContainerExecPhaseFailed)
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, "ContainerUnavailable")
	require.False(t, failed.Status.FinishedAt.IsZero())
	waitObjectAssumesState(t, ctx, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseFailed, nil
	})
	require.Zero(t, containerOrchestrator.ExecContainerCallCount(logical.Spec.Execution.Command))
	require.Zero(t, containerOrchestrator.ExecContainerCallCount(physical.Spec.Execution.Command))
}

// Verifies that container exit completes an active attached execution and closes its output
// without attempting to restart either the execution or its container.
func TestV2ContainerExecCompletesWhenContainerExits(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	target := createV2ExecTarget(t, ctx, client, "v2-exec-container-exit")
	resource := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: "v2-exec-container-exit-command"}}}
	require.NoError(t, client.Create(ctx, resource))
	waitV2LogicalExecPhase(t, ctx, resource, apiv2.ContainerExecPhaseRunning)
	require.NoError(t, retryOnConflict(ctx, target.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	}))
	exited := waitV2LogicalExecPhase(t, ctx, resource, apiv2.ContainerExecPhaseExited)
	require.True(t, exited.Status.RuntimeLogs.Complete)
	require.Equal(t, 1, containerOrchestrator.ExecContainerCallCount(resource.Spec.Execution.Command))
}

func createV2ExecNamespace(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, namespace string) *apiv2.Namespace {
	t.Helper()
	namespaceResource := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	require.NoError(t, apiClient.Create(ctx, namespaceResource))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cleanupCancel()
		require.NoError(t, ctrl_client.IgnoreNotFound(apiClient.Delete(cleanupCtx, namespaceResource)))
		ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, cleanupCtx, apiClient, namespaceResource)
	})
	return waitObjectAssumesStateEx(t, ctx, apiClient, namespaceResource.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
}

func createV2ExecTarget(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, namespace string) *apiv2.LogicalContainer {
	t.Helper()
	createV2ExecNamespace(t, ctx, apiClient, namespace)
	target := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: namespace},
		Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
			Image: &apiv2.PhysicalContainerImageConfig{Image: namespace + "-image"},
		}}}
	require.NoError(t, apiClient.Create(ctx, target))
	return waitObjectAssumesStateEx(t, ctx, apiClient, target.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
}

func waitV2LogicalExecPhase(t *testing.T, ctx context.Context, resource *apiv2.LogicalContainerExec, phase apiv2.ContainerExecPhase) *apiv2.LogicalContainerExec {
	t.Helper()
	return waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == phase, nil
	})
}
