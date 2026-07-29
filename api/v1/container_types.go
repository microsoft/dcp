/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"context"
	"encoding/gob"
	"errors"
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

	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/pointers"
)

var (
	// See: https://github.com/moby/moby/blob/master/daemon/names/names.go
	validContainerName       = `^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`
	validContainerNameRegexp = regexp.MustCompile(validContainerName)
)

// +kubebuilder:validation:Enum=session;persistent;existing;cleanup
type ContainerMode string

const (
	// ContainerModeSession creates the container with the Container resource and removes it when the resource is deleted.
	ContainerModeSession ContainerMode = "session"
	// ContainerModePersistent creates or reuses the container and leaves it running when the resource is deleted.
	ContainerModePersistent ContainerMode = "persistent"
	// ContainerModeExisting reuses an existing container but does not create or delete it.
	ContainerModeExisting ContainerMode = "existing"
	// ContainerModeCleanup reuses an existing container without creating it, then removes it when the resource is deleted.
	ContainerModeCleanup ContainerMode = "cleanup"
)

var supportedContainerModes = []string{
	string(ContainerModeSession),
	string(ContainerModePersistent),
	string(ContainerModeExisting),
	string(ContainerModeCleanup),
}

func (mode ContainerMode) ShouldReuseExisting() bool {
	switch mode {
	case ContainerModePersistent,
		ContainerModeExisting,
		ContainerModeCleanup:
		return true
	default:
		return false
	}
}

func (mode ContainerMode) ShouldCreateIfMissing() bool {
	switch mode {
	case ContainerModeSession,
		ContainerModePersistent:
		return true
	default:
		return false
	}
}

func (mode ContainerMode) ShouldDeleteContainer() bool {
	switch mode {
	case ContainerModeSession,
		ContainerModeCleanup:
		return true
	default:
		return false
	}
}

// Represents a collection of PEM formatted certificates to be written into the container
// +k8s:openapi-gen=true
type ContainerPemCertificates struct {
	// The individual certificates in PEM format.
	// +listType=atomic
	Certificates []PemCertificate `json:"certificates,omitempty"`

	// The base destination path in the container where the certificates will be written. Must be an absolute path.
	// This path will be created if it does not already exist. Individual certificate files will be created
	// in a subfolder named "certs" under this path along with OpenSSL thumbprint symlinks to each of them.
	// The certificate bundle will be created in a file named "cert.pem" under this path.
	Destination string `json:"destination,omitempty"`

	// Optional list of bundle files to overwrite with the generated certificate bundle.
	// Each path in the list must be an absolute path to a file in the container.
	// Any existing file at these paths will be overwritten with the generated certificate bundle or created if it does not exist.
	// +listType=set
	OverwriteBundlePaths []string `json:"overwriteBundlePaths,omitempty"`

	// If true, any invalid certificates in the Certificates list will be skipped, but any valid certificates will still be written.
	// If false, the entire operation will fail if any invalid certificates are found.
	ContinueOnError bool `json:"continueOnError,omitempty"`
}

func (pc *ContainerPemCertificates) Equal(other *ContainerPemCertificates) bool {
	if pc == other {
		return true
	}

	if pc == nil || other == nil {
		return false
	}

	if !slices.EqualFunc(pc.Certificates, other.Certificates, func(c1, c2 PemCertificate) bool {
		return c1.Equal(&c2)
	}) {
		return false
	}

	if pc.Destination != other.Destination {
		return false
	}

	if !slices.Equal(pc.OverwriteBundlePaths, other.OverwriteBundlePaths) {
		return false
	}

	if pc.ContinueOnError != other.ContinueOnError {
		return false
	}

	return true
}

