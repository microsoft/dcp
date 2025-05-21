package v1

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	generic_registry "k8s.io/apiserver/pkg/registry/generic"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"

	apiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
)

var (
	// See: https://github.com/moby/moby/blob/master/daemon/names/names.go
	validContainerName       = `^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`
	validContainerNameRegexp = regexp.MustCompile(validContainerName)
)

type ContainerRestartPolicy string

const (
	// Do not automatically restart the container when it exits (default)
	RestartPolicyNone ContainerRestartPolicy = "no"

	// Restart only if the container exits with non-zero status
	RestartPolicyOnFailure ContainerRestartPolicy = "on-failure"

	// Restart container, except if container is explicitly stopped (or container daemon is stopped/restarted)
	RestartPolicyUnlessStopped ContainerRestartPolicy = "unless-stopped"

	// Always try to restart the container
	RestartPolicyAlways ContainerRestartPolicy = "always"
)

type VolumeMountType string

const (
	// A volume mount to a host directory
	BindMount VolumeMountType = "bind"

	// A volume mount to a named volume managed by an orchestrator
	NamedVolumeMount VolumeMountType = "volume"
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
	TCP PortProtocol = "TCP"
	UDP PortProtocol = "UDP"
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

	// The port to be used, defaults to TCP
	Protocol PortProtocol `json:"protocol,omitempty"`

	// Optional: What host IP to bind the external port to.
	HostIP string `json:"hostIP,omitempty"`
}

type BuildSecretType string

const (
	EnvSecret  BuildSecretType = "env"
	FileSecret BuildSecretType = "file"
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
	Labels []ContainerLabel `json:"labels,omitempty"`
}

// +k8s:openapi-gen=true
type ContainerLabel struct {
	// The label key
	Key string `json:"key"`

	// The label value
	Value string `json:"value"`
}

type PullPolicy string

const (
	// Always pull the container image
	PullPolicyAlways PullPolicy = "always"

	// Pull the container image only if it is not present
	PullPolicyMissing PullPolicy = "missing"

	// Never pull the container image
	PullPolicyNever PullPolicy = "never"
)

type FileSystemEntryType string

