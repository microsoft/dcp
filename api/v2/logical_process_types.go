/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"reflect"
	"slices"
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

// LogicalProcessMode controls process creation, reuse, and retention.
// +kubebuilder:validation:Enum=session;persistent;existing
type LogicalProcessMode string

const (
	// LogicalProcessModeSession creates a process and stops it when the resource is deleted.
	LogicalProcessModeSession LogicalProcessMode = "session"
	// LogicalProcessModePersistent creates or reuses a stored process and retains it when the resource is deleted.
	LogicalProcessModePersistent LogicalProcessMode = "persistent"
	// LogicalProcessModeExisting observes an existing process without creating or stopping it on deletion.
	LogicalProcessModeExisting LogicalProcessMode = "existing"
)

// LogicalProcessPhase summarizes the lifecycle of a logical process.
type LogicalProcessPhase string

const (
	// LogicalProcessPhasePending indicates that the process is waiting for prerequisites or launch.
	LogicalProcessPhasePending LogicalProcessPhase = "Pending"
	// LogicalProcessPhaseRunning indicates that the process is running.
	LogicalProcessPhaseRunning LogicalProcessPhase = "Running"
	// LogicalProcessPhaseExited indicates that the process has exited or the referenced identity is missing.
	LogicalProcessPhaseExited LogicalProcessPhase = "Exited"
	// LogicalProcessPhaseUnknown indicates that process state cannot be determined.
	LogicalProcessPhaseUnknown LogicalProcessPhase = "Unknown"
	// LogicalProcessPhaseFailed indicates a terminal lifecycle failure.
	LogicalProcessPhaseFailed LogicalProcessPhase = "Failed"
)

// LogicalProcessSpec describes process lifecycle policy and desired configuration.
// +k8s:openapi-gen=true
type LogicalProcessSpec struct {
	// Mode controls creation, reuse, and retention. Defaults to session.
	// +kubebuilder:default:=session
	Mode LogicalProcessMode `json:"mode,omitempty"`
	// LifecycleKey identifies the desired configuration for persistent process reuse.
	// If omitted, a key is calculated from the process configuration and monitor identity.
	LifecycleKey string `json:"lifecycleKey,omitempty"`
	// Process describes the desired process. Required for session and persistent modes, forbidden for existing mode.
	Process *LogicalProcessConfig `json:"process,omitempty"`
	// PID identifies the process to observe in existing mode; forbidden in other modes.
	PID *int64 `json:"pid,omitempty"`
	// IdentityTimestamp identifies the expected process in pid. Required in existing mode.
	IdentityTimestamp metav1.MicroTime `json:"identityTimestamp,omitempty"`
	// MonitorPID optionally scopes a persistent process to another process lifetime.
	MonitorPID *int64 `json:"monitorPID,omitempty"`
	// MonitorTimestamp identifies the process in monitorPID and guards against PID reuse.
	MonitorTimestamp metav1.MicroTime `json:"monitorTimestamp,omitempty"`
	// Stop requests termination through the physical controller. Once true, it cannot be reset.
	Stop bool `json:"stop,omitempty"`
}

// LogicalProcessConfig describes the desired configuration used to launch a process.
// +k8s:openapi-gen=true
type LogicalProcessConfig struct {
	// ExecutablePath is the executable path or name to launch.
	ExecutablePath string `json:"executablePath"`
	// Args are arguments passed to the executable.
	// +listType=atomic
	Args []string `json:"args,omitempty"`
	// WorkingDirectory is the process working directory, defaulting to the controller working directory.
	WorkingDirectory string `json:"workingDirectory,omitempty"`
	// InheritEnvironment includes the controller environment. Defaults to true.
	// +kubebuilder:default:=true
	InheritEnvironment *bool `json:"inheritEnvironment,omitempty"`
	// Env contains environment variables that override inherited values.
	// +listType=map
	// +listMapKey=name
	Env []commonapi.EnvVar `json:"env,omitempty"`
}

// EffectiveMode returns the configured lifecycle mode, defaulting to session.
func (spec LogicalProcessSpec) EffectiveMode() LogicalProcessMode {
	if spec.Mode == "" {
		return LogicalProcessModeSession
	}
	return spec.Mode
}

