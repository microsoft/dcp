/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"encoding/gob"
	"io"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// EnvVar represents an environment variable present in a Container or Executable.
// Aliased to the shared type so that other packages can construct environment variables
// without depending on a specific API version.
type EnvVar = commonapi.EnvVar

// To get consistent output from gob encoders, we need to introduce types in
// a deterministic order as the encoder generates (and globally caches) an incrementing ID
// for each type it encounters. This is a bit of a hack, but it works.
// Any types being encoded in lifecycle GetLifecycleKey methods need to be registered here.
func initializeLifecycleHashEncoder() {
	initEncoder := gob.NewEncoder(io.Discard)

	_ = initEncoder.Encode(ContainerLabel{})
	_ = initEncoder.Encode(ContainerBuildSecret{})
	_ = initEncoder.Encode(VolumeMount{})
	_ = initEncoder.Encode(ContainerPort{})
	_ = initEncoder.Encode(EnvVar{})
	_ = initEncoder.Encode(CreateFileSystem{})
	_ = initEncoder.Encode(ContainerPemCertificates{})
	_ = initEncoder.Encode(ImageLayer{})

	_ = initEncoder.Encode(time.Time{})
	_ = initEncoder.Encode(ExecutablePemCertificates{})
}

func init() {
	initializeLifecycleHashEncoder()
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

	// HasTerminal reports whether the resource is configured to bridge its
	// stdin/stdout/stderr to a pseudo-terminal. When true, the resource does
	// not produce stdout/stderr log files and API requests for those log
	// streams should fail. System logs are unaffected.
	HasTerminal() bool

	// This is set by Kubernetes with 1-second precision when the resource is deleted
	// Hence we use metav1.Time here instead of metav1.MicroTime
	GetDeletionTimestamp() *metav1.Time
}
