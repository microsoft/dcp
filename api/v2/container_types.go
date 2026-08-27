/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"fmt"
	"io/fs"
	"path"
	"slices"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// The container shapes below are owned by v2 rather than shared with v1. v1 derives container
// lifecycle keys from gob encodings of its own types, so a shared type could not evolve without
// orphaning containers created by an earlier DCP version. Only trivial, cross-cutting value
// types (commonapi.EnvVar, commonapi.Label, commonapi.PortProtocol) are shared.

type VolumeMountType string

const (
	// A volume mount to a host directory.
	BindMount VolumeMountType = "bind"

	// A volume mount to a named volume managed by an orchestrator.
	NamedVolumeMount VolumeMountType = "volume"
)

// VolumeMount describes a file system to make available inside a container.
// +k8s:openapi-gen=true
type VolumeMount struct {
	Type VolumeMountType `json:"type"`

	// Bind mounts: the host directory to mount.
	// Volume mounts: name of the volume to mount.
	Source string `json:"source"`

	// The path within the container that the mount will use.
	Target string `json:"target"`

	// True if the mounted file system is supposed to be read-only.
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// ContainerPort describes a port, or contiguous range of ports, to publish from a container.
// +k8s:openapi-gen=true
type ContainerPort struct {
	// Optional: If specified, this must be a valid port number, 0 < x < 65536.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	// +optional
	HostPort int32 `json:"hostPort,omitempty"`

	// Required: This must be a valid port number, 0 < x < 65536.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	ContainerPort int32 `json:"containerPort"`

	// Number of consecutive ports to publish, starting at ContainerPort and HostPort (if specified).
	// Defaults to 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default:=1
	// +optional
	RangeSize int32 `json:"rangeSize,omitempty"`

	// The protocol to be used, defaults to TCP.
	// +optional
	Protocol commonapi.PortProtocol `json:"protocol,omitempty"`

	// Optional: What host IP to bind the external port to.
	// +optional
	HostIP string `json:"hostIP,omitempty"`
}

// EffectiveRangeSize returns the number of consecutive ports to publish.
func (cp *ContainerPort) EffectiveRangeSize() int32 {
	if cp.RangeSize > 0 {
		return cp.RangeSize
	}

	return 1
}

func ValidateContainerPorts(ports []ContainerPort, portsPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}

	for i, port := range ports {
		portPath := portsPath.Index(i)

		if port.ContainerPort <= 0 || port.ContainerPort > 65535 {
			errorList = append(errorList, field.Invalid(portPath.Child("containerPort"), port.ContainerPort, "containerPort must be between 1 and 65535"))
		}

		if port.RangeSize < 0 || port.RangeSize > 65535 {
			errorList = append(errorList, field.Invalid(portPath.Child("rangeSize"), port.RangeSize, "rangeSize must be between 1 and 65535 when specified"))
		}

		if port.HostPort < 0 || port.HostPort > 65535 {
			errorList = append(errorList, field.Invalid(portPath.Child("hostPort"), port.HostPort, "hostPort must be between 0 and 65535"))
		}

		if port.Protocol != "" && port.Protocol != commonapi.PortProtocolTCP && port.Protocol != commonapi.PortProtocolUDP {
			errorList = append(errorList, field.NotSupported(portPath.Child("protocol"), port.Protocol, []string{string(commonapi.PortProtocolTCP), string(commonapi.PortProtocolUDP)}))
		}

		rangeSize := int64(port.EffectiveRangeSize())
		containerPortEnd := int64(port.ContainerPort) + rangeSize - 1
		if containerPortEnd > 65535 {
			errorList = append(errorList, field.Invalid(portPath.Child("rangeSize"), port.RangeSize, "container port range must fit within 1 and 65535"))
		}

		if port.HostPort != 0 {
			hostPortEnd := int64(port.HostPort) + rangeSize - 1
			if hostPortEnd > 65535 {
				errorList = append(errorList, field.Invalid(portPath.Child("rangeSize"), port.RangeSize, "host port range must fit within 1 and 65535"))
			}
		}
	}

	return errorList
}

