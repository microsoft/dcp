/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"fmt"
	"path"
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

// ProjectContainerSpec identifies source code and its container launch configuration.
// +k8s:openapi-gen=true
type ProjectContainerSpec struct {
	ProjectCommonSpec `json:",inline"`
	// Container describes how to run the code as a container.
	Container ProjectContainerTemplate `json:"container"`
	// DebugAdapter declares the required adapter; configuration alone does not launch it.
	DebugAdapter *ContainerDebugAdapterConfig `json:"debugAdapter"`
	// Source declares where the Project source lives inside the container and optionally binds it from rootDirectory.
	// When omitted, the source location is unknown to DCP and {{sourceDirectory}} cannot be used.
	Source *ProjectContainerSource `json:"source,omitempty"`
}

// ProjectContainerSource locates the Project source inside the container, provided by the image or bound from the host.
// +k8s:openapi-gen=true
type ProjectContainerSource struct {
	// ContainerDirectory is the absolute container path holding the source.
	ContainerDirectory string `json:"containerDirectory"`
	// Bind mounts rootDirectory at containerDirectory. When false, the image is expected to provide the source.
	// A bind mount hides anything the image placed at that path, including installed dependencies or build output.
	Bind bool `json:"bind,omitempty"`
	// ReadOnly makes the bind mount read-only. It requires bind.
	ReadOnly bool `json:"readOnly,omitempty"`
}

// ProjectContainerStatus describes the container, launch inputs, execution and debugging progress.
// +k8s:openapi-gen=true
type ProjectContainerStatus struct {
	ProjectCommonStatus `json:",inline"`
	// PhysicalContainerRef identifies the same-namespace PhysicalContainer.
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

// GetDebugAdapterArgs resolves directory references against the installed adapter directory and source.containerDirectory.
func (resource *ProjectContainer) GetDebugAdapterArgs() []string {
	if resource.Spec.DebugAdapter == nil {
		return nil
	}
	return expandDebugAdapterArgs(resource.Spec.DebugAdapter.Args, resource.Spec.DebugAdapter.GetContainerDirectory(), resource.GetSourceDirectory())
}

// GetSourceDirectory returns source.containerDirectory, or empty when the container declares no source.
func (resource *ProjectContainer) GetSourceDirectory() string {
	if resource.Spec.Source == nil {
		return ""
	}
	return resource.Spec.Source.ContainerDirectory
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
	errorList = append(errorList, resource.validateSource(specPath)...)
	return errorList
}

// validateSource checks the source location and, for bind mounts, rejects declared mounts or the adapter installation
// overlapping it, so DCP never decides which one shadows the other.
func (resource *ProjectContainer) validateSource(specPath *field.Path) field.ErrorList {
	var errorList field.ErrorList
	source := resource.Spec.Source
	if source == nil {
		if resource.Spec.DebugAdapter != nil {
			for index, argument := range resource.Spec.DebugAdapter.Args {
				if strings.Contains(argument, "{{sourceDirectory}}") {
					errorList = append(errorList, field.Invalid(specPath.Child("debugAdapter", "args").Index(index), argument,
						"{{sourceDirectory}} requires spec.source"))
				}
			}
		}
		return errorList
	}
	sourcePath := specPath.Child("source")
	if !path.IsAbs(source.ContainerDirectory) || strings.ContainsRune(source.ContainerDirectory, '\x00') {
		return append(errorList, field.Invalid(sourcePath.Child("containerDirectory"), source.ContainerDirectory,
			"containerDirectory must be an absolute container path"))
	}
	if source.ReadOnly && !source.Bind {
		errorList = append(errorList, field.Forbidden(sourcePath.Child("readOnly"), "readOnly requires bind"))
	}
	if !source.Bind {
		return errorList
	}
	for index, mount := range resource.Spec.Container.VolumeMounts {
		if containerPathsOverlap(source.ContainerDirectory, mount.Target) {
			errorList = append(errorList, field.Invalid(specPath.Child("container", "volumeMounts").Index(index).Child("target"), mount.Target,
				"volume mount target overlaps the bound source directory"))
		}
	}
	if resource.Spec.DebugAdapter != nil && containerPathsOverlap(source.ContainerDirectory, resource.Spec.DebugAdapter.GetContainerDirectory()) {
		errorList = append(errorList, field.Invalid(sourcePath.Child("containerDirectory"), source.ContainerDirectory,
			"the bound source directory overlaps the debug adapter installation directory"))
	}
	return errorList
}

// containerPathsOverlap reports whether two absolute container paths are equal or one contains the other.
func containerPathsOverlap(first, second string) bool {
	first, second = path.Clean(first), path.Clean(second)
	if first == second || first == "/" || second == "/" {
		return true
	}
	return strings.HasPrefix(second, first+"/") || strings.HasPrefix(first, second+"/")
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
