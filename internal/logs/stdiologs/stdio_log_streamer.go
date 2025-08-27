// Copyright (c) Microsoft Corporation. All rights reserved.

package stdiologs

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/go-logr/logr"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/watch"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/logs"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
)

var (
	lastLogStreamID logs.LogStreamID // Used to generate unique log stream IDs for each log stream

	stdIoStreamer = &stdIoLogStreamer{
		lock:          &sync.Mutex{},
		activeStreams: make(logs.LogStreamMop),
	}
)

type stdIoLogStreamer struct {
	lock          *sync.Mutex
	activeStreams logs.LogStreamMop
}

func LogStreamer() *stdIoLogStreamer {
	return stdIoStreamer
}

// StreamLogs implements v1.ResourceLogStreamer.
func (sls stdIoLogStreamer) StreamLogs(
	requestCtx context.Context,
	dest io.Writer,
	obj apiserver_resource.Object,
	opts *apiv1.LogOptions,
	log logr.Logger,
) (apiv1.ResourceStreamStatus, <-chan struct{}, error) {
	// Do not let a panic in the log streaming goroutine down the entire API server process.
	defer func() { _ = resiliency.MakePanicError(recover(), log) }()

	status := apiv1.ResourceStreamStatusNotReady

	resource, isResource := obj.(apiv1.StdIoStreamableResource)
	if !isResource {
		return status, nil, apierrors.NewInternalError(fmt.Errorf("parent storage returned object of wrong type: %s", obj.GetObjectKind().GroupVersionKind().String()))
	}

	deletionRequested := !resource.GetDeletionTimestamp().IsZero()
	if deletionRequested {
		return status, nil, apierrors.NewBadRequest("resource is being deleted")
	}

	var logFilePath string
	switch opts.Source {
	case "", string(apiv1.LogStreamSourceStdout):
		logFilePath = resource.GetStdOutFile()
	case string(apiv1.LogStreamSourceStderr):
		logFilePath = resource.GetStdErrFile()
	case string(apiv1.LogStreamSourceSystem):
		logFilePath = logger.GetResourceLogPath(resource.GetResourceId())
	default:
		return apiv1.ResourceStreamStatusDone, nil, nil
	}

	if logFilePath == "" {
		log.V(1).Info("Resource logs didn't start streaming", "Kind", obj.GetObjectKind().GroupVersionKind().String(), "Name", resource.NamespacedName().String(), "Source", opts.Source)
		return status, nil, nil
	}

	logFile, fileErr := usvc_io.OpenFile(logFilePath, os.O_RDONLY, 0)
	if fileErr != nil {
		if os.IsNotExist(fileErr) {
			log.V(1).Info("Log file does not exist yet", "Path", logFilePath)
			return status, nil, nil
		}

		return status, nil, fmt.Errorf("failed to open log file '%s': %w", logFilePath, fileErr)
	}

	var src io.ReadCloser
	if opts.Tail != nil {
		src = usvc_io.NewTailReader(logFile, int(*opts.Tail))
	} else {
		src = logFile
	}
	src = usvc_io.NewTimestampAwareReader(src, logs.ToTimestampReaderOptions(opts))

	if !opts.Follow || resource.Done() {
		defer logFile.Close()

		_, copyErr := io.Copy(dest, src)
		if copyErr != nil {
			log.Error(copyErr, "Failed to copy log file to destination")
		}

		return apiv1.ResourceStreamStatusDone, nil, nil
	}

	followWriter := usvc_io.NewFollowWriter(requestCtx, src, dest)
	resourceID := resource.GetUID()

	sls.lock.Lock()
	defer sls.lock.Unlock()

	streamID := lastLogStreamID + 1
	lastLogStreamID = streamID

	followWriters, found := sls.activeStreams[resourceID]
	if !found {
		followWriters = make(map[logs.LogStreamID]*usvc_io.FollowWriter)
		sls.activeStreams[resourceID] = followWriters
	}
	followWriters[streamID] = followWriter

	go func() {
		<-followWriter.Done()

		log.V(1).Info("Log streamer completed", "Kind", obj.GetObjectKind().GroupVersionKind().String(), "Name", resource.NamespacedName().String(), "Source", opts.Source)

		if followWriter.Err() != nil {
			log.Error(followWriter.Err(), "Failed to stream logs for Resource", "Kind", obj.GetObjectKind().GroupVersionKind().String(), "Name", resource.NamespacedName().String())
		}

		sls.lock.Lock()
		defer sls.lock.Unlock()

		if writers, haveWriters := sls.activeStreams[resourceID]; haveWriters {
			delete(writers, streamID)
			if len(writers) == 0 {
				delete(sls.activeStreams, resourceID)
			}
		}
	}()

	return apiv1.ResourceStreamStatusStreaming, followWriter.Done(), nil
}

// OnResourceDeleted implements v1.ResourceLogStreamer.
func (sls *stdIoLogStreamer) OnResourceUpdated(evt apiv1.ResourceWatcherEvent, log logr.Logger) {
	resource, isResource := evt.Object.(apiv1.StdIoStreamableResource)
	if !isResource {
		log.V(1).Info("Resource watcher received a resource notification for an object that is not a supported type", "ObjectKind", evt.Object.GetObjectKind().GroupVersionKind().String())
		return
	}

	sls.lock.Lock()
	defer sls.lock.Unlock()

	stopResourceStreams := func(logMessage string) {
		resourceID := resource.GetUID()

		if fwStreams, found := sls.activeStreams[resourceID]; found {
			delete(sls.activeStreams, resourceID)

			if log.V(1).Enabled() {
				log.V(1).Info(logMessage, "Kind", evt.Object.GetObjectKind().GroupVersionKind().String(),
					"Name", resource.NamespacedName().String(),
					"StreamCount", len(fwStreams),
				)
			}

			logs.DelayCancelFollowStreams(maps.Values(fwStreams), (*usvc_io.FollowWriter).StopFollow)
		}
	}

	switch evt.Type {

	case watch.Added, watch.Modified:
		// "watch.Added" does not necessarily mean that the resource was just created.
		// It really means that the resource was added to the watch stream (has been observed for the first time).

		if !resource.GetDeletionTimestamp().IsZero() {
			stopResourceStreams("Stopping log streams for resource that is being deleted")
		} else if resource.Done() {
			// If the resource isn't running, ensure logs stop streaming once they reach EOF
			stopResourceStreams("Stopping log following for resource that reached its final state")
		}

	case watch.Deleted:
		stopResourceStreams("Stopping log streams for resource that was deleted")
	}
}

func (sls *stdIoLogStreamer) Dispose() error {
	sls.lock.Lock()
	defer sls.lock.Unlock()

	for _, w := range maps.FlattenValues(sls.activeStreams) {
		w.StopFollow()
	}
	sls.activeStreams = make(logs.LogStreamMop)

	return nil
}

var _ apiv1.ResourceLogStreamer = (*stdIoLogStreamer)(nil)