// ContainerNetworkConnectionConfig describes a network to attach to when creating a container.
// +k8s:openapi-gen=true
type ContainerNetworkConnectionConfig struct {
	// Name of the network to connect to.
	Name string `json:"name"`

	// Aliases of the container on the network.
	// +listType=set
	// +optional
	Aliases []string `json:"aliases,omitempty"`
}

func (cncc *ContainerNetworkConnectionConfig) Equal(other *ContainerNetworkConnectionConfig) bool {
	if cncc == other {
		return true
	}

	if cncc == nil || other == nil {
		return false
	}

	if cncc.Name != other.Name {
		return false
	}

	return slices.Equal(cncc.Aliases, other.Aliases)
}

type BuildSecretType string

const (
	EnvSecret  BuildSecretType = "env"
	FileSecret BuildSecretType = "file"
)

// ContainerBuildSecret is a secret made available to the image builder.
// +k8s:openapi-gen=true
type ContainerBuildSecret struct {
	// The type of secret (defaults to file).
	// +optional
	Type BuildSecretType `json:"type,omitempty"`

	// The ID of the secret.
	ID string `json:"id"`

	// For file secrets, the source filepath of the secret; for env secrets, the environment
	// variable name. Required for file secrets, optional for env secrets (defaults to the ID).
	// +optional
	Source string `json:"source,omitempty"`

	// Only used for env secrets. If set, this value is applied via the configured environment
	// variable to the build command. If unset, the value comes from the ambient environment.
	// +optional
	Value string `json:"value,omitempty"`
}

