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

// PhysicalContainerImagePhase describes the lifecycle phase of a PhysicalContainerImage.
type PhysicalContainerImagePhase PhysicalResourcePhase

const (
	// PhysicalContainerImagePhasePending indicates that the image is waiting for prerequisites.
	PhysicalContainerImagePhasePending PhysicalContainerImagePhase = PhysicalContainerImagePhase(PhysicalResourcePhasePending)

	// PhysicalContainerImagePhaseReady indicates that the image is available to the container runtime.
	PhysicalContainerImagePhaseReady PhysicalContainerImagePhase = PhysicalContainerImagePhase(PhysicalResourcePhaseReady)

	// PhysicalContainerImagePhaseUnknown indicates that image availability cannot be determined.
	PhysicalContainerImagePhaseUnknown PhysicalContainerImagePhase = PhysicalContainerImagePhase(PhysicalResourcePhaseUnknown)

	// PhysicalContainerImagePhaseFailed indicates a terminal pull or build failure.
	PhysicalContainerImagePhaseFailed PhysicalContainerImagePhase = PhysicalContainerImagePhase(PhysicalResourcePhaseFailed)
)

const (
	// PhysicalContainerImageReasonPending indicates that the image is waiting for prerequisites.
	PhysicalContainerImageReasonPending ConditionReason = "Pending"

	// PhysicalContainerImageReasonPulling indicates that image pull is in progress.
	PhysicalContainerImageReasonPulling ConditionReason = "Pulling"

	// PhysicalContainerImageReasonPulled indicates that image pull completed.
	PhysicalContainerImageReasonPulled ConditionReason = "Pulled"

	// PhysicalContainerImageReasonBuilding indicates that image build is in progress.
	PhysicalContainerImageReasonBuilding ConditionReason = "Building"

	// PhysicalContainerImageReasonBuilt indicates that image build completed.
	PhysicalContainerImageReasonBuilt ConditionReason = "Built"

	// PhysicalContainerImageReasonPullFailed indicates that image pull failed.
	PhysicalContainerImageReasonPullFailed ConditionReason = "PullFailed"

	// PhysicalContainerImageReasonBuildFailed indicates that image build failed.
	PhysicalContainerImageReasonBuildFailed ConditionReason = "BuildFailed"

	// PhysicalContainerImageReasonImageReady indicates that the image is available to the container runtime.
	PhysicalContainerImageReasonImageReady ConditionReason = "ImageReady"

	// PhysicalContainerImageReasonReconciliationFailed indicates that reconciliation failed outside a specific progress gate.
	PhysicalContainerImageReasonReconciliationFailed ConditionReason = "ReconciliationFailed"

	// PhysicalContainerImageReasonInspectFailed indicates that the runtime image could not be inspected.
	PhysicalContainerImageReasonInspectFailed ConditionReason = "InspectFailed"

	// PhysicalContainerImageReasonImageUnavailable indicates that an image required to exist locally is unavailable.
	PhysicalContainerImageReasonImageUnavailable ConditionReason = "ImageUnavailable"

	// PhysicalContainerImageReasonOperationRetryPending indicates that a completed image operation will be retried.
	PhysicalContainerImageReasonOperationRetryPending ConditionReason = "OperationRetryPending"
)

// PhysicalContainerImageSpec describes a source image to pull or an image to build.
// +k8s:openapi-gen=true
type PhysicalContainerImageSpec struct {
	// Image is the source image reference to ensure locally, or the target tag for a built image.
	Image string `json:"image,omitempty"`

	// Build describes how to build the image locally.
	Build *ContainerBuildContext `json:"build,omitempty"`

	// PullPolicy controls source image pulling. If omitted, missing is used.
	// Never is not supported for image builds.
	PullPolicy ImagePullPolicy `json:"pullPolicy,omitempty"`

	// PullRetryLimit is how many times a failed source image pull is retried, with exponential
	// backoff between attempts. Set to zero to fail on the first error. If omitted, a small
	// default number of retries is used to absorb transient registry and network failures.
	// +kubebuilder:validation:Minimum=0
	// +optional
	PullRetryLimit *int32 `json:"pullRetryLimit,omitempty"`
}

