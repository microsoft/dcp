/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

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
	require.True(t, prepared.Status.LaunchInputsReady)
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
					requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, "TemplateEvaluationFailed")
					require.False(t, observed.Status.LaunchInputsReady)
					require.Empty(t, observed.Status.PhysicalContainerRef)
				} else {
					require.Equal(t, apiv2.ProjectPhaseReady, observed.Status.Phase)
					require.Equal(t, expected, observed.Status.EffectiveEnv)
					require.True(t, observed.Status.LaunchInputsReady)
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
// for processes only, honors inheritance and suppression, and freezes the prepared snapshot.
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
				},
			},
		}}}
	require.NoError(t, client.Create(ctx, resource))
	prepared := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
	require.True(t, prepared.Status.LaunchInputsReady)
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
	require.True(t, empty.Status.LaunchInputsReady)
	require.Zero(t, len(empty.Status.EffectiveEnv))
	require.Empty(t, empty.Status.EffectiveArgs)
	container := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
				Env:   []commonapi.EnvVar{{Name: "PROJECT_OVERRIDDEN_TEST", Value: `{{printf "container-value"}}`}},
			},
		}}}
	require.NoError(t, client.Create(ctx, container))
	preparedContainer := waitProjectPhase(t, ctx, client, container, apiv2.ProjectPhaseReady)
	expectedContainerEnv := []commonapi.EnvVar{{Name: "PROJECT_OVERRIDDEN_TEST", Value: "container-value"}}
	require.Equal(t, expectedContainerEnv, preparedContainer.Status.EffectiveEnv)
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Name: preparedContainer.Status.PhysicalContainerRef, Namespace: namespace.Name}, physical))
	require.Equal(t, expectedContainerEnv, physical.Spec.Container.Env)
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
	require.True(t, stopped.Status.LaunchInputsReady)
	require.Empty(t, environment.TestProcessExecutor.FindAll([]string{"not-started"}, "", nil))
}
