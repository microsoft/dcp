/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"context"
	"time"
)

// CreateVolume command types

type CreateVolumeOptions struct {
	// Name of the volume to create
	Name string
}

type CreateVolume interface {
	// Creates a new container volume.
	CreateVolume(ctx context.Context, options CreateVolumeOptions) error
}

// InspectVolumes command types

// Contains information about an existing container volume
type InspectedVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver,omitempty"`
	MountPoint string            `json:"Mountpoint,omitempty"`
	Scope      string            `json:"Scope,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	CreatedAt  time.Time         `json:"CreatedAt,omitempty"`
}

type InspectVolumesOptions struct {
	// The list of volume names to inspect
	Volumes []string
}

type InspectVolumes interface {
	// Inspects volumes identified by given list of names.
	InspectVolumes(ctx context.Context, options InspectVolumesOptions) ([]InspectedVolume, error)
}

// RemoveVolumes command types

type RemoveVolumesOptions struct {
	// The list of volume IDs or names to remove
	Volumes []string

	// Force removal of the volume
	Force bool
}

type RemoveVolumes interface {
	// Removes volumes identified by given list of volume names.
	// Returns list of removed volumes. If some volumes are not found, an error will be reported,
	// but volumes that were found will be removed (this is NOT an all-or-nothing operation).
	RemoveVolumes(ctx context.Context, options RemoveVolumesOptions) ([]string, error)
}

// Represents portion of container orchestrator functionality that is related related to volume management
type VolumeOrchestrator interface {
	CreateVolume
	InspectVolumes
	RemoveVolumes

	RuntimeStatusChecker
}
