// Copyright (c) Microsoft Corporation. All rights reserved.

package v1

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"

	"github.com/microsoft/usvc-apiserver/internal/contextdata"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

type ResourceStreamStatus string

const (
	ResourceStreamStatusStreaming ResourceStreamStatus = "streaming"
	ResourceStreamStatusDone      ResourceStreamStatus = "done"
	ResourceStreamStatusNotReady  ResourceStreamStatus = "not_ready"
	watcherRestartInterval                             = 30 * time.Second
	watcherCreationRetryInterval                       = 2 * time.Second
)

// ResourceLogStreamFactory is an entity that knows how to stream logs for a given resource.
// +kubebuilder:object:generate=false
type ResourceLogStreamer interface {
	// StreamLogs returns a boolean indicating if the logs are ready to be streamed, a channel that will be closed when the logs are done streaming, and an error if one occurred.
	StreamLogs(ctx context.Context, dest io.Writer, obj apiserver_resource.Object, opts *LogOptions, log logr.Logger) (ResourceStreamStatus, <-chan struct{}, error)

	// Callback when a resource of the type this streamer is registered for is deleted to allow for resource cleanup.
	OnResourceUpdated(evt ResourceWatcherEvent, log logr.Logger)

	// Disposes the log streamer and underlying resources.
	Dispose() error
}

// +kubebuilder:object:generate=false
type ResourceWatcherEvent struct {
	Type   watch.EventType
	Object apiserver_resource.Object
}

// Resource for receiving parent resource updates for log streaming implementations.
// Based on watch.Interface
// +kubebuilder:object:generate=false
type resourceWatcher struct {
	resultChan       *concurrency.UnboundedChan[ResourceWatcherEvent]
	resultChanCtx    context.Context
	resultChanCancel context.CancelFunc
	onStop           func()
	stopped          bool
	mutex            *sync.Mutex
}

func (rw *resourceWatcher) Stop() {
	// We need to lock to prevent a possible race condition between sending events to the watcher and the watcher being removed
	rw.mutex.Lock()

	if rw.stopped {
		rw.mutex.Unlock()
		return
	}

	rw.stopped = true
	rw.resultChanCancel()
	onStop := rw.onStop
	rw.mutex.Unlock()

	if onStop != nil {
		onStop()
	}
}

func (rw *resourceWatcher) ResultChan() <-chan ResourceWatcherEvent {
	return rw.resultChan.Out
}

func (rw *resourceWatcher) OnStop(onStop func()) {
	rw.mutex.Lock()
	defer rw.mutex.Unlock()
	rw.onStop = onStop
}

func (rw *resourceWatcher) Queue(evt ResourceWatcherEvent) bool {
	rw.mutex.Lock()
	defer rw.mutex.Unlock()
	if !rw.stopped {
		rw.resultChan.In <- evt
		return true
	} else {
		return false
	}
}

func newResourceWatcher() *resourceWatcher {
	resultChanCtx, resultChanCancel := context.WithCancel(context.Background())
	return &resourceWatcher{
		resultChanCtx:    resultChanCtx,
		resultChanCancel: resultChanCancel,
		resultChan:       concurrency.NewUnboundedChan[ResourceWatcherEvent](resultChanCtx),
		stopped:          false,
		mutex:            &sync.Mutex{},
	}
}

// +kubebuilder:object:generate=false
type LogStorage struct {
	// The data storage for the parent object kind
	parentKindStorage registry_rest.StandardStorage

	// Factory for creating new log stream sessions
	logStreamer ResourceLogStreamer

	// The queue used for sending resource events to the log streamer
	logStreamerEventQueue       *resiliency.WorkQueue
	logStreamerEventQueueCancel context.CancelFunc

	// Cache of most recent event for each parent resource
	// Keyed off UID to handle the scenario where a resource with the same name is created multiple times
	// Used to send the current status of a resource immediately to a new watcher
	mostRecentResourceEvents *syncmap.Map[types.UID, ResourceWatcherEvent]

	// Resource watchers that want to be updated when parent resources change
	resourceWatchers *syncmap.Map[types.UID, *syncmap.Map[*resourceWatcher, bool]]

	// Mutex for synchronizing access to the LogStorage data structures
	mutex *sync.Mutex

	// The Kubernetes watcher for the parent object kind
	watcher watch.Interface

	// Channel to signal when the log storage is being disposed
	disposeCh chan struct{}
	disposed  bool
}

