/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"context"
	"fmt"

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

type ServiceState string

const (
	// The service is not ready to accept connections
	ServiceStateNotReady ServiceState = "NotReady"

	// The service is ready to accept connections
	ServiceStateReady ServiceState = "Ready"
)

type AddressAllocationMode string

const (
	// Bind to localhost (default)
	AddressAllocationModeLocalhost AddressAllocationMode = "Localhost"

	// Bind only to 127.0.0.1
	AddressAllocationModeIPv4ZeroOne AddressAllocationMode = "IPv4ZeroOne"

	// Bind to a random 127.*.*.* loopback address
	AddressAllocationModeIPv4Loopback AddressAllocationMode = "IPv4Loopback"

	// Bind only to ::1
	AddressAllocationModeIPv6ZeroOne AddressAllocationMode = "IPv6ZeroOne"

	// Bind to all interfaces (including localhost/loopback addresses).
	// DCP will bind to either IPv4 or IPv6 depending on configuration and available
	// interfaces, but not both.
	AddressAllocationModeAllInterfaces AddressAllocationMode = "AllInterfaces"

	// Bind to all IPv4 interfaces (0.0.0.0).
	AddressAllocationModeIPv4AllInterfaces AddressAllocationMode = "IPv4AllInterfaces"

	// Bind to all IPv6 interfaces (::).
	AddressAllocationModeIPv6AllInterfaces AddressAllocationMode = "IPv6AllInterfaces"

	// Don't use a proxy--the service will have the same address and port as the first endpoint
	AddressAllocationModeProxyless AddressAllocationMode = "Proxyless"
)

// ServiceSpec defines the desired state of a Service
// +k8s:openapi-gen=true
type ServiceSpec struct {
	// The desired address for the service to run on
	Address string `json:"address,omitempty"`

	// The desired port for the service to run on
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// The protocol, TCP or UDP
	Protocol PortProtocol `json:"protocol,omitempty"`

	// The mode for address allocation. If Address is set, this will be ignored.
	AddressAllocationMode AddressAllocationMode `json:"addressAllocationMode,omitempty"`
}

// ServiceStatus describes the status of a Service
// +k8s:openapi-gen=true
type ServiceStatus struct {
	// The current state of the service
	// +kubebuilder:default:=NotReady
	State ServiceState `json:"state,omitempty"`

	// The PID of the proxy process
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	// +optional
	ProxyProcessPid *int64 `json:"proxyProcessPid,omitempty"`

	// The path of the proxy config file for this service containing both routing config and service definition
	// +optional
	ProxyConfigFile string `json:"proxyConfigFile,omitempty"`

	// The actual address the service is running on
	EffectiveAddress string `json:"effectiveAddress,omitempty"`

	// The actual port the service is running on
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	EffectivePort int32 `json:"effectivePort,omitempty"`

	// When in Proxyless mode, the namespace of the Endpoint that was chosen to use as the service's effective address and port
	// +optional
	ProxylessEndpointNamespace string `json:"proxylessEndpointNamespace,omitempty"`

	// When in Proxyless mode, the name of the Endpoint that was chosen to use as the service's effective address and port
	// +optional
	ProxylessEndpointName string `json:"proxylessEndpointName,omitempty"`
}

func (cs ServiceStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	cs.DeepCopyInto(&dest.(*Service).Status)
}

// Service represents a single service implemented by zero or more endpoints
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type Service struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceSpec   `json:"spec,omitempty"`
	Status ServiceStatus `json:"status,omitempty"`
}

// Done implements StdIoStreamableResource.
func (svc *Service) Done() bool {
	return !svc.DeletionTimestamp.IsZero()
}

// GetDeletionTimestamp implements StdIoStreamableResource.
// Subtle: this method shadows the method (ObjectMeta).GetDeletionTimestamp of Service.ObjectMeta.
func (svc *Service) GetDeletionTimestamp() *metav1.Time {
	return svc.DeletionTimestamp
}

// HasStdOut implements StdOutStreamableResource.
func (ce *Service) HasStdOut() bool {
	return false
}

// HasStdErr implements StdOutStreamableResource.
func (ce *Service) HasStdErr() bool {
	return false
}

// GetStdErrFile implements StdIoStreamableResource.
func (svc *Service) GetStdErrFile() string {
	return ""
}

// GetStdOutFile implements StdIoStreamableResource.
func (svc *Service) GetStdOutFile() string {
	return ""
}

// GetUID implements StdIoStreamableResource.
// Subtle: this method shadows the method (ObjectMeta).GetUID of Service.ObjectMeta.
func (svc *Service) GetUID() types.UID {
	return svc.UID
}

func (svc *Service) GetResourceId() string {
	return fmt.Sprintf("service-%s", svc.UID)
}

func (svc *Service) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "services",
	}
}

func (svc *Service) GetObjectMeta() *metav1.ObjectMeta {
	return &svc.ObjectMeta
}

func (svc *Service) GetStatus() apiserver_resource.StatusSubResource {
	return svc.Status
}

func (svc *Service) New() runtime.Object {
	return &Service{}
}

func (svc *Service) NewList() runtime.Object {
	return &ServiceList{}
}

func (svc *Service) IsStorageVersion() bool {
	return true
}

func (svc *Service) NamespaceScoped() bool {
	return false
}

func (svc *Service) ShortNames() []string {
	return []string{"svc"}
}

func (svc *Service) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      svc.Name,
		Namespace: svc.Namespace,
	}
}

func (svc *Service) Validate(ctx context.Context) field.ErrorList {
	errorList := field.ErrorList{}

	if ResourceCreationProhibited.Load() && svc.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, errResourceCreationProhibited.Error()))
	}

	return errorList
}

// ServiceList contains a list of Service instances
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type ServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Service `json:"items"`
}

func (svcl *ServiceList) GetListMeta() *metav1.ListMeta {
	return &svcl.ListMeta
}

func (svcl *ServiceList) ItemCount() uint32 {
	return uint32(len(svcl.Items))
}

func (svcl *ServiceList) GetItems() []*Service {
	retval := make([]*Service, len(svcl.Items))
	for i := range svcl.Items {
		retval[i] = &svcl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&Service{}, &ServiceList{})
}

// Ensure types support interfaces expected by our API server
var _ apiserver_resource.Object = (*Service)(nil)
var _ apiserver_resource.ObjectList = (*ServiceList)(nil)
var _ commonapi.ListWithObjectItems[Service, *Service] = (*ServiceList)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*Service)(nil)
var _ apiserver_resource.StatusSubResource = (*ServiceStatus)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*Service)(nil)
var _ apiserver_resourcestrategy.Validater = (*Service)(nil)
var _ StdIoStreamableResource = (*Service)(nil)