// ContainerBuildContext describes how to build a container image from source.
// +k8s:openapi-gen=true
type ContainerBuildContext struct {
	// The path to the directory to be used as the root of the build context.
	Context string `json:"context"`

	// The path to a Dockerfile to use for the build.
	// +optional
	Dockerfile string `json:"dockerfile,omitempty"`

	// Additional tags to apply to the image.
	// +listType=set
	// +optional
	Tags []string `json:"tags,omitempty"`

	// Additional --build-arg values to pass to the build command.
	// +listType=atomic
	// +optional
	Args []commonapi.EnvVar `json:"args,omitempty"`

	// Build time secrets to be passed in to the builder via --secret.
	// +listType=atomic
	// +optional
	Secrets []ContainerBuildSecret `json:"secrets,omitempty"`

	// Optional: the name of the build stage to use for the build.
	// +optional
	Stage string `json:"stage,omitempty"`

	// Labels to apply to the built image. When used by PhysicalContainerImage,
	// the physical resource UID label is reserved and set by the controller.
	// +listType=map
	// +listMapKey=key
	// +optional
	Labels []commonapi.Label `json:"labels,omitempty"`

	// Optional target platform for the build (e.g. "linux/amd64").
	// +optional
	Platform string `json:"platform,omitempty"`
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

type FileSystemEntryType string

const (
	FileSystemEntryTypeFile    FileSystemEntryType = "file"    // default
	FileSystemEntryTypeOpenSSL FileSystemEntryType = "openssl" // special type for OpenSSL certificates
	FileSystemEntryTypeDir     FileSystemEntryType = "directory"
)

// FileSystemEntry represents part of the file structure to be created in the container.
// +k8s:openapi-gen=true
type FileSystemEntry struct {
	// The type of entry (file or directory).
	// +optional
	Type FileSystemEntryType `json:"type,omitempty"`

	// The name of the entry (required).
	Name string `json:"name"`

	// The UID of the file owner. Defaults to 0 (root).
	// +optional
	Owner *int32 `json:"owner,omitempty"`

	// The ID of the file group. Defaults to 0 (root).
	// +optional
	Group *int32 `json:"group,omitempty"`

	// The unix mode permissions of this entry. If Mode is 0, the umask for the create file request is applied.
	// +optional
	Mode fs.FileMode `json:"mode,omitempty"`

	// For file type entries, an optional path to a source file to copy.
	// It is an error to set both Source and Contents for a file.
	// +optional
	Source string `json:"source,omitempty"`

	// For file type entries, the string contents of the file. Optional.
	// +optional
	Contents string `json:"contents,omitempty"`

	// For file type entries, the Base64 encoded byte contents of the file. Optional.
	// +optional
	RawContents string `json:"rawContents,omitempty"`

	// For file type entries, if true, errors creating this file are logged but do not fail the operation.
	// +optional
	ContinueOnError bool `json:"continueOnError,omitempty"`

	// For directory type entries, the child entries (files or directories). Optional.
	// +listType=atomic
	// +optional
	Entries []FileSystemEntry `json:"entries,omitempty"`
}

func (fse *FileSystemEntry) GetType() FileSystemEntryType {
	if fse.Type == "" {
		return FileSystemEntryTypeFile
	}

	return fse.Type
}

func (fse *FileSystemEntry) Validate(fieldPath *field.Path) field.ErrorList {
	if fse == nil {
		return nil
	}

	var errorList field.ErrorList

	if fse.Name == "" {
		errorList = append(errorList, field.Required(fieldPath.Child("name"), "name must be set to a non-empty value"))
	}

	if path.Dir(fse.Name) != "." {
		errorList = append(errorList, field.Invalid(fieldPath.Child("name"), fse.Name, "name must not include a path component"))
	}

	if fse.Owner != nil && *fse.Owner < 0 {
		errorList = append(errorList, field.Invalid(fieldPath.Child("owner"), fse.Owner, "owner must be a non-negative integer"))
	}

	if fse.Group != nil && *fse.Group < 0 {
		errorList = append(errorList, field.Invalid(fieldPath.Child("group"), fse.Group, "group must be a non-negative integer"))
	}

	if fse.GetType() != FileSystemEntryTypeFile && fse.GetType() != FileSystemEntryTypeOpenSSL && fse.GetType() != FileSystemEntryTypeDir {
		errorList = append(errorList, field.Invalid(fieldPath.Child("type"), fse.Type, "type must be one of 'file', 'openssl', or 'directory'"))
	}

	if fse.GetType() == FileSystemEntryTypeFile || fse.GetType() == FileSystemEntryTypeOpenSSL {
		if len(fse.Entries) > 0 {
			errorList = append(errorList, field.Forbidden(fieldPath.Child("entries"), fmt.Sprintf("entries cannot be set for %s type entries", fse.GetType())))
		}

		if fse.Source != "" && fse.Contents != "" {
			errorList = append(errorList, field.Forbidden(fieldPath.Child("contents"), "source and contents cannot be set at the same time"))
		}

		if fse.Source != "" && fse.RawContents != "" {
			errorList = append(errorList, field.Forbidden(fieldPath.Child("rawContents"), "source and rawContents cannot be set at the same time"))
		}

		if fse.Contents != "" && fse.RawContents != "" {
			errorList = append(errorList, field.Forbidden(fieldPath.Child("rawContents"), "contents and rawContents cannot be set at the same time"))
		}
	}

	if fse.GetType() == FileSystemEntryTypeDir {
		if fse.Source != "" {
			errorList = append(errorList, field.Forbidden(fieldPath.Child("source"), "source cannot be set for directory type entries"))
		}

		if fse.Contents != "" {
			errorList = append(errorList, field.Forbidden(fieldPath.Child("contents"), "contents cannot be set for directory type entries"))
		}

		if fse.RawContents != "" {
			errorList = append(errorList, field.Forbidden(fieldPath.Child("rawContents"), "rawContents cannot be set for directory type entries"))
		}

		for i, entry := range fse.Entries {
			errorList = append(errorList, entry.Validate(fieldPath.Child("entries").Index(i))...)
		}
	}

	if !fse.Mode.IsRegular() {
		errorList = append(errorList, field.Invalid(fieldPath.Child("mode"), fse.Mode, "mode must not include type bits"))
	}

	return errorList
}

// CreateFileSystem describes a set of files and directories to create inside a container.
// +k8s:openapi-gen=true
type CreateFileSystem struct {
	// The absolute path in the container that the entries are created relative to.
	Destination string `json:"destination"`

	// The default UID applied to entries that do not specify an owner.
	// +optional
	DefaultOwner int32 `json:"defaultOwner,omitempty"`

	// The default GID applied to entries that do not specify a group.
	// +optional
	DefaultGroup int32 `json:"defaultGroup,omitempty"`

	// The umask applied to entries that do not specify a mode.
	// +optional
	Umask *fs.FileMode `json:"umask,omitempty"`

	// The entries to create.
	// +listType=atomic
	Entries []FileSystemEntry `json:"entries,omitempty"`
}
