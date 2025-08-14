// Copyright (c) Microsoft Corporation. All rights reserved.

package containerlogs

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
	"github.com/microsoft/usvc-apiserver/internal/containers"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	"github.com/microsoft/usvc-apiserver/internal/containers/runtimes"
	"github.com/microsoft/usvc-apiserver/internal/contextdata"
	"github.com/microsoft/usvc-apiserver/internal/logs"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
)

var (
	lastLogStreamID logs.LogStreamID // Used to generate unique log stream IDs for each log stream
)

type containerLogStreamer struct {
	// A map of Container resource UID to the log descriptor for the container.
	// Used for streaming logs from the container.
	containerLogs *logs.LogDescriptorSet

	startupLogStreams logs.LogStreamMop
	stdioLogStreams   logs.LogStreamMop

	// The container orchestrator used to capture logs from containers.
	containerLogSource containers.ContainerLogSource

	log  logr.Logger
	lock *sync.Mutex
}

func NewLogStreamer(log logr.Logger) *containerLogStreamer {
	return &containerLogStreamer{
		startupLogStreams: make(logs.LogStreamMop),
		stdioLogStreams:   make(logs.LogStreamMop),
		log:               log,
		lock:              &sync.Mutex{},
	}
}

// StreamLogs implements v1.ResourceLogStreamer.
func (c *containerLogStreamer) StreamLogs(
	ctx context.Context,
	dest io.Writer,
	obj apiserver_resource.Object,
	opts *apiv1.LogOptions,
	streamLog logr.Logger,
) (apiv1.ResourceStreamStatus, <-chan struct{}, error) {
	// Do not let a panic in the log streaming goroutine down the entire API server process.
	defer func() { _ = resiliency.MakePanicError(recover(), streamLog) }()

	ctr, isContainer := obj.(*apiv1.Container)
	if !isContainer {
		return apiv1.ResourceStreamStatusNotReady, nil, apierrors.NewInternalError(fmt.Errorf("parent storage returned object of wrong type (not Container): %s", obj.GetObjectKind().GroupVersionKind().String()))
	}

	deletionRequested := ctr.DeletionTimestamp != nil && !ctr.DeletionTimestamp.IsZero()
	if deletionRequested {
		return apiv1.ResourceStreamStatusNotReady, nil, apierrors.NewBadRequest("Container is being deleted")
	}

	switch ctr.Status.State {
	case apiv1.ContainerStateUnknown:
		return apiv1.ResourceStreamStatusNotReady, nil, apierrors.NewBadRequest(fmt.Sprintf("logs are not available for Container in state %s", ctr.Status.State))

	case "", apiv1.ContainerStatePending, apiv1.ContainerStateRuntimeUnhealthy:
		// We're not ready to start streaming logs for this container yet
		streamLog.V(1).Info("container hasn't started running yet, not ready to stream logs")
		return apiv1.ResourceStreamStatusNotReady, nil, nil
	}

	startupLogsRequestedButNotAvailable :=
		(opts.Source == string(apiv1.LogStreamSourceStartupStdout) && ctr.Status.StartupStdOutFile == "") ||
			(opts.Source == string(apiv1.LogStreamSourceStartupStderr) && ctr.Status.StartupStdErrFile == "")
	if startupLogsRequestedButNotAvailable {
		if ctr.Status.State == apiv1.ContainerStateStarting || ctr.Status.State == apiv1.ContainerStateBuilding {
			streamLog.V(1).Info("startup logs are not yet available for building/starting container")
			return apiv1.ResourceStreamStatusNotReady, nil, nil
		} else {
			streamLog.V(1).Info(fmt.Sprintf("we are not capturing %s logs for this container", string(opts.Source)), "ContainerState", ctr.Status.State)
			return apiv1.ResourceStreamStatusDone, nil, nil
		}
	}

	if opts.Source == string(apiv1.LogStreamSourceStartupStderr) && ctr.Status.StartupStdErrFile == "" {
		streamLog.V(1).Info(fmt.Sprintf("we are not capturing %s logs for this container", string(apiv1.LogStreamSourceStartupStderr)),
			"ContainerState", ctr.Status.State,
		)
		return apiv1.ResourceStreamStatusDone, nil, nil
	}

	cls, err := c.ensureDependencies(ctx)
	if err != nil {
		return apiv1.ResourceStreamStatusNotReady, nil, err
	}

	follow := opts.Follow
	var logFilePath string
	cleanup := func() {}

	if opts.Source == string(apiv1.LogStreamSourceStartupStdout) {
		// Startup stdout log streaming
		logFilePath = ctr.Status.StartupStdOutFile
		follow = follow && (ctr.Status.State == apiv1.ContainerStateStarting || ctr.Status.State == apiv1.ContainerStateBuilding)
	} else if opts.Source == string(apiv1.LogStreamSourceStartupStderr) {
		// Startup stderr log streaming
		logFilePath = ctr.Status.StartupStdErrFile
		follow = follow && (ctr.Status.State == apiv1.ContainerStateStarting || ctr.Status.State == apiv1.ContainerStateBuilding)
	} else {
		if ctr.Status.ContainerID == "" {
			streamLog.V(1).Info("container has no container ID yet, not ready to stream logs")
			// We're not ready to start streaming logs for this container yet
			return apiv1.ResourceStreamStatusNotReady, nil, nil
		}

		if ctr.Status.State == apiv1.ContainerStateStarting {
			streamLog.V(1).Info("container is still starting, not ready to stream logs")
			return apiv1.ResourceStreamStatusNotReady, nil, nil
		}

		// Standard stdout/stderr log streaming
		hostLifetimeCtx := contextdata.GetHostLifetimeContext(ctx)
		logDescriptorCtx, cancel := context.WithCancel(hostLifetimeCtx)
		ld, stdOutWriter, stdErrWriter, newlyCreated, acquireErr := c.containerLogs.AcquireForResource(logDescriptorCtx, cancel, ctr.NamespacedName(), ctr.UID)
		if acquireErr != nil {
			streamLog.Error(acquireErr, "Failed to enable log capturing for Container")
			return apiv1.ResourceStreamStatusNotReady, nil, apierrors.NewInternalError(acquireErr)
		}
		// Ensure we cleanup resources after streaming
		cleanup = ld.LogConsumerStopped

		if newlyCreated {
			// Need to start log capturing for the container
			logCaptureErr := cls.CaptureContainerLogs(ld.Context, ctr.Status.ContainerID, stdOutWriter, stdErrWriter, containers.StreamContainerLogsOptions{
				Follow:     true,
				Timestamps: true,
			})
			if logCaptureErr != nil {
				streamLog.Error(logCaptureErr, "Failed to start capturing logs for Container")
				disposeErr := ld.Dispose(ctx, 0)
				if disposeErr != nil {
					streamLog.V(1).Info("Failed to dispose log descriptor after failed log capture", "Error", disposeErr.Error())
				}
				return apiv1.ResourceStreamStatusDone, nil, apierrors.NewInternalError(logCaptureErr)
			}
		}

		stdOutPath, stdErrPath, startErr := ld.LogConsumerStarting()
		if startErr != nil {
			// This can happen if the log descriptor is being disposed because the Container is being deleted
			// We just report not found in this case
			return apiv1.ResourceStreamStatusNotReady, nil, apierrors.NewNotFound(ctr.GetGroupVersionResource().GroupResource(), ctr.NamespacedName().Name)
		}

		if opts.Source == string(apiv1.LogStreamSourceStdout) || opts.Source == "" {
			logFilePath = stdOutPath
		} else if opts.Source == string(apiv1.LogStreamSourceStderr) {
			logFilePath = stdErrPath
		}
	}

	if logFilePath == "" {
		streamLog.V(1).Info("container logs didn't start streaming")
		return apiv1.ResourceStreamStatusNotReady, nil, nil
	}

	logFile, fileErr := usvc_io.OpenFile(logFilePath, os.O_RDONLY, 0)
	if fileErr != nil {
		cleanup()
		return apiv1.ResourceStreamStatusNotReady, nil, fmt.Errorf("failed to open log file '%s': %w", logFilePath, fileErr)
	}

	var src io.ReadCloser
	if opts.Tail != nil {
		src = usvc_io.NewTailReader(logFile, int(*opts.Tail))
	} else {
		src = logFile
	}
	src = usvc_io.NewTimestampAwareReader(src, logs.ToTimestampReaderOptions(opts))

	// We always want to use a FollowWriter, even if not in "follow" mode,
	// to account for the fact that ContainerLogSource.CaptureContainerLogs() is an asynchronous operation.
	followWriter := usvc_io.NewFollowWriter(ctx, src, dest)

	c.lock.Lock()
	defer c.lock.Unlock()

	streamID := lastLogStreamID + 1
	lastLogStreamID = streamID

	var streams logs.LogStreamMop
	switch opts.Source {
	case string(apiv1.LogStreamSourceStartupStdout), string(apiv1.LogStreamSourceStartupStderr):
		streams = c.startupLogStreams
	default:
		streams = c.stdioLogStreams
	}

	followWriters, found := streams[ctr.UID]
	if !found {
		followWriters = make(map[logs.LogStreamID]*usvc_io.FollowWriter)
		streams[ctr.UID] = followWriters
	}
	followWriters[streamID] = followWriter

	go func() {
		defer cleanup()
		defer src.Close()

		streamLog.V(1).Info("starting streaming logs to destination ...")
		<-followWriter.Done()
		streamLog.V(1).Info("finished streaming logs to destination")

		if followWriter.Err() != nil {
			streamLog.Error(followWriter.Err(), "failed to stream logs for Container")
		}

		c.lock.Lock()
		defer c.lock.Unlock()

		if writers, haveWriters := streams[ctr.UID]; haveWriters {
			delete(writers, streamID)
			if len(writers) == 0 {
				delete(streams, ctr.UID)
			}
		}
	}()

	if !follow || ctr.Done() {
		logs.DelayCancelFollowStreams([]*usvc_io.FollowWriter{followWriter}, (*usvc_io.FollowWriter).StopFollow)
	}

	return apiv1.ResourceStreamStatusStreaming, followWriter.Done(), nil

}

