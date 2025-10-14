// Copyright (c) Microsoft Corporation. All rights reserved.

package v1

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
)

type ContainerVolumeState string

const (
	// Same as ContainerVolumeStatePending. Happens when ContainerVolume status has not been initialized yet.
	ContainerVolumeStateEmpty ContainerVolumeState = ""

	// The volume has not been checked for existence or has not been created yet.
	ContainerVolumeStatePending ContainerVolumeState = "Pending"

	// ContainerVolumeStateRuntimeUnhealthy indicates that the underlying Docker/Podman volume cannot be created
	// (or its existence cannot be verified) because the container runtime is not healthy.
	ContainerVolumeStateRuntimeUnhealthy ContainerVolumeState = "RuntimeUnhealthy"

	// The underlying Docker/Podman volume has been created and is ready for use.
	ContainerVolumeStateReady ContainerVolumeState = "Ready"
)

// ContainerVolumeSpec defines the desired state of a ContainerVolume
// +k8s:openapi-gen=true
type ContainerVolumeSpec struct {
	// Name of the volume
	Name string `json:"name"`

	// Is this volume persistent (is NOT cleaned up when the application ends) or not.
	// Volumes are persistent by default.
	// +kubebuilder:default=true
	Persistent *bool `json:"persistent,omitempty"`
}

// ContainerVolumeStatus describes the status of a ContainerVolume
// +k8s:openapi-gen=true
type ContainerVolumeStatus struct {
	// The current state of the ContainerVolume
	// +kubebuilder:default=Pending
	// +kubebuilder:validation:Enum=Pending;RuntimeUnhealthy;Ready
	State ContainerVolumeState `json:"state,omitempty"`
}

func (cvs ContainerVolumeStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	cvs.DeepCopyInto(&dest.(*ContainerVolume).Status)
}

// ContainerVolume represents a volume that can be consumed by Container instances
// Its lifetime is independent from the lifetime of containers
// +kubebuilder:object:root=true
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type ContainerVolume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ContainerVolumeSpec   `json:"spec,omitempty"`
	Status ContainerVolumeStatus `json:"status,omitempty"`
}

func (cv *ContainerVolume) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "containervolumes",
	}
}

func (cv *ContainerVolume) GetObjectMeta() *metav1.ObjectMeta {
	return &cv.ObjectMeta
}

func (cv *ContainerVolume) New() runtime.Object {
	persistent := true // Default value
	return &ContainerVolume{
		Spec: ContainerVolumeSpec{
			Persistent: &persistent,
		},
	}
}

func (cv *ContainerVolume) NewList() runtime.Object {
	return &ContainerVolumeList{}
}

func (cv *ContainerVolume) IsStorageVersion() bool {
	return true
}

func (cv *ContainerVolume) NamespaceScoped() bool {
	return false
}

func (cv *ContainerVolume) ShortNames() []string {
	return []string{"ctrvol"}
}

func (cv *ContainerVolume) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      cv.Name,
		Namespace: cv.Namespace,
	}
}

func (cv *ContainerVolume) GetStatus() apiserver_resource.StatusSubResource {
	return cv.Status
}

func (cv *ContainerVolume) Validate(ctx context.Context) field.ErrorList {
	errorList := field.ErrorList{}

	if ResourceCreationProhibited.Load() && cv.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, errResourceCreationProhibited.Error()))
	}

	if strings.TrimSpace(cv.Spec.Name) == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec").Child("name"), "Name is required"))
	}

	return errorList
}

func (cv *ContainerVolume) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldVolume := old.(*ContainerVolume)

	if oldVolume.Spec.Name != cv.Spec.Name {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec").Child("name"), "Name cannot be changed"))
	}

	if !pointers.EqualValue(oldVolume.Spec.Persistent, cv.Spec.Persistent) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec").Child("persistent"), "volume persistence cannot be changed"))
	}

	return errorList
}

// ContainerVolumeList contains a list of ContainerVolume instances
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type ContainerVolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContainerVolume `json:"items"`
}

func (cvl *ContainerVolumeList) GetListMeta() *metav1.ListMeta {
	return &cvl.ListMeta
}

func (cvl *ContainerVolumeList) ItemCount() uint32 {
	return uint32(len(cvl.Items))
}

func (cvl *ContainerVolumeList) GetItems() []*ContainerVolume {
	retval := make([]*ContainerVolume, len(cvl.Items))
	for i := range cvl.Items {
		retval[i] = &cvl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&ContainerVolume{}, &ContainerVolumeList{})
}

// Ensure types support interfaces expected by our API server
var _ apiserver_resource.Object = (*ContainerVolume)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*ContainerVolume)(nil)
var _ apiserver_resource.StatusSubResource = (*ContainerVolumeStatus)(nil)
var _ apiserver_resource.ObjectList = (*ContainerVolumeList)(nil)
var _ commonapi.ListWithObjectItems[ContainerVolume, *ContainerVolume] = (*ContainerVolumeList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*ContainerVolume)(nil)
var _ apiserver_resourcestrategy.Validater = (*ContainerVolume)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*ContainerVolume)(nil)
