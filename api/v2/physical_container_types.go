/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"fmt"
	"path"
	"reflect"
	"regexp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/dcp/pkg/commonapi"
)

var (
	// See: https://github.com/moby/moby/blob/master/daemon/names/names.go
	validContainerName       = `^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`
	validContainerNameRegexp = regexp.MustCompile(validContainerName)
)

// PhysicalContainerPhase describes the lifecycle phase of a PhysicalContainer.
type PhysicalContainerPhase string

const (
	// PhysicalContainerPhasePending indicates that the container is waiting for prerequisites.
	PhysicalContainerPhasePending PhysicalContainerPhase = "Pending"

	// PhysicalContainerPhaseRunning indicates that the runtime container is running.
	PhysicalContainerPhaseRunning PhysicalContainerPhase = "Running"

	// PhysicalContainerPhaseExited indicates that the runtime container has exited.
	PhysicalContainerPhaseExited PhysicalContainerPhase = "Exited"

	// PhysicalContainerPhaseMissing indicates that the referenced runtime container was not found.
	PhysicalContainerPhaseMissing PhysicalContainerPhase = "Missing"

	// PhysicalContainerPhaseFailed indicates that creating or inspecting the runtime container failed.
	PhysicalContainerPhaseFailed PhysicalContainerPhase = "Failed"
)

const (
	// PhysicalContainerReasonPending indicates that the container is waiting for prerequisites.
	PhysicalContainerReasonPending string = "Pending"

	// PhysicalContainerReasonCreating indicates that runtime container creation is in progress.
	PhysicalContainerReasonCreating string = "Creating"

	// PhysicalContainerReasonCreated indicates that runtime container creation completed.
	PhysicalContainerReasonCreated string = "Created"

	// PhysicalContainerReasonCopyingFiles indicates that pre-start file copy is in progress.
	PhysicalContainerReasonCopyingFiles string = "CopyingFiles"

	// PhysicalContainerReasonFilesCreated indicates that pre-start file copy completed.
	PhysicalContainerReasonFilesCreated string = "FilesCreated"

	// PhysicalContainerReasonStarting indicates that runtime container start is in progress.
	PhysicalContainerReasonStarting string = "Starting"

	// PhysicalContainerReasonStarted indicates that runtime container start completed.
	PhysicalContainerReasonStarted string = "Started"

	// PhysicalContainerReasonCreateFailed indicates that runtime container creation failed.
	PhysicalContainerReasonCreateFailed string = "CreateFailed"

	// PhysicalContainerReasonFileCopyFailed indicates that pre-start file copy failed.
	PhysicalContainerReasonFileCopyFailed string = "FileCopyFailed"

	// PhysicalContainerReasonStartFailed indicates that runtime container start failed.
	PhysicalContainerReasonStartFailed string = "StartFailed"

	// PhysicalContainerReasonReconciliationFailed indicates that reconciliation failed outside a specific progress gate.
	PhysicalContainerReasonReconciliationFailed string = "ReconciliationFailed"

	// PhysicalContainerReasonRuntimeContainerMissing indicates that the runtime container was not found.
	PhysicalContainerReasonRuntimeContainerMissing string = "RuntimeContainerMissing"

	// PhysicalContainerReasonRuntimeContainerRunning indicates that the runtime container is running.
	PhysicalContainerReasonRuntimeContainerRunning string = "RuntimeContainerRunning"

	// PhysicalContainerReasonRuntimeContainerExited indicates that the runtime container has exited.
	PhysicalContainerReasonRuntimeContainerExited string = "RuntimeContainerExited"

	// PhysicalContainerReasonRuntimeContainerPending indicates that the runtime container is not yet running.
	PhysicalContainerReasonRuntimeContainerPending string = "RuntimeContainerPending"
)

