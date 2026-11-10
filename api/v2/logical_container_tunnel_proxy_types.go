/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	apiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/dcp/pkg/commonapi"
)

type LogicalContainerTunnelProxyPhase string

const (
	LogicalContainerTunnelProxyPhasePending LogicalContainerTunnelProxyPhase = "Pending"
	LogicalContainerTunnelProxyPhaseRunning LogicalContainerTunnelProxyPhase = "Running"
	LogicalContainerTunnelProxyPhaseExited  LogicalContainerTunnelProxyPhase = "Exited"
	LogicalContainerTunnelProxyPhaseUnknown LogicalContainerTunnelProxyPhase = "Unknown"
	LogicalContainerTunnelProxyPhaseFailed  LogicalContainerTunnelProxyPhase = "Failed"

	// LogicalContainerTunnelProxyReasonStarting qualifies Pending while the proxy pair starts.
	LogicalContainerTunnelProxyReasonStarting ConditionReason = "Starting"
	// LogicalContainerTunnelProxyReasonRunning qualifies Running when the proxy pair is available.
	LogicalContainerTunnelProxyReasonRunning ConditionReason = "Running"
	// LogicalContainerTunnelProxyReasonPaused qualifies Running while the client container is paused.
	LogicalContainerTunnelProxyReasonPaused ConditionReason = "Paused"
	// LogicalContainerTunnelProxyReasonStopping qualifies Running while DCP stops the proxy pair.
	LogicalContainerTunnelProxyReasonStopping ConditionReason = "Stopping"
	// LogicalContainerTunnelProxyReasonCompleted qualifies Exited when a backing exited on its own.
	LogicalContainerTunnelProxyReasonCompleted ConditionReason = "Completed"
	// LogicalContainerTunnelProxyReasonStopped qualifies Exited when DCP terminated the proxy pair.
	LogicalContainerTunnelProxyReasonStopped ConditionReason = "Stopped"
	// LogicalContainerTunnelProxyReasonFailedToStart qualifies Failed when the proxy pair never reached Running.
	LogicalContainerTunnelProxyReasonFailedToStart ConditionReason = "FailedToStart"
	// LogicalContainerTunnelProxyReasonMissing qualifies Failed when a previously running backing is gone.
	LogicalContainerTunnelProxyReasonMissing ConditionReason = "Missing"
	// LogicalContainerTunnelProxyReasonLookupFailed qualifies Unknown when current state cannot be confirmed.
	LogicalContainerTunnelProxyReasonLookupFailed ConditionReason = "LookupFailed"
)

type LogicalContainerTunnelState string

const (
	LogicalContainerTunnelStateReady    LogicalContainerTunnelState = "Ready"
	LogicalContainerTunnelStateNotReady LogicalContainerTunnelState = "NotReady"
	LogicalContainerTunnelStateFailed   LogicalContainerTunnelState = "Failed"
)

// LogicalContainerTunnel connects a container-network listener to a host service.
// +k8s:openapi-gen=true
type LogicalContainerTunnel struct {
	// Name uniquely identifies the tunnel within the proxy.
	Name string `json:"name"`
	// ServerServiceRef identifies the host LogicalService using <name> or <namespace>/<name>.
	// Cross-namespace references are not supported. Only TCP services are supported.
	ServerServiceRef string `json:"serverServiceRef"`
	// ClientServiceRef identifies the Proxyless LogicalService published on the container network.
	// Uses <name> or <namespace>/<name>; cross-namespace references are not supported.
	ClientServiceRef string `json:"clientServiceRef"`
}

// LogicalContainerTunnelStatus describes one container-to-host tunnel.
// +k8s:openapi-gen=true
type LogicalContainerTunnelStatus struct {
	// Name matches the tunnel declaration.
	Name string `json:"name"`
	// TunnelID identifies the prepared tunnel in dcptun.
	TunnelID uint32 `json:"tunnelID,omitempty"`
	// State describes listener availability.
	State LogicalContainerTunnelState `json:"state"`
	// ErrorMessage describes terminal preparation failure.
	ErrorMessage string `json:"errorMessage,omitempty"`
	// ClientProxyAddresses are the listener addresses inside the container network.
	// +listType=set
	ClientProxyAddresses []string `json:"clientProxyAddresses,omitempty"`
	// ClientProxyPort is the listener port inside the container network.
	ClientProxyPort int32 `json:"clientProxyPort,omitempty"`
	// Timestamp records the last tunnel status change.
	Timestamp metav1.MicroTime `json:"timestamp"`
}

