/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"math"
	"reflect"
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

// PhysicalProcessPhase describes the lifecycle phase of a PhysicalProcess.
type PhysicalProcessPhase PhysicalResourcePhase

const (
	// PhysicalProcessPhasePending indicates that the process is waiting for reconciliation to make progress.
	PhysicalProcessPhasePending PhysicalProcessPhase = PhysicalProcessPhase(PhysicalResourcePhasePending)

	// PhysicalProcessPhaseRunning indicates that the operating system process is running.
	PhysicalProcessPhaseRunning PhysicalProcessPhase = PhysicalProcessPhase(PhysicalResourcePhaseRunning)

	// PhysicalProcessPhaseExited indicates that the operating system process has exited.
	PhysicalProcessPhaseExited PhysicalProcessPhase = PhysicalProcessPhase(PhysicalResourcePhaseExited)

	// PhysicalProcessPhaseUnknown indicates that process state is unavailable or indeterminate.
	PhysicalProcessPhaseUnknown PhysicalProcessPhase = PhysicalProcessPhase(PhysicalResourcePhaseUnknown)

	// PhysicalProcessPhaseFailed indicates a terminal operation failure.
	PhysicalProcessPhaseFailed PhysicalProcessPhase = PhysicalProcessPhase(PhysicalResourcePhaseFailed)
)

const (
	// PhysicalProcessReasonLaunching indicates that process launch is in progress.
	PhysicalProcessReasonLaunching ConditionReason = "Launching"

	// PhysicalProcessReasonLaunchFailed indicates that process launch failed and will be retried.
	PhysicalProcessReasonLaunchFailed ConditionReason = "LaunchFailed"

	// PhysicalProcessReasonRuntimeProcessRunning indicates that the operating system process is running.
	PhysicalProcessReasonRuntimeProcessRunning ConditionReason = "RuntimeProcessRunning"

	// PhysicalProcessReasonRuntimeProcessExited indicates that the operating system process has exited.
	PhysicalProcessReasonRuntimeProcessExited ConditionReason = "RuntimeProcessExited"

	// PhysicalProcessReasonRuntimeProcessMissing indicates that the operating system process was not found.
	PhysicalProcessReasonRuntimeProcessMissing ConditionReason = "RuntimeProcessMissing"

	// PhysicalProcessReasonRuntimeProcessInspectFailed indicates that process state could not be inspected.
	PhysicalProcessReasonRuntimeProcessInspectFailed ConditionReason = "RuntimeProcessInspectFailed"

	// PhysicalProcessReasonRuntimeProcessAlreadyTracked indicates that another PhysicalProcess tracks the same process instance.
	PhysicalProcessReasonRuntimeProcessAlreadyTracked ConditionReason = "RuntimeProcessAlreadyTracked"

	// PhysicalProcessReasonStopping indicates that process termination is in progress.
	PhysicalProcessReasonStopping ConditionReason = "Stopping"

	// PhysicalProcessReasonStopFailed indicates that process termination failed and will be retried.
	PhysicalProcessReasonStopFailed ConditionReason = "StopFailed"
)

// PhysicalProcessSpec describes either an existing operating system process or how to launch one.
// +k8s:openapi-gen=true
type PhysicalProcessSpec struct {
	// PID identifies an existing operating system process to track. Exactly one of pid or process must be set.
	// The process is identified by PID initially and guarded against PID reuse by status.identityTimestamp.
	PID *int64 `json:"pid,omitempty"`

	// Process describes an operating system process to launch. Exactly one of pid or process must be set.
	Process *PhysicalProcessConfig `json:"process,omitempty"`

	// Stop requests that the tracked operating system process be stopped.
	Stop bool `json:"stop,omitempty"`
}

// PhysicalProcessConfig describes an operating system process to launch.
// +k8s:openapi-gen=true
type PhysicalProcessConfig struct {
	// RetainRuntimeProcess keeps a process launched by this resource running when the resource is deleted.
	RetainRuntimeProcess bool `json:"retainRuntimeProcess,omitempty"`

	// ExecutablePath is the executable path or name to launch.
	ExecutablePath string `json:"executablePath"`

	// Args are arguments passed to the executable.
	// +listType=atomic
	Args []string `json:"args,omitempty"`

	// WorkingDirectory is the process working directory. The controller process working directory is used when omitted.
	WorkingDirectory string `json:"workingDirectory,omitempty"`

	// InheritEnvironment includes the controller process environment when true.
	InheritEnvironment bool `json:"inheritEnvironment,omitempty"`

	// Env contains process environment variables. These values override inherited variables with the same name.
	// +listType=map
	// +listMapKey=name
	Env []commonapi.EnvVar `json:"env,omitempty"`
}

