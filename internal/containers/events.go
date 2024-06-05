package containers

import (
	"fmt"

	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

type EventSource string

const (
	EventSourceBuilder   EventSource = "builder"
	EventSourceConfig    EventSource = "config"
	EventSourceContainer EventSource = "container"
	EventSourceDaemon    EventSource = "daemon"
	EventSourceImage     EventSource = "image"
	EventSourceNetwork   EventSource = "network"
	EventSourceNode      EventSource = "node"
	EventSourcePlugin    EventSource = "plugin"
	EventSourceSecret    EventSource = "secret"
	EventSourceService   EventSource = "service"
	EventSourceVolume    EventSource = "volume"
)

type EventActor struct {
	// The ID of the object that was associated with the event.
	ID string `json:"ID,omitempty"`
}

type EventAction string

// Represents a notification about a change in container orchestrator
type EventMessage struct {
	// The type of the object that caused the event
	Source EventSource `json:"Type"`

	// The action that triggered the event.
	// Different types of objects have different actions associated with them.
	Action EventAction `json:"Action"`

	// Data about the object (container etc) associate with the even
	Actor EventActor `json:"Actor,omitempty"`

	// Key value attributes associated with the event
	Attributes map[string]string `json:"Attributes,omitempty"`
}

func (em *EventMessage) String() string {
	return fmt.Sprintf("{Source: %s, Action: %s, Actor: %v, Attributes: %v}",
		logger.FriendlyString(string(em.Source)),
		logger.FriendlyString(string(em.Action)),
		logger.FriendlyString(em.Actor.ID),
		logger.FriendlyStringMap(em.Attributes),
	)
}
