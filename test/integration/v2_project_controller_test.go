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
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	internaltest "github.com/microsoft/dcp/internal/testutil"
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
	pending := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhasePending)
	requireReadyCondition(t, pending.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparing)
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
			DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"not-started"}},
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
	require.NoError(t, retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.Project) error {
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

// Verifies that Project preparation never invokes the application executable, even when execution would fail.
func TestV2ProjectPreparationDoesNotLaunchApplication(t *testing.T) {
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
		return ready != nil && ready.Reason == string(apiv2.ProjectReasonPreparing) &&
			strings.Contains(ready.Message, "LogicalContainerNetwork"), nil
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

// Verifies that an empty prepared environment survives API serialization without starving
// Project finalizer removal, allowing both Project and namespace deletion to complete.
func TestV2ProjectEmptyPreparedEnvironmentAllowsDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	server, _, startupErr := StartTestEnvironment(t, ctx, ProjectController, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-empty-environment")
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{
			LogicalProcessConfig: apiv2.LogicalProcessConfig{
				ExecutablePath:     "not-started",
				AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
				Env:                []commonapi.EnvVar{{Name: "DCP_ONLY", Value: "suppressed"}},
			},
		}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	prepared := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	requireReadyCondition(t, prepared.Status.Conditions, metav1.ConditionTrue, apiv2.ProjectReasonPrepared)
	require.Empty(t, prepared.Status.EffectiveEnv)
	require.Empty(t, prepared.Status.EffectiveArgs)
	require.NoError(t, server.Client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
	require.NoError(t, server.Client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
}

// Verifies that Project preserves case-distinct container environment names on every host and
// applies host-process casing before template evaluation, keeping published and physical inputs consistent.
func TestV2ProjectPreparedEnvironmentCasing(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
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
				ignoreCase := kind == "process" && osutil.IsWindows()
				if ignoreCase {
					expected = expected[1:]
				}
				resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
					Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir()}}
				if kind == "process" {
					resource.Spec.Process = &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{
						ExecutablePath: "not-started", Env: environment,
						AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
					}}
				} else {
					resource.Spec.Container = &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
						Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}, Env: environment,
					}}
				}
				require.NoError(t, client.Create(ctx, resource))
				observed := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
					return current.Status.Phase == apiv2.ProjectPhaseReady || current.Status.Phase == apiv2.ProjectPhaseFailed, nil
				})
				if malformed && !ignoreCase {
					require.Equal(t, apiv2.ProjectPhaseFailed, observed.Status.Phase)
					requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
					require.Empty(t, observed.Status.PhysicalContainerRef)
				} else {
					require.Equal(t, apiv2.ProjectPhaseReady, observed.Status.Phase)
					require.Equal(t, expected, observed.Status.EffectiveEnv)
					if kind == "container" {
						physical := &apiv2.PhysicalContainer{}
						require.NoError(t, client.Get(ctx, types.NamespacedName{
							Name: observed.Status.PhysicalContainerRef, Namespace: namespace.Name,
						}, physical))
						require.Equal(t, expected, physical.Spec.Container.Env)
					}
				}
			})
		}
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
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{
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
		}}}
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
	observed := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
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
	container := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
				Env: []commonapi.EnvVar{
					{Name: "PROJECT_OVERRIDDEN_TEST", Value: `{{printf "container-value"}}`},
					{Name: "DCP_PROJECT_DECLARED_TEST", Value: `{{printf "container-dcp-value"}}`},
					{Name: "DEBUG_SESSION_PROJECT_DECLARED_TEST", Value: "container-debug-value"},
					{Name: "dcp_project_declared_test", Value: "container-lowercase-value"},
				},
			},
		}}}
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
func TestV2ProjectReadyStopsWithoutApplication(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	server, environment, startupErr := StartTestEnvironment(t, ctx, ProjectController, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-session-phases")
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{
			LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "not-started", Args: []string{"prepared"}},
		}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	stopped := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseStopped)
	require.Equal(t, []string{"prepared"}, stopped.Status.EffectiveArgs)
	require.Empty(t, environment.TestProcessExecutor.FindAll([]string{"not-started"}, "", nil))
}

