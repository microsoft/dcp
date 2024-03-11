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
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apimachinery/pkg/watch"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	"github.com/microsoft/usvc-apiserver/internal/contextdata"
	"github.com/microsoft/usvc-apiserver/internal/logs"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

var (
	// A map of Container resource UID to the log descriptor for the container.
	// Used for streaming logs from the container.
	containerLogs *logs.LogDescriptorSet

	containerOrchestrator containers.ContainerOrchestrator = nil
	containerWatcher      watch.Interface                  = nil
	lock                                                   = &sync.Mutex{}
)

func CreateContainerLogStream(
	requestCtx context.Context,
	obj apiserver_resource.Object,
	opts *apiv1.LogOptions,
	parentKindStorage registry_rest.StandardStorage,
) (io.ReadCloser, error) {
	ctr, isContainer := obj.(*apiv1.Container)
	if !isContainer {
		return nil, apierrors.NewInternalError(fmt.Errorf("parent storage returned object of wrong type (not Container): %s", obj.GetObjectKind().GroupVersionKind().String()))
	}

	deletionRequested := ctr.DeletionTimestamp != nil && !ctr.DeletionTimestamp.IsZero()
	if deletionRequested {
		return nil, apierrors.NewBadRequest("Container is being deleted")
	}

	switch ctr.Status.State {

	case apiv1.ContainerStateUnknown, apiv1.ContainerStateNotFound, apiv1.ContainerStateRemoved:
		return nil, apierrors.NewBadRequest(fmt.Sprintf("logs are not available for Container in state %s", ctr.Status.State))

	case apiv1.ContainerStatePending, apiv1.ContainerStateStarting:
		// TODO: need to wait for the logs to be availabe if opts.Follow is true
		return nil, nil
	}

	if ctr.Status.ContainerID == "" {
		// TODO: need to wait for the logs to be availabe if opts.Follow is true
		return nil, nil
	}

	if opts.Source != "" && opts.Source != string(apiv1.LogStreamSourceStdout) && opts.Source != string(apiv1.LogStreamSourceStderr) {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("Invalid log source '%s'. Supported log sources are '%s' and '%s'", opts.Source, apiv1.LogStreamSourceStdout, apiv1.LogStreamSourceStderr))
	}

	hostLifetimeCtx := contextdata.GetHostLifetimeContext(requestCtx)
	log := contextdata.GetContextLogger(requestCtx)

	co, err := ensureDependencies(requestCtx, parentKindStorage, log)
	if err != nil {
		return nil, err
	}

	logDescriptorCtx, cancel := context.WithCancel(hostLifetimeCtx)
	ld, stdOutWriter, stdErrWriter, newlyCreated, err := containerLogs.AcquireForResource(logDescriptorCtx, cancel, ctr.NamespacedName(), ctr.UID)
	if err != nil {
		log.Error(err, "Failed to enable log capturing for Container", "Container", ctr.NamespacedName())
		return nil, apierrors.NewInternalError(err)
	}

	if newlyCreated {
		// Need to start log capturing for the container
		logCaptureErr := co.CaptureContainerLogs(ld.Context, ctr.Status.ContainerID, stdOutWriter, stdErrWriter, containers.StreamContainerLogsOptions{
			Follow:     true,
			Timestamps: opts.Timestamps,
		})
		if logCaptureErr != nil {
			log.Error(logCaptureErr, "Failed to start capturing logs for Container", "Container", ctr.NamespacedName())
			disposeErr := ld.Dispose(requestCtx, 0)
			if disposeErr != nil {
				log.V(1).Info("Failed to dispose log descriptor after failed log capture",
					"Container", ctr.NamespacedName(),
					"Error", disposeErr.Error())
			}
			return nil, apierrors.NewInternalError(logCaptureErr)
		}
	}

	stdOutPath, stdErrPath, err := ld.LogConsumerStarting()
	if err != nil {
		// This can happen if the log descriptor is being disposed because the Container is being deleted
		// We just report not found in this case
		return nil, apierrors.NewNotFound(ctr.GetGroupVersionResource().GroupResource(), ctr.NamespacedName().Name)
	}

	var logFilePath string
	if opts.Source == string(apiv1.LogStreamSourceStdout) || opts.Source == "" {
		logFilePath = stdOutPath
	} else {
		logFilePath = stdErrPath
	}

	reader, writer := io.Pipe()
	go func() {
		logWatchErr := logs.WatchLogs(requestCtx, logFilePath, writer, logs.WatchLogOptions{Follow: opts.Follow})
		if logWatchErr != nil {
			log.Error(logWatchErr, "Failed to watch Container logs",
				"Container", ctr.NamespacedName(),
				"LogFilePath", logFilePath,
			)
		}
		ld.LogConsumerStopped()
	}()

	return reader, nil
}