const (
	FileSystemEntryTypeFile FileSystemEntryType = "file" // default
	FileSystemEntryTypeDir  FileSystemEntryType = "directory"
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

	// For file type entries, the contents of the file. Optional.
	Contents string `json:"contents,omitempty"`

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

	if fse.Type != "" && fse.Type != FileSystemEntryTypeFile && fse.Type != FileSystemEntryTypeDir {
		errorList = append(errorList, field.Invalid(fieldPath.Child("type"), fse.Type, "type must be one of 'file' or 'directory'"))
	}

	if fse.Type == "" || fse.Type == FileSystemEntryTypeFile {
		if len(fse.Entries) > 0 {
			errorList = append(errorList, field.Forbidden(fieldPath.Child("entries"), "dirEntry cannot be set for file type entries"))
		}

		if fse.Source != "" && fse.Contents != "" {
			errorList = append(errorList, field.Forbidden(fieldPath.Child("contents"), "source and contents cannot be set at the same time"))
		}
	}

	if fse.Type == FileSystemEntryTypeDir {
		if fse.Source != "" {
			errorList = append(errorList, field.Forbidden(fieldPath.Child("source"), "source cannot be set for directory type entries"))
		}

		if fse.Contents != "" {
			errorList = append(errorList, field.Forbidden(fieldPath.Child("contents"), "contents cannot be set for directory type entries"))
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

// ContainerSpec defines the desired state of a Container
// +k8s:openapi-gen=true
type ContainerSpec struct {
	// Optional container image (required if Build is not specified)
	// If Build is specified and Image is set, the value of Image will be used to tag the resulting built image.
	// If Build is omitted, the value of Image will be used to pull the container image to run.
	Image string `json:"image,omitempty"`

	// Optional build context to use to build the container image
	Build *ContainerBuildContext `json:"build,omitempty"`

	// Optional container name
	ContainerName string `json:"containerName,omitempty"`

	// Consumed volume information
	// +listType=atomic
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`

	// Exposed ports
	// +listType=atomic
	Ports []ContainerPort `json:"ports,omitempty"`

	// Environment settings
	// +listType=map
	// +listMapKey=name
	Env []EnvVar `json:"env,omitempty"`

	// Environment files to use to populate Container environment during startup.
	// +listType=set
	EnvFiles []string `json:"envFiles,omitempty"`

	// Container restart policy
	RestartPolicy ContainerRestartPolicy `json:"restartPolicy,omitempty"`

	// Command to run in the container
	Command string `json:"command,omitempty"`

	// Arguments to pass to the command
	// +listType=atomic
	Args []string `json:"args,omitempty"`

	// Should the controller attempt to start the container?
	// +kubebuilder:default:=true
	Start *bool `json:"start,omitempty"`

	// Should the controller attempt to stop the container?
	// +kubebuilder:default:=false
	Stop bool `json:"stop,omitempty"`

	// ContainerNetworks resources the container should be attached to. If omitted or nil, the container will
	// be attached to the default network and the controller will not manage network connections.
	// +listType=atomic
	Networks *[]ContainerNetworkConnectionConfig `json:"networks,omitempty"`

	// Should this container be created and persisted between DCP runs?
	Persistent bool `json:"persistent,omitempty"`

	// Additional arguments to pass to the container run command
	// +listType=atomic
	RunArgs []string `json:"runArgs,omitempty"`

	// Labels to apply to the container
	// +listType=map
	// +listMapKey=key
	Labels []ContainerLabel `json:"labels,omitempty"`

	// Health probe configuration for the Container
	// +listType=atomic
	HealthProbes []HealthProbe `json:"healthProbes,omitempty"`

	// Optional key used to identify if an existing persistent container needs to be restarted.
	// If not set, the controller will calculate a key based on a hash of specific fields in the ContainerSpec.
	LifecycleKey string `json:"lifecycleKey,omitempty"`

	// Pull policy for container base images, if not set uses the default configuration for the container runtime.
	PullPolicy PullPolicy `json:"pullPolicy,omitempty"`

	// Files to create in the container before starting it
	// +listType=atomic
	CreateFiles []CreateFileSystem `json:"createFiles,omitempty"`
}

func (cs *ContainerSpec) Equal(other *ContainerSpec) bool {
	if cs == other {
		return true
	}

	if cs == nil || other == nil {
		return false
	}

	if cs.Image != other.Image {
		return false
	}

	if !pointers.EqualValueFunc(cs.Build, other.Build, func(c1, c2 *ContainerBuildContext) bool {
		return c1.Equal(c2)
	}) {
		return false
	}

	if cs.ContainerName != other.ContainerName {
		return false
	}

	if !slices.Equal(cs.VolumeMounts, other.VolumeMounts) {
		return false
	}

	if !slices.Equal(cs.Ports, other.Ports) {
		return false
	}

	if !slices.Equal(cs.Env, other.Env) {
		return false
	}

	if !slices.Equal(cs.EnvFiles, other.EnvFiles) {
		return false
	}

	if cs.RestartPolicy != other.RestartPolicy {
		return false
	}

	if cs.Command != other.Command {
		return false
	}

	if !slices.Equal(cs.Args, other.Args) {
		return false
	}

	if pointers.GetValueOrDefault(cs.Start, true) != pointers.GetValueOrDefault(other.Start, true) {
		return false
	}

	if !pointers.EqualValue(cs.Start, other.Start) {
		return false
	}

	if cs.Stop != other.Stop {
		return false
	}

	if !pointers.EqualValueFunc(cs.Networks, other.Networks, func(c1, c2 *[]ContainerNetworkConnectionConfig) bool {
		return slices.EqualFunc(*c1, *c2, func(cncc1, cncc2 ContainerNetworkConnectionConfig) bool {
			return cncc1.Equal(&cncc2)
		})
	}) {
		return false
	}

	if cs.Persistent != other.Persistent {
		return false
	}

	if !slices.Equal(cs.RunArgs, other.RunArgs) {
		return false
	}

	if !slices.Equal(cs.Labels, other.Labels) {
		return false
	}

	if !slices.EqualFunc(cs.HealthProbes, other.HealthProbes, func(hp1, hp2 HealthProbe) bool {
		return hp1.Equal(&hp2)
	}) {
		return false
	}

	if cs.LifecycleKey != other.LifecycleKey {
		return false
	}

	if cs.PullPolicy != other.PullPolicy {
		return false
	}

	return true
}

func (cs *ContainerSpec) GetLifecycleKey() (string, bool) {
	if cs.LifecycleKey != "" {
		return cs.LifecycleKey, false
	}

	var hashBytes bytes.Buffer
	encoder := gob.NewEncoder(&hashBytes)

	fnvHash := fnv.New128()

	// Use the image name for the hash
	_, _ = fnvHash.Write([]byte(cs.Image))

	if cs.Build != nil {
		// Use the build context for the hash

		// First attempt to determine the path to the Dockerfile
		dockerfile := cs.Build.Dockerfile

		if dockerfile == "" {
			dockerfile = filepath.Join(cs.Build.Context, "Dockerfile")
		}

		if !filepath.IsAbs(dockerfile) {
			dockerfile = filepath.Clean(filepath.Join(cs.Build.Context, dockerfile))
		}

		contents, readErr := os.ReadFile(dockerfile)
		if readErr == nil {
			// Use the contents of the Dockerfile for the hash
			_, _ = fnvHash.Write(contents)
		} else {
			// Failed to read the Dockerfile, so just use the path for the hash
			_, _ = fnvHash.Write([]byte(dockerfile))
		}

		// Add the build context to the hash
		_, _ = fnvHash.Write([]byte(cs.Build.Context))

		// Add the build stage to the hash
		_, _ = fnvHash.Write([]byte(cs.Build.Stage))

		if len(cs.Build.Labels) > 0 {
			// Add the build labels to the hash
			sortedLabels := slices.Clone(cs.Build.Labels)
			slices.SortFunc(sortedLabels, func(l1, l2 ContainerLabel) int {
				return strings.Compare(l1.Key, l2.Key)
			})

			for i := range sortedLabels {
				encodeErr := encoder.Encode(sortedLabels[i])
				if encodeErr == nil {
					_, _ = fnvHash.Write(hashBytes.Bytes())
				}
			}
		}

		if len(cs.Build.Secrets) > 0 {
			// Add the build secrets to the hash
			sortedSecrets := slices.Clone(cs.Build.Secrets)
			slices.SortFunc(sortedSecrets, func(s1, s2 ContainerBuildSecret) int {
				return strings.Compare(s1.ID, s2.ID)
			})

			for i := range sortedSecrets {
				encodeErr := encoder.Encode(sortedSecrets[i])
				if encodeErr == nil {
					_, _ = fnvHash.Write(hashBytes.Bytes())
				}

				if sortedSecrets[i].Type == "" || sortedSecrets[i].Type == FileSecret {
					// For file type secrets, track the contents of the file as part of the hash
					fileContents, readErr := os.ReadFile(sortedSecrets[i].Source)
					if readErr == nil {
						_, _ = fnvHash.Write(fileContents)
					}
				} else if sortedSecrets[i].Type == EnvSecret {
					// For env type secrets, track the value of the environment variable
					value := os.Getenv(sortedSecrets[i].Source)
					_, _ = fnvHash.Write([]byte(value))
				}
			}
		}
	}

	if len(cs.VolumeMounts) > 0 {
		// Add the volume mounts to the hash
		sortedVolumes := slices.Clone(cs.VolumeMounts)
		slices.SortFunc(sortedVolumes, func(v1, v2 VolumeMount) int {
			return strings.Compare(v1.Target, v2.Target)
		})

		for i := range sortedVolumes {
			encodeErr := encoder.Encode(sortedVolumes[i])
			if encodeErr == nil {
				_, _ = fnvHash.Write(hashBytes.Bytes())
			}
		}
	}

	if len(cs.Ports) > 0 {
		// Add the ports to the hash
		sortedPorts := slices.Clone(cs.Ports)
		slices.SortFunc(sortedPorts, func(p1, p2 ContainerPort) int {
			compare := strings.Compare(string(p1.Protocol), string(p2.Protocol))
			if compare != 0 {
				return compare
			}

			if p1.HostPort < p2.HostPort {
				return -1
			} else if p1.HostPort > p2.HostPort {
				return 1
			}

			return 0
		})

		for i := range sortedPorts {
			encodeErr := encoder.Encode(sortedPorts[i])
			if encodeErr == nil {
				_, _ = fnvHash.Write(hashBytes.Bytes())
			}
		}
	}

	if len(cs.Env) > 0 {
		// Add the environment variables to the hash
		sortedEnv := slices.Clone(cs.Env)
		slices.SortFunc(sortedEnv, func(e1, e2 EnvVar) int {
			return strings.Compare(e1.Name, e2.Name)
		})

		for i := range sortedEnv {
			encodeErr := encoder.Encode(sortedEnv[i])
			if encodeErr == nil {
				_, _ = fnvHash.Write(hashBytes.Bytes())
			}
		}
	}

	if len(cs.EnvFiles) > 0 {
		// Add the environment files to the hash
		sortedEnvFiles := slices.Clone(cs.EnvFiles)
		slices.Sort(sortedEnvFiles)

		for i := range sortedEnvFiles {
			readBytes, readErr := os.ReadFile(sortedEnvFiles[i])
			if readErr == nil {
				_, _ = fnvHash.Write(readBytes)
			}
		}
	}

	if len(cs.CreateFiles) > 0 {
		// Add the create files to the hash
		sortedCreateFiles := slices.Clone(cs.CreateFiles)
		slices.SortFunc(sortedCreateFiles, func(f1, f2 CreateFileSystem) int {
			return strings.Compare(f1.Destination, f2.Destination)
		})

		for i := range sortedCreateFiles {
			encodeErr := encoder.Encode(sortedCreateFiles[i])
			if encodeErr == nil {
				_, _ = fnvHash.Write(hashBytes.Bytes())
			}
		}
	}

	// Compute the hash for the lifecycle key
	lifecycleKey := fmt.Sprintf("%x", fnvHash.Sum(nil))

	return lifecycleKey, true
}

// +k8s:openapi-gen=true
type ContainerNetworkConnectionConfig struct {
	// Name of the network to connect to
	Name string `json:"name"`

	// Aliases of the container on the network
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

type ContainerState string

const (
	// Same as ContainerStatePending. May be encountered if the Container status has not been initialized yet.
	ContainerStateEmpty ContainerState = ""

	// Pending is the initial Container state. No attempt has been made to run the container yet.
	ContainerStatePending ContainerState = "Pending"

	// ContainerStateRuntimeUnhealthy indicates that the container start is blocked because the runtime isn't healthy, but will resume once the runtime is started.
	ContainerStateRuntimeUnhealthy ContainerState = "RuntimeUnhealthy"

	// Building is an optional state that indicates the container is in the process of being built.
	ContainerStateBuilding ContainerState = "Building"

	// Container is in the process of starting
	ContainerStateStarting ContainerState = "Starting"

	// A start attempt was made, but it failed
	ContainerStateFailedToStart ContainerState = "FailedToStart"

	// Container has been started and is executing
	ContainerStateRunning ContainerState = "Running"

	// Container is paused
	ContainerStatePaused ContainerState = "Paused"

	// Container finished execution
	ContainerStateExited ContainerState = "Exited"

	// Unknown means for some reason container state is unavailable.
	ContainerStateUnknown ContainerState = "Unknown"

	// Container is in the process of stopping
	ContainerStateStopping ContainerState = "Stopping"
)

// ContainerStatus describes the status of a Container
// +k8s:openapi-gen=true
type ContainerStatus struct {
	// +kubebuilder:default:="Pending"
	// Current state of the Container.
	State ContainerState `json:"state,omitempty"`

	// ID of the Container (if an attempt to start the Container was made)
	ContainerID string `json:"containerId,omitempty"`

	// Name of the Container (if an attempt to start the Container was made)
	ContainerName string `json:"containerName,omitempty"`

	// Timestamp of the Container start attempt
	StartupTimestamp metav1.MicroTime `json:"startupTimestamp,omitempty"`

	// Timestamp when the Container was terminated last
	FinishTimestamp metav1.MicroTime `json:"finishTimestamp,omitempty"`

	// The path of a temporary file that contains captured standard output data from the Container startup process.
	StartupStdOutFile string `json:"startupStdOutFile,omitempty"`

	// The path of a temporary file that contains captured standard error data from the Container startup process.
	StartupStdErrFile string `json:"startupStdErrFile,omitempty"`

	// Exit code of the Container.
	// Default is -1, meaning the exit code is not known, or the container is still running.
	// +kubebuilder:default:=-1
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`

	// A human-readable message that provides additional information about Container state.
	Message string `json:"message,omitempty"`

	// Effective values of environment variables, after all substitutions are applied.
	// +listType=map
	// +listMapKey=name
	EffectiveEnv []EnvVar `json:"effectiveEnv,omitempty"`

	// Effective values of launch arguments to be passed to the Container, after all substitutions are applied.
	// +listType=atomic
	EffectiveArgs []string `json:"effectiveArgs,omitempty"`

	// List of ContainerNetworks the Container is connected to
	// +listType=set
	Networks []string `json:"networks,omitempty"`

	// Health status of the Container
	HealthStatus HealthStatus `json:"healthStatus,omitempty"`

	// Results of running health probes (most reacent per probe)
	// +listType=map
	// +listMapKey=probeName
	HealthProbeResults []HealthProbeResult `json:"healthProbeResults,omitempty"`

	// The lifecycle key from the spec or the value calculated by the controller
	LifecycleKey string `json:"lifecycleKey,omitempty"`
}

func (cs ContainerStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	cs.DeepCopyInto(&dest.(*Container).Status)
}

// Container resource represents a container run using an orchestrator such as Docker or Podman
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type Container struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ContainerSpec   `json:"spec,omitempty"`
	Status ContainerStatus `json:"status,omitempty"`
}

func (c *Container) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "containers",
	}
}

