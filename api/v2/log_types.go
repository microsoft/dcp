/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"fmt"
	"io"
	"net/url"

	apiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	"github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	generic "k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/microsoft/dcp/internal/logstream"
	"github.com/microsoft/dcp/pkg/syncmap"
)

const LogSubresourceName = "log"

type LogStreamSource = logstream.LogStreamSource

const (
	LogStreamSourceStdout        = logstream.LogStreamSourceStdout
	LogStreamSourceStderr        = logstream.LogStreamSourceStderr
	LogStreamSourceSystem        = logstream.LogStreamSourceSystem
	LogStreamSourceStartupStdout = logstream.LogStreamSourceStartupStdout
	LogStreamSourceStartupStderr = logstream.LogStreamSourceStartupStderr
)

// LogSource identifies captured output and its availability without querying another resource.
// +k8s:openapi-gen=true
type LogSource struct {
	// Name is the same-namespace source resource name.
	Name string `json:"name"`
	// UID identifies the source resource's capture.
	UID types.UID `json:"uid"`
	// ContainerID selects runtime capture instead of pre-captured files.
	ContainerID string `json:"containerID,omitempty"`
	// StdoutFile is the pre-captured standard-output path.
	StdoutFile string `json:"stdoutFile,omitempty"`
	// StderrFile is the pre-captured standard-error path.
	StderrFile string `json:"stderrFile,omitempty"`
	// Available indicates that capture can be consumed.
	Available bool `json:"available,omitempty"`
	// Complete indicates that no further output is expected.
	Complete bool `json:"complete,omitempty"`
}

type ResourceLogStreamer = logstream.ResourceLogStreamer[*ResourceLogOptions]

// +k8s:openapi-gen=false
// +kubebuilder:object:generate=false
type ResourceWatcherEvent = logstream.ResourceWatcherEvent

var ResourceLogStreamers = &syncmap.Map[schema.GroupVersionResource, ResourceLogStreamer]{}

// ResourceLogOptions selects a resource output or system stream and its formatting.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +k8s:openapi-gen=true
type ResourceLogOptions logstream.LogOptions

func (opts *ResourceLogOptions) GetGroupVersionResource() schema.GroupVersionResource {
	return GroupVersion.WithResource("ResourceLogOptions")
}
func (opts *ResourceLogOptions) GetObjectMeta() *metav1.ObjectMeta { return &opts.ObjectMeta }
func (opts *ResourceLogOptions) GetFollow() bool                   { return opts.Follow }
func (opts *ResourceLogOptions) IsStorageVersion() bool            { return true }
func (opts *ResourceLogOptions) NamespaceScoped() bool             { return true }
func (opts *ResourceLogOptions) New() runtime.Object               { return &ResourceLogOptions{} }
func (opts *ResourceLogOptions) NewList() runtime.Object           { return &ResourceLogOptionsList{} }
func (opts *ResourceLogOptions) String() string                    { return (*logstream.LogOptions)(opts).String() }
func (opts *ResourceLogOptions) Validate() error {
	switch opts.Source {
	case "", string(LogStreamSourceStdout), string(LogStreamSourceStderr), string(LogStreamSourceSystem),
		string(LogStreamSourceStartupStdout), string(LogStreamSourceStartupStderr):
		return (*logstream.LogOptions)(opts).Validate()
	default:
		return fmt.Errorf("invalid log source %q; supported sources are stdout, stderr, system, startup_stdout, and startup_stderr", opts.Source)
	}
}

// ResourceLogOptionsList supports registration; log options are not persisted.
// +kubebuilder:object:generate=false
type ResourceLogOptionsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
}

func (list *ResourceLogOptionsList) DeepCopyObject() runtime.Object {
	copied := &ResourceLogOptionsList{TypeMeta: list.TypeMeta}
	list.ListMeta.DeepCopyInto(&copied.ListMeta)
	return copied
}
func (list *ResourceLogOptionsList) GetListMeta() *metav1.ListMeta { return &list.ListMeta }

// ResourceLogResponse carries a raw text response for a resource log subresource.
// +kubebuilder:object:generate=false
// +k8s:openapi-gen=true
type ResourceLogResponse struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Inner             rest.ResourceStreamer `json:"-"`
}

func (stream *ResourceLogResponse) GetGroupVersionResource() schema.GroupVersionResource {
	return GroupVersion.WithResource(LogSubresourceName)
}
func (stream *ResourceLogResponse) GetObjectMeta() *metav1.ObjectMeta { return &stream.ObjectMeta }
func (stream *ResourceLogResponse) IsStorageVersion() bool            { return true }
func (stream *ResourceLogResponse) NamespaceScoped() bool             { return true }
func (stream *ResourceLogResponse) New() runtime.Object               { return &ResourceLogResponse{} }
func (stream *ResourceLogResponse) NewList() runtime.Object           { return &ResourceLogResponseList{} }
func (stream *ResourceLogResponse) DeepCopyObject() runtime.Object {
	copied := &ResourceLogResponse{TypeMeta: stream.TypeMeta, Inner: stream.Inner}
	stream.ObjectMeta.DeepCopyInto(&copied.ObjectMeta)
	return copied
}
func (stream *ResourceLogResponse) InputStream(ctx context.Context, version, accept string) (io.ReadCloser, bool, string, error) {
	return stream.Inner.InputStream(ctx, version, accept)
}

// ResourceLogResponseList supports registration; streams are not persisted.
// +kubebuilder:object:generate=false
type ResourceLogResponseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
}

func (list *ResourceLogResponseList) DeepCopyObject() runtime.Object {
	copied := &ResourceLogResponseList{TypeMeta: list.TypeMeta}
	list.ListMeta.DeepCopyInto(&copied.ListMeta)
	return copied
}
func (list *ResourceLogResponseList) GetListMeta() *metav1.ListMeta { return &list.ListMeta }

func newLogStorageProvider(
	obj resource.Object, parentProvider apiserver.StorageProvider,
) apiserver.StorageProvider {
	return func(scheme *runtime.Scheme, getter generic.RESTOptionsGetter) (rest.Storage, error) {
		parent, parentErr := parentProvider(scheme, getter)
		if parentErr != nil {
			return nil, fmt.Errorf("get parent log storage: %w", parentErr)
		}
		storage, valid := parent.(rest.StandardStorage)
		if !valid {
			return nil, fmt.Errorf("parent %s does not support standard storage", obj.GetGroupVersionResource())
		}
		streamer, found := ResourceLogStreamers.Load(obj.GetGroupVersionResource())
		if !found {
			return nil, fmt.Errorf("log streamer not registered for %s", obj.GetGroupVersionResource())
		}
		return logstream.NewLogStorage(storage, streamer,
			func() *ResourceLogOptions { return &ResourceLogOptions{} },
			func(inner rest.ResourceStreamer) runtime.Object { return &ResourceLogResponse{Inner: inner} },
			GroupVersion.WithKind("ResourceLogResponse"))
	}
}

func init() {
	SchemeBuilder.Register(&ResourceLogOptions{}, &ResourceLogOptionsList{}, &ResourceLogResponse{}, &ResourceLogResponseList{})
	SchemeBuilder.SchemeBuilder.Register(RegisterLogOptionsConversions)
}

func RegisterLogOptionsConversions(scheme *runtime.Scheme) error {
	return scheme.AddConversionFunc((*url.Values)(nil), (*ResourceLogOptions)(nil), func(from, to interface{}, scope conversion.Scope) error {
		return logstream.UrlValuesToLogOptions(from.(*url.Values), (*logstream.LogOptions)(to.(*ResourceLogOptions)), scope)
	})
}
