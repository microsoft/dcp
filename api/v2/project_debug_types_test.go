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
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// Verifies that adapter-directory substitution selects the host directory or container installation
// directory, preserves unrelated placeholders, and supports interpreter-based command vectors.
func TestDebugAdapterDirectoryArguments(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	config := DebugAdapterConfig{Directory: directory, Args: []string{"node", "{{adapterDirectory}}/out/adapter.js", "--port={{port}}"}}
	require.Equal(t, []string{"node", directory + "/out/adapter.js", "--port={{port}}"}, config.GetArgs())
	containerConfig := ContainerDebugAdapterConfig{DebugAdapterConfig: config}
	require.Equal(t, []string{"node", "/opt/dcp/debug-adapter/out/adapter.js", "--port={{port}}"}, containerConfig.GetArgs())
	containerConfig.ContainerDirectory = "/custom"
	require.Equal(t, []string{"node", "/custom/out/adapter.js", "--port={{port}}"}, containerConfig.GetArgs())
	require.Equal(t, "{{adapterDirectory}}/out/adapter.js", config.Args[1])
}

// Verifies that ProjectProcess validates the host adapter command vector, transport mode, environment names,
// and connection timeout without restricting valid literal arguments or executable names.
func TestProjectProcessDebugAdapterValidation(t *testing.T) {
	root := t.TempDir()
	for _, testCase := range []struct {
		name   string
		config DebugAdapterConfig
		field  string
	}{
		{name: "explicit host", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}},
		{name: "missing directory", config: DebugAdapterConfig{Args: []string{"adapter"}}, field: "spec.debugAdapter.directory"},
		{name: "relative directory", config: DebugAdapterConfig{Directory: "relative", Args: []string{"adapter"}}, field: "spec.debugAdapter.directory"},
		{name: "explicit stdio", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Mode: DebugAdapterModeStdio}},
		{name: "tcp connect", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter", "--port={{port}}"}, Mode: DebugAdapterModeTCPConnect}},
		{name: "tcp callback", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter", "--connect", "127.0.0.1:{{port}}"}, Mode: DebugAdapterModeTCPCallback}},
		{name: "empty argument", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter", ""}}},
		{name: "environment", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Env: []commonapi.EnvVar{{Name: "VALUE", Value: ""}}}},
		{name: "zero timeout", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, ConnectionTimeout: &metav1.Duration{}}},
		{name: "explicit timeout", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, ConnectionTimeout: &metav1.Duration{Duration: 30 * time.Second}}},
		{name: "missing executable", config: DebugAdapterConfig{Directory: root}, field: "spec.debugAdapter.args"},
		{name: "blank executable", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{" "}}, field: "spec.debugAdapter.args[0]"},
		{name: "unknown mode", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Mode: "unknown"}, field: "spec.debugAdapter.mode"},
		{name: "missing env name", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Env: []commonapi.EnvVar{{Value: "value"}}}, field: "spec.debugAdapter.env[0].name"},
		{name: "negative timeout", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, ConnectionTimeout: &metav1.Duration{Duration: -time.Second}}, field: "spec.debugAdapter.connectionTimeout"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			project := &ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
				Spec: ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root},
					Process: ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "application"}}, DebugAdapter: &testCase.config}}
			requireSingleFieldError(t, project.Validate(context.Background()), testCase.field)
		})
	}
}

// Verifies that ProjectContainer validates container adapter installation, executable files,
// and TCP listener options, and rejects host-only callback transport.
func TestProjectContainerDebugAdapterValidation(t *testing.T) {
	root := t.TempDir()
	base := func() DebugAdapterConfig {
		return DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}
	}
	withMode := func(mode DebugAdapterMode) DebugAdapterConfig {
		config := base()
		config.Mode = mode
		return config
	}
	for _, testCase := range []struct {
		name   string
		config ContainerDebugAdapterConfig
		field  string
	}{
		{name: "stdio", config: ContainerDebugAdapterConfig{DebugAdapterConfig: base()}},
		{name: "executables", config: ContainerDebugAdapterConfig{DebugAdapterConfig: base(), ExecutableFiles: []string{"bin/adapter"}}},
		{name: "escaping executable", config: ContainerDebugAdapterConfig{DebugAdapterConfig: base(), ExecutableFiles: []string{"../adapter"}}, field: "spec.debugAdapter.executableFiles[0]"},
		{name: "tcp default", config: ContainerDebugAdapterConfig{DebugAdapterConfig: withMode(DebugAdapterModeTCPConnect)}},
		{name: "tcp explicit", config: ContainerDebugAdapterConfig{DebugAdapterConfig: withMode(DebugAdapterModeTCPConnect), TCP: &DebugAdapterTCPConfig{Port: 5678}}},
		{name: "callback deferred", config: ContainerDebugAdapterConfig{DebugAdapterConfig: withMode(DebugAdapterModeTCPCallback)}, field: "spec.debugAdapter.mode"},
		{name: "negative tcp port", config: ContainerDebugAdapterConfig{DebugAdapterConfig: withMode(DebugAdapterModeTCPConnect), TCP: &DebugAdapterTCPConfig{Port: -1}}, field: "spec.debugAdapter.tcp.port"},
		{name: "excessive tcp port", config: ContainerDebugAdapterConfig{DebugAdapterConfig: withMode(DebugAdapterModeTCPConnect), TCP: &DebugAdapterTCPConfig{Port: 65536}}, field: "spec.debugAdapter.tcp.port"},
		{name: "stdio tcp options unsupported", config: ContainerDebugAdapterConfig{DebugAdapterConfig: base(), TCP: &DebugAdapterTCPConfig{}}, field: "spec.debugAdapter.tcp"},
		{name: "relative container directory", config: ContainerDebugAdapterConfig{DebugAdapterConfig: DebugAdapterConfig{Directory: root, Args: []string{"adapter"}}, ContainerDirectory: "relative"}, field: "spec.debugAdapter.containerDirectory"},
		{name: "host directory required", config: ContainerDebugAdapterConfig{DebugAdapterConfig: DebugAdapterConfig{Args: []string{"adapter"}}}, field: "spec.debugAdapter.directory"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			project := &ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
				Spec: ProjectContainerSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root},
					Container: ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}}, DebugAdapter: &testCase.config}}
			requireSingleFieldError(t, project.Validate(context.Background()), testCase.field)
		})
	}
}