// OnResourceUpdated implements v1.ResourceLogStreamer.
func (c *containerLogStreamer) OnResourceUpdated(evt apiv1.ResourceWatcherEvent, log logr.Logger) {
	ctr, isContainer := evt.Object.(*apiv1.Container)
	if !isContainer {
		log.V(1).Info("container watcher received a resource notification for an object that is not a Container", "ObjectKind", evt.Object.GetObjectKind().GroupVersionKind().String())
		return
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	if c.containerLogSource == nil {
		// We haven't completed initialization for any resources yet
		return
	}

	switch evt.Type {

	case watch.Added, watch.Modified:
		// "watch.Added" does not necessarily mean that the resource was just created.
		// It really means that the resource was added to the watch stream (has been observed for the first time).

		if ctr.Status.State != apiv1.ContainerStateStarting && ctr.Status.State != apiv1.ContainerStateBuilding {
			// If done starting the container, ensure startup logs stop streaming once they reach EOF
			stopLogStreamsForContainer(c.startupLogStreams, ctr, "startup", log)
		}

		if ctr.Done() {
			// If the container is done, ensure standard logs stop streaming once they reach EOF
			stopLogStreamsForContainer(c.stdioLogStreams, ctr, "stdio", log)
		}

	case watch.Deleted:
		// The resource was deleted, ensure any following log streams stop and cleanup their resources
		stopLogStreamsForContainer(c.startupLogStreams, ctr, "startup", log)
		stopLogStreamsForContainer(c.stdioLogStreams, ctr, "stdio", log)

		if c.containerLogs != nil {
			// Need to stop the log streamer and any log watchers for this container (if any) as it is being deleted.
			// It is OK to call ReleaseForResource() if the resource is not in the set, it is a no-op in that case.
			c.containerLogs.ReleaseForResource(ctr.UID)
		}
	}
}

func stopLogStreamsForContainer(
	streams logs.LogStreamMop,
	ctr *apiv1.Container,
	streamKind string,
	log logr.Logger,
) {
	if ctrStreams, found := streams[ctr.UID]; found {
		delete(streams, ctr.UID)

		if log.V(1).Enabled() {
			log.V(1).Info(fmt.Sprintf("stopping %s follow logs for container", streamKind),
				"Container", ctr.Status.ContainerID,
				"StreamCount", len(ctrStreams),
			)
		}

		logs.DelayCancelFollowStreams(maps.Values(ctrStreams), (*usvc_io.FollowWriter).StopFollow)
	}
}

func (c *containerLogStreamer) Dispose() error {
	c.lock.Lock()

	for _, w := range maps.FlattenValues(c.startupLogStreams) {
		w.StopFollow()
	}
	c.startupLogStreams = make(logs.LogStreamMop)
	for _, w := range maps.FlattenValues(c.stdioLogStreams) {
		w.StopFollow()
	}
	c.stdioLogStreams = make(logs.LogStreamMop)

	lds := c.containerLogs
	c.containerLogs = nil

	c.lock.Unlock()

	if lds != nil {
		return lds.Dispose()
	} else {
		return nil
	}
}

func (c *containerLogStreamer) ensureDependencies(requestCtx context.Context) (containers.ContainerLogSource, error) {
	hostLifetimeCtx := contextdata.GetHostLifetimeContext(requestCtx)
	c.ensureContainerLogDescriptors(hostLifetimeCtx)

	cls, coErr := c.ensureContainerLogSource(requestCtx)
	if coErr != nil {
		c.log.Error(coErr, "failed to get Container orchestrator")
		return nil, apierrors.NewInternalError(coErr)
	}

	return cls, nil
}

func (c *containerLogStreamer) ensureContainerLogSource(requestCtx context.Context) (containers.ContainerLogSource, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.containerLogSource != nil {
		return c.containerLogSource, nil
	}

	pe := contextdata.GetProcessExecutor(requestCtx)
	hostLifetimeCtx := contextdata.GetHostLifetimeContext(requestCtx)

	cls := container_flags.TryGetRemoteContainerOrchestrator(hostLifetimeCtx, c.log.WithName("RemoteContainerOrchestrator"))
	if cls == nil {
		co, err := runtimes.FindAvailableContainerRuntime(requestCtx, c.log.WithName("ContainerOrchestrator").WithValues("ContainerRuntime", container_flags.GetRuntimeFlagValue()), pe)
		if err != nil {
			return nil, err
		}
		cls = co
	}

	c.containerLogSource = cls
	return cls, nil
}

func (c *containerLogStreamer) ensureContainerLogDescriptors(hostLifetimeCtx context.Context) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.containerLogs != nil {
		return
	}

	c.containerLogs = logs.NewLogDescriptorSet(hostLifetimeCtx, usvc_io.DcpTempDir(), c.log.WithName("LogDescriptorSet"))
}

var _ apiv1.ResourceLogStreamer = (*containerLogStreamer)(nil)
