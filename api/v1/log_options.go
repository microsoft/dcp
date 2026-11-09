/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"net/url"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/microsoft/dcp/internal/logstream"
)

type LogStreamSource = logstream.LogStreamSource

const (
	LogStreamSourceStdout        = logstream.LogStreamSourceStdout
	LogStreamSourceStderr        = logstream.LogStreamSourceStderr
	LogStreamSourceStartupStdout = logstream.LogStreamSourceStartupStdout
	LogStreamSourceStartupStderr = logstream.LogStreamSourceStartupStderr
	LogStreamSourceSystem        = logstream.LogStreamSourceSystem
)

// LogOptions selects a resource log stream and its output formatting.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +k8s:openapi-gen=true
type LogOptions logstream.LogOptions

func (lo *LogOptions) GetGroupVersionResource() schema.GroupVersionResource {
	return GroupVersion.WithResource("LogOptions")
}

func (lo *LogOptions) GetObjectMeta() *metav1.ObjectMeta { return &lo.ObjectMeta }
func (lo *LogOptions) GetFollow() bool                   { return lo.Follow }
func (lo *LogOptions) IsStorageVersion() bool            { return true }
func (lo *LogOptions) NamespaceScoped() bool             { return false }
func (lo *LogOptions) New() runtime.Object               { return &LogOptions{} }
func (lo *LogOptions) NewList() runtime.Object           { return &LogOptionsList{} }
func (lo *LogOptions) String() string                    { return (*logstream.LogOptions)(lo).String() }
func (lo *LogOptions) Validate() error                   { return (*logstream.LogOptions)(lo).Validate() }

// LogOptionsList supports API registration; options are not persisted.
// +kubebuilder:object:generate=false
type LogOptionsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
}

func (list *LogOptionsList) DeepCopyObject() runtime.Object {
	copied := &LogOptionsList{TypeMeta: list.TypeMeta}
	list.ListMeta.DeepCopyInto(&copied.ListMeta)
	return copied
}
func (list *LogOptionsList) GetListMeta() *metav1.ListMeta { return &list.ListMeta }

func UrlValuesToLogOptions(in *url.Values, out *LogOptions, scope conversion.Scope) error {
	return logstream.UrlValuesToLogOptions(in, (*logstream.LogOptions)(out), scope)
}

func RegisterLogOptionsConversions(scheme *runtime.Scheme) error {
	return scheme.AddConversionFunc((*url.Values)(nil), (*LogOptions)(nil), func(from, to interface{}, scope conversion.Scope) error {
		return UrlValuesToLogOptions(from.(*url.Values), to.(*LogOptions), scope)
	})
}

func init() {
	SchemeBuilder.Register(&LogOptions{}, &LogOptionsList{})
	SchemeBuilder.SchemeBuilder.Register(RegisterLogOptionsConversions)
}
