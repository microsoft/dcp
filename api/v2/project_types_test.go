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

// Verifies that ProjectProcess requires an absolute source root and a valid process template.
func TestProjectProcessValidate(t *testing.T) {
	root := t.TempDir()
	process := ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"}}
	for _, testCase := range []struct {
		name  string
		spec  ProjectProcessSpec
		valid bool
	}{
		{"process", ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root}, Process: process}, true},
		{"missing root", ProjectProcessSpec{Process: process}, false},
		{"relative root", ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: "source"}, Process: process}, false},
		{"invalid root", ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root + "\x00"}, Process: process}, false},
		{"empty command", ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: root}}, false},
		{"missing directory", ProjectProcessSpec{ProjectCommonSpec: ProjectCommonSpec{RootDirectory: filepath.Join(root, "not-present")}, Process: process}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resource := &ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"}, Spec: testCase.spec}
			require.Equal(t, testCase.valid, len(resource.Validate(context.Background())) == 0)
		})
	}
}

// Verifies that ProjectContainer requires an absolute source root and a valid container template,
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
		{"inline image", ProjectContainerSpec{ProjectCommonSpec: common, Container: ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Image: "source"}}}}, true},
		{"empty image", ProjectContainerSpec{ProjectCommonSpec: common}, false},
		{"missing root", ProjectContainerSpec{Container: ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}}}, false},
		{"cross namespace", ProjectContainerSpec{ProjectCommonSpec: common, Container: ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "other/image"}}}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resource := &ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"}, Spec: testCase.spec}
			require.Equal(t, testCase.valid, len(resource.Validate(context.Background())) == 0)
		})
	}
}

// Verifies that ProjectProcess and ProjectContainer freeze source and launch configuration,
// permitting only a one-way stop request.
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
}

// Verifies that Project template ports use the shared service-binding validation and are deeply copied.
func TestProjectTemplatePorts(t *testing.T) {
	t.Parallel()
	binding := LogicalServiceBinding{Name: "http", Service: &LogicalServiceTemplate{Name: "front"}}
	common := ProjectCommonSpec{RootDirectory: t.TempDir()}

	process := &ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
		Spec: ProjectProcessSpec{ProjectCommonSpec: common, Process: ProjectProcessTemplate{
			LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"}, Ports: []LogicalProcessPort{{LogicalServiceBinding: binding}}}}}
	require.Empty(t, process.Validate(context.Background()))
	clonedProcess := process.DeepCopy()
	clonedProcess.Spec.Process.Ports[0].Service.Name = "changed"
	clonedProcess.Spec.Process.Ports[0].ServiceRef = "other/service"
	require.NotEmpty(t, clonedProcess.Validate(context.Background()))
	require.NotEmpty(t, clonedProcess.ValidateUpdate(context.Background(), process))
	require.Equal(t, "front", process.Spec.Process.Ports[0].Service.Name)

	container := &ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
		Spec: ProjectContainerSpec{ProjectCommonSpec: common, Container: ProjectContainerTemplate{
			LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}, Ports: []LogicalContainerPort{{LogicalServiceBinding: binding, ContainerPort: 8080}}}}}
	require.Empty(t, container.Validate(context.Background()))
	clonedContainer := container.DeepCopy()
	clonedContainer.Spec.Container.Ports[0].Service.Name = "changed"
	clonedContainer.Spec.Container.Ports[0].ServiceRef = "other/service"
	clonedContainer.Spec.Container.Ports[0].ContainerPort = 0
	require.NotEmpty(t, clonedContainer.Validate(context.Background()))
	require.NotEmpty(t, clonedContainer.ValidateUpdate(context.Background(), container))
	require.Equal(t, "front", container.Spec.Container.Ports[0].Service.Name)
}