// PhysicalContainerSpec describes either an existing runtime container or how to create one.
// +k8s:openapi-gen=true
type PhysicalContainerSpec struct {
	// ContainerID identifies an existing runtime container to track. When set, creation fields are forbidden.
	ContainerID string `json:"containerID,omitempty"`

	// Stop requests that the tracked runtime container be stopped.
	Stop bool `json:"stop,omitempty"`

	// Persistent keeps a runtime container created by this resource in place when the resource is deleted.
	// Existing runtime containers referenced by containerID are always retained.
	Persistent bool `json:"persistent,omitempty"`

	// ImageRef is the name of a PhysicalContainerImage in the same namespace to use when creating a new runtime container.
	ImageRef string `json:"imageRef,omitempty"`

	// ContainerName is the runtime name to use when creating a new container.
	ContainerName string `json:"containerName,omitempty"`

	// ReplaceExisting removes an existing runtime container with containerName before creating a new one.
	ReplaceExisting bool `json:"replaceExisting,omitempty"`

	// Entrypoint is the container runtime entrypoint to run.
	Entrypoint string `json:"entrypoint,omitempty"`

	// Command is the command arguments passed to the container entrypoint.
	// +listType=atomic
	Command []string `json:"command,omitempty"`

	// Env contains environment variables to set in the container.
	// +listType=map
	// +listMapKey=name
	Env []commonapi.EnvVar `json:"env,omitempty"`

	// Ports describes ports to expose from the container.
	// +listType=atomic
	Ports []ContainerPort `json:"ports,omitempty"`

	// VolumeMounts describes volume and bind mounts for the container.
	// +listType=atomic
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`

	// Networks describes runtime networks to attach the container to when it is created.
	// If omitted, the container runtime chooses the default network.
	// +listType=atomic
	Networks []ContainerNetworkConnectionConfig `json:"networks,omitempty"`

	// CreateFiles describes files and folders to copy into the container before it starts.
	// +listType=atomic
	CreateFiles []CreateFileSystem `json:"createFiles,omitempty"`

	// Labels contains labels to apply to a newly-created runtime container.
	// +listType=map
	// +listMapKey=key
	Labels []commonapi.Label `json:"labels,omitempty"`
}

// PhysicalContainerPortMapping describes the observed host binding for one runtime container port.
// +k8s:openapi-gen=true
type PhysicalContainerPortMapping struct {
	// ContainerPort is the port exposed by the runtime container.
	ContainerPort int32 `json:"containerPort"`

	// Protocol is the port protocol.
	Protocol commonapi.PortProtocol `json:"protocol,omitempty"`

	// HostIP is the host address for the published port.
	HostIP string `json:"hostIP,omitempty"`

	// HostPort is the published host port.
	HostPort int32 `json:"hostPort,omitempty"`
}

// PhysicalContainerStatus describes the observed runtime container.
// +k8s:openapi-gen=true
type PhysicalContainerStatus struct {
	// Phase summarizes the runtime container lifecycle.
	// +kubebuilder:validation:Enum=Pending;Running;Exited;Missing;Failed
	// +optional
	Phase PhysicalContainerPhase `json:"phase,omitempty"`

	// ContainerID is the runtime container ID being tracked.
	ContainerID string `json:"containerID,omitempty"`

	// ContainerName is the runtime container name.
	ContainerName string `json:"containerName,omitempty"`

	// Image is the resolved runtime image reference used to create the container.
	Image string `json:"image,omitempty"`

	// RuntimeStatus is the raw status reported by the container runtime.
	RuntimeStatus string `json:"runtimeStatus,omitempty"`

	// CreatedAt is the runtime container creation timestamp.
	CreatedAt metav1.MicroTime `json:"createdAt,omitempty"`

	// StartedAt is the runtime container start timestamp.
	StartedAt metav1.MicroTime `json:"startedAt,omitempty"`

	// FinishedAt is the runtime container finish timestamp.
	FinishedAt metav1.MicroTime `json:"finishedAt,omitempty"`

	// ExitCode is the runtime container exit code, when available.
	ExitCode *int32 `json:"exitCode,omitempty"`

	// PortMappings are the observed host bindings for runtime container ports.
	// +listType=atomic
	PortMappings []PhysicalContainerPortMapping `json:"portMappings,omitempty"`

	// Conditions describe readiness and reconciliation progress.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func (pcs PhysicalContainerStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	pcs.DeepCopyInto(&dest.(*PhysicalContainer).Status)
}

// PhysicalContainer represents one physical container instance in a DCP V2 namespace.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=physicalcontainers,shortName=pctr
type PhysicalContainer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PhysicalContainerSpec   `json:"spec,omitempty"`
	Status PhysicalContainerStatus `json:"status,omitempty"`
}

func (pc *PhysicalContainer) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "physicalcontainers",
	}
}

func (pc *PhysicalContainer) GetObjectMeta() *metav1.ObjectMeta {
	return &pc.ObjectMeta
}

func (pc *PhysicalContainer) GetStatus() apiserver_resource.StatusSubResource {
	return pc.Status
}

func (pc *PhysicalContainer) New() runtime.Object {
	return &PhysicalContainer{}
}

func (pc *PhysicalContainer) NewList() runtime.Object {
	return &PhysicalContainerList{}
}

func (pc *PhysicalContainer) IsStorageVersion() bool {
	return true
}

func (pc *PhysicalContainer) NamespaceScoped() bool {
	return true
}

func (pc *PhysicalContainer) ShortNames() []string {
	return []string{"pctr"}
}

func (pc *PhysicalContainer) NamespacedName() types.NamespacedName {
	return NamespacedName(pc)
}

func (pc *PhysicalContainer) Validate(ctx context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(pc)
	specPath := field.NewPath("spec")

	if commonapi.ResourceCreationProhibited.Load() && pc.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}

	errorList = append(errorList, commonapi.ValidateAnnotationsSize(pc.Annotations, field.NewPath("metadata", "annotations"))...)

	if pc.Spec.ContainerID != "" {
		errorList = append(errorList, pc.validateExistingContainerSpec(specPath)...)
		return errorList
	}

	if pc.Spec.ImageRef == "" {
		errorList = append(errorList, field.Required(specPath.Child("imageRef"), "imageRef must be set when containerID is omitted"))
	} else {
		for _, validationMessage := range validation.IsDNS1123Subdomain(pc.Spec.ImageRef) {
			errorList = append(errorList, field.Invalid(specPath.Child("imageRef"), pc.Spec.ImageRef, validationMessage))
		}
	}
	if pc.Spec.ContainerName != "" && !validContainerNameRegexp.MatchString(pc.Spec.ContainerName) {
		errorList = append(errorList, field.Invalid(specPath.Child("containerName"), pc.Spec.ContainerName, fmt.Sprintf("containerName must match regex '%s'", validContainerName)))
	}
	if pc.Spec.ReplaceExisting && pc.Spec.ContainerName == "" {
		errorList = append(errorList, field.Required(specPath.Child("containerName"), "containerName must be set when replaceExisting is true"))
	}

	networksPath := specPath.Child("networks")
	for i, network := range pc.Spec.Networks {
		if network.Name == "" {
			errorList = append(errorList, field.Required(networksPath.Index(i).Child("name"), "name must be set to a non-empty value"))
		}
	}
	errorList = append(errorList, ValidateContainerPorts(pc.Spec.Ports, specPath.Child("ports"))...)
	errorList = append(errorList, validateLabels(pc.Spec.Labels, specPath.Child("labels"))...)

	createFilesPath := specPath.Child("createFiles")
	for i, createFile := range pc.Spec.CreateFiles {
		createFilePath := createFilesPath.Index(i)
		if createFile.Destination != "" && !path.IsAbs(createFile.Destination) {
			errorList = append(errorList, field.Invalid(createFilePath.Child("destination"), createFile.Destination, "destination must be absolute"))
		}
		if createFile.Umask != nil && !(*createFile.Umask).IsRegular() {
			errorList = append(errorList, field.Invalid(createFilePath.Child("umask"), *createFile.Umask, "umask must not include type bits"))
		}
		if len(createFile.Entries) == 0 {
			errorList = append(errorList, field.Required(createFilePath.Child("entries"), "at least one child entry must be specified"))
		}
		for j, item := range createFile.Entries {
			errorList = append(errorList, item.Validate(createFilePath.Child("entries").Index(j))...)
		}
		if createFile.DefaultOwner < 0 {
			errorList = append(errorList, field.Invalid(createFilePath.Child("defaultOwner"), createFile.DefaultOwner, "default owner must be a non-negative integer"))
		}
		if createFile.DefaultGroup < 0 {
			errorList = append(errorList, field.Invalid(createFilePath.Child("defaultGroup"), createFile.DefaultGroup, "default group must be a non-negative integer"))
		}
	}

	return errorList
}

func (pc *PhysicalContainer) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldPhysicalContainer := old.(*PhysicalContainer)
	if oldPhysicalContainer.Spec.Stop && !pc.Spec.Stop {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "stop"), "stop cannot be set to false once it has been set to true"))
	}

	oldSpec := oldPhysicalContainer.Spec
	newSpec := pc.Spec
	oldSpec.Stop = false
	newSpec.Stop = false
	if !reflect.DeepEqual(oldSpec, newSpec) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec"), "spec is immutable"))
	}

	return errorList
}

func (pc *PhysicalContainer) validateExistingContainerSpec(specPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}

	if pc.Spec.Persistent {
		errorList = append(errorList, field.Forbidden(specPath.Child("persistent"), "persistent cannot be set when containerID is set"))
	}
	if pc.Spec.ImageRef != "" {
		errorList = append(errorList, field.Forbidden(specPath.Child("imageRef"), "imageRef cannot be set when containerID is set"))
	}
	if pc.Spec.ContainerName != "" {
		errorList = append(errorList, field.Forbidden(specPath.Child("containerName"), "containerName cannot be set when containerID is set"))
	}
	if pc.Spec.ReplaceExisting {
		errorList = append(errorList, field.Forbidden(specPath.Child("replaceExisting"), "replaceExisting cannot be set when containerID is set"))
	}
	if pc.Spec.Entrypoint != "" {
		errorList = append(errorList, field.Forbidden(specPath.Child("entrypoint"), "entrypoint cannot be set when containerID is set"))
	}
	if len(pc.Spec.Command) > 0 {
		errorList = append(errorList, field.Forbidden(specPath.Child("command"), "command cannot be set when containerID is set"))
	}
	if len(pc.Spec.Env) > 0 {
		errorList = append(errorList, field.Forbidden(specPath.Child("env"), "env cannot be set when containerID is set"))
	}
	if len(pc.Spec.Ports) > 0 {
		errorList = append(errorList, field.Forbidden(specPath.Child("ports"), "ports cannot be set when containerID is set"))
	}
	if len(pc.Spec.VolumeMounts) > 0 {
		errorList = append(errorList, field.Forbidden(specPath.Child("volumeMounts"), "volumeMounts cannot be set when containerID is set"))
	}
	if len(pc.Spec.Networks) > 0 {
		errorList = append(errorList, field.Forbidden(specPath.Child("networks"), "networks cannot be set when containerID is set"))
	}
	if len(pc.Spec.CreateFiles) > 0 {
		errorList = append(errorList, field.Forbidden(specPath.Child("createFiles"), "createFiles cannot be set when containerID is set"))
	}
	if len(pc.Spec.Labels) > 0 {
		errorList = append(errorList, field.Forbidden(specPath.Child("labels"), "labels cannot be set when containerID is set"))
	}

	return errorList
}

// PhysicalContainerList contains a list of PhysicalContainer instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type PhysicalContainerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PhysicalContainer `json:"items"`
}

func (pcl *PhysicalContainerList) GetListMeta() *metav1.ListMeta {
	return &pcl.ListMeta
}

func (pcl *PhysicalContainerList) ItemCount() uint32 {
	return uint32(len(pcl.Items))
}

func (pcl *PhysicalContainerList) GetItems() []*PhysicalContainer {
	retval := make([]*PhysicalContainer, len(pcl.Items))
	for i := range pcl.Items {
		retval[i] = &pcl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&PhysicalContainer{}, &PhysicalContainerList{})
}

// Ensure types support interfaces expected by our API server.
var _ apiserver_resource.Object = (*PhysicalContainer)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*PhysicalContainer)(nil)
var _ apiserver_resource.StatusSubResource = (*PhysicalContainerStatus)(nil)
var _ apiserver_resource.ObjectList = (*PhysicalContainerList)(nil)
var _ commonapi.ListWithObjectItems[PhysicalContainer, *PhysicalContainer] = (*PhysicalContainerList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*PhysicalContainer)(nil)
var _ apiserver_resourcestrategy.Validater = (*PhysicalContainer)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*PhysicalContainer)(nil)
