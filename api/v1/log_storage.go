// Copyright (c) Microsoft Corporation. All rights reserved.

package v1

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/internal/contextdata"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
)

type ResourceStreamStatus string

const (
	ResourceStreamStatusStreaming ResourceStreamStatus = "streaming"
	ResourceStreamStatusDone      ResourceStreamStatus = "done"
	ResourceStreamStatusNotReady  ResourceStreamStatus = "not_ready"
	watcherRestartInterval                             = 30 * time.Second
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
type ResourceWatcher struct {
	result  chan ResourceWatcherEvent
	onStop  func()
	stopCh  chan struct{}
	stopped bool
	mutex   *sync.Mutex
}

func (rw *ResourceWatcher) Stop() {
	// We need to lock to prevent a possible race condition between sending events to the watcher and the watcher being removed
	rw.mutex.Lock()
	defer rw.mutex.Unlock()

	if !rw.stopped {
		rw.stopped = true
		close(rw.stopCh)
		close(rw.result)
	}
}

func (rw *ResourceWatcher) ResultChan() <-chan ResourceWatcherEvent {
	return rw.result
}

// +kubebuilder:object:generate=false
type LogStorage struct {
	// The data storage for the parent object kind
	parentKindStorage registry_rest.StandardStorage

	// Factory for creating new log stream sessions
	logStreamer ResourceLogStreamer

	// Cache of most recent event for each parent resource
	// Keyed off UID to handle the scenario where a resource with the same name is created multiple times
	// Used to send the current status of a resource immediately to a new watcher
	mostRecentResourceEvents *syncmap.Map[types.UID, ResourceWatcherEvent]

	// Resource watchers that want to be updated when parent resources change
	resourceWatchers *syncmap.Map[types.UID, *syncmap.Map[*ResourceWatcher, bool]]

	// Mutex for synchronizing access to the watcher
	mutex *sync.Mutex

	// The watcher for the parent object kind
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

	return &LogStorage{
		parentKindStorage:        parentKindStorage,
		logStreamer:              logStreamer,
		mostRecentResourceEvents: &syncmap.Map[types.UID, ResourceWatcherEvent]{},
		resourceWatchers:         &syncmap.Map[types.UID, *syncmap.Map[*ResourceWatcher, bool]]{},
		mutex:                    &sync.Mutex{},
		watcher:                  nil,
	}, nil
}

func (ls *LogStorage) WatchResource(uid types.UID, log logr.Logger) (*ResourceWatcher, error) {
	// Lock the log storage mutex to ensure notifications aren't being sent while a new watcher is registered
	ls.mutex.Lock()
	defer ls.mutex.Unlock()

	resourceWatchers, _ := ls.resourceWatchers.LoadOrStoreNew(uid, func() *syncmap.Map[*ResourceWatcher, bool] {
		return &syncmap.Map[*ResourceWatcher, bool]{}
	})

	resourceWatcher := &ResourceWatcher{
		result: make(chan ResourceWatcherEvent, 1), // Need a buffered channel to avoid blocking on the mutex
		stopCh: make(chan struct{}),
		mutex:  &sync.Mutex{},
	}

	resourceWatcher.onStop = func() {
		resourceWatchers.Delete(resourceWatcher)
	}

	if ls.watcher == nil {
		watchErr := ls.watchResourceEvents(log)
		if watchErr != nil {
			return nil, watchErr
		}
	} else {
		if event, found := ls.mostRecentResourceEvents.Load(uid); found {
			resourceWatcher.result <- event
		}
	}

	resourceWatchers.LoadOrStore(resourceWatcher, true)

	return resourceWatcher, nil
}

// Event processing loop for the LogStorage resource watch
func (ls *LogStorage) watchResourceEvents(log logr.Logger) error {
	sendInitialEvents := true
	listOpts := metainternalversion.ListOptions{
		Watch:                true,
		SendInitialEvents:    &sendInitialEvents,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
	}

	// This watcher will be cleaned up when the Destroy method of the log storage instance is called, which will cleanup these goroutines
	watcher, watchErr := ls.parentKindStorage.Watch(context.Background(), &listOpts)
	if watchErr != nil {
		return watchErr
	}

	ls.watcher = watcher

	go func() {
		// We've seen in tests scenarios where the watcher doesn't always return every event.
		// This can lead to the log streamer never starting streaming for a given resource. To mitigate this,
		// we are periodically restarting the watcher to ensure we don't miss any events (the watcher always
		// emits the current state of resources when it starts).
		timer := time.NewTimer(watcherRestartInterval)
		for {
			select {
			case <-timer.C:
				ls.mutex.Lock()

				if !ls.disposed {
					newWatcher, newWatchErr := ls.parentKindStorage.Watch(context.Background(), &listOpts)
					if newWatchErr != nil {
						log.V(1).Info("failed to re-establish parent resource watcher", "Error", newWatchErr)
					} else {
						ls.watcher.Stop()
						ls.watcher = newWatcher
					}

					timer.Reset(watcherRestartInterval)
				}

				ls.mutex.Unlock()
			case <-ls.disposeCh:
				timer.Stop()
				return
			case evt := <-ls.watcher.ResultChan():
				ls.resourceEventHandler(evt, log)
			}
		}
	}()

	return nil
}

func (ls *LogStorage) resourceEventHandler(evt watch.Event, log logr.Logger) {
	// Lock the log storage mutex to ensure we don't try to notify resource watchers at the same time a new resource watcher is being added
	ls.mutex.Lock()
	defer ls.mutex.Unlock()

	if evt.Type == watch.Error {
		log.V(1).Info("saw error event for parent resource: %v", evt.Object)
		return
	}

	apiObj, isApiObj := evt.Object.(apiserver_resource.Object)
	if !isApiObj {
		log.V(1).Info("parent resource was not an API object, skipping")
		return
	}

	rwevt := ResourceWatcherEvent{
		Type:   evt.Type,
		Object: apiObj,
	}

	activeResourceWatchers, found := ls.resourceWatchers.Load(apiObj.GetObjectMeta().GetUID())
	if found {
		log.V(1).Info("started notifying resource watchers of parent resource event", "Event", rwevt.Type, "Resource", apiObj.GetObjectMeta().GetName())
		watcherCount := 0
		activeResourceWatchers.Range(func(rw *ResourceWatcher, _ bool) bool {
			// We need to lock to prevent a possible race condition between sending events to the watcher and the watcher being removed
			rw.mutex.Lock()
			defer rw.mutex.Unlock()

			if rw.stopped {
				return true
			}

			// Track how many watchers we actually sent the event to
			watcherCount += 1

			// Pass the event to each subscribed resource watcher. The channel is a buffered channel of size one in order to prevent
			// a deadlock on the ResourceWatcher mutex above; the mutex locks while broadcasting this event, but also when stopping
			// the watcher, which can be triggered during this broadcast
			rw.result <- rwevt
			return true
		})
		log.V(1).Info("completed notifying resource watchers of parent resource event", "Event", rwevt.Type, "Resource", apiObj.GetObjectMeta().GetName(), "WatcherCount", watcherCount)
	} else {
		log.V(1).Info("no watchers registered for this resource", "Event", rwevt.Type, "Resource", apiObj.GetObjectMeta().GetName())
	}

	log.V(1).Info("notify the log streamer of resource update")
	go func(obj apiserver_resource.Object, log logr.Logger) {
		ls.logStreamer.OnResourceUpdated(rwevt, log)
	}(apiObj, log)

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
	defer ls.mutex.Unlock()

	close(ls.disposeCh)
	ls.disposed = true

	// Logs do not have independent storage/lifecycle, they will be deleted when the parent is deleted, but we do need to stop the watcher
	if ls.watcher != nil {
		ls.watcher.Stop()
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
		getOpts := metav1.GetOptions{}
		obj, err := ls.parentKindStorage.Get(ctx, resourceName, &getOpts)
		if err != nil {
			return nil, false, "", err
		}

		apiObj, isAPIObj := obj.(apiserver_resource.Object)
		if !isAPIObj {
			return nil, false, "", apierrors.NewInternalError(fmt.Errorf("parent storage returned object of wrong type: %s", obj.GetObjectKind().GroupVersionKind().String()))
		}

		if options.Source != "" && options.Source != string(LogStreamSourceStdout) && options.Source != string(LogStreamSourceStderr) && options.Source != string(LogStreamSourceStartupStdout) && options.Source != string(LogStreamSourceStartupStderr) {
			return nil, false, "", apierrors.NewBadRequest(fmt.Sprintf("invalid log source '%s'. Supported log sources are '%s', '%s', '%s', and '%s'", options.Source, LogStreamSourceStdout, LogStreamSourceStderr, LogStreamSourceStartupStdout, LogStreamSourceStartupStderr))
		}

		log := contextdata.GetContextLogger(ctx)

		reader, writer := io.Pipe()
		watcher, watcherErr := ls.WatchResource(apiObj.GetObjectMeta().GetUID(), log)
		if watcherErr != nil {
			return nil, false, "", apierrors.NewInternalError(fmt.Errorf("failed to register watcher for parent resource: %w", watcherErr))
		}

		log = log.WithName("logstorage").WithValues(
			"Kind", apiObj.GetObjectKind().GroupVersionKind().String(),
			"Name", apiObj.GetObjectMeta().Name,
			"UID", apiObj.GetObjectMeta().GetUID(),
			"Source", options.Source,
			"Follow", options.Follow,
		)

		log.V(1).Info("preparing log stream")

		go func() {
			defer func() {
				closeErr := writer.Close()
				if closeErr != nil {
					log.Error(closeErr, "failed to close log stream writer")
				}
			}()
			defer watcher.Stop()

			for {
				select {
				case <-ctx.Done():
					log.V(1).Info("context was canceled, closing log stream")
					return
				case evt := <-watcher.ResultChan():
					if evt.Type == watch.Deleted {
						log.V(1).Info("parent resource was deleted, closing log stream")
						return
					}

					resourceStreamStatus, doneStreaming, logStreamErr := ls.logStreamer.StreamLogs(ctx, writer, evt.Object, options, log)
					// There was an error attempting to stream logs, close the stream and exit
					if logStreamErr != nil {
						log.Error(logStreamErr, "failed to initialize log stream")
						return
					}

					// The resource was ready to stream
					if resourceStreamStatus == ResourceStreamStatusStreaming {
						// Stop the watcher since we're streaming logs now (this can be called safely multiple times)
						watcher.Stop()
						log.V(1).Info("starting log streaming for resource")
						// Wait for streaming to complete
						<-doneStreaming
						log.V(1).Info("log streaming for resource completed")
						return
					}

					if resourceStreamStatus == ResourceStreamStatusDone {
						log.V(1).Info("resource is done streaming logs")
						return
					}

					// The object wasn't ready to start streaming, but follow isn't set, so close the stream and exit
					if resourceStreamStatus != ResourceStreamStatusStreaming && !options.Follow {
						log.V(1).Info("resource not ready to stream logs")
						return
					}

					// We're following logs and the parent isn't ready to stream yet, wait for an update event to try agian
					log.V(1).Info("waiting for parent resource to be ready to stream logs")
				}
			}
		}()

		return reader /* flushOnNewData */, true, "text/plain", nil
	}), nil
}

func (ls *LogStorage) NewGetOptions() (runtime.Object, bool, string) {
	return &LogOptions{}, false, ""
}

var _ registry_rest.Storage = (*LogStorage)(nil)
var _ registry_rest.StorageMetadata = (*LogStorage)(nil)
var _ registry_rest.GetterWithOptions = (*LogStorage)(nil)
var _ registry_rest.GroupVersionKindProvider = (*LogStorage)(nil)
