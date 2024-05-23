package v1

import (
	"context"
	"fmt"
	"regexp"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	generic_registry "k8s.io/apiserver/pkg/registry/generic"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
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

// +k8s:openapi-gen=true
type ContainerBuildContext struct {
	// The path to the directory to be used as the root of the build context
	Context string `json:"context"`

	// The path to a Dockerfile to use for the build
	Dockerfile string `json:"dockerfile,omitempty"`

	// Additional --build-arg values to pass to the build command
	Args []EnvVar `json:"args,omitempty"`

	// Optional: The name of the build stage to use for the build
	Stage string `json:"stage,omitempty"`
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

	// Additional arguments to pass to the container run command
	// +listType:=atomic
	RunArgs []string `json:"runArgs,omitempty"`
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
	// Same as ContainerStatePending. May be encountered if the Container status has not been initialized yet.
	ContainerStateEmpty ContainerState = ""

	// Pending is the initial Container state. No attempt has been made to run the container yet.
	ContainerStatePending ContainerState = "Pending"

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
	StartupTimestamp metav1.Time `json:"startupTimestamp,omitempty"`

	// Timestamp when the Container was terminated last
	FinishTimestamp metav1.Time `json:"finishTimestamp,omitempty"`

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

	if e.Spec.Build == nil && e.Spec.Image == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec", "image"), "image must be set to a non-empty value"))
	}

	if e.Spec.Build != nil {
		if e.Spec.Build.Context == "" {
			errorList = append(errorList, field.Required(field.NewPath("spec", "build", "context"), "context must be set to a non-empty value when build is specified"))
		}
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

// Equivalnce check for ContainerBuildContex for use in validation
func (c1 *ContainerBuildContext) Equal(c2 *ContainerBuildContext) bool {
	if c1 == c2 {
		return true
	}

	if c1 == nil || c2 == nil {
		return false
	}

	return c1.Context == c2.Context && c1.Dockerfile == c2.Dockerfile && c1.Stage == c2.Stage && slices.EqualFunc(c1.Args, c2.Args, func(e1 EnvVar, e2 EnvVar) bool {
		return e1.Name == e2.Name && e1.Value == e2.Value
	})
}

func (c *Container) SpecifiedImageNameOrDefault() string {
	if c.Spec.Image != "" {
		return c.Spec.Image
	}

	return c.NamespacedName().Name + ":dev"
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

func (cl *ContainerList) GetItems() []ctrl_client.Object {
	retval := make([]ctrl_client.Object, len(cl.Items))
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
			return nil, fmt.Errorf("log stream factory not found for resource %s", obj.GetGroupVersionResource().String())
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
	SetCleanupPriority(&Container{}, 100)
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
var _ apiserver_resource.ObjectWithGenericSubResource = (*Container)(nil)
var _ apiserver_resource.GenericSubResource = (*ContainerLogResource)(nil)
