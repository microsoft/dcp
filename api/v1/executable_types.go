// Copyright (c) Microsoft Corporation. All rights reserved.

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

type ExecutableState string

const (
	// The Executable was successfully started and was running last time we checked.
	ExecutableStateRunning ExecutableState = "Running"

	// Terminated means the Executable was terminated by its owner. Common reasons are scale-down, or debug session end (for execution via IDE).
	ExecutableStateTerminated ExecutableState = "Terminated"

	// Failed to start means the Executable could not be started
	ExecutableStateFailedToStart ExecutableState = "FailedToStart"

	// Finished means the Executable ran to completion.
	ExecutableStateFinished ExecutableState = "Finished"

	// Unknown means we are not tracking the actual-state counterpart of the Executable (process or IDE run session).
	// As a result, we do not know whether it already finished, and what is the exit code, if any.
	// This can happen if a controller launches a process and then terminates.
	// When a new controller instance comes online, it may see non-zero ExecutionID Status,
	// but it does not track the corresponding process or IDE session.
	ExecutableStateUnknown ExecutableState = "Unknown"
)

const (
	// A valid exit code of a process is a non-negative number. We use UnknownExitCode to indicate that we have not obtained the exit code yet.
	UnknownExitCode = -1

	// Unknown PID code is used when replica is not started (or fails to start)
	UnknownPID = -1
)

type ExecutionType string

const (
	// Executable will be run directly by the controller, as a child process.
	ExecutionTypeProcess ExecutionType = "Process"

	// Executable will be run via an IDE such as VS or VS Code.
	ExecutionTypeIDE ExecutionType = "IDE"
)

// +k8s:openapi-gen=true
type ExecutableTemplate struct {
	// Metadata for child Executables
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec for child Executables
	Spec ExecutableSpec `json:"spec,omitempty"`
}

// ExecutableSpec defines the desired state of an Executable
// +k8s:openapi-gen=true
type ExecutableSpec struct {
	// Path to Executable binary
	ExecutablePath string `json:"executablePath"`

	// The working directory for the Executable
	WorkingDirectory string `json:"workingDirectory,omitempty"`

	// Launch arguments to be passed to the Executable
	// +listType=atomic
	Args []string `json:"args,omitempty"`

	// Environment variables to be set for the Executable
	// +listType=map
	// +listMapKey=name
	Env []EnvVar `json:"env,omitempty"`

	// Environment files to use to populate Executable environment during startup.
	// +listType=atomic
	EnvFiles []string `json:"envFiles,omitempty"`

	// The execution type for the Executable.
	// +kubebuilder:default:=Process
	ExecutionType ExecutionType `json:"executionType,omitempty"`
}

// ExecutableStatus describes the status of an Executable.
// +k8s:openapi-gen=true
type ExecutableStatus struct {
	// The execution ID is the identifier for the actual-state counterpart of the Executable.
	// For ExecutionType == Process it is the process ID. Process IDs will be eventually reused by OS,
	// but a combination of process ID and startup timestamp is unique for each Executable instance.
	// For ExecutionType == IDE it is the IDE session ID.
	ExecutionID string `json:"executionID"`

	// The current state of the process/IDE session started for this executable
	State ExecutableState `json:"state"`

	// Start (attempt) timestamp.
	StartupTimestamp metav1.Time `json:"startupTimestamp,omitempty"`

	// The time when the process/IDE session finished execution
	FinishTimestamp metav1.Time `json:"finishTimestamp,omitempty"`

	// Exit code of the process associated with the Executable.
	// The value is equal to UnknownExitCode if the Executable was not started, is still running, or the exit code is not available.
	ExitCode int32 `json:"exitCode,omitempty"`

	// The path of a temporary file that contains captured standard output data from the Executable process.
	StdOutFile string `json:"stdOutFile,omitempty"`

	// The path of a temporary file that contains captured standard error data from the Executable process.
	StdErrFile string `json:"stdErrFile,omitempty"`
}

func (es ExecutableStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	es.DeepCopyInto(&dest.(*Executable).Status)
}

// Executable resource represents an OS process, with one or more replicas
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type Executable struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ExecutableSpec   `json:"spec,omitempty"`
	Status ExecutableStatus `json:"status,omitempty"`
}

func (e *Executable) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "executables",
	}
}

func (e *Executable) GetObjectMeta() *metav1.ObjectMeta {
	return &e.ObjectMeta
}

func (e *Executable) GetStatus() apiserver_resource.StatusSubResource {
	return e.Status
}

func (e *Executable) New() runtime.Object {
	return &Executable{}
}

func (e *Executable) NewList() runtime.Object {
	return &ExecutableList{}
}

func (e *Executable) IsStorageVersion() bool {
	return true
}

func (e *Executable) NamespaceScoped() bool {
	return false
}

func (e *Executable) ShortNames() []string {
	return []string{"exe"}
}

func (e *Executable) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      e.Name,
		Namespace: e.Namespace,
	}
}

func (e *Executable) Validate(ctx context.Context) field.ErrorList {
	// TODO: implement validation https://github.com/microsoft/usvc-stdtypes/issues/2
	return nil
}

func (e *Executable) Done() bool {
	// When the Executable has a FinishTimestamp set, it is considered done no matter what other data says.
	return !e.Status.FinishTimestamp.IsZero()
}

func (exe *Executable) UpdateRunningStatus(exitCode int32, state ExecutableState) {
	exe.Status.ExitCode = exitCode
	exe.Status.State = state
	if state == ExecutableStateFinished || state == ExecutableStateTerminated || state == ExecutableStateFailedToStart {
		exe.Status.FinishTimestamp = metav1.Now()
	}
}

// ExecutableList contains a list of Executable instances
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type ExecutableList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Executable `json:"items"`
}

func (el *ExecutableList) GetListMeta() *metav1.ListMeta {
	return &el.ListMeta
}

func (el *ExecutableList) ItemCount() uint32 {
	return uint32(len(el.Items))
}

func (el *ExecutableList) GetItems() []ctrl_client.Object {
	retval := make([]ctrl_client.Object, len(el.Items))
	for i := range el.Items {
		retval[i] = &el.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&Executable{}, &ExecutableList{})
}

// Ensure types support interfaces expected by our API server
var _ apiserver_resource.Object = (*Executable)(nil)
var _ apiserver_resource.ObjectList = (*ExecutableList)(nil)
var _ commonapi.ListWithObjectItems = (*ExecutableList)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*Executable)(nil)
var _ apiserver_resource.StatusSubResource = (*ExecutableStatus)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*Executable)(nil)
var _ apiserver_resourcestrategy.Validater = (*Executable)(nil)
