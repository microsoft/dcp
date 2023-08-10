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

// ServiceSpec defines the desired state of a Service
// +k8s:openapi-gen=true
type ServiceSpec struct {
	// The desired address for the service to run on
	Address string `json:"address,omitempty"`

	// The desired port for the service to run on
	Port int32 `json:"port,omitempty"`
}

// ServiceStatus describes the status of a Service
// +k8s:openapi-gen=true
type ServiceStatus struct {
	// The PID of the proxy process
	ProxyProcessPid int32 `json:"proxyProcessPid,omitempty"`

	// The path of the proxy config file for this service containing both routing config and service definition
	ProxyConfigFile string `json:"proxyConfigFile,omitempty"`

	// The actual address the service is running on
	EffectiveAddress string `json:"effectiveAddress,omitempty"`

	// The actual port the service is running on
	EffectivePort int32 `json:"effectivePort,omitempty"`
}

func (cs ServiceStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	cs.DeepCopyInto(&dest.(*Service).Status)
}

// Service represents a single service implemented by zero or more endpoints
// +kubebuilder:object:root=true
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
	// TODO: implement validation
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