func (pc *ContainerPemCertificates) Validate(fieldPath *field.Path) field.ErrorList {
	if pc == nil {
		return nil
	}

	var errorList field.ErrorList
	if len(pc.Certificates) == 0 {
		errorList = append(errorList, field.Required(fieldPath.Child("certificates"), "at least one certificate must be specified"))
	}

	if !path.IsAbs(pc.Destination) {
		errorList = append(errorList, field.Invalid(fieldPath.Child("destination"), pc.Destination, "destination must be an absolute path"))
	}

	for i, bundlePath := range pc.OverwriteBundlePaths {
		if !path.IsAbs(bundlePath) {
			errorList = append(errorList, field.Invalid(fieldPath.Child("overwriteBundlePaths").Index(i), bundlePath, "each overwrite bundle path must be an absolute path"))
		}
	}

	for i, cert := range pc.Certificates {
		errorList = append(errorList, cert.Validate(fieldPath.Child("certificates").Index(i))...)
	}

	return errorList
}

// ContainerSpec defines the desired state of a Container
// +k8s:openapi-gen=true
type ContainerSpec struct {
	// Optional container image (required if Build is not specified)
	// If Build is specified and Image is set, the value of Image will be used to tag the resulting built image.
	// If Build is omitted, the value of Image will be used to pull the container image to run.
	Image string `json:"image,omitempty"`

	// Optional build context to use to build the container image
	Build *commonapi.ContainerBuildContext `json:"build,omitempty"`

	// Optional container name
	ContainerName string `json:"containerName,omitempty"`

	// Consumed volume information
	// +listType=atomic
	VolumeMounts []commonapi.VolumeMount `json:"volumeMounts,omitempty"`

	// Exposed ports
	// +listType=atomic
	Ports []commonapi.ContainerPort `json:"ports,omitempty"`

	// Environment settings
	// +listType=map
	// +listMapKey=name
	Env []commonapi.EnvVar `json:"env,omitempty"`

	// Environment files to use to populate Container environment during startup.
	// +listType=set
	EnvFiles []string `json:"envFiles,omitempty"`

	// Container restart policy
	RestartPolicy commonapi.ContainerRestartPolicy `json:"restartPolicy,omitempty"`

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
	Networks *[]commonapi.ContainerNetworkConnectionConfig `json:"networks,omitempty"`

	// Controls how the container is created, reused, and cleaned up.
	// Ignored when persistent is true.
	Mode ContainerMode `json:"mode,omitempty"`

	// Should this container be created and persisted between DCP runs?
	Persistent bool `json:"persistent,omitempty"`

	// Optional parent process PID used to scope persistent Container cleanup to a process lifecycle.
	// When set, MonitorTimestamp must also be set and the effective mode must be persistent.
	// +optional
	MonitorPID *int64 `json:"monitorPid,omitempty"`

	// Optional parent process identity timestamp used with MonitorPID to guard against PID reuse.
	// +optional
	MonitorTimestamp metav1.MicroTime `json:"monitorTimestamp,omitempty"`

	// Additional arguments to pass to the container run command
	// +listType=atomic
	RunArgs []string `json:"runArgs,omitempty"`

	// Labels to apply to the container
	// +listType=map
	// +listMapKey=key
	Labels []commonapi.Label `json:"labels,omitempty"`

	// Health probe configuration for the Container
	// +listType=atomic
	HealthProbes []HealthProbe `json:"healthProbes,omitempty"`

	// Optional key used to identify if an existing persistent container needs to be restarted.
	// If not set, the controller will calculate a key based on a hash of specific fields in the ContainerSpec.
	LifecycleKey string `json:"lifecycleKey,omitempty"`

	// Pull policy for container base images, if not set uses the default configuration for the container runtime.
	PullPolicy commonapi.ImagePullPolicy `json:"pullPolicy,omitempty"`

	// Files to create in the container before starting it
	// +listType=atomic
	CreateFiles []commonapi.CreateFileSystem `json:"createFiles,omitempty"`

	// Tar files to apply as additional image layers when running the container.
	// Each layer tar will be applied on top of the base image, producing a derived image
	// that is used to create the container.
	// +listType=atomic
	ImageLayers []commonapi.ImageLayer `json:"imageLayers,omitempty"`

	// PEM formatted public certificates to be created in the container
	// +optional
	PemCertificates *ContainerPemCertificates `json:"pemCertificates,omitempty"`

	// Optional terminal/PTY configuration. When set, the container's primary process
	// is started with connection to a pseudo-terminal
	// and its stdin/stdout/stderr are bridged to the configured UDS via HMP v1,
	// instead of the container being run detached with separate log capture.
	// +optional
	Terminal *TerminalSpec `json:"terminal,omitempty"`
}

