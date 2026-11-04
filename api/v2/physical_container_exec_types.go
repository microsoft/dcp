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

// PhysicalContainerExecSpec describes one command execution against a physical container.
// +k8s:openapi-gen=true
type PhysicalContainerExecSpec struct {
	// ContainerRef identifies a PhysicalContainer using <name> or <namespace>/<name>.
	// Cross-namespace references are not supported.
	ContainerRef string `json:"containerRef"`
	// Execution contains literal command inputs.
	Execution ContainerExecConfig `json:"execution"`
}

// PhysicalContainerExecStatus describes the execution and its captured output.
// +k8s:openapi-gen=true
type PhysicalContainerExecStatus struct {
	ContainerExecStatus `json:",inline"`
	// ContainerID is the runtime container identity selected for this execution.
	ContainerID string `json:"containerID,omitempty"`
	// StdoutFile is the captured standard output path.
	StdoutFile string `json:"stdoutFile,omitempty"`
	// StderrFile is the captured standard error path.
	StderrFile string `json:"stderrFile,omitempty"`
	// LogsComplete indicates that no more execution output will be captured.
	LogsComplete bool `json:"logsComplete,omitempty"`
}

func (status PhysicalContainerExecStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	status.DeepCopyInto(&dest.(*PhysicalContainerExec).Status)
}

// PhysicalContainerExec owns one attached execution, independently of the target container lifetime.
// Deletion cancels the attached session; it does not guarantee termination of the in-container process.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=physicalcontainerexecs,shortName=pce
type PhysicalContainerExec struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PhysicalContainerExecSpec   `json:"spec,omitempty"`
	Status            PhysicalContainerExecStatus `json:"status,omitempty"`
}

func (*PhysicalContainerExec) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupVersion.Group, Version: GroupVersion.Version, Resource: "physicalcontainerexecs"}
}
func (resource *PhysicalContainerExec) GetObjectMeta() *metav1.ObjectMeta {
	return &resource.ObjectMeta
}
func (resource *PhysicalContainerExec) GetStatus() apiserver_resource.StatusSubResource {
	return resource.Status
}
func (*PhysicalContainerExec) New() runtime.Object     { return &PhysicalContainerExec{} }
func (*PhysicalContainerExec) NewList() runtime.Object { return &PhysicalContainerExecList{} }
func (*PhysicalContainerExec) IsStorageVersion() bool  { return true }
func (*PhysicalContainerExec) NamespaceScoped() bool   { return true }
func (*PhysicalContainerExec) ShortNames() []string    { return []string{"pce"} }
func (resource *PhysicalContainerExec) NamespacedName() types.NamespacedName {
	return NamespacedName(resource)
}
func (resource *PhysicalContainerExec) GetResourceId() string {
	return fmt.Sprintf("physicalcontainerexec-%s", resource.UID)
}

// GetLogSource describes output owned by this execution.
func (resource *PhysicalContainerExec) GetLogSource() *LogSource {
	return &LogSource{Name: resource.Name, UID: resource.UID,
		StdoutFile: resource.Status.StdoutFile, StderrFile: resource.Status.StderrFile,
		Available: resource.Status.StdoutFile != "" && resource.Status.StderrFile != "", Complete: resource.Status.LogsComplete}
}

func (resource *PhysicalContainerExec) Validate(context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(resource)
	if commonapi.ResourceCreationProhibited.Load() && resource.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}
	errorList = append(errorList, commonapi.ValidateAnnotationsSize(resource.Annotations, field.NewPath("metadata", "annotations"))...)
	errorList = append(errorList, validateSameNamespaceResourceReference(resource.Spec.ContainerRef, resource.Namespace, field.NewPath("spec", "containerRef"))...)
	return append(errorList, resource.Spec.Execution.validate(field.NewPath("spec", "execution"))...)
}
func (resource *PhysicalContainerExec) ValidateUpdate(_ context.Context, old runtime.Object) field.ErrorList {
	if !reflect.DeepEqual(resource.Spec, old.(*PhysicalContainerExec).Spec) {
		return field.ErrorList{field.Forbidden(field.NewPath("spec"), "spec is immutable")}
	}
	return nil
}
func (*PhysicalContainerExec) GenericSubResources() []apiserver_resource.GenericSubResource {
	return []apiserver_resource.GenericSubResource{&PhysicalContainerExecLogResource{}}
}

// PhysicalContainerExecLogResource registers execution output and system diagnostics.
// +k8s:openapi-gen=false
// +kubebuilder:object:generate=false
type PhysicalContainerExecLogResource struct{}

func (*PhysicalContainerExecLogResource) Name() string { return LogSubresourceName }
func (*PhysicalContainerExecLogResource) GetStorageProvider(obj apiserver_resource.Object, _ string, provider apiserver.StorageProvider) apiserver.StorageProvider {
	return newLogStorageProvider(obj, provider)
}

// PhysicalContainerExecList contains physical container executions.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type PhysicalContainerExecList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PhysicalContainerExec `json:"items"`
}

func (list *PhysicalContainerExecList) GetListMeta() *metav1.ListMeta { return &list.ListMeta }
func (list *PhysicalContainerExecList) ItemCount() uint32             { return uint32(len(list.Items)) }
func (list *PhysicalContainerExecList) GetItems() []*PhysicalContainerExec {
	items := make([]*PhysicalContainerExec, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}
	return items
}
func init() { SchemeBuilder.Register(&PhysicalContainerExec{}, &PhysicalContainerExecList{}) }

var _ apiserver_resource.Object = (*PhysicalContainerExec)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*PhysicalContainerExec)(nil)
var _ apiserver_resource.StatusSubResource = (*PhysicalContainerExecStatus)(nil)
var _ apiserver_resource.ObjectList = (*PhysicalContainerExecList)(nil)
var _ commonapi.ListWithObjectItems[PhysicalContainerExec, *PhysicalContainerExec] = (*PhysicalContainerExecList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*PhysicalContainerExec)(nil)
var _ apiserver_resourcestrategy.Validater = (*PhysicalContainerExec)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*PhysicalContainerExec)(nil)
