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

// Verifies that adapter-directory substitution selects the host directory or container installation
// directory, preserves unrelated placeholders, and supports interpreter-based command vectors.
func TestDebugAdapterDirectoryArguments(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	config := DebugAdapterConfig{Directory: directory, Args: []string{"node", "{{adapterDirectory}}/out/adapter.js", "--port={{port}}"}}
	require.Equal(t, []string{"node", directory + "/out/adapter.js", "--port={{port}}"}, config.GetArgs(false))
	require.Equal(t, []string{"node", "/opt/dcp/debug-adapter/out/adapter.js", "--port={{port}}"}, config.GetArgs(true))
	config.ContainerDirectory = "/custom"
	require.Equal(t, []string{"node", "/custom/out/adapter.js", "--port={{port}}"}, config.GetArgs(true))
	require.Equal(t, "{{adapterDirectory}}/out/adapter.js", config.Args[1])
}

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
		{name: "explicit host", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}},
		{name: "container stdio", container: true, config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}},
		{name: "container executables", container: true, config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, ExecutableFiles: []string{"bin/adapter"}}},
		{name: "host executables forbidden", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, ExecutableFiles: []string{"bin/adapter"}}, field: "spec.debugAdapter.executableFiles"},
		{name: "escaping executable", container: true, config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, ExecutableFiles: []string{"../adapter"}}, field: "spec.debugAdapter.executableFiles[0]"},
		{name: "container directory requires context", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, ContainerDirectory: "/adapter"}, field: "spec.debugAdapter.containerDirectory"},
		{name: "container tcp default", container: true, config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Mode: DebugAdapterModeTCPConnect}},
		{name: "container tcp explicit", container: true, config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Mode: DebugAdapterModeTCPConnect, TCP: &DebugAdapterTCPConfig{Port: 5678}}},
		{name: "container callback deferred", container: true, config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Mode: DebugAdapterModeTCPCallback}, field: "spec.debugAdapter.mode"},
		{name: "negative tcp port", container: true, config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Mode: DebugAdapterModeTCPConnect, TCP: &DebugAdapterTCPConfig{Port: -1}}, field: "spec.debugAdapter.tcp.port"},
		{name: "excessive tcp port", container: true, config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Mode: DebugAdapterModeTCPConnect, TCP: &DebugAdapterTCPConfig{Port: 65536}}, field: "spec.debugAdapter.tcp.port"},
		{name: "host tcp options unsupported", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, Mode: DebugAdapterModeTCPConnect, TCP: &DebugAdapterTCPConfig{}}, field: "spec.debugAdapter.tcp"},
		{name: "stdio tcp options unsupported", container: true, config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}, TCP: &DebugAdapterTCPConfig{}}, field: "spec.debugAdapter.tcp"},
		{name: "missing directory", config: DebugAdapterConfig{Args: []string{"adapter"}}, field: "spec.debugAdapter.directory"},
		{name: "relative directory", config: DebugAdapterConfig{Directory: "relative", Args: []string{"adapter"}}, field: "spec.debugAdapter.directory"},
		{name: "relative container directory", container: true, config: DebugAdapterConfig{Directory: root, ContainerDirectory: "relative", Args: []string{"adapter"}}, field: "spec.debugAdapter.containerDirectory"},
		{name: "default stdio", config: DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}},
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
	config := DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}
	require.Equal(t, DebugAdapterModeStdio, config.EffectiveMode())
	require.Equal(t, 10*time.Second, config.GetConnectionTimeout())
	require.Empty(t, config.Mode)
	require.Zero(t, config.GetContainerTCPPort(false))
	containerTCP := DebugAdapterConfig{Directory: t.TempDir(), Mode: DebugAdapterModeTCPConnect}
	require.Equal(t, int32(4711), containerTCP.GetContainerTCPPort(true))
	containerTCP.TCP = &DebugAdapterTCPConfig{}
	require.Equal(t, int32(4711), containerTCP.GetContainerTCPPort(true))
	containerTCP.TCP.Port = 5678
	require.Equal(t, int32(5678), containerTCP.GetContainerTCPPort(true))
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
			DebugAdapter: &DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter", "--port={{port}}"}, Mode: DebugAdapterModeTCPConnect,
				Env: []commonapi.EnvVar{{Name: "VALUE", Value: "original"}}, ConnectionTimeout: &metav1.Duration{Duration: 20 * time.Second}}}}
	for _, change := range []func(*Project){
		func(changed *Project) { changed.Spec.DebugAdapter.Directory = t.TempDir() },
		func(changed *Project) { changed.Spec.DebugAdapter.ContainerDirectory = "/custom" },
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

// Verifies that container TCP options are deep-copied and immutable after Project creation.
func TestProjectDebugAdapterTCPImmutability(t *testing.T) {
	t.Parallel()
	project := &Project{Spec: ProjectSpec{DebugAdapter: &DebugAdapterConfig{Directory: t.TempDir(),
		Args: []string{"adapter", "{{port}}"}, Mode: DebugAdapterModeTCPConnect,
		TCP: &DebugAdapterTCPConfig{Port: 5678},
	}}}
	copied := project.DeepCopy()
	copied.Spec.DebugAdapter.TCP.Port = 6789
	require.Equal(t, int32(5678), project.Spec.DebugAdapter.TCP.Port)
	require.NotEmpty(t, copied.ValidateUpdate(context.Background(), project))
	copied.Spec.DebugAdapter.TCP = nil
	require.NotEmpty(t, copied.ValidateUpdate(context.Background(), project))
}
