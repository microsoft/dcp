/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	apiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// DefaultDebugAdapterConnectionTimeout bounds adapter transport setup when no timeout is supplied.
const DefaultDebugAdapterConnectionTimeout = 10 * time.Second

// DefaultContainerDebugAdapterPort is the internal listening port when no container TCP port is supplied.
const DefaultContainerDebugAdapterPort int32 = 4711

// DefaultContainerDebugAdapterDirectory is the installation directory for container adapters.
const DefaultContainerDebugAdapterDirectory = "/opt/dcp/debug-adapter"

// ProjectDebugShutdownGrace bounds protocol shutdown before forced session and backing cleanup.
const ProjectDebugShutdownGrace = 5 * time.Second

// Project reconciliation condition reasons.
const (
	// ProjectReasonPreparing indicates that prerequisites or execution context are being prepared.
	ProjectReasonPreparing ConditionReason = "Preparing"
	// ProjectReasonPrepared indicates that the prepared context is available for debug sessions.
	ProjectReasonPrepared ConditionReason = "Prepared"
	// ProjectReasonAwaitingStart indicates that prepared execution is waiting for start.
	ProjectReasonAwaitingStart ConditionReason = "AwaitingStart"
	// ProjectReasonApplicationRunning indicates that a directly launched application is running.
	ProjectReasonApplicationRunning ConditionReason = "ApplicationRunning"
	// ProjectReasonCompleted indicates that the run has ended: the directly launched application or the
	// container's primary adapter exited, or the main debug session of an adapter-mode ProjectProcess ended.
	// Runs are one-shot; recreate the Project to run again.
	ProjectReasonCompleted ConditionReason = "Completed"
	// ProjectReasonDebugSessionsActive indicates that the prepared context has active debug sessions.
	ProjectReasonDebugSessionsActive ConditionReason = "DebugSessionsActive"
	// ProjectReasonPaused indicates that the prepared container is paused.
	ProjectReasonPaused ConditionReason = "Paused"
	// ProjectReasonStopping indicates that session, adapter, or backing cleanup is in progress.
	ProjectReasonStopping ConditionReason = "Stopping"
	// ProjectReasonStopped indicates that requested shutdown has completed.
	ProjectReasonStopped ConditionReason = "Stopped"
	// ProjectReasonPreparationFailed indicates that preparation could not complete.
	ProjectReasonPreparationFailed ConditionReason = "PreparationFailed"
	// ProjectReasonMissing indicates that a previously prepared context is unavailable or no longer owned.
	ProjectReasonMissing ConditionReason = "Missing"
	// ProjectReasonLookupFailed indicates that the prepared context's current state cannot be confirmed.
	ProjectReasonLookupFailed ConditionReason = "LookupFailed"
)

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

