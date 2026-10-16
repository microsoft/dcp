/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2containerlogs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	"github.com/microsoft/dcp/internal/containers/runtimes"
	"github.com/microsoft/dcp/internal/contextdata"
	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/internal/logstream"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/resiliency"
)

type containerSnapshot struct {
	ownerUID    types.UID
	captureUID  types.UID
	captureName types.NamespacedName
	containerID string
	systemID    string
	started     bool
	terminal    bool
}

type logSession struct {
	source      string
	containerID string
	captureUID  types.UID
	cancel      context.CancelFunc
	done        chan struct{}
	writer      *usvc_io.FollowWriter
	finishing   bool
	destination io.Writer
}

type containerLogStreamer struct {
	lock        sync.Mutex
	sessions    map[types.UID]map[uint64]*logSession
	nextSession uint64
	disposed    bool
	captureLock sync.Mutex
	captures    *logs.LogDescriptorSet
	source      containers.ContainerLogSource
	log         logr.Logger
}

// NewLogStreamer creates shared physical capture with resource-scoped client sessions.
// The same instance serves logical and physical container kinds.
func NewLogStreamer(log logr.Logger) apiv2.ResourceLogStreamer {
	return &containerLogStreamer{
		sessions: make(map[types.UID]map[uint64]*logSession),
		log:      log,
	}
}

func snapshot(obj resource.Object) (containerSnapshot, error) {
	result := containerSnapshot{ownerUID: obj.GetObjectMeta().UID}
	switch container := obj.(type) {
	case *apiv2.PhysicalContainer:
		result.captureUID, result.captureName = container.UID, container.NamespacedName()
		result.containerID, result.systemID = container.Status.ContainerID, container.GetResourceId()
		result.started = !container.Status.StartedAt.IsZero()
		result.terminal = container.Status.Phase == apiv2.PhysicalContainerPhaseExited || container.Status.Phase == apiv2.PhysicalContainerPhaseFailed
	case *apiv2.LogicalContainer:
		result.captureUID = container.Status.PhysicalContainerUID
		result.captureName = commonapi.AsNamespacedName(container.Status.PhysicalContainerRef, container.Namespace)
		result.containerID, result.systemID = container.Status.ContainerID, container.GetResourceId()
		result.started = !container.Status.StartedAt.IsZero()
		result.terminal = container.Status.Phase == apiv2.LogicalContainerPhaseExited || container.Status.Phase == apiv2.LogicalContainerPhaseFailed
	default:
		return containerSnapshot{}, fmt.Errorf("unsupported container log resource %T", obj)
	}
	return result, nil
}

func (streamer *containerLogStreamer) Preflight(obj resource.Object, opts *apiv2.ResourceLogOptions) error {
	if optionsErr := opts.Validate(); optionsErr != nil {
		return apierrors.NewBadRequest(optionsErr.Error())
	}
	state, snapshotErr := snapshot(obj)
	if snapshotErr != nil {
		return apierrors.NewInternalError(snapshotErr)
	}
	if opts.Source != string(apiv2.LogStreamSourceSystem) && state.captureUID != "" && state.containerID != "" {
		name := state.captureName.Name
		if len(validation.IsDNS1123Subdomain(string(state.captureUID))) != 0 ||
			name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\:\x00") ||
			state.captureName.Namespace != obj.GetObjectMeta().Namespace {
			return apierrors.NewInternalError(fmt.Errorf("invalid observed physical log capture identity"))
		}
	}
	if !obj.GetObjectMeta().DeletionTimestamp.IsZero() && opts.Source != string(apiv2.LogStreamSourceSystem) {
		return apierrors.NewBadRequest("container resource is being deleted")
	}
	return nil
}

