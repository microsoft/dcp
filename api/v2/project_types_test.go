/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Verifies that ProjectProcess requires an absolute source root, a valid process template and a debug adapter.
func TestProjectProcessValidate(t *testing.T) {
	root := t.TempDir()
	process := ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"}}
	for _, testCase := range []struct {
		name  string
		spec  ProjectProcessSpec
		valid bool
	}{
		{"process", ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root}, Process: process}, true},
		{"direct", ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root, LaunchMode: ProjectLaunchModeDirect}, Process: process}, true},
		{"invalid launch mode", ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root, LaunchMode: "invalid"}, Process: process}, false},
		{"missing root", ProjectProcessSpec{Process: process}, false},
		{"relative root", ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: "source"}, Process: process}, false},
		{"invalid root", ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root + "\x00"}, Process: process}, false},
		{"empty command", ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root}}, false},
		{"missing directory", ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: filepath.Join(root, "not-present")}, Process: process}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resource := &ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"}, Spec: testCase.spec}
			resource.Spec.DebugAdapter = &DebugAdapterConfig{Directory: root, Args: []string{"adapter"}}
			require.Equal(t, testCase.valid, len(resource.Validate(context.Background())) == 0)
		})
	}
	missingAdapter := &ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
		Spec: ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root}, Process: process}}
	require.ErrorContains(t, missingAdapter.Validate(context.Background()).ToAggregate(), "spec.debugAdapter")
}

// Verifies that ProjectContainer requires an absolute source root, a valid container template and a debug adapter,
// including validation of container dependency namespaces.
func TestProjectContainerValidate(t *testing.T) {
	root := t.TempDir()
	common := ProjectCommonSpec{RootDirectory: root}
	for _, testCase := range []struct {
		name  string
		spec  ProjectContainerSpec
		valid bool
	}{
		{"image reference", ProjectContainerSpec{ProjectCommonSpec: common, Container: ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}}}, true},
		{"direct", ProjectContainerSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root, LaunchMode: ProjectLaunchModeDirect}, Container: ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}}}, true},
		{"invalid launch mode", ProjectContainerSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root, LaunchMode: "invalid"}, Container: ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}}}, false},
		{"inline image", ProjectContainerSpec{ProjectCommonSpec: common, Container: ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Image: "source"}}}}, true},
		{"empty image", ProjectContainerSpec{ProjectCommonSpec: common}, false},
		{"missing root", ProjectContainerSpec{Container: ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}}}, false},
		{"cross namespace", ProjectContainerSpec{ProjectCommonSpec: common, Container: ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "other/image"}}}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resource := &ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"}, Spec: testCase.spec}
			resource.Spec.DebugAdapter = &ContainerDebugAdapterConfig{DebugAdapterConfig: DebugAdapterConfig{Directory: root, Args: []string{"adapter"}}}
			require.Equal(t, testCase.valid, len(resource.Validate(context.Background())) == 0)
		})
	}
	missingAdapter := &ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
		Spec: ProjectContainerSpec{ProjectCommonSpec: common, Container: ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}}}}
	require.ErrorContains(t, missingAdapter.Validate(context.Background()).ToAggregate(), "spec.debugAdapter")
}

