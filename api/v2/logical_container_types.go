/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"cmp"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	apiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// LogicalContainerMode controls container creation, reuse, and retention.
// +kubebuilder:validation:Enum=session;persistent;existing
type LogicalContainerMode string

const (
	// LogicalContainerModeSession creates a container and removes it when the resource is deleted.
	LogicalContainerModeSession LogicalContainerMode = "session"

	// LogicalContainerModePersistent creates or reuses a container and retains it when the resource is deleted.
	LogicalContainerModePersistent LogicalContainerMode = "persistent"

	// LogicalContainerModeExisting observes an existing container without creating or removing it.
	LogicalContainerModeExisting LogicalContainerMode = "existing"
)

// ShouldReuseExisting indicates whether the mode permits adopting an existing runtime container.
func (mode LogicalContainerMode) ShouldReuseExisting() bool {
	switch mode {
	case LogicalContainerModePersistent, LogicalContainerModeExisting:
		return true
	default:
		return false
	}
}

// ShouldCreateIfMissing indicates whether the mode permits creating a runtime container.
func (mode LogicalContainerMode) ShouldCreateIfMissing() bool {
	switch mode {
	case LogicalContainerModeSession, LogicalContainerModePersistent:
		return true
	default:
		return false
	}
}

// LogicalContainerPhase summarizes the lifecycle of a logical container.
type LogicalContainerPhase string

const (
	// LogicalContainerPhasePending indicates that the container is waiting for prerequisites or startup.
	LogicalContainerPhasePending LogicalContainerPhase = "Pending"

	// LogicalContainerPhaseRunning indicates that the container is running.
	LogicalContainerPhaseRunning LogicalContainerPhase = "Running"

	// LogicalContainerPhasePaused indicates that the container is paused.
	LogicalContainerPhasePaused LogicalContainerPhase = "Paused"

	// LogicalContainerPhaseStopping indicates that stop is requested and has not completed.
	LogicalContainerPhaseStopping LogicalContainerPhase = "Stopping"

	// LogicalContainerPhaseExited indicates that the container has exited.
	LogicalContainerPhaseExited LogicalContainerPhase = "Exited"

	// LogicalContainerPhaseUnknown indicates that the container's state is unavailable or indeterminate.
	LogicalContainerPhaseUnknown LogicalContainerPhase = "Unknown"

	// LogicalContainerPhaseFailed indicates a terminal reconciliation failure.
	LogicalContainerPhaseFailed LogicalContainerPhase = "Failed"
)

// LogicalContainerSpec describes logical container lifecycle policy and desired configuration.
// +k8s:openapi-gen=true
type LogicalContainerSpec struct {
	// ContainerName is the runtime name to create or look up. Required for persistent and existing modes.
	// If omitted in session mode, a runtime name is generated.
	ContainerName string `json:"containerName,omitempty"`

	// Mode controls creation, reuse, and retention. Defaults to session.
	// +kubebuilder:default:=session
	Mode LogicalContainerMode `json:"mode,omitempty"`

	// LifecycleKey identifies the desired configuration for persistent container reuse.
	// If omitted, the controller calculates a key from the desired configuration.
	LifecycleKey string `json:"lifecycleKey,omitempty"`

	// Container describes the desired container. Required for session and persistent modes,
	// and forbidden for existing mode.
	Container *LogicalContainerConfig `json:"container,omitempty"`

	// MonitorPID optionally scopes a persistent container to another process lifetime.
	// When set, monitorTimestamp must also be set. The container is stopped but not removed when that process exits.
	MonitorPID *int64 `json:"monitorPID,omitempty"`

	// MonitorTimestamp identifies the process in monitorPID and guards against PID reuse.
	MonitorTimestamp metav1.MicroTime `json:"monitorTimestamp,omitempty"`

	// Stop requests that the container be stopped. Once true, it cannot be reset.
	Stop bool `json:"stop,omitempty"`
}

// EffectiveMode returns the configured lifecycle mode, defaulting to session.
func (cs LogicalContainerSpec) EffectiveMode() LogicalContainerMode {
	if cs.Mode == "" {
		return LogicalContainerModeSession
	}
	return cs.Mode
}

