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

// EndpointSpec defines the desired state of a Endpoint
// +k8s:openapi-gen=true
type EndpointSpec struct {
	// Namespace of the service the endpoint implements
	ServiceNamespace string `json:"serviceNamespace"`

	// Name of the service the endpoint implements
	ServiceName string `json:"serviceName"`

	// The desired address for the endpoint to run on
	Address string `json:"address"`

	// The desired port for the endpoint to run on
	Port uint16 `json:"port"`
}

// EndpointStatus describes the status of a Endpoint
// +k8s:openapi-gen=true
type EndpointStatus struct {
}

func (cs EndpointStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	cs.DeepCopyInto(&dest.(*Endpoint).Status)
}

// Endpoint represents a network endpoint that implements a service
// Its lifetime is dependent on the lifetime of the Executable or Container that it is attached to.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type Endpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EndpointSpec   `json:"spec,omitempty"`
	Status EndpointStatus `json:"status,omitempty"`
}

func (e *Endpoint) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "endpoints",
	}
}

func (e *Endpoint) GetObjectMeta() *metav1.ObjectMeta {
	return &e.ObjectMeta
}

func (e *Endpoint) GetStatus() apiserver_resource.StatusSubResource {
	return e.Status
}

func (e *Endpoint) New() runtime.Object {
	return &Endpoint{}
}

func (e *Endpoint) NewList() runtime.Object {
	return &EndpointList{}
}

func (e *Endpoint) IsStorageVersion() bool {
	return true
}

func (e *Endpoint) NamespaceScoped() bool {
	return false
}

func (e *Endpoint) ShortNames() []string {
	return []string{"end"}
}

func (e *Endpoint) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      e.Name,
		Namespace: e.Namespace,
	}
}

func (e *Endpoint) Validate(ctx context.Context) field.ErrorList {
	// TODO: implement validation
	return nil
}

// EndpointList contains a list of Endpoint instances
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type EndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Endpoint `json:"items"`
}

func (el *EndpointList) GetListMeta() *metav1.ListMeta {
	return &el.ListMeta
}

func (el *EndpointList) ItemCount() uint32 {
	return uint32(len(el.Items))
}

func (el *EndpointList) GetItems() []ctrl_client.Object {
	retval := make([]ctrl_client.Object, len(el.Items))
	for i := range el.Items {
		retval[i] = &el.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&Endpoint{}, &EndpointList{})
}

// Ensure types support interfaces expected by our API server
var _ apiserver_resource.Object = (*Endpoint)(nil)
var _ apiserver_resource.ObjectList = (*EndpointList)(nil)
var _ commonapi.ListWithObjectItems = (*EndpointList)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*Endpoint)(nil)
var _ apiserver_resource.StatusSubResource = (*EndpointStatus)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*Endpoint)(nil)
var _ apiserver_resourcestrategy.Validater = (*Endpoint)(nil)