func (cs ContainerSpec) EffectiveMode() ContainerMode {
	if cs.Persistent {
		return ContainerModePersistent
	}
	if cs.Mode == "" {
		return ContainerModeSession
	}
	return cs.Mode
}

func containerModeSupported(mode ContainerMode) bool {
	switch mode {
	case ContainerModeSession,
		ContainerModePersistent,
		ContainerModeExisting,
		ContainerModeCleanup:
		return true
	default:
		return false
	}
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

	if !pointers.EqualValueFunc(cs.Build, other.Build, func(c1, c2 *commonapi.ContainerBuildContext) bool {
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

	if !pointers.EqualValueFunc(cs.Networks, other.Networks, func(c1, c2 *[]commonapi.ContainerNetworkConnectionConfig) bool {
		return slices.EqualFunc(*c1, *c2, func(cncc1, cncc2 commonapi.ContainerNetworkConnectionConfig) bool {
			return cncc1.Equal(&cncc2)
		})
	}) {
		return false
	}

	if cs.Mode != other.Mode {
		return false
	}

	if cs.Persistent != other.Persistent {
		return false
	}

	if !pointers.EqualValue(cs.MonitorPID, other.MonitorPID) {
		return false
	}

	if !osutil.MicroEqual(cs.MonitorTimestamp, other.MonitorTimestamp) {
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

	if !cs.PemCertificates.Equal(other.PemCertificates) {
		return false
	}

	if !slices.EqualFunc(cs.ImageLayers, other.ImageLayers, func(l1, l2 commonapi.ImageLayer) bool {
		return l1.Equal(&l2)
	}) {
		return false
	}

	if !cs.Terminal.Equal(other.Terminal) {
		return false
	}

	return true
}

func (cs *ContainerSpec) GetLifecycleKey() (string, bool, error) {
	if cs.LifecycleKey != "" {
		return cs.LifecycleKey, false, nil
	}

	fnvHash := fnv.New128()
	encoder := gob.NewEncoder(fnvHash)

	var writeErr, hashErr error

	if cs.Build == nil {
		// Use the image name for the hash
		_, writeErr = fnvHash.Write([]byte(cs.Image))
		hashErr = errors.Join(hashErr, writeErr)
	} else {
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
			_, writeErr = fnvHash.Write(contents)
			hashErr = errors.Join(hashErr, writeErr)
		} else {
			// Failed to read the Dockerfile, so just use the path for the hash
			_, writeErr = fnvHash.Write([]byte(dockerfile))
			hashErr = errors.Join(hashErr, writeErr)
		}

		// Add the build context to the hash
		_, writeErr = fnvHash.Write([]byte(cs.Build.Context))
		hashErr = errors.Join(hashErr, writeErr)

		// Add the build stage to the hash
		_, writeErr = fnvHash.Write([]byte(cs.Build.Stage))
		hashErr = errors.Join(hashErr, writeErr)

		// Add the build platform to the hash; changing the target platform
		// produces a different image, so persistent containers must rebuild.
		// Encoded via gob (length-framed) and only when set, to keep the
		// hash unambiguous against the adjacent Stage write and to preserve
		// the legacy key for existing workloads where Platform is unset.
		if cs.Build.Platform != "" {
			hashErr = errors.Join(hashErr, encoder.Encode(cs.Build.Platform))
		}

		if len(cs.Build.Labels) > 0 {
			// Add the build labels to the hash
			sortedLabels := slices.Clone(cs.Build.Labels)
			slices.SortFunc(sortedLabels, func(l1, l2 commonapi.Label) int {
				return strings.Compare(l1.Key, l2.Key)
			})

			for i := range sortedLabels {
				hashErr = errors.Join(hashErr, encoder.Encode(lifecycleHashContainerLabel(sortedLabels[i])))
			}
		}

		if len(cs.Build.Secrets) > 0 {
			// Add the build secrets to the hash
			sortedSecrets := slices.Clone(cs.Build.Secrets)
			slices.SortFunc(sortedSecrets, func(s1, s2 commonapi.ContainerBuildSecret) int {
				return strings.Compare(s1.ID, s2.ID)
			})

			for i := range sortedSecrets {
				hashErr = errors.Join(hashErr, encoder.Encode(lifecycleHashContainerBuildSecret(sortedSecrets[i])))
				switch sortedSecrets[i].Type {
				case "", commonapi.BuildSecretTypeFile:
					// For file type secrets, track the contents of the file as part of the hash
					fileContents, secretFileReadErr := os.ReadFile(sortedSecrets[i].Source)
					if secretFileReadErr == nil {
						_, writeErr = fnvHash.Write(fileContents)
						hashErr = errors.Join(hashErr, writeErr)
					}
				case commonapi.BuildSecretTypeEnv:
					// For env type secrets, track the value of the environment variable
					value := os.Getenv(sortedSecrets[i].Source)
					_, writeErr = fnvHash.Write([]byte(value))
					hashErr = errors.Join(hashErr, writeErr)
				}
			}
		}
	}

	if len(cs.VolumeMounts) > 0 {
		// Add the volume mounts to the hash
		sortedVolumes := slices.Clone(cs.VolumeMounts)
		slices.SortFunc(sortedVolumes, func(v1, v2 commonapi.VolumeMount) int {
			return strings.Compare(v1.Target, v2.Target)
		})

		for i := range sortedVolumes {
			hashErr = errors.Join(hashErr, encoder.Encode(lifecycleHashVolumeMount(sortedVolumes[i])))
		}
	}

	if len(cs.Ports) > 0 {
		// Add the ports to the hash
		sortedPorts := slices.Clone(cs.Ports)
		slices.SortFunc(sortedPorts, func(p1, p2 commonapi.ContainerPort) int {
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
			hashErr = errors.Join(hashErr, encodeContainerPortLifecycleHash(encoder, sortedPorts[i]))
		}
	}

	if len(cs.Env) > 0 {
		// Add the environment variables to the hash
		sortedEnv := slices.Clone(cs.Env)
		slices.SortFunc(sortedEnv, func(e1, e2 commonapi.EnvVar) int {
			return strings.Compare(e1.Name, e2.Name)
		})

		for i := range sortedEnv {
			hashErr = errors.Join(hashErr, encoder.Encode(lifecycleHashEnvVar(sortedEnv[i])))
		}
	}

	if len(cs.EnvFiles) > 0 {
		// Add the environment files to the hash
		sortedEnvFiles := slices.Clone(cs.EnvFiles)
		slices.Sort(sortedEnvFiles)

		for i := range sortedEnvFiles {
			readBytes, readErr := os.ReadFile(sortedEnvFiles[i])
			if readErr != nil {
				hashErr = errors.Join(hashErr, readErr)
			} else {
				_, _ = fnvHash.Write(readBytes)
			}
		}
	}

	if cs.MonitorPID != nil {
		hashErr = errors.Join(hashErr, encoder.Encode(*cs.MonitorPID))
		hashErr = errors.Join(hashErr, encoder.Encode(cs.MonitorTimestamp.Time))
	}

	if len(cs.CreateFiles) > 0 {
		// Add the create files to the hash
		sortedCreateFiles := slices.Clone(cs.CreateFiles)
		slices.SortFunc(sortedCreateFiles, func(f1, f2 commonapi.CreateFileSystem) int {
			return strings.Compare(f1.Destination, f2.Destination)
		})

		for i := range sortedCreateFiles {
			hashErr = errors.Join(hashErr, encoder.Encode(lifecycleHashCreateFileSystem(sortedCreateFiles[i])))
		}
	}

	if cs.PemCertificates != nil {
		// Add the PEM certificates to the hash
		sortedPemCertificates := slices.Clone(cs.PemCertificates.Certificates)
		slices.SortFunc(sortedPemCertificates, func(c1, c2 PemCertificate) int {
			return strings.Compare(c1.Thumbprint, c2.Thumbprint)
		})

		for i := range sortedPemCertificates {
			hashErr = errors.Join(hashErr, encoder.Encode(sortedPemCertificates[i]))
		}

		hashErr = errors.Join(hashErr, encoder.Encode(cs.PemCertificates.Destination))

		sortedOverwritePaths := slices.Clone(cs.PemCertificates.OverwriteBundlePaths)
		slices.Sort(sortedOverwritePaths)
		for i := range sortedOverwritePaths {
			hashErr = errors.Join(hashErr, encoder.Encode(sortedOverwritePaths[i]))
		}
	}

	if len(cs.ImageLayers) > 0 {
		// Add image layer digests to the hash in order, as layer order is significant
		// (later layers override files from earlier layers). Only the Digest field is used,
		// not the raw tar contents, because the Digest represents meaningful data identity
		// independent of binary differences. Each digest is encoded via gob to ensure
		// unambiguous separation between entries.
		for i := range cs.ImageLayers {
			hashErr = errors.Join(hashErr, encoder.Encode(cs.ImageLayers[i].Digest))
		}
	}

	if cs.Terminal != nil {
		// Columns and rows do not matter that much (the client can always resize the terminal as necessary),
		// but once a Container is started with terminal support, the UDS path and socket mode do not change.
		hashErr = errors.Join(hashErr, encoder.Encode(cs.Terminal.UDSPath))
		hashErr = errors.Join(hashErr, encoder.Encode(cs.Terminal.SocketMode.Normalized()))
	}

	// Compute the hash for the lifecycle key
	lifecycleKey := fmt.Sprintf("%x", fnvHash.Sum(nil))

	return lifecycleKey, true, hashErr
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

	// ContainerStateNotFound indicates the Container is waiting for an existing container that does not exist.
	ContainerStateNotFound ContainerState = "NotFound"

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

	// The filesystem path of the terminal HMP v1 Unix domain socket, when the Container is
	// configured with a terminal. In "listen" mode this is the socket DCP owns (and is the
	// DCP-generated path when the spec left UDSPath empty); in "connect" mode it is the peer-owned
	// socket DCP dials.
	TerminalSocketPath string `json:"terminalSocketPath,omitempty"`

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
	EffectiveEnv []commonapi.EnvVar `json:"effectiveEnv,omitempty"`

	// Effective values of launch arguments to be passed to the Container, after all substitutions are applied.
	// +listType=atomic
	EffectiveArgs []string `json:"effectiveArgs,omitempty"`

	// List of ContainerNetworks the Container is connected to
	// +listType=set
	Networks []string `json:"networks,omitempty"`

	// Health status of the Container
	HealthStatus HealthStatus `json:"healthStatus,omitempty"`

	// Results of running health probes (most recent per probe)
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

func (c *Container) GetResourceId() string {
	return fmt.Sprintf("container-%s", c.UID)
}

func (c *Container) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "containers",
	}
}

func (c *Container) GetLeaseKey() string {
	return fmt.Sprintf("%s/%s", c.GetGroupVersionResource().Resource, strings.TrimSpace(c.Spec.ContainerName))
}

func (c *Container) GetObjectMeta() *metav1.ObjectMeta {
	return &c.ObjectMeta
}

func (c *Container) GetStatus() apiserver_resource.StatusSubResource {
	return c.Status
}

func (_ *Container) New() runtime.Object {
	return &Container{}
}

func (_ *Container) NewList() runtime.Object {
	return &ContainerList{}
}

func (_ *Container) IsStorageVersion() bool {
	return true
}

func (_ *Container) NamespaceScoped() bool {
	return false
}

func (_ *Container) ShortNames() []string {
	return []string{"ctr"}
}

func (c *Container) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      c.Name,
		Namespace: c.Namespace,
	}
}

