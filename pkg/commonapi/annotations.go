/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commonapi

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

// MaxAnnotationsTotalSize is the maximum total size of all annotations in bytes.
// This is a Kubernetes API server limit.
// See: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations/#syntax-and-character-set
const MaxAnnotationsTotalSize = 256 * 1024 // 256 KB = 262144 bytes

// ValidateAnnotationsSize checks if the total size of annotations exceeds the Kubernetes limit.
func ValidateAnnotationsSize(annotations map[string]string, fieldPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}

	totalSize := calculateAnnotationsSize(annotations)
	if totalSize > MaxAnnotationsTotalSize {
		errorList = append(errorList, field.TooLongMaxLength(
			fieldPath,
			totalSize,
			MaxAnnotationsTotalSize,
		))
	}

	return errorList
}

// calculateAnnotationsSize calculates the total size of annotations in bytes.
func calculateAnnotationsSize(annotations map[string]string) int {
	totalSize := 0
	for key, value := range annotations {
		totalSize += len(key) + len(value)
	}
	return totalSize
}

// annotationsSizeInfo returns a human-readable description of the annotation size.
func annotationsSizeInfo(annotations map[string]string) string {
	totalSize := calculateAnnotationsSize(annotations)
	return fmt.Sprintf("%d bytes (limit: %d bytes / 256 KB)", totalSize, MaxAnnotationsTotalSize)
}