// GetLifecycleKey returns the explicit lifecycle key, or computes one from the desired configuration.
// The second result is true when the key is computed. Build-content identity comes only from the supplied build digest.
func (cs *LogicalContainerSpec) GetLifecycleKey() (string, bool, error) {
	if cs.LifecycleKey != "" {
		return cs.LifecycleKey, false, nil
	}
	if cs.Container == nil {
		return "", false, errors.New("container configuration is required to compute a lifecycle key")
	}

	desired := cs.Container.DeepCopy()
	// Network attachments can change without recreating the container.
	desired.Networks = nil
	if desired.ImageRef != "" {
		desired.ImageRef = commonapi.AsNamespacedName(desired.ImageRef, "").Name
	}
	if desired.Image != nil {
		desired.Image.PullRetryLimit = nil
		if desired.Image.PullPolicy == "" {
			desired.Image.PullPolicy = PullPolicyMissing
		}
		if desired.Image.Build != nil {
			if desired.Image.BuildPolicy == "" {
				desired.Image.BuildPolicy = BuildPolicyIfNeeded
			}
			build := desired.Image.Build
			// Archive transport and bytes do not substitute for a caller-supplied content digest.
			build.ContextArchive = nil
			slices.Sort(build.Tags)
			build.Tags = slices.Compact(build.Tags)
			slices.Sort(build.BaseImages)
			build.BaseImages = slices.Compact(build.BaseImages)
			slices.SortStableFunc(build.Labels, func(left, right commonapi.Label) int { return strings.Compare(left.Key, right.Key) })
			slices.SortStableFunc(build.Secrets, func(left, right ContainerBuildSecret) int { return strings.Compare(left.ID, right.ID) })
		}
	}
	slices.SortStableFunc(desired.Env, func(left, right commonapi.EnvVar) int { return strings.Compare(left.Name, right.Name) })
	slices.SortStableFunc(desired.Labels, func(left, right commonapi.Label) int { return strings.Compare(left.Key, right.Key) })
	for i := range desired.VolumeMounts {
		if desired.VolumeMounts[i].VolumeRef != "" {
			desired.VolumeMounts[i].VolumeRef = commonapi.AsNamespacedName(desired.VolumeMounts[i].VolumeRef, "").Name
		}
	}
	slices.SortStableFunc(desired.VolumeMounts, func(left, right VolumeMount) int { return strings.Compare(left.Target, right.Target) })
	slices.SortStableFunc(desired.CreateFiles, func(left, right CreateFileSystem) int {
		return strings.Compare(left.Destination, right.Destination)
	})
	for i := range desired.Ports {
		desired.Ports[i].RangeSize = desired.Ports[i].EffectiveRangeSize()
		if desired.Ports[i].Protocol == "" {
			desired.Ports[i].Protocol = commonapi.PortProtocolTCP
		}
	}
	slices.SortStableFunc(desired.Ports, func(left, right ContainerPort) int {
		if protocolOrder := strings.Compare(string(left.Protocol), string(right.Protocol)); protocolOrder != 0 {
			return protocolOrder
		}
		return cmp.Compare(left.HostPort, right.HostPort)
	})

	fnvHash := fnv.New128()
	encoder := gob.NewEncoder(fnvHash)
	hashErr := encoder.Encode(desired)
	if cs.MonitorPID != nil {
		hashErr = errors.Join(hashErr, encoder.Encode(*cs.MonitorPID), encoder.Encode(cs.MonitorTimestamp.Time))
	}
	return fmt.Sprintf("%x", fnvHash.Sum(nil)), true, hashErr
}

