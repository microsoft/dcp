package v1

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
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
	return nil
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

func (svcl *ServiceList) GetItems() []ctrl_client.Object {
	retval := make([]ctrl_client.Object, len(svcl.Items))
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
var _ commonapi.ListWithObjectItems = (*ServiceList)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*Service)(nil)
var _ apiserver_resource.StatusSubResource = (*ServiceStatus)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*Service)(nil)
var _ apiserver_resourcestrategy.Validater = (*Service)(nil)
