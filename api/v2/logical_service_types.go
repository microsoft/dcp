/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
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

// LogicalServicePhase summarizes the lifecycle of a logical service.
type LogicalServicePhase string

const (
	// LogicalServiceReasonStarting qualifies Pending while a serving address and port are unavailable.
	LogicalServiceReasonStarting ConditionReason = "Starting"
	// LogicalServiceReasonAllocated indicates that a serving address and port are known, without guaranteeing service readiness.
	LogicalServiceReasonAllocated ConditionReason = "Allocated"
	// LogicalServiceReasonReady indicates that the service is up and has backing endpoints.
	LogicalServiceReasonReady ConditionReason = "Ready"
	// LogicalServiceReasonFailedToStart indicates that listener startup could not complete.
	LogicalServiceReasonFailedToStart ConditionReason = "FailedToStart"
	// LogicalServiceReasonMissing indicates that allocated listeners are no longer available.
	LogicalServiceReasonMissing ConditionReason = "Missing"
	// LogicalServiceReasonLookupFailed indicates that current availability cannot be confirmed.
	LogicalServiceReasonLookupFailed ConditionReason = "LookupFailed"
)

const (
	// LogicalServicePhasePending indicates that listeners or proxyless endpoint selection are pending.
	LogicalServicePhasePending LogicalServicePhase = "Pending"

	// LogicalServicePhaseRunning indicates that a serving address and port are allocated.
	// The Ready condition uses Allocated until the service is up with backing endpoints, then Ready, without probing reachability.
	LogicalServicePhaseRunning LogicalServicePhase = "Running"

	// LogicalServicePhaseUnknown indicates that service availability cannot be determined.
	LogicalServicePhaseUnknown LogicalServicePhase = "Unknown"

	// LogicalServicePhaseFailed indicates a terminal reconciliation failure.
	LogicalServicePhaseFailed LogicalServicePhase = "Failed"
)

// ServiceAddressAllocationMode controls listener address selection for DCP-managed services.
// +kubebuilder:validation:Enum=Localhost;IPv4ZeroOne;IPv4Loopback;IPv6ZeroOne;AllInterfaces;IPv4AllInterfaces;IPv6AllInterfaces;Proxyless
type ServiceAddressAllocationMode string

const (
	// ServiceAddressAllocationModeLocalhost binds to the available localhost addresses.
	ServiceAddressAllocationModeLocalhost ServiceAddressAllocationMode = "Localhost"

	// ServiceAddressAllocationModeIPv4ZeroOne binds only to 127.0.0.1.
	ServiceAddressAllocationModeIPv4ZeroOne ServiceAddressAllocationMode = "IPv4ZeroOne"

	// ServiceAddressAllocationModeIPv4Loopback selects an available IPv4 loopback address.
	ServiceAddressAllocationModeIPv4Loopback ServiceAddressAllocationMode = "IPv4Loopback"

	// ServiceAddressAllocationModeIPv6ZeroOne binds only to ::1.
	ServiceAddressAllocationModeIPv6ZeroOne ServiceAddressAllocationMode = "IPv6ZeroOne"

	// ServiceAddressAllocationModeAllInterfaces selects interfaces using the host's available addresses.
	ServiceAddressAllocationModeAllInterfaces ServiceAddressAllocationMode = "AllInterfaces"

	// ServiceAddressAllocationModeIPv4AllInterfaces binds to all IPv4 interfaces.
	ServiceAddressAllocationModeIPv4AllInterfaces ServiceAddressAllocationMode = "IPv4AllInterfaces"

	// ServiceAddressAllocationModeIPv6AllInterfaces binds to all IPv6 interfaces.
	ServiceAddressAllocationModeIPv6AllInterfaces ServiceAddressAllocationMode = "IPv6AllInterfaces"

	// ServiceAddressAllocationModeProxyless publishes one endpoint's addresses without allocating listeners.
	ServiceAddressAllocationModeProxyless ServiceAddressAllocationMode = "Proxyless"
)

// LogicalServiceSpec describes a DCP-managed service's listener allocation or endpoint selection.
// +k8s:openapi-gen=true
type LogicalServiceSpec struct {
	// Protocol is the transport protocol, defaulting to TCP.
	// +kubebuilder:validation:Enum=TCP;UDP
	// +kubebuilder:default:=TCP
	Protocol commonapi.PortProtocol `json:"protocol,omitempty"`

	// Address is the requested IP address or hostname. If set, it overrides addressAllocationMode,
	// except in Proxyless mode, where address and port are ignored.
	// IPv6 literals are enclosed in brackets.
	Address string `json:"address,omitempty"`

	// Port is the requested listener port. Zero selects an available port.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// AddressAllocationMode selects listener addresses, defaulting to Localhost.
	// Proxyless selects one ServiceEndpoint and publishes all of its addresses.
	// +kubebuilder:default:=Localhost
	AddressAllocationMode ServiceAddressAllocationMode `json:"addressAllocationMode,omitempty"`
}

// EffectiveProtocol returns the configured transport protocol, defaulting to TCP.
func (config LogicalServiceSpec) EffectiveProtocol() commonapi.PortProtocol {
	if config.Protocol == "" {
		return commonapi.PortProtocolTCP
	}
	return config.Protocol
}

// Equal compares requested service configuration after normalizing transport and allocation defaults.
func (config LogicalServiceSpec) Equal(other LogicalServiceSpec) bool {
	config.Protocol, other.Protocol = config.EffectiveProtocol(), other.EffectiveProtocol()
	if config.AddressAllocationMode == "" {
		config.AddressAllocationMode = ServiceAddressAllocationModeLocalhost
	}
	if other.AddressAllocationMode == "" {
		other.AddressAllocationMode = ServiceAddressAllocationModeLocalhost
	}
	return config == other
}

