/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

type projectContainerEnvironmentOrchestrator struct {
	containers.ContainerOrchestrator
}

func (orchestrator *projectContainerEnvironmentOrchestrator) InspectContainers(
	ctx context.Context, options containers.InspectContainersOptions,
) ([]containers.InspectedContainer, error) {
	inspected, inspectErr := orchestrator.ContainerOrchestrator.InspectContainers(ctx, options)
	if inspectErr != nil {
		return nil, inspectErr
	}
	for index := range inspected {
		environment := map[string]string{"IMAGE_DEFAULT": "from-image", "OVERRIDE": "image-value"}
		maps.Copy(environment, inspected[index].Env)
		inspected[index].Env = environment
	}
	return inspected, nil
}

// Verifies that ProjectContainer reports the inspected environment, including image defaults
// and explicit overrides, without changing its declared inputs or losing observations on stop.
func TestV2ProjectContainerReportsObservedContainerEnvironment(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
			return &projectContainerEnvironmentOrchestrator{ContainerOrchestrator: orchestrator}
		}})
	require.NoError(t, startupErr)
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-observed-env")
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
				Env:   []commonapi.EnvVar{{Name: "OVERRIDE", Value: `{{printf "configured-value"}}`}},
			}}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	prepared := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	expected := []commonapi.EnvVar{{Name: "IMAGE_DEFAULT", Value: "from-image"}, {Name: "OVERRIDE", Value: "configured-value"}}
	require.Equal(t, expected, prepared.Status.EffectiveEnv)
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: prepared.Status.PhysicalContainerRef}, physical))
	require.Equal(t, expected, physical.Status.EffectiveEnv)
	require.Equal(t, []commonapi.EnvVar{{Name: "OVERRIDE", Value: "configured-value"}}, physical.Spec.Container.Env)
	require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"environment-projection":"repeat"}},"spec":{"stop":true}}`))))
	stopped := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseStopped)
	require.Equal(t, expected, stopped.Status.EffectiveEnv)
	require.NoError(t, server.Client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
}

// Verifies that ProjectProcess prepares a process snapshot independently of its source root,
// becomes Ready without launching it, and retains that snapshot until explicit stop/deletion.
func TestV2ProjectProcessLifecycle(t *testing.T) {
	t.Parallel()
	for _, directory := range []string{"", "relative-assets"} {
		t.Run("working-directory-"+directory, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespace := createActiveV2Namespace(t, ctx, "v2-project-process-"+directory+"case")
			resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: filepath.Join(t.TempDir(), "source-not-working-directory")}, Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-command", Args: []string{"literal"},
					WorkingDirectory: directory, AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
					Env: []commonapi.EnvVar{{Name: "VALUE", Value: "expected"}}}}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
			require.NoError(t, client.Create(ctx, resource))
			running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
			require.Equal(t, []string{"literal"}, running.Status.EffectiveArgs)
			require.Equal(t, []commonapi.EnvVar{{Name: "VALUE", Value: "expected"}}, running.Status.EffectiveEnv)
			require.Equal(t, directory, running.Spec.Process.WorkingDirectory)
			var logical apiv2.LogicalProcessList
			require.NoError(t, client.List(ctx, &logical, ctrl_client.InNamespace(namespace.Name)))
			require.Empty(t, logical.Items)
			var physical apiv2.PhysicalProcessList
			require.NoError(t, client.List(ctx, &physical, ctrl_client.InNamespace(namespace.Name)))
			require.Empty(t, physical.Items)
			executions := testProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil)
			require.Empty(t, executions)
			require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			exited := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
			require.Equal(t, running.Status.EffectiveArgs, exited.Status.EffectiveArgs)
			require.Equal(t, running.Status.EffectiveEnv, exited.Status.EffectiveEnv)
			require.NoError(t, client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
		})
	}
}

// Verifies that ProjectProcess waits for namespace readiness and that stop before preparation never launches a process.
func TestV2ProjectProcessNamespaceAndEarlyStop(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "v2-project-missing-namespace"},
		Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "v2-project-early-stop-command"}}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
	require.NoError(t, client.Create(ctx, resource))
	pending := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhasePending)
	requireReadyCondition(t, pending.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparing)
	require.NoError(t, retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.ProjectProcess) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	}))
	waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhasePending)
	createActiveV2Namespace(t, ctx, resource.Namespace)
	waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
	output, outputErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Follow: true})
	require.NoError(t, outputErr)
	require.Empty(t, output)
	require.Empty(t, testProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil))
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
}

// Verifies that ProjectContainer owns a physical container and inline image, preserves explicit mounts and run arguments,
// creates the container with its adapter as the unstarted primary process, observes the start performed by a debug session,
// shares runtime capture, retains its prepared lifecycle while paused, and removes execution on stop/deletion.
func TestV2ProjectContainerLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-project-container")
	root := t.TempDir()
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: filepath.Join(root, "source")}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"not-started"}}},
			Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
				Entrypoint: "program", Command: []string{"one"}, RunArgs: []string{"--user", "1000"}, Env: []commonapi.EnvVar{{Name: "VALUE", Value: "literal"}},
				VolumeMounts: []apiv2.VolumeMount{{Type: apiv2.BindMount, Source: filepath.Join(root, "assets"), Target: "/assets", ReadOnly: true}}},
				Ports: []apiv2.LogicalContainerPort{{ContainerPort: 8080, LogicalServiceBinding: apiv2.LogicalServiceBinding{
					Name: "http", Service: &apiv2.LogicalServiceTemplate{Name: "http"},
				}}}}}}
	require.NoError(t, client.Create(ctx, resource))
	prepared := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: prepared.Status.PhysicalContainerRef}, physical))
	require.True(t, metav1.IsControlledBy(physical, resource))
	require.False(t, physical.Spec.Container.RetainRuntimeContainer)
	require.Equal(t, resource.Spec.Container.VolumeMounts, physical.Spec.Container.VolumeMounts)
	require.Equal(t, resource.Spec.Container.RunArgs, physical.Spec.Container.RunArgs)
	require.False(t, physical.Spec.ShouldStart(), "an adapter-mode container waits for its first debug session")
	require.Equal(t, "not-started", physical.Spec.Container.Entrypoint)
	require.Empty(t, physical.Spec.Container.Command)
	require.True(t, physical.Spec.Container.OpenStdin)
	require.Equal(t, resource.Spec.Container.Command, prepared.Status.EffectiveArgs)
	require.NotEmpty(t, prepared.Status.ContainerID)
	require.True(t, prepared.Status.StartedAt.IsZero())
	running := startProjectContainerAdapter(t, ctx, client, containerOrchestrator, prepared)
	require.NotEmpty(t, running.Status.PortMappings)
	image := &apiv2.PhysicalContainerImage{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.ImageRef}, image))
	require.True(t, metav1.IsControlledBy(image, resource))
	require.Equal(t, image.GetLogSource(), running.Status.StartupLogs)
	var logical apiv2.LogicalContainerList
	require.NoError(t, client.List(ctx, &logical, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, logical.Items)
	require.NoError(t, containerOrchestrator.SimulateContainerLogging(running.Status.ContainerID, apiv1.LogStreamSourceStdout, []byte("container-project\n")))
	output, outputErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{})
	require.NoError(t, outputErr)
	require.Equal(t, "container-project\n", output)
	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, running.Status.ContainerID, containers.ContainerStatusPaused))
	paused := waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == string(apiv2.ProjectReasonPaused), nil
	})
	require.Equal(t, apiv2.ProjectPhaseReady, paused.Status.Phase)
	requireReadyCondition(t, paused.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPaused)
	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, running.Status.ContainerID, containers.ContainerStatusRunning))
	waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, running.Status.ContainerID, containers.ContainerStatusCreated))
	unknown := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseUnknown)
	require.Equal(t, running.Status.RuntimeLogs.UID, unknown.Status.RuntimeLogs.UID)
	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, running.Status.ContainerID, containers.ContainerStatusRunning))
	recovered := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	require.Equal(t, running.Status.RuntimeLogs.UID, recovered.Status.RuntimeLogs.UID)
	require.NoError(t, retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.ProjectContainer) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	}))
	stopped := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
	require.Equal(t, running.Status.ContainerID, stopped.Status.ContainerID)
	requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonStopped)
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, image)
	waitContainerMissing(t, ctx, running.Status.ContainerID)
	require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(physical.Spec.Container.ContainerName))
}

// Verifies that ProjectProcess preparation never invokes the application executable, even when execution would fail.
func TestV2ProjectProcessPreparationDoesNotLaunchApplication(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-project-process-failure")
	command := namespace.Name + "-command"
	criteria := internaltest.ProcessSearchCriteria{Command: []string{command}}
	testProcessExecutor.InstallAutoExecution(internaltest.AutoExecution{
		Condition: criteria, StartupError: func(*internaltest.ProcessExecution) error { return errors.New("project-launch-denied") },
	})
	defer testProcessExecutor.RemoveAutoExecution(criteria)
	resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: command}}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
	require.NoError(t, client.Create(ctx, resource))
	waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	require.Empty(t, testProcessExecutor.FindAll([]string{command}, "", nil))
}

// Verifies that ProjectContainer delegates terminal image failure without creating a runtime container
// and forwards image diagnostics through the ProjectContainer's system stream.
func TestV2ProjectContainerImageFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-project-image-failure")
	source := namespace.Name + "-image"
	containerOrchestrator.FailNextPullImage(source, errors.New("project-image-denied"))
	noRetries := int32(0)
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: source, PullPolicy: apiv2.PullPolicyAlways, PullRetryLimit: &noRetries}}}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, client.Create(ctx, resource))
	failed := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseFailed)
	require.Empty(t, failed.Status.ContainerID)
	drained := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		return current.Status.StartupLogs != nil && current.Status.StartupLogs.Complete, nil
	})
	require.Equal(t, apiv2.ProjectPhaseFailed, drained.Status.Phase)
	system, systemErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Source: "system"})
	require.NoError(t, systemErr)
	require.Contains(t, system, "project-image-denied")
	require.Zero(t, containerOrchestrator.CreateContainerCallCount("project-"+string(resource.UID)))
}

// Verifies that namespace cleanup deletes prepared Projects and their reusable container contexts.
func TestV2ProjectNamespaceCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-project-namespace-cleanup")
	processProject := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-command"}}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
	containerProject := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}}}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, client.Create(ctx, processProject))
	require.NoError(t, client.Create(ctx, containerProject))
	waitProjectPhase(t, ctx, client, processProject, apiv2.ProjectPhaseReady)
	runningContainer := waitProjectPhase(t, ctx, client, containerProject, apiv2.ProjectPhaseReady)
	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, processProject)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, containerProject)
	waitContainerMissing(t, ctx, runningContainer.Status.ContainerID)
	require.Empty(t, testProcessExecutor.FindAll([]string{processProject.Spec.Process.ExecutablePath}, "", nil))
}

// Verifies that ProjectContainer watches logical network references, delegates external image readiness,
// and leaves independently owned dependencies and their startup logs separate.
func TestV2ProjectContainerDependencyWatches(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-project-dependencies")
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{ImageRef: "shared-image", Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: namespace.Name + "/network", Aliases: []string{"project"}}}}}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, client.Create(ctx, resource))
	waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == string(apiv2.ProjectReasonPreparing) &&
			strings.Contains(ready.Message, "LogicalContainerNetwork"), nil
	})
	network := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerNetworkSpec{Network: &apiv2.LogicalContainerNetworkConfig{}}}
	require.NoError(t, client.Create(ctx, network))
	readyNetwork := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	pending := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		return current.Status.PhysicalContainerRef != "" && current.Status.Phase == apiv2.ProjectPhasePending, nil
	})
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: pending.Status.PhysicalContainerRef}, physical))
	require.Equal(t, []apiv2.ContainerNetworkConnectionConfig{{NetworkRef: readyNetwork.Status.PhysicalNetworkRef, Aliases: []string{"project"}}}, physical.Spec.Container.Networks)
	image := createReadyV2PhysicalContainerImage(t, ctx, namespace.Name, "shared-image", namespace.Name+"-image")
	running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	require.Equal(t, image.Name, running.Status.ImageRef)
	require.Nil(t, running.Status.StartupLogs)
	startup, startupErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Source: "startup_stdout", Follow: true})
	require.NoError(t, startupErr)
	require.Empty(t, startup)
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	require.NoError(t, client.Get(ctx, image.NamespacedName(), &apiv2.PhysicalContainerImage{}))
	require.NoError(t, client.Get(ctx, network.NamespacedName(), &apiv2.LogicalContainerNetwork{}))
}

// Verifies that ProjectProcess admission rejects reversing a stop request
// through the real API server rather than relying only on Go validation calls.
func TestV2ProjectProcessAPIValidation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "v2-project-validation"},
		Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir(), Stop: true},
			Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "program"}}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
	require.NoError(t, client.Create(ctx, resource))
	require.True(t, apierrors.IsInvalid(client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":false}}`)))))
}

