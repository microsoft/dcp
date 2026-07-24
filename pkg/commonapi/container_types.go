/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commonapi

import (
	"encoding/base64"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/microsoft/dcp/pkg/pointers"
)

var validSHA256HexRegexp = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

type VolumeMountType string

const (
	VolumeMountTypeBind   VolumeMountType = "bind"
	VolumeMountTypeVolume VolumeMountType = "volume"

	BindMount        = VolumeMountTypeBind
	NamedVolumeMount = VolumeMountTypeVolume
)

// +k8s:openapi-gen=true
type VolumeMount struct {
	Type VolumeMountType `json:"type"`

	// Bind mounts: the host directory to mount
	// Volume mounts: name of the volume to mount
	Source string `json:"source"`

	// The path within the container that the mount will use
	Target string `json:"target"`

	// True if the mounted file system is supposed to be read-only
	ReadOnly bool `json:"readOnly,omitempty"`
}

type PortProtocol string

const (
	PortProtocolTCP PortProtocol = "TCP"
	PortProtocolUDP PortProtocol = "UDP"

	TCP = PortProtocolTCP
	UDP = PortProtocolUDP
)

// +k8s:openapi-gen=true
type ContainerPort struct {
	// Optional: If specified, this must be a valid port number, 0 < x < 65536.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	HostPort int32 `json:"hostPort,omitempty"`

	// Required: This must be a valid port number, 0 < x < 65536.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	ContainerPort int32 `json:"containerPort"`

	// Optional end of the inclusive container port range.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	ContainerPortEnd int32 `json:"containerPortEnd,omitempty"`

	// The port to be used, defaults to TCP
	Protocol PortProtocol `json:"protocol,omitempty"`

	// Optional: What host IP to bind the external port to.
	HostIP string `json:"hostIP,omitempty"`
}

