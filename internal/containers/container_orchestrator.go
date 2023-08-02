package containers

import (
	"context"
	"time"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
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

	// Exit code
	ExitCode int32 `json:"ExitCode,omitempty"`
}

type RunContainerOptions struct {
	// Name of the container
	Name string

	apiv1.ContainerSpec
}

// Represents portion of container orchestrator functionality that is related to container management
type ContainerOrchestrator interface {
	// Starts the container. If successful, the ID of the container is returned.
	RunContainer(ctx context.Context, options RunContainerOptions) (string, error)

	// Inspects containers identified by given list of IDs or names.
	InspectContainers(ctx context.Context, containers []string) ([]InspectedContainer, error)

	// Stops containers identified by given list of IDs or names.
	// Returns list of stopped containers. If some containers are not found, an error will be reported,
	// but containers that were found will be removed (this is NOT and all-or-noting operation).
	//
	// secondsToKill is the time to wait for the container to gracefully exit before killing it (default 10).
	StopContainers(ctx context.Context, containers []string, secondsToKill uint) ([]string, error)

	// Removes containers identified by given list of IDs or names.
	// Returns list of removed containers. If some containers are not found, an error will be reported,
	// but containers that were found will be removed (this is NOT and all-or-noting operation).
	RemoveContainers(ctx context.Context, containers []string, force bool) ([]string, error)

	// Subscribes to events about container state changes
	// When the subscription is cancelled, the channel will be closed
	WatchContainers(ctx context.Context, sink chan<- EventMessage) (EventSubscription, error)
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
)