// DebugAdapterConfig declares a host adapter to launch only on an explicit debug-subresource request.
// DAP initialize, launch, and attach configuration is supplied over the debug stream, not through this spec.
// +k8s:openapi-gen=true
type DebugAdapterConfig struct {
	// Directory is the absolute host directory containing the adapter and its dependencies.
	Directory string `json:"directory"`
	// Args contains the executable name or path followed by its arguments.
	// TCP modes replace {{port}} with the selected listening port.
	// {{adapterDirectory}} expands to the adapter directory in the execution context, and {{sourceDirectory}}
	// to the Project source directory there: rootDirectory for processes, source.containerDirectory for containers.
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

// ContainerDebugAdapterConfig declares an adapter copied into and launched inside the prepared container.
// +k8s:openapi-gen=true
type ContainerDebugAdapterConfig struct {
	// DebugAdapterConfig.Directory is copied from the DCP host into the container before the prepared container starts.
	DebugAdapterConfig `json:",inline"`
	// ContainerDirectory overrides the container installation directory, defaulting to /opt/dcp/debug-adapter.
	ContainerDirectory string `json:"containerDirectory,omitempty"`
	// ExecutableFiles lists regular files relative to Directory that gain execute permissions when copied into the container.
	// Paths use forward slashes without "." or ".." components.
	// +listType=atomic
	ExecutableFiles []string `json:"executableFiles,omitempty"`
	// TCP configures the internal listener for tcp-connect adapters.
	TCP *DebugAdapterTCPConfig `json:"tcp,omitempty"`
}

// DebugAdapterTCPConfig declares the internal container listener, not its runtime-assigned host binding.
// +k8s:openapi-gen=true
type DebugAdapterTCPConfig struct {
	// Port is the internal listening port. Omitted or zero uses 4711.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
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

// GetTCPPort returns the selected internal TCP port, or zero when no listener is required.
func (config ContainerDebugAdapterConfig) GetTCPPort() int32 {
	if config.EffectiveMode() != DebugAdapterModeTCPConnect {
		return 0
	}
	if config.TCP != nil && config.TCP.Port != 0 {
		return config.TCP.Port
	}
	return DefaultContainerDebugAdapterPort
}

// GetContainerDirectory returns the adapter installation directory inside the container.
func (config ContainerDebugAdapterConfig) GetContainerDirectory() string {
	if config.ContainerDirectory != "" {
		return config.ContainerDirectory
	}
	return DefaultContainerDebugAdapterDirectory
}

// expandDebugAdapterArgs resolves adapter- and source-directory references without interpreting other argument contents.
// An empty sourceDirectory leaves {{sourceDirectory}} unexpanded; admission rejects it where no source directory is declared.
func expandDebugAdapterArgs(args []string, adapterDirectory, sourceDirectory string) []string {
	arguments := make([]string, len(args))
	for index, argument := range args {
		argument = strings.ReplaceAll(argument, "{{adapterDirectory}}", adapterDirectory)
		if sourceDirectory != "" {
			argument = strings.ReplaceAll(argument, "{{sourceDirectory}}", sourceDirectory)
		}
		arguments[index] = argument
	}
	return arguments
}

func (config *DebugAdapterConfig) validate(configPath *field.Path) field.ErrorList {
	var errorList field.ErrorList
	if !filepath.IsAbs(config.Directory) || strings.ContainsRune(config.Directory, '\x00') {
		errorList = append(errorList, field.Invalid(configPath.Child("directory"), config.Directory, "directory must be an absolute host directory path"))
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

func (config *ContainerDebugAdapterConfig) validate(configPath *field.Path) field.ErrorList {
	errorList := config.DebugAdapterConfig.validate(configPath)
	errorList = append(errorList, validateExecutableFiles(config.ExecutableFiles, configPath.Child("executableFiles"))...)
	if config.ContainerDirectory != "" && (!path.IsAbs(config.ContainerDirectory) || strings.ContainsRune(config.ContainerDirectory, '\x00')) {
		errorList = append(errorList, field.Invalid(configPath.Child("containerDirectory"), config.ContainerDirectory, "containerDirectory must be an absolute container path"))
	}
	if config.EffectiveMode() == DebugAdapterModeTCPCallback {
		errorList = append(errorList, field.Forbidden(configPath.Child("mode"), "container adapters do not support tcp-callback"))
	}
	if config.TCP != nil {
		if config.EffectiveMode() != DebugAdapterModeTCPConnect {
			errorList = append(errorList, field.Forbidden(configPath.Child("tcp"), "TCP options require tcp-connect mode"))
		}
		if config.TCP.Port < 0 || config.TCP.Port > 65535 {
			errorList = append(errorList, field.Invalid(configPath.Child("tcp", "port"), config.TCP.Port, "port must be between 0 and 65535"))
		}
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
	// Progress reports bridge-owned connection state and established DAP milestones.
	Progress *ProjectDebugSessionProgress `json:"progress,omitempty"`
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

type ProjectDebugConnectionPhase string

const (
	ProjectDebugConnectionConnected    ProjectDebugConnectionPhase = "Connected"
	ProjectDebugConnectionTerminating  ProjectDebugConnectionPhase = "Terminating"
	ProjectDebugConnectionDisconnected ProjectDebugConnectionPhase = "Disconnected"
)

// ProjectDebugSessionProgress reports current connection state and facts observed during one bridge session.
// Milestones remain established once observed; termination and disconnection cannot reopen the connection.
// +k8s:openapi-gen=true
type ProjectDebugSessionProgress struct {
	// Phase describes the bridge connection, independently of adapter process and target lifetimes.
	// +kubebuilder:validation:Enum=Connected;Terminating;Disconnected
	Phase ProjectDebugConnectionPhase `json:"phase"`
	// StreamsConnected records that both client and adapter streams were connected.
	StreamsConnected bool `json:"streamsConnected,omitempty"`
	// InitializeRequested records a client initialize request.
	InitializeRequested bool `json:"initializeRequested,omitempty"`
	// LaunchRequested records a client launch request, not successful target startup.
	LaunchRequested bool `json:"launchRequested,omitempty"`
	// LaunchSucceeded records the adapter's successful launch response; adapters may omit initialized for launch.noDebug.
	LaunchSucceeded bool `json:"launchSucceeded,omitempty"`
	// AttachRequested records a client attach request, not successful target attachment.
	AttachRequested bool `json:"attachRequested,omitempty"`
	// AdapterInitialized records the adapter's initialized event.
	AdapterInitialized bool `json:"adapterInitialized,omitempty"`
	// ProcessReported records an adapter process event; Processes holds the target registrations.
	ProcessReported bool `json:"processReported,omitempty"`
	// ClientTerminating records a client terminate or disconnect request.
	ClientTerminating bool `json:"clientTerminating,omitempty"`
	// AdapterTerminating records the adapter's terminated event or termination request.
	AdapterTerminating bool `json:"adapterTerminating,omitempty"`
	// ResourceTerminating records shutdown initiated by DCP.
	ResourceTerminating bool `json:"resourceTerminating,omitempty"`
	// DebuggeeExited records the adapter's exited event, not independently observed target termination.
	DebuggeeExited bool `json:"debuggeeExited,omitempty"`
	// DebuggeeExitCode is the exit code supplied in the adapter's exited event.
	DebuggeeExitCode *int32 `json:"debuggeeExitCode,omitempty"`
	// ClientDisconnected records that the client stream was lost or closed.
	ClientDisconnected bool `json:"clientDisconnected,omitempty"`
	// AdapterDisconnected records that the adapter stream was lost or closed.
	AdapterDisconnected bool `json:"adapterDisconnected,omitempty"`
	// ClientDisconnectMessage describes client stream loss or bridge-initiated closure.
	ClientDisconnectMessage string `json:"clientDisconnectMessage,omitempty"`
	// AdapterDisconnectMessage describes adapter stream loss or bridge-initiated closure.
	AdapterDisconnectMessage string `json:"adapterDisconnectMessage,omitempty"`
	// Message is the current protocol diagnostic; request/response payloads and history are not retained.
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
	// Attached records an adapter-reported attachment; DCP does not take cleanup ownership of any reported target.
	Attached bool `json:"attached,omitempty"`
	// ContainerID scopes a reported PID to the prepared container, never the DCP host.
	ContainerID string `json:"containerID,omitempty"`
	// IdentityTimestamp captures local PID-reuse identity; absent when identity could not be resolved.
	// On Linux this represents elapsed time since boot, not a wall-clock start time.
	IdentityTimestamp metav1.MicroTime `json:"identityTimestamp,omitempty"`
	// TrackingError reports unavailable or non-local process identity without failing the DAP session.
	TrackingError string `json:"trackingError,omitempty"`
	// Observation reports process liveness independently of session phase, without taking cleanup ownership.
	Observation ProjectDebugProcessObservation `json:"observation,omitempty"`
}

type ProjectDebugProcessPhase string

const (
	ProjectDebugProcessUnknown ProjectDebugProcessPhase = "Unknown"
	ProjectDebugProcessRunning ProjectDebugProcessPhase = "Running"
	ProjectDebugProcessExited  ProjectDebugProcessPhase = "Exited"
)

// ProjectDebugProcessObservation describes the observed liveness of an adapter-reported process instance.
// +k8s:openapi-gen=true
type ProjectDebugProcessObservation struct {
	// Phase is Unknown, Running, or Exited; Exited also covers confirmed absence or PID reuse.
	// +kubebuilder:validation:Enum=Unknown;Running;Exited
	Phase ProjectDebugProcessPhase `json:"phase,omitempty"`
	// FinishedAt records when exit or absence was confirmed, without claiming an unavailable exit code.
	FinishedAt metav1.MicroTime `json:"finishedAt,omitempty"`
	// Message describes observation failures.
	Message string `json:"message,omitempty"`
}

// ProjectPhase summarizes preparation and debug-session coordination.
type ProjectPhase string

const (
	ProjectPhasePending  ProjectPhase = "Pending"
	ProjectPhaseReady    ProjectPhase = "Ready"
	ProjectPhaseActive   ProjectPhase = "Active"
	ProjectPhaseExited   ProjectPhase = "Exited"
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
	// Endpoints are published while a direct application is running or a ready debug session has requested launch.
	// +listType=atomic
	Ports []LogicalProcessPort `json:"ports,omitempty"`
}

// ProjectContainerTemplate combines container launch inputs with service-backed published ports.
// +k8s:openapi-gen=true
type ProjectContainerTemplate struct {
	LogicalContainerConfig `json:",inline"`
	// Ports declares named service-backed endpoints for the container.
	// Endpoints are published while a direct application is running or a ready debug session has requested launch.
	// +listType=atomic
	Ports []LogicalContainerPort `json:"ports,omitempty"`
}

// ProjectLaunchMode selects whether the application is launched by an adapter or directly by DCP.
// +kubebuilder:validation:Enum=adapter;direct
type ProjectLaunchMode string

const (
	// ProjectLaunchModeAdapter leaves application launch to the configured adapter through DAP.
	ProjectLaunchModeAdapter ProjectLaunchMode = "adapter"
	// ProjectLaunchModeDirect launches the application template directly for subsequent debug attachment.
	ProjectLaunchModeDirect ProjectLaunchMode = "direct"
)

// ProjectCommonSpec contains source configuration and lifecycle requests shared by Project kinds.
// +k8s:openapi-gen=true
type ProjectCommonSpec struct {
	// RootDirectory is an absolute source directory on the DCP host. It does not default working directories or
	// resolve paths; a ProjectContainer mounts it only when source.bind is set.
	RootDirectory string `json:"rootDirectory"`
	// LaunchMode selects adapter-driven application launch or direct execution of the application template.
	// Defaults to adapter. Direct execution does not require an adapter or a debug connection.
	// +kubebuilder:default:=adapter
	LaunchMode ProjectLaunchMode `json:"launchMode,omitempty"`
	// Start permits initial execution after preparation. Defaults to true.
	// Once true, it cannot be reset. Stop takes precedence over start.
	// +kubebuilder:default:=true
	Start *bool `json:"start,omitempty"`
	// Stop requests termination or prevents launch. Once true, it cannot be reset.
	Stop bool `json:"stop,omitempty"`
}

// ShouldStart reports whether initial execution is permitted.
func (spec *ProjectCommonSpec) ShouldStart() bool {
	return spec.Start == nil || *spec.Start
}

// runtimeLogStreams describes the immediate execution output; an exited Project completes only after its capture does.
func (status *ProjectCommonStatus) runtimeLogStreams() LogStreams {
	streams := LogStreams{OutputSupported: true, Runtime: status.RuntimeLogs}
	streams.Complete = status.Phase == ProjectPhaseStopped || status.Phase == ProjectPhaseFailed ||
		(status.Phase == ProjectPhaseExited && (streams.Runtime == nil || streams.Runtime.Complete))
	if streams.Runtime == nil && streams.Complete {
		streams.Runtime = &LogSource{Complete: true}
	}
	return streams
}

// EffectiveLaunchMode returns the selected launch mode, defaulting to adapter-driven launch.
func (spec *ProjectCommonSpec) EffectiveLaunchMode() ProjectLaunchMode {
	if spec.LaunchMode == "" {
		return ProjectLaunchModeAdapter
	}
	return spec.LaunchMode
}

// ProjectCommonStatus describes launch inputs, execution, and debugging progress shared by Project kinds.
// +k8s:openapi-gen=true
type ProjectCommonStatus struct {
	// EffectiveArgs contains evaluated application launch arguments.
	// +listType=atomic
	EffectiveArgs []string `json:"effectiveArgs,omitempty"`
	// EffectiveEnv contains the observed container environment, including image-provided values,
	// or the prepared process launch environment, including permitted host inheritance.
	// +listType=map
	// +listMapKey=name
	EffectiveEnv []commonapi.EnvVar `json:"effectiveEnv,omitempty"`
	// Ports reports allocated listening addresses and selected services, independently of proxy readiness.
	// +listType=map
	// +listMapKey=name
	Ports []LogicalPortStatus `json:"ports,omitempty"`
	// Debug contains API-server-owned adapter/session observations and controller-owned target observations.
	Debug *ProjectDebugStatus `json:"debug,omitempty"`
	// Phase summarizes Project preparation, execution, debug activity and shutdown.
	// +kubebuilder:validation:Enum=Pending;Ready;Active;Exited;Stopping;Stopped;Unknown;Failed
	Phase ProjectPhase `json:"phase,omitempty"`
	// FinishedAt is the observed completion time of the directly launched application.
	FinishedAt metav1.MicroTime `json:"finishedAt,omitempty"`
	// ExitCode is the directly launched application's exit code, when available.
	ExitCode *int32 `json:"exitCode,omitempty"`
	// RuntimeLogs describes capture owned by the immediate physical execution.
	RuntimeLogs *LogSource `json:"runtimeLogs,omitempty"`
	// SourceDirectory is the Project source directory in the execution context: rootDirectory for processes,
	// source.containerDirectory for containers, and absent when a container declares no source.
	SourceDirectory string `json:"sourceDirectory,omitempty"`
	// Conditions describe readiness and reconciliation progress.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ProjectObject is implemented by Project kinds that run user code and serve debug sessions.
// +k8s:openapi-gen=false
// +kubebuilder:object:generate=false
type ProjectObject interface {
	apiserver_resource.Object
	metav1.Object
	NamespacedName() types.NamespacedName
	GetResourceId() string
	// GetProjectSpec returns the shared spec fields.
	GetProjectSpec() *ProjectCommonSpec
	// GetProjectStatus returns the shared status fields for in-place updates.
	GetProjectStatus() *ProjectCommonStatus
	// GetDebugAdapter returns the execution-independent adapter configuration, or nil when none is declared.
	GetDebugAdapter() *DebugAdapterConfig
	// GetDebugAdapterArgs returns the adapter arguments with directory references resolved for the execution context.
	GetDebugAdapterArgs() []string
	// GetSourceDirectory returns the source directory in the execution context, or empty when none is declared.
	GetSourceDirectory() string
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

// validateProject validates metadata and shared spec fields common to Project kinds.
func validateProject(resource ProjectObject, specPath *field.Path) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(resource)
	if commonapi.ResourceCreationProhibited.Load() && resource.GetDeletionTimestamp().IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}
	errorList = append(errorList, commonapi.ValidateAnnotationsSize(resource.GetAnnotations(), field.NewPath("metadata", "annotations"))...)
	root := resource.GetProjectSpec().RootDirectory
	if !filepath.IsAbs(root) || strings.ContainsRune(root, '\x00') {
		errorList = append(errorList, field.Invalid(specPath.Child("rootDirectory"), root, "rootDirectory must be an absolute host directory path"))
	}
	switch resource.GetProjectSpec().EffectiveLaunchMode() {
	case ProjectLaunchModeAdapter, ProjectLaunchModeDirect:
	default:
		errorList = append(errorList, field.NotSupported(specPath.Child("launchMode"), resource.GetProjectSpec().LaunchMode,
			[]ProjectLaunchMode{ProjectLaunchModeAdapter, ProjectLaunchModeDirect}))
	}
	return errorList
}

// validateProjectUpdate permits one-way start and stop requests on copies of the old and new specs.
func validateProjectUpdate[S any](oldSpec, newSpec S, commonSpec func(*S) *ProjectCommonSpec) field.ErrorList {
	errorList := field.ErrorList{}
	oldCommon, newCommon := commonSpec(&oldSpec), commonSpec(&newSpec)
	if oldCommon.Stop && !newCommon.Stop {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "stop"), "stop cannot be set to false once it has been set to true"))
	}
	if oldCommon.ShouldStart() && !newCommon.ShouldStart() {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "start"), "start cannot be set to false once it is true"))
	}
	oldCommon.Stop, newCommon.Stop = false, false
	oldCommon.Start, newCommon.Start = nil, nil
	if !reflect.DeepEqual(oldSpec, newSpec) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec"), "spec is immutable"))
	}
	return errorList
}

var _ apiserver_resource.GenericSubResource = (*ProjectLogResource)(nil)