// Verifies that ProjectProcess and ProjectContainer freeze source and launch configuration,
// permitting only one-way start and stop requests.
func TestProjectValidateUpdate(t *testing.T) {
	process := &ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
		Spec: ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: t.TempDir()},
			Process: ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"}}}}
	stoppedProcess := process.DeepCopy()
	stoppedProcess.Spec.Stop = true
	require.Empty(t, stoppedProcess.ValidateUpdate(context.Background(), process))
	require.NotEmpty(t, process.ValidateUpdate(context.Background(), stoppedProcess))
	for _, change := range []func(*ProjectProcess){
		func(changed *ProjectProcess) {
			changed.Spec.RootDirectory = filepath.Join(changed.Spec.RootDirectory, "other")
		},
		func(changed *ProjectProcess) { changed.Spec.Process.WorkingDirectory = "assets" },
		func(changed *ProjectProcess) { changed.Spec.LaunchMode = ProjectLaunchModeDirect },
		func(changed *ProjectProcess) {
			changed.Spec.Process.Ports = []LogicalProcessPort{{LogicalServiceBinding: LogicalServiceBinding{Name: "http", ServiceRef: "service"}}}
		},
	} {
		changed := process.DeepCopy()
		change(changed)
		require.NotEmpty(t, changed.ValidateUpdate(context.Background(), process))
	}

	container := &ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
		Spec: ProjectContainerSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: t.TempDir()},
			Container: ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}}}}
	stoppedContainer := container.DeepCopy()
	stoppedContainer.Spec.Stop = true
	require.Empty(t, stoppedContainer.ValidateUpdate(context.Background(), container))
	require.NotEmpty(t, container.ValidateUpdate(context.Background(), stoppedContainer))
	changedContainer := container.DeepCopy()
	changedContainer.Spec.Container.ImageRef = "other"
	require.NotEmpty(t, changedContainer.ValidateUpdate(context.Background(), container))
	changedContainer = container.DeepCopy()
	changedContainer.Spec.LaunchMode = ProjectLaunchModeDirect
	require.NotEmpty(t, changedContainer.ValidateUpdate(context.Background(), container))
}

// Verifies that both Project kinds allow starting once, treat nil as true, and leave the input specs unchanged.
func TestProjectValidateUpdateStartTransitions(t *testing.T) {
	t.Parallel()
	startFalse, startTrue := false, true
	for _, testCase := range []struct {
		name     string
		oldStart *bool
		newStart *bool
		allowed  bool
	}{
		{name: "false to true", oldStart: &startFalse, newStart: &startTrue, allowed: true},
		{name: "false to default", oldStart: &startFalse, allowed: true},
		{name: "default to false", newStart: &startFalse},
		{name: "true to false", oldStart: &startTrue, newStart: &startFalse},
		{name: "true to default", oldStart: &startTrue, allowed: true},
		{name: "default to true", newStart: &startTrue, allowed: true},
		{name: "false unchanged", oldStart: &startFalse, newStart: &startFalse, allowed: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			oldProcess := &ProjectProcess{Spec: ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{Start: testCase.oldStart}}}
			newProcess := oldProcess.DeepCopy()
			newProcess.Spec.Start = testCase.newStart
			oldContainer := &ProjectContainer{Spec: ProjectContainerSpec{ProjectCommonSpec: ProjectCommonSpec{Start: testCase.oldStart}}}
			newContainer := oldContainer.DeepCopy()
			newContainer.Spec.Start = testCase.newStart
			oldProcessSnapshot, newProcessSnapshot := oldProcess.DeepCopy(), newProcess.DeepCopy()
			oldContainerSnapshot, newContainerSnapshot := oldContainer.DeepCopy(), newContainer.DeepCopy()

			processErrors := newProcess.ValidateUpdate(context.Background(), oldProcess)
			containerErrors := newContainer.ValidateUpdate(context.Background(), oldContainer)
			if testCase.allowed {
				require.Empty(t, processErrors)
				require.Empty(t, containerErrors)
			} else {
				require.ErrorContains(t, processErrors.ToAggregate(), "spec.start")
				require.ErrorContains(t, containerErrors.ToAggregate(), "spec.start")
			}
			require.Equal(t, oldProcessSnapshot, oldProcess)
			require.Equal(t, newProcessSnapshot, newProcess)
			require.Equal(t, oldContainerSnapshot, oldContainer)
			require.Equal(t, newContainerSnapshot, newContainer)
		})
	}
}

