/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"context"
	"encoding/json"
	"fmt"

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

	"github.com/microsoft/dcp/pkg/commonapi"
)

// IdeSessionState represents the lifecycle state of an IdeSession.
// The same type is used for both the actual state (Status.State) and the user-requested
// state (Spec.DesiredState), but the set of values allowed in Spec.DesiredState
// is restricted by ValidateDesiredState.
type IdeSessionState string

const (
	// IdeSessionStateInitial (the zero value) indicates an IdeSession that has not been
	// scheduled for execution yet. An IdeSession remains in this state until
	// Spec.DesiredState is set to IdeSessionStateRunning.
	IdeSessionStateInitial IdeSessionState = ""

	// IdeSessionStateStarting indicates that the controller has accepted the start request
	// and is attempting to launch the session in the connected IDE.
	IdeSessionStateStarting IdeSessionState = "Starting"

	// IdeSessionStateRunning indicates that the IDE has reported that the session is running.
	IdeSessionStateRunning IdeSessionState = "Running"

	// IdeSessionStateStopping indicates that the controller has asked the IDE to stop the session
	// and is waiting for confirmation that the session has terminated.
	IdeSessionStateStopping IdeSessionState = "Stopping"

	// IdeSessionStateStopped indicates that the session terminated normally.
	// IdeSessionStateStopped is a terminal state.
	IdeSessionStateStopped IdeSessionState = "Stopped"

	// IdeSessionStateFailed indicates that the session could not be started, or terminated
	// abnormally. Status.Message typically contains additional details.
	// IdeSessionStateFailed is a terminal state.
	IdeSessionStateFailed IdeSessionState = "Failed"
)

// CanUpdateTo reports whether transitioning from the current state to the supplied
// new state is allowed. Failed is reachable from any non-initial, non-terminal state.
// Stopped and Failed are terminal.
func (s IdeSessionState) CanUpdateTo(newState IdeSessionState) bool {
	if s == newState {
		return true
	}

	// Failed is reachable from any non-initial, non-terminal state.
	if newState == IdeSessionStateFailed && s != IdeSessionStateInitial && !s.IsTerminal() {
		return true
	}

	switch s {
	case IdeSessionStateInitial:
		return newState == IdeSessionStateStarting
	case IdeSessionStateStarting:
		return newState == IdeSessionStateRunning ||
			newState == IdeSessionStateStopping ||
			newState == IdeSessionStateStopped
	case IdeSessionStateRunning:
		return newState == IdeSessionStateStopping ||
			newState == IdeSessionStateStopped
	case IdeSessionStateStopping:
		return newState == IdeSessionStateStopped
	default:
		// IdeSessionStateStopped and IdeSessionStateFailed are terminal.
		return false
	}
}

// IsTerminal reports whether the state is terminal (no further transitions are allowed).
func (s IdeSessionState) IsTerminal() bool {
	return s == IdeSessionStateStopped || s == IdeSessionStateFailed
}

// IsValidDesiredState reports whether the value is one of the IdeSessionState values that
// users are permitted to set via Spec.DesiredState.
func (s IdeSessionState) IsValidDesiredState() bool {
	return s == IdeSessionStateInitial ||
		s == IdeSessionStateRunning ||
		s == IdeSessionStateStopped
}

// IdeSessionSpec describes the desired state of an IdeSession.
// +k8s:openapi-gen=true
type IdeSessionSpec struct {
	// LaunchConfigurations is a JSON document (typically an array of one or more launch
	// configuration objects) that is passed to the connected IDE when the session is started.
	// The exact schema is defined by the Aspire IDE-execution protocol; the controller does
	// not interpret the payload beyond verifying that it parses as JSON.
	LaunchConfigurations string `json:"launchConfigurations"`

	// DesiredState requests a particular target state for the IdeSession.
	// Allowed values are "" (Initial), "Running", and "Stopped". The session does not begin
	// execution until DesiredState is set to "Running". Once DesiredState is set to "Stopped"
	// it cannot be changed back to "Running".
	// +optional
	// +kubebuilder:validation:Enum="";Running;Stopped
	DesiredState IdeSessionState `json:"desiredState,omitempty"`
}

