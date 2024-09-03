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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

var stdIoStreamer = &stdIoLogStreamer{
	lock:          &sync.Mutex{},
	activeStreams: &syncmap.Map[types.UID, []*usvc_io.FollowWriter]{},
}

type stdIoLogStreamer struct {
	lock          *sync.Mutex
	activeStreams *syncmap.Map[types.UID, []*usvc_io.FollowWriter]
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
	default:
		return apiv1.ResourceStreamStatusDone, nil, nil
	}

	if logFilePath == "" {
		log.V(1).Info("resource logs didn't start streaming", "Kind", obj.GetObjectKind().GroupVersionKind().String(), "Name", resource.NamespacedName().String(), "Source", opts.Source)
		return status, nil, nil
	}

	src, fileErr := usvc_io.OpenFile(logFilePath, os.O_RDONLY, 0)
	if fileErr != nil {
		return status, nil, fmt.Errorf("failed to open log file '%s': %w", logFilePath, fileErr)
	}

	if !opts.Follow {
		_, copyErr := io.Copy(dest, usvc_io.NewTimestampAwareReader(src, opts.Timestamps))
		if copyErr != nil {
			log.Error(copyErr, "failed to copy log file to destination")
		}

		return apiv1.ResourceStreamStatusDone, nil, nil
	}

	sls.lock.Lock()
	defer sls.lock.Unlock()

	// Track this writer instance
	followWriter := usvc_io.NewFollowWriter(requestCtx, usvc_io.NewTimestampAwareReader(src, opts.Timestamps), dest)
	followWriters, _ := sls.activeStreams.LoadOrStore(resource.GetUID(), []*usvc_io.FollowWriter{})
	followWriters = append(followWriters, followWriter)
	sls.activeStreams.Store(resource.GetUID(), followWriters)

	go func() {
		<-followWriter.Done()

		log.V(1).Info("log streamer completed", "Kind", obj.GetObjectKind().GroupVersionKind().String(), "Name", resource.NamespacedName().String(), "Source", opts.Source)

		if followWriter.Err() != nil {
			log.Error(followWriter.Err(), "failed to stream logs for Resource", "Kind", obj.GetObjectKind().GroupVersionKind().String(), "Name", resource.NamespacedName().String())
		}
	}()

	return apiv1.ResourceStreamStatusStreaming, followWriter.Done(), nil
}

// OnResourceDeleted implements v1.ResourceLogStreamer.
func (sls *stdIoLogStreamer) OnResourceUpdated(evt apiv1.ResourceWatcherEvent, log logr.Logger) {
	resource, isResource := evt.Object.(apiv1.StdIoStreamableResource)
	if !isResource {
		log.V(1).Info("resource watcher received a resource notification for an object that is not a supported type", "ObjectKind", evt.Object.GetObjectKind().GroupVersionKind().String())
		return
	}

	sls.lock.Lock()
	defer sls.lock.Unlock()

	if evt.Type == watch.Modified {
		if resource.Done() {
			// If the resource isn't running, ensure logs stop streaming once they reach EOF
			if logs, found := sls.activeStreams.Load(resource.GetUID()); found {
				log.V(1).Info("stopping follow logs for resource", "Kind", evt.Object.GetObjectKind().GroupVersionKind().String(), "Name", resource.NamespacedName().String(), "StreamCount", len(logs))
				for i := range logs {
					logs[i].StopFollow()
				}

				sls.activeStreams.Delete(resource.GetUID())
			}
		}
	} else if evt.Type == watch.Deleted {
		followWriters, found := sls.activeStreams.Load(resource.GetUID())
		if found {
			for _, followWriter := range followWriters {
				followWriter.StopFollow()
			}

			sls.activeStreams.Delete(resource.GetUID())
		}
	}
}

func (sls *stdIoLogStreamer) Dispose() error {
	stopWriters := func(_ types.UID, writers []*usvc_io.FollowWriter) bool {
		for _, w := range writers {
			w.StopFollow()
		}
		return true // Continue iteration
	}

	sls.lock.Lock()
	defer sls.lock.Unlock()

	sls.activeStreams.Range(stopWriters)

	return nil
}

var _ apiv1.ResourceLogStreamer = (*stdIoLogStreamer)(nil)
