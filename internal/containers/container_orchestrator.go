/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os/exec"
	"time"

	"github.com/microsoft/dcp/internal/pubsub"
	"github.com/microsoft/dcp/internal/termpty"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
)

// EnvVar is a name/value environment variable pair.
type EnvVar = commonapi.EnvVar

// Label is a key/value label to apply to a container or image.
type Label = commonapi.Label

type ContainerStatus string

// Reference: https://github.com/moby/moby/blob/master/api/swagger.yaml
// (search for 'ContainerState' object definition, Status property)
const (
	ContainerStatusCreated    ContainerStatus = "created"
	ContainerStatusRunning    ContainerStatus = "running"
	ContainerStatusPaused     ContainerStatus = "paused"
	ContainerStatusRestarting ContainerStatus = "restarting"
	ContainerStatusRemoving   ContainerStatus = "removing"
	ContainerStatusExited     ContainerStatus = "exited"
	ContainerStatusDead       ContainerStatus = "dead"
)

type ContainerRuntimeStatus struct {
	Installed bool
	Running   bool
	Error     string
}

func (crs ContainerRuntimeStatus) IsHealthy() bool {
	return crs.Installed && crs.Running
}

type LabelFilter struct {
	// Key of the label to filter by
	Key string
	// Value of the label to filter by
	Value string
}

type InspectedContainerPortMapping map[string][]InspectedContainerHostPortConfig

type InspectedContainerHostPortConfig struct {
	HostIp   string `json:"HostIp,omitempty"`
	HostPort string `json:"HostPort,omitempty"`
}

// Common options for commands that support streamed output
type StreamCommandOptions struct {
	// Stream to write stdout to
	StdOutStream io.WriteCloser

	// Stream to write stderr to
	StdErrStream io.WriteCloser
}

type TimeoutOption struct {
	Timeout time.Duration
}

// CLICommandRunner abstracts the ability to create and run container runtime CLI commands.
// Both Docker and Podman orchestrators implement this interface, allowing shared logic
// (such as image layer application) to invoke runtime-specific CLI commands.
type CLICommandRunner interface {
	// MakeCommand creates an exec.Cmd for the container runtime with the given arguments.
	MakeCommand(args ...string) *exec.Cmd

	// RunBufferedCommand runs a command, capturing stdout and stderr into buffers.
	// If the command does not complete within the timeout, it is terminated.
	RunBufferedCommand(ctx context.Context, opName string, cmd *exec.Cmd, stdout io.WriteCloser, stderr io.WriteCloser, timeout time.Duration) (*bytes.Buffer, *bytes.Buffer, error)
}

type ContainerDiagnostics struct {
	// Container runtime client version
	ClientVersion string `json:"clientVersion,omitempty"`

	// Container runtime server version
	ServerVersion string `json:"serverVersion,omitempty"`
}

type GetDiagnostics interface {
	GetDiagnostics(ctx context.Context) (ContainerDiagnostics, error)
}

type ListContainersFilters struct {
	LabelFilters []LabelFilter
}

type ListContainersOptions struct {
	Filters ListContainersFilters
}

type ListedContainer struct {
	// ID of the container
	Id string `json:"Id"`

	// Name of the container
	Name string `json:"Name,omitempty"`

	// Container image name or ID
	Image string `json:"Image,omitempty"`

	// Status of the container
	Status ContainerStatus `json:"State,omitempty"`

	// Labels applied to the container
	Labels map[string]string `json:"Labels,omitempty"`

	// Connected network names or IDs
	Networks []string `json:"Networks,omitempty"`
}

type ListContainers interface {
	ListContainers(ctx context.Context, options ListContainersOptions) ([]ListedContainer, error)
}

// InspectContainers command types

