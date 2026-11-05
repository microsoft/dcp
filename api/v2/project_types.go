/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	apiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// DefaultDebugAdapterConnectionTimeout bounds adapter transport setup when no timeout is supplied.
const DefaultDebugAdapterConnectionTimeout = 10 * time.Second

// ProjectDebugShutdownGrace bounds protocol shutdown before forced session and backing cleanup.
const ProjectDebugShutdownGrace = 5 * time.Second

// DebugSubresourceName identifies the bidirectional Project DAP endpoint.
const DebugSubresourceName = "debug"

// DebugAdapterMode selects the adapter's DAP transport.
// +kubebuilder:validation:Enum=stdio;tcp-connect;tcp-callback
type DebugAdapterMode string

const (
	// DebugAdapterModeStdio uses a separate adapter's stdin/stdout for each debug stream.
	DebugAdapterModeStdio DebugAdapterMode = "stdio"
	// DebugAdapterModeTCPConnect connects debug streams to a shared listening adapter provider.
	DebugAdapterModeTCPConnect DebugAdapterMode = "tcp-connect"
	// DebugAdapterModeTCPCallback accepts an outbound adapter connection on a separate listener per stream.
	DebugAdapterModeTCPCallback DebugAdapterMode = "tcp-callback"
)

// DebugAdapterPlacement selects the adapter's execution context.
type DebugAdapterPlacement string

const (
	DebugAdapterPlacementHost      DebugAdapterPlacement = "host"
	DebugAdapterPlacementContainer DebugAdapterPlacement = "container"
)

// DebugAdapterConfig declares an adapter to launch only on an explicit debug-subresource request.
// DAP initialize, launch, and attach configuration is supplied over the debug stream, not through this spec.
// +k8s:openapi-gen=true
type DebugAdapterConfig struct {
	// Placement selects host execution or execution in the prepared Project container. Defaults to host.
	// Container placement currently supports only stdio.
	// +kubebuilder:validation:Enum=host;container
	// +kubebuilder:default:=host
	Placement DebugAdapterPlacement `json:"placement,omitempty"`
	// Args contains the executable name or path followed by its arguments.
	// TCP modes replace {{port}} in these values with a DCP-allocated loopback port.
	// +listType=atomic
	Args []string `json:"args"`
	// Mode selects the adapter DAP transport. Defaults to stdio.
	// +kubebuilder:default:=stdio
	Mode DebugAdapterMode `json:"mode,omitempty"`
	// Env contains explicit adapter environment variables that override inherited values.
	// +listType=map
	// +listMapKey=name
	Env []commonapi.EnvVar `json:"env,omitempty"`
	// ConnectionTimeout bounds pre-upgrade adapter transport setup, including shared-provider availability.
	// Omitted or zero uses 10 seconds. It does not limit the established debug stream or DAP initialization.
	ConnectionTimeout *metav1.Duration `json:"connectionTimeout,omitempty"`
}

// EffectivePlacement defaults omitted adapter placement to host.
func (config DebugAdapterConfig) EffectivePlacement() DebugAdapterPlacement {
	if config.Placement == "" {
		return DebugAdapterPlacementHost
	}
	return config.Placement
}

// EffectiveMode returns the declared transport, defaulting to stdio when omitted.
func (config DebugAdapterConfig) EffectiveMode() DebugAdapterMode {
	if config.Mode == "" {
		return DebugAdapterModeStdio
	}
	return config.Mode
}

// GetConnectionTimeout returns the setup timeout, defaulting omitted or zero values to 10 seconds.
func (config DebugAdapterConfig) GetConnectionTimeout() time.Duration {
	if config.ConnectionTimeout == nil || config.ConnectionTimeout.Duration == 0 {
		return DefaultDebugAdapterConnectionTimeout
	}
	return config.ConnectionTimeout.Duration
}

