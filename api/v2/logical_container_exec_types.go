/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"fmt"
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	apiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// LogicalContainerExecSpec describes one command execution against a logical container.
// +k8s:openapi-gen=true
type LogicalContainerExecSpec struct {
	// ContainerRef identifies a LogicalContainer using <name> or <namespace>/<name>.
	// Cross-namespace references are not supported.
	ContainerRef string `json:"containerRef"`
	// Execution contains literal command inputs without DCP template expansion.
	Execution ContainerExecConfig `json:"execution"`
}

// LogicalContainerExecStatus projects the immediate physical execution.
// +k8s:openapi-gen=true
type LogicalContainerExecStatus struct {
	ContainerExecStatus `json:",inline"`
	// PhysicalExecRef identifies the same-namespace backing PhysicalContainerExec.
	PhysicalExecRef string `json:"physicalExecRef,omitempty"`
	// ContainerID is the runtime container identity selected by the physical execution.
	ContainerID string `json:"containerID,omitempty"`
	// RuntimeLogs describes output captured by the immediate physical execution.
	RuntimeLogs *LogSource `json:"runtimeLogs,omitempty"`
}

func (status LogicalContainerExecStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	status.DeepCopyInto(&dest.(*LogicalContainerExec).Status)
}

// LogicalContainerExec resolves logical container readiness and owns one physical execution.
// Deletion cancels the attached session without stopping the target container or guaranteeing remote termination.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=logicalcontainerexecs,shortName=lce
type LogicalContainerExec struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              LogicalContainerExecSpec   `json:"spec,omitempty"`
	Status            LogicalContainerExecStatus `json:"status,omitempty"`
}

func (*LogicalContainerExec) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupVersion.Group, Version: GroupVersion.Version, Resource: "logicalcontainerexecs"}
}
func (resource *LogicalContainerExec) GetObjectMeta() *metav1.ObjectMeta { return &resource.ObjectMeta }
func (resource *LogicalContainerExec) GetStatus() apiserver_resource.StatusSubResource {
	return resource.Status
}
func (*LogicalContainerExec) New() runtime.Object     { return &LogicalContainerExec{} }
func (*LogicalContainerExec) NewList() runtime.Object { return &LogicalContainerExecList{} }
func (*LogicalContainerExec) IsStorageVersion() bool  { return true }
func (*LogicalContainerExec) NamespaceScoped() bool   { return true }
func (*LogicalContainerExec) ShortNames() []string    { return []string{"lce"} }
func (resource *LogicalContainerExec) NamespacedName() types.NamespacedName {
	return NamespacedName(resource)
}
func (resource *LogicalContainerExec) GetResourceId() string {
	return fmt.Sprintf("logicalcontainerexec-%s", resource.UID)
}
func (resource *LogicalContainerExec) Validate(context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(resource)
	if commonapi.ResourceCreationProhibited.Load() && resource.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}
	errorList = append(errorList, commonapi.ValidateAnnotationsSize(resource.Annotations, field.NewPath("metadata", "annotations"))...)
	errorList = append(errorList, validateSameNamespaceResourceReference(resource.Spec.ContainerRef, resource.Namespace, field.NewPath("spec", "containerRef"))...)
	return append(errorList, resource.Spec.Execution.validate(field.NewPath("spec", "execution"))...)
}
func (resource *LogicalContainerExec) ValidateUpdate(_ context.Context, old runtime.Object) field.ErrorList {
	if !reflect.DeepEqual(resource.Spec, old.(*LogicalContainerExec).Spec) {
		return field.ErrorList{field.Forbidden(field.NewPath("spec"), "spec is immutable")}
	}
	return nil
}
func (*LogicalContainerExec) GenericSubResources() []apiserver_resource.GenericSubResource {
	return []apiserver_resource.GenericSubResource{&LogicalContainerExecLogResource{}}
}

// LogicalContainerExecLogResource registers projected output and owner-scoped diagnostics.
// +k8s:openapi-gen=false
// +kubebuilder:object:generate=false
type LogicalContainerExecLogResource struct{}

func (*LogicalContainerExecLogResource) Name() string { return LogSubresourceName }
func (*LogicalContainerExecLogResource) GetStorageProvider(obj apiserver_resource.Object, _ string, provider apiserver.StorageProvider) apiserver.StorageProvider {
	return newLogStorageProvider(obj, provider)
}

// LogicalContainerExecList contains logical container executions.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type LogicalContainerExecList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LogicalContainerExec `json:"items"`
}

func (list *LogicalContainerExecList) GetListMeta() *metav1.ListMeta { return &list.ListMeta }
func (list *LogicalContainerExecList) ItemCount() uint32             { return uint32(len(list.Items)) }
func (list *LogicalContainerExecList) GetItems() []*LogicalContainerExec {
	items := make([]*LogicalContainerExec, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}
	return items
}
func init() { SchemeBuilder.Register(&LogicalContainerExec{}, &LogicalContainerExecList{}) }

var _ apiserver_resource.Object = (*LogicalContainerExec)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*LogicalContainerExec)(nil)
var _ apiserver_resource.StatusSubResource = (*LogicalContainerExecStatus)(nil)
var _ apiserver_resource.ObjectList = (*LogicalContainerExecList)(nil)
var _ commonapi.ListWithObjectItems[LogicalContainerExec, *LogicalContainerExec] = (*LogicalContainerExecList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*LogicalContainerExec)(nil)
var _ apiserver_resourcestrategy.Validater = (*LogicalContainerExec)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*LogicalContainerExec)(nil)
