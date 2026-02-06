/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// EnvVar represents an environment variable present in a Container or Executable.
// +k8s:openapi-gen=true
type EnvVar struct {
	// Name of the environment variable
	Name string `json:"name"`

	// Value of the environment variable. Defaults to "" (empty string).
	// +optional
	Value string `json:"value,omitempty"`
	// CONSIDER allowing expansion of existing variable references e.g. using ${VAR_NAME} syntax and $$ to escape the $ sign
}

const LogSubresourceName = "log"

// MaxAnnotationsTotalSize is the maximum total size of all annotations in bytes.
// This is a Kubernetes API server limit.
// See: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations/#syntax-and-character-set
const MaxAnnotationsTotalSize = 256 * 1024 // 256 KB = 262144 bytes

// AnnotationSizeWarningThreshold is the threshold at which we warn about annotation size.
// We use 90% of the max size to give users early warning before hitting the hard limit.
const AnnotationSizeWarningThreshold = MaxAnnotationsTotalSize * 90 / 100 // ~230 KB

// ValidateAnnotationsSize checks if the total size of annotations exceeds the Kubernetes limit.
// It returns an error if the annotations exceed MaxAnnotationsTotalSize (256 KB).
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
// The size includes both keys and values.
func calculateAnnotationsSize(annotations map[string]string) int {
	totalSize := 0
	for key, value := range annotations {
		totalSize += len(key) + len(value)
	}
	return totalSize
}

// GetAnnotationsSizeInfo returns a human-readable description of the annotation size.
// This can be used to provide helpful context in error messages.
func GetAnnotationsSizeInfo(annotations map[string]string) string {
	totalSize := calculateAnnotationsSize(annotations)
	return fmt.Sprintf("%d bytes (limit: %d bytes / 256 KB)", totalSize, MaxAnnotationsTotalSize)
}

// +kubebuilder:object:generate=false
// +k8s:openapi-gen=false
type StdIoStreamableResource interface {
	GetUID() types.UID
	NamespacedName() types.NamespacedName
	HasStdOut() bool
	HasStdErr() bool
	GetStdOutFile() string
	GetStdErrFile() string
	GetResourceId() string
	Done() bool

	// This is set by Kubernetes with 1-second precision when the resource is deleted
	// Hence we use metav1.Time here instead of metav1.MicroTime
	GetDeletionTimestamp() *metav1.Time
}