func waitProjectPhase[T commonapi.ObjectStruct, PT interface {
	commonapi.PObjectStruct[T]
	apiv2.ProjectObject
}](t *testing.T, ctx context.Context, apiClient ctrl_client.Client, resource PT, phase apiv2.ProjectPhase) *T {
	t.Helper()
	return waitObjectAssumesStateEx[T, PT](t, ctx, apiClient, resource.NamespacedName(), func(current *T) (bool, error) {
		project := PT(current)
		status := project.GetProjectStatus()
		ready := apimeta.FindStatusCondition(status.Conditions, string(apiv2.ConditionReady))
		return status.Phase == phase && ready != nil && ready.ObservedGeneration == project.GetGeneration(), nil
	})
}

// startProjectContainerAdapter starts an adapter-mode Project's created container the way its first debug session does,
// then waits for the Project to observe the running container.
func startProjectContainerAdapter(
	t *testing.T, ctx context.Context, apiClient ctrl_client.Client, orchestrator *ctrl_testutil.TestContainerOrchestrator, prepared *apiv2.ProjectContainer,
) *apiv2.ProjectContainer {
	t.Helper()
	_, startErr := orchestrator.StartAttachedContainer(ctx, containers.StartAttachedContainerOptions{Container: prepared.Status.ContainerID})
	require.NoError(t, startErr)
	return waitObjectAssumesStateEx(t, ctx, apiClient, prepared.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		return current.Status.Phase == apiv2.ProjectPhaseReady && !current.Status.StartedAt.IsZero(), nil
	})
}

