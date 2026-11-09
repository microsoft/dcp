/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logstream

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
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"

	"github.com/microsoft/dcp/internal/contextdata"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/container"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/syncmap"
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
type Options interface {
	runtime.Object
	Validate() error
	String() string
	GetFollow() bool
}

type ResourceLogStreamer[O Options] interface {
	// Preflight is called synchronously by the LogStorage before any streaming goroutine is launched.
	// It offers an opportunity for a streamer to surface a proper Kubernetes API error to the client,
	// because once StreamLogs starts running its results are delivered through an io.Pipe
	// whose (successful) HTTP response status has already been committed.
	// Implementations should only perform cheap, side-effect-free checks here
	// (e.g. validating that the requested log source is compatible with the resource's configuration).
	// Returning nil allows streaming to proceed; returning an error short-circuits the request.
	Preflight(obj apiserver_resource.Object, opts O) error

	// Returns the readiness status, a completion channel for asynchronous delivery, and any error.
	StreamLogs(ctx context.Context, dest io.Writer, obj apiserver_resource.Object, opts O, log logr.Logger) (ResourceStreamStatus, <-chan struct{}, error)

	// Receives lifecycle events in order for each resource UID.
	// Callbacks for different resources may run concurrently.
	OnResourceUpdated(evt ResourceWatcherEvent, log logr.Logger)

	// Disposes the log streamer and underlying resources.
	Dispose() error
}

// +kubebuilder:object:generate=false
type ResourceWatcherEvent struct {
	Type   watch.EventType
	Object apiserver_resource.Object
}

type resourceLogState struct {
	processing sync.RWMutex
	latest     ResourceWatcherEvent
}

// Notifies a log request that a newer resource state is available.
// +kubebuilder:object:generate=false
type resourceWatcher struct {
	resource *resourceLogState
	changed  *concurrency.AutoResetEvent
	onStop   func()
	stopped  bool
	mutex    *sync.Mutex
}

func (rw *resourceWatcher) Stop() {
	// We need to lock to prevent a possible race condition between sending events to the watcher and the watcher being removed
	rw.mutex.Lock()

	if rw.stopped {
		rw.mutex.Unlock()
		return
	}

	rw.stopped = true
	rw.changed.SetAndFreeze()
	onStop := rw.onStop
	rw.mutex.Unlock()

	if onStop != nil {
		onStop()
	}
}

func (rw *resourceWatcher) ResultChan() <-chan struct{} {
	return rw.changed.Wait()
}

func (rw *resourceWatcher) OnStop(onStop func()) {
	rw.mutex.Lock()
	defer rw.mutex.Unlock()
	rw.onStop = onStop
}

func (rw *resourceWatcher) Notify() bool {
	rw.mutex.Lock()
	defer rw.mutex.Unlock()
	if !rw.stopped {
		rw.changed.Set()
		return true
	} else {
		return false
	}
}

func newResourceWatcher(state *resourceLogState) *resourceWatcher {
	return &resourceWatcher{
		resource: state,
		changed:  concurrency.NewAutoResetEvent(false),
		mutex:    &sync.Mutex{},
	}
}