func (config *DebugAdapterConfig) validate(configPath *field.Path) field.ErrorList {
	var errorList field.ErrorList
	switch config.EffectivePlacement() {
	case DebugAdapterPlacementHost:
	case DebugAdapterPlacementContainer:
		if config.EffectiveMode() != DebugAdapterModeStdio {
			errorList = append(errorList, field.Forbidden(configPath.Child("mode"), "container adapter placement currently supports only stdio"))
		}
	default:
		errorList = append(errorList, field.NotSupported(configPath.Child("placement"), config.Placement, []string{"host", "container"}))
	}
	if len(config.Args) == 0 {
		errorList = append(errorList, field.Required(configPath.Child("args"), "args must contain the adapter executable"))
	} else if strings.TrimSpace(config.Args[0]) == "" {
		errorList = append(errorList, field.Required(configPath.Child("args").Index(0), "adapter executable must be set"))
	}
	switch config.EffectiveMode() {
	case DebugAdapterModeStdio, DebugAdapterModeTCPConnect, DebugAdapterModeTCPCallback:
	default:
		errorList = append(errorList, field.NotSupported(configPath.Child("mode"), config.Mode, []string{
			string(DebugAdapterModeStdio), string(DebugAdapterModeTCPConnect), string(DebugAdapterModeTCPCallback),
		}))
	}
	for i, env := range config.Env {
		if env.Name == "" {
			errorList = append(errorList, field.Required(configPath.Child("env").Index(i).Child("name"), "name must be set"))
		}
	}
	if config.ConnectionTimeout != nil && config.ConnectionTimeout.Duration < 0 {
		errorList = append(errorList, field.Invalid(configPath.Child("connectionTimeout"), config.ConnectionTimeout.String(), "connectionTimeout must not be negative"))
	}
	return errorList
}

// ProjectDebugPhase summarizes adapter transport setup and execution, not DAP initialization or application readiness.
type ProjectDebugPhase string

const (
	ProjectDebugPhasePending ProjectDebugPhase = "Pending"
	ProjectDebugPhaseRunning ProjectDebugPhase = "Running"
	ProjectDebugPhaseExited  ProjectDebugPhase = "Exited"
	ProjectDebugPhaseFailed  ProjectDebugPhase = "Failed"
)

// ProjectDebugStatus reports adapter executions and sessions independently of application status.
// Entries are retained until Project deletion; active provider references are not reconstructed from this history.
// +k8s:openapi-gen=true
type ProjectDebugStatus struct {
	// PrerequisitesReady reports controller-observed execution prerequisites, not adapter or application readiness.
	// Process prerequisites do not require the application to be running; container prerequisites require a prepared context.
	PrerequisitesReady bool `json:"prerequisitesReady,omitempty"`
	// Executions contains DCP-owned adapter process and shared provider observations.
	// +listType=map
	// +listMapKey=id
	Executions []ProjectDebugExecutionStatus `json:"executions,omitempty"`
	// Sessions contains pending, active, and completed debug stream observations.
	// +listType=map
	// +listMapKey=id
	Sessions []ProjectDebugSessionStatus `json:"sessions,omitempty"`
}

