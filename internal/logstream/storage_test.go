/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logstream

import (
	"context"
	"fmt"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/microsoft/dcp/pkg/resiliency"
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

type orderedStorageConsumer struct {
	logStorageTestStreamer
	ctx          context.Context
	firstStarted chan struct{}
	releaseFirst chan struct{}
	events       chan ResourceWatcherEvent
}

type registrationStorageConsumer struct {
	logStorageTestStreamer
	ctx                 context.Context
	calls               chan string
	events              chan ResourceWatcherEvent
	releaseRegistration <-chan struct{}
	registered          atomic.Bool
	done                chan struct{}
}

func (consumer *registrationStorageConsumer) StreamLogs(
	ctx context.Context, _ io.Writer, obj apiserver_resource.Object, _ *storageOptions, _ logr.Logger,
) (ResourceStreamStatus, <-chan struct{}, error) {
	select {
	case consumer.calls <- obj.GetObjectMeta().ResourceVersion:
	case <-ctx.Done():
		return ResourceStreamStatusDone, nil, ctx.Err()
	}
	if consumer.releaseRegistration != nil {
		select {
		case <-consumer.releaseRegistration:
		case <-ctx.Done():
			return ResourceStreamStatusDone, nil, ctx.Err()
		}
	}
	consumer.registered.Store(true)
	return ResourceStreamStatusStreaming, consumer.done, nil
}

func (consumer *registrationStorageConsumer) OnResourceUpdated(event ResourceWatcherEvent, _ logr.Logger) {
	if event.Type == watch.Deleted && consumer.registered.Load() {
		close(consumer.done)
	}
	select {
	case consumer.events <- event:
	case <-consumer.ctx.Done():
	}
}

// Verifies that LogStorage readiness checks consume only the latest processed state,
// including deletion after an earlier readiness notification has already been consumed.
func TestLogStorageReadinessUsesLatestResourceState(t *testing.T) {
	t.Parallel()
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted-%t", deleted), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			parent := &watchedStorage{
				object: &watchedStorageObject{
					TypeMeta:   metav1.TypeMeta{APIVersion: "test/v1", Kind: "Parent"},
					ObjectMeta: metav1.ObjectMeta{Name: "parent", UID: "parent-uid", ResourceVersion: "1"},
				},
				watcher: watch.NewRaceFreeFake(),
			}
			consumer := &registrationStorageConsumer{
				ctx: ctx, calls: make(chan string, 1),
				events: make(chan ResourceWatcherEvent, 1), done: make(chan struct{}),
			}
			storage, storageErr := NewLogStorage(parent, consumer,
				func() *storageOptions { return &storageOptions{} },
				func(inner rest.ResourceStreamer) runtime.Object {
					return &streamingStorageResponse{ResourceStreamer: inner}
				},
				schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "LogResponse"})
			require.NoError(t, storageErr)
			defer storage.Destroy()
			watcher, watchErr := storage.watchResource(ctx, parent.object.UID, logr.Discard())
			require.NoError(t, watchErr)
			defer watcher.Stop()
			for version := 1; version <= 3; version++ {
				event := watch.Event{Type: watch.Modified, Object: &watchedStorageObject{
					TypeMeta: parent.object.TypeMeta,
					ObjectMeta: metav1.ObjectMeta{
						Name: parent.object.Name, UID: parent.object.UID, ResourceVersion: fmt.Sprint(version),
					},
				}}
				if version == 1 {
					event.Type = watch.Added
				} else if version == 3 && deleted {
					event.Type = watch.Deleted
				}
				storage.resourceEventHandler(event, logr.Discard())
				select {
				case delivered, open := <-consumer.events:
					require.True(t, open)
					require.Equal(t, event.Type, delivered.Type)
				case <-ctx.Done():
					t.Fatal("lifecycle event was not delivered")
				}
				if version == 1 {
					select {
					case _, open := <-watcher.ResultChan():
						require.True(t, open)
					case <-ctx.Done():
						t.Fatal("initial readiness notification was not delivered")
					}
				}
			}
			status, _, streamErr := storage.streamLatestResourceLogs(ctx, watcher, io.Discard,
				&storageOptions{LogOptions: LogOptions{Follow: true}}, logr.Discard())
			require.NoError(t, streamErr)
			if deleted {
				require.Equal(t, ResourceStreamStatusDone, status)
				require.Empty(t, consumer.calls)
				response, getErr := storage.Get(ctx, parent.object.Name, &storageOptions{LogOptions: LogOptions{Follow: true}})
				require.NoError(t, getErr)
				reader, _, _, inputErr := response.(*streamingStorageResponse).InputStream(ctx, "", "")
				require.NoError(t, inputErr)
				defer reader.Close()
				output, readErr := io.ReadAll(reader)
				require.NoError(t, readErr)
				require.Empty(t, output)
				require.Empty(t, consumer.calls)
			} else {
				require.Equal(t, ResourceStreamStatusStreaming, status)
				select {
				case version, open := <-consumer.calls:
					require.True(t, open)
					require.Equal(t, "3", version)
				case <-ctx.Done():
					t.Fatal("latest resource state was not used")
				}
			}
			select {
			case <-watcher.ResultChan():
				t.Fatal("consumed state left a duplicate readiness notification")
			default:
			}
		})
	}
}

