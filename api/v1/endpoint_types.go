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
	// Name of the service the endpoint implements
	Service string `json:"service"`
}

// EndpointStatus describes the status of a Endpoint
// +k8s:openapi-gen=true
type EndpointStatus struct {
	// The URL of the endpoint implementing the service
	Url string `json:"url,omitempty"`
}

// Endpoint represents a network endpoint that implements a service
// Its lifetime is dependent on the lifetime of the Executable or Container that it is attached to.
// +kubebuilder:object:root=true
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type Endpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EndpointSpec   `json:"spec,omitempty"`
	Status EndpointStatus `json:"status,omitempty"`
}

func (cv *Endpoint) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "endpoints",
	}
}

func (cv *Endpoint) GetObjectMeta() *metav1.ObjectMeta {
	return &cv.ObjectMeta
}

func (cv *Endpoint) New() runtime.Object {
	return &Endpoint{}
}

func (cv *Endpoint) NewList() runtime.Object {
	return &EndpointList{}
}

func (cv *Endpoint) IsStorageVersion() bool {
	return true
}

func (cv *Endpoint) NamespaceScoped() bool {
	return false
}

func (cv *Endpoint) ShortNames() []string {
	return []string{"end"}
}

func (cv *Endpoint) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      cv.Name,
		Namespace: cv.Namespace,
	}
}

func (cv *Endpoint) Validate(ctx context.Context) field.ErrorList {
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

func (cvl *EndpointList) GetListMeta() *metav1.ListMeta {
	return &cvl.ListMeta
}

func (cvl *EndpointList) ItemCount() uint32 {
	return uint32(len(cvl.Items))
}

func (cvl *EndpointList) GetItems() []ctrl_client.Object {
	retval := make([]ctrl_client.Object, len(cvl.Items))
	for i := range cvl.Items {
		retval[i] = &cvl.Items[i]
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
var _ apiserver_resourcerest.ShortNamesProvider = (*Endpoint)(nil)
var _ apiserver_resourcestrategy.Validater = (*Endpoint)(nil)