// removeProjectRuntimeContainer removes a Project's runtime container out of band, which its backing reports as missing.
func removeProjectRuntimeContainer(t *testing.T, ctx context.Context, orchestrator *ctrl_testutil.TestContainerOrchestrator, containerID string) {
	t.Helper()
	_, removeErr := orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{Containers: []string{containerID}, Force: true})
	require.NoError(t, removeErr)
}

// Verifies that an empty prepared environment survives API serialization without starving
// ProjectProcess finalizer removal, allowing both ProjectProcess and namespace deletion to complete.
func TestV2ProjectProcessEmptyPreparedEnvironmentAllowsDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	namespace := createV2ExecNamespace(t, ctx, client, "v2-project-empty-environment")
	resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Process: apiv2.ProjectProcessTemplate{
			LogicalProcessConfig: apiv2.LogicalProcessConfig{
				ExecutablePath:     "not-started",
				AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
				Env:                []commonapi.EnvVar{{Name: "DCP_ONLY", Value: "suppressed"}},
			},
		}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
	require.NoError(t, client.Create(ctx, resource))
	prepared := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	requireReadyCondition(t, prepared.Status.Conditions, metav1.ConditionTrue, apiv2.ProjectReasonPrepared)
	require.Empty(t, prepared.Status.EffectiveEnv)
	require.Empty(t, prepared.Status.EffectiveArgs)
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
}

// Verifies that ProjectProcess applies host environment-name casing before template evaluation,
// so a superseded case-variant value cannot cause a preparation failure.
func TestV2ProjectProcessPreparedEnvironmentCasing(t *testing.T) {
	t.Parallel()
	kind := "process"

	for _, malformed := range []bool{false, true} {
		name := kind + "-literal"
		if malformed {
			name = kind + "-malformed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createActiveV2Namespace(t, ctx, "v2-project-env-"+name)
			value := `{{printf "upper"}}`
			if malformed {
				value = "{{"
			}
			environment := []commonapi.EnvVar{{Name: "VALUE", Value: value}, {Name: "value", Value: "lower"}}
			expected := []commonapi.EnvVar{{Name: "VALUE", Value: "upper"}, {Name: "value", Value: "lower"}}
			ignoreCase := osutil.IsWindows()
			if ignoreCase {
				expected = expected[1:]
			}
			resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}

			resource.Spec.Process = apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{
				ExecutablePath: "not-started", Env: environment,
				AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
			}}

			require.NoError(t, client.Create(ctx, resource))
			observed := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
				return current.Status.Phase == apiv2.ProjectPhaseReady || current.Status.Phase == apiv2.ProjectPhaseFailed, nil
			})
			if malformed && !ignoreCase {
				require.Equal(t, apiv2.ProjectPhaseFailed, observed.Status.Phase)
				requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
			} else {
				require.Equal(t, apiv2.ProjectPhaseReady, observed.Status.Phase)
				require.Equal(t, expected, observed.Status.EffectiveEnv)

			}
		})
	}

}

// Verifies that ProjectContainer preserves case-distinct environment names on every host and
// keeps its published environment consistent with the physical container inputs.
func TestV2ProjectContainerPreparedEnvironmentCasing(t *testing.T) {
	t.Parallel()
	kind := "container"

	for _, malformed := range []bool{false, true} {
		name := kind + "-literal"
		if malformed {
			name = kind + "-malformed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createActiveV2Namespace(t, ctx, "v2-project-env-"+name)
			value := `{{printf "upper"}}`
			if malformed {
				value = "{{"
			}
			environment := []commonapi.EnvVar{{Name: "VALUE", Value: value}, {Name: "value", Value: "lower"}}
			expected := []commonapi.EnvVar{{Name: "VALUE", Value: "upper"}, {Name: "value", Value: "lower"}}
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}

			resource.Spec.Container = apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}, Env: environment,
			}}

			require.NoError(t, client.Create(ctx, resource))
			observed := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
				return current.Status.Phase == apiv2.ProjectPhaseReady || current.Status.Phase == apiv2.ProjectPhaseFailed, nil
			})
			if malformed {
				require.Equal(t, apiv2.ProjectPhaseFailed, observed.Status.Phase)
				requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
				require.Empty(t, observed.Status.PhysicalContainerRef)
			} else {
				require.Equal(t, apiv2.ProjectPhaseReady, observed.Status.Phase)
				require.Equal(t, expected, observed.Status.EffectiveEnv)

				physical := &apiv2.PhysicalContainer{}
				require.NoError(t, client.Get(ctx, types.NamespacedName{
					Name: observed.Status.PhysicalContainerRef, Namespace: namespace.Name,
				}, physical))
				require.Equal(t, expected, physical.Spec.Container.Env)

			}
		})
	}

}