// Verifies that LogStorage allows concurrent requests for one resource, blocks deletion until their
// processing returns, and lets other resources progress while those requests are active.
func TestLogStorageDeletionWaitsForConcurrentRequests(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	parent := &watchedStorage{
		object: &watchedStorageObject{
			TypeMeta:   metav1.TypeMeta{APIVersion: "test/v1", Kind: "Parent"},
			ObjectMeta: metav1.ObjectMeta{Name: "parent", UID: "parent-uid", ResourceVersion: "1"},
		},
		watcher: watch.NewRaceFreeFake(),
	}
	releaseRegistration := make(chan struct{}, 1)
	defer close(releaseRegistration)
	consumer := &registrationStorageConsumer{
		ctx: ctx, calls: make(chan string, 1), events: make(chan ResourceWatcherEvent, 3),
		done: make(chan struct{}), releaseRegistration: releaseRegistration,
	}
	storage, storageErr := NewLogStorage(parent, consumer,
		func() *storageOptions { return &storageOptions{} },
		func(inner rest.ResourceStreamer) runtime.Object {
			return &streamingStorageResponse{ResourceStreamer: inner}
		},
		schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "LogResponse"})
	require.NoError(t, storageErr)
	defer storage.Destroy()
	storage.logStreamerResourceQueueCancel()
	queueCtx, cancelQueue := context.WithCancel(ctx)
	storage.logStreamerResourceQueue = resiliency.NewWorkQueue(queueCtx, 2)
	storage.logStreamerResourceQueueCancel = cancelQueue
	storage.lifetimeCtx = queueCtx
	response, getErr := storage.Get(ctx, parent.object.Name, &storageOptions{LogOptions: LogOptions{Follow: true}})
	require.NoError(t, getErr)
	reader, _, _, inputErr := response.(*streamingStorageResponse).InputStream(ctx, "", "")
	require.NoError(t, inputErr)
	defer reader.Close()
	secondResponse, secondGetErr := storage.Get(ctx, parent.object.Name, &storageOptions{LogOptions: LogOptions{Follow: true}})
	require.NoError(t, secondGetErr)
	secondReader, _, _, secondInputErr := secondResponse.(*streamingStorageResponse).InputStream(ctx, "", "")
	require.NoError(t, secondInputErr)
	defer secondReader.Close()
	storage.resourceEventHandler(watch.Event{Type: watch.Added, Object: parent.object}, logr.Discard())
	for requestIndex := 0; requestIndex < 2; requestIndex++ {
		select {
		case <-consumer.calls:
		case <-ctx.Done():
			t.Fatal("stream requests did not process the resource concurrently")
		}
	}
	select {
	case event, open := <-consumer.events:
		require.True(t, open)
		require.Equal(t, watch.Added, event.Type)
	case <-ctx.Done():
		t.Fatal("initial lifecycle event was not delivered")
	}
	state, found := storage.resourceStates.Load(parent.object.UID)
	require.True(t, found)
	if state.processing.TryLock() {
		state.processing.Unlock()
		t.Fatal("registration did not hold the resource processing lock")
	}
	storage.resourceEventHandler(watch.Event{Type: watch.Deleted, Object: parent.object}, logr.Discard())
	other := &watchedStorageObject{
		TypeMeta:   parent.object.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{Name: "other", UID: "other-uid", ResourceVersion: "2"},
	}
	storage.resourceEventHandler(watch.Event{Type: watch.Added, Object: other}, logr.Discard())
	select {
	case event, open := <-consumer.events:
		require.True(t, open)
		require.Equal(t, other.UID, event.Object.GetObjectMeta().UID)
	case <-ctx.Done():
		t.Fatal("another resource was blocked by registration")
	}
	releaseRegistration <- struct{}{}
	releaseRegistration <- struct{}{}
	select {
	case event, open := <-consumer.events:
		require.True(t, open)
		require.Equal(t, watch.Deleted, event.Type)
		require.True(t, consumer.registered.Load())
	case <-ctx.Done():
		t.Fatal("deletion did not follow registration")
	}
	output, readErr := io.ReadAll(reader)
	require.NoError(t, readErr)
	require.Empty(t, output)
	secondOutput, secondReadErr := io.ReadAll(secondReader)
	require.NoError(t, secondReadErr)
	require.Empty(t, secondOutput)
}