func (c *Container) GetObjectMeta() *metav1.ObjectMeta {
	return &c.ObjectMeta
}

func (c *Container) GetStatus() apiserver_resource.StatusSubResource {
	return c.Status
}

func (e *Container) New() runtime.Object {
	return &Container{}
}

func (e *Container) NewList() runtime.Object {
	return &ContainerList{}
}

func (e *Container) IsStorageVersion() bool {
	return true
}

func (e *Container) NamespaceScoped() bool {
	return false
}

func (e *Container) ShortNames() []string {
	return []string{"ctr"}
}

func (e *Container) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      e.Name,
		Namespace: e.Namespace,
	}
}

func (e *Container) Validate(ctx context.Context) field.ErrorList {
	errorList := field.ErrorList{}

	if ResourceCreationProhibited.Load() {
		errorList = append(errorList, field.Forbidden(nil, errResourceCreationProhibited.Error()))
	}

	if e.Spec.Build == nil && e.Spec.Image == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec", "image"), "image must be set to a non-empty value"))
	}

	if e.Spec.Build != nil {
		if e.Spec.Build.Context == "" {
			errorList = append(errorList, field.Required(field.NewPath("spec", "build", "context"), "context must be set to a non-empty value when build is specified"))
		}

		for i, secret := range e.Spec.Build.Secrets {
			if secret.Type != "" && secret.Type != FileSecret && secret.Type != EnvSecret {
				errorList = append(errorList, field.Invalid(field.NewPath("spec", "build", "secrets").Index(i).Child("type"), secret.Type, "type must be one of 'file' or 'env'"))
			}

			if secret.ID == "" {
				errorList = append(errorList, field.Required(field.NewPath("spec", "build", "secrets").Index(i).Child("id"), "id must be set to a non-empty value"))
			}

			if secret.Type != EnvSecret && secret.Source == "" {
				errorList = append(errorList, field.Required(field.NewPath("spec", "build", "secrets").Index(i).Child("source"), "source must be set to a non-empty value"))
			}
		}

		for i, label := range e.Spec.Build.Labels {
			// TODO: Validate key format?
			if label.Key == "" {
				errorList = append(errorList, field.Required(field.NewPath("spec", "build", "labels").Index(i).Child("name"), "name must be set to a non-empty value"))
			}

			if label.Value == "" {
				errorList = append(errorList, field.Required(field.NewPath("spec", "build", "labels").Index(i).Child("value"), "value must be set to a non-empty value"))
			}
		}
	}

	for i, label := range e.Spec.Labels {
		// TODO: Validate key format?
		if label.Key == "" {
			errorList = append(errorList, field.Required(field.NewPath("spec", "labels").Index(i).Child("name"), "name must be set to a non-empty value"))
		}

		if label.Value == "" {
			errorList = append(errorList, field.Required(field.NewPath("spec", "labels").Index(i).Child("value"), "value must be set to a non-empty value"))
		}
	}

	// Validate the object name to ensure it is a valid container name
	if e.Spec.ContainerName != "" && !validContainerNameRegexp.MatchString(e.Spec.ContainerName) {
		errorList = append(errorList, field.Invalid(field.NewPath("spec", "containerName"), e.Spec.ContainerName, fmt.Sprintf("containerName must match regex '%s'", validContainerName)))
	}

	if e.Spec.Persistent && e.Spec.ContainerName == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec", "containerName"), "containerName must be set to a value when persistent is true"))
	}

	healthProbesPath := field.NewPath("spec", "healthProbes")
	for i, probe := range e.Spec.HealthProbes {
		errorList = append(errorList, probe.Validate(healthProbesPath.Index(i))...)
	}

	for i, createFile := range e.Spec.CreateFiles {
		if createFile.Destination != "" && !path.IsAbs(createFile.Destination) {
			errorList = append(errorList, field.Invalid(field.NewPath("spec", "createFiles").Index(i).Child("destination"), createFile.Destination, "destination must be absolute"))
		}

		if createFile.Umask != nil && !fs.FileMode(*createFile.Umask).IsRegular() {
			errorList = append(errorList, field.Invalid(field.NewPath("spec", "createFiles").Index(i).Child("umask"), *createFile.Umask, "umask must not include type bits"))
		}

		if len(createFile.Entries) == 0 {
			errorList = append(errorList, field.Required(field.NewPath("spec", "createFiles").Index(i).Child("entries"), "at least one child entry must be specified"))
		}

		for j, item := range createFile.Entries {
			errorList = append(errorList, item.Validate(field.NewPath("spec", "createFiles").Index(i).Child("entries").Index(j))...)
		}

		if createFile.DefaultOwner < 0 {
			errorList = append(errorList, field.Invalid(field.NewPath("spec", "createFiles").Index(i).Child("defaultOwner"), createFile.DefaultOwner, "default owner must be a non-negative integer"))
		}

		if createFile.DefaultGroup < 0 {
			errorList = append(errorList, field.Invalid(field.NewPath("spec", "createFiles").Index(i).Child("defaultGroup"), createFile.DefaultGroup, "default group must be a non-negative integer"))
		}
	}

	return errorList
}