type InspectedContainer struct {
	// ID of the container
	Id string `json:"Id"`

	// Name of the container
	Name string `json:"Name,omitempty"`

	// Image reference that was used to create the container.
	Image string `json:"Image,omitempty"`

	// Container creation timestamp
	CreatedAt time.Time `json:"CreatedAt,omitempty"`

	// Container start timestamp
	StartedAt time.Time `json:"StartedAt,omitempty"`

	// Container finish timestamp (the timestamp of last exit/death)
	FinishedAt time.Time `json:"FinishedAt,omitempty"`

	// Container status
	Status ContainerStatus `json:"Status,omitempty"`

	// Error message (if any) that was reported when the container exited
	Error string `json:"Error,omitempty"`

	// Exit code
	ExitCode int32 `json:"ExitCode,omitempty"`

	// The command that is configured to health check the container (if any)
	Healthcheck []string `json:"Healthcheck,omitempty"`

	// The status of any container health checks
	Health *InspectedContainerHealth `json:"Health,omitempty"`

	// Environment variables
	Env map[string]string `json:"Env,omitempty"`

	// Launch arguments
	Args []string `json:"Args,omitempty"`

	// Container volume/bind mounts
	Mounts []VolumeMount `json:"Mounts,omitempty"`

	// Container ports
	Ports InspectedContainerPortMapping `json:"Ports,omitempty"`

	// Container networks
	Networks []InspectedContainerNetwork `json:"Networks,omitempty"`

	// Container labels
	Labels map[string]string `json:"Labels,omitempty"`
}

// Results of container health check
type InspectedContainerHealth struct {
	// Status of the container health check
	Status string `json:"Status,omitempty"`

	// How many times the health check has failed
	FailingStreak int32 `json:"FailingStreak,omitempty"`

	// Log of health check results
	Log []InspectedContainerHealthLog `json:"Log,omitempty"`
}

// Configuration for the container health check
type InspectedContainerHealthcheck struct {
	// The command to run for the health check
	Test []string `json:"Test,omitempty"`
}

type InspectedContainerHealthLog struct {
	// The start time of the health check
	Start time.Time `json:"Start,omitempty"`
	// The time the health check completed
	End time.Time `json:"End,omitempty"`
	// The exit code of the health check
	Exit int32 `json:"Exit,omitempty"`
	// The output of the health check command
	Output string `json:"Output,omitempty"`
}

type InspectedContainerNetwork struct {
	// ID of the network
	Id string `json:"NetworkID"`

	// Name of the network
	Name string `json:"Name"`

	// IP address of the container on this network
	IPAddress string `json:"IPAddress,omitempty"`

	// MAC address of the container on this network
	MacAddress string `json:"MacAddress,omitempty"`

	// Gateway for the container on this network
	Gateway string `json:"Gateway,omitempty"`

	// Aliases of the container on this network
	Aliases []string `json:"Aliases,omitempty"`
}

type InspectContainersOptions struct {
	// List of container IDs or names to inspect
	Containers []string
}

type InspectContainers interface {
	// Inspects containers identified by given list of IDs or names.
	InspectContainers(ctx context.Context, options InspectContainersOptions) ([]InspectedContainer, error)
}

// StopContainers command types

type StopContainersOptions struct {
	// The list of containers to stop (by name or ID)
	Containers []string

	// How many seconds to wait for the container to gracefully exit before killing it
	SecondsToKill uint
}

type StopContainers interface {
	// Stops containers identified by given list of IDs or names.
	// Returns list of stopped containers. If some containers are not found, an error will be reported,
	// but containers that were found will be stopped (this is NOT an all-or-noting operation).
	StopContainers(ctx context.Context, options StopContainersOptions) ([]string, error)
}

// RemoveContainers command types

type RemoveContainersOptions struct {
	// The list of containers to remove (by name or ID)
	Containers []string

	// If true, the containers will be removed even if they are running
	Force bool
}

type RemoveContainers interface {
	// Removes containers identified by given list of IDs or names.
	// Returns list of removed containers. If some containers are not found, an error will be reported,
	// but containers that were found will be removed (this is NOT an all-or-noting operation).
	RemoveContainers(ctx context.Context, options RemoveContainersOptions) ([]string, error)
}

// CreateContainer command types

type CreateContainerPort struct {
	HostPort      int32
	ContainerPort int32
	Protocol      string
	HostIP        string
}

type VolumeMountType string

const (
	BindMount        VolumeMountType = "bind"
	NamedVolumeMount VolumeMountType = "volume"
)

