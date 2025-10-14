// Copyright (c) Microsoft Corporation. All rights reserved.

package v1

import (
	"context"
	"fmt"
	std_slices "slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
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

	// Initial state -- no attempt to prepare the tunnel have been made yet.
	TunnelStateEmpty TunnelState = ""

	// Tunnel is being prepared, or is waiting for required services to become ready.
	TunnelStateNotReady TunnelState = "NotReady"
)

// TunnelConfiguration defines a single tunnel enabled by a ContainerNetworkTunnelProxy.
// +k8s:openapi-gen=true
type TunnelConfiguration struct {
	// User-friendly name for the tunnel (used in status reporting and debugging).
	// Must be unique within the ContainerNetworkTunnelProxy.
	Name string `json:"name"`

	// Namespace of the Service that identifies the server the tunnel connects to.
	// +optional
	ServerServiceNamespace string `json:"serverServiceNamespace,omitempty"`

	// Name of the Service that identifies the server the tunnel connects to.
	ServerServiceName string `json:"serverServiceName"`

	// Namespace of the Service associated with the client proxy on the container network.
	// +optional
	ClientServiceNamespace string `json:"clientServiceNamespace,omitempty"`

	// Name of the Service associated with the client proxy on the container network.
	ClientServiceName string `json:"clientServiceName"`
}

// Converts a TunnelConfiguration to key-value pair for use in maps.
func (tc TunnelConfiguration) KV() (string, TunnelConfiguration) { return tc.Name, tc }

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

	// Addresses on the container network that client proxy is listening on for this tunnel.
	// May be empty if the tunnel is not ready.
	// +listType=set
	ClientProxyAddresses []string `json:"clientProxyAddresses,omitempty"`

	// Port on the container network that client proxy is listening on for this tunnel.
	// May be zero if the tunnel is not ready.
	ClientProxyPort int32 `json:"clientProxyPort,omitempty"`

	// The timestamp for the status (last update).
	Timestamp metav1.MicroTime `json:"timestamp"`
}

func (ts TunnelStatus) Equal(other TunnelStatus) bool {
	allmostEqual := ts.Name == other.Name &&
		ts.TunnelID == other.TunnelID &&
		ts.State == other.State &&
		ts.ErrorMessage == other.ErrorMessage &&
		osutil.MicroEqual(ts.Timestamp, other.Timestamp) &&
		ts.ClientProxyPort == other.ClientProxyPort
	if !allmostEqual {
		return false
	}

	std_slices.Sort(ts.ClientProxyAddresses)
	std_slices.Sort(other.ClientProxyAddresses)
	return std_slices.Equal(ts.ClientProxyAddresses, other.ClientProxyAddresses)
}

func (ts TunnelStatus) Clone() TunnelStatus {
	retval := ts
	retval.ClientProxyAddresses = std_slices.Clone(ts.ClientProxyAddresses)
	return retval
}

func (ts TunnelStatus) KV() (string, TunnelStatus) { return ts.Name, ts }

// ContainerNetworkTunnelProxySpec defines the desired state of a ContainerNetworkTunnelProxy.
// +k8s:openapi-gen=true
type ContainerNetworkTunnelProxySpec struct {
	// Reference to the ContainerNetwork that the client proxy should connect to.
	// This field is required and must reference an existing ContainerNetwork resource.
	ContainerNetworkName string `json:"containerNetworkName"`

	// Aliases (DNS names) that can be used to reach the client proxy container on the container network.
	// +listType=set
	Aliases []string `json:"aliases,omitempty"`

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

	// The name and tag of the container image used for the client proxy container.
	ClientProxyContainerImage string `json:"clientProxyContainerImage,omitempty"`

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

	if ResourceCreationProhibited.Load() && cntp.ObjectMeta.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, errResourceCreationProhibited.Error()))
	}

	// ContainerNetworkName must be set (not empty)
	if cntp.Spec.ContainerNetworkName == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec", "containerNetworkName"), "Container network name is required"))
	}

	// Check that each tunnel configuration has a unique name.
	tunnelNames := make(map[string]bool)

	for i, tunnel := range cntp.Spec.Tunnels {
		if tunnel.Name == "" {
			errorList = append(errorList, field.Required(field.NewPath("spec", "tunnels").Index(i).Child("name"), "Tunnel name is required"))
		} else if tunnelNames[tunnel.Name] {
			errorList = append(errorList, field.Duplicate(field.NewPath("spec", "tunnels"), fmt.Sprintf("Tunnel name '%s' is not unique", tunnel.Name)))
		} else {
			tunnelNames[tunnel.Name] = true
		}

		if tunnel.ServerServiceName == "" {
			errorList = append(errorList, field.Required(field.NewPath("spec", "tunnels").Index(i).Child("serverServiceName"), "Server service name is required"))
		}

		if tunnel.ClientServiceName == "" {
			errorList = append(errorList, field.Required(field.NewPath("spec", "tunnels").Index(i).Child("clientServiceName"), "Client service name is required"))
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

	// Aliases cannot be changed during object lifetime
	oldAliases := std_slices.Sorted(std_slices.Values(oldProxy.Spec.Aliases))
	newAliases := std_slices.Sorted(std_slices.Values(cntp.Spec.Aliases))
	if !std_slices.Equal(oldAliases, newAliases) {
		errorList = append(errorList, field.Invalid(field.NewPath("spec", "aliases"), cntp.Spec.Aliases, "Aliases cannot be changed after ContainerNetworkTunnelProxy is created"))
	}

	// New tunnels can be added and existing tunnels can be removed,
	// but existing tunnels cannot be re-defined.
	oldTunnelMap := maps.SliceToMap(oldProxy.Spec.Tunnels, TunnelConfiguration.KV)
	newTunnelMap := maps.SliceToMap(cntp.Spec.Tunnels, TunnelConfiguration.KV)
	preservedTunnelNames := slices.Intersect(maps.Keys(newTunnelMap), maps.Keys(oldTunnelMap))
	redefinedTunnelNames := slices.Select(preservedTunnelNames, func(name string) bool {
		return oldTunnelMap[name] != newTunnelMap[name] // All-property comparison
	})
	for i, tc := range cntp.Spec.Tunnels {
		if slices.Contains(redefinedTunnelNames, tc.Name) {
			errorList = append(errorList, field.Invalid(field.NewPath("spec", "tunnels").Index(i), tc, "Tunnel configuration cannot be changed after the tunnel is created"))
		}
	}

	errorList = append(errorList, cntp.Validate(ctx)...)

	return errorList
}

func (cntp *ContainerNetworkTunnelProxy) ServicesProduced() []commonapi.ServiceProducer {
	var retval []commonapi.ServiceProducer
	tunnelMap := maps.SliceToMap(cntp.Spec.Tunnels, TunnelConfiguration.KV)

	for _, ts := range cntp.Status.TunnelStatuses {
		if ts.State != TunnelStateReady {
			continue
		}

		tc, found := tunnelMap[ts.Name]
		if !found {
			// Should not really happen (having a status for a tunnel that is not in the spec???)
			continue
		}

		for _, addr := range ts.ClientProxyAddresses {
			retval = append(retval, commonapi.ServiceProducer{
				ServiceName:      tc.ClientServiceName,
				ServiceNamespace: tc.ClientServiceNamespace,
				Port:             ts.ClientProxyPort,
				Address:          addr,
			})
		}
	}

	return retval
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
var _ commonapi.DynamicServiceProducer = (*ContainerNetworkTunnelProxy)(nil)
