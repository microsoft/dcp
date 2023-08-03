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
	// Name of the service
	Name string `json:"name"`
}

// ServiceStatus describes the status of a Service
// +k8s:openapi-gen=true
type ServiceStatus struct {
	// A human-readable message that provides additional information about Service state.
	Message string `json:"message,omitempty"`
}

// Service represents a single service implemented by one or more endpoints
// Its lifetime is dependent on the lifetime of the Executables or Containers that it is attached to.
// +kubebuilder:object:root=true
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type Service struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceSpec   `json:"spec,omitempty"`
	Status ServiceStatus `json:"status,omitempty"`
}

func (cv *Service) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "services",
	}
}

func (cv *Service) GetObjectMeta() *metav1.ObjectMeta {
	return &cv.ObjectMeta
}

func (cv *Service) New() runtime.Object {
	return &Service{}
}

func (cv *Service) NewList() runtime.Object {
	return &ServiceList{}
}

func (cv *Service) IsStorageVersion() bool {
	return true
}

func (cv *Service) NamespaceScoped() bool {
	return false
}

func (cv *Service) ShortNames() []string {
	return []string{"svc"}
}

func (cv *Service) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      cv.Name,
		Namespace: cv.Namespace,
	}
}

func (cv *Service) Validate(ctx context.Context) field.ErrorList {
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

func (cvl *ServiceList) GetListMeta() *metav1.ListMeta {
	return &cvl.ListMeta
}

func (cvl *ServiceList) ItemCount() uint32 {
	return uint32(len(cvl.Items))
}

func (cvl *ServiceList) GetItems() []ctrl_client.Object {
	retval := make([]ctrl_client.Object, len(cvl.Items))
	for i := range cvl.Items {
		retval[i] = &cvl.Items[i]
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
var _ apiserver_resourcerest.ShortNamesProvider = (*Service)(nil)
var _ apiserver_resourcestrategy.Validater = (*Service)(nil)
