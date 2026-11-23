/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// Verifies that container execution APIs require same-namespace targets and valid literal command inputs.
func TestContainerExecValidation(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		ref    string
		config ContainerExecConfig
		valid  bool
	}{
		{name: "name", ref: "target", config: ContainerExecConfig{Command: "echo"}, valid: true},
		{name: "qualified", ref: "test/target", config: ContainerExecConfig{Command: "echo"}, valid: true},
		{name: "literal", ref: "target", config: ContainerExecConfig{Command: "echo", Args: []string{"{{not-a-template}}"},
			Env: []commonapi.EnvVar{{Name: "VALUE", Value: "{{literal}}"}}, EnvFiles: []string{"/host/env"}}, valid: true},
		{name: "missing target", config: ContainerExecConfig{Command: "echo"}},
		{name: "cross namespace", ref: "elsewhere/target", config: ContainerExecConfig{Command: "echo"}},
		{name: "missing command", ref: "target"},
		{name: "blank command", ref: "target", config: ContainerExecConfig{Command: " "}},
		{name: "empty env name", ref: "target", config: ContainerExecConfig{Command: "echo", Env: []commonapi.EnvVar{{Value: "x"}}}},
		{name: "empty env file", ref: "target", config: ContainerExecConfig{Command: "echo", EnvFiles: []string{""}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			physical := &PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: "test"},
				Spec: PhysicalContainerExecSpec{ContainerRef: testCase.ref, Execution: testCase.config}}
			logical := &LogicalContainerExec{ObjectMeta: physical.ObjectMeta,
				Spec: LogicalContainerExecSpec{ContainerRef: testCase.ref, Execution: testCase.config}}
			require.Equal(t, testCase.valid, len(physical.Validate(context.Background())) == 0)
			require.Equal(t, testCase.valid, len(logical.Validate(context.Background())) == 0)
		})
	}
}

// Verifies that container execution specifications are immutable and register distinct namespaced kinds and log endpoints.
func TestContainerExecImmutabilityAndRegistration(t *testing.T) {
	t.Parallel()
	physical := &PhysicalContainerExec{Spec: PhysicalContainerExecSpec{ContainerRef: "target", Execution: ContainerExecConfig{Command: "echo"}}}
	logical := &LogicalContainerExec{Spec: LogicalContainerExecSpec{ContainerRef: "target", Execution: ContainerExecConfig{Command: "echo"}}}
	require.Empty(t, physical.ValidateUpdate(context.Background(), physical.DeepCopy()))
	require.Empty(t, logical.ValidateUpdate(context.Background(), logical.DeepCopy()))
	for _, mutate := range []func(*ContainerExecConfig){
		func(config *ContainerExecConfig) { config.Command = "other" },
		func(config *ContainerExecConfig) { config.Args = []string{"different"} },
		func(config *ContainerExecConfig) { config.Env = []commonapi.EnvVar{{Name: "NEW", Value: "value"}} },
		func(config *ContainerExecConfig) { config.EnvFiles = []string{"other-file"} },
		func(config *ContainerExecConfig) { config.WorkingDirectory = "/other" },
	} {
		changedPhysical, changedLogical := physical.DeepCopy(), logical.DeepCopy()
		mutate(&changedPhysical.Spec.Execution)
		mutate(&changedLogical.Spec.Execution)
		require.NotEmpty(t, changedPhysical.ValidateUpdate(context.Background(), physical))
		require.NotEmpty(t, changedLogical.ValidateUpdate(context.Background(), logical))
	}
	changedPhysical, changedLogical := physical.DeepCopy(), logical.DeepCopy()
	changedPhysical.Spec.ContainerRef, changedLogical.Spec.ContainerRef = "other", "other"
	require.NotEmpty(t, changedPhysical.ValidateUpdate(context.Background(), physical))
	require.NotEmpty(t, changedLogical.ValidateUpdate(context.Background(), logical))
	require.True(t, physical.NamespaceScoped())
	require.True(t, logical.NamespaceScoped())
	require.NotEqual(t, physical.GetGroupVersionResource(), logical.GetGroupVersionResource())
	require.Equal(t, LogSubresourceName, physical.GenericSubResources()[0].Name())
	require.Equal(t, LogSubresourceName, logical.GenericSubResources()[0].Name())
	require.Contains(t, PersistentTypes, &PhysicalContainerExec{})
	require.Contains(t, PersistentTypes, &LogicalContainerExec{})
}
