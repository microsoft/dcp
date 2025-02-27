package containers

import (
	"context"
	"time"
)

// Contains information about an existing container volume
type InspectedVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver,omitempty"`
	MountPoint string            `json:"Mountpoint,omitempty"`
	Scope      string            `json:"Scope,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	CreatedAt  time.Time         `json:"CreatedAt,omitempty"`
}

// Represents portion of container orchestrator functionality that is related related to volume management
type VolumeOrchestrator interface {
	// Creates a new container volume.
	CreateVolume(ctx context.Context, name string) error

	// Inspects volumes identified by given list of names.
	InspectVolumes(ctx context.Context, volumes []string) ([]InspectedVolume, error)

	// Removes volumes identified by given list of volume names.
	// Returns list of removed volumes. If some volumes are not found, an error will be reported,
	// but volumes that were found will be removed (this is NOT an all-or-nothing operation).
	RemoveVolumes(ctx context.Context, volumes []string, force bool) ([]string, error)

	RuntimeStatusChecker
}
