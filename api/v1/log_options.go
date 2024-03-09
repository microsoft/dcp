// Copyright (c) Microsoft Corporation. All rights reserved.

package v1

import (
	"fmt"
	"net/url"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
)

type LogStreamSource string

const (
	LogStreamSourceStdout LogStreamSource = "stdout"
	LogStreamSourceStderr LogStreamSource = "stderr"
	LogStreamSourceAll    LogStreamSource = "all"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +k8s:openapi-gen=true
// +k8s:conversion-gen:explicit-from=net/url.Values
// +k8s:conversion-gen:explicit-from=net/url.Values
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type LogOptions struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// If true, follow the logs for the Executable or Container.
	// +optional
	Follow bool `json:"follow,omitempty"`

	// The source of the logs to display.
	// Note: the K8s API server does not support non-standard types in query parameters, so we can't use LogStreamSource here.
	// +optional
	Source string `json:"source,omitempty"`

	// If true, include timestamps (RFC3339) in the logs.
	// +optional
	Timestamps bool `json:"timestamps,omitempty"`

	// CONSIDER other options such as the timestamp from which to start showing logs,
	// whether to include timestamps etc. For inspiration see K8s PodLogOptions struct definition:
	// https://github.com/kubernetes/kubernetes/blob/master/pkg/apis/core/types.go#L5212
}

func (lo *LogOptions) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "LogOptions",
	}
}

func (lo *LogOptions) GetObjectMeta() *metav1.ObjectMeta {
	return &lo.ObjectMeta
}

// LogOptions are not really persisted. This function is somewhat mis-named:
// it is used by the API server for multi-version resources to distinguish between
// backward-compatibility versions that are not stored, and the "current" versions that are.
// So in our case the answer is "yes" since this code is the only version of LogOptions resource.
func (lo *LogOptions) IsStorageVersion() bool {
	return true
}

func (lo *LogOptions) NamespaceScoped() bool {
	return false
}

func (lo *LogOptions) New() runtime.Object {
	return &LogOptions{}
}

func (lo *LogOptions) NewList() runtime.Object {
	return &LogOptionsList{}
}

// We'll never have "lists of LogOptions", but list definition is required to register the resource with the API server.
// +kubebuilder:object:generate=false
type LogOptionsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
}

func (lsl *LogOptionsList) DeepCopyObject() runtime.Object {
	retval := LogOptionsList{
		TypeMeta: lsl.TypeMeta,
	}
	lsl.ListMeta.DeepCopyInto(&retval.ListMeta)
	return &retval
}

func (lsl *LogOptionsList) GetListMeta() *metav1.ListMeta {
	return &lsl.ListMeta
}

// Normally we would use conversion-gen code generator to generate conversion functions,
// but for some reason this generator is not finding the DeepCopyObject() implementation for this type
// and is failing. Probably a bug. As a workaround we will implement the conversion functions manually.
// We might try harder to use the code generator if we end up having more type conversions
// (e.g. if we support multiple schema versions for DCP).
func urlValuesToLogOptions(in *url.Values, out *LogOptions, cscope conversion.Scope) error {
	if in == nil {
		return fmt.Errorf("expected a valid net/url.Values object, but got nil")
	}
	if out == nil {
		return fmt.Errorf("expected a valid *v1.LogOptions object as result parameter, but got nil")
	}

	followValues := (*in)["follow"]
	if len(followValues) > 0 {
		if err := runtime.Convert_Slice_string_To_bool(&followValues, &out.Follow, cscope); err != nil {
			return fmt.Errorf("failed to convert LogOptions 'follow' parameter: %w", err)
		}
	} else {
		out.Follow = false
	}

	sourceValues := (*in)["source"]
	if len(sourceValues) > 0 {
		if err := runtime.Convert_Slice_string_To_string(&sourceValues, &out.Source, cscope); err != nil {
			return fmt.Errorf("failed to convert LogOptions 'source' parameter: %w", err)
		}
	} else {
		out.Source = ""
	}

	return nil
}

func RegisterLogOptionsConversions(scheme *runtime.Scheme) error {
	registrationErr := scheme.AddConversionFunc((*url.Values)(nil), (*LogOptions)(nil), func(from, to interface{}, scope conversion.Scope) error {
		return urlValuesToLogOptions(from.(*url.Values), to.(*LogOptions), scope)
	})
	return registrationErr
}

func init() {
	SchemeBuilder.Register(&LogOptions{}, &LogOptionsList{})

	// The following invocation looks strange, but it is not our fault
	// that controller-runtime SchemeBuilder wraps apimachinery type of the same name.
	SchemeBuilder.SchemeBuilder.Register(RegisterLogOptionsConversions)
}

// Ensure all objects defined here implement appropriate interfaces
var _ apiserver_resource.Object = (*LogOptions)(nil)
var _ apiserver_resource.ObjectList = (*LogOptionsList)(nil)
