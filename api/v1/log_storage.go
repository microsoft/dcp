/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"k8s.io/apimachinery/pkg/runtime"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"

	"github.com/microsoft/dcp/internal/logstream"
)

type ResourceStreamStatus = logstream.ResourceStreamStatus

// +k8s:openapi-gen=false
// +kubebuilder:object:generate=false
type ResourceWatcherEvent = logstream.ResourceWatcherEvent
type ResourceLogStreamer = logstream.ResourceLogStreamer[*LogOptions]

// +k8s:openapi-gen=false
// +kubebuilder:object:generate=false
type LogStorage = logstream.LogStorage[*LogOptions]

const (
	ResourceStreamStatusStreaming = logstream.ResourceStreamStatusStreaming
	ResourceStreamStatusDone      = logstream.ResourceStreamStatusDone
	ResourceStreamStatusNotReady  = logstream.ResourceStreamStatusNotReady
)

func NewLogStorage(parent registry_rest.StandardStorage, streamer ResourceLogStreamer) (*LogStorage, error) {
	return logstream.NewLogStorage(parent, streamer,
		func() *LogOptions { return &LogOptions{} },
		func(inner registry_rest.ResourceStreamer) runtime.Object { return &LogStreamer{Inner: inner} },
		GroupVersion.WithKind("LogStreamer"))
}