// PhysicalContainerImageStatus describes the observed runtime image.
// +k8s:openapi-gen=true
type PhysicalContainerImageStatus struct {
	// Phase summarizes whether the image is available.
	// +kubebuilder:validation:Enum=Pending;Ready;Unknown;Failed
	// +optional
	Phase PhysicalContainerImagePhase `json:"phase,omitempty"`

	// Image is the image reference that containers should use.
	Image string `json:"image,omitempty"`

	// ImageID is the runtime image ID.
	ImageID string `json:"imageID,omitempty"`

	// Digest is the runtime image digest, when available.
	Digest string `json:"digest,omitempty"`

	// Tags are the tags observed on the runtime image.
	// +listType=set
	Tags []string `json:"tags,omitempty"`

	// Conditions describe readiness and reconciliation progress.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func (pcis PhysicalContainerImageStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	pcis.DeepCopyInto(&dest.(*PhysicalContainerImage).Status)
}

// PhysicalContainerImage represents a runtime image needed by V2 PhysicalContainers.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=physicalcontainerimages,shortName=pci
type PhysicalContainerImage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PhysicalContainerImageSpec   `json:"spec,omitempty"`
	Status PhysicalContainerImageStatus `json:"status,omitempty"`
}

func (pci *PhysicalContainerImage) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "physicalcontainerimages",
	}
}

func (pci *PhysicalContainerImage) GetObjectMeta() *metav1.ObjectMeta {
	return &pci.ObjectMeta
}

func (pci *PhysicalContainerImage) GetStatus() apiserver_resource.StatusSubResource {
	return pci.Status
}

func (pci *PhysicalContainerImage) New() runtime.Object {
	return &PhysicalContainerImage{}
}

func (pci *PhysicalContainerImage) NewList() runtime.Object {
	return &PhysicalContainerImageList{}
}

func (pci *PhysicalContainerImage) IsStorageVersion() bool {
	return true
}

func (pci *PhysicalContainerImage) NamespaceScoped() bool {
	return true
}

func (pci *PhysicalContainerImage) ShortNames() []string {
	return []string{"pci"}
}

func (pci *PhysicalContainerImage) NamespacedName() types.NamespacedName {
	return NamespacedName(pci)
}

func (pci *PhysicalContainerImage) Validate(ctx context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(pci)
	specPath := field.NewPath("spec")

	if commonapi.ResourceCreationProhibited.Load() && pci.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}

	errorList = append(errorList, commonapi.ValidateAnnotationsSize(pci.Annotations, field.NewPath("metadata", "annotations"))...)

	if pci.Spec.Image == "" && pci.Spec.Build == nil {
		errorList = append(errorList, field.Required(specPath.Child("image"), "image or build must be set"))
	}
	if pci.Spec.Image != "" && strings.ContainsAny(pci.Spec.Image, "\r\n\t ") {
		errorList = append(errorList, field.Invalid(specPath.Child("image"), pci.Spec.Image, "image must not contain whitespace or control characters"))
	}

	switch pci.Spec.PullPolicy {
	case "", PullPolicyAlways, PullPolicyMissing, PullPolicyNever:
	default:
		errorList = append(errorList, field.NotSupported(specPath.Child("pullPolicy"), pci.Spec.PullPolicy, []string{
			string(PullPolicyAlways),
			string(PullPolicyMissing),
			string(PullPolicyNever),
		}))
	}

	if pci.Spec.PullRetryLimit != nil && *pci.Spec.PullRetryLimit < 0 {
		errorList = append(errorList, field.Invalid(specPath.Child("pullRetryLimit"), *pci.Spec.PullRetryLimit, "pullRetryLimit must not be negative"))
	}

	if pci.Spec.Build != nil {
		if pci.Spec.PullPolicy == PullPolicyNever {
			errorList = append(errorList, field.Invalid(specPath.Child("pullPolicy"), pci.Spec.PullPolicy, "pullPolicy never is not supported for image builds"))
		}
		errorList = append(errorList, validatePhysicalContainerImageBuild(pci.Spec.Build, specPath.Child("build"))...)
	}

	return errorList
}

