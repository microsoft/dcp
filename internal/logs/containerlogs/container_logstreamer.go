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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	"github.com/microsoft/usvc-apiserver/internal/contextdata"
	"github.com/microsoft/usvc-apiserver/internal/logs"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

type containerLogStreamer struct {
	// A map of Container resource UID to the log descriptor for the container.
	// Used for streaming logs from the container.
	containerLogs *logs.LogDescriptorSet

	startupLogStreams *syncmap.Map[types.UID, []*usvc_io.FollowWriter]
	stdioLogStreams   *syncmap.Map[types.UID, []*usvc_io.FollowWriter]

	// The container orchestrator used to capture logs from containers.
	containerLogSource containers.ContainerLogSource

	log  logr.Logger
	lock *sync.Mutex
}

func NewLogStreamer(log logr.Logger) *containerLogStreamer {
	return &containerLogStreamer{
		startupLogStreams: &syncmap.Map[types.UID, []*usvc_io.FollowWriter]{},
		stdioLogStreams:   &syncmap.Map[types.UID, []*usvc_io.FollowWriter]{},
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
	status := apiv1.ResourceStreamStatusNotReady
	ctr, isContainer := obj.(*apiv1.Container)
	if !isContainer {
		return status, nil, apierrors.NewInternalError(fmt.Errorf("parent storage returned object of wrong type (not Container): %s", obj.GetObjectKind().GroupVersionKind().String()))
	}

	deletionRequested := ctr.DeletionTimestamp != nil && !ctr.DeletionTimestamp.IsZero()
	if deletionRequested {
		return status, nil, apierrors.NewBadRequest("Container is being deleted")
	}

	switch ctr.Status.State {
	case apiv1.ContainerStateUnknown:
		return status, nil, apierrors.NewBadRequest(fmt.Sprintf("logs are not available for Container in state %s", ctr.Status.State))

	case "", apiv1.ContainerStatePending:
		// We're not ready to start streaming logs for this container yet
		streamLog.V(1).Info("container hasn't started running yet, not ready to stream logs")
		return status, nil, nil
	}

	if opts.Source == string(apiv1.LogStreamSourceStartupStdout) && ctr.Status.StartupStdOutFile == "" {
		// No startup stdout file, so nothing to stream
		return apiv1.ResourceStreamStatusDone, nil, nil
	}

	if opts.Source == string(apiv1.LogStreamSourceStartupStderr) && ctr.Status.StartupStdErrFile == "" {
		// No startup stderr file, so nothing to stream
		return apiv1.ResourceStreamStatusDone, nil, nil
	}

	hostLifetimeCtx := contextdata.GetHostLifetimeContext(ctx)

	cls, err := c.ensureDependencies(ctx)
	if err != nil {
		return status, nil, err
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
			return status, nil, nil
		}

		if ctr.Status.State == apiv1.ContainerStateStarting {
			streamLog.V(1).Info("container is still starting, not ready to stream logs")
			return status, nil, nil
		}

		// Standard stdout/stderr log streaming
		logDescriptorCtx, cancel := context.WithCancel(hostLifetimeCtx)
		ld, stdOutWriter, stdErrWriter, newlyCreated, acquireErr := c.containerLogs.AcquireForResource(logDescriptorCtx, cancel, ctr.NamespacedName(), ctr.UID)
		if acquireErr != nil {
			streamLog.Error(err, "Failed to enable log capturing for Container")
			return status, nil, apierrors.NewInternalError(acquireErr)
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
				return status, nil, apierrors.NewInternalError(logCaptureErr)
			}
		}

		stdOutPath, stdErrPath, startErr := ld.LogConsumerStarting()
		if startErr != nil {
			// This can happen if the log descriptor is being disposed because the Container is being deleted
			// We just report not found in this case
			return status, nil, apierrors.NewNotFound(ctr.GetGroupVersionResource().GroupResource(), ctr.NamespacedName().Name)
		}

		if opts.Source == string(apiv1.LogStreamSourceStdout) || opts.Source == "" {
			logFilePath = stdOutPath
		} else if opts.Source == string(apiv1.LogStreamSourceStderr) {
			logFilePath = stdErrPath
		}
	}

	if logFilePath == "" {
		streamLog.V(1).Info("container logs didn't start streaming")
		return status, nil, nil
	}

	src, fileErr := usvc_io.OpenFile(logFilePath, os.O_RDONLY, 0)
	if fileErr != nil {
		cleanup()
		return status, nil, fmt.Errorf("failed to open log file '%s': %w", logFilePath, fileErr)
	}

	if !follow {
		// If we aren't following, just copy the file and report that we're done streaming
		defer cleanup()
		defer src.Close()

		_, copyErr := io.Copy(dest, usvc_io.NewTimestampAwareReader(src, opts.Timestamps))
		if copyErr != nil {
			streamLog.Error(copyErr, "failed to copy log file to destination")
		}

		return apiv1.ResourceStreamStatusDone, nil, nil
	}

	// If we're following, use a follow writer to keep streaming until stopped
	c.lock.Lock()
	defer c.lock.Unlock()

	followWriter := usvc_io.NewFollowWriter(ctx, usvc_io.NewTimestampAwareReader(src, opts.Timestamps), dest)

	switch opts.Source {
	case string(apiv1.LogStreamSourceStartupStdout), string(apiv1.LogStreamSourceStartupStderr):
		followWriters, _ := c.startupLogStreams.LoadOrStore(ctr.UID, []*usvc_io.FollowWriter{})
		followWriters = append(followWriters, followWriter)
		c.startupLogStreams.Store(ctr.UID, followWriters)
	default:
		followWriters, _ := c.stdioLogStreams.LoadOrStore(ctr.UID, []*usvc_io.FollowWriter{})
		followWriters = append(followWriters, followWriter)
		c.stdioLogStreams.Store(ctr.UID, followWriters)
	}

	// If we're following, we need to report that to the caller so they can wait for the goroutine to complete
	go func() {
		defer cleanup()
		defer src.Close()

		<-followWriter.Done()

		streamLog.V(1).Info("log streamer completed", "Container", ctr.Status.ContainerID, "Source", opts.Source)

		if followWriter.Err() != nil {
			streamLog.Error(followWriter.Err(), "failed to stream logs for Container")
		}
	}()

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
		// We haven't completed initalization for any resources yet
		return
	}

	if evt.Type == watch.Modified {
		if ctr.Status.State != apiv1.ContainerStateStarting && ctr.Status.State != apiv1.ContainerStateBuilding {
			// If done starting the container, ensure startup logs stop streaming once they reach EOF
			if logs, found := c.startupLogStreams.Load(ctr.UID); found {
				log.V(1).Info("stopping startup follow logs for container", "Container", ctr.Status.ContainerID, "StreamCount", len(logs))
				for i := range logs {
					logs[i].StopFollow()
				}

				c.startupLogStreams.Delete(ctr.UID)
			}
		}

		if ctr.Status.State == apiv1.ContainerStateFailedToStart || ctr.Status.State == apiv1.ContainerStateExited || ctr.Status.State == apiv1.ContainerStateUnknown {
			// If the container isn't running, ensure standard logs stop streaming once they reach EOF
			if logs, found := c.stdioLogStreams.Load(ctr.UID); found {
				log.V(1).Info("stopping stdio follow logs for container", "Container", ctr.Status.ContainerID, "StreamCount", len(logs))
				for i := range logs {
					logs[i].StopFollow()
				}

				c.stdioLogStreams.Delete(ctr.UID)
			}
		}
	} else if evt.Type == watch.Deleted {
		if c.containerLogs != nil {
			// Need to stop the log streamer and any log watchers for this container (if any) as it is being deleted.
			// It is OK to call RealaeseForResource() if the resource is not in the set, it is a no-op in that case.
			c.containerLogs.ReleaseForResource(ctr.UID)
		}

		// The resource was deleted, ensure any following log streams stop and cleanup their resources
		if logs, found := c.startupLogStreams.Load(ctr.UID); found {
			for i := range logs {
				logs[i].StopFollow()
			}

			c.startupLogStreams.Delete(ctr.UID)
		}

		if logs, found := c.stdioLogStreams.Load(ctr.UID); found {
			for i := range logs {
				logs[i].StopFollow()
			}

			c.stdioLogStreams.Delete(ctr.UID)
		}
	}
}

func (c *containerLogStreamer) Dispose() error {
	var lds *logs.LogDescriptorSet
	stopWriters := func(_ types.UID, writers []*usvc_io.FollowWriter) bool {
		for _, w := range writers {
			w.StopFollow()
		}
		return true // Continue iteration
	}

	c.lock.Lock()

	c.startupLogStreams.Range(stopWriters)
	c.stdioLogStreams.Range(stopWriters)
	lds = c.containerLogs
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

	cls := container_flags.TryGetTestContainerLogSource(hostLifetimeCtx, c.log.WithName("TestContainerLogSource"))
	if cls == nil {
		co, err := container_flags.GetContainerOrchestrator(hostLifetimeCtx, c.log.WithName("ContainerOrchestrator").WithValues("ContainerRuntime", container_flags.GetRuntimeFlagArg()), pe)
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