// GetLifecycleKey returns the explicit key or computes one from the desired process configuration.
// The second result is true when the key is computed; inherited environment values are not hashed.
func (spec *LogicalProcessSpec) GetLifecycleKey() (string, bool, error) {
	if spec.LifecycleKey != "" {
		return spec.LifecycleKey, false, nil
	}
	if spec.Process == nil {
		return "", false, errors.New("process configuration is required to compute a lifecycle key")
	}
	desired := spec.Process.DeepCopy()
	inherit := desired.InheritEnvironment == nil || *desired.InheritEnvironment
	desired.InheritEnvironment = &inherit
	slices.SortStableFunc(desired.Env, func(left, right commonapi.EnvVar) int { return strings.Compare(left.Name, right.Name) })
	fnvHash := fnv.New128()
	encoder := gob.NewEncoder(fnvHash)
	hashErr := encoder.Encode(desired)
	if spec.MonitorPID != nil {
		hashErr = errors.Join(hashErr, encoder.Encode(*spec.MonitorPID), encoder.Encode(spec.MonitorTimestamp.Time))
	}
	return fmt.Sprintf("%x", fnvHash.Sum(nil)), true, hashErr
}

// LogicalProcessStatus describes the observed process and its physical backing resource.
// +k8s:openapi-gen=true
type LogicalProcessStatus struct {
	// Phase summarizes process availability.
	// +kubebuilder:validation:Enum=Pending;Running;Exited;Unknown;Failed
	Phase LogicalProcessPhase `json:"phase,omitempty"`
	// PhysicalProcessRef identifies the same-namespace backing PhysicalProcess.
	PhysicalProcessRef string `json:"physicalProcessRef,omitempty"`
	// LifecycleKey is the supplied or calculated lifecycle key.
	LifecycleKey string `json:"lifecycleKey,omitempty"`
	// PID is the observed operating system process ID.
	PID *int64 `json:"pid,omitempty"`
	// IdentityTimestamp is the observed process identity, not necessarily a wall-clock timestamp.
	IdentityTimestamp metav1.MicroTime `json:"identityTimestamp,omitempty"`
	// FinishedAt is when the physical controller observed process exit.
	FinishedAt metav1.MicroTime `json:"finishedAt,omitempty"`
	// ExitCode is the observed exit code, when available.
	ExitCode *int32 `json:"exitCode,omitempty"`
	// Conditions describe readiness and reconciliation progress.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func (status LogicalProcessStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	status.DeepCopyInto(&dest.(*LogicalProcess).Status)
}

// LogicalProcess represents process lifecycle policy independently of runtime execution.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=logicalprocesses,shortName=lproc
type LogicalProcess struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              LogicalProcessSpec   `json:"spec,omitempty"`
	Status            LogicalProcessStatus `json:"status,omitempty"`
}

func (resource *LogicalProcess) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupVersion.Group, Version: GroupVersion.Version, Resource: "logicalprocesses"}
}

// GetLeaseKey identifies the persistent process by its logical namespace and name.
func (resource *LogicalProcess) GetLeaseKey() string {
	return resource.NamespacedName().String()
}

func (resource *LogicalProcess) GetObjectMeta() *metav1.ObjectMeta {
	return &resource.ObjectMeta
}
func (resource *LogicalProcess) GetStatus() apiserver_resource.StatusSubResource {
	return resource.Status
}
func (resource *LogicalProcess) New() runtime.Object {
	return &LogicalProcess{}
}
func (resource *LogicalProcess) NewList() runtime.Object {
	return &LogicalProcessList{}
}
func (resource *LogicalProcess) IsStorageVersion() bool {
	return true
}
func (resource *LogicalProcess) NamespaceScoped() bool {
	return true
}
func (resource *LogicalProcess) ShortNames() []string {
	return []string{"lproc"}
}
func (resource *LogicalProcess) NamespacedName() types.NamespacedName {
	return NamespacedName(resource)
}

