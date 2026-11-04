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

// Verifies that Project requires an absolute source root and exactly one valid launch template,
// including validation of container dependency namespaces.
func TestProjectValidate(t *testing.T) {
	root := t.TempDir()
	for _, testCase := range []struct {
		name  string
		spec  ProjectSpec
		valid bool
	}{
		{"process", ProjectSpec{RootDirectory: root, Process: &ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"}}}, true},
		{"container", ProjectSpec{RootDirectory: root, Container: &ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}}}, true},
		{"inline image", ProjectSpec{RootDirectory: root, Container: &ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Image: "source"}}}}, true},
		{"neither", ProjectSpec{RootDirectory: root}, false},
		{"both", ProjectSpec{RootDirectory: root, Process: &ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"}}, Container: &ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}}}, false},
		{"missing root", ProjectSpec{Process: &ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"}}}, false},
		{"relative root", ProjectSpec{RootDirectory: "source", Process: &ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"}}}, false},
		{"invalid root", ProjectSpec{RootDirectory: root + "\x00", Process: &ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"}}}, false},
		{"empty command", ProjectSpec{RootDirectory: root, Process: &ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{}}}, false},
		{"empty image", ProjectSpec{RootDirectory: root, Container: &ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{}}}, false},
		{"cross namespace", ProjectSpec{RootDirectory: root, Container: &ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "other/image"}}}, false},
		{"missing directory", ProjectSpec{RootDirectory: filepath.Join(root, "not-present"), Process: &ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"}}}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resource := &Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"}, Spec: testCase.spec}
			require.Equal(t, testCase.valid, len(resource.Validate(context.Background())) == 0)
		})
	}
}

// Verifies that Project freezes source and launch configuration, permitting only a one-way stop request.
func TestProjectValidateUpdate(t *testing.T) {
	resource := &Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
		Spec: ProjectSpec{RootDirectory: t.TempDir(), Process: &ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"}}}}
	stopped := resource.DeepCopy()
	stopped.Spec.Stop = true
	require.Empty(t, stopped.ValidateUpdate(context.Background(), resource))
	require.NotEmpty(t, resource.ValidateUpdate(context.Background(), stopped))
	for _, change := range []func(*Project){
		func(changed *Project) {
			changed.Spec.RootDirectory = filepath.Join(changed.Spec.RootDirectory, "other")
		},
		func(changed *Project) { changed.Spec.Process.WorkingDirectory = "assets" },
		func(changed *Project) {
			changed.Spec.Process.Ports = []LogicalProcessPort{{LogicalServiceBinding: LogicalServiceBinding{Name: "http", ServiceRef: "service"}}}
		},
		func(changed *Project) {
			changed.Spec.Process = nil
			changed.Spec.Container = &ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"}}
		},
	} {
		changed := resource.DeepCopy()
		change(changed)
		require.NotEmpty(t, changed.ValidateUpdate(context.Background(), resource))
	}
}

// Verifies that Project template ports use the shared service-binding validation and are deeply copied.
func TestProjectTemplatePorts(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		t.Run(kind, func(t *testing.T) {
			binding := LogicalServiceBinding{Name: "http", Service: &LogicalServiceTemplate{Name: "front"}}
			resource := &Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "test"},
				Spec: ProjectSpec{RootDirectory: t.TempDir()}}
			if kind == "process" {
				resource.Spec.Process = &ProjectProcessTemplate{LogicalProcessConfig: LogicalProcessConfig{ExecutablePath: "program"},
					Ports: []LogicalProcessPort{{LogicalServiceBinding: binding}}}
			} else {
				resource.Spec.Container = &ProjectContainerTemplate{LogicalContainerConfig: LogicalContainerConfig{ImageRef: "image"},
					Ports: []LogicalContainerPort{{LogicalServiceBinding: binding, ContainerPort: 8080}}}
			}
			require.Empty(t, resource.Validate(context.Background()))
			cloned := resource.DeepCopy()
			if kind == "process" {
				cloned.Spec.Process.Ports[0].Service.Name = "changed"
				cloned.Spec.Process.Ports[0].ServiceRef = "other/service"
			} else {
				cloned.Spec.Container.Ports[0].Service.Name = "changed"
				cloned.Spec.Container.Ports[0].ServiceRef = "other/service"
				cloned.Spec.Container.Ports[0].ContainerPort = 0
			}
			require.NotEmpty(t, cloned.Validate(context.Background()))
			require.NotEmpty(t, cloned.ValidateUpdate(context.Background(), resource))
			if kind == "process" {
				require.Equal(t, "front", resource.Spec.Process.Ports[0].Service.Name)
			} else {
				require.Equal(t, "front", resource.Spec.Container.Ports[0].Service.Name)
			}
		})
	}
}