func NewLogStorage(parentKindStorage registry_rest.StandardStorage, logStreamer ResourceLogStreamer) (*LogStorage, error) {
	if parentKindStorage == nil {
		return nil, fmt.Errorf("parent object kind storage is required for LogStorage to function")
	}
	if logStreamer == nil {
		return nil, fmt.Errorf("log streamer is required for LogStorage to function")
	}

	logStreamerEventQueueCtx, logStreamerEventQueueCancel := context.WithCancel(context.Background())

	return &LogStorage{
		parentKindStorage:           parentKindStorage,
		logStreamer:                 logStreamer,
		logStreamerEventQueue:       resiliency.NewWorkQueue(logStreamerEventQueueCtx, resiliency.DefaultConcurrency),
		logStreamerEventQueueCancel: logStreamerEventQueueCancel,
		mostRecentResourceEvents:    &syncmap.Map[types.UID, ResourceWatcherEvent]{},
		resourceWatchers:            &syncmap.Map[types.UID, *syncmap.Map[*resourceWatcher, bool]]{},
		mutex:                       &sync.Mutex{},
		watcher:                     nil,
	}, nil
}

func (ls *LogStorage) watchResource(uid types.UID, log logr.Logger) (*resourceWatcher, error) {
	// Lock the log storage mutex to ensure notifications aren't being sent while a new watcher is registered
	ls.mutex.Lock()
	defer ls.mutex.Unlock()

	if ls.disposed {
		return nil, fmt.Errorf("log storage has been disposed")
	}

	resourceWatchers, _ := ls.resourceWatchers.LoadOrStoreNew(uid, func() *syncmap.Map[*resourceWatcher, bool] {
		return &syncmap.Map[*resourceWatcher, bool]{}
	})

	rw := newResourceWatcher()
	rw.OnStop(func() {
		resourceWatchers.Delete(rw)
	})

	if ls.watcher == nil {
		watchErr := ls.watchResourceEvents(log)
		if watchErr != nil {
			return nil, watchErr
		}
	} else {
		if event, found := ls.mostRecentResourceEvents.Load(uid); found {
			rw.Queue(event)
		}
	}

	_, _ = resourceWatchers.LoadOrStore(rw, true)

	return rw, nil
}

// Event processing loop for the LogStorage resource watch
func (ls *LogStorage) watchResourceEvents(log logr.Logger) error {
	// ls.mutex is locked on entry

	sendInitialEvents := true
	listOpts := metainternalversion.ListOptions{
		Watch:                true,
		SendInitialEvents:    &sendInitialEvents,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
	}

	initialWatcher, watchErr := ls.parentKindStorage.Watch(context.Background(), &listOpts)
	if watchErr != nil {
		return watchErr
	}

	// The ls.watcher will be cleaned up when the Destroy method of the log storage instance is called.
	ls.watcher = initialWatcher

	go func() {
		// We've seen in tests scenarios where the watcher doesn't always return every event.
		// This can lead to the log streamer never starting streaming for a given resource. To mitigate this,
		// we are periodically restarting the watcher to ensure we don't miss any events (the watcher always
		// emits the current state of resources when it starts).

		ls.mutex.Lock()
		if ls.watcher == nil {
			ls.mutex.Unlock()
			return
		}
		watcherResultChan := ls.watcher.ResultChan()
		ls.mutex.Unlock()
		timer := time.NewTimer(watcherRestartInterval)

		for {
			select {
			case <-timer.C:
				timer.Stop()
				log.V(1).Info("Restarting parent resource watcher...")
				ls.mutex.Lock()

				func() {
					defer ls.mutex.Unlock()
					if ls.disposed {
						return
					}

					// Use a separate goroutine to stop the old watcher and drain its channel
					// so that events from the new watcher are processed expeditiously.
					//
					// This has to be initiated before making a call to create a new watcher.
					// If not, we can end up in a deadlock situation where the storage is trying to deliver
					// a watch event (holding the watch set lock in shared mode) while at the same time
					// we are making a call to create a new watcher (which tries to take the watch set lock
					// in exclusive mode).
					go func(w watch.Interface) {
						stopWatcher(w)
						log.V(1).Info("Old parent resource watcher stopped")
					}(ls.watcher)

					newWatcher, newWatchErr := ls.parentKindStorage.Watch(context.Background(), &listOpts)
					if newWatchErr != nil {
						log.V(1).Info("Failed to re-establish parent resource watcher", "Error", newWatchErr)
						timer.Reset(watcherCreationRetryInterval)
						return
					}

					log.V(1).Info("New parent resource watcher created")

					ls.watcher = newWatcher
					watcherResultChan = newWatcher.ResultChan()
					timer.Reset(watcherRestartInterval)
				}()

			case <-ls.disposeCh:
				// The watcher (if any) will be stopped in the Destroy method
				timer.Stop()
				return

			case evt, isOpen := <-watcherResultChan:
				if !isOpen {
					watcherResultChan = nil // Do not read zero-values from a stopped watcher
				} else {
					ls.resourceEventHandler(evt, log)
				}
			}
		}
	}()

	return nil
}