// IdeSessionStatus describes the actual state of an IdeSession.
// +k8s:openapi-gen=true
type IdeSessionStatus struct {
	// State is the current lifecycle state of the IdeSession as observed by the controller.
	State IdeSessionState `json:"state"`

	// Message is a human-readable explanation of the current State, typically populated
	// when the session enters Failed or as additional context for state transitions.
	// +optional
	Message string `json:"message,omitempty"`

	// SessionID is the identifier assigned by the IDE to the underlying run session.
	// It is populated once the session has been successfully started.
	// +optional
	SessionID string `json:"sessionID,omitempty"`

	// PID is the process identifier of the main process the IDE attached to.
	// The IDE may report a new PID over time (e.g. when an application is restarted),
	// in which case this value is updated.
	// +optional
	PID *int64 `json:"pid,omitempty"`

	// StartupTimestamp records when the session start attempt was made.
	// +optional
	StartupTimestamp metav1.MicroTime `json:"startupTimestamp,omitempty"`

	// FinishTimestamp records when the session was observed to have terminated.
	// +optional
	FinishTimestamp metav1.MicroTime `json:"finishTimestamp,omitempty"`

	// ExitCode is the exit code reported by the IDE for the session, if any.
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`

	// StdOutFile is the path to the temporary file capturing standard-output messages
	// emitted by the session.
	// +optional
	StdOutFile string `json:"stdOutFile,omitempty"`

	// StdErrFile is the path to the temporary file capturing standard-error messages
	// emitted by the session.
	// +optional
	StdErrFile string `json:"stdErrFile,omitempty"`
}

func (s IdeSessionStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	s.DeepCopyInto(&dest.(*IdeSession).Status)
}

// IdeSession represents a debug session running inside a connected IDE.
// Unlike Executable, an IdeSession is not associated with a child process owned by DCP;
// it is purely a request for the IDE to spin up (and later tear down) a debug session
// described by Spec.LaunchConfigurations.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type IdeSession struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IdeSessionSpec   `json:"spec,omitempty"`
	Status IdeSessionStatus `json:"status,omitempty"`
}

func (s *IdeSession) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "idesessions",
	}
}

func (s *IdeSession) GetObjectMeta() *metav1.ObjectMeta {
	return &s.ObjectMeta
}

func (s *IdeSession) GetStatus() apiserver_resource.StatusSubResource {
	return s.Status
}

func (s *IdeSession) New() runtime.Object {
	return &IdeSession{}
}

func (s *IdeSession) NewList() runtime.Object {
	return &IdeSessionList{}
}

func (s *IdeSession) IsStorageVersion() bool {
	return true
}

func (s *IdeSession) NamespaceScoped() bool {
	return false
}

func (s *IdeSession) ShortNames() []string {
	return []string{"ides"}
}

func (s *IdeSession) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      s.Name,
		Namespace: s.Namespace,
	}
}

// HasStdOut implements StdIoStreamableResource.
func (s *IdeSession) HasStdOut() bool {
	return true
}

// HasStdErr implements StdIoStreamableResource.
func (s *IdeSession) HasStdErr() bool {
	return true
}

// GetStdOutFile implements StdIoStreamableResource.
func (s *IdeSession) GetStdOutFile() string {
	return s.Status.StdOutFile
}

// GetStdErrFile implements StdIoStreamableResource.
func (s *IdeSession) GetStdErrFile() string {
	return s.Status.StdErrFile
}

// GetResourceId implements StdIoStreamableResource.
func (s *IdeSession) GetResourceId() string {
	return fmt.Sprintf("idesession-%s", s.UID)
}

func (s *IdeSession) Validate(ctx context.Context) field.ErrorList {
	errorList := field.ErrorList{}

	if ResourceCreationProhibited.Load() && s.DeletionTimestamp.IsZero() {
		errorList = append(errorList, field.Forbidden(nil, errResourceCreationProhibited.Error()))
	}

	errorList = append(errorList, s.Spec.Validate(field.NewPath("spec"))...)
	errorList = append(errorList, ValidateAnnotationsSize(s.Annotations, field.NewPath("metadata", "annotations"))...)

	return errorList
}

func (s *IdeSession) ValidateUpdate(ctx context.Context, obj runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	old := obj.(*IdeSession)

	if old.Spec.LaunchConfigurations != s.Spec.LaunchConfigurations {
		errorList = append(errorList, field.Forbidden(
			field.NewPath("spec", "launchConfigurations"),
			"launchConfigurations cannot be changed after the IdeSession is created."))
	}

	if old.Spec.DesiredState == IdeSessionStateStopped && s.Spec.DesiredState != IdeSessionStateStopped {
		errorList = append(errorList, field.Forbidden(
			field.NewPath("spec", "desiredState"),
			"desiredState cannot be changed once it is set to Stopped."))
	}

	errorList = append(errorList, s.Spec.Validate(field.NewPath("spec"))...)

	return errorList
}

func (s *IdeSession) Done() bool {
	return s.Status.State.IsTerminal()
}

func (*IdeSession) GenericSubResources() []apiserver_resource.GenericSubResource {
	return []apiserver_resource.GenericSubResource{
		&IdeSessionLogResource{},
	}
}

// Validate checks that the IdeSessionSpec is internally consistent.
func (spec IdeSessionSpec) Validate(specPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}

	if spec.LaunchConfigurations == "" {
		errorList = append(errorList, field.Required(
			specPath.Child("launchConfigurations"),
			"launchConfigurations is required."))
	} else {
		// We do not interpret the launch configurations payload, but we do verify it parses as
		// JSON so users can correct obviously-malformed input before the IDE rejects it.
		var parsed json.RawMessage
		if err := json.Unmarshal([]byte(spec.LaunchConfigurations), &parsed); err != nil {
			errorList = append(errorList, field.Invalid(
				specPath.Child("launchConfigurations"),
				spec.LaunchConfigurations,
				fmt.Sprintf("launchConfigurations must be valid JSON: %v", err)))
		}
	}

	if !spec.DesiredState.IsValidDesiredState() {
		errorList = append(errorList, field.Invalid(
			specPath.Child("desiredState"),
			spec.DesiredState,
			"desiredState must be empty, Running, or Stopped."))
	}

	return errorList
}

// IdeSessionList contains a list of IdeSession instances.
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type IdeSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IdeSession `json:"items"`
}

func (l *IdeSessionList) GetListMeta() *metav1.ListMeta {
	return &l.ListMeta
}

func (l *IdeSessionList) ItemCount() uint32 {
	return uint32(len(l.Items))
}

func (l *IdeSessionList) GetItems() []*IdeSession {
	retval := make([]*IdeSession, len(l.Items))
	for i := range l.Items {
		retval[i] = &l.Items[i]
	}
	return retval
}

// IdeSessionLogResource implements the log subresource for IdeSession objects.
type IdeSessionLogResource struct{}

func (r *IdeSessionLogResource) Name() string {
	return LogSubresourceName
}

func (r *IdeSessionLogResource) GetStorageProvider(
	obj apiserver_resource.Object,
	rootPath string,
	parentSP apiserver.StorageProvider,
) apiserver.StorageProvider {
	return func(scheme *runtime.Scheme, reg generic_registry.RESTOptionsGetter) (registry_rest.Storage, error) {
		storage, err := parentSP(scheme, reg)
		if err != nil {
			return nil, fmt.Errorf("failed to get parent (%s) storage: %w", obj.GetObjectKind().GroupVersionKind().Kind, err)
		}

		idsStorage, isGetter := storage.(registry_rest.StandardStorage)
		if !isGetter {
			return nil, fmt.Errorf("parent (%s) should implement registry_rest.Getter", obj.GetObjectKind().GroupVersionKind().Kind)
		}

		logStreamFactory, found := ResourceLogStreamers.Load(obj.GetGroupVersionResource())
		if !found {
			return nil, fmt.Errorf("log stream factory not found for resource '%s'", obj.GetGroupVersionResource().String())
		}

		logStorage, err := NewLogStorage(idsStorage, logStreamFactory)
		if err != nil {
			return nil, err
		}

		return logStorage, nil
	}
}

func init() {
	SchemeBuilder.Register(&IdeSession{}, &IdeSessionList{})
}

// Ensure types support interfaces expected by our API server.
var _ apiserver_resource.Object = (*IdeSession)(nil)
var _ apiserver_resource.ObjectList = (*IdeSessionList)(nil)
var _ commonapi.ListWithObjectItems[IdeSession, *IdeSession] = (*IdeSessionList)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*IdeSession)(nil)
var _ apiserver_resource.StatusSubResource = (*IdeSessionStatus)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*IdeSession)(nil)
var _ apiserver_resourcestrategy.Validater = (*IdeSession)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*IdeSession)(nil)
var _ apiserver_resource.ObjectWithGenericSubResource = (*IdeSession)(nil)
var _ apiserver_resource.GenericSubResource = (*IdeSessionLogResource)(nil)
var _ StdIoStreamableResource = (*IdeSession)(nil)
