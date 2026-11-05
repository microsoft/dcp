/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"math"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// EnvironmentBehavior controls inheritance from the controller process environment.
// +kubebuilder:validation:Enum=Inherit;DoNotInherit
type EnvironmentBehavior string

const (
	// EnvironmentBehaviorInherit includes the controller process environment.
	EnvironmentBehaviorInherit EnvironmentBehavior = "Inherit"
	// EnvironmentBehaviorDoNotInherit excludes the controller process environment.
	EnvironmentBehaviorDoNotInherit EnvironmentBehavior = "DoNotInherit"
)

// AmbientEnvironment configures inheritance from the controller process environment.
// +k8s:openapi-gen=true
type AmbientEnvironment struct {
	// Behavior controls environment inheritance. Defaults to Inherit.
	// +kubebuilder:default:=Inherit
	Behavior EnvironmentBehavior `json:"behavior,omitempty"`
}

// EffectiveBehavior returns the configured inheritance behavior, defaulting to Inherit.
func (environment AmbientEnvironment) EffectiveBehavior() EnvironmentBehavior {
	if environment.Behavior == "" {
		return EnvironmentBehaviorInherit
	}
	return environment.Behavior
}

func (environment AmbientEnvironment) validate(environmentPath *field.Path) field.ErrorList {
	switch environment.EffectiveBehavior() {
	case EnvironmentBehaviorInherit, EnvironmentBehaviorDoNotInherit:
		return nil
	default:
		return field.ErrorList{field.NotSupported(environmentPath.Child("behavior"), environment.Behavior,
			[]string{string(EnvironmentBehaviorInherit), string(EnvironmentBehaviorDoNotInherit)})}
	}
}

// ConditionType identifies a condition reported by a V2 resource.
type ConditionType string

// ConditionReason identifies the reason for a V2 condition's current status.
type ConditionReason string

const (
	// LogicalResourceReasonPortPreparationPending indicates that service-backed ports are not prepared.
	LogicalResourceReasonPortPreparationPending ConditionReason = "PortPreparationPending"
	// LogicalResourceReasonTemplateEvaluationFailed indicates that argument or environment template evaluation failed.
	LogicalResourceReasonTemplateEvaluationFailed ConditionReason = "TemplateEvaluationFailed"
	// LogicalResourceReasonPersistenceFailed indicates that lifecycle bookkeeping could not be saved.
	LogicalResourceReasonPersistenceFailed ConditionReason = "PersistenceFailed"
	// LogicalResourceReasonStateStoreNotConfigured indicates that persistent resource storage is unavailable.
	LogicalResourceReasonStateStoreNotConfigured ConditionReason = "StateStoreNotConfigured"
	// LogicalResourceReasonLeaseAcquisitionFailed indicates that a creation lease could not be acquired.
	LogicalResourceReasonLeaseAcquisitionFailed ConditionReason = "ResourceLeaseAcquisitionFailed"
	// LogicalResourceReasonLeaseHeld indicates that another owner holds the creation lease.
	LogicalResourceReasonLeaseHeld ConditionReason = "ResourceLeaseHeld"
	// LogicalResourceReasonLeaseLost indicates that the creation lease is no longer held.
	LogicalResourceReasonLeaseLost ConditionReason = "ResourceLeaseLost"
	// LogicalResourceReasonLifecycleKeyCalculationFailed indicates that lifecycle identity could not be calculated.
	LogicalResourceReasonLifecycleKeyCalculationFailed ConditionReason = "LifecycleKeyCalculationFailed"
	// LogicalResourceReasonLifecyclePreparationFailed indicates that lifecycle preparation could not complete.
	LogicalResourceReasonLifecyclePreparationFailed ConditionReason = "LifecyclePreparationFailed"
)

// PhysicalResourcePhase summarizes the high-level state of a V2 physical resource.
type PhysicalResourcePhase string

const (
	// ConditionReady indicates whether the resource has completed reconciliation and is ready for use.
	ConditionReady ConditionType = "Ready"
)

const (
	// PhysicalResourceReasonNamespaceNotFound indicates that the resource's namespace does not exist.
	PhysicalResourceReasonNamespaceNotFound ConditionReason = "NamespaceNotFound"

	// PhysicalResourceReasonNamespaceTerminating indicates that the resource's namespace is terminating.
	PhysicalResourceReasonNamespaceTerminating ConditionReason = "NamespaceTerminating"

	// PhysicalResourceReasonNamespaceNotReady indicates that the namespace controller has not initialized the resource's namespace.
	PhysicalResourceReasonNamespaceNotReady ConditionReason = "NamespaceNotReady"

	// PhysicalResourceReasonNamespaceNotActive indicates that the resource's namespace is not active.
	PhysicalResourceReasonNamespaceNotActive ConditionReason = "NamespaceNotActive"

	// PhysicalResourceReasonNamespaceLookupFailed indicates that the resource's namespace could not be read.
	PhysicalResourceReasonNamespaceLookupFailed ConditionReason = "NamespaceLookupFailed"

	// PhysicalResourceReasonContainerRuntimeUnhealthy indicates that reconciliation is blocked because the container runtime is not healthy.
	PhysicalResourceReasonContainerRuntimeUnhealthy ConditionReason = "ContainerRuntimeUnhealthy"

	// PhysicalResourceReasonOperationStateInvalid indicates that controller-owned operation state is invalid.
	PhysicalResourceReasonOperationStateInvalid ConditionReason = "OperationStateInvalid"
)