// Verifies that Project evaluates declared environment before merging over literal ambient values
// for processes only, suppresses reserved names only for processes, and freezes the prepared snapshot.
func TestV2ProjectPreparedEnvironmentSnapshot(t *testing.T) {
	const inherited = "PROJECT_PREPARED_INPUT_TEST"
	t.Setenv(inherited, "initial-value")
	t.Setenv("PROJECT_OVERRIDDEN_TEST", "{{")
	t.Setenv("PROJECT_LITERAL_TEMPLATE_TEST", `{{printf "must-not-evaluate"}}`)
	t.Setenv("PROJECT_LITERAL_MALFORMED_TEST", "{{")
	t.Setenv("PROJECT_CASE_OVERRIDE_TEST", "ambient-case")
	t.Setenv("DCP_PROJECT_PREPARED_SECRET", "{{")
	t.Setenv("DEBUG_SESSION_PROJECT_TEST", "{{")
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	namespace := createActiveV2Namespace(t, ctx, "v2-project-prepared-environment")
	resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Process: apiv2.ProjectProcessTemplate{
			LogicalProcessConfig: apiv2.LogicalProcessConfig{
				ExecutablePath: "application-must-not-start", Args: []string{},
				Env: []commonapi.EnvVar{
					{Name: "PROJECT_OVERRIDDEN_TEST", Value: `{{printf "explicit-value"}}`},
					{Name: "PROJECT_DECLARED_LITERAL_TEST", Value: `{{printf "{{literal}}"}}`},
					{Name: "project_case_override_test", Value: `{{printf "declared-case"}}`},
					{Name: "DCP_PROJECT_DECLARED_TEST", Value: "explicit-process-value"},
					{Name: "DEBUG_SESSION_PROJECT_DECLARED_TEST", Value: "explicit-process-value"},
				},
			},
		}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
	require.NoError(t, client.Create(ctx, resource))
	prepared := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	require.Empty(t, prepared.Status.EffectiveArgs)
	environment := make(map[string]string)
	for _, entry := range prepared.Status.EffectiveEnv {
		environment[entry.Name] = entry.Value
	}
	require.Equal(t, "initial-value", environment[inherited])
	require.Equal(t, "explicit-value", environment["PROJECT_OVERRIDDEN_TEST"])
	require.Equal(t, "{{literal}}", environment["PROJECT_DECLARED_LITERAL_TEST"])
	require.Equal(t, `{{printf "must-not-evaluate"}}`, environment["PROJECT_LITERAL_TEMPLATE_TEST"])
	require.Equal(t, "{{", environment["PROJECT_LITERAL_MALFORMED_TEST"])
	require.Equal(t, "declared-case", environment["project_case_override_test"])
	if osutil.IsWindows() {
		require.NotContains(t, environment, "PROJECT_CASE_OVERRIDE_TEST")
	} else {
		require.Equal(t, "ambient-case", environment["PROJECT_CASE_OVERRIDE_TEST"])
	}
	require.NotContains(t, environment, "DCP_PROJECT_PREPARED_SECRET")
	require.NotContains(t, environment, "DEBUG_SESSION_PROJECT_TEST")
	require.NotContains(t, environment, "DCP_PROJECT_DECLARED_TEST")
	require.NotContains(t, environment, "DEBUG_SESSION_PROJECT_DECLARED_TEST")
	t.Setenv(inherited, "changed-after-preparation")
	require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"prepared-snapshot-probe":"changed"}}}`))))
	observed := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
		return current.Annotations["prepared-snapshot-probe"] == "changed" && current.Status.Phase == apiv2.ProjectPhaseReady, nil
	})
	require.True(t, reflect.DeepEqual(prepared.Status.EffectiveEnv, observed.Status.EffectiveEnv))
	var physicals apiv2.PhysicalProcessList
	require.NoError(t, client.List(ctx, &physicals, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, physicals.Items)
	noInheritance := resource.DeepCopy()
	noInheritance.ObjectMeta = metav1.ObjectMeta{Name: "no-inheritance", Namespace: namespace.Name}
	noInheritance.Spec.Process.AmbientEnvironment.Behavior = apiv2.EnvironmentBehaviorDoNotInherit
	noInheritance.Spec.Process.Env = nil
	require.NoError(t, client.Create(ctx, noInheritance))
	empty := waitProjectPhase(t, ctx, client, noInheritance, apiv2.ProjectPhaseReady)
	require.Zero(t, len(empty.Status.EffectiveEnv))
	require.Empty(t, empty.Status.EffectiveArgs)
	container := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
				Env: []commonapi.EnvVar{
					{Name: "PROJECT_OVERRIDDEN_TEST", Value: `{{printf "container-value"}}`},
					{Name: "DCP_PROJECT_DECLARED_TEST", Value: `{{printf "container-dcp-value"}}`},
					{Name: "DEBUG_SESSION_PROJECT_DECLARED_TEST", Value: "container-debug-value"},
					{Name: "dcp_project_declared_test", Value: "container-lowercase-value"},
				},
			},
		}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, client.Create(ctx, container))
	preparedContainer := waitProjectPhase(t, ctx, client, container, apiv2.ProjectPhaseReady)
	expectedContainerEnv := []commonapi.EnvVar{
		{Name: "PROJECT_OVERRIDDEN_TEST", Value: "container-value"},
		{Name: "DCP_PROJECT_DECLARED_TEST", Value: "container-dcp-value"},
		{Name: "DEBUG_SESSION_PROJECT_DECLARED_TEST", Value: "container-debug-value"},
		{Name: "dcp_project_declared_test", Value: "container-lowercase-value"},
	}
	require.ElementsMatch(t, expectedContainerEnv, preparedContainer.Status.EffectiveEnv)
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Name: preparedContainer.Status.PhysicalContainerRef, Namespace: namespace.Name}, physical))
	require.ElementsMatch(t, expectedContainerEnv, physical.Spec.Container.Env)
}

// Verifies that explicit stop moves Ready to Stopped without launching an application
// or discarding the prepared input snapshot.
func TestV2ProjectProcessReadyStopsWithoutApplication(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	namespace := createV2ExecNamespace(t, ctx, client, "v2-project-session-phases")
	resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Process: apiv2.ProjectProcessTemplate{
			LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-not-started", Args: []string{"prepared"}},
		}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
	require.NoError(t, client.Create(ctx, resource))
	waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	stopped := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
	require.Equal(t, []string{"prepared"}, stopped.Status.EffectiveArgs)
	require.Empty(t, testProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil))
}

// Verifies that process debug prerequisites follow namespace admission independently of application
// startup, and namespace deletion reaches ProjectProcess deletion without launching an adapter.
func TestV2ProjectProcessDebugPrerequisites(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "v2-project-debug-prerequisites",
		Finalizers: []string{"test/debug-prerequisite-cleanup"}},
		Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "v2-project-pending-application"}},
			DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"v2-project-prerequisite-adapter"}}}}
	require.NoError(t, client.Create(ctx, resource))
	releaseFinalizer := func() {
		cleanupCtx, cleanupCancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cleanupCancel()
		require.NoError(t, ctrl_client.IgnoreNotFound(client.Patch(cleanupCtx, resource, ctrl_client.RawPatch(types.MergePatchType,
			[]byte(`{"metadata":{"finalizers":[]}}`)))))
	}
	t.Cleanup(releaseFinalizer)
	pending := waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
		return current.Status.Phase == apiv2.ProjectPhasePending, nil
	})
	require.Nil(t, pending.Status.Debug)
	namespace := createV2ExecNamespace(t, ctx, client, resource.Namespace)
	// Release before namespace cleanup as well as after an early failure.
	t.Cleanup(releaseFinalizer)
	ready := waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
		return current.Status.Phase == apiv2.ProjectPhaseReady, nil
	})
	require.Equal(t, apiv2.ProjectPhaseReady, ready.Status.Phase)
	requireReadyCondition(t, ready.Status.Conditions, metav1.ConditionTrue, apiv2.ProjectReasonPrepared)
	require.Empty(t, testProcessExecutor.FindAll(resource.Spec.DebugAdapter.Args, "", nil))
	require.NoError(t, client.Delete(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
		return !current.DeletionTimestamp.IsZero(), nil
	})
	require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"finalizers":[]}}`))))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
}

