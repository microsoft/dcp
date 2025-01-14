// Copyright (c) Microsoft Corporation. All rights reserved.

package v1

import (
	"context"
	"fmt"
	stdmaps "maps"
	stdslices "slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	generic_registry "k8s.io/apiserver/pkg/registry/generic"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"

	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
)

type ExecutableState string

const (
	// Same as ExecutableStateStarting. May be encountered if the Executable status has not been initialized yet.
	ExecutableStateEmpty ExecutableState = ""

	// The Executable has been scheduled to launch, but we will need to re-evaluate its state in a subsequent
	// reconciliation loop.
	ExecutableStateStarting ExecutableState = "Starting"

	// The Executable was successfully started and was running last time we checked.
	ExecutableStateRunning ExecutableState = "Running"

	// Executable is stopping (DCP is trying to stop the process)
	ExecutableStateStopping ExecutableState = "Stopping"

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

func (es ExecutableState) CanUpdateTo(newState ExecutableState) bool {
	// We live in imperfect world, and losing track of Executable state is always a possibility.
	if newState == ExecutableStateUnknown {
		return true
	}

	switch {
	case es == ExecutableStateEmpty:
		return newState == ExecutableStateStarting ||
			newState == ExecutableStateRunning ||
			newState == ExecutableStateTerminated ||
			newState == ExecutableStateFailedToStart

	case es == ExecutableStateStarting:
		return newState == ExecutableStateRunning ||
			newState == ExecutableStateFailedToStart ||
			newState == ExecutableStateFinished

	case es == ExecutableStateRunning:
		return newState == ExecutableStateStopping ||
			newState == ExecutableStateTerminated ||
			newState == ExecutableStateFinished

	case es == ExecutableStateStopping:
		return newState == ExecutableStateTerminated ||
			newState == ExecutableStateFinished

	case es.IsTerminal():
		return false

	default:
		return false
	}
}

func (es ExecutableState) IsTerminal() bool {
	return es == ExecutableStateTerminated || es == ExecutableStateFailedToStart || es == ExecutableStateFinished || es == ExecutableStateUnknown
}

// A valid exit code of a process is a non-negative number. We use UnknownExitCode to indicate that we have not obtained the exit code yet.
var UnknownExitCode *int32 = nil

// Unknown PID code is used when replica is not started (or fails to start)
var UnknownPID *int64 = nil

type ExecutionType string

const (
	// Executable will be run directly by the controller, as a child process.
	ExecutionTypeProcess ExecutionType = "Process"

	// Executable will be run via an IDE such as VS or VS Code.
	ExecutionTypeIDE ExecutionType = "IDE"
)

type EnvironmentBehavior string

const (
	// The executable will inherit the environment of the controller process.
	// This is the default behavior.
	EnvironmentBehaviorInherit EnvironmentBehavior = "Inherit"

	// The executable will not inherit the environment of the controller process.
	EnvironmentBehaviorDoNotInherit EnvironmentBehavior = "DoNotInherit"
)

// +k8s:openapi-gen=true
type AmbientEnvironment struct {
	// How environment variables should be inherited from the controller process.
	// +kubebuilder:default:=Inherit
	Behavior EnvironmentBehavior `json:"behavior,omitempty"`
}

// +k8s:openapi-gen=true
type ExecutableTemplate struct {
	// Labels to apply to child Executable objects
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations to apply to child Executable objects
	Annotations map[string]string `json:"annotations,omitempty"`

	// Spec for child Executables
	Spec ExecutableSpec `json:"spec"`
}

func (et ExecutableTemplate) Equal(other ExecutableTemplate) bool {
	if !stdmaps.Equal(et.Labels, other.Labels) {
		return false
	}

	if !stdmaps.Equal(et.Annotations, other.Annotations) {
		return false
	}

	return et.Spec.Equal(other.Spec)
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
	// +listType=set
	EnvFiles []string `json:"envFiles,omitempty"`

	// The execution type for the Executable.
	// +kubebuilder:default:=Process
	ExecutionType ExecutionType `json:"executionType,omitempty"`

	// Controls behavior of environment variables inherited from the controller process.
	AmbientEnvironment AmbientEnvironment `json:"ambientEnvironment,omitempty"`

	// Should the controller attempt to stop the Executable
	// +kubebuilder:default:=false
	Stop bool `json:"stop,omitempty"`

	// Health probe configuration for the Executable
	// +listType=atomic
	HealthProbes []HealthProbe `json:"healthProbes,omitempty"`
}

func (es ExecutableSpec) Equal(other ExecutableSpec) bool {
	if es.ExecutablePath != other.ExecutablePath {
		return false
	}

	if es.WorkingDirectory != other.WorkingDirectory {
		return false
	}

	if !stdslices.Equal(es.Args, other.Args) {
		return false
	}

	if !stdslices.Equal(es.Env, other.Env) {
		return false
	}

	if !stdslices.Equal(es.EnvFiles, other.EnvFiles) {
		return false
	}

	if es.ExecutionType != other.ExecutionType {
		return false
	}

	if es.AmbientEnvironment != other.AmbientEnvironment {
		return false
	}

	if es.Stop != other.Stop {
		return false
	}

	if len(es.HealthProbes) != len(other.HealthProbes) {
		return false
	}

	for i, probe := range es.HealthProbes {
		if !probe.Equal(&other.HealthProbes[i]) {
			return false
		}
	}

	return true
}

func (es ExecutableSpec) Validate(specPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}

	if es.ExecutablePath == "" {
		errorList = append(errorList, field.Invalid(specPath.Child("executablePath"), es.ExecutablePath, "Executable path is required."))
	}

	invalidExecutionType := es.ExecutionType != "" && es.ExecutionType != ExecutionTypeProcess && es.ExecutionType != ExecutionTypeIDE
	if invalidExecutionType {
		errorList = append(errorList, field.Invalid(specPath.Child("executionType"), es.ExecutionType, "Execution type must be either Process or IDE."))
	}

	invalidAmbientEnvironmentBehavior := es.AmbientEnvironment.Behavior != "" &&
		es.AmbientEnvironment.Behavior != EnvironmentBehaviorInherit &&
		es.AmbientEnvironment.Behavior != EnvironmentBehaviorDoNotInherit
	if invalidAmbientEnvironmentBehavior {
		errorList = append(errorList, field.Invalid(specPath.Child("ambientEnvironment", "behavior"), es.AmbientEnvironment.Behavior, "Ambient environment behavior must be either Inherit or DoNotInherit."))
	}

	healthProbesPath := specPath.Child("healthProbes")
	for i, probe := range es.HealthProbes {
		errorList = append(errorList, probe.Validate(healthProbesPath.Index(i))...)
	}

	return errorList
}