func (streamer *containerLogStreamer) StreamLogs(
	ctx context.Context, dest io.Writer, obj resource.Object, opts *apiv2.ResourceLogOptions, log logr.Logger,
) (logstream.ResourceStreamStatus, <-chan struct{}, error) {
	if preflightErr := streamer.Preflight(obj, opts); preflightErr != nil {
		return logstream.ResourceStreamStatusNotReady, nil, preflightErr
	}
	state, snapshotErr := snapshot(obj)
	if snapshotErr != nil {
		return logstream.ResourceStreamStatusNotReady, nil, snapshotErr
	}
	system := opts.Source == string(apiv2.LogStreamSourceSystem)
	if !system && (state.containerID == "" || (!state.started && !state.terminal) || state.captureUID == "") {
		if state.terminal {
			return logstream.ResourceStreamStatusDone, nil, nil
		}
		return logstream.ResourceStreamStatusNotReady, nil, nil
	}
	sessionCtx, cancelSession := context.WithCancel(ctx)
	session := &logSession{
		source: opts.Source, containerID: state.containerID, captureUID: state.captureUID, cancel: cancelSession,
		done: make(chan struct{}), finishing: !opts.Follow || (!system && state.terminal), destination: dest,
	}
	streamer.lock.Lock()
	if streamer.disposed {
		streamer.lock.Unlock()
		cancelSession()
		return logstream.ResourceStreamStatusDone, nil, fmt.Errorf("container log streamer has been disposed")
	}
	streamer.nextSession++
	sessionID := streamer.nextSession
	if streamer.sessions[state.ownerUID] == nil {
		streamer.sessions[state.ownerUID] = make(map[uint64]*logSession)
	}
	streamer.sessions[state.ownerUID][sessionID] = session
	streamer.lock.Unlock()

	path := logger.GetResourceLogPath(state.systemID)
	releaseCapture := func() {}
	if !system {
		capturePath, release, captureErr := streamer.runtimeLogPath(sessionCtx, state, opts)
		if captureErr != nil {
			streamer.finishSession(state.ownerUID, sessionID, session)
			return logstream.ResourceStreamStatusDone, nil, captureErr
		}
		path, releaseCapture = capturePath, release
	}
	go func() {
		defer streamer.finishSession(state.ownerUID, sessionID, session)
		defer releaseCapture()
		openErr := streamer.followFile(sessionCtx, path, dest, opts, session)
		if openErr != nil && sessionCtx.Err() == nil {
			log.Error(openErr, "Failed to stream container logs")
			if pipe, ok := dest.(interface{ CloseWithError(error) error }); ok {
				_ = pipe.CloseWithError(openErr)
			}
		}
	}()
	return logstream.ResourceStreamStatusStreaming, session.done, nil
}

func (streamer *containerLogStreamer) finishSession(uid types.UID, id uint64, session *logSession) {
	session.cancel()
	streamer.lock.Lock()
	delete(streamer.sessions[uid], id)
	if len(streamer.sessions[uid]) == 0 {
		delete(streamer.sessions, uid)
	}
	streamer.lock.Unlock()
	close(session.done)
}

func (streamer *containerLogStreamer) runtimeLogPath(
	ctx context.Context, state containerSnapshot, opts *apiv2.ResourceLogOptions) (string, func(), error) {
	streamer.captureLock.Lock()
	defer streamer.captureLock.Unlock()
	if contextErr := ctx.Err(); contextErr != nil {
		return "", nil, contextErr
	}
	hostCtx := contextdata.GetHostLifetimeContext(ctx)
	if streamer.captures == nil {
		streamer.captures = logs.NewLogDescriptorSet(hostCtx, usvc_io.DcpTempDir(), streamer.log)
	}
	if streamer.source == nil {
		source := container_flags.TryGetRemoteContainerOrchestrator(hostCtx, streamer.log)
		if source == nil {
			runtimeSource, runtimeErr := runtimes.FindAvailableContainerRuntime(ctx, streamer.log, contextdata.GetProcessExecutor(ctx))
			if runtimeErr != nil {
				return "", nil, runtimeErr
			}
			source = runtimeSource
		}
		streamer.source = source
	}
	captureCtx, cancelCapture := context.WithCancel(hostCtx)
	descriptor, stdout, stderr, created, acquireErr := streamer.captures.AcquireForResource(
		captureCtx, cancelCapture, state.captureName, state.captureUID)
	if acquireErr != nil {
		cancelCapture()
		return "", nil, acquireErr
	}
	if !created {
		cancelCapture()
	} else {
		captureErr := streamer.source.CaptureContainerLogs(descriptor.Context, state.containerID, stdout, stderr,
			containers.StreamContainerLogsOptions{Follow: true, Timestamps: true})
		if captureErr != nil {
			stdoutErr, stderrErr := stdout.Close(), stderr.Close()
			disposeErr := descriptor.Dispose(ctx, 0)
			return "", nil, errors.Join(captureErr, stdoutErr, stderrErr, disposeErr)
		}
	}
	stdoutPath, stderrPath, startErr := descriptor.LogConsumerStarting()
	if startErr != nil {
		return "", nil, startErr
	}
	path := stdoutPath
	if opts.Source == string(apiv2.LogStreamSourceStderr) {
		path = stderrPath
	}
	return path, descriptor.LogConsumerStopped, nil
}