// Verifies that ProjectProcess stores literal adapter configuration without launching it,
// rejects configuration changes, and preserves the declaration after stop.
func TestV2ProjectProcessDebugAdapterConfigurationDoesNotLaunch(t *testing.T) {
	t.Parallel()
	kind := "process"

	t.Run(kind, func(t *testing.T) {
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cancel()
		namespace := createActiveV2Namespace(t, ctx, "v2-project-debug-config-"+kind)
		adapterCommand := namespace.Name + "-adapter"
		adapter := &apiv2.DebugAdapterConfig{Directory: t.TempDir(),
			Args: []string{adapterCommand, "--port={{port}}", ""}, Mode: apiv2.DebugAdapterModeTCPConnect,
			Env:               []commonapi.EnvVar{{Name: "VALUE", Value: "literal-{{port}}"}},
			ConnectionTimeout: &metav1.Duration{Duration: 25 * time.Second},
		}
		resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
			Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, DebugAdapter: adapter}}

		resource.Spec.Process = apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-application"}}

		require.NoError(t, client.Create(ctx, resource))
		running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
		require.Equal(t, adapter, running.Spec.DebugAdapter)
		requireReadyCondition(t, running.Status.Conditions, metav1.ConditionTrue, apiv2.ProjectReasonPrepared)
		require.Nil(t, running.Status.Debug)
		require.Empty(t, testProcessExecutor.FindAll([]string{adapterCommand}, "", nil))
		for _, change := range []func(*apiv2.ProjectProcess){
			func(changed *apiv2.ProjectProcess) { changed.Spec.DebugAdapter.Args[0] = "other-adapter" },
			func(changed *apiv2.ProjectProcess) {
				changed.Spec.DebugAdapter.Mode = apiv2.DebugAdapterModeTCPCallback
			},
			func(changed *apiv2.ProjectProcess) { changed.Spec.DebugAdapter.Env[0].Value = "changed" },
			func(changed *apiv2.ProjectProcess) {
				changed.Spec.DebugAdapter.ConnectionTimeout.Duration = time.Second
			},
			func(changed *apiv2.ProjectProcess) { changed.Spec.DebugAdapter = nil },
		} {
			// Controller writes can race the read; only a validation failure is the outcome under test.
			updateErr := retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.ProjectProcess) error {
				change(current)
				return client.Update(updateCtx, current)
			})
			require.True(t, apierrors.IsInvalid(updateErr), "%v", updateErr)
		}
		for _, patch := range []string{
			`{"spec":{"debugAdapter":{"args":["other-adapter"]}}}`,
			`{"spec":{"debugAdapter":{"mode":"stdio"}}}`,
			`{"spec":{"debugAdapter":{"env":[{"name":"VALUE","value":"changed"}]}}}`,
			`{"spec":{"debugAdapter":{"connectionTimeout":"1s"}}}`,
			`{"spec":{"debugAdapter":null}}`,
		} {
			patchErr := client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(patch)))
			require.True(t, apierrors.IsInvalid(patchErr), "%v", patchErr)
		}
		require.NoError(t, retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.ProjectProcess) error {
			current.Spec.Stop = true
			return client.Update(updateCtx, current)
		}))
		stopped := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
		require.Equal(t, adapter, stopped.Spec.DebugAdapter)
		requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonStopped)
		require.Empty(t, testProcessExecutor.FindAll([]string{adapterCommand}, "", nil))
	})

}

// Verifies that ProjectContainer stores literal adapter configuration without launching it,
// rejects configuration changes, and preserves the declaration after stop.
func TestV2ProjectContainerDebugAdapterConfigurationDoesNotLaunch(t *testing.T) {
	t.Parallel()
	kind := "container"

	t.Run(kind, func(t *testing.T) {
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cancel()
		namespace := createActiveV2Namespace(t, ctx, "v2-project-debug-config-"+kind)
		adapterCommand := namespace.Name + "-adapter"
		adapter := &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(),
			Args: []string{adapterCommand, "--port={{port}}", ""}, Mode: apiv2.DebugAdapterModeTCPConnect,
			Env:               []commonapi.EnvVar{{Name: "VALUE", Value: "literal-{{port}}"}},
			ConnectionTimeout: &metav1.Duration{Duration: 25 * time.Second},
		}}
		resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
			Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, DebugAdapter: adapter}}

		resource.Spec.Container = apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}}}

		require.NoError(t, client.Create(ctx, resource))
		running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
		require.Equal(t, adapter, running.Spec.DebugAdapter)
		requireReadyCondition(t, running.Status.Conditions, metav1.ConditionTrue, apiv2.ProjectReasonPrepared)
		require.Nil(t, running.Status.Debug)
		require.Empty(t, testProcessExecutor.FindAll([]string{adapterCommand}, "", nil))
		for _, change := range []func(*apiv2.ProjectContainer){
			func(changed *apiv2.ProjectContainer) { changed.Spec.DebugAdapter.Args[0] = "other-adapter" },
			func(changed *apiv2.ProjectContainer) {
				changed.Spec.DebugAdapter.Mode = apiv2.DebugAdapterModeTCPCallback
			},
			func(changed *apiv2.ProjectContainer) { changed.Spec.DebugAdapter.Env[0].Value = "changed" },
			func(changed *apiv2.ProjectContainer) {
				changed.Spec.DebugAdapter.ConnectionTimeout.Duration = time.Second
			},
			func(changed *apiv2.ProjectContainer) { changed.Spec.DebugAdapter = nil },
		} {
			// Controller writes can race the read; only a validation failure is the outcome under test.
			updateErr := retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.ProjectContainer) error {
				change(current)
				return client.Update(updateCtx, current)
			})
			require.True(t, apierrors.IsInvalid(updateErr), "%v", updateErr)
		}
		for _, patch := range []string{
			`{"spec":{"debugAdapter":{"args":["other-adapter"]}}}`,
			`{"spec":{"debugAdapter":{"mode":"stdio"}}}`,
			`{"spec":{"debugAdapter":{"env":[{"name":"VALUE","value":"changed"}]}}}`,
			`{"spec":{"debugAdapter":{"connectionTimeout":"1s"}}}`,
			`{"spec":{"debugAdapter":null}}`,
		} {
			patchErr := client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(patch)))
			require.True(t, apierrors.IsInvalid(patchErr), "%v", patchErr)
		}
		require.NoError(t, retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.ProjectContainer) error {
			current.Spec.Stop = true
			return client.Update(updateCtx, current)
		}))
		stopped := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
		require.Equal(t, adapter, stopped.Spec.DebugAdapter)
		requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonStopped)
		require.Empty(t, testProcessExecutor.FindAll([]string{adapterCommand}, "", nil))
	})

}