// VolumeMount describes a file system to make available inside a container.
type VolumeMount struct {
	Type VolumeMountType `json:"type"`

	// Bind mounts: the host directory to mount.
	// Volume mounts: name of the volume to mount.
	Source string `json:"source"`

	// The path within the container that the mount will use.
	Target string `json:"target"`

	// True if the mounted file system is supposed to be read-only.
	ReadOnly bool `json:"readOnly,omitempty"`
}

type ImagePullPolicy string

const (
	// Always pull the container image.
	PullPolicyAlways ImagePullPolicy = "always"

	// Pull the container image only if it is not present.
	PullPolicyMissing ImagePullPolicy = "missing"

	// Never pull the container image.
	PullPolicyNever ImagePullPolicy = "never"
)

type ContainerRestartPolicy string

const (
	RestartPolicyNone          ContainerRestartPolicy = "no"
	RestartPolicyOnFailure     ContainerRestartPolicy = "on-failure"
	RestartPolicyUnlessStopped ContainerRestartPolicy = "unless-stopped"
	RestartPolicyAlways        ContainerRestartPolicy = "always"
)

type CreateContainerVolumeMount struct {
	Type     VolumeMountType
	Source   string
	Target   string
	ReadOnly bool
}

type CreateContainerOptions struct {
	// Name of the container. If empty, the container orchestrator will provide a default name for the new container.
	Name string

	// Image is the image used to create the container.
	Image string

	// Entrypoint is the container runtime entrypoint to run.
	Entrypoint string

	// Command is the command arguments passed to the container entrypoint.
	Command []string

	// Env contains environment variables to set in the container.
	Env []EnvVar

	// EnvFiles contains files used to populate the container environment.
	EnvFiles []string

	// Ports describes ports to expose from the container.
	Ports []CreateContainerPort

	// VolumeMounts describes volume and bind mounts for the container.
	VolumeMounts []CreateContainerVolumeMount

	// Labels contains labels to apply to the container.
	Labels []Label

	// RestartPolicy is the container runtime restart policy.
	RestartPolicy ContainerRestartPolicy

	// PullPolicy controls how the runtime pulls the container image.
	PullPolicy ImagePullPolicy

	// RunArgs are raw additional runtime arguments passed before the image name.
	RunArgs []string

	// AttachTerminal allocates a TTY and keeps standard input open for later attach operations.
	AttachTerminal bool

	// Networks to connect to _at creation time_, with optional per-network aliases.
	// If not set, the container will be connected to the default network.
	Networks []CreateContainerNetworkOptions

	// Healthcheck configuration for the container
	// This is currently only used for testing purposes
	Healthcheck ContainerHealthcheck

	StreamCommandOptions
	TimeoutOption
}

type CreateContainerNetworkOptions struct {
	// Name or ID of a network to connect to _at creation time_.
	Name string

	// Network aliases to use for the container on this network _at creation time_.
	Aliases []string
}

type ContainerHealthcheck struct {
	// The command to run for the health check
	Command []string

	// The interval between health checks
	Interval time.Duration

	// The maximum time to wait for the health check to complete
	Timeout time.Duration

	// The number of failures before the container is considered unhealthy
	Retries int32

	// The duration after the container starts before failures count against health check retry failures
	StartPeriod time.Duration

	// The interval between health checks during the start period
	StartInterval time.Duration
}

type CreateContainer interface {
	// Create (but do not start) a container. If successful, the ID of the container is returned.
	CreateContainer(ctx context.Context, options CreateContainerOptions) (string, error)
}

// StartContainers command types

type StartContainersOptions struct {
	// The list of containers to start (by name or ID)
	Containers []string

	StreamCommandOptions
}

type StartContainers interface {
	// Start one or more stopped containers. Returns list of started containers.
	StartContainers(ctx context.Context, options StartContainersOptions) ([]string, error)
}

// RunContainer command types

type RunContainerOptions struct {
	CreateContainerOptions
}

type RunContainer interface {
	// Starts the container. If successful, the ID of the container is returned.
	RunContainer(ctx context.Context, options RunContainerOptions) (string, error)
}

// ExecContainer command types

type ExecContainerOptions struct {
	// The container (name/id) to execute the command in
	Container string

	// The working directory for the command
	WorkingDirectory string

	// The environment variables to set
	Env []EnvVar

	// Environment files to use to populate the environment for the command
	EnvFiles []string

	// The command to run
	Command string

	// The arguments to pass to the command
	Args []string

	StreamCommandOptions
}

