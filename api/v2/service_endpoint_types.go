/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// ServiceAddress identifies one reachable transport address.
// +k8s:openapi-gen=true
type ServiceAddress struct {
	// Address is an IP address or hostname without a port.
	Address string `json:"address"`

	// Port is the port reachable at this address.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// ServiceEndpointSpec describes the resolved addresses for one producer binding.
// +k8s:openapi-gen=true
type ServiceEndpointSpec struct {
	// ServiceRef identifies the LogicalService using <name> or <namespace>/<name>.
	// Cross-namespace references are not supported.
	ServiceRef string `json:"serviceRef"`

	// Addresses are alternative ways to reach the same producer binding.
	// The transport protocol is defined by the referenced LogicalService.
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	Addresses []ServiceAddress `json:"addresses"`
}

// ServiceEndpoint represents resolved routing addresses for one LogicalService producer binding.
// Workload-owned endpoints follow their owner's availability and lifetime; caller-managed endpoints need no workload owner.
// +kubebuilder:object:root=true
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=serviceendpoints,shortName=sep
type ServiceEndpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ServiceEndpointSpec `json:"spec,omitempty"`
}

func (endpoint *ServiceEndpoint) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupVersion.Group, Version: GroupVersion.Version, Resource: "serviceendpoints"}
}

func (endpoint *ServiceEndpoint) GetObjectMeta() *metav1.ObjectMeta {
	return &endpoint.ObjectMeta
}

func (endpoint *ServiceEndpoint) New() runtime.Object {
	return &ServiceEndpoint{}
}

func (endpoint *ServiceEndpoint) NewList() runtime.Object {
	return &ServiceEndpointList{}
}

func (endpoint *ServiceEndpoint) IsStorageVersion() bool {
	return true
}

func (endpoint *ServiceEndpoint) NamespaceScoped() bool {
	return true
}

func (endpoint *ServiceEndpoint) ShortNames() []string {
	return []string{"sep"}
}

func (endpoint *ServiceEndpoint) NamespacedName() types.NamespacedName {
	return NamespacedName(endpoint)
}

func (endpoint *ServiceEndpoint) Validate(ctx context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(endpoint)
	specPath := field.NewPath("spec")
	if commonapi.ResourceCreationProhibited.Load() && endpoint.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}
	errorList = append(errorList, commonapi.ValidateAnnotationsSize(endpoint.Annotations, field.NewPath("metadata", "annotations"))...)
	errorList = append(errorList, ValidateSameNamespaceResourceReference(endpoint.Spec.ServiceRef, endpoint.Namespace, specPath.Child("serviceRef"))...)
	errorList = append(errorList, validateServiceAddresses(endpoint.Spec.Addresses, specPath.Child("addresses"))...)
	return errorList
}

func validateServiceAddresses(addresses []ServiceAddress, addressesPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}
	if len(addresses) == 0 {
		return append(errorList, field.Required(addressesPath, "at least one address must be set"))
	}
	seen := make(map[ServiceAddress]struct{}, len(addresses))
	for addressIndex, address := range addresses {
		addressPath := addressesPath.Index(addressIndex)
		if strings.TrimSpace(address.Address) == "" {
			errorList = append(errorList, field.Required(addressPath.Child("address"), "address must be set"))
		}
		if address.Port <= 0 || address.Port > 65535 {
			errorList = append(errorList, field.Invalid(addressPath.Child("port"), address.Port, "port must be between 1 and 65535"))
		}
		if _, found := seen[address]; found {
			errorList = append(errorList, field.Duplicate(addressPath, address))
		}
		seen[address] = struct{}{}
	}
	return errorList
}

// ServiceEndpointList contains a list of ServiceEndpoint instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type ServiceEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceEndpoint `json:"items"`
}

func (list *ServiceEndpointList) GetListMeta() *metav1.ListMeta {
	return &list.ListMeta
}

func (list *ServiceEndpointList) ItemCount() uint32 {
	return uint32(len(list.Items))
}

func (list *ServiceEndpointList) GetItems() []*ServiceEndpoint {
	items := make([]*ServiceEndpoint, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}
	return items
}

func init() {
	SchemeBuilder.Register(&ServiceEndpoint{}, &ServiceEndpointList{})
}

var _ apiserver_resource.Object = (*ServiceEndpoint)(nil)
var _ apiserver_resource.ObjectList = (*ServiceEndpointList)(nil)
var _ commonapi.ListWithObjectItems[ServiceEndpoint, *ServiceEndpoint] = (*ServiceEndpointList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*ServiceEndpoint)(nil)
var _ apiserver_resourcestrategy.Validater = (*ServiceEndpoint)(nil)
