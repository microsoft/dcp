/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
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

// Verifies that container Project reports the inspected environment, including image defaults
// and explicit overrides, without changing its declared inputs or losing observations on stop.
func TestV2ProjectReportsObservedContainerEnvironment(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
			return &projectContainerEnvironmentOrchestrator{ContainerOrchestrator: orchestrator}
		}})
	require.NoError(t, startupErr)
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-observed-env")
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
				Env:   []commonapi.EnvVar{{Name: "OVERRIDE", Value: `{{printf "configured-value"}}`}},
			}}}}
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

// Verifies that Project prepares a process snapshot independently of its source root,
// becomes Ready without launching it, and retains that snapshot until explicit stop/deletion.
func TestV2ProjectProcessLifecycle(t *testing.T) {
	t.Parallel()
	for _, directory := range []string{"", "relative-assets"} {
		t.Run("working-directory-"+directory, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespace := createActiveV2Namespace(t, ctx, "v2-project-process-"+directory+"case")
			resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: filepath.Join(t.TempDir(), "source-not-working-directory"),
					Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-command", Args: []string{"literal"},
						WorkingDirectory: directory, AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
						Env: []commonapi.EnvVar{{Name: "VALUE", Value: "expected"}}}}}}
			require.NoError(t, client.Create(ctx, resource))
			running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
			require.Equal(t, []string{"literal"}, running.Status.EffectiveArgs)
			require.Equal(t, []commonapi.EnvVar{{Name: "VALUE", Value: "expected"}}, running.Status.EffectiveEnv)
			require.Equal(t, directory, running.Spec.Process.WorkingDirectory)
			require.Empty(t, running.Status.PhysicalContainerRef)
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

// Verifies that Project waits for namespace readiness and that stop before preparation never launches a process.
func TestV2ProjectNamespaceAndEarlyStop(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "v2-project-missing-namespace"},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "v2-project-early-stop-command"}}}}
	require.NoError(t, client.Create(ctx, resource))
	waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhasePending)
	require.NoError(t, retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.Project) error {
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

// Verifies that Project owns a physical container and inline image, preserves explicit mounts and run arguments,
// shares runtime capture, retains its prepared lifecycle while paused, and removes execution on stop/deletion.
func TestV2ProjectContainerLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-project-container")
	root := t.TempDir()
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: filepath.Join(root, "source"),
			DebugAdapter: &apiv2.DebugAdapterConfig{Args: []string{"not-started"}},
			Container: &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
				Entrypoint: "program", Command: []string{"one"}, RunArgs: []string{"--user", "1000"}, Env: []commonapi.EnvVar{{Name: "VALUE", Value: "literal"}},
				VolumeMounts: []apiv2.VolumeMount{{Type: apiv2.BindMount, Source: filepath.Join(root, "assets"), Target: "/assets", ReadOnly: true}}},
				Ports: []apiv2.LogicalContainerPort{{ContainerPort: 8080, LogicalServiceBinding: apiv2.LogicalServiceBinding{
					Name: "http", Service: &apiv2.LogicalServiceTemplate{Name: "http"},
				}}}}}}
	require.NoError(t, client.Create(ctx, resource))
	running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
	require.True(t, metav1.IsControlledBy(physical, resource))
	require.False(t, physical.Spec.Container.RetainRuntimeContainer)
	require.Equal(t, resource.Spec.Container.VolumeMounts, physical.Spec.Container.VolumeMounts)
	require.Equal(t, resource.Spec.Container.RunArgs, physical.Spec.Container.RunArgs)
	require.Equal(t, []string{"-f", "/dev/null"}, physical.Spec.Container.Command)
	require.Equal(t, "tail", physical.Spec.Container.Entrypoint)
	require.Equal(t, resource.Spec.Container.Command, running.Status.EffectiveArgs)
	require.NotEmpty(t, running.Status.ContainerID)
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
	paused := waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == string(apiv2.PhysicalContainerReasonRuntimeContainerPaused), nil
	})
	require.Equal(t, apiv2.ProjectPhaseReady, paused.Status.Phase)
	requireReadyCondition(t, paused.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerPaused)
	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, running.Status.ContainerID, containers.ContainerStatusRunning))
	waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, running.Status.ContainerID, containers.ContainerStatusCreated))
	unknown := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseUnknown)
	require.Equal(t, running.Status.RuntimeLogs.UID, unknown.Status.RuntimeLogs.UID)
	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, running.Status.ContainerID, containers.ContainerStatusRunning))
	recovered := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	require.Equal(t, running.Status.RuntimeLogs.UID, recovered.Status.RuntimeLogs.UID)
	require.NoError(t, retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.Project) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	}))
	stopped := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
	require.Equal(t, running.Status.ContainerID, stopped.Status.ContainerID)
	requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, "ProjectStopped")
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, image)
	waitContainerMissing(t, ctx, running.Status.ContainerID)
	require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(physical.Spec.Container.ContainerName))
}