func (pci *PhysicalContainerImage) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldImage := old.(*PhysicalContainerImage)
	if !reflect.DeepEqual(oldImage.Spec, pci.Spec) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec"), "spec is immutable"))
	}

	return errorList
}

func validatePhysicalContainerImageBuild(build *ContainerBuildContext, buildPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}

	if build.Context == "" {
		errorList = append(errorList, field.Required(buildPath.Child("context"), "context is required"))
	}
	for i, tag := range build.Tags {
		if tag == "" || strings.ContainsAny(tag, "\r\n\t ") {
			errorList = append(errorList, field.Invalid(buildPath.Child("tags").Index(i), tag, "tag must be non-empty and must not contain whitespace or control characters"))
		}
	}
	for i, secret := range build.Secrets {
		secretPath := buildPath.Child("secrets").Index(i)
		if secret.ID == "" {
			errorList = append(errorList, field.Required(secretPath.Child("id"), "id is required"))
		}
		switch secret.Type {
		case "", FileSecret, EnvSecret:
		default:
			errorList = append(errorList, field.NotSupported(secretPath.Child("type"), secret.Type, []string{
				string(FileSecret),
				string(EnvSecret),
			}))
		}
		if secret.Type != EnvSecret && secret.Source == "" {
			errorList = append(errorList, field.Required(secretPath.Child("source"), "source must be set to a non-empty value"))
		}
	}
	errorList = append(errorList, validateLabels(build.Labels, buildPath.Child("labels"))...)

	return errorList
}

func validateLabels(labels []commonapi.Label, labelsPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}
	for i, label := range labels {
		labelPath := labelsPath.Index(i)
		if label.Key == "" {
			errorList = append(errorList, field.Required(labelPath.Child("key"), "key must be set to a non-empty value"))
		}
		if label.Value == "" {
			errorList = append(errorList, field.Required(labelPath.Child("value"), "value must be set to a non-empty value"))
		}
	}
	return errorList
}

// PhysicalContainerImageList contains a list of PhysicalContainerImage instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type PhysicalContainerImageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PhysicalContainerImage `json:"items"`
}

func (pcil *PhysicalContainerImageList) GetListMeta() *metav1.ListMeta {
	return &pcil.ListMeta
}

func (pcil *PhysicalContainerImageList) ItemCount() uint32 {
	return uint32(len(pcil.Items))
}

func (pcil *PhysicalContainerImageList) GetItems() []*PhysicalContainerImage {
	retval := make([]*PhysicalContainerImage, len(pcil.Items))
	for i := range pcil.Items {
		retval[i] = &pcil.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&PhysicalContainerImage{}, &PhysicalContainerImageList{})
}

// Ensure types support interfaces expected by our API server.
var _ apiserver_resource.Object = (*PhysicalContainerImage)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*PhysicalContainerImage)(nil)
var _ apiserver_resource.StatusSubResource = (*PhysicalContainerImageStatus)(nil)
var _ apiserver_resource.ObjectList = (*PhysicalContainerImageList)(nil)
var _ commonapi.ListWithObjectItems[PhysicalContainerImage, *PhysicalContainerImage] = (*PhysicalContainerImageList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*PhysicalContainerImage)(nil)
var _ apiserver_resourcestrategy.Validater = (*PhysicalContainerImage)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*PhysicalContainerImage)(nil)