func ensureDependencies(requestCtx context.Context, parentKindStorage registry_rest.StandardStorage, log logr.Logger) (containers.ContainerOrchestrator, error) {
	hostLifetimeCtx := contextdata.GetHostLifetimeContext(requestCtx)
	ensureContainerLogDescriptors(hostLifetimeCtx)

	containerWatcherErr := ensureContainerWatcher(hostLifetimeCtx, parentKindStorage, log)
	if containerWatcherErr != nil {
		log.Error(containerWatcherErr, "failed to create Container watcher")
		return nil, apierrors.NewInternalError(containerWatcherErr)
	}

	co, coErr := ensureContainerOrchestrator(requestCtx, log)
	if coErr != nil {
		log.Error(coErr, "failed to get Container orchestrator")
		return nil, apierrors.NewInternalError(coErr)
	}

	return co, nil
}

func ensureContainerOrchestrator(requestCtx context.Context, log logr.Logger) (containers.ContainerOrchestrator, error) {
	lock.Lock()
	defer lock.Unlock()
	if containerOrchestrator != nil {
		return containerOrchestrator, nil
	}

	pe := contextdata.GetProcessExecutor(requestCtx)
	hostLifetimeCtx := contextdata.GetHostLifetimeContext(requestCtx)
	co, err := container_flags.GetContainerOrchestrator(hostLifetimeCtx, log.WithName("ContainerOrchestrator").WithValues("ContainerRuntime", container_flags.GetRuntimeFlagArg()), pe)
	if err != nil {
		return nil, err
	}

	containerOrchestrator = co
	return co, nil
}

func ensureContainerWatcher(hostLifetimeCtx context.Context, containerStorage registry_rest.StandardStorage, log logr.Logger) error {
	lock.Lock()
	defer lock.Unlock()
	if containerWatcher != nil {
		return nil
	}

	opts := metainternalversion.ListOptions{
		Watch: true,
	}
	w, err := containerStorage.Watch(hostLifetimeCtx, &opts)
	if err != nil {
		return err
	}
	containerWatcher = w
	go processContainerEvents(hostLifetimeCtx, containerWatcher, log)

	return nil
}

func ensureContainerLogDescriptors(hostLifetimeCtx context.Context) {
	lock.Lock()
	defer lock.Unlock()
	if containerLogs != nil {
		return
	}

	logFolder := os.TempDir()
	if dcpSessionDir, found := os.LookupEnv(logger.DCP_SESSION_FOLDER); found {
		logFolder = dcpSessionDir
	}

	containerLogs = logs.NewLogDescriptorSet(hostLifetimeCtx, logFolder)
}

func processContainerEvents(hostLifetimeCtx context.Context, containerWatcher watch.Interface, log logr.Logger) {
	ensureContainerLogDescriptors(hostLifetimeCtx)

	for {
		select {

		case <-hostLifetimeCtx.Done():
			containerWatcher.Stop()
			return

		case event, isChanOpen := <-containerWatcher.ResultChan():
			if !isChanOpen {
				return
			}

			if event.Type != watch.Deleted {
				continue
			}

			ctr, isContainer := event.Object.(*apiv1.Container)
			if !isContainer {
				log.V(1).Info("container watcher received a Deleted event for an object that is not a Container", "ObjectKind", event.Object.GetObjectKind().GroupVersionKind().String())
				continue
			}

			// Need to stop the log streamer and any log watchers for this container (if any) as it is being deleted.
			// It is OK to call RealaeseForResource() if the resource is not in the set, it is a no-op in that case.
			containerLogs.ReleaseForResource(ctr.UID)
		}
	}
}