// LogicalContainerConfig describes the desired configuration used to create a container.
// +k8s:openapi-gen=true
type LogicalContainerConfig struct {
	// ImageRef identifies a PhysicalContainerImage using <name> or <namespace>/<name>.
	// Cross-namespace references are not supported. Exactly one of imageRef or image must be set.
	ImageRef string `json:"imageRef,omitempty"`

	// Image describes an image to pull or build for this container.
	// Exactly one of imageRef or image must be set.
	Image *PhysicalContainerImageConfig `json:"image,omitempty"`

	// Entrypoint is the container runtime entrypoint to run.
	Entrypoint string `json:"entrypoint,omitempty"`

	// Command contains arguments passed to the container entrypoint.
	// +listType=atomic
	Command []string `json:"command,omitempty"`

	// Env contains environment variables to set in the container.
	// +listType=map
	// +listMapKey=name
	Env []commonapi.EnvVar `json:"env,omitempty"`

	// Ports describes ports to expose from the container.
	// +listType=atomic
	Ports []ContainerPort `json:"ports,omitempty"`

	// VolumeMounts describes volume and bind mounts for the container.
	// +listType=atomic
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`

	// Networks describes LogicalContainerNetworks to attach the container to.
	// If omitted, the container runtime chooses the default network.
	// +listType=atomic
	Networks []LogicalContainerNetworkConnectionConfig `json:"networks,omitempty"`

	// CreateFiles describes files and folders to copy into the container before it starts.
	// +listType=atomic
	CreateFiles []CreateFileSystem `json:"createFiles,omitempty"`

	// Labels contains labels to apply to a newly-created runtime container.
	// DCP lifecycle and physical resource ownership labels are reserved and set by controllers.
	// +listType=map
	// +listMapKey=key
	Labels []commonapi.Label `json:"labels,omitempty"`
}

// LogicalContainerNetworkConnectionConfig describes a logical network attachment for a container.
// +k8s:openapi-gen=true
type LogicalContainerNetworkConnectionConfig struct {
	// NetworkRef identifies a LogicalContainerNetwork using <name> or <namespace>/<name>.
	// Cross-namespace references are not supported.
	NetworkRef string `json:"networkRef"`

	// Aliases contains network-scoped aliases for the container.
	// +listType=set
	// +optional
	Aliases []string `json:"aliases,omitempty"`
}

// LogicalContainerStatus describes the observed logical container and its physical dependencies.
// +k8s:openapi-gen=true
type LogicalContainerStatus struct {
	// StartupLogs identifies output from the physical image dependency.
	StartupLogs *LogSource `json:"startupLogs,omitempty"`

	// RuntimeLogs identifies output from the backing physical container.
	RuntimeLogs *LogSource `json:"runtimeLogs,omitempty"`

	// Phase summarizes the container lifecycle.
	// +kubebuilder:validation:Enum=Pending;Running;Paused;Stopping;Exited;Unknown;Failed
	// +optional
	Phase LogicalContainerPhase `json:"phase,omitempty"`

	// PhysicalContainerRef identifies the backing PhysicalContainer using <name> or <namespace>/<name>.
	// Cross-namespace references are not supported.
	PhysicalContainerRef string `json:"physicalContainerRef,omitempty"`

	// ImageRef identifies the supplied or generated PhysicalContainerImage using <name> or <namespace>/<name>.
	// Cross-namespace references are not supported.
	ImageRef string `json:"imageRef,omitempty"`

	// ContainerID is the observed runtime container ID.
	ContainerID string `json:"containerID,omitempty"`

	// ContainerName is the observed runtime container name.
	ContainerName string `json:"containerName,omitempty"`

	// Image is the resolved runtime image reference.
	Image string `json:"image,omitempty"`

	// LifecycleKey is the supplied or calculated key applied to the runtime container.
	LifecycleKey string `json:"lifecycleKey,omitempty"`

	// CreatedAt is the runtime container creation timestamp.
	CreatedAt metav1.MicroTime `json:"createdAt,omitempty"`

	// StartedAt is the runtime container start timestamp.
	StartedAt metav1.MicroTime `json:"startedAt,omitempty"`

	// FinishedAt is the runtime container finish timestamp.
	FinishedAt metav1.MicroTime `json:"finishedAt,omitempty"`

	// ExitCode is the runtime container exit code, when available.
	ExitCode *int32 `json:"exitCode,omitempty"`

	// PortMappings are the observed host bindings for runtime container ports.
	// +listType=atomic
	PortMappings []PhysicalContainerPortMapping `json:"portMappings,omitempty"`

	// Conditions describe readiness and reconciliation progress.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func (cs LogicalContainerStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	cs.DeepCopyInto(&dest.(*LogicalContainer).Status)
}

// LogicalContainer represents logical container lifecycle policy in a DCP V2 namespace.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced,path=logicalcontainers,shortName=lctr
type LogicalContainer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LogicalContainerSpec   `json:"spec,omitempty"`
	Status LogicalContainerStatus `json:"status,omitempty"`
}

func (c *LogicalContainer) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupVersion.Group, Version: GroupVersion.Version, Resource: "logicalcontainers"}
}

// GetResourceId identifies the resource-specific system log stream.
func (c *LogicalContainer) GetResourceId() string {
	return fmt.Sprintf("logicalcontainer-%s", c.UID)
}

// GetLogSource describes the runtime capture observed for the backing physical container.
func (c *LogicalContainer) GetLogSource() *LogSource {
	return c.Status.RuntimeLogs
}

func (*LogicalContainer) GenericSubResources() []apiserver_resource.GenericSubResource {
	return []apiserver_resource.GenericSubResource{&LogicalContainerLogResource{}}
}

// LogicalContainerLogResource registers the logical container log subresource.
// +k8s:openapi-gen=false
// +kubebuilder:object:generate=false
type LogicalContainerLogResource struct{}

func (*LogicalContainerLogResource) Name() string { return LogSubresourceName }

func (*LogicalContainerLogResource) GetStorageProvider(
	obj apiserver_resource.Object, _ string, parentProvider apiserver.StorageProvider,
) apiserver.StorageProvider {
	return newLogStorageProvider(obj, parentProvider)
}

// GetLeaseKey identifies the runtime container shared by logical resources and V1 containers.
func (c *LogicalContainer) GetLeaseKey() string {
	return "containers/" + strings.TrimSpace(c.Spec.ContainerName)
}

func (c *LogicalContainer) GetObjectMeta() *metav1.ObjectMeta {
	return &c.ObjectMeta
}

func (c *LogicalContainer) GetStatus() apiserver_resource.StatusSubResource {
	return c.Status
}

func (c *LogicalContainer) New() runtime.Object {
	return &LogicalContainer{}
}

func (c *LogicalContainer) NewList() runtime.Object {
	return &LogicalContainerList{}
}

func (c *LogicalContainer) IsStorageVersion() bool {
	return true
}

func (c *LogicalContainer) NamespaceScoped() bool {
	return true
}

func (c *LogicalContainer) ShortNames() []string {
	return []string{"lctr"}
}

func (c *LogicalContainer) NamespacedName() types.NamespacedName {
	return NamespacedName(c)
}

func (c *LogicalContainer) Validate(ctx context.Context) field.ErrorList {
	errorList := ValidateNamespacedResourceMetadata(c)
	specPath := field.NewPath("spec")
	if commonapi.ResourceCreationProhibited.Load() && c.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, commonapi.ErrResourceCreationProhibited.Error()))
	}
	errorList = append(errorList, commonapi.ValidateAnnotationsSize(c.Annotations, field.NewPath("metadata", "annotations"))...)

	mode := c.Spec.EffectiveMode()
	switch mode {
	case LogicalContainerModeSession, LogicalContainerModePersistent:
		if c.Spec.Container == nil {
			errorList = append(errorList, field.Required(specPath.Child("container"), "container must be set for session and persistent containers"))
		}
	case LogicalContainerModeExisting:
		if c.Spec.Container != nil {
			errorList = append(errorList, field.Forbidden(specPath.Child("container"), "container cannot be set for existing containers"))
		}
	default:
		errorList = append(errorList, field.NotSupported(specPath.Child("mode"), c.Spec.Mode, []string{
			string(LogicalContainerModeSession), string(LogicalContainerModePersistent), string(LogicalContainerModeExisting),
		}))
	}

	if (mode == LogicalContainerModePersistent || mode == LogicalContainerModeExisting) && c.Spec.ContainerName == "" {
		errorList = append(errorList, field.Required(specPath.Child("containerName"), "containerName must be set for persistent and existing containers"))
	}
	if c.Spec.ContainerName != "" && !validContainerNameRegexp.MatchString(c.Spec.ContainerName) {
		errorList = append(errorList, field.Invalid(specPath.Child("containerName"), c.Spec.ContainerName, fmt.Sprintf("containerName must match regex '%s'", validContainerName)))
	}
	errorList = append(errorList, validateRetainedResourceMonitor(mode == LogicalContainerModePersistent, c.Spec.MonitorPID, c.Spec.MonitorTimestamp, specPath)...)

	if c.Spec.Container != nil {
		errorList = append(errorList, c.Spec.Container.Validate(c.Namespace, specPath.Child("container"))...)
	}
	return errorList
}

// Validate checks the image source and desired runtime configuration within a namespace.
func (ct *LogicalContainerConfig) Validate(namespace string, containerPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}
	if ct.ImageRef == "" && ct.Image == nil {
		errorList = append(errorList, field.Required(containerPath, "exactly one of imageRef or image must be set"))
	}
	if ct.ImageRef != "" && ct.Image != nil {
		errorList = append(errorList, field.Forbidden(containerPath.Child("image"), "image cannot be set when imageRef is set"))
	}
	if ct.ImageRef != "" {
		errorList = append(errorList, validateSameNamespaceResourceReference(ct.ImageRef, namespace, containerPath.Child("imageRef"))...)
	}
	if ct.Image != nil {
		errorList = append(errorList, ct.Image.Validate(containerPath.Child("image"))...)
	}
	for i, network := range ct.Networks {
		errorList = append(errorList, validateSameNamespaceResourceReference(network.NetworkRef, namespace, containerPath.Child("networks").Index(i).Child("networkRef"))...)
	}
	errorList = append(errorList, ValidateVolumeMounts(ct.VolumeMounts, namespace, containerPath.Child("volumeMounts"))...)
	errorList = append(errorList, ValidateContainerPorts(ct.Ports, containerPath.Child("ports"))...)
	errorList = append(errorList, validateLabels(ct.Labels, containerPath.Child("labels"))...)
	errorList = append(errorList, validateCreateFiles(ct.CreateFiles, containerPath.Child("createFiles"))...)
	return errorList
}

func (c *LogicalContainer) ValidateUpdate(ctx context.Context, old runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}
	oldContainer := old.(*LogicalContainer)
	if oldContainer.Spec.Stop && !c.Spec.Stop {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "stop"), "stop cannot be set to false once it has been set to true"))
	}

	oldSpec := oldContainer.Spec
	newSpec := c.Spec
	oldSpec.Stop = false
	newSpec.Stop = false
	if !reflect.DeepEqual(oldSpec, newSpec) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec"), "spec is immutable"))
	}
	return errorList
}

// LogicalContainerList contains a list of LogicalContainer instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type LogicalContainerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LogicalContainer `json:"items"`
}

func (cl *LogicalContainerList) GetListMeta() *metav1.ListMeta {
	return &cl.ListMeta
}

func (cl *LogicalContainerList) ItemCount() uint32 {
	return uint32(len(cl.Items))
}

func (cl *LogicalContainerList) GetItems() []*LogicalContainer {
	retval := make([]*LogicalContainer, len(cl.Items))
	for i := range cl.Items {
		retval[i] = &cl.Items[i]
	}
	return retval
}

func init() {
	SchemeBuilder.Register(&LogicalContainer{}, &LogicalContainerList{})
}

var _ apiserver_resource.Object = (*LogicalContainer)(nil)
var _ apiserver_resource.ObjectWithGenericSubResource = (*LogicalContainer)(nil)
var _ apiserver_resource.GenericSubResource = (*LogicalContainerLogResource)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*LogicalContainer)(nil)
var _ apiserver_resource.StatusSubResource = (*LogicalContainerStatus)(nil)
var _ apiserver_resource.ObjectList = (*LogicalContainerList)(nil)
var _ commonapi.ListWithObjectItems[LogicalContainer, *LogicalContainer] = (*LogicalContainerList)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*LogicalContainer)(nil)
var _ apiserver_resourcestrategy.Validater = (*LogicalContainer)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*LogicalContainer)(nil)
