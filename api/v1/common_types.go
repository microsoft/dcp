/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