// Verifies that process debug prerequisites follow namespace admission independently of application
// startup, and namespace deletion reaches Project deletion without launching an adapter.
func TestV2ProjectProcessDebugPrerequisites(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	serverInfo, environment, startupErr := StartTestEnvironment(t, ctx, ProjectController, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	apiClient := serverInfo.Client
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "v2-project-debug-prerequisites",
		Finalizers: []string{"test/debug-prerequisite-cleanup"}},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(),
			Process:      &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "v2-project-pending-application"}},
			DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"v2-project-prerequisite-adapter"}}}}
	require.NoError(t, apiClient.Create(ctx, resource))
	pending := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return current.Status.Phase == apiv2.ProjectPhasePending, nil
	})
	require.Nil(t, pending.Status.Debug)
	namespace := createV2ExecNamespace(t, ctx, apiClient, resource.Namespace)
	ready := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return current.Status.Phase == apiv2.ProjectPhaseReady, nil
	})
	require.Equal(t, apiv2.ProjectPhaseReady, ready.Status.Phase)
	requireReadyCondition(t, ready.Status.Conditions, metav1.ConditionTrue, apiv2.ProjectReasonPrepared)
	require.Empty(t, environment.TestProcessExecutor.FindAll(resource.Spec.DebugAdapter.Args, "", nil))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cleanupCancel()
		require.NoError(t, ctrl_client.IgnoreNotFound(apiClient.Patch(cleanupCtx, resource, ctrl_client.RawPatch(types.MergePatchType,
			[]byte(`{"metadata":{"finalizers":[]}}`)))))
	})
	require.NoError(t, apiClient.Delete(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return !current.DeletionTimestamp.IsZero(), nil
	})
	require.NoError(t, apiClient.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"finalizers":[]}}`))))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, resource)
}

// Verifies that Project stores literal host adapter configuration independently of process/container
// execution, freezes it on update, and does not launch the adapter when the application starts or stops.
func TestV2ProjectDebugAdapterConfigurationDoesNotLaunch(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
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
			resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), DebugAdapter: adapter}}
			if kind == "process" {
				resource.Spec.Process = &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-application"}}
			} else {
				resource.Spec.Container = &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}}}
			}
			require.NoError(t, client.Create(ctx, resource))
			running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
			require.Equal(t, adapter, running.Spec.DebugAdapter)
			requireReadyCondition(t, running.Status.Conditions, metav1.ConditionTrue, apiv2.ProjectReasonPrepared)
			require.Nil(t, running.Status.Debug)
			require.Empty(t, testProcessExecutor.FindAll([]string{adapterCommand}, "", nil))
			for _, change := range []func(*apiv2.Project){
				func(changed *apiv2.Project) { changed.Spec.DebugAdapter.Args[0] = "other-adapter" },
				func(changed *apiv2.Project) { changed.Spec.DebugAdapter.Mode = apiv2.DebugAdapterModeTCPCallback },
				func(changed *apiv2.Project) { changed.Spec.DebugAdapter.Env[0].Value = "changed" },
				func(changed *apiv2.Project) { changed.Spec.DebugAdapter.ConnectionTimeout.Duration = time.Second },
				func(changed *apiv2.Project) { changed.Spec.DebugAdapter = nil },
			} {
				require.NoError(t, client.Get(ctx, resource.NamespacedName(), running))
				changed := running.DeepCopy()
				change(changed)
				require.True(t, apierrors.IsInvalid(client.Update(ctx, changed)))
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
			require.NoError(t, retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.Project) error {
				current.Spec.Stop = true
				return client.Update(updateCtx, current)
			}))
			stopped := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
			require.Equal(t, adapter, stopped.Spec.DebugAdapter)
			requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonStopped)
			require.Empty(t, testProcessExecutor.FindAll([]string{adapterCommand}, "", nil))
		})
	}
}

// Verifies that adding adapter configuration to an existing unconfigured Project is rejected
// through both update and patch, preserving the original launch declaration.
func TestV2ProjectRejectsAddingDebugAdapter(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "v2-project-debug-addition"},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Stop: true,
			Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"}}}}
	require.NoError(t, client.Create(ctx, resource))
	current := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhasePending)
	changed := current.DeepCopy()
	changed.Spec.DebugAdapter = &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}
	require.True(t, apierrors.IsInvalid(client.Update(ctx, changed)))
	patchErr := client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"spec":{"debugAdapter":{"args":["adapter"]}}}`)))
	require.True(t, apierrors.IsInvalid(patchErr), "%v", patchErr)
	require.NoError(t, client.Get(ctx, resource.NamespacedName(), current))
	require.Nil(t, current.Spec.DebugAdapter)
}

// Verifies that the real Project API accepts all declared adapter transport modes and omitted/zero
// timeout defaults, while rejecting missing commands, unsupported modes, empty environment names, and negative timeouts.
func TestV2ProjectDebugAdapterAdmission(t *testing.T) {
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
			resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: testCase.name, Namespace: "v2-project-debug-admission"},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Stop: true,
					Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"}}, DebugAdapter: testCase.config.DeepCopy()}}
			createErr := client.Create(ctx, resource)
			if !testCase.valid {
				require.True(t, apierrors.IsInvalid(createErr), "%v", createErr)
				return
			}
			require.NoError(t, createErr)
			stored := &apiv2.Project{}
			require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}, stored))
			require.Equal(t, &testCase.config, stored.Spec.DebugAdapter)
			require.Equal(t, 10*time.Second, stored.Spec.DebugAdapter.GetConnectionTimeout())
		})
	}
}

const projectTestControllers = ProjectController | PhysicalProcessController | PhysicalContainerController | PhysicalContainerImageController
