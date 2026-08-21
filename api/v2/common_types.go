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

const (
	// ConditionReady indicates whether the resource has completed reconciliation and is ready for use.
	ConditionReady string = "Ready"
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