type ExecContainers interface {
	// Executes a command in a running container. Returns a channel that will emit the final exit code of running the command.
	ExecContainer(ctx context.Context, options ExecContainerOptions) (<-chan int32, error)
}

// AttachContainer command types

// AttachContainerOptions parameterizes a call to AttachContainer.
type AttachContainerOptions struct {
	// The container (name/id) to attach to.
	Container string

	// Initial PTY dimensions for the attach session. A zero value lets the
	// orchestrator (or the underlying terminal layer) pick a sensible default.
	Cols uint16
	Rows uint16
}

// AttachContainer attaches a pseudo-terminal to a running container's
// stdin/stdout/stderr. The container must have been created with AttachTerminal
// set so the runtime allocates a TTY and keeps standard input open.
//
// Implementations typically spawn the runtime's "attach" subcommand
// (docker attach / podman attach) on a freshly allocated PTY. The returned
// PseudoTerminalProcess's PTY master end is bridged to the container's stdio.
// The caller owns the lifetime of the returned PTY (must Close it) and is
// responsible for tearing down any ConnManager built on top.
type AttachContainer interface {
	AttachContainer(ctx context.Context, options AttachContainerOptions) (*termpty.PseudoTerminalProcess, error)
}

// CreateFiles command types

type FileSystemEntryType string

const (
	FileSystemEntryTypeFile    FileSystemEntryType = "file"    // default
	FileSystemEntryTypeOpenSSL FileSystemEntryType = "openssl" // special type for OpenSSL certificates
	FileSystemEntryTypeDir     FileSystemEntryType = "directory"
	// The public CreateFiles API validation doesn't allow specifying "symlink" as a FileSystemEntry
	// type, but the internal ContainerOrchestrator.CreateFiles library does support it.
	FileSystemEntryTypeSymlink FileSystemEntryType = "symlink"
)

// FileSystemEntry represents part of the file structure to be created in the container.
type FileSystemEntry struct {
	// The type of entry (file, symlink, or directory).
	Type FileSystemEntryType `json:"type,omitempty"`

	// The name of the entry (required).
	Name string `json:"name"`

	// The UID of the file owner. Defaults to 0 (root).
	Owner *int32 `json:"owner,omitempty"`

	// The ID of the file group. Defaults to 0 (root).
	Group *int32 `json:"group,omitempty"`

	// The unix mode permissions of this entry. If Mode is 0, the umask for the create file request will be applied.
	Mode fs.FileMode `json:"mode,omitempty"`

	// For file type entries, an optional path to a source file to copy. It is an error to set both Source and Contents.
	Source string `json:"source,omitempty"`

	// For symlink type entries, the target of the symlink. The target must be a valid path in the container
	// (existing or created as part of this create files set), either absolute or relative to the new symlink.
	Target string `json:"target,omitempty"`

	// For file type entries, the string contents of the file. Optional.
	Contents string `json:"contents,omitempty"`

	// For file type entries, the Base64 encoded byte contents of the file. Optional.
	RawContents string `json:"rawContents,omitempty"`

	// For file type entries, if true, errors creating this file will be logged but will not fail the overall operation.
	ContinueOnError bool `json:"continueOnError,omitempty"`

	// For directory type entries, the child entries (files or directories). Optional.
	Entries []FileSystemEntry `json:"entries,omitempty"`
}

type CreateFilesOptions struct {
	// The container (name/id) to copy the file to
	Container string

	// Time the file was modified/created
	ModTime time.Time

	// The base path in the container under which the files and folders will be created
	Destination string

	// The default owner ID for created files (defaults to 0 for root)
	DefaultOwner int32

	// The default group ID for created files (defaults to 0 for root)
	DefaultGroup int32

	// The umask for created files and folders without explicit permissions set (defaults to 022)
	Umask fs.FileMode

	// The specific entries to create in the container (must have at least one item)
	Entries []FileSystemEntry
}

type CreateFiles interface {
	// Create files/folders in the container based on the provided structure
	CreateFiles(ctx context.Context, options CreateFilesOptions) error
}

// ApplyImageLayers command types