// ProjectDebugExecutionStatus reports a host adapter process or attached container execution.
// Attached execution completion alone does not establish in-container process termination.
// +k8s:openapi-gen=true
type ProjectDebugExecutionStatus struct {
	// ID identifies this execution independently of its reusable operating system PID.
	ID string `json:"id"`
	// Phase summarizes adapter launch and process availability.
	// +kubebuilder:validation:Enum=Pending;Running;Exited;Failed
	Phase ProjectDebugPhase `json:"phase"`
	// PID is the host adapter process ID, absent for container executions and failed host launches.
	PID *int64 `json:"pid,omitempty"`
	// IdentityTimestamp identifies the process in pid for PID-reuse protection.
	IdentityTimestamp metav1.MicroTime `json:"identityTimestamp,omitempty"`
	// FinishedAt is when launch failure, host process completion or attached execution completion was observed.
	FinishedAt metav1.MicroTime `json:"finishedAt,omitempty"`
	// ExitCode is the observed adapter process exit code, when known.
	ExitCode *int32 `json:"exitCode,omitempty"`
	// Conditions describe adapter execution readiness and specific failures.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ProjectDebugSessionStatus reports one debug stream's transport lifetime independently of its provider process.
// +k8s:openapi-gen=true
type ProjectDebugSessionStatus struct {
	// ID identifies this stream independently of other sessions using the same adapter provider.
	ID string `json:"id"`
	// ExecutionID references an entry in debug.executions when DCP owns its adapter process.
	ExecutionID string `json:"executionID,omitempty"`
	// ProtocolRequests retains initialize, launch and attach outcomes independently of transport phase.
	// Arguments, environment and adapter-specific response bodies are not retained.
	// +listType=map
	// +listMapKey=requestSeq
	ProtocolRequests []ProjectDebugRequestStatus `json:"protocolRequests,omitempty"`
	// Processes retains adapter-reported targets independently of the stream and adapter lifetimes.
	// +listType=map
	// +listMapKey=id
	Processes []ProjectDebugProcessStatus `json:"processes,omitempty"`
	// ProcessTrackingUnconfirmed indicates targets whose local identity cannot be confirmed.
	ProcessTrackingUnconfirmed bool `json:"processTrackingUnconfirmed,omitempty"`
	// ProcessTrackingMessage describes missing or unavailable target identity information.
	ProcessTrackingMessage string `json:"processTrackingMessage,omitempty"`
	// Phase reports transport setup, established transport, normal closure, or failure.
	// +kubebuilder:validation:Enum=Pending;Running;Exited;Failed
	Phase ProjectDebugPhase `json:"phase"`
	// FinishedAt is when session closure or failure was observed.
	FinishedAt metav1.MicroTime `json:"finishedAt,omitempty"`
	// Conditions describe transport readiness and specific session failures.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type ProjectDebugRequestPhase string

const (
	ProjectDebugRequestPending     ProjectDebugRequestPhase = "Pending"
	ProjectDebugRequestSucceeded   ProjectDebugRequestPhase = "Succeeded"
	ProjectDebugRequestFailed      ProjectDebugRequestPhase = "Failed"
	ProjectDebugRequestInterrupted ProjectDebugRequestPhase = "Interrupted"
)

// ProjectDebugRequestStatus records an API-server-observed client request, not target health or execution.
// +k8s:openapi-gen=true
type ProjectDebugRequestStatus struct {
	// RequestSeq is the originating client's DAP sequence number, scoped to this session.
	// +kubebuilder:validation:Minimum=1
	RequestSeq int64 `json:"requestSeq"`
	// Command identifies the observed protocol operation.
	// +kubebuilder:validation:Enum=initialize;launch;attach
	Command string `json:"command"`
	// Phase records a pending request, its response outcome, or session closure before a response.
	// +kubebuilder:validation:Enum=Pending;Succeeded;Failed;Interrupted
	Phase ProjectDebugRequestPhase `json:"phase"`
	// FinishedAt records when a response or interruption was observed.
	FinishedAt metav1.MicroTime `json:"finishedAt,omitempty"`
	// Message contains the adapter's response message or the reason no response was observed.
	Message string `json:"message,omitempty"`
}

// ProjectDebugProcessStatus registers one adapter-reported process instance with its originating session.
// Registration fields are API-server-owned; Observation is controller-owned.
// +k8s:openapi-gen=true
type ProjectDebugProcessStatus struct {
	// ID identifies this registration independently of reusable operating system PIDs.
	ID string `json:"id"`
	// Name is the diagnostic name supplied by the adapter.
	Name string `json:"name,omitempty"`
	// PID is the reported operating system process ID, if provided.
	PID *int64 `json:"pid,omitempty"`
	// ContainerID scopes a reported PID to the prepared container, never the DCP host.
	ContainerID string `json:"containerID,omitempty"`
	// IdentityTimestamp captures local PID-reuse identity; absent when identity could not be resolved.
	// On Linux this represents elapsed time since boot, not a wall-clock start time.
	IdentityTimestamp metav1.MicroTime `json:"identityTimestamp,omitempty"`
	// TrackingError reports unavailable or non-local process identity without failing the DAP session.
	TrackingError string `json:"trackingError,omitempty"`
	// Observation reports Project-owned process observation and cleanup independently of session phase.
	Observation ProjectDebugProcessObservation `json:"observation,omitempty"`
}

type ProjectDebugProcessPhase string

const (
	ProjectDebugProcessUnknown ProjectDebugProcessPhase = "Unknown"
	ProjectDebugProcessRunning ProjectDebugProcessPhase = "Running"
	ProjectDebugProcessExited  ProjectDebugProcessPhase = "Exited"
)

type ProjectDebugProcessCleanup string

const (
	ProjectDebugProcessCleanupComplete    ProjectDebugProcessCleanup = "Complete"
	ProjectDebugProcessCleanupUnconfirmed ProjectDebugProcessCleanup = "Unconfirmed"
)

// ProjectDebugProcessObservation describes an observed process instance and its cleanup disposition.
// +k8s:openapi-gen=true
type ProjectDebugProcessObservation struct {
	// Phase is Unknown, Running, or Exited; Exited also covers confirmed absence or PID reuse.
	// +kubebuilder:validation:Enum=Unknown;Running;Exited
	Phase ProjectDebugProcessPhase `json:"phase,omitempty"`
	// FinishedAt records when exit or absence was confirmed, without claiming an unavailable exit code.
	FinishedAt metav1.MicroTime `json:"finishedAt,omitempty"`
	// Cleanup is empty while pending, Complete when confirmed, or Unconfirmed when cleanup cannot be confirmed.
	// +kubebuilder:validation:Enum=Complete;Unconfirmed
	Cleanup ProjectDebugProcessCleanup `json:"cleanup,omitempty"`
	// Message describes observation failures or unconfirmed cleanup.
	Message string `json:"message,omitempty"`
}

// ProjectPhase summarizes preparation and debug-session coordination.
type ProjectPhase string

const (
	ProjectPhasePending  ProjectPhase = "Pending"
	ProjectPhaseReady    ProjectPhase = "Ready"
	ProjectPhaseActive   ProjectPhase = "Active"
	ProjectPhaseStopping ProjectPhase = "Stopping"
	ProjectPhaseStopped  ProjectPhase = "Stopped"
	ProjectPhaseUnknown  ProjectPhase = "Unknown"
	ProjectPhaseFailed   ProjectPhase = "Failed"
)

// ProjectProcessTemplate combines process launch inputs with service-backed listening ports.
// +k8s:openapi-gen=true
type ProjectProcessTemplate struct {
	LogicalProcessConfig `json:",inline"`
	// Ports declares named service-backed endpoints for the process.
	// +listType=atomic
	Ports []LogicalProcessPort `json:"ports,omitempty"`
}

// ProjectContainerTemplate combines container launch inputs with service-backed published ports.
// +k8s:openapi-gen=true
type ProjectContainerTemplate struct {
	LogicalContainerConfig `json:",inline"`
	// Ports declares named service-backed endpoints for the container.
	// +listType=atomic
	Ports []LogicalContainerPort `json:"ports,omitempty"`
}

// ProjectSpec identifies source code and exactly one process or container launch configuration.
// +k8s:openapi-gen=true
type ProjectSpec struct {
	// RootDirectory is an absolute source directory on the DCP host. It is informational:
	// it does not default working directories, resolve paths, or create container mounts.
	RootDirectory string `json:"rootDirectory"`
	// Process describes how to run the code as a process. Exactly one of process or container must be set.
	Process *ProjectProcessTemplate `json:"process,omitempty"`
	// Container describes how to run the code as a container. Exactly one of process or container must be set.
	Container *ProjectContainerTemplate `json:"container,omitempty"`
	// DebugAdapter declares an adapter; configuration alone does not launch it.
	// Host placement is valid with either application template; container placement requires a container template.
	DebugAdapter *DebugAdapterConfig `json:"debugAdapter,omitempty"`
	// Stop requests termination or prevents launch. Once true, it cannot be reset.
	Stop bool `json:"stop,omitempty"`
}

// ProjectStatus describes prepared execution context and debugging progress.
// +k8s:openapi-gen=true
type ProjectStatus struct {
	// EffectiveArgs contains evaluated application launch arguments, never the container placeholder's arguments.
	// +listType=atomic
	EffectiveArgs []string `json:"effectiveArgs,omitempty"`
	// EffectiveEnv contains the prepared launch environment, including permitted process inheritance.
	// +listType=map
	// +listMapKey=name
	EffectiveEnv []commonapi.EnvVar `json:"effectiveEnv,omitempty"`
	// LaunchInputsReady distinguishes a fully prepared snapshot from empty or unavailable inputs.
	LaunchInputsReady bool `json:"launchInputsReady,omitempty"`
	// Ports reports allocated listening addresses and selected services, independently of proxy readiness.
	// +listType=map
	// +listMapKey=name
	Ports []LogicalPortStatus `json:"ports,omitempty"`
	// Debug contains controller-owned prerequisite readiness and API-server-owned adapter/session observations.
	Debug *ProjectDebugStatus `json:"debug,omitempty"`
	// Phase summarizes Project preparation, debug activity and shutdown.
	// +kubebuilder:validation:Enum=Pending;Ready;Active;Stopping;Stopped;Unknown;Failed
	Phase ProjectPhase `json:"phase,omitempty"`
	// PhysicalContainerRef identifies the same-namespace backing PhysicalContainer.
	// Cross-namespace references are not supported.
	PhysicalContainerRef string `json:"physicalContainerRef,omitempty"`
	// ContainerRuntime identifies the controller-selected runtime for the prepared container.
	ContainerRuntime string `json:"containerRuntime,omitempty"`
	// ImageRef identifies the supplied or owned PhysicalContainerImage.
	// Cross-namespace references are not supported.
	ImageRef string `json:"imageRef,omitempty"`
	// ContainerID is the observed runtime container identity.
	ContainerID string `json:"containerID,omitempty"`
	// ContainerName is the observed runtime container name.
	ContainerName string `json:"containerName,omitempty"`
	// Image is the resolved runtime image reference.
	Image string `json:"image,omitempty"`
	// CreatedAt is the runtime container creation time.
	CreatedAt metav1.MicroTime `json:"createdAt,omitempty"`
	// StartedAt is the runtime container start time.
	StartedAt metav1.MicroTime `json:"startedAt,omitempty"`
	// PortMappings are the observed host bindings for container ports.
	// +listType=atomic
	PortMappings []PhysicalContainerPortMapping `json:"portMappings,omitempty"`
	// RuntimeLogs describes capture owned by the immediate physical execution.
	RuntimeLogs *LogSource `json:"runtimeLogs,omitempty"`
	// StartupLogs describes image acquisition capture for container execution.
	StartupLogs *LogSource `json:"startupLogs,omitempty"`
	// Conditions describe readiness and reconciliation progress.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func (status ProjectStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	status.DeepCopyInto(&dest.(*Project).Status)
}

// Project prepares user-code launch inputs and execution context for AppHost-controlled debugging.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=projects
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ProjectSpec   `json:"spec,omitempty"`
	Status            ProjectStatus `json:"status,omitempty"`
}

func (*Project) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupName, Version: Version, Resource: "projects"}
}