func (e *Container) ValidateUpdate(ctx context.Context, obj runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldContainer := obj.(*Container)

	// The image property cannot be changed after the resource is first created
	if oldContainer.Spec.Image != e.Spec.Image {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "image"), "image cannot be changed"))
	}

	if !oldContainer.Spec.Build.Equal(e.Spec.Build) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "build"), "build cannot be changed"))
	}

	// A container name cannot be changed after it's created
	if oldContainer.Spec.ContainerName != e.Spec.ContainerName {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "containerName"), "containerName cannot be changed"))
	}

	if oldContainer.Spec.Networks != nil && e.Spec.Networks == nil {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "networks"), "networks cannot be set to null if it was initialized with a list value"))
	}

	if oldContainer.Spec.Networks == nil && e.Spec.Networks != nil {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "networks"), "networks cannot be set to a list value if it was initialized as null"))
	}

	// Make sure start isn't changed to false after the container was created
	if (oldContainer.Spec.Start == nil || *oldContainer.Spec.Start) && (e.Spec.Start != nil && !*e.Spec.Start) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "start"), "start cannot be set to false after container creation"))
	}

	// Make sure stop isn't set to false after having been set to true
	if oldContainer.Spec.Stop && e.Spec.Stop != oldContainer.Spec.Stop {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "stop"), "stop cannot be set to false once it has been set to true"))
	}

	// Make sure Persistent isn't changed after the container is created
	if oldContainer.Spec.Persistent != e.Spec.Persistent {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "persistent"), "persistent cannot be changed"))
	}

	// Forbid changing labels after the resource is created
	if !slices.Equal(oldContainer.Spec.Labels, e.Spec.Labels) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "labels"), "labels cannot be changed"))
	}

	if len(oldContainer.Spec.HealthProbes) != len(e.Spec.HealthProbes) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "healthProbes"), "Health probes cannot be changed once a Container is created."))
	} else {
		for i, probe := range oldContainer.Spec.HealthProbes {
			if !probe.Equal(&e.Spec.HealthProbes[i]) {
				errorList = append(errorList, field.Forbidden(field.NewPath("spec", "healthProbes").Index(i), "Health probes cannot be changed once a Container is created."))
			}
		}
	}

	if oldContainer.Spec.PullPolicy != e.Spec.PullPolicy {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "pullPolicy"), "pullPolicy cannot be changed"))
	}

	if len(oldContainer.Spec.CreateFiles) != len(e.Spec.CreateFiles) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "createFiles"), "created files cannot be changed once a Container is created."))
	} else {
		for i, item := range oldContainer.Spec.CreateFiles {
			if !item.Equal(&e.Spec.CreateFiles[i]) {
				errorList = append(errorList, field.Forbidden(field.NewPath("spec", "createFiles").Index(i), "created files cannot be changed once a Container is created."))
			}
		}
	}

	return errorList
}

