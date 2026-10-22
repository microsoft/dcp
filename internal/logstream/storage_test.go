/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logstream

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/microsoft/dcp/pkg/testutil"
)

type storageOptions struct{ LogOptions }

func (opts *storageOptions) DeepCopyObject() runtime.Object {
	copied := *opts
	return &copied
}
func (opts *storageOptions) GetFollow() bool { return opts.Follow }

type unusedStorage struct{ rest.StandardStorage }
type unusedStreamer struct {
	ResourceLogStreamer[*storageOptions]
}

type watchedStorageObject struct {
	apiserver_resource.Object
	metav1.TypeMeta
	metav1.ObjectMeta
}

func (obj *watchedStorageObject) GetObjectKind() schema.ObjectKind  { return &obj.TypeMeta }
func (obj *watchedStorageObject) GetObjectMeta() *metav1.ObjectMeta { return &obj.ObjectMeta }

type watchedStorage struct {
	rest.StandardStorage
	object  *watchedStorageObject
	watcher *watch.RaceFreeFakeWatcher
}

func (storage *watchedStorage) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	return storage.object, nil
}

func (storage *watchedStorage) Watch(context.Context, *metainternal.ListOptions) (watch.Interface, error) {
	return storage.watcher, nil
}

type streamingStorageResponse struct {
	metav1.Status
	rest.ResourceStreamer
}

type streamingStorageConsumer struct {
	ResourceLogStreamer[*storageOptions]
	started     chan struct{}
	done        chan struct{}
	updates     chan struct{}
	updateCount atomic.Int32
}

func (*streamingStorageConsumer) Preflight(apiserver_resource.Object, *storageOptions) error {
	return nil
}

func (consumer *streamingStorageConsumer) StreamLogs(
	context.Context, io.Writer, apiserver_resource.Object, *storageOptions, logr.Logger,
) (ResourceStreamStatus, <-chan struct{}, error) {
	close(consumer.started)
	return ResourceStreamStatusStreaming, consumer.done, nil
}

func (consumer *streamingStorageConsumer) OnResourceUpdated(ResourceWatcherEvent, logr.Logger) {
	consumer.updateCount.Add(1)
	consumer.updates <- struct{}{}
}

// Verifies that LogStorage stops the request-specific watcher once streaming starts
// and delivers subsequent resource updates only through the existing lifecycle callback queue.
func TestLogStorageHandsStreamingUpdatesToLifecycleCallbacks(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	parent := &watchedStorage{
		object: &watchedStorageObject{
			TypeMeta:   metav1.TypeMeta{APIVersion: "test/v1", Kind: "Parent"},
			ObjectMeta: metav1.ObjectMeta{Name: "parent", UID: "parent-uid"},
		},
		watcher: watch.NewRaceFreeFake(),
	}
	consumer := &streamingStorageConsumer{
		started: make(chan struct{}), done: make(chan struct{}), updates: make(chan struct{}, 4),
	}
	storage, storageErr := NewLogStorage(parent, consumer,
		func() *storageOptions { return &storageOptions{} },
		func(inner rest.ResourceStreamer) runtime.Object {
			return &streamingStorageResponse{ResourceStreamer: inner}
		},
		schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "LogResponse"})
	require.NoError(t, storageErr)
	defer storage.Destroy()
	response, getErr := storage.Get(ctx, "parent", &storageOptions{LogOptions: LogOptions{Follow: true}})
	require.NoError(t, getErr)
	reader, _, _, streamErr := response.(*streamingStorageResponse).InputStream(ctx, "", "")
	require.NoError(t, streamErr)
	defer func() { require.NoError(t, reader.Close()) }()
	parent.watcher.Add(parent.object)
	select {
	case <-consumer.started:
	case <-ctx.Done():
		require.FailNow(t, "log streaming did not start")
	}
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		watchers, found := storage.resourceWatchers.Load(parent.object.UID)
		require.True(t, found)
		count := 0
		watchers.Range(func(*resourceWatcher, bool) bool { count++; return true })
		return count == 0, nil
	}))
	select {
	case <-consumer.updates:
	case <-ctx.Done():
		require.FailNow(t, "initial lifecycle callback was not delivered")
	}
	parent.watcher.Modify(parent.object)
	select {
	case <-consumer.updates:
	case <-ctx.Done():
		require.FailNow(t, "streaming lifecycle callback was not delivered")
	}
	require.Equal(t, int32(2), consumer.updateCount.Load())
	close(consumer.done)
	contents, readErr := io.ReadAll(reader)
	require.NoError(t, readErr)
	require.Empty(t, contents)
}

// Verifies that log storage initializes its disposal signal and can be destroyed repeatedly
// without a watcher or any registered client streams.
func TestLogStorageDisposeWithoutWatchers(t *testing.T) {
	storage, storageErr := NewLogStorage(&unusedStorage{}, &unusedStreamer{},
		func() *storageOptions { return &storageOptions{} },
		func(rest.ResourceStreamer) runtime.Object { return &metav1.Status{} },
		schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "LogResponse"})
	require.NoError(t, storageErr)
	storage.Destroy()
	storage.Destroy()
	select {
	case <-storage.disposeCh:
	default:
		require.FailNow(t, "storage did not close its disposal signal")
	}
}

// Verifies that resource watchers deliver valid events and stop idempotently without
// accepting new events or leaving a result channel open.
func TestResourceWatcherStopsDelivery(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	watcher := newResourceWatcher()
	defer watcher.Stop()
	require.True(t, watcher.Queue(ResourceWatcherEvent{Type: watch.Added}))
	select {
	case event, open := <-watcher.ResultChan():
		require.True(t, open)
		require.Equal(t, watch.Added, event.Type)
	case <-ctx.Done():
		require.FailNow(t, "resource event was not delivered")
	}
	watcher.Stop()
	watcher.Stop()
	require.False(t, watcher.Queue(ResourceWatcherEvent{Type: watch.Modified}))
	select {
	case _, open := <-watcher.ResultChan():
		require.False(t, open)
	case <-ctx.Done():
		require.FailNow(t, "resource watcher did not close")
	}
}