// Verifies that LogStorage.Destroy cancels in-flight registration and closes its client response
// without waiting for resource initialization to finish.
func TestLogStorageDestroyCancelsRegistration(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	parent := &watchedStorage{
		object: &watchedStorageObject{
			TypeMeta:   metav1.TypeMeta{APIVersion: "test/v1", Kind: "Parent"},
			ObjectMeta: metav1.ObjectMeta{Name: "parent", UID: "parent-uid", ResourceVersion: "1"},
		},
		watcher: watch.NewRaceFreeFake(),
	}
	consumer := &registrationStorageConsumer{
		ctx: ctx, calls: make(chan string, 1), events: make(chan ResourceWatcherEvent, 1),
		done: make(chan struct{}), releaseRegistration: make(chan struct{}),
	}
	storage, storageErr := NewLogStorage(parent, consumer,
		func() *storageOptions { return &storageOptions{} },
		func(inner rest.ResourceStreamer) runtime.Object {
			return &streamingStorageResponse{ResourceStreamer: inner}
		},
		schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "LogResponse"})
	require.NoError(t, storageErr)
	defer storage.Destroy()
	response, getErr := storage.Get(ctx, parent.object.Name, &storageOptions{LogOptions: LogOptions{Follow: true}})
	require.NoError(t, getErr)
	reader, _, _, inputErr := response.(*streamingStorageResponse).InputStream(ctx, "", "")
	require.NoError(t, inputErr)
	defer reader.Close()
	storage.resourceEventHandler(watch.Event{Type: watch.Added, Object: parent.object}, logr.Discard())
	select {
	case <-consumer.calls:
	case <-ctx.Done():
		t.Fatal("stream registration did not start")
	}
	storage.Destroy()
	_, readErr := io.ReadAll(reader)
	require.ErrorIs(t, readErr, context.Canceled)
	require.False(t, consumer.registered.Load())
}

func (consumer *orderedStorageConsumer) OnResourceUpdated(event ResourceWatcherEvent, _ logr.Logger) {
	if event.Object.GetObjectMeta().ResourceVersion == "1" {
		close(consumer.firstStarted)
		select {
		case <-consumer.releaseFirst:
		case <-consumer.ctx.Done():
			return
		}
	}
	select {
	case consumer.events <- event:
	case <-consumer.ctx.Done():
	}
}

// Verifies that LogStorage delivers each resource's events in FIFO order without blocking
// other resources, yields between events, and releases idle event queues for subsequent reuse.
func TestLogStorageProcessesResourceEventsInOrder(t *testing.T) {
	t.Parallel()
	for _, workerCount := range []uint8{1, 2} {
		t.Run(fmt.Sprintf("%d-workers", workerCount), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			consumer := &orderedStorageConsumer{
				ctx: ctx, firstStarted: make(chan struct{}),
				releaseFirst: make(chan struct{}, 1), events: make(chan ResourceWatcherEvent, 8),
			}
			defer close(consumer.releaseFirst)
			storage, storageErr := NewLogStorage(&unusedStorage{}, consumer,
				func() *storageOptions { return &storageOptions{} },
				func(rest.ResourceStreamer) runtime.Object { return &metav1.Status{} },
				schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "LogResponse"})
			require.NoError(t, storageErr)
			defer storage.Destroy()
			storage.logStreamerResourceQueueCancel()
			queueCtx, cancelQueue := context.WithCancel(ctx)
			storage.logStreamerResourceQueue = resiliency.NewWorkQueue(queueCtx, workerCount)
			storage.logStreamerResourceQueueCancel = cancelQueue
			storage.lifetimeCtx = queueCtx

			resourceEvent := func(uid types.UID, version string, eventType watch.EventType) ResourceWatcherEvent {
				return ResourceWatcherEvent{Type: eventType, Object: &watchedStorageObject{
					TypeMeta:   metav1.TypeMeta{APIVersion: "test/v1", Kind: "Parent"},
					ObjectMeta: metav1.ObjectMeta{Name: "parent", UID: uid, ResourceVersion: version},
				}}
			}
			expected := []ResourceWatcherEvent{
				resourceEvent("original-uid", "1", watch.Added),
				resourceEvent("original-uid", "2", watch.Modified),
				resourceEvent("original-uid", "3", watch.Modified),
				resourceEvent("original-uid", "4", watch.Deleted),
				resourceEvent("replacement-uid", "5", watch.Added),
			}
			storage.resourceEventHandler(watch.Event{Type: expected[0].Type, Object: expected[0].Object}, logr.Discard())
			select {
			case <-consumer.firstStarted:
			case <-ctx.Done():
				t.Fatal("first resource event did not start")
			}
			for _, event := range expected[1:] {
				storage.resourceEventHandler(watch.Event{Type: event.Type, Object: event.Object}, logr.Discard())
			}
			expectEvent := func(wanted ResourceWatcherEvent) {
				select {
				case actual, open := <-consumer.events:
					require.True(t, open)
					require.Equal(t, wanted, actual)
				case <-ctx.Done():
					t.Fatal("resource event was not delivered")
				}
			}
			if workerCount == 2 {
				expectEvent(expected[4])
				consumer.releaseFirst <- struct{}{}
				expectEvent(expected[0])
			} else {
				consumer.releaseFirst <- struct{}{}
				expectEvent(expected[0])
				expectEvent(expected[4])
			}
			for _, event := range expected[1:4] {
				expectEvent(event)
			}
			require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
				storage.mutex.Lock()
				defer storage.mutex.Unlock()
				return len(storage.pendingResourceEvents) == 0, nil
			}))
			nextEvent := resourceEvent("replacement-uid", "6", watch.Modified)
			storage.resourceEventHandler(watch.Event{Type: nextEvent.Type, Object: nextEvent.Object}, logr.Discard())
			expectEvent(nextEvent)
		})
	}
}