// Verifies that Project preparation never invokes the application executable, even when execution would fail.
func TestV2ProjectPreparationDoesNotLaunchApplication(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-project-process-failure")
	command := namespace.Name + "-command"
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{command}}
	testProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria, StartupError: func(*internal_testutil.ProcessExecution) error { return errors.New("project-launch-denied") },
	})
	defer testProcessExecutor.RemoveAutoExecution(criteria)
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: command}}}}
	require.NoError(t, client.Create(ctx, resource))
	waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	require.Empty(t, testProcessExecutor.FindAll([]string{command}, "", nil))
}

// Verifies that Project delegates terminal image failure without creating a runtime container
// and forwards image diagnostics through the Project's system stream.
func TestV2ProjectImageFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-project-image-failure")
	source := namespace.Name + "-image"
	containerOrchestrator.FailNextPullImage(source, errors.New("project-image-denied"))
	noRetries := int32(0)
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: source, PullPolicy: apiv2.PullPolicyAlways, PullRetryLimit: &noRetries}}}}}
	require.NoError(t, client.Create(ctx, resource))
	failed := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseFailed)
	require.Empty(t, failed.Status.ContainerID)
	require.True(t, failed.Status.StartupLogs.Complete)
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
	processProject := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-command"}}}}
	containerProject := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}}}}}
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

// Verifies that Project watches logical network references, delegates external image readiness,
// and leaves independently owned dependencies and their startup logs separate.
func TestV2ProjectContainerDependencyWatches(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-project-dependencies")
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{ImageRef: "shared-image", Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: namespace.Name + "/network", Aliases: []string{"project"}}}}}}}
	require.NoError(t, client.Create(ctx, resource))
	waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "NetworkNotReady", nil
	})
	network := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerNetworkSpec{Network: &apiv2.LogicalContainerNetworkConfig{}}}
	require.NoError(t, client.Create(ctx, network))
	readyNetwork := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	pending := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
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

// Verifies that Project admission rejects incompatible templates and immutable launch changes
// through the real API server rather than relying only on Go validation calls.
func TestV2ProjectAPIValidation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "v2-project-validation"},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir()}}
	require.True(t, apierrors.IsInvalid(client.Create(ctx, resource)))
	resource.Spec.Process = &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "program"}}
	resource.Spec.Container = &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{ImageRef: "image"}}
	require.True(t, apierrors.IsInvalid(client.Create(ctx, resource)))
	resource.Spec.Container = nil
	resource.Spec.Stop = true
	require.NoError(t, client.Create(ctx, resource))
	require.True(t, apierrors.IsInvalid(client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":false}}`)))))
}

func waitProjectPhase(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, resource *apiv2.Project, phase apiv2.ProjectPhase) *apiv2.Project {
	t.Helper()
	return waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == phase && ready != nil && ready.ObservedGeneration == current.Generation, nil
	})
}
