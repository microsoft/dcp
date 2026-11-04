/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that process debug prerequisites follow namespace admission independently of application
// startup, and clear during namespace deletion without launching an adapter.
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
			DebugAdapter: &apiv2.DebugAdapterConfig{Args: []string{"v2-project-prerequisite-adapter"}}}}
	require.NoError(t, apiClient.Create(ctx, resource))
	pending := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return current.Status.Phase == apiv2.ProjectPhasePending, nil
	})
	require.True(t, pending.Status.Debug == nil || !pending.Status.Debug.PrerequisitesReady)
	namespace := createV2ExecNamespace(t, ctx, apiClient, resource.Namespace)
	ready := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return current.Status.Debug != nil && current.Status.Debug.PrerequisitesReady && current.Status.LaunchInputsReady, nil
	})
	require.Equal(t, apiv2.ProjectPhaseReady, ready.Status.Phase)
	require.Empty(t, environment.TestProcessExecutor.FindAll(resource.Spec.DebugAdapter.Args, "", nil))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cleanupCancel()
		require.NoError(t, ctrl_client.IgnoreNotFound(apiClient.Patch(cleanupCtx, resource, ctrl_client.RawPatch(types.MergePatchType,
			[]byte(`{"metadata":{"finalizers":[]}}`)))))
	})
	require.NoError(t, apiClient.Delete(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return current.Status.Debug != nil && !current.Status.Debug.PrerequisitesReady, nil
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
			adapter := &apiv2.DebugAdapterConfig{
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
			if kind == "process" {
				require.NotNil(t, running.Status.Debug)
				require.True(t, running.Status.Debug.PrerequisitesReady)
				require.Empty(t, running.Status.Debug.Executions)
				require.Empty(t, running.Status.Debug.Sessions)
			} else {
				require.NotNil(t, running.Status.Debug)
				require.True(t, running.Status.Debug.PrerequisitesReady)
				require.Empty(t, running.Status.Debug.Executions)
			}
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
			require.True(t, stopped.Status.Debug == nil || !stopped.Status.Debug.PrerequisitesReady)
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
	changed.Spec.DebugAdapter = &apiv2.DebugAdapterConfig{Args: []string{"adapter"}}
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
		{name: "default", config: apiv2.DebugAdapterConfig{Args: []string{"adapter"}}, valid: true},
		{name: "stdio", config: apiv2.DebugAdapterConfig{Args: []string{"adapter"}, Mode: apiv2.DebugAdapterModeStdio}, valid: true},
		{name: "tcp-connect", config: apiv2.DebugAdapterConfig{Args: []string{"adapter", "{{port}}"}, Mode: apiv2.DebugAdapterModeTCPConnect}, valid: true},
		{name: "tcp-callback", config: apiv2.DebugAdapterConfig{Args: []string{"adapter", "{{port}}"}, Mode: apiv2.DebugAdapterModeTCPCallback}, valid: true},
		{name: "zero-timeout", config: apiv2.DebugAdapterConfig{Args: []string{"adapter"}, ConnectionTimeout: &metav1.Duration{}}, valid: true},
		{name: "missing-command"},
		{name: "blank-command", config: apiv2.DebugAdapterConfig{Args: []string{" "}}},
		{name: "unknown-mode", config: apiv2.DebugAdapterConfig{Args: []string{"adapter"}, Mode: "unknown"}},
		{name: "empty-environment-name", config: apiv2.DebugAdapterConfig{Args: []string{"adapter"}, Env: []commonapi.EnvVar{{Value: "value"}}}},
		{name: "negative-timeout", config: apiv2.DebugAdapterConfig{Args: []string{"adapter"}, ConnectionTimeout: &metav1.Duration{Duration: -time.Second}}},
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