// Verifies that ProjectProcess rejects changing or removing its debug adapter configuration
// through both update and patch, preserving the original launch declaration.
func TestV2ProjectProcessRejectsDebugAdapterChanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "v2-project-debug-change"},
		Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir(), Stop: true}, Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"}}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
	require.NoError(t, client.Create(ctx, resource))
	current := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhasePending)
	changed := current.DeepCopy()
	changed.Spec.DebugAdapter = &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"other-adapter"}}
	require.True(t, apierrors.IsInvalid(client.Update(ctx, changed)))
	removed := current.DeepCopy()
	removed.Spec.DebugAdapter = nil
	require.True(t, apierrors.IsInvalid(client.Update(ctx, removed)))
	patchErr := client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"spec":{"debugAdapter":{"args":["other-adapter"]}}}`)))
	require.True(t, apierrors.IsInvalid(patchErr), "%v", patchErr)
	require.NoError(t, client.Get(ctx, resource.NamespacedName(), current))
	require.Equal(t, resource.Spec.DebugAdapter, current.Spec.DebugAdapter)
}

// Verifies that the real ProjectProcess API accepts all declared adapter transport modes and omitted/zero
// timeout defaults, while rejecting missing commands, unsupported modes, empty environment names, and negative timeouts.
func TestV2ProjectProcessDebugAdapterAdmission(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	for _, testCase := range []struct {
		name   string
		config apiv2.DebugAdapterConfig
		valid  bool
	}{
		{name: "default", config: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}, valid: true},
		{name: "stdio", config: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Mode: apiv2.DebugAdapterModeStdio}, valid: true},
		{name: "tcp-connect", config: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter", "{{port}}"}, Mode: apiv2.DebugAdapterModeTCPConnect}, valid: true},
		{name: "tcp-callback", config: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter", "{{port}}"}, Mode: apiv2.DebugAdapterModeTCPCallback}, valid: true},
		{name: "zero-timeout", config: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, ConnectionTimeout: &metav1.Duration{}}, valid: true},
		{name: "missing-command"},
		{name: "blank-command", config: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{" "}}},
		{name: "unknown-mode", config: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Mode: "unknown"}},
		{name: "empty-environment-name", config: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Env: []commonapi.EnvVar{{Value: "value"}}}},
		{name: "negative-timeout", config: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, ConnectionTimeout: &metav1.Duration{Duration: -time.Second}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: testCase.name, Namespace: "v2-project-debug-admission"},
				Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir(), Stop: true}, Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"}}, DebugAdapter: testCase.config.DeepCopy()}}
			createErr := client.Create(ctx, resource)
			if !testCase.valid {
				require.True(t, apierrors.IsInvalid(createErr), "%v", createErr)
				return
			}
			require.NoError(t, createErr)
			stored := &apiv2.ProjectProcess{}
			require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}, stored))
			require.Equal(t, &testCase.config, stored.Spec.DebugAdapter)
			require.Equal(t, 10*time.Second, stored.Spec.DebugAdapter.GetConnectionTimeout())
		})
	}
}

const projectTestControllers = ProjectController | PhysicalProcessController | PhysicalContainerController | PhysicalContainerImageController

// Verifies that a direct ProjectProcess waits for start, launches one owned PhysicalProcess,
// publishes its service endpoint only while running, and preserves exit status and output.
func TestV2ProjectProcessDirectExecution(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	namespace := createActiveV2Namespace(t, ctx, "v2-project-direct-process")
	resource := serviceProjectProcess("project", namespace.Name, t.TempDir(),
		[]string{`--port={{portForServing "http"}}`}, []commonapi.EnvVar{{Name: "PORT", Value: `{{portForServing "http"}}`}})
	start := false
	resource.Spec.Start = &start
	resource.Spec.LaunchMode = apiv2.ProjectLaunchModeDirect
	resource.Spec.DebugAdapter = &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{namespace.Name + "-adapter"}}
	require.NoError(t, client.Create(ctx, resource))
	prepared := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	requireReadyCondition(t, prepared.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonAwaitingStart)
	require.Len(t, prepared.Status.Ports, 1)
	require.Empty(t, prepared.Status.PhysicalProcessRef)
	require.Empty(t, testProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil))
	var processes apiv2.PhysicalProcessList
	require.NoError(t, client.List(ctx, &processes, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, processes.Items)
	var endpoints apiv2.ServiceEndpointList
	require.NoError(t, client.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, endpoints.Items)

	require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"start":true}}`))))
	running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseActive)
	requireReadyCondition(t, running.Status.Conditions, metav1.ConditionTrue, apiv2.ProjectReasonApplicationRunning)
	require.NotNil(t, running.Status.PID)
	require.False(t, running.Status.IdentityTimestamp.IsZero())
	require.Equal(t, prepared.Status.EffectiveArgs, running.Status.EffectiveArgs)
	require.Equal(t, prepared.Status.EffectiveEnv, running.Status.EffectiveEnv)
	physical := &apiv2.PhysicalProcess{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalProcessRef}, physical))
	require.True(t, metav1.IsControlledBy(physical, resource))
	require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
	require.Contains(t, physical.Finalizers, apiv2.GroupName+"/physicalprocess-reconciler")
	require.Equal(t, running.Status.EffectiveArgs, physical.Spec.Process.Args)
	require.ElementsMatch(t, running.Status.EffectiveEnv, physical.Spec.Process.Env)
	require.Equal(t, apiv2.EnvironmentBehaviorDoNotInherit, physical.Spec.Process.AmbientEnvironment.Behavior)
	require.Equal(t, physical.UID, running.Status.RuntimeLogs.UID)
	executions := testProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil)
	require.Len(t, executions, 1)
	require.Equal(t, int64(executions[0].PID), *running.Status.PID)
	require.Empty(t, testProcessExecutor.FindAll(resource.Spec.DebugAdapter.Args, "", nil))
	endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(resource.UID) + "-http", Namespace: namespace.Name}}
	published := waitObjectAssumesStateEx(t, ctx, client, endpoint.NamespacedName(), func(current *apiv2.ServiceEndpoint) (bool, error) {
		return len(current.Spec.Addresses) != 0, nil
	})
	require.True(t, metav1.IsControlledBy(published, resource))
	require.Equal(t, []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: prepared.Status.Ports[0].Port}}, published.Spec.Addresses)
	_, writeErr := executions[0].Cmd.Stdout.Write([]byte("direct-process-output\n"))
	require.NoError(t, writeErr)
	testProcessExecutor.SimulateProcessExit(t, executions[0].PID, 7)
	exited := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseExited)
	requireReadyCondition(t, exited.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonCompleted)
	require.NotNil(t, exited.Status.ExitCode)
	require.Equal(t, int32(7), *exited.Status.ExitCode)
	require.False(t, exited.Status.FinishedAt.IsZero())
	require.Equal(t, running.Status.PID, exited.Status.PID)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, endpoint)
	waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
		return current.Status.RuntimeLogs != nil && current.Status.RuntimeLogs.Complete, nil
	})
	output, outputErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Follow: true})
	require.NoError(t, outputErr)
	require.Equal(t, "direct-process-output\n", output)
	require.Len(t, testProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil), 1)
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
}

