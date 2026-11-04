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
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that Pending prepares one full effective environment, Ready publishes that snapshot
// without launching the application, and subsequent host-environment changes cannot alter it.
func TestV2ProjectPreparedEnvironmentSnapshot(t *testing.T) {
	const inherited = "PROJECT_PREPARED_INPUT_TEST"
	t.Setenv(inherited, "initial-value")
	t.Setenv("PROJECT_OVERRIDDEN_TEST", "{{")
	t.Setenv("DCP_PROJECT_PREPARED_SECRET", "not-for-application")
	t.Setenv("DEBUG_SESSION_PROJECT_TEST", "not-for-application")
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	namespace := createActiveV2Namespace(t, ctx, "v2-project-prepared-environment")
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{
			LogicalProcessConfig: apiv2.LogicalProcessConfig{
				ExecutablePath: "application-must-not-start", Args: []string{},
				Env: []commonapi.EnvVar{{Name: "PROJECT_OVERRIDDEN_TEST", Value: "explicit-value"}},
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