func (ls *LogStorage) resourceEventHandler(evt watch.Event, log logr.Logger) {
	// Lock the log storage mutex to ensure we don't try to notify resource watchers at the same time a new resource watcher is being added
	ls.mutex.Lock()
	defer ls.mutex.Unlock()

	if ls.disposed {
		return
	}

	if evt.Type == watch.Error {
		log.V(1).Info("Saw error event for parent resource: %v", evt.Object)
		return
	}

	apiObj, isApiObj := evt.Object.(apiserver_resource.Object)
	if !isApiObj {
		log.V(1).Info("Parent resource was not an API object, skipping")
		return
	}

	rwevt := ResourceWatcherEvent{
		Type:   evt.Type,
		Object: apiObj,
	}

	resourceName := commonapi.GetNamespacedNameWithKindForResourceObject(apiObj).String()
	startTime := time.Now()
	const longNotificationThreshold = 2 * time.Second
	defer func() {
		duration := time.Since(startTime)
		if duration > longNotificationThreshold {
			log.Info("Warning: notifying resource watchers of parent resource event took longer than expected",
				"Event", rwevt.Type,
				"Resource", resourceName,
				"Duration", duration,
			)
		}
	}()

	activeResourceWatchers, found := ls.resourceWatchers.Load(apiObj.GetObjectMeta().GetUID())
	if found {
		watcherCount := 0
		activeResourceWatchers.Range(func(rw *resourceWatcher, _ bool) bool {
			if rw.Queue(rwevt) {
				watcherCount += 1
			}
			return true
		})
		if watcherCount > 0 {
			log.V(1).Info("Completed notifying resource watchers of parent resource event",
				"Event", rwevt.Type,
				"Resource", resourceName,
				"WatcherCount", watcherCount,
			)
		}
	}

	_ = ls.logStreamerEventQueue.Enqueue(func(_ context.Context) {
		ls.logStreamer.OnResourceUpdated(rwevt, log)
	})

	// Store the event so subsequent resource watchers can receive it as the latest value
	ls.mostRecentResourceEvents.Store(
		apiObj.GetObjectMeta().GetUID(),
		rwevt,
	)
}

func (ls *LogStorage) New() runtime.Object {
	return &LogStreamer{}
}

