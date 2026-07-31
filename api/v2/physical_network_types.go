/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"fmt"
	"reflect"
	"regexp"

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

var (
	// Network names are validated separately from container names. Podman enforces
	// https://github.com/containers/common/blob/main/libnetwork/types/define.go (NameRegex), which allows
	// a single-character name, whereas the Docker container name pattern requires at least two characters.
	// Docker itself applies no pattern to network names, so this is the set both runtimes accept.
	validNetworkName       = `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`
	validNetworkNameRegexp = regexp.MustCompile(validNetworkName)
)

// PhysicalNetworkPhase describes the lifecycle phase of a PhysicalNetwork.
type PhysicalNetworkPhase string

const (
	// PhysicalNetworkPhasePending indicates that the network is waiting for prerequisites.
	PhysicalNetworkPhasePending PhysicalNetworkPhase = "Pending"

	// PhysicalNetworkPhaseReady indicates that the runtime network is available.
	PhysicalNetworkPhaseReady PhysicalNetworkPhase = "Ready"

	// PhysicalNetworkPhaseMissing indicates that the referenced runtime network was not found.
	PhysicalNetworkPhaseMissing PhysicalNetworkPhase = "Missing"

	// PhysicalNetworkPhaseFailed indicates that creating or inspecting the runtime network failed.
	PhysicalNetworkPhaseFailed PhysicalNetworkPhase = "Failed"
)

const (
	// PhysicalNetworkReasonPending indicates that the network is waiting for prerequisites.
	PhysicalNetworkReasonPending string = "Pending"

	// PhysicalNetworkReasonCreating indicates that runtime network creation is in progress.
	PhysicalNetworkReasonCreating string = "Creating"

	// PhysicalNetworkReasonCreated indicates that runtime network creation completed.
	PhysicalNetworkReasonCreated string = "Created"

	// PhysicalNetworkReasonCreateFailed indicates that runtime network creation failed.
	PhysicalNetworkReasonCreateFailed string = "CreateFailed"

	// PhysicalNetworkReasonNetworkReady indicates that the runtime network is available.
	PhysicalNetworkReasonNetworkReady string = "NetworkReady"

	// PhysicalNetworkReasonRuntimeNetworkMissing indicates that the runtime network was not found.
	PhysicalNetworkReasonRuntimeNetworkMissing string = "RuntimeNetworkMissing"

	// PhysicalNetworkReasonReconciliationFailed indicates that reconciliation failed outside a specific progress gate.
	PhysicalNetworkReasonReconciliationFailed string = "ReconciliationFailed"
)

// PhysicalNetworkSpec describes either an existing runtime network or how to create one.
// +k8s:openapi-gen=true
type PhysicalNetworkSpec struct {
	// NetworkID identifies an existing runtime network to track. When set, creation fields are forbidden.
	NetworkID string `json:"networkID,omitempty"`

	// NetworkName is the runtime name to use when creating a new network. Required when networkID is omitted.
	NetworkName string `json:"networkName,omitempty"`

	// IPv6 enables IPv6 on a newly created runtime network.
	IPv6 bool `json:"ipv6,omitempty"`

	// PreserveOnDeletion keeps the runtime network in place when this resource is deleted.
	// By default the runtime network is removed, including when this resource only tracks a
	// network it did not create.
	PreserveOnDeletion bool `json:"preserveOnDeletion,omitempty"`

	// Labels contains labels to apply to a newly-created runtime network.
	// +listType=map
	// +listMapKey=key
	Labels []commonapi.Label `json:"labels,omitempty"`
}

// PhysicalNetworkStatus describes the observed runtime network.
// +k8s:openapi-gen=true
type PhysicalNetworkStatus struct {
	// Phase summarizes whether the runtime network is available.
	// +kubebuilder:validation:Enum=Pending;Ready;Missing;Failed
	// +optional
	Phase PhysicalNetworkPhase `json:"phase,omitempty"`

	// NetworkID is the runtime network ID being tracked.
	NetworkID string `json:"networkID,omitempty"`

	// NetworkName is the runtime network name.
	NetworkName string `json:"networkName,omitempty"`

	// Driver is the runtime network driver.
	Driver string `json:"driver,omitempty"`

	// IPv6 reports whether IPv6 is enabled on the runtime network.
	IPv6 bool `json:"ipv6,omitempty"`

	// Subnets are the subnets allocated to the runtime network.
	// +listType=set
	Subnets []string `json:"subnets,omitempty"`

	// Gateways are the gateways allocated to the runtime network.
	// +listType=set
	Gateways []string `json:"gateways,omitempty"`

	// CreatedAt is the runtime network creation timestamp.
	CreatedAt metav1.MicroTime `json:"createdAt,omitempty"`

	// Conditions describe readiness and reconciliation progress.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func (pns PhysicalNetworkStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	pns.DeepCopyInto(&dest.(*PhysicalNetwork).Status)
}

