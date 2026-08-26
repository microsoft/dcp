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

// PhysicalContainerNetworkPhase describes the lifecycle phase of a PhysicalContainerNetwork.
type PhysicalContainerNetworkPhase string

const (
	// PhysicalContainerNetworkPhasePending indicates that the network is waiting for prerequisites.
	PhysicalContainerNetworkPhasePending PhysicalContainerNetworkPhase = "Pending"

	// PhysicalContainerNetworkPhaseReady indicates that the runtime network is available.
	PhysicalContainerNetworkPhaseReady PhysicalContainerNetworkPhase = "Ready"

	// PhysicalContainerNetworkPhaseMissing indicates that the referenced runtime network was not found.
	PhysicalContainerNetworkPhaseMissing PhysicalContainerNetworkPhase = "Missing"

	// PhysicalContainerNetworkPhaseFailed indicates that creating, inspecting, or removing the runtime network
	// failed, or that its deletion policy is invalid for the observed runtime network.
	PhysicalContainerNetworkPhaseFailed PhysicalContainerNetworkPhase = "Failed"
)

const (
	// PhysicalContainerNetworkReasonCreating indicates that runtime network creation is in progress.
	PhysicalContainerNetworkReasonCreating ConditionReason = "Creating"

	// PhysicalContainerNetworkReasonCreated indicates that runtime network creation completed.
	PhysicalContainerNetworkReasonCreated ConditionReason = "Created"

	// PhysicalContainerNetworkReasonCreateFailed indicates that runtime network creation failed.
	PhysicalContainerNetworkReasonCreateFailed ConditionReason = "CreateFailed"

	// PhysicalContainerNetworkReasonNetworkReady indicates that the runtime network is available.
	PhysicalContainerNetworkReasonNetworkReady ConditionReason = "NetworkReady"

	// PhysicalContainerNetworkReasonRuntimeNetworkMissing indicates that the runtime network was not found.
	PhysicalContainerNetworkReasonRuntimeNetworkMissing ConditionReason = "RuntimeNetworkMissing"

	// PhysicalContainerNetworkReasonRuntimeNetworkRemoveFailed indicates that the runtime network could not be removed.
	PhysicalContainerNetworkReasonRuntimeNetworkRemoveFailed ConditionReason = "RuntimeNetworkRemoveFailed"

	// PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable indicates that replacement targeted a built-in runtime network.
	PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable ConditionReason = "BuiltInNetworkNotRemovable"

	// PhysicalContainerNetworkReasonReconciliationFailed indicates that reconciliation failed outside a specific progress gate.
	PhysicalContainerNetworkReasonReconciliationFailed ConditionReason = "ReconciliationFailed"
)

// PhysicalContainerNetworkSpec describes either an existing runtime network or how to create one.
// +k8s:openapi-gen=true
type PhysicalContainerNetworkSpec struct {
	// NetworkID identifies an existing runtime network to track. Exactly one of networkID or network must be set.
	NetworkID string `json:"networkID,omitempty"`

	// Network describes a runtime network to create. Exactly one of networkID or network must be set.
	Network *PhysicalContainerNetworkConfig `json:"network,omitempty"`
}

// PhysicalContainerNetworkConfig describes a runtime network to create.
// +k8s:openapi-gen=true
type PhysicalContainerNetworkConfig struct {
	// NetworkName is the runtime name to use when creating a new network.
	NetworkName string `json:"networkName,omitempty"`

	// IPv6 enables IPv6 on a newly created runtime network.
	IPv6 bool `json:"ipv6,omitempty"`

	// RetainRuntimeNetwork keeps the created runtime network in place when this resource is deleted.
	RetainRuntimeNetwork bool `json:"retainRuntimeNetwork,omitempty"`

	// ReplaceExisting removes an existing runtime network with networkName before creating a new one.
	// Attached containers are disconnected but are not removed.
	ReplaceExisting bool `json:"replaceExisting,omitempty"`

	// Labels contains labels to apply to a newly-created runtime network.
	// +listType=map
	// +listMapKey=key
	Labels []commonapi.Label `json:"labels,omitempty"`
}

// PhysicalContainerNetworkStatus describes the observed runtime network.
// +k8s:openapi-gen=true
type PhysicalContainerNetworkStatus struct {
	// Phase summarizes whether the runtime network is available.
	// +kubebuilder:validation:Enum=Pending;Ready;Missing;Failed
	// +optional
	Phase PhysicalContainerNetworkPhase `json:"phase,omitempty"`

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

func (pns PhysicalContainerNetworkStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	pns.DeepCopyInto(&dest.(*PhysicalContainerNetwork).Status)
}

// PhysicalContainerNetwork represents one runtime container network in a DCP V2 namespace.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=physicalcontainernetworks,shortName=pcn
type PhysicalContainerNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PhysicalContainerNetworkSpec   `json:"spec,omitempty"`
	Status PhysicalContainerNetworkStatus `json:"status,omitempty"`
}

