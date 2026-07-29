/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"encoding/gob"
	"io/fs"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// VolumeMountType is the legacy V1 volume mount type used for lifecycle hash compatibility.
type VolumeMountType string

const (
	// BindMount is the legacy V1 bind mount type used for lifecycle hash compatibility.
	BindMount VolumeMountType = "bind"

	// NamedVolumeMount is the legacy V1 named volume mount type used for lifecycle hash compatibility.
	NamedVolumeMount VolumeMountType = "volume"
)

// VolumeMount is the legacy V1 volume mount shape used for lifecycle hash compatibility.
type VolumeMount struct {
	Type     VolumeMountType `json:"type"`
	Source   string          `json:"source"`
	Target   string          `json:"target"`
	ReadOnly bool            `json:"readOnly,omitempty"`
}

// PortProtocol is the legacy V1 port protocol type used for lifecycle hash compatibility.
type PortProtocol string

const (
	// TCP is the legacy V1 TCP protocol value used for lifecycle hash compatibility.
	TCP PortProtocol = "TCP"

	// UDP is the legacy V1 UDP protocol value used for lifecycle hash compatibility.
	UDP PortProtocol = "UDP"
)

// ContainerPort is the legacy V1 container port shape used for lifecycle hash compatibility.
type ContainerPort struct {
	HostPort      int32        `json:"hostPort,omitempty"`
	ContainerPort int32        `json:"containerPort"`
	Protocol      PortProtocol `json:"protocol,omitempty"`
	HostIP        string       `json:"hostIP,omitempty"`
}

type containerPortRangeLifecycleHash struct {
	HostPort         int32        `json:"hostPort,omitempty"`
	ContainerPort    int32        `json:"containerPort"`
	ContainerPortEnd int32        `json:"containerPortEnd,omitempty"`
	Protocol         PortProtocol `json:"protocol,omitempty"`
	HostIP           string       `json:"hostIP,omitempty"`
}

// BuildSecretType is the legacy V1 build secret type used for lifecycle hash compatibility.
type BuildSecretType string

const (
	// EnvSecret is the legacy V1 env secret type used for lifecycle hash compatibility.
	EnvSecret BuildSecretType = "env"

	// FileSecret is the legacy V1 file secret type used for lifecycle hash compatibility.
	FileSecret BuildSecretType = "file"
)

// ContainerBuildSecret is the legacy V1 build secret shape used for lifecycle hash compatibility.
type ContainerBuildSecret struct {
	Type   BuildSecretType `json:"type,omitempty"`
	ID     string          `json:"id"`
	Source string          `json:"source,omitempty"`
	Value  string          `json:"value,omitempty"`
}

// ContainerLabel is the legacy V1 container label shape used for lifecycle hash compatibility.
type ContainerLabel struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// EnvVar is the legacy V1 environment variable shape used for lifecycle hash compatibility.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// FileSystemEntryType is the legacy V1 file entry type used for lifecycle hash compatibility.
type FileSystemEntryType string

// FileSystemEntry is the legacy V1 file entry shape used for lifecycle hash compatibility.
type FileSystemEntry struct {
	Type            FileSystemEntryType `json:"type,omitempty"`
	Name            string              `json:"name"`
	Owner           *int32              `json:"owner,omitempty"`
	Group           *int32              `json:"group,omitempty"`
	Mode            fs.FileMode         `json:"mode,omitempty"`
	Source          string              `json:"source,omitempty"`
	Target          string              `json:"target,omitempty"`
	Contents        string              `json:"contents,omitempty"`
	RawContents     string              `json:"rawContents,omitempty"`
	ContinueOnError bool                `json:"continueOnError,omitempty"`
	Entries         []FileSystemEntry   `json:"entries,omitempty"`
}

// CreateFileSystem is the legacy V1 create-files shape used for lifecycle hash compatibility.
type CreateFileSystem struct {
	Destination  string            `json:"destination,omitempty"`
	DefaultOwner int32             `json:"defaultOwner,omitempty"`
	DefaultGroup int32             `json:"defaultGroup,omitempty"`
	Umask        *fs.FileMode      `json:"umask,omitempty"`
	Entries      []FileSystemEntry `json:"entries,omitempty"`
}

// ImageLayer is the legacy V1 image layer shape used for lifecycle hash compatibility.
type ImageLayer struct {
	Digest      string `json:"digest"`
	Source      string `json:"source,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	RawContents string `json:"rawContents,omitempty"`
}

func encodeContainerPortLifecycleHash(encoder *gob.Encoder, port commonapi.ContainerPort) error {
	if port.ContainerPortEnd == 0 {
		return encoder.Encode(ContainerPort{
			HostPort:      port.HostPort,
			ContainerPort: port.ContainerPort,
			Protocol:      PortProtocol(port.Protocol),
			HostIP:        port.HostIP,
		})
	}

	return encoder.Encode(containerPortRangeLifecycleHash{
		HostPort:         port.HostPort,
		ContainerPort:    port.ContainerPort,
		ContainerPortEnd: port.ContainerPortEnd,
		Protocol:         PortProtocol(port.Protocol),
		HostIP:           port.HostIP,
	})
}

func lifecycleHashContainerLabel(label commonapi.Label) ContainerLabel {
	return ContainerLabel{
		Key:   label.Key,
		Value: label.Value,
	}
}

func lifecycleHashContainerBuildSecret(secret commonapi.ContainerBuildSecret) ContainerBuildSecret {
	return ContainerBuildSecret{
		Type:   BuildSecretType(secret.Type),
		ID:     secret.ID,
		Source: secret.Source,
		Value:  secret.Value,
	}
}

func lifecycleHashVolumeMount(mount commonapi.VolumeMount) VolumeMount {
	return VolumeMount{
		Type:     VolumeMountType(mount.Type),
		Source:   mount.Source,
		Target:   mount.Target,
		ReadOnly: mount.ReadOnly,
	}
}

func lifecycleHashEnvVar(env commonapi.EnvVar) EnvVar {
	return EnvVar{
		Name:  env.Name,
		Value: env.Value,
	}
}

func lifecycleHashCreateFileSystem(createFile commonapi.CreateFileSystem) CreateFileSystem {
	return CreateFileSystem{
		Destination:  createFile.Destination,
		DefaultOwner: createFile.DefaultOwner,
		DefaultGroup: createFile.DefaultGroup,
		Umask:        createFile.Umask,
		Entries:      lifecycleHashFileSystemEntries(createFile.Entries),
	}
}

func lifecycleHashFileSystemEntries(entries []commonapi.FileSystemEntry) []FileSystemEntry {
	if len(entries) == 0 {
		return nil
	}

	legacyEntries := make([]FileSystemEntry, len(entries))
	for i, entry := range entries {
		legacyEntries[i] = FileSystemEntry{
			Type:            FileSystemEntryType(entry.Type),
			Name:            entry.Name,
			Owner:           entry.Owner,
			Group:           entry.Group,
			Mode:            entry.Mode,
			Source:          entry.Source,
			Target:          entry.Target,
			Contents:        entry.Contents,
			RawContents:     entry.RawContents,
			ContinueOnError: entry.ContinueOnError,
			Entries:         lifecycleHashFileSystemEntries(entry.Entries),
		}
	}
	return legacyEntries
}
