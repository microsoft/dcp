/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"context"
	"fmt"
	"strings"

	"github.com/microsoft/dcp/internal/statestore"
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

type ContainerNetworkState string

const (
	// The network is being created
	ContainerNetworkStatePending ContainerNetworkState = "Pending"
	// The network was successfully created
	ContainerNetworkStateRunning ContainerNetworkState = "Running"
	// An attempt was made to create the network, but it failed
	ContainerNetworkStateFailedToStart ContainerNetworkState = "FailedToStart"
	// Network was running at some point, but has been removed
	ContainerNetworkStateRemoved ContainerNetworkState = "Removed"
	// An existing network was not found
	ContainerNetworkStateNotFound ContainerNetworkState = "NotFound"
)

// +kubebuilder:validation:Enum=session;persistent;existing;cleanup
type ContainerNetworkMode string

const (
	// ContainerNetworkModeSession creates the network with the ContainerNetwork resource and removes it when the resource is deleted.
	ContainerNetworkModeSession ContainerNetworkMode = "session"
	// ContainerNetworkModePersistent creates or reuses the network and leaves it running when the resource is deleted.
	ContainerNetworkModePersistent ContainerNetworkMode = "persistent"
	// ContainerNetworkModeExisting reuses an existing network but does not create or delete it.
	ContainerNetworkModeExisting ContainerNetworkMode = "existing"
	// ContainerNetworkModeCleanup reuses an existing network without creating it, then removes it when the resource is deleted.
	ContainerNetworkModeCleanup ContainerNetworkMode = "cleanup"
)

var supportedContainerNetworkModes = []string{
	string(ContainerNetworkModeSession),
	string(ContainerNetworkModePersistent),
	string(ContainerNetworkModeExisting),
	string(ContainerNetworkModeCleanup),
}

// ContainerNetworkSpec defines the desired state of a ContainerNetwork
// +k8s:openapi-gen=true
type ContainerNetworkSpec struct {
	// Name of the network (if omitted, a name is generated based on the resource name)
	NetworkName string `json:"networkName,omitempty"`

	// Should IPv6 be enabled for the network?
	IPv6 bool `json:"ipv6,omitempty"`

	// Controls how the network is created, reused, and cleaned up.
	// Ignored when persistent is true.
	Mode ContainerNetworkMode `json:"mode,omitempty"`

	// Should this network be created and persisted between DCP runs?
	Persistent bool `json:"persistent,omitempty"`
}

func (cns ContainerNetworkSpec) EffectiveMode() ContainerNetworkMode {
	if cns.Persistent {
		return ContainerNetworkModePersistent
	}
	if cns.Mode == "" {
		return ContainerNetworkModeSession
	}
	return cns.Mode
}

// ContainerNetworkStatus defines the current state of a ContainerNetwork
// +k8s:openapi-gen=true
type ContainerNetworkStatus struct {
	// The current state of the network
	State ContainerNetworkState `json:"state,omitempty"`

	// If the network is in a failed state, this is the reason
	Message string `json:"message,omitempty"`

	// The ID of the network
	ID string `json:"id,omitempty"`

	// The name of the network
	NetworkName string `json:"networkName,omitempty"`

	// The driver of the network
	Driver string `json:"driver,omitempty"`

	// Does the network support IPv6?
	IPv6 bool `json:"ipv6,omitempty"`

	// Subnets allocated to the network (if any)
	// +listType=set
	Subnets []string `json:"subnets,omitempty"`

	// Gateways allocated to the network (if any)
	// +listType=set
	Gateways []string `json:"gateways,omitempty"`

	// The list of container IDs connected to the network
	// +listType=set
	ContainerIDs []string `json:"containerIds,omitempty"`
}

func (cs ContainerNetworkStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	cs.DeepCopyInto(&dest.(*ContainerNetwork).Status)
}

// ContainerNetwork represents a network that can be consumed by Container instances
// Its lifetime is independent from the lifetime of containers
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type ContainerNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ContainerNetworkSpec   `json:"spec,omitempty"`
	Status ContainerNetworkStatus `json:"status,omitempty"`
}

func (cn *ContainerNetwork) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "containernetworks",
	}
}

func (cn *ContainerNetwork) GetLeaseKey() string {
	return fmt.Sprintf("%s/%s", cn.GetGroupVersionResource().Resource, strings.TrimSpace(cn.Spec.NetworkName))
}