// GetResourceId identifies the Project system log stream.
func (resource *Project) GetResourceId() string {
	return fmt.Sprintf("project-%s", resource.UID)
}

// GetLogSource returns the observed immediate physical execution output source.
func (resource *Project) GetLogSource() *LogSource {
	return resource.Status.RuntimeLogs
}

func (*Project) GenericSubResources() []apiserver_resource.GenericSubResource {
	return []apiserver_resource.GenericSubResource{&ProjectLogResource{}}
}

// ProjectLogResource registers Project output and system log storage.
// +k8s:openapi-gen=false
// +kubebuilder:object:generate=false
type ProjectLogResource struct{}

func (*ProjectLogResource) Name() string { return LogSubresourceName }

func (*ProjectLogResource) GetStorageProvider(
	obj apiserver_resource.Object, _ string, parentProvider apiserver.StorageProvider,
) apiserver.StorageProvider {
	return newLogStorageProvider(obj, parentProvider)
}

func (resource *Project) GetObjectMeta() *metav1.ObjectMeta               { return &resource.ObjectMeta }
func (resource *Project) GetStatus() apiserver_resource.StatusSubResource { return resource.Status }
func (*Project) New() runtime.Object                                      { return &Project{} }
func (*Project) NewList() runtime.Object                                  { return &ProjectList{} }
func (*Project) IsStorageVersion() bool                                   { return true }
func (*Project) NamespaceScoped() bool                                    { return true }
func (resource *Project) NamespacedName() types.NamespacedName            { return NamespacedName(resource) }

