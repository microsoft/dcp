/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"reflect"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// PhysicalContainerVolumePhase describes the lifecycle phase of a PhysicalContainerVolume.
type PhysicalContainerVolumePhase string

const (
	// PhysicalContainerVolumePhasePending indicates that the volume is waiting for prerequisites.
	PhysicalContainerVolumePhasePending PhysicalContainerVolumePhase = "Pending"

	// PhysicalContainerVolumePhaseReady indicates that the runtime volume is available.
	PhysicalContainerVolumePhaseReady PhysicalContainerVolumePhase = "Ready"

	// PhysicalContainerVolumePhaseMissing indicates that the referenced runtime volume was not found.
	PhysicalContainerVolumePhaseMissing PhysicalContainerVolumePhase = "Missing"

	// PhysicalContainerVolumePhaseFailed indicates that creating or inspecting the runtime volume failed.
	PhysicalContainerVolumePhaseFailed PhysicalContainerVolumePhase = "Failed"
)

const (
	// PhysicalContainerVolumeReasonPending indicates that the volume is waiting for prerequisites.
	PhysicalContainerVolumeReasonPending string = "Pending"

	// PhysicalContainerVolumeReasonCreating indicates that runtime volume creation is in progress.
	PhysicalContainerVolumeReasonCreating string = "Creating"

	// PhysicalContainerVolumeReasonCreated indicates that runtime volume creation completed.
	PhysicalContainerVolumeReasonCreated string = "Created"

	// PhysicalContainerVolumeReasonCreateFailed indicates that runtime volume creation failed.
	PhysicalContainerVolumeReasonCreateFailed string = "CreateFailed"

	// PhysicalContainerVolumeReasonVolumeReady indicates that the runtime volume is available.
	PhysicalContainerVolumeReasonVolumeReady string = "VolumeReady"

	// PhysicalContainerVolumeReasonRuntimeVolumeMissing indicates that the runtime volume was not found.
	PhysicalContainerVolumeReasonRuntimeVolumeMissing string = "RuntimeVolumeMissing"

	// PhysicalContainerVolumeReasonReconciliationFailed indicates that reconciliation failed outside a specific progress gate.
	PhysicalContainerVolumeReasonReconciliationFailed string = "ReconciliationFailed"
)

// PhysicalContainerVolumeSpec describes either an existing runtime volume or how to create one.
// +k8s:openapi-gen=true
type PhysicalContainerVolumeSpec struct {
	// VolumeID identifies an existing runtime volume to track. Container runtimes commonly use
	// the volume name as its identifier. When set, creation fields are forbidden.
	VolumeID string `json:"volumeID,omitempty"`

	// VolumeName is the runtime name to use when creating a new volume. Required when volumeID is omitted.
	VolumeName string `json:"volumeName,omitempty"`

	// Persistent keeps a runtime volume created by this resource in place when the resource is deleted.
	// Existing runtime volumes referenced by volumeID are always retained.
	Persistent bool `json:"persistent,omitempty"`

	// ReplaceExisting removes an existing runtime volume with volumeName before creating a new one.
	// Replacement waits while the existing volume is in use and never removes attached containers.
	ReplaceExisting bool `json:"replaceExisting,omitempty"`

	// Labels contains labels to apply to a newly-created runtime volume.
	// +listType=map
	// +listMapKey=key
	Labels []commonapi.Label `json:"labels,omitempty"`
}

// PhysicalContainerVolumeStatus describes the observed runtime volume.
// +k8s:openapi-gen=true
type PhysicalContainerVolumeStatus struct {
	// Phase summarizes whether the runtime volume is available.
	// +kubebuilder:validation:Enum=Pending;Ready;Missing;Failed
	// +optional
	Phase PhysicalContainerVolumePhase `json:"phase,omitempty"`

	// VolumeID is the runtime identifier being tracked.
	VolumeID string `json:"volumeID,omitempty"`

	// VolumeName is the runtime volume name.
	VolumeName string `json:"volumeName,omitempty"`

	// Driver is the runtime volume driver.
	Driver string `json:"driver,omitempty"`

	// MountPoint is the host path where the runtime stores volume data, when exposed by the runtime.
	MountPoint string `json:"mountPoint,omitempty"`

	// Scope is the runtime volume scope.
	Scope string `json:"scope,omitempty"`

	// CreatedAt is the runtime volume creation timestamp.
	CreatedAt metav1.MicroTime `json:"createdAt,omitempty"`

	// Conditions describe readiness and reconciliation progress.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func (pcvs PhysicalContainerVolumeStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	pcvs.DeepCopyInto(&dest.(*PhysicalContainerVolume).Status)
}

// PhysicalContainerVolume represents one runtime container volume in a DCP V2 namespace.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=physicalcontainervolumes,shortName=pcv
type PhysicalContainerVolume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PhysicalContainerVolumeSpec   `json:"spec,omitempty"`
	Status PhysicalContainerVolumeStatus `json:"status,omitempty"`
}