func ValidateContainerPorts(ports []ContainerPort, portsPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}
	for i, port := range ports {
		portPath := portsPath.Index(i)
		if port.ContainerPort <= 0 || port.ContainerPort > 65535 {
			errorList = append(errorList, field.Invalid(portPath.Child("containerPort"), port.ContainerPort, "containerPort must be between 1 and 65535"))
		}
		if port.ContainerPortEnd != 0 {
			if port.ContainerPortEnd < port.ContainerPort {
				errorList = append(errorList, field.Invalid(portPath.Child("containerPortEnd"), port.ContainerPortEnd, "containerPortEnd must be greater than or equal to containerPort"))
			}
			if port.ContainerPortEnd > 65535 {
				errorList = append(errorList, field.Invalid(portPath.Child("containerPortEnd"), port.ContainerPortEnd, "containerPortEnd must be between 1 and 65535"))
			}
		}
		if port.HostPort < 0 || port.HostPort > 65535 {
			errorList = append(errorList, field.Invalid(portPath.Child("hostPort"), port.HostPort, "hostPort must be between 0 and 65535"))
		}
		if port.HostPort != 0 && port.ContainerPortEnd != 0 {
			hostPortEnd := int64(port.HostPort) + int64(port.ContainerPortEnd-port.ContainerPort)
			if hostPortEnd > 65535 {
				errorList = append(errorList, field.Invalid(portPath.Child("hostPort"), port.HostPort, "host port range must fit within 1 and 65535"))
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
	// +listType=atomic
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

	if !slices.Equal(cncc.Aliases, other.Aliases) {
		return false
	}

	return true
}

type BuildSecretType string

const (
	BuildSecretTypeEnv  BuildSecretType = "env"
	BuildSecretTypeFile BuildSecretType = "file"

	EnvSecret  = BuildSecretTypeEnv
	FileSecret = BuildSecretTypeFile
)

// +k8s:openapi-gen=true
type ContainerBuildSecret struct {
	// The type of secret (defaults to file)
	Type BuildSecretType `json:"type,omitempty"`

	// The ID of the secret
	ID string `json:"id"`

	// If type is file (or empty), the source filepath of the secret, if type is env, the environment variable name
	// Required for file secrets, optional for env secrets (defaults to the ID)
	Source string `json:"source,omitempty"`

	// Only used for "env" type secrets. If set, this value is applied via the configured environment variable
	// to the build command. If unset, it is assumed the environment secret comes from an ambient environment variables
	Value string `json:"value,omitempty"`
}

// +k8s:openapi-gen=true
type ContainerBuildContext struct {
	// The path to the directory to be used as the root of the build context
	Context string `json:"context"`

	// The path to a Dockerfile to use for the build
	Dockerfile string `json:"dockerfile,omitempty"`

	// Additional tags to apply to the image
	// +listType=set
	Tags []string `json:"tags,omitempty"`

	// Additional --build-arg values to pass to the build command
	// +listType=atomic
	Args []EnvVar `json:"args,omitempty"`

	// Build time secrets to be passed in to the builder via --secret
	// +listType=atomic
	Secrets []ContainerBuildSecret `json:"secrets,omitempty"`

	// Optional: The name of the build stage to use for the build
	Stage string `json:"stage,omitempty"`

	// Labels to apply to the built image
	// +listType=map
	// +listMapKey=key
	Labels []Label `json:"labels,omitempty"`

	// Optional target platform for the build (e.g. "linux/amd64")
	Platform string `json:"platform,omitempty"`
}

func (c1 *ContainerBuildContext) Equal(c2 *ContainerBuildContext) bool {
	if c1 == c2 {
		return true
	}

	if c1 == nil || c2 == nil {
		return false
	}

	if c1.Context != c2.Context {
		return false
	}

	if c1.Dockerfile != c2.Dockerfile {
		return false
	}

	if c1.Stage != c2.Stage {
		return false
	}

	if c1.Platform != c2.Platform {
		return false
	}

	if !slices.Equal(c1.Args, c2.Args) {
		return false
	}

	if !slices.Equal(c1.Secrets, c2.Secrets) {
		return false
	}

	return true
}

type ImagePullPolicy string

const (
	// Always pull the container image
	ImagePullPolicyAlways ImagePullPolicy = "always"

	// Pull the container image only if it is not present
	ImagePullPolicyMissing ImagePullPolicy = "missing"

	// Never pull the container image
	ImagePullPolicyNever ImagePullPolicy = "never"

	PullPolicyAlways  = ImagePullPolicyAlways
	PullPolicyMissing = ImagePullPolicyMissing
	PullPolicyNever   = ImagePullPolicyNever
)

type FileSystemEntryType string

const (
	FileSystemEntryTypeFile    FileSystemEntryType = "file"    // default
	FileSystemEntryTypeOpenSSL FileSystemEntryType = "openssl" // special type for OpenSSL certificates
	FileSystemEntryTypeDir     FileSystemEntryType = "directory"
	// The public CreateFiles API validation doesn't allow specifying "symlink" as a FileSystemEntry
	// type, but the internal ContainerOrchestrator.CreateFiles library does support it.
	FileSystemEntryTypeSymlink FileSystemEntryType = "symlink"
)

// Represents part of the file structure to be created in the container
// +k8s:openapi-gen=true
type FileSystemEntry struct {
	// The type of entry (file, symlink, or directory)
	Type FileSystemEntryType `json:"type,omitempty"`

	// The name of the entry (required)
	Name string `json:"name"`

	// The UID of the file owner. Defaults to 0 (root).
	Owner *int32 `json:"owner,omitempty"`

	// The ID of the file group. Defaults to 0 (root).
	Group *int32 `json:"group,omitempty"`

	// The unix mode permissions of this entry. If Mode is 0, the umask for the create file request will be applied.
	Mode fs.FileMode `json:"mode,omitempty"`

	// For file type entries, an optional path to a source file to copy. It's an error to set both a Source and Contents for a file.
	Source string `json:"source,omitempty"`

	// For symlink type entries, the target of the symlink. The target must be a valid path in the container (existing or created as
	// part of this create files set). The value can either be an absolute path or a relative path from the newly created symlink.
	Target string `json:"target,omitempty"`

	// For file type entries, the string contents of the file. Optional.
	Contents string `json:"contents,omitempty"`

	// For file type entries, the Base64 encoded byte contents of the file. Optional
	RawContents string `json:"rawContents,omitempty"`

	// For file type entries, if true, errors creating this file will be logged, but will not cause the overall CreateFiles operation to fail.
	ContinueOnError bool `json:"continueOnError,omitempty"`

	// For directory type entries, the child entries (files or directories). Optional.
	// +listType=atomic
	Entries []FileSystemEntry `json:"entries,omitempty"`
}

func (fse *FileSystemEntry) GetType() FileSystemEntryType {
	if fse.Type == "" {
		return FileSystemEntryTypeFile
	}

	return fse.Type
}

func (cfi *FileSystemEntry) Equal(other *FileSystemEntry) bool {
	if cfi == other {
		return true
	}

	if cfi == nil || other == nil {
		return false
	}

	if cfi.Type != other.Type {
		return false
	}

	if cfi.Name != other.Name {
		return false
	}

	if !pointers.EqualValue(cfi.Owner, other.Owner) {
		return false
	}

	if !pointers.EqualValue(cfi.Group, other.Group) {
		return false
	}

	if cfi.Mode != other.Mode {
		return false
	}

	if cfi.Source != other.Source {
		return false
	}

	if cfi.Contents != other.Contents {
		return false
	}

	if cfi.RawContents != other.RawContents {
		return false
	}

	if cfi.ContinueOnError != other.ContinueOnError {
		return false
	}

	if !slices.EqualFunc(cfi.Entries, other.Entries, func(i1, i2 FileSystemEntry) bool {
		return i1.Equal(&i2)
	}) {
		return false
	}

	return true
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
		errorList = append(errorList, field.Invalid(fieldPath.Child("type"), fse.Type, "type must be one of 'file', 'certificate', or 'directory'"))
	}

	if fse.GetType() == FileSystemEntryTypeFile || fse.GetType() == FileSystemEntryTypeOpenSSL {
		if len(fse.Entries) > 0 {
			errorList = append(errorList, field.Forbidden(fieldPath.Child("entries"), fmt.Sprintf("dirEntry cannot be set for %s type entries", fse.GetType())))
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

// Describes files and/or folders to be created in the Container before it is started
// +k8s:openapi-gen=true
type CreateFileSystem struct {
	// The destination path for the file (should already exist in the container)
	Destination string `json:"destination,omitempty"`

	// The default owner ID for created files (defaults to 0 for root)
	DefaultOwner int32 `json:"defaultOwner,omitempty"`

	// The default group ID for created files (defaults to 0 for root)
	DefaultGroup int32 `json:"defaultGroup,omitempty"`

	// The umask for created files and folders without explicit permissions set (defaults to 022)
	Umask *fs.FileMode `json:"umask,omitempty"`

	// The specific entries to create in the container (must have at least one item)
	// +listType=atomic
	Entries []FileSystemEntry `json:"entries,omitempty"`
}

func (cf *CreateFileSystem) Equal(other *CreateFileSystem) bool {
	if cf == other {
		return true
	}

	if cf == nil || other == nil {
		return false
	}

	if cf.Destination != other.Destination {
		return false
	}

	if !pointers.EqualValue(cf.Umask, other.Umask) {
		return false
	}

	if !slices.EqualFunc(cf.Entries, other.Entries, func(i1, i2 FileSystemEntry) bool {
		return i1.Equal(&i2)
	}) {
		return false
	}

	return true
}

// Represents a tar file to be applied as an additional image layer when running the container.
// The layer can be provided either as a path to a tar file (with a SHA256 hash for verification)
// or as base64-encoded tar contents.
// +k8s:openapi-gen=true
type ImageLayer struct {
	// An opaque identifier for this layer used in lifecycle key generation.
	// This allows tracking whether a layer has meaningfully changed independently of
	// the raw binary content (which may vary due to timestamps or other
	// materially unimportant differences in the tar file).
	Digest string `json:"digest"`

	// Path to a tar file on the host filesystem. Mutually exclusive with RawContents.
	Source string `json:"source,omitempty"`

	// SHA256 hash of the tar file referenced by Source, used for integrity verification. Required when Source is set.
	SHA256 string `json:"sha256,omitempty"`

	// Base64-encoded tar file contents. Mutually exclusive with Source.
	RawContents string `json:"rawContents,omitempty"`
}

func (il *ImageLayer) Equal(other *ImageLayer) bool {
	if il == other {
		return true
	}

	if il == nil || other == nil {
		return false
	}

	if il.Digest != other.Digest {
		return false
	}

	if il.Source != other.Source {
		return false
	}

	if il.SHA256 != other.SHA256 {
		return false
	}

	if il.RawContents != other.RawContents {
		return false
	}

	return true
}

func (il *ImageLayer) Validate(fieldPath *field.Path) field.ErrorList {
	if il == nil {
		return nil
	}

	var errorList field.ErrorList

	if il.Digest == "" {
		errorList = append(errorList, field.Required(fieldPath.Child("digest"), "digest must be set to a non-empty value"))
	}

	if il.Source == "" && il.RawContents == "" {
		errorList = append(errorList, field.Required(fieldPath, "either source or rawContents must be set"))
	}

	if il.Source != "" && il.RawContents != "" {
		errorList = append(errorList, field.Forbidden(fieldPath.Child("rawContents"), "source and rawContents cannot be set at the same time"))
	}

	if il.Source != "" && il.SHA256 == "" {
		errorList = append(errorList, field.Required(fieldPath.Child("sha256"), "sha256 must be set when source is specified"))
	}

	if il.SHA256 != "" && il.Source == "" {
		errorList = append(errorList, field.Forbidden(fieldPath.Child("sha256"), "sha256 can only be set when source is specified"))
	}

	if il.SHA256 != "" {
		hexPart := il.SHA256
		if strings.HasPrefix(strings.ToLower(hexPart), "sha256:") {
			hexPart = hexPart[7:]
		}
		if !validSHA256HexRegexp.MatchString(hexPart) {
			errorList = append(errorList, field.Invalid(fieldPath.Child("sha256"), il.SHA256, "sha256 must be a 64-character hex string, optionally prefixed with 'sha256:'"))
		}
	}

	if il.RawContents != "" {
		if _, decodeErr := base64.StdEncoding.DecodeString(il.RawContents); decodeErr != nil {
			errorList = append(errorList, field.Invalid(fieldPath.Child("rawContents"), "<base64 data>", fmt.Sprintf("rawContents must be valid base64: %s", decodeErr.Error())))
		}
	}

	return errorList
}

type ContainerRestartPolicy string

const (
	ContainerRestartPolicyNone          ContainerRestartPolicy = "no"
	ContainerRestartPolicyOnFailure     ContainerRestartPolicy = "on-failure"
	ContainerRestartPolicyUnlessStopped ContainerRestartPolicy = "unless-stopped"
	ContainerRestartPolicyAlways        ContainerRestartPolicy = "always"

	RestartPolicyNone          = ContainerRestartPolicyNone
	RestartPolicyOnFailure     = ContainerRestartPolicyOnFailure
	RestartPolicyUnlessStopped = ContainerRestartPolicyUnlessStopped
	RestartPolicyAlways        = ContainerRestartPolicyAlways
)