// PhysicalProcessStatus describes the observed operating system process.
// +k8s:openapi-gen=true
type PhysicalProcessStatus struct {
	// Phase summarizes the operating system process lifecycle.
	// +kubebuilder:validation:Enum=Pending;Running;Exited;Unknown;Failed
	// +optional
	Phase PhysicalProcessPhase `json:"phase,omitempty"`

	// PID is the operating system process ID being tracked.
	PID *int64 `json:"pid,omitempty"`

	// IdentityTimestamp identifies the specific process instance and prevents PID reuse from targeting a different process.
	// On Linux this value represents elapsed time since boot rather than wall-clock time.
	IdentityTimestamp metav1.MicroTime `json:"identityTimestamp,omitempty"`

	// FinishedAt is when the controller observed process exit.
	FinishedAt metav1.MicroTime `json:"finishedAt,omitempty"`

	// ExitCode is the process exit code when it was available to the controller.
	ExitCode *int32 `json:"exitCode,omitempty"`

	// Conditions describe readiness and reconciliation progress.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func (pps PhysicalProcessStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	pps.DeepCopyInto(&dest.(*PhysicalProcess).Status)
}

// PhysicalProcess represents one operating system process in a DCP V2 namespace.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=physicalprocesses,shortName=pproc
type PhysicalProcess struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PhysicalProcessSpec   `json:"spec,omitempty"`
	Status PhysicalProcessStatus `json:"status,omitempty"`
}

func (pp *PhysicalProcess) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "physicalprocesses",
	}
}

func (pp *PhysicalProcess) GetObjectMeta() *metav1.ObjectMeta {
	return &pp.ObjectMeta
}

func (pp *PhysicalProcess) GetStatus() apiserver_resource.StatusSubResource {
	return pp.Status
}

func (pp *PhysicalProcess) New() runtime.Object {
	return &PhysicalProcess{}
}

func (pp *PhysicalProcess) NewList() runtime.Object {
	return &PhysicalProcessList{}
}

func (pp *PhysicalProcess) IsStorageVersion() bool {
	return true
}

func (pp *PhysicalProcess) NamespaceScoped() bool {
	return true
}

func (pp *PhysicalProcess) ShortNames() []string {
	return []string{"pproc"}
}

func (pp *PhysicalProcess) NamespacedName() types.NamespacedName {
	return NamespacedName(pp)
}

func (pp *PhysicalProcess) Validate(ctx context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(pp)
	specPath := field.NewPath("spec")

	if commonapi.ResourceCreationProhibited.Load() && pp.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}

	errorList = append(errorList, commonapi.ValidateAnnotationsSize(pp.Annotations, field.NewPath("metadata", "annotations"))...)

	if pp.Spec.PID == nil && pp.Spec.Process == nil {
		errorList = append(errorList, field.Required(specPath, "exactly one of pid or process must be set"))
		return errorList
	}
	if pp.Spec.PID != nil && pp.Spec.Process != nil {
		errorList = append(errorList, field.Forbidden(specPath.Child("process"), "process cannot be set when pid is set"))
		return errorList
	}
	if pp.Spec.PID != nil {
		if *pp.Spec.PID <= 0 || *pp.Spec.PID > math.MaxUint32 {
			errorList = append(errorList, field.Invalid(specPath.Child("pid"), *pp.Spec.PID, "pid must be between 1 and 4294967295"))
		}
		return errorList
	}

	processConfig := pp.Spec.Process
	processPath := specPath.Child("process")
	if strings.TrimSpace(processConfig.ExecutablePath) == "" {
		errorList = append(errorList, field.Required(processPath.Child("executablePath"), "executablePath must be set"))
	}

	return errorList
}

// ValidateUpdate freezes process identity and creation fields while allowing stop to be requested.
func (pp *PhysicalProcess) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldPhysicalProcess := old.(*PhysicalProcess)
	if oldPhysicalProcess.Spec.Stop && !pp.Spec.Stop {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "stop"), "stop cannot be set to false once it has been set to true"))
	}

	oldSpec := oldPhysicalProcess.Spec
	newSpec := pp.Spec
	oldSpec.Stop = false
	newSpec.Stop = false
	if !reflect.DeepEqual(oldSpec, newSpec) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec"), "spec is immutable"))
	}

	return errorList
}

// PhysicalProcessList contains a list of PhysicalProcess instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type PhysicalProcessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PhysicalProcess `json:"items"`
}

func (ppl *PhysicalProcessList) GetListMeta() *metav1.ListMeta {
	return &ppl.ListMeta
}

func (ppl *PhysicalProcessList) ItemCount() uint32 {
	return uint32(len(ppl.Items))
}

func (ppl *PhysicalProcessList) GetItems() []*PhysicalProcess {
	retval := make([]*PhysicalProcess, len(ppl.Items))
	for i := range ppl.Items {
		retval[i] = &ppl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&PhysicalProcess{}, &PhysicalProcessList{})
}

// Ensure types support interfaces expected by our API server.
var _ apiserver_resource.Object = (*PhysicalProcess)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*PhysicalProcess)(nil)
var _ apiserver_resource.StatusSubResource = (*PhysicalProcessStatus)(nil)
var _ apiserver_resource.ObjectList = (*PhysicalProcessList)(nil)
var _ commonapi.ListWithObjectItems[PhysicalProcess, *PhysicalProcess] = (*PhysicalProcessList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*PhysicalProcess)(nil)
var _ apiserver_resourcestrategy.Validater = (*PhysicalProcess)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*PhysicalProcess)(nil)