func (pv *PhysicalContainerVolume) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "physicalcontainervolumes",
	}
}

func (pv *PhysicalContainerVolume) GetObjectMeta() *metav1.ObjectMeta {
	return &pv.ObjectMeta
}

func (pv *PhysicalContainerVolume) GetStatus() apiserver_resource.StatusSubResource {
	return pv.Status
}

func (pv *PhysicalContainerVolume) New() runtime.Object {
	return &PhysicalContainerVolume{}
}

func (pv *PhysicalContainerVolume) NewList() runtime.Object {
	return &PhysicalContainerVolumeList{}
}

func (pv *PhysicalContainerVolume) IsStorageVersion() bool {
	return true
}

func (pv *PhysicalContainerVolume) NamespaceScoped() bool {
	return true
}

func (pv *PhysicalContainerVolume) ShortNames() []string {
	return []string{"pcv"}
}

func (pv *PhysicalContainerVolume) NamespacedName() types.NamespacedName {
	return NamespacedName(pv)
}

func (pv *PhysicalContainerVolume) Validate(ctx context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(pv)
	specPath := field.NewPath("spec")

	if commonapi.ResourceCreationProhibited.Load() && pv.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}

	errorList = append(errorList, commonapi.ValidateAnnotationsSize(pv.Annotations, field.NewPath("metadata", "annotations"))...)

	if pv.Spec.VolumeID != "" {
		if pv.Spec.Persistent {
			errorList = append(errorList, field.Forbidden(specPath.Child("persistent"), "persistent cannot be set when volumeID is set"))
		}
		if strings.TrimSpace(pv.Spec.VolumeID) != pv.Spec.VolumeID {
			errorList = append(errorList, field.Invalid(specPath.Child("volumeID"), pv.Spec.VolumeID, "volumeID must not have leading or trailing whitespace"))
		}
		if pv.Spec.VolumeName != "" {
			errorList = append(errorList, field.Forbidden(specPath.Child("volumeName"), "volumeName cannot be set when volumeID is set"))
		}
		if len(pv.Spec.Labels) > 0 {
			errorList = append(errorList, field.Forbidden(specPath.Child("labels"), "labels cannot be set when volumeID is set"))
		}
		if pv.Spec.ReplaceExisting {
			errorList = append(errorList, field.Forbidden(specPath.Child("replaceExisting"), "replaceExisting cannot be set when volumeID is set"))
		}
		return errorList
	}

	if strings.TrimSpace(pv.Spec.VolumeName) == "" {
		errorList = append(errorList, field.Required(specPath.Child("volumeName"), "volumeName must be set when volumeID is omitted"))
	} else if strings.TrimSpace(pv.Spec.VolumeName) != pv.Spec.VolumeName {
		errorList = append(errorList, field.Invalid(specPath.Child("volumeName"), pv.Spec.VolumeName, "volumeName must not have leading or trailing whitespace"))
	}

	errorList = append(errorList, validateLabels(pv.Spec.Labels, specPath.Child("labels"))...)
	return errorList
}

// ValidateUpdate freezes the spec of an existing PhysicalContainerVolume.
func (pv *PhysicalContainerVolume) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldPhysicalContainerVolume := old.(*PhysicalContainerVolume)
	if !reflect.DeepEqual(oldPhysicalContainerVolume.Spec, pv.Spec) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec"), "spec is immutable"))
	}

	return errorList
}

// PhysicalContainerVolumeList contains a list of PhysicalContainerVolume instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type PhysicalContainerVolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PhysicalContainerVolume `json:"items"`
}

func (pvl *PhysicalContainerVolumeList) GetListMeta() *metav1.ListMeta {
	return &pvl.ListMeta
}

func (pvl *PhysicalContainerVolumeList) ItemCount() uint32 {
	return uint32(len(pvl.Items))
}

func (pvl *PhysicalContainerVolumeList) GetItems() []*PhysicalContainerVolume {
	retval := make([]*PhysicalContainerVolume, len(pvl.Items))
	for i := range pvl.Items {
		retval[i] = &pvl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&PhysicalContainerVolume{}, &PhysicalContainerVolumeList{})
}

// Ensure types support interfaces expected by our API server.
var _ apiserver_resource.Object = (*PhysicalContainerVolume)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*PhysicalContainerVolume)(nil)
var _ apiserver_resource.StatusSubResource = (*PhysicalContainerVolumeStatus)(nil)
var _ apiserver_resource.ObjectList = (*PhysicalContainerVolumeList)(nil)
var _ commonapi.ListWithObjectItems[PhysicalContainerVolume, *PhysicalContainerVolume] = (*PhysicalContainerVolumeList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*PhysicalContainerVolume)(nil)
var _ apiserver_resourcestrategy.Validater = (*PhysicalContainerVolume)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*PhysicalContainerVolume)(nil)