func (c *Container) Validate(ctx context.Context) field.ErrorList {
	errorList := field.ErrorList{}

	if ResourceCreationProhibited.Load() && c.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, errResourceCreationProhibited.Error()))
	}

	effectiveMode := c.Spec.EffectiveMode()
	requiresContainerImage := effectiveMode == ContainerModeSession || effectiveMode == ContainerModePersistent
	if requiresContainerImage && c.Spec.Build == nil && c.Spec.Image == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec", "image"), "image must be set to a non-empty value"))
	}

	if c.Spec.Image != "" && strings.ContainsAny(c.Spec.Image, "\r\n\t ") {
		errorList = append(errorList, field.Invalid(field.NewPath("spec", "image"), c.Spec.Image, "image must not contain whitespace or control characters"))
	}

	if c.Spec.Build != nil {
		if c.Spec.Build.Context == "" {
			errorList = append(errorList, field.Required(field.NewPath("spec", "build", "context"), "context must be set to a non-empty value when build is specified"))
		}

		for i, secret := range c.Spec.Build.Secrets {
			if secret.Type != "" && secret.Type != commonapi.BuildSecretTypeFile && secret.Type != commonapi.BuildSecretTypeEnv {
				errorList = append(errorList, field.Invalid(field.NewPath("spec", "build", "secrets").Index(i).Child("type"), secret.Type, "type must be one of 'file' or 'env'"))
			}

			if secret.ID == "" {
				errorList = append(errorList, field.Required(field.NewPath("spec", "build", "secrets").Index(i).Child("id"), "id must be set to a non-empty value"))
			}

			if secret.Type != commonapi.BuildSecretTypeEnv && secret.Source == "" {
				errorList = append(errorList, field.Required(field.NewPath("spec", "build", "secrets").Index(i).Child("source"), "source must be set to a non-empty value"))
			}
		}

		for i, label := range c.Spec.Build.Labels {
			// TODO: Validate key format?
			if label.Key == "" {
				errorList = append(errorList, field.Required(field.NewPath("spec", "build", "labels").Index(i).Child("name"), "name must be set to a non-empty value"))
			}

			if label.Value == "" {
				errorList = append(errorList, field.Required(field.NewPath("spec", "build", "labels").Index(i).Child("value"), "value must be set to a non-empty value"))
			}
		}
	}

	for i, label := range c.Spec.Labels {
		// TODO: Validate key format?
		if label.Key == "" {
			errorList = append(errorList, field.Required(field.NewPath("spec", "labels").Index(i).Child("name"), "name must be set to a non-empty value"))
		}

		if label.Value == "" {
			errorList = append(errorList, field.Required(field.NewPath("spec", "labels").Index(i).Child("value"), "value must be set to a non-empty value"))
		}
	}

	specPath := field.NewPath("spec")
	errorList = append(errorList, commonapi.ValidateContainerPorts(c.Spec.Ports, specPath.Child("ports"))...)

	// Validate the object name to ensure it is a valid container name
	if c.Spec.ContainerName != "" && !validContainerNameRegexp.MatchString(c.Spec.ContainerName) {
		errorList = append(errorList, field.Invalid(field.NewPath("spec", "containerName"), c.Spec.ContainerName, fmt.Sprintf("containerName must match regex '%s'", validContainerName)))
	}

	if !c.Spec.Persistent && c.Spec.Mode != "" && !containerModeSupported(c.Spec.Mode) {
		errorList = append(errorList, field.NotSupported(specPath.Child("mode"), c.Spec.Mode, supportedContainerModes))
	}

	if effectiveMode != ContainerModeSession {
		if c.Spec.ContainerName == "" {
			message := "containerName must be set to a value when mode requires an existing or persistent container"
			if c.Spec.Persistent {
				message = "containerName must be set to a value when persistent is true"
			}
			errorList = append(errorList, field.Required(specPath.Child("containerName"), message))
		}
		if c.Spec.Terminal != nil {
			message := "Container modes that reuse existing containers cannot use a terminal."
			if c.Spec.Persistent {
				message = "Persistent Containers cannot use a terminal."
			}
			errorList = append(errorList, field.Forbidden(specPath.Child("terminal"), message))
		}
	}

	monitorTimestampSet := !c.Spec.MonitorTimestamp.IsZero()
	if c.Spec.MonitorPID != nil && *c.Spec.MonitorPID <= 0 {
		errorList = append(errorList, field.Invalid(specPath.Child("monitorPid"), *c.Spec.MonitorPID, "monitorPid must be positive"))
	}
	if effectiveMode != ContainerModePersistent && c.Spec.MonitorPID != nil {
		errorList = append(errorList, field.Forbidden(specPath.Child("monitorPid"), "monitorPid can only be set for persistent containers"))
	}
	if effectiveMode != ContainerModePersistent && monitorTimestampSet {
		errorList = append(errorList, field.Forbidden(specPath.Child("monitorTimestamp"), "monitorTimestamp can only be set for persistent containers"))
	}
	if c.Spec.MonitorPID != nil && !monitorTimestampSet {
		errorList = append(errorList, field.Required(specPath.Child("monitorTimestamp"), "monitorTimestamp must be set when monitorPid is set"))
	}
	if c.Spec.MonitorPID == nil && monitorTimestampSet {
		errorList = append(errorList, field.Required(specPath.Child("monitorPid"), "monitorPid must be set when monitorTimestamp is set"))
	}

	healthProbesPath := specPath.Child("healthProbes")
	for i, probe := range c.Spec.HealthProbes {
		errorList = append(errorList, probe.Validate(healthProbesPath.Index(i))...)
	}

	for i, createFile := range c.Spec.CreateFiles {
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

	imageLayersPath := field.NewPath("spec", "imageLayers")
	for i, layer := range c.Spec.ImageLayers {
		errorList = append(errorList, layer.Validate(imageLayersPath.Index(i))...)
	}

	// Validate PEM certificates configuration
	errorList = append(errorList, c.Spec.PemCertificates.Validate(field.NewPath("spec", "pemCertificates"))...)

	// Validate terminal configuration
	errorList = append(errorList, c.Spec.Terminal.Validate(field.NewPath("spec", "terminal"))...)

	// Validate that annotations don't exceed the Kubernetes size limit.
	// This provides a clearer error message than the generic Kubernetes API server error,
	// especially when long arguments or environment variables are stored in annotations.
	errorList = append(errorList, commonapi.ValidateAnnotationsSize(c.Annotations, field.NewPath("metadata", "annotations"))...)

	return errorList
}