// Verifies that a direct ProjectContainer defers creating its container until start is requested, then runs the
// configured command as the primary process, publishes endpoints while running, and preserves exit status and runtime output.
func TestV2ProjectContainerDirectExecution(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	namespace := createActiveV2Namespace(t, ctx, "v2-project-direct-container")
	resource := serviceProjectContainer("project", namespace.Name, t.TempDir(),
		[]string{`--port={{portForServing "http"}}`}, []commonapi.EnvVar{{Name: "VALUE", Value: "literal"}})
	start := false
	resource.Spec.Start = &start
	resource.Spec.LaunchMode = apiv2.ProjectLaunchModeDirect
	resource.Spec.Container.Entrypoint = "application"
	resource.Spec.DebugAdapter = &apiv2.ContainerDebugAdapterConfig{
		DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}},
	}
	require.NoError(t, client.Create(ctx, resource))
	prepared := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	requireReadyCondition(t, prepared.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonAwaitingStart)
	require.Empty(t, prepared.Status.PhysicalContainerRef)
	require.Empty(t, prepared.Status.ContainerID)
	var backings apiv2.PhysicalContainerList
	require.NoError(t, client.List(ctx, &backings, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, backings.Items, "the container is not created before start")
	var endpoints apiv2.ServiceEndpointList
	require.NoError(t, client.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, endpoints.Items)

	require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"start":true}}`))))
	running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseActive)
	requireReadyCondition(t, running.Status.Conditions, metav1.ConditionTrue, apiv2.ProjectReasonApplicationRunning)
	require.NotEmpty(t, running.Status.ContainerID)
	require.False(t, running.Status.StartedAt.IsZero())
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
	require.True(t, metav1.IsControlledBy(physical, resource))
	require.True(t, physical.Spec.ShouldStart())
	require.Equal(t, "application", physical.Spec.Container.Entrypoint)
	require.Equal(t, running.Status.EffectiveArgs, physical.Spec.Container.Command)
	require.Len(t, physical.Spec.Container.CopyFiles, 1)
	require.Equal(t, physical.UID, running.Status.RuntimeLogs.UID)
	endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(resource.UID) + "-http", Namespace: namespace.Name}}
	published := waitObjectAssumesStateEx(t, ctx, client, endpoint.NamespacedName(), func(current *apiv2.ServiceEndpoint) (bool, error) {
		return len(current.Spec.Addresses) != 0, nil
	})
	require.True(t, metav1.IsControlledBy(published, resource))
	require.Equal(t, prepared.Status.Ports[0].ServiceRef, published.Spec.ServiceRef)
	require.NoError(t, containerOrchestrator.SimulateContainerLogging(running.Status.ContainerID, apiv1.LogStreamSourceStdout, []byte("direct-container-output\n")))
	require.NoError(t, containerOrchestrator.SimulateContainerExit(ctx, running.Status.ContainerID, 9))
	exited := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseExited)
	requireReadyCondition(t, exited.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonCompleted)
	require.NotNil(t, exited.Status.ExitCode)
	require.Equal(t, int32(9), *exited.Status.ExitCode)
	require.False(t, exited.Status.FinishedAt.IsZero())
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, endpoint)
	output, outputErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Follow: true})
	require.NoError(t, outputErr)
	require.Equal(t, "direct-container-output\n", output)
	require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(physical.Spec.Container.ContainerName))
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
	waitContainerMissing(t, ctx, running.Status.ContainerID)
}

// Verifies that direct ProjectContainer execution waits for start, runs the configured command
// on each available real runtime, and exposes its exit code and output.
func TestV2ProjectContainerDirectExecutionWithRealOrchestrator(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 3*time.Minute)
	t.Cleanup(testCancel)
	containertest.ForEachHealthyRuntime(t, testCtx, func(t *testing.T, runtimeCtx context.Context, runtime containertest.Runtime) {
		ctx, cancel := context.WithCancel(runtimeCtx)
		defer cancel()
		tracker := containertest.NewResourceTracker(t, runtime)
		server, environment, startupErr := StartAdvancedTestEnvironmentWithOptions(ctx,
			projectTestControllers, containertest.UniqueName(t, "project-direct-environment"), t.TempDir(),
			AdvancedTestEnvironmentOptions{ApiServerFlags: ctrl_testutil.ApiServerUseTrueContainerOrchestrator, ContainerOrchestrator: runtime.Orchestrator})
		require.NoError(t, startupErr)
		defer environment.ProcessExecutor.Dispose()
		defer shutdownAdvancedTestEnvironment(t, ctx, cancel, server)

		namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: containertest.UniqueName(t, "project-direct-namespace")}}
		require.NoError(t, server.Client.Create(ctx, namespace))
		start := false
		resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
			Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{
				RootDirectory: t.TempDir(), LaunchMode: apiv2.ProjectLaunchModeDirect, Start: &start,
			}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: "busybox:latest"}, Labels: tracker.Labels(),
				Entrypoint: "sh", Command: []string{"-c", "printf 'direct-runtime-output\\n'; exit 23"},
			}}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
		require.NoError(t, server.Client.Create(ctx, resource))
		require.NoError(t, tracker.TrackContainer("project-"+string(resource.UID)))
		prepared := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
		requireReadyCondition(t, prepared.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonAwaitingStart)
		inspected, inspectErr := runtime.Orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
			Containers: []string{prepared.Status.ContainerID},
		})
		require.NoError(t, inspectErr)
		require.Len(t, inspected, 1)
		require.Equal(t, containers.ContainerStatusCreated, inspected[0].Status)

		require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"start":true}}`))))
		exited := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseExited)
		requireReadyCondition(t, exited.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonCompleted)
		require.NotNil(t, exited.Status.ExitCode)
		require.Equal(t, int32(23), *exited.Status.ExitCode)
		stream, streamErr := server.RestClient.Get().
			AbsPath("/apis", apiv2.GroupName, apiv2.Version, "namespaces", namespace.Name, "projectcontainers", resource.Name, apiv2.LogSubresourceName).
			Param("source", "stdout").Stream(ctx)
		require.NoError(t, streamErr)
		output, readErr := io.ReadAll(stream)
		require.NoError(t, stream.Close())
		require.NoError(t, readErr)
		require.Equal(t, "direct-runtime-output\n", string(output))
		require.NoError(t, server.Client.Delete(ctx, resource))
		ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
		require.NoError(t, server.Client.Delete(ctx, namespace))
		ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
	})
}

// Verifies that direct Projects start by default and that stop cleans up their owned execution
// without launching a replacement.
func TestV2ProjectDirectDefaultStartAndStop(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createActiveV2Namespace(t, ctx, "v2-project-direct-default-"+kind)
			if kind == "process" {
				resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
					Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir(), LaunchMode: apiv2.ProjectLaunchModeDirect},
						Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-command"}}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
				require.NoError(t, client.Create(ctx, resource))
				running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseActive)
				physical := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: running.Status.PhysicalProcessRef, Namespace: namespace.Name}}
				require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
				stopped := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
				require.Equal(t, running.Status.PID, stopped.Status.PID)
				ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
				require.Len(t, testProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil), 1)
				require.NoError(t, client.Delete(ctx, resource))
				ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
				return
			}
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir(), LaunchMode: apiv2.ProjectLaunchModeDirect},
					Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
						Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}, Entrypoint: "application",
					}}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
			require.NoError(t, client.Create(ctx, resource))
			running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseActive)
			physical := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: running.Status.PhysicalContainerRef, Namespace: namespace.Name}}
			require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			stopped := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
			require.Equal(t, running.Status.ContainerID, stopped.Status.ContainerID)
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
			waitContainerMissing(t, ctx, running.Status.ContainerID)
			require.NoError(t, client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
		})
	}
}