// PrepareForCreate prevents callers from supplying prepared inputs or debug observations.
func (resource *Project) PrepareForCreate(context.Context) {
	resource.Status = ProjectStatus{}
}

func (resource *Project) Validate(context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(resource)
	specPath := field.NewPath("spec")
	if commonapi.ResourceCreationProhibited.Load() && resource.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}
	errorList = append(errorList, commonapi.ValidateAnnotationsSize(resource.Annotations, field.NewPath("metadata", "annotations"))...)
	if !filepath.IsAbs(resource.Spec.RootDirectory) || strings.ContainsRune(resource.Spec.RootDirectory, '\x00') {
		errorList = append(errorList, field.Invalid(specPath.Child("rootDirectory"), resource.Spec.RootDirectory, "rootDirectory must be an absolute host directory path"))
	}
	if resource.Spec.Process == nil && resource.Spec.Container == nil {
		errorList = append(errorList, field.Required(specPath, "exactly one of process or container must be set"))
	}
	if resource.Spec.Process != nil && resource.Spec.Container != nil {
		errorList = append(errorList, field.Forbidden(specPath.Child("container"), "container cannot be set when process is set"))
	}
	if resource.Spec.Process != nil && strings.TrimSpace(resource.Spec.Process.ExecutablePath) == "" {
		errorList = append(errorList, field.Required(specPath.Child("process", "executablePath"), "executablePath must be set"))
	}
	if resource.Spec.Process != nil {
		errorList = append(errorList, validateLogicalProcessPorts(resource.Spec.Process.Ports, resource.Namespace, resource.Name, specPath.Child("process", "ports"))...)
		errorList = append(errorList, resource.Spec.Process.AmbientEnvironment.validate(specPath.Child("process", "ambientEnvironment"))...)
	}
	if resource.Spec.Container != nil {
		errorList = append(errorList, resource.Spec.Container.Validate(resource.Namespace, specPath.Child("container"))...)
		errorList = append(errorList, validateLogicalContainerPorts(resource.Spec.Container.Ports, resource.Namespace, resource.Name, specPath.Child("container", "ports"))...)
	}
	if resource.Spec.DebugAdapter != nil {
		errorList = append(errorList, resource.Spec.DebugAdapter.validate(specPath.Child("debugAdapter"))...)
		if resource.Spec.DebugAdapter.EffectivePlacement() == DebugAdapterPlacementContainer && resource.Spec.Container == nil {
			errorList = append(errorList, field.Forbidden(specPath.Child("debugAdapter", "placement"), "container adapter placement requires a container Project"))
		}
	}
	return errorList
}

func (resource *Project) ValidateUpdate(_ context.Context, old runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}
	oldResource := old.(*Project)
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

// ProjectList contains a list of Project instances.
// +kubebuilder:object:root=true
// +k8s:openapi-gen=true
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

func (list *ProjectList) GetListMeta() *metav1.ListMeta { return &list.ListMeta }
func (list *ProjectList) ItemCount() uint32             { return uint32(len(list.Items)) }
func (list *ProjectList) GetItems() []*Project {
	items := make([]*Project, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}
	return items
}

func init() {
	SchemeBuilder.Register(&Project{}, &ProjectList{})
}

var _ apiserver_resource.Object = (*Project)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*Project)(nil)
var _ apiserver_resource.ObjectWithGenericSubResource = (*Project)(nil)
var _ apiserver_resource.GenericSubResource = (*ProjectLogResource)(nil)
var _ apiserver_resource.StatusSubResource = (*ProjectStatus)(nil)
var _ apiserver_resource.ObjectList = (*ProjectList)(nil)
var _ commonapi.ListWithObjectItems[Project, *Project] = (*ProjectList)(nil)
var _ apiserver_resourcestrategy.Validater = (*Project)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*Project)(nil)