// Validate checks the requested transport, listener port, and address-allocation mode.
func (config *LogicalServiceSpec) Validate(configPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}
	if config.Protocol != "" && config.Protocol != commonapi.PortProtocolTCP && config.Protocol != commonapi.PortProtocolUDP {
		errorList = append(errorList, field.NotSupported(configPath.Child("protocol"), config.Protocol,
			[]string{string(commonapi.PortProtocolTCP), string(commonapi.PortProtocolUDP)}))
	}
	if config.Port < 0 || config.Port > 65535 {
		errorList = append(errorList, field.Invalid(configPath.Child("port"), config.Port, "port must be between 0 and 65535"))
	}
	switch config.AddressAllocationMode {
	case "", ServiceAddressAllocationModeLocalhost, ServiceAddressAllocationModeIPv4ZeroOne,
		ServiceAddressAllocationModeIPv4Loopback, ServiceAddressAllocationModeIPv6ZeroOne,
		ServiceAddressAllocationModeAllInterfaces, ServiceAddressAllocationModeIPv4AllInterfaces,
		ServiceAddressAllocationModeIPv6AllInterfaces, ServiceAddressAllocationModeProxyless:
	default:
		errorList = append(errorList, field.NotSupported(configPath.Child("addressAllocationMode"), config.AddressAllocationMode, []string{
			string(ServiceAddressAllocationModeLocalhost), string(ServiceAddressAllocationModeIPv4ZeroOne),
			string(ServiceAddressAllocationModeIPv4Loopback), string(ServiceAddressAllocationModeIPv6ZeroOne),
			string(ServiceAddressAllocationModeAllInterfaces), string(ServiceAddressAllocationModeIPv4AllInterfaces),
			string(ServiceAddressAllocationModeIPv6AllInterfaces), string(ServiceAddressAllocationModeProxyless),
		}))
	}
	return errorList
}

// LogicalServiceStatus describes observed service availability and all of its effective addresses.
// +k8s:openapi-gen=true
type LogicalServiceStatus struct {
	// Phase summarizes serving-address allocation and service lifecycle.
	// +kubebuilder:validation:Enum=Pending;Running;Unknown;Failed
	Phase LogicalServicePhase `json:"phase,omitempty"`

	// EffectiveAddresses contains the allocated serving addresses and their individual ports.
	// Allocation does not guarantee service or backend readiness.
	// +listType=atomic
	EffectiveAddresses []ServiceAddress `json:"effectiveAddresses,omitempty"`

	// ProxylessEndpointRef identifies the selected same-namespace ServiceEndpoint in Proxyless mode.
	ProxylessEndpointRef string `json:"proxylessEndpointRef,omitempty"`

	// Conditions describe readiness and reconciliation progress.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func (status LogicalServiceStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	status.DeepCopyInto(&dest.(*LogicalService).Status)
}

// LogicalService represents a DCP-managed service with a lifetime independent of its workloads.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=logicalservices,shortName=lsvc
type LogicalService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LogicalServiceSpec   `json:"spec,omitempty"`
	Status LogicalServiceStatus `json:"status,omitempty"`
}

func (service *LogicalService) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupVersion.Group, Version: GroupVersion.Version, Resource: "logicalservices"}
}

func (service *LogicalService) GetObjectMeta() *metav1.ObjectMeta {
	return &service.ObjectMeta
}

func (service *LogicalService) GetStatus() apiserver_resource.StatusSubResource {
	return service.Status
}

func (service *LogicalService) New() runtime.Object {
	return &LogicalService{}
}

func (service *LogicalService) NewList() runtime.Object {
	return &LogicalServiceList{}
}

func (service *LogicalService) IsStorageVersion() bool {
	return true
}

func (service *LogicalService) NamespaceScoped() bool {
	return true
}

func (service *LogicalService) ShortNames() []string {
	return []string{"lsvc"}
}

func (service *LogicalService) NamespacedName() types.NamespacedName {
	return NamespacedName(service)
}

func (service *LogicalService) Validate(ctx context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(service)
	specPath := field.NewPath("spec")
	if commonapi.ResourceCreationProhibited.Load() && service.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}
	errorList = append(errorList, commonapi.ValidateAnnotationsSize(service.Annotations, field.NewPath("metadata", "annotations"))...)

	errorList = append(errorList, service.Spec.Validate(specPath)...)
	return errorList
}

// ValidateUpdate freezes service configuration while permitting status and metadata updates.
func (service *LogicalService) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	oldService := old.(*LogicalService)
	if !reflect.DeepEqual(oldService.Spec, service.Spec) {
		return field.ErrorList{field.Forbidden(field.NewPath("spec"), "spec is immutable")}
	}
	return nil
}

// LogicalServiceList contains a list of LogicalService instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type LogicalServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LogicalService `json:"items"`
}

func (list *LogicalServiceList) GetListMeta() *metav1.ListMeta {
	return &list.ListMeta
}

func (list *LogicalServiceList) ItemCount() uint32 {
	return uint32(len(list.Items))
}

func (list *LogicalServiceList) GetItems() []*LogicalService {
	items := make([]*LogicalService, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}
	return items
}

func init() {
	SchemeBuilder.Register(&LogicalService{}, &LogicalServiceList{})
}

var _ apiserver_resource.Object = (*LogicalService)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*LogicalService)(nil)
var _ apiserver_resource.StatusSubResource = (*LogicalServiceStatus)(nil)
var _ apiserver_resource.ObjectList = (*LogicalServiceList)(nil)
var _ commonapi.ListWithObjectItems[LogicalService, *LogicalService] = (*LogicalServiceList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*LogicalService)(nil)
var _ apiserver_resourcestrategy.Validater = (*LogicalService)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*LogicalService)(nil)
