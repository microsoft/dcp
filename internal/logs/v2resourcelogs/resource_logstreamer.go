/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2resourcelogs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/resiliency"
)

type resourceSnapshot struct {
	ownerUID types.UID
	systemID string
	runtime  *apiv2.LogSource
	startup  *apiv2.LogSource
	complete bool
}

type logSession struct {
	source      string
	capture     apiv2.LogSource
	cancel      context.CancelFunc
	done        chan struct{}
	writer      *usvc_io.FollowWriter
	finishing   bool
	destination io.Writer
}

type resourceLogStreamer struct {
	lock        sync.Mutex
	sessions    map[types.UID]map[uint64]*logSession
	nextSession uint64
	disposed    bool
	captureLock sync.Mutex
	captures    *logs.LogDescriptorSet
	source      containers.ContainerLogSource
	log         logr.Logger
}

// NewLogStreamer creates shared capture with resource-scoped client sessions
// for logical containers, physical containers, and physical images.
func NewLogStreamer(log logr.Logger) *resourceLogStreamer {
	return &resourceLogStreamer{
		sessions: make(map[types.UID]map[uint64]*logSession),
		log:      log,
	}
}

func snapshot(obj resource.Object) (resourceSnapshot, error) {
	result := resourceSnapshot{ownerUID: obj.GetObjectMeta().UID}
	switch container := obj.(type) {
	case *apiv2.PhysicalContainer:
		result.runtime = container.GetLogSource()
		result.systemID = container.GetResourceId()
		result.complete = result.runtime.Complete
	case *apiv2.LogicalContainer:
		result.runtime, result.startup = container.Status.RuntimeLogs, container.Status.StartupLogs
		result.systemID = container.GetResourceId()
		result.complete = container.Status.Phase == apiv2.LogicalContainerPhaseExited || container.Status.Phase == apiv2.LogicalContainerPhaseFailed
		if result.startup == nil && (container.Spec.EffectiveMode() == apiv2.LogicalContainerModeExisting ||
			!container.Status.StartedAt.IsZero() || result.complete) {
			result.startup = &apiv2.LogSource{Complete: true}
		}
	case *apiv2.PhysicalContainerImage:
		result.systemID = container.GetResourceId()
		result.runtime = container.GetLogSource()
		result.complete = result.runtime.Complete
	case *apiv2.PhysicalContainerNetwork:
		result.systemID = container.GetResourceId()
	case *apiv2.LogicalContainerNetwork:
		result.systemID = container.GetResourceId()
	case *apiv2.PhysicalProcess:
		result.systemID = container.GetResourceId()
		result.runtime = container.GetLogSource()
		result.complete = result.runtime.Complete
	case *apiv2.LogicalProcess:
		result.systemID = container.GetResourceId()
		result.runtime = container.Status.RuntimeLogs
		result.complete = container.Status.Phase == apiv2.LogicalProcessPhaseExited ||
			container.Status.Phase == apiv2.LogicalProcessPhaseFailed
	default:
		return resourceSnapshot{}, fmt.Errorf("unsupported log resource %T", obj)
	}
	return result, nil
}

func (state resourceSnapshot) logSource(source string) *apiv2.LogSource {
	switch source {
	case string(apiv2.LogStreamSourceStartupStdout), string(apiv2.LogStreamSourceStartupStderr):
		return state.startup
	default:
		return state.runtime
	}
}