// Equivalence check for ContainerBuildContex for use in validation
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

	// If the build arguments aren't the same
	if !slices.Equal(c1.Args, c2.Args) {
		return false
	}

	// If the secret arguments aren't the same
	if !slices.Equal(c1.Secrets, c2.Secrets) {
		return false
	}

	return true
}

func (c *Container) SpecifiedImageNameOrDefault() string {
	if c.Spec.Image != "" {
		return c.Spec.Image
	}

	return c.NamespacedName().Name + ":dev"
}

func (c *Container) ShouldStart() bool {
	return c.Spec.Start == nil || *c.Spec.Start
}

func (*Container) GenericSubResources() []apiserver_resource.GenericSubResource {
	return []apiserver_resource.GenericSubResource{
		&ContainerLogResource{},
	}
}

// ContainerList contains a list of Executable instances
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type ContainerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Container `json:"items"`
}

func (cl *ContainerList) GetListMeta() *metav1.ListMeta {
	return &cl.ListMeta
}

func (cl *ContainerList) ItemCount() uint32 {
	return uint32(len(cl.Items))
}

func (cl *ContainerList) GetItems() []*Container {
	retval := make([]*Container, len(cl.Items))
	for i := range cl.Items {
		retval[i] = &cl.Items[i]
	}
	return retval
}

