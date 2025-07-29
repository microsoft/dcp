package v1

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
)

type ContainerNetworkTunnelProxyState string

const (
	// The same as ContainerNetworkTunnelProxyStatePending.
	// May be encountered when ContainerNetworkTunnelProxy status has not been initialized yet.
	ContainerNetworkTunnelProxyStateEmpty ContainerNetworkTunnelProxyState = ""

	// Initial state - proxy pair is being created.
	ContainerNetworkTunnelProxyStatePending ContainerNetworkTunnelProxyState = "Pending"

	// Building the client proxy container image.
	ContainerNetworkTunnelProxyStateBuildingImage ContainerNetworkTunnelProxyState = "BuildingImage"

	// Starting the proxy pair.
	ContainerNetworkTunnelProxyStateStarting ContainerNetworkTunnelProxyState = "Starting"

	// Proxy pair is ready with all tunnels operational.
	ContainerNetworkTunnelProxyStateRunning ContainerNetworkTunnelProxyState = "Running"

	// Proxy pair encountered an unrecoverable error, either during startup, or during execution.
	ContainerNetworkTunnelProxyStateFailed ContainerNetworkTunnelProxyState = "Failed"
)

type TunnelState string

const (
	// Tunnel is ready and accepting connections.
	TunnelStateReady TunnelState = "Ready"

	// Tunnel preparation failed, see ErrorMessage for details.
	TunnelStateFailed TunnelState = "Failed"
)

// TunnelConfiguration defines a single tunnel enabled by a ContainerNetworkTunnelProxy.
// +k8s:openapi-gen=true
type TunnelConfiguration struct {
	// User-friendly name for the tunnel (used in status reporting and debugging).
	// Must be unique within the ContainerNetworkTunnelProxy.
	Name string `json:"name"`

	// Address of the server on the host that clients will be tunneled to.
	// Defaults to "localhost" if not specified.
	// +optional
	ServerAddress string `json:"serverAddress,omitempty"`

	// Port of the server on the host that clients will be tunneled to.
	ServerPort int32 `json:"serverPort"`

	// Address that the client proxy will bind to on the container network
	// Defaults to "0.0.0.0" (all interfaces) if not specified.
	// +optional
	ClientProxyAddress string `json:"clientProxyAddress,omitempty"`

	// Port that the client proxy will use on the container network.
	// If set to 0 or not specified, a random port will be assigned.
	// +optional
	ClientProxyPort int32 `json:"clientProxyPort,omitempty"`
}

// TunnelStatus represents the status of a single tunnel within the proxy pair
// +k8s:openapi-gen=true
type TunnelStatus struct {
	// Name of the tunnel (matches TunnelConfiguration.Name).
	Name string `json:"name"`

	// Internal tunnel ID assigned by the proxy pair.
	TunnelID uint32 `json:"tunnelId,omitempty"`

	// Current state of the tunnel.
	State TunnelState `json:"state"`

	// Human-readable explanation for why the tunnel preparation failed (if it did).
	ErrorMessage string `json:"errorMessage,omitempty"`

	// The timestamp for the status (last update).
	Timestamp metav1.MicroTime `json:"timestamp"`
}

// ContainerNetworkTunnelProxySpec defines the desired state of a ContainerNetworkTunnelProxy.
// +k8s:openapi-gen=true
type ContainerNetworkTunnelProxySpec struct {
	// Reference to the ContainerNetwork that the client proxy should connect to.
	// This field is required and must reference an existing ContainerNetwork resource.
	ContainerNetworkName string `json:"containerNetworkName"`

	// List of tunnels to prepare. Each tunnel enables clients on the container network
	// to connect to a server on the host (establish a tunnel stream).
	// +listType=atomic
	Tunnels []TunnelConfiguration `json:"tunnels,omitempty"`

	// Base container image to use for the client proxy container.
	// Defaults to mcr.microsoft.com/azurelinux/base/core:3.0 if not specified.
	// +optional
	BaseImage string `json:"baseImage,omitempty"`
}

