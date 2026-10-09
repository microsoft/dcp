/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"context"
	"reflect"
	"strings"

	"github.com/microsoft/dcp/pkg/commonapi"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ContainerVolumeResetSpec requests a single destructive reset of a Container's named volumes.
// +k8s:openapi-gen=true
type ContainerVolumeResetSpec struct {
	// Name of the Container API resource to reset.
	ContainerName string `json:"containerName"`
	// UID of the expected Container API resource, preventing reset of a replacement.
	ContainerUID types.UID `json:"containerUid"`
}

// ContainerVolumeResetConsumer identifies another consumer that prevents a named-volume reset.
// +k8s:openapi-gen=true
type ContainerVolumeResetConsumer struct {
	VolumeName    string `json:"volumeName"`
	ContainerName string `json:"containerName"`
	ContainerID   string `json:"containerId,omitempty"`
}

// ContainerVolumeResetStatus reports reset progress and any partial destructive outcome.
// +k8s:openapi-gen=true
type ContainerVolumeResetStatus struct {
	// Pending, Running, Succeeded, or Failed. Only Succeeded confirms all volumes were reset.
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	// Time the operation finished. Terminal results are retained for one hour after publication.
	FinishTimestamp metav1.MicroTime `json:"finishTimestamp,omitempty"`
	// Names of volumes successfully removed and recreated empty.
	// +listType=atomic
	Volumes []string `json:"volumes,omitempty"`
	// Other consumers, including stopped runtime containers, found during preflight.
	// +listType=atomic
	Consumers []ContainerVolumeResetConsumer `json:"consumers,omitempty"`
	// Whether the target physical container was removed, even when the operation failed.
	ContainerRemoved bool `json:"containerRemoved,omitempty"`
}

func (status ContainerVolumeResetStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	status.DeepCopyInto(&dest.(*ContainerVolumeReset).Status)
}

// ContainerVolumeReset resets a specific Container's owned named volumes once.
// Create a new operation to retry; completed operations do not suspend Container reconciliation.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type ContainerVolumeReset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ContainerVolumeResetSpec   `json:"spec"`
	Status            ContainerVolumeResetStatus `json:"status,omitempty"`
}

func (*ContainerVolumeReset) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupVersion.Group, Version: GroupVersion.Version, Resource: "containervolumeresets"}
}
func (reset *ContainerVolumeReset) GetObjectMeta() *metav1.ObjectMeta { return &reset.ObjectMeta }
func (reset *ContainerVolumeReset) GetStatus() apiserver_resource.StatusSubResource {
	return reset.Status
}
func (*ContainerVolumeReset) New() runtime.Object     { return &ContainerVolumeReset{} }
func (*ContainerVolumeReset) NewList() runtime.Object { return &ContainerVolumeResetList{} }
func (*ContainerVolumeReset) IsStorageVersion() bool  { return true }
func (*ContainerVolumeReset) NamespaceScoped() bool   { return false }
func (*ContainerVolumeReset) ShortNames() []string    { return []string{"ctrvolreset"} }
func (reset *ContainerVolumeReset) NamespacedName() types.NamespacedName {
	return types.NamespacedName{Name: reset.Name, Namespace: reset.Namespace}
}

func (reset *ContainerVolumeReset) Validate(_ context.Context) field.ErrorList {
	errors := field.ErrorList{}
	if ResourceCreationProhibited.Load() && reset.DeletionTimestamp.IsZero() {
		errors = append(errors, field.Forbidden(nil, errResourceCreationProhibited.Error()))
	}
	if strings.TrimSpace(reset.Spec.ContainerName) == "" {
		errors = append(errors, field.Required(field.NewPath("spec", "containerName"), "containerName must be set"))
	}
	if strings.TrimSpace(string(reset.Spec.ContainerUID)) == "" {
		errors = append(errors, field.Required(field.NewPath("spec", "containerUid"), "containerUid must be set"))
	}
	return append(errors, commonapi.ValidateAnnotationsSize(reset.Annotations, field.NewPath("metadata", "annotations"))...)
}

func (reset *ContainerVolumeReset) ValidateUpdate(_ context.Context, old runtime.Object) field.ErrorList {
	if !reflect.DeepEqual(reset.Spec, old.(*ContainerVolumeReset).Spec) {
		return field.ErrorList{field.Forbidden(field.NewPath("spec"), "reset target cannot be changed")}
	}
	return nil
}

// ContainerVolumeResetList contains ContainerVolumeReset operations.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type ContainerVolumeResetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContainerVolumeReset `json:"items"`
}

func (list *ContainerVolumeResetList) GetListMeta() *metav1.ListMeta { return &list.ListMeta }
func (list *ContainerVolumeResetList) ItemCount() uint32             { return uint32(len(list.Items)) }
func (list *ContainerVolumeResetList) GetItems() []*ContainerVolumeReset {
	items := make([]*ContainerVolumeReset, len(list.Items))
	for index := range list.Items {
		items[index] = &list.Items[index]
	}
	return items
}

func init() { SchemeBuilder.Register(&ContainerVolumeReset{}, &ContainerVolumeResetList{}) }

var _ apiserver_resource.Object = (*ContainerVolumeReset)(nil)
var _ apiserver_resource.ObjectList = (*ContainerVolumeResetList)(nil)
var _ commonapi.ListWithObjectItems[ContainerVolumeReset, *ContainerVolumeReset] = (*ContainerVolumeResetList)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*ContainerVolumeReset)(nil)
var _ apiserver_resource.StatusSubResource = (*ContainerVolumeResetStatus)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*ContainerVolumeReset)(nil)
var _ apiserver_resourcestrategy.Validater = (*ContainerVolumeReset)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*ContainerVolumeReset)(nil)
