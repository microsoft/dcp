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

// VolumeSpec defines the desired state of a ContainerVolume
// +k8s:openapi-gen=true
type ContainerVolumeSpec struct {
	// Name of the volume
	Name string `json:"name"`
}

// ContainerVolume represents a volume that can be consumed by Container instances
// Its lifetime is independent from the lifetime of containers
// +kubebuilder:object:root=true
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type ContainerVolume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ContainerVolumeSpec `json:"spec,omitempty"`
	// No status for now, but we might need to introduce status in future.
}

func (cv *ContainerVolume) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "containervolumes",
	}
}

func (cv *ContainerVolume) GetObjectMeta() *metav1.ObjectMeta {
	return &cv.ObjectMeta
}

func (cv *ContainerVolume) New() runtime.Object {
	return &ContainerVolume{}
}

func (cv *ContainerVolume) NewList() runtime.Object {
	return &ContainerVolumeList{}
}

func (cv *ContainerVolume) IsStorageVersion() bool {
	return true
}

func (cv *ContainerVolume) NamespaceScoped() bool {
	return false
}

func (cv *ContainerVolume) ShortNames() []string {
	return []string{"ctrvol"}
}

func (cv *ContainerVolume) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      cv.Name,
		Namespace: cv.Namespace,
	}
}

func (cv *ContainerVolume) Validate(ctx context.Context) field.ErrorList {
	// TODO: implement validation
	return nil
}

// ContainerVolumeList contains a list of ContainerVolume instances
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type ContainerVolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContainerVolume `json:"items"`
}

func (cvl *ContainerVolumeList) GetListMeta() *metav1.ListMeta {
	return &cvl.ListMeta
}

func (cvl *ContainerVolumeList) ItemCount() uint32 {
	return uint32(len(cvl.Items))
}

func (cvl *ContainerVolumeList) GetItems() []ctrl_client.Object {
	retval := make([]ctrl_client.Object, len(cvl.Items))
	for i := range cvl.Items {
		retval[i] = &cvl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&ContainerVolume{}, &ContainerVolumeList{})
}

// Ensure types support interfaces expected by our API server
var _ apiserver_resource.Object = (*ContainerVolume)(nil)
var _ apiserver_resource.ObjectList = (*ContainerVolumeList)(nil)
var _ commonapi.ListWithObjectItems = (*ContainerVolumeList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*ContainerVolume)(nil)
var _ apiserver_resourcestrategy.Validater = (*ContainerVolume)(nil)
