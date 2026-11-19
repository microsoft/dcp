/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"fmt"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// ProjectContainerSpec identifies source code and its container launch configuration.
// +k8s:openapi-gen=true
type ProjectContainerSpec struct {
	ProjectCommonSpec `json:",inline"`
	// Container describes how to run the code as a container.
	Container ProjectContainerTemplate `json:"container"`
	// DebugAdapter declares the required adapter; configuration alone does not launch it.
	DebugAdapter *ContainerDebugAdapterConfig `json:"debugAdapter"`
}

// ProjectContainerStatus describes the container, launch inputs, execution and debugging progress.
// +k8s:openapi-gen=true
type ProjectContainerStatus struct {
	ProjectCommonStatus `json:",inline"`
	// PhysicalContainerRef identifies the same-namespace backing PhysicalContainer.
	// Cross-namespace references are not supported.
	PhysicalContainerRef string `json:"physicalContainerRef,omitempty"`
	// ContainerRuntime identifies the controller-selected runtime for the prepared container.
	ContainerRuntime string `json:"containerRuntime,omitempty"`
	// DebugAdapterPort is the prepared internal TCP listening port, absent for other adapter transports.
	DebugAdapterPort int32 `json:"debugAdapterPort,omitempty"`
	// DebugAdapterAddress is the runtime-published host TCP endpoint for the prepared adapter listener.
	DebugAdapterAddress string `json:"debugAdapterAddress,omitempty"`
	// ImageRef identifies the supplied or owned PhysicalContainerImage.
	// Cross-namespace references are not supported.
	ImageRef string `json:"imageRef,omitempty"`
	// ContainerID is the observed runtime container identity.
	ContainerID string `json:"containerID,omitempty"`
	// ContainerName is the observed runtime container name.
	ContainerName string `json:"containerName,omitempty"`
	// Image is the resolved runtime image reference.
	Image string `json:"image,omitempty"`
	// CreatedAt is the runtime container creation time.
	CreatedAt metav1.MicroTime `json:"createdAt,omitempty"`
	// StartedAt is the runtime container start time.
	StartedAt metav1.MicroTime `json:"startedAt,omitempty"`
	// PortMappings are the observed host bindings for container ports.
	// +listType=atomic
	PortMappings []PhysicalContainerPortMapping `json:"portMappings,omitempty"`
	// StartupLogs describes image acquisition capture.
	StartupLogs *LogSource `json:"startupLogs,omitempty"`
}

func (status ProjectContainerStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	status.DeepCopyInto(&dest.(*ProjectContainer).Status)
}

// ProjectContainer runs user code directly or prepares it for adapter-controlled container execution.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=projectcontainers
type ProjectContainer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ProjectContainerSpec   `json:"spec,omitempty"`
	Status            ProjectContainerStatus `json:"status,omitempty"`
}

func (*ProjectContainer) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupName, Version: Version, Resource: "projectcontainers"}
}

// GetResourceId identifies the ProjectContainer system log stream.
func (resource *ProjectContainer) GetResourceId() string {
	return fmt.Sprintf("projectcontainer-%s", resource.UID)
}

// GetLogSource returns the observed immediate physical execution output source.
func (resource *ProjectContainer) GetLogSource() *LogSource {
	return resource.Status.RuntimeLogs
}

// GetLogStreams treats startup as complete when no image acquisition is owned or execution has finished.
func (resource *ProjectContainer) GetLogStreams() LogStreams {
	streams := resource.Status.runtimeLogStreams()
	streams.StartupSupported = true
	streams.Startup = resource.Status.StartupLogs
	if streams.Startup == nil && (resource.Spec.Container.Image == nil || streams.Complete) {
		streams.Startup = &LogSource{Complete: true}
	}
	return streams
}

func (*ProjectContainer) GenericSubResources() []apiserver_resource.GenericSubResource {
	return []apiserver_resource.GenericSubResource{&ProjectLogResource{}}
}