func requireSingleFieldError(t *testing.T, validationErrors field.ErrorList, fieldPath string) {
	if fieldPath == "" {
		require.Empty(t, validationErrors)
	} else {
		require.Len(t, validationErrors, 1)
		require.Equal(t, fieldPath, validationErrors[0].Field)
	}
}

// Verifies that DebugAdapterConfig defaults omitted mode and timeout, preserves explicit values,
// and does not silently normalize invalid transport names or mutate the declared configuration.
func TestDebugAdapterConfigurationDefaults(t *testing.T) {
	config := DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}
	require.Equal(t, DebugAdapterModeStdio, config.EffectiveMode())
	require.Equal(t, 10*time.Second, config.GetConnectionTimeout())
	require.Empty(t, config.Mode)
	require.Zero(t, ContainerDebugAdapterConfig{DebugAdapterConfig: config}.GetTCPPort())
	containerTCP := ContainerDebugAdapterConfig{DebugAdapterConfig: DebugAdapterConfig{Directory: t.TempDir(), Mode: DebugAdapterModeTCPConnect}}
	require.Equal(t, int32(4711), containerTCP.GetTCPPort())
	containerTCP.TCP = &DebugAdapterTCPConfig{}
	require.Equal(t, int32(4711), containerTCP.GetTCPPort())
	containerTCP.TCP.Port = 5678
	require.Equal(t, int32(5678), containerTCP.GetTCPPort())
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

// Verifies that ProjectProcess deep copies adapter arguments, environment, and timeout, and freezes
// adapter configuration while permitting the existing one-way stop request.
func TestProjectDebugAdapterCopyAndImmutability(t *testing.T) {
	project := &ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
		Spec: ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: t.TempDir()},
			Process: ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "application"}},
			DebugAdapter: &DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter", "--port={{port}}"}, Mode: DebugAdapterModeTCPConnect,
				Env: []commonapi.EnvVar{{Name: "VALUE", Value: "original"}}, ConnectionTimeout: &metav1.Duration{Duration: 20 * time.Second}}}}
	for _, change := range []func(*ProjectProcess){
		func(changed *ProjectProcess) { changed.Spec.DebugAdapter.Directory = t.TempDir() },
		func(changed *ProjectProcess) { changed.Spec.DebugAdapter.Args[0] = "other" },
		func(changed *ProjectProcess) { changed.Spec.DebugAdapter.Mode = DebugAdapterModeStdio },
		func(changed *ProjectProcess) { changed.Spec.DebugAdapter.Env[0].Value = "other" },
		func(changed *ProjectProcess) { changed.Spec.DebugAdapter.ConnectionTimeout.Duration = time.Second },
		func(changed *ProjectProcess) { changed.Spec.DebugAdapter = nil },
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

// Verifies that ProjectContainer adapter installation and TCP options are deep-copied and immutable after creation.
func TestProjectContainerDebugAdapterImmutability(t *testing.T) {
	t.Parallel()
	project := &ProjectContainer{Spec: ProjectContainerSpec{DebugAdapter: &ContainerDebugAdapterConfig{
		DebugAdapterConfig: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter", "{{port}}"}, Mode: DebugAdapterModeTCPConnect},
		TCP:                &DebugAdapterTCPConfig{Port: 5678},
	}}}
	copied := project.DeepCopy()
	copied.Spec.DebugAdapter.TCP.Port = 6789
	require.Equal(t, int32(5678), project.Spec.DebugAdapter.TCP.Port)
	require.NotEmpty(t, copied.ValidateUpdate(context.Background(), project))
	copied.Spec.DebugAdapter.TCP = nil
	require.NotEmpty(t, copied.ValidateUpdate(context.Background(), project))
	directoryChanged := project.DeepCopy()
	directoryChanged.Spec.DebugAdapter.ContainerDirectory = "/custom"
	require.NotEmpty(t, directoryChanged.ValidateUpdate(context.Background(), project))
}
