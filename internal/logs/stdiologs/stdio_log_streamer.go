/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package stdiologs

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/watch"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/logs"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/resiliency"
)

var (
	lastLogStreamID atomic.Uint64 // Used to generate unique log stream IDs for each log stream

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

func (sls stdIoLogStreamer) Preflight(obj apiserver_resource.Object, opts *apiv1.LogOptions) error {
	resource, isResource := obj.(apiv1.StdIoStreamableResource)
	if !isResource {
		return apierrors.NewInternalError(fmt.Errorf("parent storage returned object of wrong type: %s", obj.GetObjectKind().GroupVersionKind().String()))
	}

	if !resource.GetDeletionTimestamp().IsZero() {
		return apierrors.NewBadRequest("resource is being deleted")
	}

	if !resource.HasTerminal() {
		return nil
	}

	switch opts.Source {
	case "", string(apiv1.LogStreamSourceStdout):
		return apierrors.NewBadRequest(fmt.Sprintf(
			"stdout logs are not available for %s '%s' because it is configured to use a terminal",
			obj.GetObjectKind().GroupVersionKind().Kind,
			resource.NamespacedName().String()))
	case string(apiv1.LogStreamSourceStderr):
		return apierrors.NewBadRequest(fmt.Sprintf(
			"stderr logs are not available for %s '%s' because it is configured to use a terminal",
			obj.GetObjectKind().GroupVersionKind().Kind,
			resource.NamespacedName().String()))
	}
	return nil
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

	// Defense in depth: the same checks run in Preflight (where their errors
	// actually reach the client), but we also short-circuit here so that any
	// direct caller of StreamLogs gets the same fail-fast behavior.
	if preflightErr := sls.Preflight(obj, opts); preflightErr != nil {
		return status, nil, preflightErr
	}

	var logFilePath string
	switch opts.Source {
	case "", string(apiv1.LogStreamSourceStdout):
		if resource.HasStdOut() {
			logFilePath = resource.GetStdOutFile()
		} else {
			return apiv1.ResourceStreamStatusDone, nil, nil
		}
	case string(apiv1.LogStreamSourceStderr):
		if resource.HasStdErr() {
			logFilePath = resource.GetStdErrFile()
		} else {
			return apiv1.ResourceStreamStatusDone, nil, nil
		}
	case string(apiv1.LogStreamSourceSystem):
		logFilePath = logger.GetResourceLogPath(resource.GetResourceId())
	default:
		return apiv1.ResourceStreamStatusDone, nil, nil
	}

	if logFilePath == "" {
		log.V(1).Info("Resource logs didn't start streaming",
			"Kind", obj.GetObjectKind().GroupVersionKind().String(),
			"Name", resource.NamespacedName().String(),
			"Source", opts.Source)
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

	followWriter := usvc_io.NewFollowWriter(requestCtx, src, dest, usvc_io.WithCloseSourceOnCancel())
	resourceID := resource.GetUID()

	sls.lock.Lock()
	defer sls.lock.Unlock()

	streamID := logs.LogStreamID(lastLogStreamID.Add(1))

	followWriters, found := sls.activeStreams[resourceID]
	if !found {
		followWriters = make(map[logs.LogStreamID]*usvc_io.FollowWriter)
		sls.activeStreams[resourceID] = followWriters
	}
	followWriters[streamID] = followWriter

	go func() {
		<-followWriter.Done()

		log.V(1).Info("Log streamer completed",
			"Kind", obj.GetObjectKind().GroupVersionKind().String(),
			"Name", resource.NamespacedName().String(),
			"Source", opts.Source)

		if followWriter.Err() != nil {
			log.Error(followWriter.Err(), "Failed to stream logs for Resource",
				"Kind", obj.GetObjectKind().GroupVersionKind().String(),
				"Name", resource.NamespacedName().String())
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

	var streamsToStop []*usvc_io.FollowWriter
	var stopStreams func([]*usvc_io.FollowWriter)
	var logMessage string

	switch evt.Type {

	case watch.Added, watch.Modified:
		// "watch.Added" does not necessarily mean that the resource was just created.
		// It really means that the resource was added to the watch stream (has been observed for the first time).

		if !resource.GetDeletionTimestamp().IsZero() {
			logMessage = "Stopping log streams for resource that is being deleted"
			stopStreams = logs.DelayStopFollowing
		} else if resource.Done() {
			// If the resource isn't running, ensure logs stop streaming once they reach EOF
			logMessage = "Stopping log following for resource that reached its final state"
			stopStreams = logs.DelayStopFollowing
		}

	case watch.Deleted:
		logMessage = "Stopping log streams for resource that was deleted"
		stopStreams = logs.DelayStopFollowing
	}

	if stopStreams == nil {
		return
	}

	resourceID := resource.GetUID()
	sls.lock.Lock()
	if fwStreams, found := sls.activeStreams[resourceID]; found {
		delete(sls.activeStreams, resourceID)
		streamsToStop = maps.Values(fwStreams)
	}
	sls.lock.Unlock()

	if len(streamsToStop) == 0 {
		return
	}

	if log.V(1).Enabled() {
		log.V(1).Info(logMessage, "Kind", evt.Object.GetObjectKind().GroupVersionKind().String(),
			"Name", resource.NamespacedName().String(),
			"StreamCount", len(streamsToStop),
		)
	}
	stopStreams(streamsToStop)
}

func (sls *stdIoLogStreamer) Dispose() error {
	sls.lock.Lock()
	streamsToCancel := maps.FlattenValues(sls.activeStreams)
	sls.activeStreams = make(logs.LogStreamMop)
	sls.lock.Unlock()

	logs.CancelFollowStreams(streamsToCancel)
	return nil
}

var _ apiv1.ResourceLogStreamer = (*stdIoLogStreamer)(nil)
