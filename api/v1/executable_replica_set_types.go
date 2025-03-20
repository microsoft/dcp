// Copyright (c) Microsoft Corporation. All rights reserved.

package v1

import (
	"context"

	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ExecutableReplicaSetSpec desribes the desired state of an ExecutableReplicaSet
// +k8s:openapi-gen=true
type ExecutableReplicaSetSpec struct {
	// Number of desired child Executable objects
	// +kubebuilder:default:=1
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`

	// Should the replica be soft deleted on scale down instead of deleted?
	// +kubebuilder:default:=false
	StopOnScaleDown bool `json:"stopOnScaleDown,omitempty"`

	// Template describing the configuration of child Executable objects created by the ExecutableReplicaSet
	// +kubebuilder:validation:Required
	Template ExecutableTemplate `json:"template"`
}

// ExecutableReplicaSetStatus desribes the status of an ExecutableReplicaSet
// +k8s:openapi-gen=true
type ExecutableReplicaSetStatus struct {
	// Total number of observed child executables
	// +kubebuilder:default:=0
	ObservedReplicas int32 `json:"observedReplicas"`

	// Total number of current running child Executables
	// +kubebuilder:default:=0
	RunningReplicas int32 `json:"runningReplicas"`

	// Total number of current Executable replicas that failed to start
	// +kubebuilder:default:=0
	FailedReplicas int32 `json:"failedReplicas"`

	// Total number of current child Executables that have finished running
	// +kubebuilder:default:=0
	FinishedReplicas int32 `json:"finishedReplicas"`

	// Last time the replica set was scaled up or down by the controller
	LastScaleTime metav1.MicroTime `json:"lastScaleTime,omitempty"`

	// Health status of the replica set
	HealthStatus HealthStatus `json:"healthStatus,omitempty"`
}

// CopyTo implements apiserver_resource.ObjectWithStatusSubResource.
func (erss ExecutableReplicaSetStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	erss.DeepCopyInto(&dest.(*ExecutableReplicaSet).Status)
}

// ExecutableReplicaSet resource represents a replication configuration for zero or more Executable resources
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type ExecutableReplicaSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ExecutableReplicaSetSpec   `json:"spec,omitempty"`
	Status ExecutableReplicaSetStatus `json:"status,omitempty"`
}

// Validate implements resourcestrategy.Validater.
func (ers *ExecutableReplicaSet) Validate(ctx context.Context) field.ErrorList {
	errorList := ers.Spec.Template.Spec.Validate(field.NewPath("spec", "template", "spec"))

	if ers.Spec.Template.Spec.Stop {
		errorList = append(errorList, field.Invalid(field.NewPath("spec", "template", "spec", "stop"), ers.Spec.Template.Spec.Stop, "replicas with Stop==true will never run"))
	}

	if ResourceCreationProhibited.Load() {
		errorList = append(errorList, field.Forbidden(nil, errResourceCreationProhibited.Error()))
	}

	return errorList
}

// ShortNames implements rest.ShortNamesProvider.
func (*ExecutableReplicaSet) ShortNames() []string {
	return []string{"exers"}
}

// NamespaceScoped implements resource.Object.
func (*ExecutableReplicaSet) NamespaceScoped() bool {
	return false
}

// New implements resource.Object.
func (*ExecutableReplicaSet) New() runtime.Object {
	return &ExecutableReplicaSet{}
}

// NewList implements resource.Object.
func (*ExecutableReplicaSet) NewList() runtime.Object {
	return &ExecutableReplicaSetList{}
}

func (e *ExecutableReplicaSet) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      e.Name,
		Namespace: e.Namespace,
	}
}

// IsStorageVersion implements apiserver_resource.ObjectWithStorageVersion.
func (e *ExecutableReplicaSet) IsStorageVersion() bool {
	return true
}

// GetGroupVersionResource implements apiserver_resource.ObjectWithGroupVersionResource.
func (e *ExecutableReplicaSet) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "executablereplicasets",
	}
}

// GetObjectMeta implements apiserver_resource.ObjectWithObjectMeta.
func (e *ExecutableReplicaSet) GetObjectMeta() *metav1.ObjectMeta {
	return &e.ObjectMeta
}

// GetStatus implements apiserver_resource.ObjectWithStatusSubResource.
func (e *ExecutableReplicaSet) GetStatus() apiserver_resource.StatusSubResource {
	return e.Status
}

// ExecutableReplicaSetList contains a list of ExecutableReplicaSet instances
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type ExecutableReplicaSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ExecutableReplicaSet `json:"items"`
}

// GetListMeta implements apiserver_resource.ObjectList.
func (el *ExecutableReplicaSetList) GetListMeta() *metav1.ListMeta {
	return &el.ListMeta
}

// ItemCount implements apiserver_resource.ObjectList.
func (el *ExecutableReplicaSetList) ItemCount() uint32 {
	return uint32(len(el.Items))
}

// GetItems implements apiserver_resource.ObjectList.
func (el *ExecutableReplicaSetList) GetItems() []*ExecutableReplicaSet {
	retval := make([]*ExecutableReplicaSet, len(el.Items))
	for i := range el.Items {
		retval[i] = &el.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&ExecutableReplicaSet{}, &ExecutableReplicaSetList{})

}

// Ensure types support interfaces expected by our API server
var _ apiserver_resource.Object = (*ExecutableReplicaSet)(nil)
var _ apiserver_resource.ObjectList = (*ExecutableReplicaSetList)(nil)
var _ commonapi.ListWithObjectItems[ExecutableReplicaSet, *ExecutableReplicaSet] = (*ExecutableReplicaSetList)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*ExecutableReplicaSet)(nil)
var _ apiserver_resource.StatusSubResource = (*ExecutableReplicaSetStatus)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*ExecutableReplicaSet)(nil)
var _ apiserver_resourcestrategy.Validater = (*ExecutableReplicaSet)(nil)