func (cn *ContainerNetwork) GetObjectMeta() *metav1.ObjectMeta {
	return &cn.ObjectMeta
}

func (cn *ContainerNetwork) GetStatus() apiserver_resource.StatusSubResource {
	return cn.Status
}

func (cn *ContainerNetwork) New() runtime.Object {
	return &ContainerNetwork{}
}

func (cn *ContainerNetwork) NewList() runtime.Object {
	return &ContainerNetworkList{}
}

func (cn *ContainerNetwork) IsStorageVersion() bool {
	return true
}

func (cn *ContainerNetwork) NamespaceScoped() bool {
	return false
}

func (cn *ContainerNetwork) ShortNames() []string {
	return []string{"ctrnet"}
}

func (cn *ContainerNetwork) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      cn.Name,
		Namespace: cn.Namespace,
	}
}

func (cn *ContainerNetwork) Validate(ctx context.Context) field.ErrorList {
	errorList := field.ErrorList{}

	if ResourceCreationProhibited.Load() && cn.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, errResourceCreationProhibited.Error()))
	}

	if cn.Spec.Persistent {
		if cn.Spec.NetworkName == "" {
			errorList = append(errorList, field.Required(field.NewPath("spec", "networkName"), "networkName must be set to a value when persistent is true"))
		}
		return errorList
	}

	if cn.Spec.Mode != "" && !containerNetworkModeSupported(cn.Spec.Mode) {
		errorList = append(errorList, field.NotSupported(field.NewPath("spec", "mode"), cn.Spec.Mode, supportedContainerNetworkModes))
	}

	if cn.Spec.EffectiveMode() != ContainerNetworkModeSession && cn.Spec.NetworkName == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec", "networkName"), "networkName must be set to a value when mode requires an existing or persistent network"))
	}

	return errorList
}

func containerNetworkModeSupported(mode ContainerNetworkMode) bool {
	switch mode {
	case ContainerNetworkModeSession,
		ContainerNetworkModePersistent,
		ContainerNetworkModeExisting,
		ContainerNetworkModeCleanup:
		return true
	default:
		return false
	}
}

func (cn *ContainerNetwork) ValidateUpdate(ctx context.Context, obj runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldNetwork := obj.(*ContainerNetwork)
	if oldNetwork.Spec.NetworkName != cn.Spec.NetworkName {
		errorList = append(errorList, field.Invalid(field.NewPath("spec", "name"), cn.Spec.NetworkName, "networkName is immutable"))
	}

	if oldNetwork.Spec.IPv6 != cn.Spec.IPv6 {
		errorList = append(errorList, field.Invalid(field.NewPath("spec", "ipv6"), cn.Spec.IPv6, "ipv6 is immutable"))
	}

	if oldNetwork.Spec.EffectiveMode() != cn.Spec.EffectiveMode() {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "mode"), "mode cannot be changed"))
	}

	// Make sure Persistent isn't changed after the network is created
	if oldNetwork.Spec.Persistent != cn.Spec.Persistent {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "persistent"), "persistent cannot be changed"))
	}

	return errorList
}

// ContainerNetworkList contains a list of ContainerNetwork instances
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type ContainerNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContainerNetwork `json:"items"`
}

func (cnl *ContainerNetworkList) GetListMeta() *metav1.ListMeta {
	return &cnl.ListMeta
}

func (cnl *ContainerNetworkList) ItemCount() uint32 {
	return uint32(len(cnl.Items))
}

func (cnl *ContainerNetworkList) GetItems() []*ContainerNetwork {
	retval := make([]*ContainerNetwork, len(cnl.Items))
	for i := range cnl.Items {
		retval[i] = &cnl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&ContainerNetwork{}, &ContainerNetworkList{})
}

// Ensure types support interfaces expected by our API server
var _ apiserver_resource.Object = (*ContainerNetwork)(nil)
var _ apiserver_resource.ObjectList = (*ContainerNetworkList)(nil)
var _ commonapi.ListWithObjectItems[ContainerNetwork, *ContainerNetwork] = (*ContainerNetworkList)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*ContainerNetwork)(nil)
var _ apiserver_resource.StatusSubResource = (*ContainerNetworkStatus)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*ContainerNetwork)(nil)
var _ apiserver_resourcestrategy.Validater = (*ContainerNetwork)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*ContainerNetwork)(nil)
var _ statestore.LeasableResource = (*ContainerNetwork)(nil)
