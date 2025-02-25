package v1

import (
	"context"
	"strings"

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
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
)

// VolumeSpec defines the desired state of a ContainerVolume
// +k8s:openapi-gen=true
type ContainerVolumeSpec struct {
	// Name of the volume
	Name string `json:"name"`

	// Is this volume persistent (is NOT cleaned up when the application ends) or not.
	// Volumes are persistent by default.
	// +kubebuilder:default=true
	Persistent *bool `json:"persistent,omitempty"`
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
	persistent := true // Default value
	return &ContainerVolume{
		Spec: ContainerVolumeSpec{
			Persistent: &persistent,
		},
	}
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
	errorList := field.ErrorList{}

	if ResourceCreationProhibited.Load() {
		errorList = append(errorList, field.Forbidden(nil, errResourceCreationProhibited.Error()))
	}

	if strings.TrimSpace(cv.Spec.Name) == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec").Child("name"), "Name is required"))
	}

	return errorList
}

func (cv *ContainerVolume) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldVolume := old.(*ContainerVolume)

	if oldVolume.Spec.Name != cv.Spec.Name {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec").Child("name"), "Name cannot be changed"))
	}

	if !pointers.EqualValue(oldVolume.Spec.Persistent, cv.Spec.Persistent) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec").Child("persistent"), "volume persistence cannot be changed"))
	}

	return errorList
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
var _ apiserver_resourcestrategy.ValidateUpdater = (*ContainerVolume)(nil)
