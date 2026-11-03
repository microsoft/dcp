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

// LogicalServiceMode controls whether DCP allocates a service or describes an existing one.
// +kubebuilder:validation:Enum=session;existing
type LogicalServiceMode string

const (
	// LogicalServiceModeSession allocates a service and releases its listeners when the resource is deleted.
	LogicalServiceModeSession LogicalServiceMode = "session"

	// LogicalServiceModeExisting describes supplied addresses without allocating listeners or managing their lifetime.
	LogicalServiceModeExisting LogicalServiceMode = "existing"
)

// LogicalServicePhase summarizes the availability of a logical service.
type LogicalServicePhase string

const (
	// LogicalServicePhasePending indicates that the service is waiting for allocation or endpoints.
	LogicalServicePhasePending LogicalServicePhase = "Pending"

	// LogicalServicePhaseReady indicates that the service has usable addresses and, for session mode, an endpoint.
	// It does not indicate that network reachability has been probed.
	LogicalServicePhaseReady LogicalServicePhase = "Ready"

	// LogicalServicePhaseUnknown indicates that service availability cannot be determined.
	LogicalServicePhaseUnknown LogicalServicePhase = "Unknown"

	// LogicalServicePhaseFailed indicates a terminal reconciliation failure.
	LogicalServicePhaseFailed LogicalServicePhase = "Failed"
)

// ServiceAddressAllocationMode controls listener address selection for session services.
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

// LogicalServiceSpec describes service lifecycle policy and requested addresses.
// +k8s:openapi-gen=true
type LogicalServiceSpec struct {
	LogicalServiceConfig `json:",inline"`

	// Mode controls address allocation and ownership. Defaults to session.
	// +kubebuilder:default:=session
	Mode LogicalServiceMode `json:"mode,omitempty"`

	// Addresses are the predefined addresses in existing mode; forbidden in session mode.
	// They require no workload or endpoint resource and are published without reachability probes.
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	Addresses []ServiceAddress `json:"addresses,omitempty"`
}

// LogicalServiceConfig describes listener allocation or endpoint selection for a session service.
// +k8s:openapi-gen=true
type LogicalServiceConfig struct {
	// Protocol is the transport protocol, defaulting to TCP.
	// +kubebuilder:validation:Enum=TCP;UDP
	// +kubebuilder:default:=TCP
	Protocol commonapi.PortProtocol `json:"protocol,omitempty"`

	// Address is the requested IP address or hostname. If set, it overrides addressAllocationMode,
	// except in Proxyless mode, where address and port are ignored.
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

// EffectiveMode returns the configured lifecycle mode, defaulting to session.
func (spec LogicalServiceSpec) EffectiveMode() LogicalServiceMode {
	if spec.Mode == "" {
		return LogicalServiceModeSession
	}
	return spec.Mode
}

// EffectiveProtocol returns the configured transport protocol, defaulting to TCP.
func (config LogicalServiceConfig) EffectiveProtocol() commonapi.PortProtocol {
	if config.Protocol == "" {
		return commonapi.PortProtocolTCP
	}
	return config.Protocol
}

// Equal compares requested session configuration after normalizing transport and allocation defaults.
func (config LogicalServiceConfig) Equal(other LogicalServiceConfig) bool {
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
func (config *LogicalServiceConfig) Validate(configPath *field.Path) field.ErrorList {
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
	// Phase summarizes service availability without implying a network reachability probe.
	// +kubebuilder:validation:Enum=Pending;Ready;Unknown;Failed
	Phase LogicalServicePhase `json:"phase,omitempty"`

	// EffectiveAddresses contains the usable addresses and their individual ports.
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

// LogicalService represents a session-scoped service or predefined addresses independent of a workload.
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

	switch service.Spec.EffectiveMode() {
	case LogicalServiceModeSession:
		if len(service.Spec.Addresses) != 0 {
			errorList = append(errorList, field.Forbidden(specPath.Child("addresses"), "addresses can only be set in existing mode"))
		}
	case LogicalServiceModeExisting:
		if service.Spec.Address != "" {
			errorList = append(errorList, field.Forbidden(specPath.Child("address"), "address cannot be set in existing mode; use addresses"))
		}
		if service.Spec.Port != 0 {
			errorList = append(errorList, field.Forbidden(specPath.Child("port"), "port cannot be set in existing mode; use addresses"))
		}
		if service.Spec.AddressAllocationMode != "" {
			errorList = append(errorList, field.Forbidden(specPath.Child("addressAllocationMode"), "addressAllocationMode cannot be set in existing mode"))
		}
		errorList = append(errorList, validateServiceAddresses(service.Spec.Addresses, specPath.Child("addresses"))...)
	default:
		errorList = append(errorList, field.NotSupported(specPath.Child("mode"), service.Spec.Mode, []string{
			string(LogicalServiceModeSession), string(LogicalServiceModeExisting),
		}))
	}
	errorList = append(errorList, service.Spec.LogicalServiceConfig.Validate(specPath)...)
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
