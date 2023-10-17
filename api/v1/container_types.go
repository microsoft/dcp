package v1

import (
	"context"

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
)

// ContainerStatus describes the status of a Container
// +k8s:openapi-gen=true
type ContainerStatus struct {
	// +kubebuilder:default:="Pending"
	// Current state of the Container.
	State ContainerState `json:"state,omitempty"`

	// ID of the Container (if an attempt to start the Container was made)
	ContainerID string `json:"containerId,omitempty"`

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
	// TODO: implement validation https://github.com/microsoft/usvc-apiserver/issues/2
	return nil
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