// ExecutableStatus describes the status of an Executable.
// +k8s:openapi-gen=true
type ExecutableStatus struct {
	// The execution ID is the identifier for the actual-state counterpart of the Executable.
	// For ExecutionType == Process it is the process ID. Process IDs will be eventually reused by OS,
	// but a combination of process ID and startup timestamp is unique for each Executable instance.
	// For ExecutionType == IDE it is the IDE session ID.
	// +optional
	ExecutionID string `json:"executionID"`

	// The PID of the process associated with the Executable
	// For either ExecutionType == Process or ExecutionType == IDE, this is the PID of the process that runs the Executable.
	// +optional
	PID *int64 `json:"pid,omitempty"`

	// The current state of the process/IDE session started for this executable
	State ExecutableState `json:"state"`

	// Start (attempt) timestamp.
	StartupTimestamp metav1.MicroTime `json:"startupTimestamp,omitempty"`

	// The time when the process/IDE session finished execution
	FinishTimestamp metav1.MicroTime `json:"finishTimestamp,omitempty"`

	// Exit code of the process associated with the Executable.
	// The value is equal to UnknownExitCode if the Executable was not started, is still running, or the exit code is not available.
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`

	// The path of a temporary file that contains captured standard output data from the Executable process.
	StdOutFile string `json:"stdOutFile,omitempty"`

	// The path of a temporary file that contains captured standard error data from the Executable process.
	StdErrFile string `json:"stdErrFile,omitempty"`

	// Effective values of environment variables, after all substitutions are applied.
	// +listType=map
	// +listMapKey=name
	EffectiveEnv []EnvVar `json:"effectiveEnv,omitempty"`

	// Effective values of launch arguments to be passed to the Executable, after all substitutions are applied.
	// +listType=atomic
	EffectiveArgs []string `json:"effectiveArgs,omitempty"`

	// Health status of the Executable
	HealthStatus HealthStatus `json:"healthStatus,omitempty"`

	// Results of running health probes (most reacent per probe)
	// +listType=map
	// +listMapKey=probeName
	HealthProbeResults []HealthProbeResult `json:"healthProbeResults,omitempty"`
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

// StdOutFile implements StdOutStreamableResource.
func (e *Executable) GetStdOutFile() string {
	return e.Status.StdOutFile
}

// StdErrFile implements StdOutStreamableResource.
func (e *Executable) GetStdErrFile() string {
	return e.Status.StdErrFile
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
	errorList := field.ErrorList{}

	errorList = append(errorList, e.Spec.Validate(field.NewPath("spec"))...)

	return errorList
}

func (e *Executable) ValidateUpdate(ctx context.Context, obj runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldExe := obj.(*Executable)
	if oldExe.Spec.Stop && e.Spec.Stop != oldExe.Spec.Stop {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "stop"), "Cannot unset stop property once it is set."))
	}

	if oldExe.Spec.AmbientEnvironment.Behavior != e.Spec.AmbientEnvironment.Behavior {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "ambientEnvironment", "behavior"), "Cannot change ambient environment behavior once it is set."))
	}

	if len(oldExe.Spec.HealthProbes) != len(e.Spec.HealthProbes) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "healthProbes"), "Health probes cannot be changed once an Executable is created."))
	} else {
		for i, probe := range oldExe.Spec.HealthProbes {
			if !probe.Equal(&e.Spec.HealthProbes[i]) {
				errorList = append(errorList, field.Forbidden(field.NewPath("spec", "healthProbes").Index(i), "Health probes cannot be changed once an Executable is created."))
			}
		}
	}

	return errorList
}

func (e *Executable) Done() bool {
	// When the Executable has a FinishTimestamp set, it is considered done no matter what other data says.
	return !e.Status.FinishTimestamp.IsZero()
}

func (*Executable) GenericSubResources() []apiserver_resource.GenericSubResource {
	return []apiserver_resource.GenericSubResource{
		&ExecutableLogResource{},
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

type ExecutableLogResource struct{}

func (elr *ExecutableLogResource) Name() string {
	return LogSubresourceName
}

func (elr *ExecutableLogResource) GetStorageProvider(
	obj apiserver_resource.Object,
	rootPath string,
	parentSP apiserver.StorageProvider,
) apiserver.StorageProvider {
	return func(scheme *runtime.Scheme, reg generic_registry.RESTOptionsGetter) (registry_rest.Storage, error) {
		storage, err := parentSP(scheme, reg)
		if err != nil {
			return nil, fmt.Errorf("failed to get parent (%s) storage: %w", obj.GetObjectKind().GroupVersionKind().Kind, err)
		}

		exeStorage, isGetter := storage.(registry_rest.StandardStorage)
		if !isGetter {
			return nil, fmt.Errorf("parent (%s) should implement registry_rest.Getter", obj.GetObjectKind().GroupVersionKind().Kind)
		}

		logStreamFactory, found := ResourceLogStreamers.Load(obj.GetGroupVersionResource())
		if !found {
			return nil, fmt.Errorf("log stream factory not found for resource '%s'", obj.GetGroupVersionResource().String())
		}

		logStorage, err := NewLogStorage(exeStorage, logStreamFactory)
		if err != nil {
			return nil, err
		}

		return logStorage, nil
	}
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
var _ apiserver_resourcestrategy.ValidateUpdater = (*Executable)(nil)
var _ apiserver_resource.ObjectWithGenericSubResource = (*Executable)(nil)
var _ apiserver_resource.GenericSubResource = (*ExecutableLogResource)(nil)
var _ StdIoStreamableResource = (*Executable)(nil)
