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

// ContainerNetworkConnectionSpec defines the desired state of a ContainerNetworkConnection
// +k8s:openapi-gen=true
type ContainerNetworkConnectionSpec struct {
	// The name of the ContainerNetwork resource to connect to
	ContainerNetworkName string `json:"containerNetworkName"`

	// The ID of the container (container orchestrator resource ID) to connect to the ContainerNetwork
	ContainerID string `json:"containerID"`

	// The optional list of custom aliases for the container on the ContainerNetwork
	// +listType=set
	Aliases []string `json:"aliases,omitempty"`
}

// ContainerNetworkConnection represents a Container that wishes to be connected to a specific ContainerNetwork
// Its lifetime is dependent on the lifetime of the Container that it is attached to.
// +kubebuilder:object:root=true
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type ContainerNetworkConnection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ContainerNetworkConnectionSpec `json:"spec,omitempty"`
}

func (cn *ContainerNetworkConnection) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "containernetworkconnections",
	}
}

func (cn *ContainerNetworkConnection) GetObjectMeta() *metav1.ObjectMeta {
	return &cn.ObjectMeta
}

func (cn *ContainerNetworkConnection) New() runtime.Object {
	return &ContainerNetworkConnection{}
}

func (cn *ContainerNetworkConnection) NewList() runtime.Object {
	return &ContainerNetworkConnectionList{}
}

func (cn *ContainerNetworkConnection) IsStorageVersion() bool {
	return true
}

func (cn *ContainerNetworkConnection) NamespaceScoped() bool {
	return false
}

func (cn *ContainerNetworkConnection) ShortNames() []string {
	return []string{"ctrnetconn"}
}

func (cn *ContainerNetworkConnection) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      cn.Name,
		Namespace: cn.Namespace,
	}
}

func (cn *ContainerNetworkConnection) Validate(ctx context.Context) field.ErrorList {
	errorList := field.ErrorList{}

	if ResourceCreationProhibited.Load() {
		errorList = append(errorList, field.Forbidden(nil, errResourceCreationProhibited.Error()))
	}

	return errorList
}

// ContainerNetworkConnectionList contains a list of ContainerNetworkConnection instances
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type ContainerNetworkConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContainerNetworkConnection `json:"items"`
}

func (cnl *ContainerNetworkConnectionList) GetListMeta() *metav1.ListMeta {
	return &cnl.ListMeta
}

func (cnl *ContainerNetworkConnectionList) ItemCount() uint32 {
	return uint32(len(cnl.Items))
}

func (cnl *ContainerNetworkConnectionList) GetItems() []ctrl_client.Object {
	retval := make([]ctrl_client.Object, len(cnl.Items))
	for i := range cnl.Items {
		retval[i] = &cnl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&ContainerNetworkConnection{}, &ContainerNetworkConnectionList{})
}

// Ensure types support interfaces expected by our API server
var _ apiserver_resource.Object = (*ContainerNetworkConnection)(nil)
var _ apiserver_resource.ObjectList = (*ContainerNetworkConnectionList)(nil)
var _ commonapi.ListWithObjectItems = (*ContainerNetworkConnectionList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*ContainerNetworkConnection)(nil)
var _ apiserver_resourcestrategy.Validater = (*ContainerNetworkConnection)(nil)