// +kubebuilder:object:generate=false
type LogStorage[O Options] struct {
	// The data storage for the parent object kind
	parentKindStorage registry_rest.StandardStorage

	// Factory for creating new log stream sessions
	logStreamer  ResourceLogStreamer[O]
	newOptions   func() O
	newResponse  func(registry_rest.ResourceStreamer) runtime.Object
	responseKind schema.GroupVersionKind

	// Schedules resources with pending lifecycle events.
	logStreamerResourceQueue       *resiliency.WorkQueue
	logStreamerResourceQueueCancel context.CancelFunc
	lifetimeCtx                    context.Context

	// An entry remains while its resource is queued or processing an event, even when its FIFO is empty.
	pendingResourceEvents map[types.UID]*container.RingBuffer[ResourceWatcherEvent]

	// Shares the latest processed state and processing lock between lifecycle callbacks and log requests.
	resourceStates *syncmap.Map[types.UID, *resourceLogState]

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

func NewLogStorage[O Options](
	parentKindStorage registry_rest.StandardStorage, logStreamer ResourceLogStreamer[O],
	newOptions func() O, newResponse func(registry_rest.ResourceStreamer) runtime.Object,
	responseKind schema.GroupVersionKind,
) (*LogStorage[O], error) {
	if parentKindStorage == nil {
		return nil, fmt.Errorf("parent object kind storage is required for LogStorage to function")
	}
	if logStreamer == nil {
		return nil, fmt.Errorf("log streamer is required for LogStorage to function")
	}
	if newOptions == nil || newResponse == nil || responseKind.Empty() {
		return nil, fmt.Errorf("log storage requires option and response factories and a response kind")
	}

	logStreamerResourceQueueCtx, logStreamerResourceQueueCancel := context.WithCancel(context.Background())

	return &LogStorage[O]{
		parentKindStorage:              parentKindStorage,
		logStreamer:                    logStreamer,
		logStreamerResourceQueue:       resiliency.NewWorkQueue(logStreamerResourceQueueCtx, resiliency.DefaultConcurrency),
		logStreamerResourceQueueCancel: logStreamerResourceQueueCancel,
		lifetimeCtx:                    logStreamerResourceQueueCtx,
		pendingResourceEvents:          make(map[types.UID]*container.RingBuffer[ResourceWatcherEvent]),
		resourceStates:                 &syncmap.Map[types.UID, *resourceLogState]{},
		resourceWatchers:               &syncmap.Map[types.UID, *syncmap.Map[*resourceWatcher, bool]]{},
		mutex:                          &sync.Mutex{},
		watcher:                        nil,
		disposeCh:                      make(chan struct{}),
		newOptions:                     newOptions,
		newResponse:                    newResponse,
		responseKind:                   responseKind,
	}, nil
}

func (ls *LogStorage[O]) watchResource(ctx context.Context, uid types.UID, log logr.Logger) (*resourceWatcher, error) {
	// Serialize parent-watch setup and subscription registration.
	ls.mutex.Lock()
	if ls.disposed {
		ls.mutex.Unlock()
		return nil, fmt.Errorf("log storage has been disposed")
	}

	if ls.watcher == nil {
		watchErr := ls.watchResourceEvents(log)
		if watchErr != nil {
			ls.mutex.Unlock()
			return nil, watchErr
		}
	}

	resourceWatchers, _ := ls.resourceWatchers.LoadOrStoreNew(uid, func() *syncmap.Map[*resourceWatcher, bool] {
		return &syncmap.Map[*resourceWatcher, bool]{}
	})

	state, _ := ls.resourceStates.LoadOrStoreNew(uid, func() *resourceLogState {
		return &resourceLogState{}
	})
	rw := newResourceWatcher(state)
	rw.OnStop(func() {
		resourceWatchers.Delete(rw)
	})
	_, _ = resourceWatchers.LoadOrStore(rw, true)
	ls.mutex.Unlock()

	state.processing.RLock()
	if contextErr := ctx.Err(); contextErr != nil {
		state.processing.RUnlock()
		rw.Stop()
		return nil, contextErr
	}
	if state.latest.Object != nil {
		rw.Notify()
	}
	state.processing.RUnlock()

	return rw, nil
}

// Event processing loop for the LogStorage resource watch
func (ls *LogStorage[O]) watchResourceEvents(log logr.Logger) error {
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
						log.V(1).Info("LogStorage is disposed, giving up on refreshing parent resource watcher")
						return
					}

					// Stop the old watcher before registering its replacement so generations do not overlap.
					log.V(1).Info("Stopping old parent resource watcher...")
					stopWatcher(ls.watcher)
					log.V(1).Info("Old parent resource watcher stopped")

					log.V(1).Info("Creating new parent resource watcher...")
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

func (ls *LogStorage[O]) resourceEventHandler(evt watch.Event, log logr.Logger) {
	// Serialize incoming events and scheduling for each resource.
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
	const longEventProcessingThreshold = 2 * time.Second
	defer func() {
		duration := time.Since(startTime)
		if duration > longEventProcessingThreshold {
			log.Info("Warning: processing parent resource watch event took longer than expected",
				"Event", rwevt.Type,
				"Resource", resourceName,
				"Duration", duration,
			)
		}
	}()

	uid := apiObj.GetObjectMeta().GetUID()
	pending, scheduled := ls.pendingResourceEvents[uid]
	if !scheduled {
		pending = container.NewRingBuffer[ResourceWatcherEvent]()
		ls.pendingResourceEvents[uid] = pending
	}
	pending.Push(rwevt)
	if !scheduled {
		ls.scheduleResourceEvent(uid, log)
	}
}

// scheduleResourceEvent requires ls.mutex to be held.
func (ls *LogStorage[O]) scheduleResourceEvent(uid types.UID, log logr.Logger) {
	scheduleErr := ls.logStreamerResourceQueue.Enqueue(func(ctx context.Context) {
		ls.processResourceEvent(ctx, uid, log)
	})
	if scheduleErr != nil {
		delete(ls.pendingResourceEvents, uid)
		log.Error(scheduleErr, "Failed to schedule log resource event", "UID", uid)
	}
}

func (ls *LogStorage[O]) processResourceEvent(ctx context.Context, uid types.UID, log logr.Logger) {
	ls.mutex.Lock()
	if ls.disposed {
		ls.mutex.Unlock()
		return
	}
	pending := ls.pendingResourceEvents[uid]
	event, found := pending.Pop()
	if !found {
		delete(ls.pendingResourceEvents, uid)
		ls.mutex.Unlock()
		log.Error(fmt.Errorf("resource event queue is empty"), "Failed to dispatch log resource event", "UID", uid)
		return
	}
	ls.mutex.Unlock()

	state, _ := ls.resourceStates.LoadOrStoreNew(uid, func() *resourceLogState {
		return &resourceLogState{}
	})
	state.processing.Lock()
	if ctx.Err() != nil {
		state.processing.Unlock()
		return
	}
	state.latest = event
	ls.logStreamer.OnResourceUpdated(event, log)
	if watchers, watching := ls.resourceWatchers.Load(uid); watching {
		watchers.Range(func(rw *resourceWatcher, _ bool) bool {
			rw.Notify()
			return true
		})
	}
	state.processing.Unlock()

	ls.mutex.Lock()
	defer ls.mutex.Unlock()
	if ls.disposed {
		return
	}
	if pending.Empty() {
		delete(ls.pendingResourceEvents, uid)
	} else {
		ls.scheduleResourceEvent(uid, log)
	}
}

// Processes the latest state under a read lock, excluding lifecycle updates but allowing other requests.
func (ls *LogStorage[O]) streamLatestResourceLogs(
	ctx context.Context, watcher *resourceWatcher, writer io.Writer, options O, log logr.Logger,
) (ResourceStreamStatus, <-chan struct{}, error) {
	state := watcher.resource
	state.processing.RLock()
	defer state.processing.RUnlock()
	if contextErr := ctx.Err(); contextErr != nil {
		return ResourceStreamStatusDone, nil, contextErr
	}
	watcher.mutex.Lock()
	if watcher.stopped {
		watcher.mutex.Unlock()
		return ResourceStreamStatusDone, nil, nil
	}
	watcher.changed.Clear()
	watcher.mutex.Unlock()
	select {
	case <-ls.disposeCh:
		return ResourceStreamStatusDone, nil, nil
	default:
	}
	if state.latest.Object == nil {
		return ResourceStreamStatusNotReady, nil, nil
	}
	if state.latest.Type == watch.Deleted {
		log.V(1).Info("Parent resource was deleted, closing log stream")
		return ResourceStreamStatusDone, nil, nil
	}
	return ls.logStreamer.StreamLogs(ctx, writer, state.latest.Object, options, log)
}

func (ls *LogStorage[O]) New() runtime.Object {
	return ls.newResponse(nil)
}

func (ls *LogStorage[O]) Destroy() {
	ls.mutex.Lock()

	if ls.disposed {
		ls.mutex.Unlock()
		return
	}

	ls.disposed = true
	close(ls.disposeCh)
	watcher := ls.watcher
	ls.watcher = nil
	ls.logStreamerResourceQueueCancel()
	clear(ls.pendingResourceEvents)
	ls.mutex.Unlock()

	// Logs do not have independent storage/lifecycle, they will be deleted when the parent is deleted,
	// but we do need to stop the watcher, and we do not want to hold the storage lock while doing so.
	if watcher != nil {
		stopWatcher(watcher)
	}
}

func (ls *LogStorage[O]) ProducesMIMETypes(httpVerb string) []string {
	return []string{"text/plain"}
}

// ProducesObject returns an object the specified HTTP verb respond with.
// Only the type of the returned object matters, the value is ignored.
// We return string, since the logs are streamed as text/plain;
// the API server does not really expose "streamer" objects returned by the Get function.
func (ls *LogStorage[O]) ProducesObject(httpVerb string) interface{} {
	return ""
}

func (ls *LogStorage[O]) GroupVersionKind(containingGV schema.GroupVersion) schema.GroupVersionKind {
	return ls.responseKind
}

func (ls *LogStorage[O]) Get(ctx context.Context, name string, options runtime.Object) (runtime.Object, error) {
	logOptions, ok := options.(O)
	if !ok {
		return nil, apierrors.NewInternalError(fmt.Errorf("invalid log options"))
	}

	namespace, _ := apirequest.NamespaceFrom(ctx)
	streamer, err := ls.resourceStreamerFactory(name, namespace, logOptions)
	if err != nil {
		return nil, err
	}

	return ls.newResponse(streamer), nil
}

func (ls *LogStorage[O]) resourceStreamerFactory(resourceName, namespace string, options O) (registry_rest.ResourceStreamer, error) {
	return ResourceStreamerFunc(func(ctx context.Context, apiVersion, acceptHeader string) (io.ReadCloser, bool, string, error) {
		ctx = apirequest.WithNamespace(ctx, namespace)
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

		if preflightErr := ls.logStreamer.Preflight(apiObj, options); preflightErr != nil {
			return nil, false, "", preflightErr
		}

		log := contextdata.GetContextLogger(ctx)
		log = log.WithName("Logstorage").WithValues(
			"Kind", apiObj.GetObjectKind().GroupVersionKind().String(),
		)

		requestCtx, cancelRequest := context.WithCancel(ctx)
		stopOnDispose := context.AfterFunc(ls.lifetimeCtx, cancelRequest)
		watcher, watcherErr := ls.watchResource(requestCtx, apiObj.GetObjectMeta().GetUID(), log)
		if watcherErr != nil {
			stopOnDispose()
			cancelRequest()
			return nil, false, "", apierrors.NewInternalError(fmt.Errorf("failed to register watcher for parent resource: %w", watcherErr))
		}

		log = log.WithValues(
			"Name", apiObj.GetObjectMeta().Name,
			"UID", apiObj.GetObjectMeta().GetUID(),
			"Options", options.String(),
		)
		log.V(1).Info("Preparing log stream")
		reader, writer := io.Pipe()

		go func() {
			defer stopOnDispose()
			defer cancelRequest()
			defer func() {
				closeErr := writer.Close()
				if closeErr != nil {
					log.Error(closeErr, "Failed to close log stream writer")
				}
			}()
			defer watcher.Stop()

			for {
				select {
				case <-requestCtx.Done():
					log.V(1).Info("Context was canceled, closing log stream")
					return
				case <-ls.disposeCh:
					return
				case _, open := <-watcher.ResultChan():
					if !open {
						return
					}
					resourceStreamStatus, doneStreaming, logStreamErr := ls.streamLatestResourceLogs(requestCtx, watcher, writer, options, log)
					// There was an error attempting to stream logs, close the stream and exit
					if logStreamErr != nil {
						log.Error(logStreamErr, "Failed to initialize log stream")
						_ = writer.CloseWithError(logStreamErr)
						return
					}

					// The resource was ready to stream
					if resourceStreamStatus == ResourceStreamStatusStreaming {
						watcher.Stop()
						log.V(1).Info("Starting log streaming for resource")
						select {
						case <-requestCtx.Done():
						case <-ls.disposeCh:
						case <-doneStreaming:
						}
						return
					}

					if resourceStreamStatus == ResourceStreamStatusDone {
						log.V(1).Info("Resource is done streaming logs")
						return
					}

					// The object wasn't ready to start streaming, but follow isn't set, so close the stream and exit
					if resourceStreamStatus != ResourceStreamStatusStreaming && !options.GetFollow() {
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

func (ls *LogStorage[O]) NewGetOptions() (runtime.Object, bool, string) {
	return ls.newOptions(), false, ""
}

func stopWatcher(w watch.Interface) {
	// Keep draining until the result channel closes so pending deliveries are not abandoned during shutdown.
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

type ResourceStreamerFunc func(context.Context, string, string) (io.ReadCloser, bool, string, error)

func (stream ResourceStreamerFunc) InputStream(ctx context.Context, apiVersion, acceptHeader string) (io.ReadCloser, bool, string, error) {
	return stream(ctx, apiVersion, acceptHeader)
}
