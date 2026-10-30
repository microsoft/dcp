/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"fmt"
	"reflect"

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

// LogicalContainerNetworkMode controls network creation, reuse, and retention.
// +kubebuilder:validation:Enum=session;persistent;existing
type LogicalContainerNetworkMode string

const (
	// LogicalContainerNetworkModeSession creates a network and removes it when the resource is deleted.
	LogicalContainerNetworkModeSession LogicalContainerNetworkMode = "session"

	// LogicalContainerNetworkModePersistent creates or reuses a network and retains it when the resource is deleted.
	LogicalContainerNetworkModePersistent LogicalContainerNetworkMode = "persistent"

	// LogicalContainerNetworkModeExisting observes an existing network without creating or removing it.
	LogicalContainerNetworkModeExisting LogicalContainerNetworkMode = "existing"
)

// LogicalContainerNetworkPhase summarizes the availability of a logical container network.
type LogicalContainerNetworkPhase string

const (
	// LogicalContainerNetworkPhasePending indicates that the network is waiting for prerequisites or creation.
	LogicalContainerNetworkPhasePending LogicalContainerNetworkPhase = "Pending"

	// LogicalContainerNetworkPhaseReady indicates that the network is available for container attachments.
	LogicalContainerNetworkPhaseReady LogicalContainerNetworkPhase = "Ready"

	// LogicalContainerNetworkPhaseUnknown indicates that network availability cannot be determined.
	LogicalContainerNetworkPhaseUnknown LogicalContainerNetworkPhase = "Unknown"

	// LogicalContainerNetworkPhaseFailed indicates a terminal reconciliation failure.
	LogicalContainerNetworkPhaseFailed LogicalContainerNetworkPhase = "Failed"
)

// LogicalContainerNetworkSpec describes network lifecycle policy and desired configuration.
// +k8s:openapi-gen=true
type LogicalContainerNetworkSpec struct {
	// NetworkName is the runtime name to create or look up. Required for persistent and existing modes.
	// If omitted in session mode, a runtime name is generated.
	NetworkName string `json:"networkName,omitempty"`

	// Mode controls creation, reuse, and retention. Defaults to session.
	// +kubebuilder:default:=session
	Mode LogicalContainerNetworkMode `json:"mode,omitempty"`

	// Network describes the desired network. Required for session and persistent modes,
	// and forbidden for existing mode.
	Network *LogicalContainerNetworkConfig `json:"network,omitempty"`
}

// EffectiveMode returns the configured lifecycle mode, defaulting to session.
func (spec LogicalContainerNetworkSpec) EffectiveMode() LogicalContainerNetworkMode {
	if spec.Mode == "" {
		return LogicalContainerNetworkModeSession
	}
	return spec.Mode
}

// LogicalContainerNetworkConfig describes the desired configuration used to create a network.
// +k8s:openapi-gen=true
type LogicalContainerNetworkConfig struct {
	// IPv6 enables IPv6 on a newly created runtime network.
	IPv6 bool `json:"ipv6,omitempty"`

	// Labels contains labels to apply to a newly-created runtime network.
	// DCP lifecycle and physical resource ownership labels are reserved and set by controllers.
	// +listType=map
	// +listMapKey=key
	Labels []commonapi.Label `json:"labels,omitempty"`
}

// LogicalContainerNetworkStatus describes the observed network and its physical backing resource.
// +k8s:openapi-gen=true
type LogicalContainerNetworkStatus struct {
	// Phase summarizes network availability.
	// +kubebuilder:validation:Enum=Pending;Ready;Unknown;Failed
	// +optional
	Phase LogicalContainerNetworkPhase `json:"phase,omitempty"`

	// PhysicalNetworkRef identifies the backing PhysicalContainerNetwork using <name> or <namespace>/<name>.
	// Cross-namespace references are not supported.
	PhysicalNetworkRef string `json:"physicalNetworkRef,omitempty"`

	// NetworkID is the observed runtime network ID.
	NetworkID string `json:"networkID,omitempty"`

	// NetworkName is the observed runtime network name.
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

	// ContainerIDs are the runtime container IDs currently attached to the network.
	// +listType=set
	ContainerIDs []string `json:"containerIDs,omitempty"`

	// CreatedAt is the runtime network creation timestamp.
	CreatedAt metav1.MicroTime `json:"createdAt,omitempty"`

	// Conditions describe readiness and reconciliation progress.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func (status LogicalContainerNetworkStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	status.DeepCopyInto(&dest.(*LogicalContainerNetwork).Status)
}

// LogicalContainerNetwork represents container network lifecycle policy independent of container lifetimes.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=logicalcontainernetworks,shortName=lcn
type LogicalContainerNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LogicalContainerNetworkSpec   `json:"spec,omitempty"`
	Status LogicalContainerNetworkStatus `json:"status,omitempty"`
}

