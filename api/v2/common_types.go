/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ConditionType identifies a condition reported by a V2 resource.
type ConditionType string

// ConditionReason identifies the reason for a V2 condition's current status.
type ConditionReason string

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