type ContainerLogResource struct{}

func (clr *ContainerLogResource) Name() string {
	return LogSubresourceName
}

func (clr *ContainerLogResource) GetStorageProvider(
	obj apiserver_resource.Object,
	rootPath string,
	parentSP apiserver.StorageProvider,
) apiserver.StorageProvider {
	return func(scheme *runtime.Scheme, reg generic_registry.RESTOptionsGetter) (registry_rest.Storage, error) {
		storage, err := parentSP(scheme, reg)
		if err != nil {
			return nil, fmt.Errorf("failed to get parent (%s) storage: %w", obj.GetObjectKind().GroupVersionKind().Kind, err)
		}

		containerStorage, isGetter := storage.(registry_rest.StandardStorage)
		if !isGetter {
			return nil, fmt.Errorf("parent (%s) should implement registry_rest.Getter", obj.GetObjectKind().GroupVersionKind().Kind)
		}

		logStreamFactory, found := ResourceLogStreamers.Load(obj.GetGroupVersionResource())
		if !found {
			return nil, fmt.Errorf("log stream factory not found for resource '%s'", obj.GetGroupVersionResource().String())
		}

		logStorage, err := NewLogStorage(containerStorage, logStreamFactory)
		if err != nil {
			return nil, err
		}

		return logStorage, nil
	}
}

func init() {
	SchemeBuilder.Register(&Container{}, &ContainerList{})
}

// Ensure types support interfaces expected by our API server
var _ apiserver_resource.Object = (*Container)(nil)
var _ apiserver_resource.ObjectList = (*ContainerList)(nil)
var _ commonapi.ListWithObjectItems[Container, *Container] = (*ContainerList)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*Container)(nil)
var _ apiserver_resource.StatusSubResource = (*ContainerStatus)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*Container)(nil)
var _ apiserver_resourcestrategy.Validater = (*Container)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*Container)(nil)
var _ apiserver_resource.ObjectWithGenericSubResource = (*Container)(nil)
var _ apiserver_resource.GenericSubResource = (*ContainerLogResource)(nil)
