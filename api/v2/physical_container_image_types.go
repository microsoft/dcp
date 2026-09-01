/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"regexp"
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

var validSHA256HexRegexp = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

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

	// PhysicalContainerImageReasonImageAvailable indicates that the image is available to the container runtime.
	PhysicalContainerImageReasonImageAvailable ConditionReason = "ImageAvailable"

	// PhysicalContainerImageReasonRuntimeImageInspectFailed indicates that the runtime image could not be inspected.
	PhysicalContainerImageReasonRuntimeImageInspectFailed ConditionReason = "RuntimeImageInspectFailed"

	// PhysicalContainerImageReasonLocalImageNotFound indicates that an image required to exist locally was not found.
	PhysicalContainerImageReasonLocalImageNotFound ConditionReason = "LocalImageNotFound"

	// PhysicalContainerImageReasonPullResultMissingImageID indicates that a completed pull returned no image ID.
	PhysicalContainerImageReasonPullResultMissingImageID ConditionReason = "PullResultMissingImageID"

	// PhysicalContainerImageReasonBuildResultMissingImageID indicates that a completed build did not produce an image ID.
	PhysicalContainerImageReasonBuildResultMissingImageID ConditionReason = "BuildResultMissingImageID"
)

// PhysicalContainerImageSpec describes either an existing runtime image or how to create one.
// +k8s:openapi-gen=true
type PhysicalContainerImageSpec struct {
	// ImageID identifies an existing runtime image to track. Exactly one of imageID or image must be set.
	ImageID string `json:"imageID,omitempty"`

	// Image describes a runtime image to pull or build. Exactly one of imageID or image must be set.
	Image *PhysicalContainerImageConfig `json:"image,omitempty"`
}

// PhysicalContainerImageConfig describes a runtime image to pull or build.
// +k8s:openapi-gen=true
type PhysicalContainerImageConfig struct {
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

	if pci.Spec.ImageID == "" && pci.Spec.Image == nil {
		errorList = append(errorList, field.Required(specPath, "exactly one of imageID or image must be set"))
		return errorList
	}
	if pci.Spec.ImageID != "" && pci.Spec.Image != nil {
		errorList = append(errorList, field.Forbidden(specPath.Child("image"), "image cannot be set when imageID is set"))
		return errorList
	}
	if pci.Spec.Image == nil {
		return errorList
	}

	image := pci.Spec.Image
	imagePath := specPath.Child("image")
	if image.Image == "" && image.Build == nil {
		errorList = append(errorList, field.Required(imagePath.Child("image"), "image or build must be set"))
	}
	if image.Image != "" && strings.ContainsAny(image.Image, "\r\n\t ") {
		errorList = append(errorList, field.Invalid(imagePath.Child("image"), image.Image, "image must not contain whitespace or control characters"))
	}

	switch image.PullPolicy {
	case "", PullPolicyAlways, PullPolicyMissing, PullPolicyNever:
	default:
		errorList = append(errorList, field.NotSupported(imagePath.Child("pullPolicy"), image.PullPolicy, []string{
			string(PullPolicyAlways),
			string(PullPolicyMissing),
			string(PullPolicyNever),
		}))
	}

	if image.PullRetryLimit != nil && *image.PullRetryLimit < 0 {
		errorList = append(errorList, field.Invalid(imagePath.Child("pullRetryLimit"), *image.PullRetryLimit, "pullRetryLimit must not be negative"))
	}

	if image.Build != nil {
		if image.PullPolicy == PullPolicyNever {
			errorList = append(errorList, field.Invalid(imagePath.Child("pullPolicy"), image.PullPolicy, "pullPolicy never is not supported for image builds"))
		}
		errorList = append(errorList, validatePhysicalContainerImageBuild(image.Build, imagePath.Child("build"))...)
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

	if build.Context == "" && build.ContextArchive == nil {
		errorList = append(errorList, field.Required(buildPath, "exactly one of context or contextArchive is required"))
	}
	if build.Context != "" && build.ContextArchive != nil {
		errorList = append(errorList, field.Forbidden(buildPath.Child("contextArchive"), "contextArchive cannot be set when context is set"))
	}
	if build.ContextArchive != nil {
		archive := build.ContextArchive
		archivePath := buildPath.Child("contextArchive")
		if archive.Digest == "" {
			errorList = append(errorList, field.Required(archivePath.Child("digest"), "digest must be set to a non-empty value"))
		}
		if archive.Source == "" && archive.RawContents == "" {
			errorList = append(errorList, field.Required(archivePath, "either source or rawContents must be set"))
		}
		if archive.Source != "" && archive.RawContents != "" {
			errorList = append(errorList, field.Forbidden(archivePath.Child("rawContents"), "source and rawContents cannot be set at the same time"))
		}
		if archive.Source != "" && archive.SHA256 == "" {
			errorList = append(errorList, field.Required(archivePath.Child("sha256"), "sha256 must be set when source is specified"))
		}
		if archive.SHA256 != "" && archive.Source == "" {
			errorList = append(errorList, field.Forbidden(archivePath.Child("sha256"), "sha256 can only be set when source is specified"))
		}
		if archive.SHA256 != "" {
			hexPart := archive.SHA256
			if strings.HasPrefix(strings.ToLower(hexPart), "sha256:") {
				hexPart = hexPart[7:]
			}
			if !validSHA256HexRegexp.MatchString(hexPart) {
				errorList = append(errorList, field.Invalid(archivePath.Child("sha256"), archive.SHA256, "sha256 must be a 64-character hex string, optionally prefixed with 'sha256:'"))
			}
		}
		if archive.RawContents != "" {
			if _, decodeErr := base64.StdEncoding.DecodeString(archive.RawContents); decodeErr != nil {
				errorList = append(errorList, field.Invalid(archivePath.Child("rawContents"), "<base64 data>", fmt.Sprintf("rawContents must be valid base64: %s", decodeErr.Error())))
			}
		}
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