func (network *LogicalContainerNetwork) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupVersion.Group, Version: GroupVersion.Version, Resource: "logicalcontainernetworks"}
}

func (network *LogicalContainerNetwork) GetObjectMeta() *metav1.ObjectMeta {
	return &network.ObjectMeta
}

func (network *LogicalContainerNetwork) GetStatus() apiserver_resource.StatusSubResource {
	return network.Status
}

func (network *LogicalContainerNetwork) New() runtime.Object {
	return &LogicalContainerNetwork{}
}

func (network *LogicalContainerNetwork) NewList() runtime.Object {
	return &LogicalContainerNetworkList{}
}

func (network *LogicalContainerNetwork) IsStorageVersion() bool {
	return true
}

func (network *LogicalContainerNetwork) NamespaceScoped() bool {
	return true
}

func (network *LogicalContainerNetwork) ShortNames() []string {
	return []string{"lcn"}
}

func (network *LogicalContainerNetwork) NamespacedName() types.NamespacedName {
	return NamespacedName(network)
}

func (network *LogicalContainerNetwork) Validate(ctx context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(network)
	specPath := field.NewPath("spec")
	if commonapi.ResourceCreationProhibited.Load() && network.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}
	errorList = append(errorList, commonapi.ValidateAnnotationsSize(network.Annotations, field.NewPath("metadata", "annotations"))...)

	mode := network.Spec.EffectiveMode()
	switch mode {
	case LogicalContainerNetworkModeSession, LogicalContainerNetworkModePersistent:
		if network.Spec.Network == nil {
			errorList = append(errorList, field.Required(specPath.Child("network"), "network must be set for session and persistent networks"))
		}
	case LogicalContainerNetworkModeExisting:
		if network.Spec.Network != nil {
			errorList = append(errorList, field.Forbidden(specPath.Child("network"), "network cannot be set for existing networks"))
		}
	default:
		errorList = append(errorList, field.NotSupported(specPath.Child("mode"), network.Spec.Mode, []string{
			string(LogicalContainerNetworkModeSession), string(LogicalContainerNetworkModePersistent), string(LogicalContainerNetworkModeExisting),
		}))
	}
	if (mode == LogicalContainerNetworkModePersistent || mode == LogicalContainerNetworkModeExisting) && network.Spec.NetworkName == "" {
		errorList = append(errorList, field.Required(specPath.Child("networkName"), "networkName must be set for persistent and existing networks"))
	}
	if network.Spec.NetworkName != "" && !validNetworkNameRegexp.MatchString(network.Spec.NetworkName) {
		errorList = append(errorList, field.Invalid(specPath.Child("networkName"), network.Spec.NetworkName, fmt.Sprintf("networkName must match regex '%s'", validNetworkName)))
	}
	if network.Spec.Network != nil {
		errorList = append(errorList, validateLabels(network.Spec.Network.Labels, specPath.Child("network", "labels"))...)
	}
	return errorList
}

// ValidateUpdate freezes the spec while allowing status and metadata updates, including during shutdown.
func (network *LogicalContainerNetwork) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}
	oldNetwork := old.(*LogicalContainerNetwork)
	if !reflect.DeepEqual(oldNetwork.Spec, network.Spec) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec"), "spec is immutable"))
	}
	return errorList
}

// LogicalContainerNetworkList contains a list of LogicalContainerNetwork instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type LogicalContainerNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LogicalContainerNetwork `json:"items"`
}

func (list *LogicalContainerNetworkList) GetListMeta() *metav1.ListMeta {
	return &list.ListMeta
}

func (list *LogicalContainerNetworkList) ItemCount() uint32 {
	return uint32(len(list.Items))
}

func (list *LogicalContainerNetworkList) GetItems() []*LogicalContainerNetwork {
	items := make([]*LogicalContainerNetwork, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}
	return items
}

func init() {
	SchemeBuilder.Register(&LogicalContainerNetwork{}, &LogicalContainerNetworkList{})
}

var _ apiserver_resource.Object = (*LogicalContainerNetwork)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*LogicalContainerNetwork)(nil)
var _ apiserver_resource.StatusSubResource = (*LogicalContainerNetworkStatus)(nil)
var _ apiserver_resource.ObjectList = (*LogicalContainerNetworkList)(nil)
var _ commonapi.ListWithObjectItems[LogicalContainerNetwork, *LogicalContainerNetwork] = (*LogicalContainerNetworkList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*LogicalContainerNetwork)(nil)
var _ apiserver_resourcestrategy.Validater = (*LogicalContainerNetwork)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*LogicalContainerNetwork)(nil)
