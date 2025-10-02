// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/pubsub"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

const (
	containerEventChanBuffer = 20 // Container events tend to come in bursts, so we can help a bit with buffering
)

// Provides data and common functionality for reconcilers that need to watch (Docker/Podman) containers
type ContainerWatcher[T commonapi.ObjectStruct] struct {
	// Callbacks to execute when a container or network event is received
	ProcessContainerEvent func(em containers.EventMessage)
	ProcessNetworkEvent   func(em containers.EventMessage)

	// The orchestrator used to watch containers
	orchestrator containers.ContainerOrchestrator

	// Container events subscription
	containerEvtSub *pubsub.Subscription[containers.EventMessage]

	// Network events subscription
	networkEvtSub *pubsub.Subscription[containers.EventMessage]

	// Channel to receive container change events
	containerEvtCh *concurrency.UnboundedChan[containers.EventMessage]

	// Cancel function for the container event channel
	containerEvtChCancel context.CancelFunc

	// Channel to receive network change events
	networkEvtCh *concurrency.UnboundedChan[containers.EventMessage]

	// Cancel function for the network event channel
	networkEvtChCancel context.CancelFunc

	// Channel to stop the event worker
	containerEvtWorkerStop chan struct{}

	// Effectively a set of UIDs of objects that are being watched.
	// When this set becomes empty, we cancel the container watch.
	watchingResources *syncmap.Map[types.UID, bool]

	// Lock to protect the data above from concurrent access
	lock sync.Locker

	// Lifetime context, cancelled when the watcher is being shut down
	lifetimeCtx context.Context

	// The kind of the object being reconciled, for logging purposes
	kind string
}

func NewContainerWatcher[T commonapi.ObjectStruct](
	orchestrator containers.ContainerOrchestrator,
	lock sync.Locker,
	lifetimeCtx context.Context,
) *ContainerWatcher[T] {
	return &ContainerWatcher[T]{
		orchestrator:      orchestrator,
		watchingResources: &syncmap.Map[types.UID, bool]{},
		lock:              lock,
		lifetimeCtx:       lifetimeCtx,
		kind:              reflect.TypeFor[T]().Name(),
		// Other members are initialized when starting the watch
	}
}

func (r *ContainerWatcher[T]) EnsureContainerWatchForResource(resourceID types.UID, log logr.Logger) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.lifetimeCtx.Err() != nil {
		return // Do not start a container watch if we are done
	}

	if r.ProcessContainerEvent == nil && r.ProcessNetworkEvent == nil {
		return // No one is listening
	}

	_, _ = r.watchingResources.LoadOrStore(resourceID, true)

	var containerSub, networkSub *pubsub.Subscription[containers.EventMessage]
	var containerSubErr, networkSubErr error

	if r.ProcessContainerEvent != nil && r.containerEvtSub == nil {
		containerEvtChCtx, containerEvtChCancel := context.WithCancel(r.lifetimeCtx)
		r.containerEvtCh = concurrency.NewUnboundedChanBuffered[containers.EventMessage](
			containerEvtChCtx,
			containerEventChanBuffer,
			containerEventChanBuffer,
		)
		r.containerEvtChCancel = containerEvtChCancel
		containerSub, containerSubErr = r.orchestrator.WatchContainers(r.containerEvtCh.In)
	}

	if r.ProcessNetworkEvent != nil && r.networkEvtSub == nil {
		networkEvtChCtx, networkEvtChCancel := context.WithCancel(r.lifetimeCtx)
		r.networkEvtCh = concurrency.NewUnboundedChanBuffered[containers.EventMessage](
			networkEvtChCtx,
			containerEventChanBuffer,
			containerEventChanBuffer,
		)
		r.networkEvtChCancel = networkEvtChCancel
		networkSub, networkSubErr = r.orchestrator.WatchNetworks(r.networkEvtCh.In)
	}

	if r.containerEvtWorkerStop == nil {
		r.containerEvtWorkerStop = make(chan struct{})
		var workerCtrCh, workerNetCh <-chan containers.EventMessage
		if r.containerEvtCh != nil {
			workerCtrCh = r.containerEvtCh.Out
		}
		if r.networkEvtCh != nil {
			workerNetCh = r.networkEvtCh.Out
		}
		go r.containerEventWorker(r.containerEvtWorkerStop, workerCtrCh, workerNetCh)
	}

	err := errors.Join(containerSubErr, networkSubErr)

	if err != nil {
		log.Error(err, "Could not subscribe to container events")
		r.cancelContainerWatch()
		return
	}

	r.containerEvtSub = containerSub
	r.networkEvtSub = networkSub
}

func (r *ContainerWatcher[T]) ReleaseContainerWatchForResource(resourceID types.UID, log logr.Logger) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.watchingResources.Delete(resourceID)
	if r.watchingResources.Empty() {
		log.Info(fmt.Sprintf("No more %s resources are being watched, cancelling container watch", r.kind))
		r.cancelContainerWatch()
	}
}

func (r *ContainerWatcher[T]) cancelContainerWatch() {
	// Assumes r.lock is held

	if r.containerEvtWorkerStop != nil {
		close(r.containerEvtWorkerStop)
		r.containerEvtWorkerStop = nil
	}
	if r.containerEvtSub != nil {
		r.containerEvtSub.Cancel()
		r.containerEvtSub = nil
	}
	if r.containerEvtChCancel != nil {
		r.containerEvtChCancel()
		r.containerEvtChCancel = nil
		r.containerEvtCh = nil
	}
	if r.networkEvtSub != nil {
		r.networkEvtSub.Cancel()
		r.networkEvtSub = nil
	}
	if r.networkEvtChCancel != nil {
		r.networkEvtChCancel()
		r.networkEvtChCancel = nil
		r.networkEvtCh = nil
	}
}

func (r *ContainerWatcher[T]) containerEventWorker(
	stopCh chan struct{},
	containerEvtCh <-chan containers.EventMessage,
	networkEvtCh <-chan containers.EventMessage,
) {
	for {
		select {

		case cem, isOpen := <-containerEvtCh:
			if !isOpen {
				containerEvtCh = nil
				continue
			}

			if cem.Source != containers.EventSourceContainer {
				continue
			}

			if r.ProcessContainerEvent != nil {
				r.ProcessContainerEvent(cem)
			}

		case nem, isOpen := <-networkEvtCh:
			if !isOpen {
				networkEvtCh = nil
				continue
			}

			if nem.Source != containers.EventSourceNetwork {
				continue
			}

			if r.ProcessNetworkEvent != nil {
				r.ProcessNetworkEvent(nem)
			}

		case <-stopCh:
			return
		}
	}
}
