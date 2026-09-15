/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// PhysicalContainerNetworkConnectionSpec identifies the physical container and network that should be connected.
// Both references resolve within the connection resource's namespace.
// +k8s:openapi-gen=true
type PhysicalContainerNetworkConnectionSpec struct {
	// ContainerRef is the name of the PhysicalContainer to connect.
	ContainerRef string `json:"containerRef"`

	// NetworkRef is the name of the PhysicalContainerNetwork to connect to.
	NetworkRef string `json:"networkRef"`

	// Aliases contains network-scoped aliases for the container.
	// +listType=set
	Aliases []string `json:"aliases,omitempty"`
}

// PhysicalContainerNetworkConnection represents desired runtime network membership for one physical container.
// +kubebuilder:object:root=true
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=physicalcontainernetworkconnections,shortName=pcnc
type PhysicalContainerNetworkConnection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PhysicalContainerNetworkConnectionSpec `json:"spec,omitempty"`
}

func (connection *PhysicalContainerNetworkConnection) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "physicalcontainernetworkconnections",
	}
}

func (connection *PhysicalContainerNetworkConnection) GetObjectMeta() *metav1.ObjectMeta {
	return &connection.ObjectMeta
}

func (connection *PhysicalContainerNetworkConnection) New() runtime.Object {
	return &PhysicalContainerNetworkConnection{}
}

func (connection *PhysicalContainerNetworkConnection) NewList() runtime.Object {
	return &PhysicalContainerNetworkConnectionList{}
}

func (connection *PhysicalContainerNetworkConnection) IsStorageVersion() bool {
	return true
}

func (connection *PhysicalContainerNetworkConnection) NamespaceScoped() bool {
	return true
}

func (connection *PhysicalContainerNetworkConnection) ShortNames() []string {
	return []string{"pcnc"}
}

func (connection *PhysicalContainerNetworkConnection) NamespacedName() types.NamespacedName {
	return NamespacedName(connection)
}

func (connection *PhysicalContainerNetworkConnection) Validate(ctx context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(connection)
	specPath := field.NewPath("spec")

	if commonapi.ResourceCreationProhibited.Load() && connection.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}

	errorList = append(errorList, commonapi.ValidateAnnotationsSize(connection.Annotations, field.NewPath("metadata", "annotations"))...)
	errorList = append(errorList, validatePhysicalResourceReference(connection.Spec.ContainerRef, specPath.Child("containerRef"))...)
	errorList = append(errorList, validatePhysicalResourceReference(connection.Spec.NetworkRef, specPath.Child("networkRef"))...)
	return errorList
}

func (connection *PhysicalContainerNetworkConnection) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	oldConnection := old.(*PhysicalContainerNetworkConnection)
	if reflect.DeepEqual(oldConnection.Spec, connection.Spec) {
		return nil
	}

	return field.ErrorList{
		field.Forbidden(field.NewPath("spec"), "spec is immutable"),
	}
}

func validatePhysicalResourceReference(reference string, referencePath *field.Path) field.ErrorList {
	if reference == "" {
		return field.ErrorList{field.Required(referencePath, "reference must be set")}
	}

	errorList := field.ErrorList{}
	for _, validationMessage := range validation.IsDNS1123Subdomain(reference) {
		errorList = append(errorList, field.Invalid(referencePath, reference, validationMessage))
	}
	return errorList
}

// PhysicalContainerNetworkConnectionList contains a list of PhysicalContainerNetworkConnection instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type PhysicalContainerNetworkConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PhysicalContainerNetworkConnection `json:"items"`
}

func (connections *PhysicalContainerNetworkConnectionList) GetListMeta() *metav1.ListMeta {
	return &connections.ListMeta
}

func (connections *PhysicalContainerNetworkConnectionList) ItemCount() uint32 {
	return uint32(len(connections.Items))
}

func (connections *PhysicalContainerNetworkConnectionList) GetItems() []*PhysicalContainerNetworkConnection {
	result := make([]*PhysicalContainerNetworkConnection, len(connections.Items))
	for i := range connections.Items {
		result[i] = &connections.Items[i]
	}
	return result
}

func init() {
	SchemeBuilder.Register(&PhysicalContainerNetworkConnection{}, &PhysicalContainerNetworkConnectionList{})
}

var _ apiserver_resource.Object = (*PhysicalContainerNetworkConnection)(nil)
var _ apiserver_resource.ObjectList = (*PhysicalContainerNetworkConnectionList)(nil)
var _ commonapi.ListWithObjectItems[PhysicalContainerNetworkConnection, *PhysicalContainerNetworkConnection] = (*PhysicalContainerNetworkConnectionList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*PhysicalContainerNetworkConnection)(nil)
var _ apiserver_resourcestrategy.Validater = (*PhysicalContainerNetworkConnection)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*PhysicalContainerNetworkConnection)(nil)