// LogicalContainerTunnelProxySpec declares a proxy pair and its mutable tunnel collection.
// +k8s:openapi-gen=true
type LogicalContainerTunnelProxySpec struct {
	// NetworkRef identifies a LogicalContainerNetwork using <name> or <namespace>/<name>.
	// Cross-namespace references are not supported.
	NetworkRef string `json:"networkRef"`
	// Aliases are DNS names for the client proxy on the container network.
	// +listType=set
	Aliases []string `json:"aliases,omitempty"`
	// BaseImage overrides the client proxy's base image, defaulting to mcr.microsoft.com/azurelinux/base/core:3.0.
	BaseImage string `json:"baseImage,omitempty"`
	// Tunnels can be added or removed; an existing tunnel definition cannot be changed.
	// +listType=atomic
	Tunnels []LogicalContainerTunnel `json:"tunnels,omitempty"`
}

// LogicalContainerTunnelProxyStatus describes the proxy pair and its tunnels.
// +k8s:openapi-gen=true
type LogicalContainerTunnelProxyStatus struct {
	// Phase summarizes proxy-pair availability.
	// +kubebuilder:validation:Enum=Pending;Running;Exited;Unknown;Failed
	Phase LogicalContainerTunnelProxyPhase `json:"phase,omitempty"`
	// PhysicalContainerRef identifies the same-namespace client PhysicalContainer.
	PhysicalContainerRef string `json:"physicalContainerRef,omitempty"`
	// PhysicalImageRef identifies the same-namespace client PhysicalContainerImage.
	PhysicalImageRef string `json:"physicalImageRef,omitempty"`
	// PhysicalProcessRef identifies the same-namespace server PhysicalProcess.
	PhysicalProcessRef string `json:"physicalProcessRef,omitempty"`
	// ContainerID identifies the observed client runtime container.
	ContainerID string `json:"containerID,omitempty"`
	// Image is the observed client image reference.
	Image string `json:"image,omitempty"`
	// PID identifies the observed host server process.
	PID *int64 `json:"pid,omitempty"`
	// TunnelStatuses describes the tunnels currently managed by this proxy.
	// +listType=atomic
	TunnelStatuses []LogicalContainerTunnelStatus `json:"tunnelStatuses,omitempty"`
	// TunnelConfigurationVersion increases when the observed tunnel configuration changes.
	TunnelConfigurationVersion int32 `json:"tunnelConfigurationVersion,omitempty"`
	// Conditions report readiness and reconciliation diagnostics.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func (status LogicalContainerTunnelProxyStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	status.DeepCopyInto(&dest.(*LogicalContainerTunnelProxy).Status)
}

// LogicalContainerTunnelProxy forwards connections from a container network to host services.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=logicalcontainertunnelproxies,shortName=lctp
type LogicalContainerTunnelProxy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              LogicalContainerTunnelProxySpec   `json:"spec,omitempty"`
	Status            LogicalContainerTunnelProxyStatus `json:"status,omitempty"`
}

func (*LogicalContainerTunnelProxy) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupVersion.Group, Version: GroupVersion.Version, Resource: "logicalcontainertunnelproxies"}
}
func (proxy *LogicalContainerTunnelProxy) GetObjectMeta() *metav1.ObjectMeta {
	return &proxy.ObjectMeta
}
func (proxy *LogicalContainerTunnelProxy) GetStatus() apiserver_resource.StatusSubResource {
	return proxy.Status
}
func (*LogicalContainerTunnelProxy) New() runtime.Object { return &LogicalContainerTunnelProxy{} }
func (*LogicalContainerTunnelProxy) NewList() runtime.Object {
	return &LogicalContainerTunnelProxyList{}
}
func (*LogicalContainerTunnelProxy) IsStorageVersion() bool { return true }
func (*LogicalContainerTunnelProxy) NamespaceScoped() bool  { return true }
func (*LogicalContainerTunnelProxy) ShortNames() []string   { return []string{"lctp"} }
func (proxy *LogicalContainerTunnelProxy) NamespacedName() types.NamespacedName {
	return NamespacedName(proxy)
}
func (proxy *LogicalContainerTunnelProxy) GetResourceId() string {
	return fmt.Sprintf("logicalcontainertunnelproxy-%s", proxy.UID)
}