func (ls *LogStorage) Destroy() {
	ls.mutex.Lock()

	if ls.disposed {
		ls.mutex.Unlock()
		return
	}

	ls.disposed = true
	close(ls.disposeCh)
	watcher := ls.watcher
	ls.watcher = nil
	ls.logStreamerEventQueueCancel()
	ls.mutex.Unlock()

	// Logs do not have independent storage/lifecycle, they will be deleted when the parent is deleted,
	// but we do need to stop the watcher, and we do not want to hold the storage lock while doing so.
	if watcher != nil {
		stopWatcher(watcher)
	}
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
		vErr := options.Validate()
		if vErr != nil {
			return nil, false, "", apierrors.NewBadRequest(vErr.Error())
		}

		getOpts := metav1.GetOptions{}
		obj, err := ls.parentKindStorage.Get(ctx, resourceName, &getOpts)
		if err != nil {
			return nil, false, "", err
		}

		apiObj, isAPIObj := obj.(apiserver_resource.Object)
		if !isAPIObj {
			return nil, false, "", apierrors.NewInternalError(fmt.Errorf("parent storage returned object of wrong type: %s", obj.GetObjectKind().GroupVersionKind().String()))
		}

		log := contextdata.GetContextLogger(ctx)

		reader, writer := io.Pipe()
		watcher, watcherErr := ls.watchResource(apiObj.GetObjectMeta().GetUID(), log)
		if watcherErr != nil {
			return nil, false, "", apierrors.NewInternalError(fmt.Errorf("failed to register watcher for parent resource: %w", watcherErr))
		}

		log = log.WithName("Logstorage").WithValues(
			"Kind", apiObj.GetObjectKind().GroupVersionKind().String(),
			"Name", apiObj.GetObjectMeta().Name,
			"UID", apiObj.GetObjectMeta().GetUID(),
			"Options", options.String(),
		)

		log.V(1).Info("Preparing log stream")

		go func() {
			defer func() {
				closeErr := writer.Close()
				if closeErr != nil {
					log.Error(closeErr, "Failed to close log stream writer")
				}
			}()
			defer watcher.Stop()

			for {
				select {
				case <-ctx.Done():
					log.V(1).Info("Context was canceled, closing log stream")
					return
				case evt := <-watcher.ResultChan():
					if evt.Type == watch.Deleted {
						log.V(1).Info("Parent resource was deleted, closing log stream")
						return
					}

					resourceStreamStatus, doneStreaming, logStreamErr := ls.logStreamer.StreamLogs(ctx, writer, evt.Object, options, log)
					// There was an error attempting to stream logs, close the stream and exit
					if logStreamErr != nil {
						log.Error(logStreamErr, "Failed to initialize log stream")
						return
					}

					// The resource was ready to stream
					if resourceStreamStatus == ResourceStreamStatusStreaming {
						// Stop the watcher since we're streaming logs now (this can be called safely multiple times)
						watcher.Stop()
						log.V(1).Info("Starting log streaming for resource")
						// Wait for streaming to complete
						<-doneStreaming
						log.V(1).Info("Log streaming for resource completed")
						return
					}

					if resourceStreamStatus == ResourceStreamStatusDone {
						log.V(1).Info("Resource is done streaming logs")
						return
					}

					// The object wasn't ready to start streaming, but follow isn't set, so close the stream and exit
					if resourceStreamStatus != ResourceStreamStatusStreaming && !options.Follow {
						log.V(1).Info("Resource not ready to stream logs")
						return
					}

					// We're following logs and the parent isn't ready to stream yet, wait for an update event to try again
					log.V(1).Info("Waiting for parent resource to be ready to stream logs")
				}
			}
		}()

		return reader /* flushOnNewData */, true, "text/plain", nil
	}), nil
}

func (ls *LogStorage) NewGetOptions() (runtime.Object, bool, string) {
	return &LogOptions{}, false, ""
}

func stopWatcher(w watch.Interface) {
	// We need to drain the watcher result channel because it is an unbuffered channel
	// that the Tilt API server storage layer synchronously writes to it as part of handling object changes.
	// If we simply abandon the channel with some pending writes, the corresponding update will block forever.
	// Even calling Stop() on the watcher will NOT help, because the Stop() method tries to take an exclusive lock
	// on the "watch set" that the watcher belongs to before closing the watcher channel,
	// and lock is held (in shared mode) by the goroutine that is writing to the channel
	// to broadcast object update events.
	// By calling drain() on a separate goroutine we ensure that the channel
	// will continue to be drained until the call to Stop() succeeds.
	go drain(w.ResultChan())

	w.Stop()
}

func drain[T any](ch <-chan T) {
	if ch == nil {
		return
	}

	for {
		_, isOpen := <-ch
		if !isOpen {
			return
		}
	}
}

var _ registry_rest.Storage = (*LogStorage)(nil)
var _ registry_rest.StorageMetadata = (*LogStorage)(nil)
var _ registry_rest.GetterWithOptions = (*LogStorage)(nil)
var _ registry_rest.GroupVersionKindProvider = (*LogStorage)(nil)
