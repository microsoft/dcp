/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that logical argument/environment preparation applies last-value-wins environment
// semantics before templating, ignoring malformed values that have been overwritten.
func TestV2LogicalValuesIgnoreOverriddenEnvironment(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespace := createActiveV2Namespace(t, ctx, "v2-overridden-template-"+kind)
			name := types.NamespacedName{Name: "workload", Namespace: namespace.Name}
			environment := []commonapi.EnvVar{{Name: "VALUE", Value: "{{"}, {Name: "VALUE", Value: "effective"}}
			if kind == "process" {
				resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
					Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{
						ExecutablePath: "overridden-template-process", Env: environment,
					}}}
				require.NoError(t, client.Create(ctx, resource))
				observed := waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalProcess) (bool, error) {
					return current.Status.Phase == apiv2.LogicalProcessPhaseRunning || current.Status.Phase == apiv2.LogicalProcessPhaseFailed, nil
				})
				require.Equal(t, apiv2.LogicalProcessPhaseRunning, observed.Status.Phase, "%+v", observed.Status.Conditions)
				physical := &apiv2.PhysicalProcess{}
				require.NoError(t, client.Get(ctx, types.NamespacedName{Name: observed.Status.PhysicalProcessRef, Namespace: name.Namespace}, physical))
				require.Equal(t, []commonapi.EnvVar{{Name: "VALUE", Value: "effective"}}, physical.Spec.Process.Env)
				require.Contains(t, observed.Status.EffectiveEnv, commonapi.EnvVar{Name: "VALUE", Value: "effective"})
			} else {
				resource := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
					Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
						Image: &apiv2.PhysicalContainerImageConfig{Image: "overridden-template-image"}, Env: environment,
					}}}
				require.NoError(t, client.Create(ctx, resource))
				observed := waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalContainer) (bool, error) {
					return current.Status.Phase == apiv2.LogicalContainerPhaseRunning || current.Status.Phase == apiv2.LogicalContainerPhaseFailed, nil
				})
				require.Equal(t, apiv2.LogicalContainerPhaseRunning, observed.Status.Phase, "%+v", observed.Status.Conditions)
				physical := &apiv2.PhysicalContainer{}
				require.NoError(t, client.Get(ctx, types.NamespacedName{Name: observed.Status.PhysicalContainerRef, Namespace: name.Namespace}, physical))
				require.Equal(t, []commonapi.EnvVar{{Name: "VALUE", Value: "effective"}}, physical.Spec.Container.Env)
				require.Contains(t, physical.Status.EffectiveEnv, commonapi.EnvVar{Name: "VALUE", Value: "effective"})
				require.Equal(t, physical.Status.EffectiveEnv, observed.Status.EffectiveEnv)
			}
			require.NoError(t, client.Delete(ctx, namespace))
			ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
		})
	}
}

// Verifies that logical containers preserve case-distinct environment names on every host,
// while logical processes apply host casing rules before evaluating overwritten templates.
func TestV2LogicalValuesEnvironmentCasing(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		for _, valueKind := range []string{"literal", "malformed"} {
			t.Run(kind+"/"+valueKind, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
				defer cancel()
				namespace := createActiveV2Namespace(t, ctx, "v2-environment-casing-"+kind+"-"+valueKind)
				name := types.NamespacedName{Name: "workload", Namespace: namespace.Name}
				value := `{{printf "upper"}}`
				if valueKind == "malformed" {
					value = "{{"
				}
				environment := []commonapi.EnvVar{{Name: "VALUE", Value: value}, {Name: "value", Value: "lower"}}
				expectedEnvironment := []commonapi.EnvVar{{Name: "VALUE", Value: "upper"}, {Name: "value", Value: "lower"}}
				ignoreUppercase := kind == "process" && osutil.IsWindows()
				if ignoreUppercase {
					expectedEnvironment = expectedEnvironment[1:]
				}
				expectedFailure := valueKind == "malformed" && !ignoreUppercase
				if kind == "process" {
					resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
						Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{
							ExecutablePath: namespace.Name, Env: environment,
						}}}
					require.NoError(t, client.Create(ctx, resource))
					observed := waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalProcess) (bool, error) {
						return current.Status.Phase == apiv2.LogicalProcessPhaseRunning || current.Status.Phase == apiv2.LogicalProcessPhaseFailed, nil
					})
					if expectedFailure {
						require.Equal(t, apiv2.LogicalProcessPhaseFailed, observed.Status.Phase)
						requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, apiv2.ConditionReason("FailedToStart"))
						require.Empty(t, observed.Status.PhysicalProcessRef)
					} else {
						require.Equal(t, apiv2.LogicalProcessPhaseRunning, observed.Status.Phase, "%+v", observed.Status.Conditions)
						physical := &apiv2.PhysicalProcess{}
						require.NoError(t, client.Get(ctx, types.NamespacedName{Name: observed.Status.PhysicalProcessRef, Namespace: name.Namespace}, physical))
						require.Equal(t, expectedEnvironment, physical.Spec.Process.Env)
						evaluated := observed.Spec.DeepCopy()
						evaluated.Process.Env = expectedEnvironment
						expectedKey, _, keyErr := evaluated.GetLifecycleKey()
						require.NoError(t, keyErr)
						require.Equal(t, expectedKey, observed.Status.LifecycleKey)
					}
				} else {
					resource := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
						Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
							Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name}, Env: environment,
						}}}
					require.NoError(t, client.Create(ctx, resource))
					observed := waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalContainer) (bool, error) {
						return current.Status.Phase == apiv2.LogicalContainerPhaseRunning || current.Status.Phase == apiv2.LogicalContainerPhaseFailed, nil
					})
					if expectedFailure {
						require.Equal(t, apiv2.LogicalContainerPhaseFailed, observed.Status.Phase)
						requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, apiv2.ConditionReason("FailedToStart"))
						require.Empty(t, observed.Status.PhysicalContainerRef)
					} else {
						require.Equal(t, apiv2.LogicalContainerPhaseRunning, observed.Status.Phase, "%+v", observed.Status.Conditions)
						physical := &apiv2.PhysicalContainer{}
						require.NoError(t, client.Get(ctx, types.NamespacedName{Name: observed.Status.PhysicalContainerRef, Namespace: name.Namespace}, physical))
						require.Equal(t, expectedEnvironment, physical.Spec.Container.Env)
						evaluated := observed.Spec.DeepCopy()
						evaluated.Container.Env = expectedEnvironment
						expectedKey, _, keyErr := evaluated.GetLifecycleKey(physical.Spec.Container.Ports)
						require.NoError(t, keyErr)
						require.Equal(t, expectedKey, observed.Status.LifecycleKey)
					}
				}
				require.NoError(t, client.Delete(ctx, namespace))
				ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
			})
		}
	}
}