func (streamer *containerLogStreamer) followFile(
	ctx context.Context, path string, dest io.Writer, opts *apiv2.ResourceLogOptions, session *logSession,
) error {
	file, openErr := resiliency.RetryGet(ctx, backoff.NewConstantBackOff(200*time.Millisecond), func() (*os.File, error) {
		opened, fileErr := usvc_io.OpenFileReadOnly(path)
		if fileErr != nil && (!os.IsNotExist(fileErr) || !opts.Follow) {
			return nil, resiliency.Permanent(fileErr)
		}
		return opened, fileErr
	})
	if openErr != nil {
		if os.IsNotExist(openErr) && !opts.Follow && opts.Source == string(apiv2.LogStreamSourceSystem) {
			return nil
		}
		return openErr
	}
	var source io.ReadCloser = file
	if opts.Tail != nil {
		source = usvc_io.NewTailReader(source, int(*opts.Tail))
	}
	source = usvc_io.NewTimestampAwareReader(source, usvc_io.TimestampAwareReaderOptions{
		Timestamps: opts.Timestamps, LineNumbers: opts.LineNumbers,
		Limit: pointers.GetValueOrDefault(opts.Limit, 0), Skip: pointers.GetValueOrDefault(opts.Skip, 0),
	})
	writer := usvc_io.NewFollowWriter(ctx, source, dest,
		usvc_io.WithNoDataStopRetries(3), usvc_io.WithCloseSourceOnCancel())
	streamer.lock.Lock()
	session.writer = writer
	if session.finishing {
		writer.StopFollow()
	}
	streamer.lock.Unlock()
	<-writer.Done()
	return writer.Err()
}

func (streamer *containerLogStreamer) OnResourceUpdated(evt apiv2.ResourceWatcherEvent, _ logr.Logger) {
	state, snapshotErr := snapshot(evt.Object)
	if snapshotErr != nil {
		streamer.log.Error(snapshotErr, "Failed to process container log resource update")
		return
	}
	_, physical := evt.Object.(*apiv2.PhysicalContainer)
	streamer.lock.Lock()
	for ownerUID, sessions := range streamer.sessions {
		for _, session := range sessions {
			system := session.source == string(apiv2.LogStreamSourceSystem)
			physicalSourceDeleted := physical && evt.Type == watch.Deleted && !system && session.captureUID == state.captureUID
			if ownerUID != state.ownerUID && !physicalSourceDeleted {
				continue
			}
			if evt.Type != watch.Deleted && (system || (!state.terminal && state.containerID == session.containerID)) {
				continue
			}
			if evt.Type == watch.Deleted && !system {
				session.finishing = true
				if session.writer != nil {
					session.writer.StopFollow()
				} else {
					session.cancel()
				}
				continue
			}
			if session.finishing {
				continue
			}
			session.finishing = true
			if session.writer != nil {
				logs.DelayStopFollowing([]*usvc_io.FollowWriter{session.writer})
			} else if evt.Type == watch.Deleted {
				session.cancel()
			}
		}
	}
	streamer.lock.Unlock()
	if evt.Type == watch.Deleted {
		if physical {
			streamer.captureLock.Lock()
			if streamer.captures != nil {
				streamer.captures.ReleaseForResource(state.captureUID)
			}
			streamer.captureLock.Unlock()
		}
	}
}

func (streamer *containerLogStreamer) Dispose() error {
	streamer.lock.Lock()
	if streamer.disposed {
		streamer.lock.Unlock()
		return nil
	}
	streamer.disposed = true
	var pending []*logSession
	for _, sessions := range streamer.sessions {
		for _, session := range sessions {
			session.cancel()
			pending = append(pending, session)
		}
	}
	streamer.lock.Unlock()
	var disposeErrors error
	for _, session := range pending {
		if destination, closable := session.destination.(io.Closer); closable {
			disposeErrors = errors.Join(disposeErrors, destination.Close())
		}
	}
	streamer.captureLock.Lock()
	captures := streamer.captures
	streamer.captureLock.Unlock()
	if captures != nil {
		disposeErrors = errors.Join(disposeErrors, captures.Dispose())
	}
	for _, session := range pending {
		<-session.done
	}
	return disposeErrors
}