// Verifies that LogStorage.Destroy discards pending events and prevents a ready resource
// from starting another callback, while allowing an already running callback to finish.
func TestLogStorageDestroyDiscardsPendingResourceEvents(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	consumer := &orderedStorageConsumer{
		ctx: ctx, firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}, 1), events: make(chan ResourceWatcherEvent, 2),
	}
	defer close(consumer.releaseFirst)
	storage, storageErr := NewLogStorage(&unusedStorage{}, consumer,
		func() *storageOptions { return &storageOptions{} },
		func(rest.ResourceStreamer) runtime.Object { return &metav1.Status{} },
		schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "LogResponse"})
	require.NoError(t, storageErr)
	defer storage.Destroy()
	original := &watchedStorageObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "test/v1", Kind: "Parent"},
		ObjectMeta: metav1.ObjectMeta{Name: "parent", UID: "parent-uid", ResourceVersion: "1"},
	}
	storage.resourceEventHandler(watch.Event{Type: watch.Added, Object: original}, logr.Discard())
	select {
	case <-consumer.firstStarted:
	case <-ctx.Done():
		t.Fatal("first resource event did not start")
	}
	deleted := &watchedStorageObject{
		TypeMeta:   original.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{Name: original.Name, UID: original.UID, ResourceVersion: "2"},
	}
	storage.resourceEventHandler(watch.Event{Type: watch.Deleted, Object: deleted}, logr.Discard())
	storage.Destroy()
	storage.mutex.Lock()
	pendingCount := len(storage.pendingResourceEvents)
	storage.mutex.Unlock()
	require.Zero(t, pendingCount)
	storage.processResourceEvent(ctx, original.UID, logr.Discard())
	storage.resourceEventHandler(watch.Event{Type: watch.Modified, Object: deleted}, logr.Discard())
	consumer.releaseFirst <- struct{}{}
	select {
	case event, open := <-consumer.events:
		require.True(t, open)
		require.Equal(t, watch.Added, event.Type)
	case <-ctx.Done():
		t.Fatal("running resource callback did not finish")
	}
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

// Verifies that resourceWatcher coalesces unread notifications and stops idempotently
// without accepting new notifications or leaving its result channel open.
func TestResourceWatcherStopsDelivery(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	watcher := newResourceWatcher(nil)
	defer watcher.Stop()
	require.True(t, watcher.Notify())
	require.True(t, watcher.Notify())
	require.True(t, watcher.Notify())
	select {
	case _, open := <-watcher.ResultChan():
		require.True(t, open)
	case <-ctx.Done():
		require.FailNow(t, "resource event was not delivered")
	}
	select {
	case <-watcher.ResultChan():
		t.Fatal("unread notifications were not coalesced")
	default:
	}
	watcher.Stop()
	watcher.Stop()
	require.False(t, watcher.Notify())
	select {
	case _, open := <-watcher.ResultChan():
		require.False(t, open)
	case <-ctx.Done():
		require.FailNow(t, "resource watcher did not close")
	}
}