const (
	// PhysicalResourcePhasePending indicates that reconciliation is expected to make progress.
	PhysicalResourcePhasePending PhysicalResourcePhase = "Pending"

	// PhysicalResourcePhaseReady indicates that the resource is available for use.
	PhysicalResourcePhaseReady PhysicalResourcePhase = "Ready"

	// PhysicalResourcePhaseRunning indicates that the runtime resource is actively executing.
	PhysicalResourcePhaseRunning PhysicalResourcePhase = "Running"

	// PhysicalResourcePhasePaused indicates that the runtime resource is suspended.
	PhysicalResourcePhasePaused PhysicalResourcePhase = "Paused"

	// PhysicalResourcePhaseExited indicates that the runtime resource has stopped executing.
	PhysicalResourcePhaseExited PhysicalResourcePhase = "Exited"

	// PhysicalResourcePhaseUnknown indicates that the resource's actual state is unavailable or indeterminate.
	PhysicalResourcePhaseUnknown PhysicalResourcePhase = "Unknown"

	// PhysicalResourcePhaseFailed indicates a terminal failure that reconciliation cannot recover from.
	PhysicalResourcePhaseFailed PhysicalResourcePhase = "Failed"
)

// NamespacedName returns the standard Kubernetes identity for a V2 namespaced resource.
func NamespacedName(obj metav1.Object) types.NamespacedName {
	return types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}
}

// ValidateNamespacedResourceMetadata validates metadata required by V2 namespace-scoped resources.
func ValidateNamespacedResourceMetadata(obj metav1.Object) field.ErrorList {
	errorList := field.ErrorList{}
	metadataPath := field.NewPath("metadata")

	if strings.TrimSpace(obj.GetNamespace()) == "" {
		errorList = append(errorList, field.Required(metadataPath.Child("namespace"), "Namespace is required"))
	} else {
		for _, validationMessage := range validation.IsDNS1123Label(obj.GetNamespace()) {
			errorList = append(errorList, field.Invalid(metadataPath.Child("namespace"), obj.GetNamespace(), validationMessage))
		}
	}

	return errorList
}

func validateRetainedResourceMonitor(
	retainRuntimeResource bool,
	monitorPID *int64,
	monitorTimestamp metav1.MicroTime,
	monitorPath *field.Path,
) field.ErrorList {
	errorList := field.ErrorList{}
	monitorTimestampSet := !monitorTimestamp.IsZero()

	if monitorPID != nil && (*monitorPID <= 0 || *monitorPID > math.MaxUint32) {
		errorList = append(errorList, field.Invalid(monitorPath.Child("monitorPID"), *monitorPID, "monitorPID must be between 1 and 4294967295"))
	}
	if !retainRuntimeResource && monitorPID != nil {
		errorList = append(errorList, field.Forbidden(monitorPath.Child("monitorPID"), "monitorPID can only be set for retained runtime resources"))
	}
	if !retainRuntimeResource && monitorTimestampSet {
		errorList = append(errorList, field.Forbidden(monitorPath.Child("monitorTimestamp"), "monitorTimestamp can only be set for retained runtime resources"))
	}
	if monitorPID != nil && !monitorTimestampSet {
		errorList = append(errorList, field.Required(monitorPath.Child("monitorTimestamp"), "monitorTimestamp must be set when monitorPID is set"))
	}
	if monitorPID == nil && monitorTimestampSet {
		errorList = append(errorList, field.Required(monitorPath.Child("monitorPID"), "monitorPID must be set when monitorTimestamp is set"))
	}

	return errorList
}

// ValidateSameNamespaceResourceReference validates a required name or namespace/name reference within the given namespace.
func ValidateSameNamespaceResourceReference(
	reference string,
	namespace string,
	referencePath *field.Path,
) field.ErrorList {
	if reference == "" {
		return field.ErrorList{field.Required(referencePath, "reference must be set")}
	}

	namespacedName := commonapi.AsNamespacedName(reference, namespace)
	errorList := field.ErrorList{}
	if strings.Contains(reference, string(types.Separator)) {
		for _, validationMessage := range validation.IsDNS1123Label(namespacedName.Namespace) {
			errorList = append(errorList, field.Invalid(referencePath, reference, validationMessage))
		}
		if namespacedName.Namespace != namespace {
			errorList = append(errorList, field.Invalid(referencePath, reference, "cross-namespace references are not supported"))
		}
	}
	for _, validationMessage := range validation.IsDNS1123Subdomain(namespacedName.Name) {
		errorList = append(errorList, field.Invalid(referencePath, reference, validationMessage))
	}
	return errorList
}