// Verifies that Project template ports use the shared service-binding validation and are deeply copied.
func TestProjectTemplatePorts(t *testing.T) {
	t.Parallel()
	binding := LogicalServiceBinding{Name: "http", Service: &LogicalServiceTemplate{Name: "front"}}
	common := ProjectCommonSpec{RootDirectory: t.TempDir()}

	process := &ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
		Spec: ProjectProcessSpec{ProjectCommonSpec: common, Process: ProjectProcessTemplate{
			LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"}, Ports: []LogicalProcessPort{{LogicalServiceBinding: binding}}},
			DebugAdapter: &DebugAdapterConfig{Directory: common.RootDirectory, Args: []string{"adapter"}}}}
	require.Empty(t, process.Validate(context.Background()))
	clonedProcess := process.DeepCopy()
	clonedProcess.Spec.Process.Ports[0].Service.Name = "changed"
	clonedProcess.Spec.Process.Ports[0].ServiceRef = "other/service"
	require.NotEmpty(t, clonedProcess.Validate(context.Background()))
	require.NotEmpty(t, clonedProcess.ValidateUpdate(context.Background(), process))
	require.Equal(t, "front", process.Spec.Process.Ports[0].Service.Name)

	container := &ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
		Spec: ProjectContainerSpec{ProjectCommonSpec: common, Container: ProjectContainerTemplate{
			LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}, Ports: []LogicalContainerPort{{LogicalServiceBinding: binding, ContainerPort: 8080}}},
			DebugAdapter: &ContainerDebugAdapterConfig{DebugAdapterConfig: DebugAdapterConfig{Directory: common.RootDirectory, Args: []string{"adapter"}}}}}
	require.Empty(t, container.Validate(context.Background()))
	clonedContainer := container.DeepCopy()
	clonedContainer.Spec.Container.Ports[0].Service.Name = "changed"
	clonedContainer.Spec.Container.Ports[0].ServiceRef = "other/service"
	clonedContainer.Spec.Container.Ports[0].ContainerPort = 0
	require.NotEmpty(t, clonedContainer.Validate(context.Background()))
	require.NotEmpty(t, clonedContainer.ValidateUpdate(context.Background(), container))
	require.Equal(t, "front", container.Spec.Container.Ports[0].Service.Name)
}

// Verifies that ProjectContainer requires an absolute source containerDirectory, restricts readOnly to binds,
// rejects bound sources overlapping declared mounts or the adapter installation, and requires source for {{sourceDirectory}}.
func TestProjectContainerSourceValidate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, testCase := range []struct {
		name    string
		source  *ProjectContainerSource
		mounts  []VolumeMount
		args    []string
		invalid string
	}{
		{name: "image-provided", source: &ProjectContainerSource{ContainerDirectory: "/app"}},
		{name: "bind", source: &ProjectContainerSource{ContainerDirectory: "/app", Bind: true, ReadOnly: true}},
		{name: "relative", source: &ProjectContainerSource{ContainerDirectory: "app"}, invalid: "spec.source.containerDirectory"},
		{name: "read-only-without-bind", source: &ProjectContainerSource{ContainerDirectory: "/app", ReadOnly: true}, invalid: "spec.source.readOnly"},
		{name: "bind-overlaps-mount", source: &ProjectContainerSource{ContainerDirectory: "/app", Bind: true},
			mounts: []VolumeMount{{Type: NamedVolumeMount, VolumeRef: "data", Target: "/app/data"}}, invalid: "spec.container.volumeMounts[0].target"},
		{name: "unbound-allows-mount", source: &ProjectContainerSource{ContainerDirectory: "/app"},
			mounts: []VolumeMount{{Type: NamedVolumeMount, VolumeRef: "data", Target: "/app/data"}}},
		{name: "bind-overlaps-adapter", source: &ProjectContainerSource{ContainerDirectory: "/opt/dcp", Bind: true}, invalid: "spec.source.containerDirectory"},
		{name: "source-reference-without-source", args: []string{"adapter", "{{sourceDirectory}}"}, invalid: "spec.debugAdapter.args[1]"},
		{name: "source-reference", source: &ProjectContainerSource{ContainerDirectory: "/app"}, args: []string{"adapter", "{{sourceDirectory}}"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			args := testCase.args
			if args == nil {
				args = []string{"adapter"}
			}
			resource := &ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"}, Spec: ProjectContainerSpec{
				ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root},
				Container:         ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image", VolumeMounts: testCase.mounts}},
				DebugAdapter:      &ContainerDebugAdapterConfig{DebugAdapterConfig: DebugAdapterConfig{Directory: root, Args: args}},
				Source:            testCase.source,
			}}
			errorList := resource.Validate(context.Background())
			if testCase.invalid == "" {
				require.Empty(t, errorList)
			} else {
				require.ErrorContains(t, errorList.ToAggregate(), testCase.invalid)
			}
		})
	}
}
