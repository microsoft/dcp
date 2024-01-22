package v1

import (
	"context"
	"fmt"
	"regexp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
)

// See: https://github.com/moby/moby/blob/master/daemon/names/names.go
var validContainerName = `^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`
var validContainerNameRegexp = regexp.MustCompile(validContainerName)

type ContainerRestartPolicy string

const (
	// Do not automatically restart the container when it exits (default)
	RestartPolicyNone ContainerRestartPolicy = "no"

	// Restart only if the container exits with non-zero status
	RestartPolicyOnFailure = "on-failure"

	// Restart container, except if container is explicitly stopped (or container daemon is stopped/restarted)
	RestartPolicyUnlessStopped = "unless-stopped"

	// Always try to restart the container
	RestartPolicyAlways = "always"
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

// ContainerSpec defines the desired state of a Container
// +k8s:openapi-gen=true
type ContainerSpec struct {
	// Container image
	Image string `json:"image"`

	// Optional container name
	ContainerName string `json:"containerName,omitempty"`

	// Consumed volume information
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`

	// Exposed ports
	Ports []ContainerPort `json:"ports,omitempty"`

	// Environment settings
	Env []EnvVar `json:"env,omitempty"`

	// Environment files to use to populate Container environment during startup.
	EnvFiles []string `json:"envFiles,omitempty"`

	// Container restart policy
	RestartPolicy ContainerRestartPolicy `json:"restartPolicy,omitempty"`

	// Command to run in the container
	Command string `json:"command,omitempty"`

	// Arguments to pass to the command
	Args []string `json:"args,omitempty"`

	// Should the controller attempt to stop the container?
	// +kubebuilder:default:=false
	Stop bool `json:"stop,omitempty"`

	// ContaineNetworks resources the container should be attached to. If omitted or nil, the container will
	// be attached to the default network and the controller will not manage network connections.
	// +listType:=atomic
	Networks *[]ContainerNetworkConnectionConfig `json:"networks,omitempty"`

	// Should this container be created and persisted between DCP runs?
	Persistent bool `json:"persistent,omitempty"`
}

// +k8s:openapi-gen=true
type ContainerNetworkConnectionConfig struct {
	// Name of the network to connect to
	Name string `json:"name"`

	// Aliases of the container on the network
	// +listType:=atomic
	Aliases []string `json:"aliases,omitempty"`
}

type ContainerState string

const (
	// Pending is the initial Container state. No attempt has been made to run the container yet.
	ContainerStatePending ContainerState = "Pending"

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

	// Container was running at some point, but has been removed.
	ContainerStateRemoved ContainerState = "Removed"

	// Unknown means for some reason container state is unavailable.
	ContainerStateUnknown ContainerState = "Unknown"

	// An existing container was not found
	ContainerStateNotFound ContainerState = "NotFound"
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
	StartupTimestamp metav1.Time `json:"startupTimestamp,omitempty"`

	// Timestamp when the Container was terminated last
	FinishTimestamp metav1.Time `json:"finishTimestamp,omitempty"`

	// Exit code of the Container.
	// Default is -1, meaning the exit code is not known, or the container is still running.
	// +kubebuilder:default:=-1
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`

	// A human-readable message that provides additional information about Container state.
	Message string `json:"message,omitempty"`

	// Effective values of environment variables, after all substitutions are applied.
	// +listType:=map
	// +listMapKey:=name
	EffectiveEnv []EnvVar `json:"effectiveEnv,omitempty"`

	// Effective values of launch arguments to be passed to the Container, after all substitutions are applied.
	// +listType:=atomic
	EffectiveArgs []string `json:"effectiveArgs,omitempty"`

	// List of ContainerNetworks the Container is connected to
	Networks []string `json:"networks,omitempty"`
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

	if e.Spec.Image == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec", "image"), "image must be set to a non-empty value"))
	}

	// Validate the object name to ensure it is a valid container name
	if e.Spec.ContainerName != "" && !validContainerNameRegexp.MatchString(e.Spec.ContainerName) {
		errorList = append(errorList, field.Invalid(field.NewPath("spec", "containerName"), e.Spec.ContainerName, fmt.Sprintf("containerName must match regex '%s'", validContainerName)))
	}

	// Validate that stop isn't set to true when mode is set to Create or Existing
	if e.Spec.Persistent && e.Spec.Stop {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "stop"), "stop cannot be set to true if persistent is true"))
	}

	if e.Spec.Persistent && e.Spec.ContainerName == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec", "containerName"), "containerName must be set to a value when persistent is true"))
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

	// Make sure stop isn't set to false after having been set to true
	if oldContainer.Spec.Stop && e.Spec.Stop != oldContainer.Spec.Stop {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "stop"), "stop cannot be set to false once it has been set to true"))
	}

	// Make sure Persistent isn't changed after the container is created
	if oldContainer.Spec.Persistent != e.Spec.Persistent {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "persistent"), "persistent cannot be changed"))
	}

	// Ensure that we forbid attempting to stop persistent resources
	if e.Spec.Persistent && e.Spec.Stop {
		errorList = append(errorList, field.Invalid(field.NewPath("spec", "stop"), e.NamespacedName().Name, "stop cannot be set to true if persistent is true"))
	}

	return errorList
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

func (cl *ContainerList) GetItems() []ctrl_client.Object {
	retval := make([]ctrl_client.Object, len(cl.Items))
	for i := range cl.Items {
		retval[i] = &cl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&Container{}, &ContainerList{})
	SetCleanupPriority(&Container{}, 10)
}

// Ensure types support interfaces expected by our API server
var _ apiserver_resource.Object = (*Container)(nil)
var _ apiserver_resource.ObjectList = (*ContainerList)(nil)
var _ commonapi.ListWithObjectItems = (*ContainerList)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*Container)(nil)
var _ apiserver_resource.StatusSubResource = (*ContainerStatus)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*Container)(nil)
var _ apiserver_resourcestrategy.Validater = (*Container)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*Container)(nil)
