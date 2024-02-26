// Copyright (c) Microsoft Corporation. All rights reserved.

package v1

import (
	"context"
	"fmt"
	"io"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
)

// ResourceLogStreamFactory is an entity that knows how to stream logs for a given resource.
// +kubebuilder:object:generate=false
type ResourceLogStreamFactory interface {
	CreateLogStream(ctx context.Context, obj apiserver_resource.Object, opts *LogOptions, parentKindStorage registry_rest.StandardStorage) (io.ReadCloser, error)
}

// +kubebuilder:object:generate=false
type CreateLogStreamFunc func(ctx context.Context, obj apiserver_resource.Object, opts *LogOptions, parentKindStorage registry_rest.StandardStorage) (io.ReadCloser, error)

func (f CreateLogStreamFunc) CreateLogStream(ctx context.Context, obj apiserver_resource.Object, opts *LogOptions, parentKindStorage registry_rest.StandardStorage) (io.ReadCloser, error) {
	return f(ctx, obj, opts, parentKindStorage)
}

// +kubebuilder:object:generate=false
type LogStorage struct {
	// The data storage for the parent object kind
	parentKindStorage registry_rest.StandardStorage

	logStreamFactory ResourceLogStreamFactory
}

func NewLogStorage(parentKindStorage registry_rest.StandardStorage, logStreamFactory ResourceLogStreamFactory) (*LogStorage, error) {
	if parentKindStorage == nil {
		return nil, fmt.Errorf("Parent object kind storage is required for LogStorage to function")
	}
	if logStreamFactory == nil {
		return nil, fmt.Errorf("Log stream factory is required for LogStorage to function")
	}

	return &LogStorage{
		parentKindStorage: parentKindStorage,
		logStreamFactory:  logStreamFactory,
	}, nil
}

func (ls *LogStorage) New() runtime.Object {
	return &LogStreamer{}
}

func (ls *LogStorage) Destroy() {
	// Logs do not have independent storage/lifecycle, they will be deleted when the parent is deleted,
	// so this is a no-op.
}

func (ls *LogStorage) ProducesMIMETypes(httpVerb string) []string {
	return []string{"text/plain"}
}

// ProducesObject returns an object the specified HTTP verb respond with.
// Only the type of the returned object matters, the value is ignored.
// We return string, since the logs are streamed as text/plain;
// the API server does not really expose "streamer" objects returned by the Get function.
func (ls *LogStorage) ProducesObject(httpVerb string) interface{} {
	return ""
}

func (ls *LogStorage) GroupVersionKind(containingGV schema.GroupVersion) schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   GroupVersion.Group,
		Version: GroupVersion.Version,
		Kind:    "LogStreamer",
	}
}

func (ls *LogStorage) Get(ctx context.Context, name string, options runtime.Object) (runtime.Object, error) {
	logOptions, ok := options.(*LogOptions)
	if !ok {
		return nil, apierrors.NewInternalError(fmt.Errorf("invalid log options"))
	}

	streamer, err := ls.resourceStreamerFactory(name, logOptions)
	if err != nil {
		return nil, err
	}

	return &LogStreamer{
		Inner: streamer,
	}, nil
}

func (ls *LogStorage) resourceStreamerFactory(resourceName string, options *LogOptions) (registry_rest.ResourceStreamer, error) {
	return ResourceStreamerFunc(func(ctx context.Context, apiVersion, acceptHeader string) (io.ReadCloser, bool, string, error) {
		getOpts := metav1.GetOptions{}
		obj, err := ls.parentKindStorage.Get(ctx, resourceName, &getOpts)
		if err != nil {
			return nil, false, "", err
		}

		apiObj, isAPIObj := obj.(apiserver_resource.Object)
		if !isAPIObj {
			return nil, false, "", apierrors.NewInternalError(fmt.Errorf("parent storage objects (%s) do not expose sufficient metadata required for log streaming", obj.GetObjectKind().GroupVersionKind().String()))

		}

		logStream, err := ls.logStreamFactory.CreateLogStream(ctx, apiObj, options, ls.parentKindStorage)
		if err != nil {
			return nil, false, "", err
		}

		return logStream /* flushOnNewData */, true, "text/plain", nil
	}), nil
}

func (ls *LogStorage) NewGetOptions() (runtime.Object, bool, string) {
	return &LogOptions{}, false, ""
}

var _ registry_rest.Storage = (*LogStorage)(nil)
var _ registry_rest.StorageMetadata = (*LogStorage)(nil)
var _ registry_rest.GetterWithOptions = (*LogStorage)(nil)
var _ registry_rest.GroupVersionKindProvider = (*LogStorage)(nil)