func (resource *LogicalProcess) Validate(ctx context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(resource)
	specPath := field.NewPath("spec")
	if commonapi.ResourceCreationProhibited.Load() && resource.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}
	errorList = append(errorList, commonapi.ValidateAnnotationsSize(resource.Annotations, field.NewPath("metadata", "annotations"))...)
	mode := resource.Spec.EffectiveMode()
	switch mode {
	case LogicalProcessModeSession, LogicalProcessModePersistent:
		if resource.Spec.Process == nil {
			errorList = append(errorList, field.Required(specPath.Child("process"), "process must be set for session and persistent modes"))
		} else if strings.TrimSpace(resource.Spec.Process.ExecutablePath) == "" {
			errorList = append(errorList, field.Required(specPath.Child("process", "executablePath"), "executablePath must be set"))
		}
		if resource.Spec.PID != nil {
			errorList = append(errorList, field.Forbidden(specPath.Child("pid"), "pid is only supported in existing mode"))
		}
		if !resource.Spec.IdentityTimestamp.IsZero() {
			errorList = append(errorList, field.Forbidden(specPath.Child("identityTimestamp"), "identityTimestamp is only supported in existing mode"))
		}
	case LogicalProcessModeExisting:
		if resource.Spec.IdentityTimestamp.IsZero() {
			errorList = append(errorList, field.Required(specPath.Child("identityTimestamp"), "identityTimestamp must be set in existing mode"))
		}
		if resource.Spec.Process != nil {
			errorList = append(errorList, field.Forbidden(specPath.Child("process"), "process cannot be set in existing mode"))
		}
		if resource.Spec.PID == nil {
			errorList = append(errorList, field.Required(specPath.Child("pid"), "pid must be set in existing mode"))
		} else if *resource.Spec.PID <= 0 || *resource.Spec.PID > math.MaxUint32 {
			errorList = append(errorList, field.Invalid(specPath.Child("pid"), *resource.Spec.PID, "pid must be between 1 and 4294967295"))
		}
	default:
		errorList = append(errorList, field.NotSupported(specPath.Child("mode"), resource.Spec.Mode, []string{
			string(LogicalProcessModeSession), string(LogicalProcessModePersistent), string(LogicalProcessModeExisting),
		}))
	}
	errorList = append(errorList, validateRetainedResourceMonitor(mode == LogicalProcessModePersistent, resource.Spec.MonitorPID, resource.Spec.MonitorTimestamp, specPath)...)
	return errorList
}

// ValidateUpdate freezes lifecycle configuration while allowing a monotonic stop request.
func (resource *LogicalProcess) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}
	oldResource := old.(*LogicalProcess)
	if oldResource.Spec.Stop && !resource.Spec.Stop {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "stop"), "stop cannot be set to false once it has been set to true"))
	}
	oldSpec, newSpec := oldResource.Spec, resource.Spec
	oldSpec.Stop, newSpec.Stop = false, false
	if !reflect.DeepEqual(oldSpec, newSpec) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec"), "spec is immutable"))
	}
	return errorList
}

// LogicalProcessList contains a list of LogicalProcess instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type LogicalProcessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LogicalProcess `json:"items"`
}

func (list *LogicalProcessList) GetListMeta() *metav1.ListMeta {
	return &list.ListMeta
}
func (list *LogicalProcessList) ItemCount() uint32 {
	return uint32(len(list.Items))
}
func (list *LogicalProcessList) GetItems() []*LogicalProcess {
	items := make([]*LogicalProcess, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}
	return items
}

func init() {
	SchemeBuilder.Register(&LogicalProcess{}, &LogicalProcessList{})
}

var _ apiserver_resource.Object = (*LogicalProcess)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*LogicalProcess)(nil)
var _ apiserver_resource.StatusSubResource = (*LogicalProcessStatus)(nil)
var _ apiserver_resource.ObjectList = (*LogicalProcessList)(nil)
var _ commonapi.ListWithObjectItems[LogicalProcess, *LogicalProcess] = (*LogicalProcessList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*LogicalProcess)(nil)
var _ apiserver_resourcestrategy.Validater = (*LogicalProcess)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*LogicalProcess)(nil)
