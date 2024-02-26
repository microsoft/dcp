package v1

import (
	"context"
	"io"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
)

// The sole purpose of the LogStreamer existence is that a Kubernetes storage object
// (LogStorage for DCP logs) must be associated with an object (type) that it stores.
// Otherwise, resource_rest.ResourceStreamer instance would be sufficient for what we need to do.
//
// +kubebuilder:object:generate=false
// +k8s:openapi-gen=true
type LogStreamer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Inner registry_rest.ResourceStreamer `json:"-"` // Do not serialize into JSON
}

func (ls *LogStreamer) GetObjectMeta() *metav1.ObjectMeta {
	return &ls.ObjectMeta
}

// LogStreamer is not really persisted. This function is somewhat mis-named:
// it is used by the API server for multi-version resources to distinguish between
// backward-compatibility versions that are not stored, and the "current" versions that are.
// So in our case the answer is "yes" since this code is the only version of LogStreamer resource.
func (ls *LogStreamer) IsStorageVersion() bool {
	return true
}

func (ls *LogStreamer) NamespaceScoped() bool {
	return false
}

func (ls *LogStreamer) New() runtime.Object {
	return &LogStreamer{}
}

func (ls *LogStreamer) NewList() runtime.Object {
	return &LogStreamerList{}
}

func (ls *LogStreamer) DeepCopyObject() runtime.Object {
	retval := LogStreamer{
		TypeMeta: ls.TypeMeta,
	}
	ls.ObjectMeta.DeepCopyInto(&retval.ObjectMeta)
	return &retval
}

func (ls *LogStreamer) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: LogSubresourceName,
	}
}

// Returns stream with the log contents
func (ls *LogStreamer) InputStream(ctx context.Context, apiVersion, acceptHeader string) (io.ReadCloser, bool, string, error) {
	return ls.Inner.InputStream(ctx, apiVersion, acceptHeader)
}

// We'll never have "lists of LogStreamers", but list definition is required to register the resource with the API server.
// +kubebuilder:object:generate=false
type LogStreamerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
}

func (lsl *LogStreamerList) DeepCopyObject() runtime.Object {
	retval := LogStreamerList{
		TypeMeta: lsl.TypeMeta,
	}
	lsl.ListMeta.DeepCopyInto(&retval.ListMeta)
	return &retval
}

func (lsl *LogStreamerList) GetListMeta() *metav1.ListMeta {
	return &lsl.ListMeta
}

// +kubebuilder:object:generate=false
type ResourceStreamerFunc func(ctx context.Context, apiVersion, acceptHeader string) (io.ReadCloser, bool, string, error)

func (rs ResourceStreamerFunc) InputStream(ctx context.Context, apiVersion, acceptHeader string) (io.ReadCloser, bool, string, error) {
	return rs(ctx, apiVersion, acceptHeader)
}

func init() {
	SchemeBuilder.Register(&LogStreamer{}, &LogStreamerList{})
}

var _ apiserver_resource.Object = (*LogStreamer)(nil)
var _ registry_rest.ResourceStreamer = (*LogStreamer)(nil)
var _ apiserver_resource.ObjectList = (*LogStreamerList)(nil)