func (resource *ProjectContainer) GetProjectSpec() *ProjectCommonSpec {
	return &resource.Spec.ProjectCommonSpec
}
func (resource *ProjectContainer) GetProjectStatus() *ProjectCommonStatus {
	return &resource.Status.ProjectCommonStatus
}
func (resource *ProjectContainer) GetDebugAdapter() *DebugAdapterConfig {
	if resource.Spec.DebugAdapter == nil {
		return nil
	}
	return &resource.Spec.DebugAdapter.DebugAdapterConfig
}

func (resource *ProjectContainer) GetObjectMeta() *metav1.ObjectMeta { return &resource.ObjectMeta }
func (resource *ProjectContainer) GetStatus() apiserver_resource.StatusSubResource {
	return resource.Status
}
func (*ProjectContainer) New() runtime.Object     { return &ProjectContainer{} }
func (*ProjectContainer) NewList() runtime.Object { return &ProjectContainerList{} }
func (*ProjectContainer) IsStorageVersion() bool  { return true }
func (*ProjectContainer) NamespaceScoped() bool   { return true }
func (resource *ProjectContainer) NamespacedName() types.NamespacedName {
	return NamespacedName(resource)
}

// PrepareForCreate prevents callers from supplying prepared inputs or debug observations.
func (resource *ProjectContainer) PrepareForCreate(context.Context) {
	resource.Status = ProjectContainerStatus{}
}

func (resource *ProjectContainer) Validate(context.Context) field.ErrorList {
	specPath := field.NewPath("spec")
	errorList := validateProject(resource, specPath)
	errorList = append(errorList, resource.Spec.Container.Validate(resource.Namespace, specPath.Child("container"))...)
	errorList = append(errorList, validateLogicalContainerPorts(resource.Spec.Container.Ports, resource.Namespace, resource.Name, specPath.Child("container", "ports"))...)
	if resource.Spec.DebugAdapter == nil {
		errorList = append(errorList, field.Required(specPath.Child("debugAdapter"), "debugAdapter must be set"))
	} else {
		errorList = append(errorList, resource.Spec.DebugAdapter.validate(specPath.Child("debugAdapter"))...)
	}
	return errorList
}

func (resource *ProjectContainer) ValidateUpdate(_ context.Context, old runtime.Object) field.ErrorList {
	return validateProjectUpdate(old.(*ProjectContainer).Spec, resource.Spec, func(spec *ProjectContainerSpec) *ProjectCommonSpec { return &spec.ProjectCommonSpec })
}

// ProjectContainerList contains a list of ProjectContainer instances.
// +kubebuilder:object:root=true
// +k8s:openapi-gen=true
type ProjectContainerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProjectContainer `json:"items"`
}

func (list *ProjectContainerList) GetListMeta() *metav1.ListMeta { return &list.ListMeta }
func (list *ProjectContainerList) ItemCount() uint32             { return uint32(len(list.Items)) }
func (list *ProjectContainerList) GetItems() []*ProjectContainer {
	items := make([]*ProjectContainer, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}
	return items
}

func init() {
	SchemeBuilder.Register(&ProjectContainer{}, &ProjectContainerList{})
}

var _ ProjectObject = (*ProjectContainer)(nil)
var _ apiserver_resource.Object = (*ProjectContainer)(nil)
var _ LogStreamingResource = (*ProjectContainer)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*ProjectContainer)(nil)
var _ apiserver_resource.ObjectWithGenericSubResource = (*ProjectContainer)(nil)
var _ apiserver_resource.StatusSubResource = (*ProjectContainerStatus)(nil)
var _ apiserver_resource.ObjectList = (*ProjectContainerList)(nil)
var _ commonapi.ListWithObjectItems[ProjectContainer, *ProjectContainer] = (*ProjectContainerList)(nil)
var _ apiserver_resourcestrategy.Validater = (*ProjectContainer)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*ProjectContainer)(nil)
