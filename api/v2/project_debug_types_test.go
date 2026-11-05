/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// Verifies that Project validates the adapter command vector, transport mode, environment names,
// and connection timeout without restricting valid literal arguments or executable names.
func TestProjectDebugAdapterValidation(t *testing.T) {
	root := t.TempDir()
	for _, testCase := range []struct {
		name      string
		config    DebugAdapterConfig
		field     string
		container bool
	}{
		{name: "explicit host", config: DebugAdapterConfig{Args: []string{"adapter"}, Placement: DebugAdapterPlacementHost}},
		{name: "container stdio", container: true, config: DebugAdapterConfig{Args: []string{"adapter"}, Placement: DebugAdapterPlacementContainer}},
		{name: "container requires context", config: DebugAdapterConfig{Args: []string{"adapter"}, Placement: DebugAdapterPlacementContainer}, field: "spec.debugAdapter.placement"},
		{name: "container tcp deferred", container: true, config: DebugAdapterConfig{Args: []string{"adapter"}, Placement: DebugAdapterPlacementContainer, Mode: DebugAdapterModeTCPConnect}, field: "spec.debugAdapter.mode"},
		{name: "unknown placement", config: DebugAdapterConfig{Args: []string{"adapter"}, Placement: "unknown"}, field: "spec.debugAdapter.placement"},
		{name: "default stdio", config: DebugAdapterConfig{Args: []string{"adapter"}}},
		{name: "explicit stdio", config: DebugAdapterConfig{Args: []string{"adapter"}, Mode: DebugAdapterModeStdio}},
		{name: "tcp connect", config: DebugAdapterConfig{Args: []string{"adapter", "--port={{port}}"}, Mode: DebugAdapterModeTCPConnect}},
		{name: "tcp callback", config: DebugAdapterConfig{Args: []string{"adapter", "--connect", "127.0.0.1:{{port}}"}, Mode: DebugAdapterModeTCPCallback}},
		{name: "empty argument", config: DebugAdapterConfig{Args: []string{"adapter", ""}}},
		{name: "environment", config: DebugAdapterConfig{Args: []string{"adapter"}, Env: []commonapi.EnvVar{{Name: "VALUE", Value: ""}}}},
		{name: "zero timeout", config: DebugAdapterConfig{Args: []string{"adapter"}, ConnectionTimeout: &metav1.Duration{}}},
		{name: "explicit timeout", config: DebugAdapterConfig{Args: []string{"adapter"}, ConnectionTimeout: &metav1.Duration{Duration: 30 * time.Second}}},
		{name: "missing executable", field: "spec.debugAdapter.args"},
		{name: "blank executable", config: DebugAdapterConfig{Args: []string{" "}}, field: "spec.debugAdapter.args[0]"},
		{name: "unknown mode", config: DebugAdapterConfig{Args: []string{"adapter"}, Mode: "unknown"}, field: "spec.debugAdapter.mode"},
		{name: "missing env name", config: DebugAdapterConfig{Args: []string{"adapter"}, Env: []commonapi.EnvVar{{Value: "value"}}}, field: "spec.debugAdapter.env[0].name"},
		{name: "negative timeout", config: DebugAdapterConfig{Args: []string{"adapter"}, ConnectionTimeout: &metav1.Duration{Duration: -time.Second}}, field: "spec.debugAdapter.connectionTimeout"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			project := &Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
				Spec: ProjectSpec{RootDirectory: root, Process: &ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "application"}}, DebugAdapter: &testCase.config}}
			if testCase.container {
				project.Spec.Process = nil
				project.Spec.Container = &ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}}
			}
			validationErrors := project.Validate(context.Background())
			if testCase.field == "" {
				require.Empty(t, validationErrors)
			} else {
				require.Len(t, validationErrors, 1)
				require.Equal(t, testCase.field, validationErrors[0].Field)
			}
		})
	}
}

// Verifies that DebugAdapterConfig defaults omitted mode and timeout, preserves explicit values,
// and does not silently normalize invalid transport names or mutate the declared configuration.
func TestDebugAdapterConfigurationDefaults(t *testing.T) {
	config := DebugAdapterConfig{Args: []string{"adapter"}}
	require.Equal(t, DebugAdapterModeStdio, config.EffectiveMode())
	require.Equal(t, 10*time.Second, config.GetConnectionTimeout())
	require.Empty(t, config.Mode)
	require.Equal(t, DebugAdapterPlacementHost, config.EffectivePlacement())
	require.Empty(t, config.Placement)
	require.Nil(t, config.ConnectionTimeout)
	config.ConnectionTimeout = &metav1.Duration{}
	require.Equal(t, 10*time.Second, config.GetConnectionTimeout())
	config.ConnectionTimeout.Duration = 25 * time.Second
	require.Equal(t, 25*time.Second, config.GetConnectionTimeout())
	for _, mode := range []DebugAdapterMode{DebugAdapterModeStdio, DebugAdapterModeTCPConnect, DebugAdapterModeTCPCallback, "invalid"} {
		config.Mode = mode
		require.Equal(t, mode, config.EffectiveMode())
	}
}

// Verifies that Project deep copies adapter arguments, environment, and timeout, and freezes
// adapter configuration while permitting the existing one-way stop request.
func TestProjectDebugAdapterCopyAndImmutability(t *testing.T) {
	project := &Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
		Spec: ProjectSpec{RootDirectory: t.TempDir(), Process: &ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "application"}},
			DebugAdapter: &DebugAdapterConfig{Args: []string{"adapter", "--port={{port}}"}, Mode: DebugAdapterModeTCPConnect,
				Env: []commonapi.EnvVar{{Name: "VALUE", Value: "original"}}, ConnectionTimeout: &metav1.Duration{Duration: 20 * time.Second}}}}
	for _, change := range []func(*Project){
		func(changed *Project) { changed.Spec.DebugAdapter.Placement = DebugAdapterPlacementContainer },
		func(changed *Project) { changed.Spec.DebugAdapter.Args[0] = "other" },
		func(changed *Project) { changed.Spec.DebugAdapter.Mode = DebugAdapterModeStdio },
		func(changed *Project) { changed.Spec.DebugAdapter.Env[0].Value = "other" },
		func(changed *Project) { changed.Spec.DebugAdapter.ConnectionTimeout.Duration = time.Second },
		func(changed *Project) { changed.Spec.DebugAdapter = nil },
	} {
		changed := project.DeepCopy()
		change(changed)
		require.NotEmpty(t, changed.ValidateUpdate(context.Background(), project))
	}
	require.Equal(t, "adapter", project.Spec.DebugAdapter.Args[0])
	require.Equal(t, DebugAdapterModeTCPConnect, project.Spec.DebugAdapter.Mode)
	require.Equal(t, "original", project.Spec.DebugAdapter.Env[0].Value)
	require.Equal(t, 20*time.Second, project.Spec.DebugAdapter.ConnectionTimeout.Duration)
	stopped := project.DeepCopy()
	stopped.Spec.Stop = true
	require.Empty(t, stopped.ValidateUpdate(context.Background(), project))
	unconfigured := project.DeepCopy()
	unconfigured.Spec.DebugAdapter = nil
	require.NotEmpty(t, project.ValidateUpdate(context.Background(), unconfigured))
}
