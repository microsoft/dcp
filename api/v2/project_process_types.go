/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"fmt"
	"strings"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// ProjectProcessSpec identifies source code and its host process launch configuration.
// +k8s:openapi-gen=true
type ProjectProcessSpec struct {
	ProjectCommonSpec `json:",inline"`
	// Process describes how to run the code as a host process.
	Process ProjectProcessTemplate `json:"process"`
	// DebugAdapter declares the required adapter; configuration alone does not launch it.
	DebugAdapter *DebugAdapterConfig `json:"debugAdapter"`
}

// ProjectProcessStatus describes process execution and debugging progress.
// +k8s:openapi-gen=true
type ProjectProcessStatus struct {
	ProjectCommonStatus `json:",inline"`
	// PhysicalProcessRef identifies the owned same-namespace PhysicalProcess in direct mode.
	PhysicalProcessRef string `json:"physicalProcessRef,omitempty"`
	// PID is the directly launched host process ID.
	PID *int64 `json:"pid,omitempty"`
	// IdentityTimestamp identifies the process instance together with PID.
	// On Linux this represents elapsed time since boot, not a wall-clock start time.
	IdentityTimestamp metav1.MicroTime `json:"identityTimestamp,omitempty"`
}

func (status ProjectProcessStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	status.DeepCopyInto(&dest.(*ProjectProcess).Status)
}

// ProjectProcess runs user code directly or prepares it for adapter-controlled host execution.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=projectprocesses
type ProjectProcess struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ProjectProcessSpec   `json:"spec,omitempty"`
	Status            ProjectProcessStatus `json:"status,omitempty"`
}

func (*ProjectProcess) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupName, Version: Version, Resource: "projectprocesses"}
}

// GetResourceId identifies the ProjectProcess system log stream.
func (resource *ProjectProcess) GetResourceId() string {
	return fmt.Sprintf("projectprocess-%s", resource.UID)
}

// GetLogSource returns the observed immediate physical execution output source.
func (resource *ProjectProcess) GetLogSource() *LogSource {
	return resource.Status.RuntimeLogs
}

// GetLogStreams describes the immediate execution output.
func (resource *ProjectProcess) GetLogStreams() LogStreams {
	return resource.Status.runtimeLogStreams()
}

func (*ProjectProcess) GenericSubResources() []apiserver_resource.GenericSubResource {
	return []apiserver_resource.GenericSubResource{&ProjectLogResource{}}
}

func (resource *ProjectProcess) GetProjectSpec() *ProjectCommonSpec {
	return &resource.Spec.ProjectCommonSpec
}
func (resource *ProjectProcess) GetProjectStatus() *ProjectCommonStatus {
	return &resource.Status.ProjectCommonStatus
}
func (resource *ProjectProcess) GetDebugAdapter() *DebugAdapterConfig {
	if resource.Spec.DebugAdapter == nil {
		return nil
	}
	return resource.Spec.DebugAdapter
}

// GetDebugAdapterArgs resolves directory references against the host adapter directory and rootDirectory.
func (resource *ProjectProcess) GetDebugAdapterArgs() []string {
	if resource.Spec.DebugAdapter == nil {
		return nil
	}
	return expandDebugAdapterArgs(resource.Spec.DebugAdapter.Args, resource.Spec.DebugAdapter.Directory, resource.Spec.RootDirectory)
}

// GetSourceDirectory returns rootDirectory, where a host process sees its source.
func (resource *ProjectProcess) GetSourceDirectory() string {
	return resource.Spec.RootDirectory
}

func (resource *ProjectProcess) GetObjectMeta() *metav1.ObjectMeta { return &resource.ObjectMeta }
func (resource *ProjectProcess) GetStatus() apiserver_resource.StatusSubResource {
	return resource.Status
}
func (*ProjectProcess) New() runtime.Object     { return &ProjectProcess{} }
func (*ProjectProcess) NewList() runtime.Object { return &ProjectProcessList{} }
func (*ProjectProcess) IsStorageVersion() bool  { return true }
func (*ProjectProcess) NamespaceScoped() bool   { return true }
func (resource *ProjectProcess) NamespacedName() types.NamespacedName {
	return NamespacedName(resource)
}

// PrepareForCreate prevents callers from supplying prepared inputs or debug observations.
func (resource *ProjectProcess) PrepareForCreate(context.Context) {
	resource.Status = ProjectProcessStatus{}
}

func (resource *ProjectProcess) Validate(context.Context) field.ErrorList {
	specPath := field.NewPath("spec")
	errorList := validateProject(resource, specPath)
	if strings.TrimSpace(resource.Spec.Process.ExecutablePath) == "" {
		errorList = append(errorList, field.Required(specPath.Child("process", "executablePath"), "executablePath must be set"))
	}
	errorList = append(errorList, validateLogicalProcessPorts(resource.Spec.Process.Ports, resource.Namespace, resource.Name, specPath.Child("process", "ports"))...)
	errorList = append(errorList, resource.Spec.Process.AmbientEnvironment.validate(specPath.Child("process", "ambientEnvironment"))...)
	if resource.Spec.DebugAdapter == nil {
		errorList = append(errorList, field.Required(specPath.Child("debugAdapter"), "debugAdapter must be set"))
	} else {
		errorList = append(errorList, resource.Spec.DebugAdapter.validate(specPath.Child("debugAdapter"))...)
	}
	return errorList
}

func (resource *ProjectProcess) ValidateUpdate(_ context.Context, old runtime.Object) field.ErrorList {
	return validateProjectUpdate(old.(*ProjectProcess).Spec, resource.Spec, func(spec *ProjectProcessSpec) *ProjectCommonSpec { return &spec.ProjectCommonSpec })
}

// ProjectProcessList contains a list of ProjectProcess instances.
// +kubebuilder:object:root=true
// +k8s:openapi-gen=true
type ProjectProcessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProjectProcess `json:"items"`
}

func (list *ProjectProcessList) GetListMeta() *metav1.ListMeta { return &list.ListMeta }
func (list *ProjectProcessList) ItemCount() uint32             { return uint32(len(list.Items)) }
func (list *ProjectProcessList) GetItems() []*ProjectProcess {
	items := make([]*ProjectProcess, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}
	return items
}

func init() {
	SchemeBuilder.Register(&ProjectProcess{}, &ProjectProcessList{})
}

var _ ProjectObject = (*ProjectProcess)(nil)
var _ apiserver_resource.Object = (*ProjectProcess)(nil)
var _ LogStreamingResource = (*ProjectProcess)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*ProjectProcess)(nil)
var _ apiserver_resource.ObjectWithGenericSubResource = (*ProjectProcess)(nil)
var _ apiserver_resource.StatusSubResource = (*ProjectProcessStatus)(nil)
var _ apiserver_resource.ObjectList = (*ProjectProcessList)(nil)
var _ commonapi.ListWithObjectItems[ProjectProcess, *ProjectProcess] = (*ProjectProcessList)(nil)
var _ apiserver_resourcestrategy.Validater = (*ProjectProcess)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*ProjectProcess)(nil)