func (c *Container) ValidateUpdate(ctx context.Context, obj runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldContainer := obj.(*Container)

	// The image property cannot be changed after the resource is first created
	if oldContainer.Spec.Image != c.Spec.Image {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "image"), "image cannot be changed"))
	}

	if !oldContainer.Spec.Build.Equal(c.Spec.Build) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "build"), "build cannot be changed"))
	}

	// A container name cannot be changed after it's created
	if oldContainer.Spec.ContainerName != c.Spec.ContainerName {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "containerName"), "containerName cannot be changed"))
	}

	if oldContainer.Spec.Networks != nil && c.Spec.Networks == nil {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "networks"), "networks cannot be set to null if it was initialized with a list value"))
	}

	if oldContainer.Spec.Networks == nil && c.Spec.Networks != nil {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "networks"), "networks cannot be set to a list value if it was initialized as null"))
	}

	// Make sure start isn't changed to false after the container was created
	if (oldContainer.Spec.Start == nil || *oldContainer.Spec.Start) && (c.Spec.Start != nil && !*c.Spec.Start) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "start"), "start cannot be set to false after container creation"))
	}

	// Make sure stop isn't set to false after having been set to true
	if oldContainer.Spec.Stop && c.Spec.Stop != oldContainer.Spec.Stop {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "stop"), "stop cannot be set to false once it has been set to true"))
	}

	// Make sure Persistent isn't changed after the container is created
	if oldContainer.Spec.Persistent != c.Spec.Persistent {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "persistent"), "persistent cannot be changed"))
	}

	if oldContainer.Spec.EffectiveMode() != c.Spec.EffectiveMode() {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "mode"), "mode cannot be changed"))
	}

	if !pointers.EqualValue(oldContainer.Spec.MonitorPID, c.Spec.MonitorPID) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "monitorPid"), "monitorPid cannot be changed"))
	}

	if !osutil.MicroEqual(oldContainer.Spec.MonitorTimestamp, c.Spec.MonitorTimestamp) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "monitorTimestamp"), "monitorTimestamp cannot be changed"))
	}

	// Forbid changing labels after the resource is created
	if !slices.Equal(oldContainer.Spec.Labels, c.Spec.Labels) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "labels"), "labels cannot be changed"))
	}

	if len(oldContainer.Spec.HealthProbes) != len(c.Spec.HealthProbes) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "healthProbes"), "Health probes cannot be changed once a Container is created."))
	} else {
		for i, probe := range oldContainer.Spec.HealthProbes {
			if !probe.Equal(&c.Spec.HealthProbes[i]) {
				errorList = append(errorList, field.Forbidden(field.NewPath("spec", "healthProbes").Index(i), "Health probes cannot be changed once a Container is created."))
			}
		}
	}

	if oldContainer.Spec.PullPolicy != c.Spec.PullPolicy {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "pullPolicy"), "pullPolicy cannot be changed"))
	}

	if len(oldContainer.Spec.CreateFiles) != len(c.Spec.CreateFiles) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "createFiles"), "created files cannot be changed once a Container is created."))
	} else {
		for i, item := range oldContainer.Spec.CreateFiles {
			if !item.Equal(&c.Spec.CreateFiles[i]) {
				errorList = append(errorList, field.Forbidden(field.NewPath("spec", "createFiles").Index(i), "created files cannot be changed once a Container is created."))
			}
		}
	}

	if !oldContainer.Spec.PemCertificates.Equal(c.Spec.PemCertificates) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "pemCertificates"), "pemCertificates cannot be changed once a Container is created."))
	}

	if len(oldContainer.Spec.ImageLayers) != len(c.Spec.ImageLayers) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "imageLayers"), "image layers cannot be changed once a Container is created."))
	} else {
		for i, layer := range oldContainer.Spec.ImageLayers {
			if !layer.Equal(&c.Spec.ImageLayers[i]) {
				errorList = append(errorList, field.Forbidden(field.NewPath("spec", "imageLayers").Index(i), "image layers cannot be changed once a Container is created."))
			}
		}
	}

	errorList = append(errorList, c.Spec.Terminal.ValidateUpdate(oldContainer.Spec.Terminal, field.NewPath("spec", "terminal"))...)

	return errorList
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

// True if the Container is in a terminal state.
func (c *Container) Done() bool {
	return c.Status.State == ContainerStateFailedToStart || c.Status.State == ContainerStateExited || c.Status.State == ContainerStateUnknown
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
var _ statestore.LeasableResource = (*Container)(nil)