type ApplyImageLayersOptions struct {
	// The inspected base image to apply layers on top of
	BaseImage InspectedImage

	// The image layers to apply (tar files)
	Layers []ImageLayer

	// Tag to apply to the derived image
	Tag string

	TimeoutOption
}

type LogStreamSource string

const (
	LogStreamSourceStdout LogStreamSource = "stdout"
	LogStreamSourceStderr LogStreamSource = "stderr"
)

type ApplyImageLayers interface {
	// Builds a derived image by applying additional tar layers on top of a base image.
	// Returns the tag/ID of the derived image.
	ApplyImageLayers(ctx context.Context, options ApplyImageLayersOptions) (string, error)
}

type StreamContainerLogsOptions struct {
	// Follow the logs vs. just returning the current logs at the time the command was run
	Follow bool

	// Request the container orchestrator to add timestamps to the log entries
	Timestamps bool
}

type ContainerLogSource interface {
	// Starts capturing container logs to the provided writers
	CaptureContainerLogs(ctx context.Context, container string, stdout usvc_io.WriteSyncerCloser, stderr usvc_io.WriteSyncerCloser, options StreamContainerLogsOptions) error
}

type CachedRuntimeStatusUsage string

const CachedRuntimeStatusAllowed CachedRuntimeStatusUsage = "cachedResultAllowed"
const IgnoreCachedRuntimeStatus CachedRuntimeStatusUsage = "ignoreCachedResult"

type RuntimeStatusChecker interface {
	// Check the runtime status
	CheckStatus(ctx context.Context, cacheUsage CachedRuntimeStatusUsage) ContainerRuntimeStatus
}

// Represents portion of container orchestrator functionality that is related to container management
type ContainerOrchestrator interface {
	// Is this the default orchestrator?
	IsDefault() bool

	// Get the name of the runtime
	Name() string

	// Get the container machine host name for the runtime
	ContainerHost() string

	// Start running background checks for the runtime status
	EnsureBackgroundStatusUpdates(ctx context.Context)

	// Get container runtime diagnostic information
	GetDiagnostics

	CreateContainer
	StartContainers
	RunContainer
	ListContainers
	InspectContainers
	StopContainers
	RemoveContainers
	ExecContainers
	AttachContainer
	CreateFiles
	ApplyImageLayers

	// Subscribes to events about container state changes
	// When the subscription is cancelled, the channel will be closed
	WatchContainers(sink chan<- EventMessage) (*pubsub.Subscription[EventMessage], error)

	ContainerLogSource
	VolumeOrchestrator
	ImageOrchestrator
	NetworkOrchestrator
	RuntimeStatusChecker
}

// Types of events reported for containers
// See https://github.com/moby/moby/blob/master/api/swagger.yaml, search for "Containers report these events"
const (
	EventActionAttach       EventAction = "attach"
	EventActionCommit       EventAction = "commit"
	EventActionCopy         EventAction = "copy"
	EventActionCreate       EventAction = "create"
	EventActionDestroy      EventAction = "destroy"
	EventActionDetach       EventAction = "detach"
	EventActionDie          EventAction = "die"
	EventActionDied         EventAction = "died" // Podman-specific - doesn't adhere to the standard event types
	EventActionExecCreate   EventAction = "exec_create"
	EventActionExecDetach   EventAction = "exec_detach"
	EventActionExecStart    EventAction = "exec_start"
	EventActionExecDie      EventAction = "exec_die"
	EventActionExport       EventAction = "export"
	EventActionHealthStatus EventAction = "health_status"
	EventActionKill         EventAction = "kill"
	EventActionOom          EventAction = "oom"
	EventActionPause        EventAction = "pause"
	EventActionRename       EventAction = "rename"
	EventActionResize       EventAction = "resize"
	EventActionRestart      EventAction = "restart"
	EventActionStart        EventAction = "start"
	EventActionStop         EventAction = "stop"
	EventActionTop          EventAction = "top"
	EventActionUnpause      EventAction = "unpause"
	EventActionUpdate       EventAction = "update"
	EventActionPrune        EventAction = "prune"
	EventActionConnect      EventAction = "connect"
	EventActionDisconnect   EventAction = "disconnect"
)