func (pn *PhysicalContainerNetwork) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "physicalcontainernetworks",
	}
}

func (pn *PhysicalContainerNetwork) GetObjectMeta() *metav1.ObjectMeta {
	return &pn.ObjectMeta
}

func (pn *PhysicalContainerNetwork) GetStatus() apiserver_resource.StatusSubResource {
	return pn.Status
}

func (pn *PhysicalContainerNetwork) New() runtime.Object {
	return &PhysicalContainerNetwork{}
}

func (pn *PhysicalContainerNetwork) NewList() runtime.Object {
	return &PhysicalContainerNetworkList{}
}

func (pn *PhysicalContainerNetwork) IsStorageVersion() bool {
	return true
}

func (pn *PhysicalContainerNetwork) NamespaceScoped() bool {
	return true
}

func (pn *PhysicalContainerNetwork) ShortNames() []string {
	return []string{"pcn"}
}

func (pn *PhysicalContainerNetwork) NamespacedName() types.NamespacedName {
	return NamespacedName(pn)
}

func (pn *PhysicalContainerNetwork) Validate(ctx context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(pn)
	specPath := field.NewPath("spec")

	if commonapi.ResourceCreationProhibited.Load() && pn.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}

	errorList = append(errorList, commonapi.ValidateAnnotationsSize(pn.Annotations, field.NewPath("metadata", "annotations"))...)

	if pn.Spec.NetworkID == "" && pn.Spec.Network == nil {
		errorList = append(errorList, field.Required(specPath, "exactly one of networkID or network must be set"))
		return errorList
	}
	if pn.Spec.NetworkID != "" && pn.Spec.Network != nil {
		errorList = append(errorList, field.Forbidden(specPath.Child("network"), "network cannot be set when networkID is set"))
		return errorList
	}
	if pn.Spec.Network == nil {
		return errorList
	}

	network := pn.Spec.Network
	networkPath := specPath.Child("network")
	if network.NetworkName == "" {
		errorList = append(errorList, field.Required(networkPath.Child("networkName"), "networkName must be set"))
	} else if !validNetworkNameRegexp.MatchString(network.NetworkName) {
		errorList = append(errorList, field.Invalid(networkPath.Child("networkName"), network.NetworkName, fmt.Sprintf("networkName must match regex '%s'", validNetworkName)))
	}

	errorList = append(errorList, validateLabels(network.Labels, networkPath.Child("labels"))...)
	return errorList
}

// ValidateUpdate freezes the spec of an existing PhysicalContainerNetwork.
//
// This runs for status writes as well as user-initiated updates, so it must not re-run
// creation-only checks. In particular the ResourceCreationProhibited gate in Validate is
// deliberately absent: applying it here would block the controller from recording status
// or removing its finalizer during shutdown.
func (pn *PhysicalContainerNetwork) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldPhysicalContainerNetwork := old.(*PhysicalContainerNetwork)
	if !reflect.DeepEqual(oldPhysicalContainerNetwork.Spec, pn.Spec) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec"), "spec is immutable"))
	}

	return errorList
}

// PhysicalContainerNetworkList contains a list of PhysicalContainerNetwork instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type PhysicalContainerNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PhysicalContainerNetwork `json:"items"`
}

func (pnl *PhysicalContainerNetworkList) GetListMeta() *metav1.ListMeta {
	return &pnl.ListMeta
}

func (pnl *PhysicalContainerNetworkList) ItemCount() uint32 {
	return uint32(len(pnl.Items))
}

func (pnl *PhysicalContainerNetworkList) GetItems() []*PhysicalContainerNetwork {
	retval := make([]*PhysicalContainerNetwork, len(pnl.Items))
	for i := range pnl.Items {
		retval[i] = &pnl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&PhysicalContainerNetwork{}, &PhysicalContainerNetworkList{})
}

// Ensure types support interfaces expected by our API server.
var _ apiserver_resource.Object = (*PhysicalContainerNetwork)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*PhysicalContainerNetwork)(nil)
var _ apiserver_resource.StatusSubResource = (*PhysicalContainerNetworkStatus)(nil)
var _ apiserver_resource.ObjectList = (*PhysicalContainerNetworkList)(nil)
var _ commonapi.ListWithObjectItems[PhysicalContainerNetwork, *PhysicalContainerNetwork] = (*PhysicalContainerNetworkList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*PhysicalContainerNetwork)(nil)
var _ apiserver_resourcestrategy.Validater = (*PhysicalContainerNetwork)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*PhysicalContainerNetwork)(nil)