func (proxy *LogicalContainerTunnelProxy) validateSpec() field.ErrorList {
	specPath := field.NewPath("spec")
	errors := ValidateSameNamespaceResourceReference(proxy.Spec.NetworkRef, proxy.Namespace, specPath.Child("networkRef"))
	if strings.ContainsAny(proxy.Spec.BaseImage, " \t\r\n") {
		errors = append(errors, field.Invalid(specPath.Child("baseImage"), proxy.Spec.BaseImage, "baseImage must be a single image reference"))
	}
	names := make(map[string]bool)
	for index, tunnel := range proxy.Spec.Tunnels {
		path := specPath.Child("tunnels").Index(index)
		if problems := validation.IsDNS1123Label(tunnel.Name); len(problems) != 0 {
			errors = append(errors, field.Invalid(path.Child("name"), tunnel.Name, strings.Join(problems, "; ")))
		}
		if names[tunnel.Name] {
			errors = append(errors, field.Duplicate(path.Child("name"), tunnel.Name))
		}
		names[tunnel.Name] = true
		errors = append(errors, ValidateSameNamespaceResourceReference(tunnel.ServerServiceRef, proxy.Namespace, path.Child("serverServiceRef"))...)
		errors = append(errors, ValidateSameNamespaceResourceReference(tunnel.ClientServiceRef, proxy.Namespace, path.Child("clientServiceRef"))...)
	}
	return errors
}

func (proxy *LogicalContainerTunnelProxy) Validate(context.Context) field.ErrorList {
	errors := ValidateNamespacedResourceMetadata(proxy)
	if commonapi.ResourceCreationProhibited.Load() && proxy.DeletionTimestamp.IsZero() {
		errors = append(errors, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}
	errors = append(errors, commonapi.ValidateAnnotationsSize(proxy.Annotations, field.NewPath("metadata", "annotations"))...)
	return append(errors, proxy.validateSpec()...)
}

func (proxy *LogicalContainerTunnelProxy) ValidateUpdate(_ context.Context, old runtime.Object) field.ErrorList {
	previous := old.(*LogicalContainerTunnelProxy)
	errors := proxy.validateSpec()
	if previous.Spec.NetworkRef != proxy.Spec.NetworkRef || previous.Spec.BaseImage != proxy.Spec.BaseImage ||
		!slices.Equal(slices.Sorted(slices.Values(previous.Spec.Aliases)), slices.Sorted(slices.Values(proxy.Spec.Aliases))) {
		errors = append(errors, field.Forbidden(field.NewPath("spec"), "only tunnels may be added or removed"))
	}
	for _, tunnel := range proxy.Spec.Tunnels {
		for _, original := range previous.Spec.Tunnels {
			if original.Name == tunnel.Name && !reflect.DeepEqual(original, tunnel) {
				errors = append(errors, field.Forbidden(field.NewPath("spec", "tunnels"), "existing tunnel definitions are immutable"))
			}
		}
	}
	return errors
}

func (*LogicalContainerTunnelProxy) GenericSubResources() []apiserver_resource.GenericSubResource {
	return []apiserver_resource.GenericSubResource{&LogicalContainerTunnelProxyLogResource{}}
}

// LogicalContainerTunnelProxyLogResource registers system diagnostics.
// +k8s:openapi-gen=false
// +kubebuilder:object:generate=false
type LogicalContainerTunnelProxyLogResource struct{}

func (*LogicalContainerTunnelProxyLogResource) Name() string { return LogSubresourceName }
func (*LogicalContainerTunnelProxyLogResource) GetStorageProvider(obj apiserver_resource.Object, _ string, provider apiserver.StorageProvider) apiserver.StorageProvider {
	return newLogStorageProvider(obj, provider)
}

// LogicalContainerTunnelProxyList contains logical container tunnel proxies.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type LogicalContainerTunnelProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LogicalContainerTunnelProxy `json:"items"`
}

func (list *LogicalContainerTunnelProxyList) GetListMeta() *metav1.ListMeta { return &list.ListMeta }
func (list *LogicalContainerTunnelProxyList) ItemCount() uint32             { return uint32(len(list.Items)) }
func (list *LogicalContainerTunnelProxyList) GetItems() []*LogicalContainerTunnelProxy {
	items := make([]*LogicalContainerTunnelProxy, len(list.Items))
	for index := range list.Items {
		items[index] = &list.Items[index]
	}
	return items
}
func init() {
	SchemeBuilder.Register(&LogicalContainerTunnelProxy{}, &LogicalContainerTunnelProxyList{})
}

var _ apiserver_resource.Object = (*LogicalContainerTunnelProxy)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*LogicalContainerTunnelProxy)(nil)
var _ apiserver_resource.StatusSubResource = (*LogicalContainerTunnelProxyStatus)(nil)
var _ apiserver_resource.ObjectList = (*LogicalContainerTunnelProxyList)(nil)
var _ commonapi.ListWithObjectItems[LogicalContainerTunnelProxy, *LogicalContainerTunnelProxy] = (*LogicalContainerTunnelProxyList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*LogicalContainerTunnelProxy)(nil)
var _ apiserver_resourcestrategy.Validater = (*LogicalContainerTunnelProxy)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*LogicalContainerTunnelProxy)(nil)
