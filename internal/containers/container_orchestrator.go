package containers

import (
	"context"
	"io"
	"io/fs"
	"time"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/pubsub"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
)

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
	Mounts []apiv1.VolumeMount `json:"Mounts,omitempty"`

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

type CreateContainerOptions struct {
	// Name of the container
	Name string

	// Name or ID of a network to connect to
	Network string

	// Healthcheck configuration for the container
	// This is currently only used for testing purposes
	Healthcheck ContainerHealthcheck

	StreamCommandOptions
	TimeoutOption

	apiv1.ContainerSpec
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
	Env []apiv1.EnvVar

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

// CreateFiles command types

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
	Entries []apiv1.FileSystemEntry
}

type CreateFiles interface {
	// Create files/folders in the container based on the provided structure
	CreateFiles(ctx context.Context, options CreateFilesOptions) error
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
	CreateFiles

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