func (streamer *resourceLogStreamer) Preflight(obj resource.Object, opts *apiv2.ResourceLogOptions) error {
	if optionsErr := opts.Validate(); optionsErr != nil {
		return apierrors.NewBadRequest(optionsErr.Error())
	}
	state, snapshotErr := snapshot(obj)
	if snapshotErr != nil {
		return apierrors.NewInternalError(snapshotErr)
	}
	switch obj.(type) {
	case *apiv2.PhysicalContainerNetwork, *apiv2.LogicalContainerNetwork:
		if opts.Source != string(apiv2.LogStreamSourceSystem) {
			return apierrors.NewBadRequest("network log source must be system")
		}
	}
	startup := opts.Source == string(apiv2.LogStreamSourceStartupStdout) || opts.Source == string(apiv2.LogStreamSourceStartupStderr)
	if _, physical := obj.(*apiv2.PhysicalContainer); physical && startup {
		return apierrors.NewBadRequest("physical container log sources are stdout, stderr, and system")
	}
	if _, image := obj.(*apiv2.PhysicalContainerImage); image && startup {
		return apierrors.NewBadRequest("image log sources are stdout, stderr, and system")
	}
	switch obj.(type) {
	case *apiv2.LogicalProcess, *apiv2.PhysicalProcess:
		if startup {
			return apierrors.NewBadRequest("process log sources are stdout, stderr, and system")
		}
	}
	capture := state.logSource(opts.Source)
	if opts.Source != string(apiv2.LogStreamSourceSystem) && capture != nil && capture.Available {
		name := capture.Name
		if len(validation.IsDNS1123Subdomain(string(capture.UID))) != 0 ||
			name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\:\x00") {
			return apierrors.NewInternalError(fmt.Errorf("invalid observed log capture identity"))
		}
		if capture.ContainerID == "" {
			for _, output := range []struct {
				path   string
				source logstream.LogStreamSource
			}{
				{capture.StdoutFile, logstream.LogStreamSourceStdout},
				{capture.StderrFile, logstream.LogStreamSourceStderr},
			} {
				expectedName, nameErr := logstream.LogFileName(name, capture.UID, output.source, "")
				if nameErr != nil || !filepath.IsAbs(output.path) || filepath.Base(output.path) != expectedName {
					return apierrors.NewInternalError(fmt.Errorf("invalid observed log capture path"))
				}
			}
		}
	}
	if !obj.GetObjectMeta().DeletionTimestamp.IsZero() && opts.Source != string(apiv2.LogStreamSourceSystem) {
		return apierrors.NewBadRequest("log resource is being deleted")
	}
	return nil
}

func (streamer *resourceLogStreamer) StreamLogs(
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
	capture := state.logSource(opts.Source)
	if !system && (capture == nil || !capture.Available) {
		if state.complete || (capture != nil && capture.Complete) {
			return logstream.ResourceStreamStatusDone, nil, nil
		}
		return logstream.ResourceStreamStatusNotReady, nil, nil
	}
	sessionCtx, cancelSession := context.WithCancel(ctx)
	session := &logSession{
		source: opts.Source, cancel: cancelSession,
		done: make(chan struct{}), finishing: !opts.Follow, destination: dest,
	}
	if !system {
		session.capture = *capture
		session.finishing = !opts.Follow || capture.Complete
	}
	streamer.lock.Lock()
	if streamer.disposed {
		streamer.lock.Unlock()
		cancelSession()
		return logstream.ResourceStreamStatusDone, nil, fmt.Errorf("resource log streamer has been disposed")
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
		path = capture.StdoutFile
		if opts.Source == string(apiv2.LogStreamSourceStderr) || opts.Source == string(apiv2.LogStreamSourceStartupStderr) {
			path = capture.StderrFile
		}
		if capture.ContainerID != "" {
			capturePath, release, captureErr := streamer.runtimeLogPath(sessionCtx, obj.GetObjectMeta().Namespace, *capture, opts)
			if captureErr != nil {
				streamer.finishSession(state.ownerUID, sessionID, session)
				return logstream.ResourceStreamStatusDone, nil, captureErr
			}
			path, releaseCapture = capturePath, release
		}
	}
	go func() {
		defer streamer.finishSession(state.ownerUID, sessionID, session)
		defer releaseCapture()
		openErr := streamer.followFile(sessionCtx, path, dest, opts, session)
		if openErr != nil && sessionCtx.Err() == nil {
			log.Error(openErr, "Failed to stream resource logs")
			if pipe, ok := dest.(interface{ CloseWithError(error) error }); ok {
				_ = pipe.CloseWithError(openErr)
			}
		}
	}()
	return logstream.ResourceStreamStatusStreaming, session.done, nil
}