// PhysicalNetwork represents one runtime container network in a DCP V2 namespace.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=physicalnetworks,shortName=pnet
type PhysicalNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PhysicalNetworkSpec   `json:"spec,omitempty"`
	Status PhysicalNetworkStatus `json:"status,omitempty"`
}

func (pn *PhysicalNetwork) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "physicalnetworks",
	}
}

func (pn *PhysicalNetwork) GetObjectMeta() *metav1.ObjectMeta {
	return &pn.ObjectMeta
}

func (pn *PhysicalNetwork) GetStatus() apiserver_resource.StatusSubResource {
	return pn.Status
}

func (pn *PhysicalNetwork) New() runtime.Object {
	return &PhysicalNetwork{}
}

func (pn *PhysicalNetwork) NewList() runtime.Object {
	return &PhysicalNetworkList{}
}

func (pn *PhysicalNetwork) IsStorageVersion() bool {
	return true
}

func (pn *PhysicalNetwork) NamespaceScoped() bool {
	return true
}

func (pn *PhysicalNetwork) ShortNames() []string {
	return []string{"pnet"}
}

func (pn *PhysicalNetwork) NamespacedName() types.NamespacedName {
	return NamespacedName(pn)
}

func (pn *PhysicalNetwork) Validate(ctx context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(pn)
	specPath := field.NewPath("spec")

	if commonapi.ResourceCreationProhibited.Load() && pn.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}

	errorList = append(errorList, commonapi.ValidateAnnotationsSize(pn.Annotations, field.NewPath("metadata", "annotations"))...)

	if pn.Spec.NetworkID != "" {
		errorList = append(errorList, pn.validateExistingNetworkSpec(specPath)...)
		return errorList
	}

	if pn.Spec.NetworkName == "" {
		errorList = append(errorList, field.Required(specPath.Child("networkName"), "networkName must be set when networkID is omitted"))
	} else if !validNetworkNameRegexp.MatchString(pn.Spec.NetworkName) {
		errorList = append(errorList, field.Invalid(specPath.Child("networkName"), pn.Spec.NetworkName, fmt.Sprintf("networkName must match regex '%s'", validNetworkName)))
	}

	errorList = append(errorList, validateLabels(pn.Spec.Labels, specPath.Child("labels"))...)

	return errorList
}

// ValidateUpdate freezes the spec of an existing PhysicalNetwork.
//
// This runs for status writes as well as user-initiated updates, so it must not re-run
// creation-only checks. In particular the ResourceCreationProhibited gate in Validate is
// deliberately absent: applying it here would block the controller from recording status
// or removing its finalizer during shutdown.
func (pn *PhysicalNetwork) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldPhysicalNetwork := old.(*PhysicalNetwork)
	if !reflect.DeepEqual(oldPhysicalNetwork.Spec, pn.Spec) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec"), "spec is immutable"))
	}

	return errorList
}

func (pn *PhysicalNetwork) validateExistingNetworkSpec(specPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}

	if pn.Spec.NetworkName != "" {
		errorList = append(errorList, field.Forbidden(specPath.Child("networkName"), "networkName cannot be set when networkID is set"))
	}
	if pn.Spec.IPv6 {
		errorList = append(errorList, field.Forbidden(specPath.Child("ipv6"), "ipv6 cannot be set when networkID is set"))
	}
	if len(pn.Spec.Labels) > 0 {
		errorList = append(errorList, field.Forbidden(specPath.Child("labels"), "labels cannot be set when networkID is set"))
	}

	return errorList
}

// PhysicalNetworkList contains a list of PhysicalNetwork instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type PhysicalNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PhysicalNetwork `json:"items"`
}

func (pnl *PhysicalNetworkList) GetListMeta() *metav1.ListMeta {
	return &pnl.ListMeta
}

func (pnl *PhysicalNetworkList) ItemCount() uint32 {
	return uint32(len(pnl.Items))
}

func (pnl *PhysicalNetworkList) GetItems() []*PhysicalNetwork {
	retval := make([]*PhysicalNetwork, len(pnl.Items))
	for i := range pnl.Items {
		retval[i] = &pnl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&PhysicalNetwork{}, &PhysicalNetworkList{})
}

// Ensure types support interfaces expected by our API server.
var _ apiserver_resource.Object = (*PhysicalNetwork)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*PhysicalNetwork)(nil)
var _ apiserver_resource.StatusSubResource = (*PhysicalNetworkStatus)(nil)
var _ apiserver_resource.ObjectList = (*PhysicalNetworkList)(nil)
var _ commonapi.ListWithObjectItems[PhysicalNetwork, *PhysicalNetwork] = (*PhysicalNetworkList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*PhysicalNetwork)(nil)
var _ apiserver_resourcestrategy.Validater = (*PhysicalNetwork)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*PhysicalNetwork)(nil)
