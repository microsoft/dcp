/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"

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

// NamespacePhase describes the lifecycle phase of a Namespace.
type NamespacePhase string

const (
	// NamespaceWorkloadIDAnnotation overrides the global workload ID for V2 resources in the namespace.
	NamespaceWorkloadIDAnnotation = GroupName + "/workload-id"

	// NamespaceFinalizer keeps a Namespace until namespace-scoped child cleanup completes.
	NamespaceFinalizer = GroupName + "/namespace-reconciler"
)

const (
	// NamespacePhaseActive indicates that the namespace can accept V2 resources.
	NamespacePhaseActive NamespacePhase = "Active"

	// NamespacePhaseTerminating indicates that namespace cleanup is in progress.
	NamespacePhaseTerminating NamespacePhase = "Terminating"
)

// NamespaceStatus describes the status of a Namespace.
// +k8s:openapi-gen=true
type NamespaceStatus struct {
	// Phase is the current lifecycle phase of the namespace.
	// +kubebuilder:validation:Enum=Active;Terminating
	// +optional
	Phase NamespacePhase `json:"phase,omitempty"`

	// Conditions describe namespace lifecycle and cleanup progress.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func (ns NamespaceStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	ns.DeepCopyInto(&dest.(*Namespace).Status)
}

// Namespace defines a DCP namespace for V2 resources.
//
// This intentionally mirrors the standard Kubernetes core/v1 Namespace metadata/status shape while
// serving the resource from the DCP V2 API group. DCP uses metadata.finalizers for controller cleanup,
// so it does not expose the legacy spec.finalizers field from core/v1.Namespace.
// https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/namespace-v1/
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type Namespace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Status NamespaceStatus `json:"status,omitempty"`
}

func (ns *Namespace) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "namespaces",
	}
}

func (ns *Namespace) GetObjectMeta() *metav1.ObjectMeta {
	return &ns.ObjectMeta
}

func (ns *Namespace) GetStatus() apiserver_resource.StatusSubResource {
	return ns.Status
}

func (ns *Namespace) New() runtime.Object {
	return &Namespace{}
}

func (ns *Namespace) NewList() runtime.Object {
	return &NamespaceList{}
}

func (ns *Namespace) IsStorageVersion() bool {
	return true
}

func (ns *Namespace) NamespaceScoped() bool {
	return false
}

func (ns *Namespace) ShortNames() []string {
	return []string{"ns"}
}

func (ns *Namespace) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name: ns.Name,
	}
}

func (ns *Namespace) Validate(ctx context.Context) field.ErrorList {
	errorList := field.ErrorList{}
	metadataPath := field.NewPath("metadata")

	if commonapi.ResourceCreationProhibited.Load() && ns.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}

	if ns.Name != "" {
		for _, validationMessage := range validation.IsDNS1123Label(ns.Name) {
			errorList = append(errorList, field.Invalid(metadataPath.Child("name"), ns.Name, validationMessage))
		}
	}

	if ns.Namespace != "" {
		errorList = append(errorList, field.Forbidden(metadataPath.Child("namespace"), "Namespace resources are cluster-scoped"))
	}

	errorList = append(errorList, commonapi.ValidateAnnotationsSize(ns.Annotations, metadataPath.Child("annotations"))...)

	workloadIDText, hasWorkloadID := ns.Annotations[NamespaceWorkloadIDAnnotation]
	if hasWorkloadID {
		normalizedWorkloadID := commonapi.NormalizeWorkloadID(workloadIDText)
		if validationErr := normalizedWorkloadID.Validate(); validationErr != nil {
			errorList = append(errorList, field.Invalid(metadataPath.Child("annotations").Key(NamespaceWorkloadIDAnnotation), workloadIDText, validationErr.Error()))
		}
	}

	return errorList
}

// ValidateUpdate validates mutable metadata on an existing Namespace.
//
// Namespace has no spec, so there is nothing to freeze here; annotations are the only
// mutable input that carries meaning. Note that this runs for status writes as well as
// user-initiated updates, so it must not re-run creation-only checks. In particular the
// ResourceCreationProhibited gate in Validate is deliberately absent: applying it here
// would block the namespace controller from recording cleanup status or removing its
// finalizer during shutdown, which would deadlock namespace deletion.
func (ns *Namespace) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	metadataPath := field.NewPath("metadata")
	errorList := commonapi.ValidateAnnotationsSize(ns.Annotations, metadataPath.Child("annotations"))

	workloadIDText, hasWorkloadID := ns.Annotations[NamespaceWorkloadIDAnnotation]
	if hasWorkloadID {
		normalizedWorkloadID := commonapi.NormalizeWorkloadID(workloadIDText)
		if validationErr := normalizedWorkloadID.Validate(); validationErr != nil {
			errorList = append(errorList, field.Invalid(metadataPath.Child("annotations").Key(NamespaceWorkloadIDAnnotation), workloadIDText, validationErr.Error()))
		}
	}

	return errorList
}

// NamespaceList contains a list of Namespace instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type NamespaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Namespace `json:"items"`
}

func (nsl *NamespaceList) GetListMeta() *metav1.ListMeta {
	return &nsl.ListMeta
}

func (nsl *NamespaceList) ItemCount() uint32 {
	return uint32(len(nsl.Items))
}

func (nsl *NamespaceList) GetItems() []*Namespace {
	retval := make([]*Namespace, len(nsl.Items))
	for i := range nsl.Items {
		retval[i] = &nsl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&Namespace{}, &NamespaceList{})
}

// Ensure types support interfaces expected by our API server.
var _ apiserver_resource.Object = (*Namespace)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*Namespace)(nil)
var _ apiserver_resource.StatusSubResource = (*NamespaceStatus)(nil)
var _ apiserver_resource.ObjectList = (*NamespaceList)(nil)
var _ commonapi.ListWithObjectItems[Namespace, *Namespace] = (*NamespaceList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*Namespace)(nil)
var _ apiserver_resourcestrategy.Validater = (*Namespace)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*Namespace)(nil)