func (streamer *resourceLogStreamer) finishSession(uid types.UID, id uint64, session *logSession) {
	session.cancel()
	streamer.lock.Lock()
	delete(streamer.sessions[uid], id)
	if len(streamer.sessions[uid]) == 0 {
		delete(streamer.sessions, uid)
	}
	streamer.lock.Unlock()
	close(session.done)
}

func (streamer *resourceLogStreamer) runtimeLogPath(
	ctx context.Context, namespace string, capture apiv2.LogSource, opts *apiv2.ResourceLogOptions) (string, func(), error) {
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
		captureCtx, cancelCapture, types.NamespacedName{Namespace: namespace, Name: capture.Name}, capture.UID)
	if acquireErr != nil {
		cancelCapture()
		return "", nil, acquireErr
	}
	if !created {
		cancelCapture()
	} else {
		captureErr := streamer.source.CaptureContainerLogs(descriptor.Context, capture.ContainerID, stdout, stderr,
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

func (streamer *resourceLogStreamer) followFile(
	ctx context.Context, path string, dest io.Writer, opts *apiv2.ResourceLogOptions, session *logSession,
) error {
	file, openErr := resiliency.RetryGet(ctx, backoff.NewConstantBackOff(200*time.Millisecond), func() (*os.File, error) {
		opened, fileErr := usvc_io.OpenFileReadOnly(path)
		streamer.lock.Lock()
		finishing := session.finishing
		streamer.lock.Unlock()
		if fileErr != nil && (!os.IsNotExist(fileErr) || !opts.Follow || finishing) {
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

func (streamer *resourceLogStreamer) OnResourceUpdated(evt apiv2.ResourceWatcherEvent, _ logr.Logger) {
	switch evt.Type {
	case watch.Added, watch.Modified, watch.Deleted:
	default:
		return
	}
	state, snapshotErr := snapshot(evt.Object)
	if snapshotErr != nil {
		streamer.log.Error(snapshotErr, "Failed to process log resource update")
		return
	}
	_, physical := evt.Object.(*apiv2.PhysicalContainer)
	_, image := evt.Object.(*apiv2.PhysicalContainerImage)
	_, process := evt.Object.(*apiv2.PhysicalProcess)
	var streamsToStop []*usvc_io.FollowWriter
	streamer.lock.Lock()
	for ownerUID, sessions := range streamer.sessions {
		for _, session := range sessions {
			system := session.source == string(apiv2.LogStreamSourceSystem)
			sourceUpdated := !system && (physical || image || process) && session.capture.UID == state.ownerUID
			if ownerUID != state.ownerUID && !sourceUpdated {
				continue
			}
			capture := state.logSource(session.source)
			if sourceUpdated {
				capture = state.runtime
			}
			if evt.Type != watch.Deleted &&
				(system || capture == nil || capture.UID != session.capture.UID || !capture.Complete) {
				continue
			}
			if session.finishing {
				continue
			}
			session.finishing = true
			if session.writer != nil {
				streamsToStop = append(streamsToStop, session.writer)
			} else if evt.Type == watch.Deleted {
				session.cancel()
			}
		}
	}
	streamer.lock.Unlock()
	if len(streamsToStop) > 0 {
		logs.DelayStopFollowing(streamsToStop)
	}
	if evt.Type == watch.Deleted {
		if physical {
			streamer.captureLock.Lock()
			if streamer.captures != nil {
				streamer.captures.ReleaseForResource(state.ownerUID)
			}
			streamer.captureLock.Unlock()
		}
	}
}

func (streamer *resourceLogStreamer) Dispose() error {
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