// ContainerNetworkTunnelProxyStatus defines the current state of a ContainerNetworkTunnelProxy.
// +k8s:openapi-gen=true
type ContainerNetworkTunnelProxyStatus struct {
	// Overall state of the tunnel proxy pair.
	// +kubebuilder:default:="Pending"
	State ContainerNetworkTunnelProxyState `json:"state,omitempty"`

	// Status of individual tunnels within the proxy pair.
	// +listType=atomic
	TunnelStatuses []TunnelStatus `json:"tunnelStatuses,omitempty"`

	// Monotonically increasing version number of the tunnel configuration that was applied to the proxy pair.
	// Can be used by clients changing tunnel configuration (Tunnels property) to learn that the new configuration has become effective.
	TunnelConfigurationVersion int32 `json:"tunnelConfigurationVersion,omitempty"`

	// Container ID of the running client proxy container.
	ClientProxyContainerID string `json:"clientProxyContainerId,omitempty"`

	// Server proxy process ID.
	ServerProxyProcessID *int64 `json:"serverProxyProcessId,omitempty"`

	// Server proxy process startup timestamp.
	ServerProxyStartupTimestamp metav1.MicroTime `json:"serverProxyStartupTimestamp,omitempty"`

	// The path of a temporary file that contains captured standard output data from the server proxy process.
	ServerProxyStdOutFile string `json:"serverProxyStdOutFile,omitempty"`

	// The path of a temporary file that contains captured standard error data from the server proxy process.
	ServerProxyStdErrFile string `json:"serverProxyStdErrFile,omitempty"`

	// Published (host) port for client proxy control endpoint.
	ClientProxyControlPort int32 `json:"clientProxyControlPort,omitempty"`

	// Published (host) port for client proxy data endpoint.
	ClientProxyDataPort int32 `json:"clientProxyDataPort,omitempty"`

	// Server proxy control port (for controlling the proxy pair).
	ServerProxyControlPort int32 `json:"serverProxyControlPort,omitempty"`
}

func (s ContainerNetworkTunnelProxyStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	s.DeepCopyInto(&dest.(*ContainerNetworkTunnelProxy).Status)
}

// ContainerNetworkTunnelProxy represents a tunnel proxy pair that handles multiple tunnels
// between a container network and host network.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type ContainerNetworkTunnelProxy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ContainerNetworkTunnelProxySpec   `json:"spec,omitempty"`
	Status ContainerNetworkTunnelProxyStatus `json:"status,omitempty"`
}

func (cntp *ContainerNetworkTunnelProxy) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "containernetworktunnelproxies",
	}
}

func (cntp *ContainerNetworkTunnelProxy) GetObjectMeta() *metav1.ObjectMeta {
	return &cntp.ObjectMeta
}

func (cntp *ContainerNetworkTunnelProxy) GetStatus() apiserver_resource.StatusSubResource {
	return cntp.Status
}

func (cntp *ContainerNetworkTunnelProxy) New() runtime.Object {
	return &ContainerNetworkTunnelProxy{}
}

func (cntp *ContainerNetworkTunnelProxy) NewList() runtime.Object {
	return &ContainerNetworkTunnelProxyList{}
}

func (cntp *ContainerNetworkTunnelProxy) IsStorageVersion() bool {
	return true
}

func (cntp *ContainerNetworkTunnelProxy) NamespaceScoped() bool {
	return false
}

func (cntp *ContainerNetworkTunnelProxy) ShortNames() []string {
	return []string{"cntp"}
}

func (cntp *ContainerNetworkTunnelProxy) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      cntp.Name,
		Namespace: cntp.Namespace,
	}
}