// Verifies that both Project kinds defer backing creation until start, that a started adapter-mode container is created
// without starting, and that stop before start prevents application launch in both adapter and direct modes.
func TestV2ProjectStartGateAndEarlyStop(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		for _, mode := range []apiv2.ProjectLaunchMode{apiv2.ProjectLaunchModeAdapter, apiv2.ProjectLaunchModeDirect} {
			for _, action := range []string{"start", "stop"} {
				if mode == apiv2.ProjectLaunchModeDirect && action == "start" {
					continue
				}
				t.Run(kind+"-"+string(mode)+"-"+action, func(t *testing.T) {
					t.Parallel()
					ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
					t.Cleanup(cancel)
					namespace := createActiveV2Namespace(t, ctx, "v2-project-gate-"+kind+"-"+string(mode)+"-"+action)
					start := false
					common := apiv2.ProjectCommonSpec{RootDirectory: t.TempDir(), LaunchMode: mode, Start: &start}
					patch := []byte(`{"spec":{"stop":true}}`)
					expectedPhase := apiv2.ProjectPhaseStopped
					if action == "start" {
						patch = []byte(`{"spec":{"start":true}}`)
						expectedPhase = apiv2.ProjectPhaseReady
					}
					if kind == "process" {
						resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
							Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: common,
								Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-command"}}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
						require.NoError(t, client.Create(ctx, resource))
						prepared := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
						requireReadyCondition(t, prepared.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonAwaitingStart)
						require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, patch)))
						settled := waitProjectPhase(t, ctx, client, resource, expectedPhase)
						if action == "start" {
							settled = waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
								ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
								return current.Spec.ShouldStart() && current.Status.Phase == apiv2.ProjectPhaseReady &&
									ready != nil && ready.Status == metav1.ConditionTrue && ready.Reason == string(apiv2.ProjectReasonPrepared), nil
							})
						}
						require.Empty(t, settled.Status.PhysicalProcessRef)
						require.Empty(t, testProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil))
						require.NoError(t, client.Delete(ctx, resource))
						ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
						return
					}
					resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
						Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: common,
							Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
								Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}, Entrypoint: "application",
							}}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
					require.NoError(t, client.Create(ctx, resource))
					prepared := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
					requireReadyCondition(t, prepared.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonAwaitingStart)
					require.Empty(t, prepared.Status.PhysicalContainerRef, "the container is not created before start")
					require.Empty(t, prepared.Status.ContainerID)
					require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, patch)))
					settled := waitProjectPhase(t, ctx, client, resource, expectedPhase)
					require.True(t, settled.Status.StartedAt.IsZero())
					if action == "start" {
						settled = waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
							ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
							return current.Spec.ShouldStart() && current.Status.Phase == apiv2.ProjectPhaseReady &&
								ready != nil && ready.Status == metav1.ConditionTrue && ready.Reason == string(apiv2.ProjectReasonPrepared), nil
						})
						require.True(t, settled.Status.StartedAt.IsZero(), "an adapter-mode container waits for its first debug session")
						physical := &apiv2.PhysicalContainer{}
						require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: settled.Status.PhysicalContainerRef}, physical))
						require.False(t, physical.Spec.ShouldStart())
						require.Equal(t, "adapter", physical.Spec.Container.Entrypoint)
					} else {
						var backings apiv2.PhysicalContainerList
						require.NoError(t, client.List(ctx, &backings, ctrl_client.InNamespace(namespace.Name)))
						require.Empty(t, backings.Items)
					}
					require.NoError(t, client.Delete(ctx, resource))
					ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
				})
			}
		}
	}
}

// Verifies that ProjectProcess and ProjectContainer with the same namespaced name prepare independently
// and deleting either kind leaves the other kind and its prepared execution context intact.
func TestV2ProjectKindsWithSameNameReconcileIndependently(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	namespace := createActiveV2Namespace(t, ctx, "v2-project-same-name")
	processProject := &apiv2.ProjectProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectProcessSpec{
			ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()},
			Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{
				ExecutablePath: namespace.Name + "-command", Args: []string{"process-argument"},
			}},
			DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}},
		},
	}
	containerProject := &apiv2.ProjectContainer{
		ObjectMeta: metav1.ObjectMeta{Name: processProject.Name, Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{
			ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()},
			Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}, Command: []string{"container-argument"},
			}},
			DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}},
		},
	}
	require.NoError(t, client.Create(ctx, processProject))
	require.NoError(t, client.Create(ctx, containerProject))
	preparedProcess := waitProjectPhase(t, ctx, client, processProject, apiv2.ProjectPhaseReady)
	preparedContainer := waitProjectPhase(t, ctx, client, containerProject, apiv2.ProjectPhaseReady)
	require.Equal(t, []string{"process-argument"}, preparedProcess.Status.EffectiveArgs)
	require.Equal(t, []string{"container-argument"}, preparedContainer.Status.EffectiveArgs)
	require.NotEqual(t, preparedProcess.UID, preparedContainer.UID)
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{
		Namespace: namespace.Name, Name: preparedContainer.Status.PhysicalContainerRef,
	}, physical))
	require.True(t, metav1.IsControlledBy(physical, containerProject))
	require.Equal(t, "ProjectContainer", metav1.GetControllerOf(physical).Kind)

	require.NoError(t, client.Delete(ctx, processProject))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, processProject)
	retainedContainer := waitProjectPhase(t, ctx, client, containerProject, apiv2.ProjectPhaseReady)
	require.Equal(t, preparedContainer.UID, retainedContainer.UID)
	require.Equal(t, preparedContainer.Status.ContainerID, retainedContainer.Status.ContainerID)
	require.NoError(t, client.Get(ctx, physical.NamespacedName(), physical))
	require.True(t, physical.DeletionTimestamp.IsZero())

	replacement := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{
		Name: processProject.Name, Namespace: namespace.Name,
	}, Spec: processProject.Spec}
	require.NoError(t, client.Create(ctx, replacement))
	waitProjectPhase(t, ctx, client, replacement, apiv2.ProjectPhaseReady)
	require.NoError(t, client.Delete(ctx, containerProject))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, containerProject)
	waitProjectBackingDeleted(t, ctx, client, physical)
	retainedProcess := waitProjectPhase(t, ctx, client, replacement, apiv2.ProjectPhaseReady)
	require.Equal(t, replacement.UID, retainedProcess.UID)
	require.True(t, retainedProcess.DeletionTimestamp.IsZero())
	require.Equal(t, preparedProcess.Status.EffectiveArgs, retainedProcess.Status.EffectiveArgs)
	require.Empty(t, testProcessExecutor.FindAll([]string{replacement.Spec.Process.ExecutablePath}, "", nil))
	require.NoError(t, client.Delete(ctx, replacement))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, replacement)
}

// Verifies that ProjectContainer appends a bind mount for a bound source after declared mounts, leaves image-provided
// source unmounted, expands {{sourceDirectory}} in the adapter entrypoint, and that both Project kinds publish
// their source directory in status.
func TestV2ProjectSourceDirectory(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"bind", "image"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createActiveV2Namespace(t, ctx, "v2-project-source-"+scenario)
			root := t.TempDir()
			declared := apiv2.VolumeMount{Type: apiv2.BindMount, Source: t.TempDir(), Target: "/assets", ReadOnly: true}
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: root},
					Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
						Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}, VolumeMounts: []apiv2.VolumeMount{declared},
					}},
					DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{
						Directory: t.TempDir(), Args: []string{"adapter", "--source={{sourceDirectory}}"}}},
					Source: &apiv2.ProjectContainerSource{ContainerDirectory: "/app", Bind: scenario == "bind", ReadOnly: scenario == "bind"},
				}}
			require.NoError(t, client.Create(ctx, resource))
			prepared := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
			require.Equal(t, "/app", prepared.Status.SourceDirectory)
			physical := &apiv2.PhysicalContainer{}
			require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: prepared.Status.PhysicalContainerRef}, physical))
			require.Equal(t, "adapter", physical.Spec.Container.Entrypoint)
			require.Equal(t, []string{"--source=/app"}, physical.Spec.Container.Command)
			expectedMounts := []apiv2.VolumeMount{declared}
			if scenario == "bind" {
				expectedMounts = append(expectedMounts, apiv2.VolumeMount{Type: apiv2.BindMount, Source: root, Target: "/app", ReadOnly: true})
			}
			require.Equal(t, expectedMounts, physical.Spec.Container.VolumeMounts)
			require.NoError(t, client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
		})
	}
	t.Run("process", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		t.Cleanup(cancel)
		namespace := createActiveV2Namespace(t, ctx, "v2-project-source-process")
		root := t.TempDir()
		resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
			Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: root},
				Process:      apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-command"}},
				DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
		require.NoError(t, client.Create(ctx, resource))
		prepared := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
		require.Equal(t, root, prepared.Status.SourceDirectory)
		require.NoError(t, client.Delete(ctx, resource))
		ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	})
}