func (cntp *ContainerNetworkTunnelProxy) Validate(ctx context.Context) field.ErrorList {
	errorList := field.ErrorList{}

	if ResourceCreationProhibited.Load() {
		errorList = append(errorList, field.Forbidden(nil, errResourceCreationProhibited.Error()))
	}

	// ContainerNetworkName must be set (not empty)
	if cntp.Spec.ContainerNetworkName == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec", "containerNetworkName"), "Container network name is required"))
	}

	// Check that each tunnel configuration has a unique name and valid server port.
	tunnelNames := make(map[string]bool)

	for i, tunnel := range cntp.Spec.Tunnels {
		if tunnel.Name == "" {
			errorList = append(errorList, field.Required(field.NewPath("spec", "tunnels").Index(i).Child("name"), "Tunnel name is required"))
		} else if tunnelNames[tunnel.Name] {
			errorList = append(errorList, field.Duplicate(field.NewPath("spec", "tunnels"), fmt.Sprintf("Tunnel name '%s' is not unique", tunnel.Name)))
		} else {
			tunnelNames[tunnel.Name] = true
		}

		// (cannot use networking package to validate port number because it depends on this package i.e. api/v1)
		if tunnel.ServerPort <= 0 || tunnel.ServerPort > 65535 {
			errorList = append(errorList, field.Required(field.NewPath("spec", "tunnels").Index(i).Child("serverPort"), fmt.Sprintf("Server port must be between 1 and 65535, got %d", tunnel.ServerPort)))
		}
	}

	return errorList
}

func (cntp *ContainerNetworkTunnelProxy) ValidateUpdate(ctx context.Context, obj runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldProxy := obj.(*ContainerNetworkTunnelProxy)

	// ContainerNetworkName cannot change during object lifetime
	if oldProxy.Spec.ContainerNetworkName != cntp.Spec.ContainerNetworkName {
		errorList = append(errorList, field.Invalid(field.NewPath("spec", "containerNetworkName"), cntp.Spec.ContainerNetworkName, "Container network name cannot be changed after ContainerNetworkTunnelProxy is created"))
	}

	// BaseImage, if set, cannot change during object lifetime
	if oldProxy.Spec.BaseImage != cntp.Spec.BaseImage {
		errorList = append(errorList, field.Invalid(field.NewPath("spec", "baseImage"), cntp.Spec.BaseImage, "Base image cannot be changed after ContainerNetworkTunnelProxy is created"))
	}

	return errorList
}

// ContainerNetworkTunnelProxyList contains a list of ContainerNetworkTunnelProxy instances
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type ContainerNetworkTunnelProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContainerNetworkTunnelProxy `json:"items"`
}

func (cntpl *ContainerNetworkTunnelProxyList) GetListMeta() *metav1.ListMeta {
	return &cntpl.ListMeta
}

func (cntpl *ContainerNetworkTunnelProxyList) ItemCount() uint32 {
	return uint32(len(cntpl.Items))
}

func (cntpl *ContainerNetworkTunnelProxyList) GetItems() []*ContainerNetworkTunnelProxy {
	retval := make([]*ContainerNetworkTunnelProxy, len(cntpl.Items))
	for i := range cntpl.Items {
		retval[i] = &cntpl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&ContainerNetworkTunnelProxy{}, &ContainerNetworkTunnelProxyList{})
}

// Ensure types support interfaces expected by our API server
var _ apiserver_resource.Object = (*ContainerNetworkTunnelProxy)(nil)
var _ apiserver_resource.ObjectList = (*ContainerNetworkTunnelProxyList)(nil)
var _ commonapi.ListWithObjectItems[ContainerNetworkTunnelProxy, *ContainerNetworkTunnelProxy] = (*ContainerNetworkTunnelProxyList)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*ContainerNetworkTunnelProxy)(nil)
var _ apiserver_resource.StatusSubResource = (*ContainerNetworkTunnelProxyStatus)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*ContainerNetworkTunnelProxy)(nil)
var _ apiserver_resourcestrategy.Validater = (*ContainerNetworkTunnelProxy)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*ContainerNetworkTunnelProxy)(nil)
