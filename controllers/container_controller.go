// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	"github.com/smallnest/chanx"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	ctrl_handler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	ct "github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
)

const (
	containerEventChanInitialCapacity = 20
)

var (
	containerFinalizer string = fmt.Sprintf("%s/container-reconciler", apiv1.GroupVersion.Group)
)

type ContainerReconciler struct {
	ctrl_client.Client
	Log          logr.Logger
	orchestrator ct.ContainerOrchestrator

	// Channel uset to trigger reconciliation when underlying containers change
	notifyContainerChanged chan ctrl_event.GenericEvent

	// A map that stores information about running containers,
	// searchable by container ID (first key), or Container object name (second key).
	// Currently we only store startup error, if any.
	runningContainers *maps.SynchronizedDualKeyMap[string, types.NamespacedName, error]

	// Container events subscription
	containerEvtSub ct.EventSubscription
	// Channel to receive container change events
	containerEvtCh         *chanx.UnboundedChan[ct.EventMessage]
	containerEvtWorkerStop chan struct{}

	// Debouncer used to schedule reconciliation. Extra data is the running container ID whose state changed.
	debouncer *reconcilerDebouncer[string]

	// Lock to protect the reconciler data that requires synchronized access
	lock *sync.Mutex

	// A reasonably-unique string identifying the reconciler instance.
	// Used to indicate the controller that "owns" a running container.
	reconcilerId string

	// Reconciler lifetime context, used to cancel container watch during reconciler shutdown
	lifetimeCtx context.Context
}

func NewContainerReconciler(lifetimeCtx context.Context, client ctrl_client.Client, log logr.Logger, orchestrator ct.ContainerOrchestrator) *ContainerReconciler {
	r := ContainerReconciler{
		Client:                 client,
		orchestrator:           orchestrator,
		notifyContainerChanged: make(chan ctrl_event.GenericEvent),
		runningContainers:      maps.NewSynchronizedDualKeyMap[string, types.NamespacedName, error](),
		containerEvtSub:        nil,
		containerEvtCh:         chanx.NewUnboundedChan[ct.EventMessage](lifetimeCtx, containerEventChanInitialCapacity),
		containerEvtWorkerStop: nil,
		debouncer:              newReconcilerDebouncer[string](reconciliationDebounceDelay),
		lock:                   &sync.Mutex{},
		lifetimeCtx:            lifetimeCtx,
	}

	r.Log = log.WithValues("Controller", containerFinalizer)

	go r.onShutdown()

	return &r
}

func (r *ContainerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	src := ctrl_source.Channel{
		Source: r.notifyContainerChanged,
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Container{}).
		WatchesRawSource(&src, &ctrl_handler.EnqueueRequestForObject{}).
		Complete(r)
}

func (r *ContainerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("ContainerName", req.NamespacedName)

	r.debouncer.OnReconcile(req.NamespacedName)

	select {
	case _, isOpen := <-ctx.Done():
		if !isOpen {
			log.Info("Request context expired, nothing to do...")
			return ctrl.Result{}, nil
		}
	default: // not done, proceed
	}

	container := apiv1.Container{}
	err := r.Get(ctx, req.NamespacedName, &container)

	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("the Container object was deleted")
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the Container object")
			return ctrl.Result{}, err
		}
	}

	var change objectChange
	patch := ctrl_client.MergeFrom(container.DeepCopy())

	if container.DeletionTimestamp != nil && !container.DeletionTimestamp.IsZero() {
		log.Info("Container object is being deleted...")
		r.deleteContainer(ctx, &container, log)
		change = deleteFinalizer(&container, containerFinalizer)
		// Removing the finalizer will unblock the deletion of the Container object.
		// Status update will fail, because the object will no longer be there, so suppress it.
		change &= ^statusChanged
	} else {
		change = ensureFinalizer(&container, containerFinalizer)
		change |= r.manageContainer(ctx, &container, log)
	}

	if change == noChange {
		log.Info("no changes detected for Container object, continue monitoring...")
		return ctrl.Result{}, nil
	}

	var update *apiv1.Container

	if (change & (metadataChanged | specChanged)) != 0 {
		update = container.DeepCopy()
		err = r.Patch(ctx, update, patch)
		if err != nil {
			log.Error(err, "Container object update failed")
			return ctrl.Result{}, err
		} else {
			log.Info("Container object update succeeded")
		}
	}

	if (change & statusChanged) != 0 {
		update = container.DeepCopy()
		err = r.Status().Patch(ctx, update, patch)
		if err != nil {
			log.Error(err, "Container status update failed")
			return ctrl.Result{}, err
		} else {
			log.Info("Container status update succeeded")
		}
	}

	if (change & additionalReconciliationNeeded) != 0 {
		log.Info("scheduling additional reconciliation for Container...")
		return ctrl.Result{RequeueAfter: additionalReconciliationDelay}, nil
	} else {
		return ctrl.Result{}, nil
	}
}

func (r *ContainerReconciler) deleteContainer(ctx context.Context, container *apiv1.Container, log logr.Logger) {
	if container.Status.OwningController != "" && container.Status.OwningController != r.reconcilerId {
		// Someone else is managing this Container
		return
	}

	// r.runningContainers should have the latest data
	containerID, _, found := r.runningContainers.FindBySecondKey(container.NamespacedName())
	if !found {
		containerID = container.Status.ContainerID
	}

	// Since the container is being removed, we want to remove it from runningContainers map now
	if found {
		r.runningContainers.DeleteBySecondKey(container.NamespacedName())
	}

	if containerID == "" {
		// This can happen if the container was never started -- nothing to do
		return
	}

	_, err := r.orchestrator.RemoveContainers(ctx, []string{containerID}, true /*force*/)
	if err != nil {
		log.Error(err, "could not remove the running container corresponding to Container object", "ContainerID", containerID)
	}
}

func (r *ContainerReconciler) manageContainer(ctx context.Context, container *apiv1.Container, log logr.Logger) objectChange {
	if container.Status.OwningController != "" && container.Status.OwningController != r.reconcilerId {
		// We are not managing this Container
		return noChange
	}

	containerID, _, found := r.runningContainers.FindBySecondKey(container.NamespacedName())

	if container.Status.State == "" || container.Status.State == apiv1.ContainerStatePending {
		// Check if we haven't already started this (sometimes we might get stale data from the object cache).
		if found {
			// Just wait a bit for the cache to catch up and reconcile again
			return additionalReconciliationNeeded
		}

		// Nope, we need to attempt to start the container.
		r.ensureContainerWatch(log)
		return r.startContainer(ctx, container, log)
	}

	done := container.Status.State == apiv1.ContainerStateUnknown ||
		container.Status.State == apiv1.ContainerStateFailedToStart ||
		container.Status.State == apiv1.ContainerStateExited ||
		container.Status.State == apiv1.ContainerStateRemoved
	if done {
		// Now that the status indicates that the container is done,
		// we no longer need to track it in the runningContainers map.
		r.runningContainers.DeleteBySecondKey(container.NamespacedName())

		return noChange
	}

	if !found {
		// This should never really happen--we should be tracking this container via our runningContainers map.
		// Not much we can do at this point, let's mark it as finished-unknown state
		log.Error(fmt.Errorf("missing running container data"), "", "ContainerID", container.Status.ContainerID)
		container.Status.State = apiv1.ContainerStateUnknown
		container.Status.FinishTimestamp = metav1.Now()
		return statusChanged
	}

	res, err := r.orchestrator.InspectContainers(ctx, []string{containerID})
	if err != nil || len(res) == 0 {
		// The container was probably removed
		container.Status.State = apiv1.ContainerStateRemoved
		container.Status.FinishTimestamp = metav1.Now()
		r.runningContainers.DeleteBySecondKey(container.NamespacedName())
		return statusChanged
	} else {
		inspected := res[0]
		return r.updateContainerStatus(container, &inspected)
	}
}

func (r *ContainerReconciler) startContainer(ctx context.Context, container *apiv1.Container, log logr.Logger) objectChange {
	var err error

	log.Info("starting container",
		"image", container.Spec.Image,
	)

	var cs apiv1.ContainerStatus
	cs.OwningController = r.reconcilerId

	cs.ExitCode = apiv1.UnknownExitCode

	opts := ct.RunContainerOptions{
		ContainerSpec: container.Spec,
	}

	cs.StartupTimestamp = metav1.Now()

	containerID, err := r.orchestrator.RunContainer(ctx, opts)
	if err != nil {
		log.Error(err, "could not start the container")
		cs.ContainerID = ""
		cs.State = apiv1.ContainerStateFailedToStart
		cs.Message = fmt.Sprintf("Container could not be started: %s", err.Error())
	} else {
		log.Info("container started", "ContainerID", containerID)
		cs.ContainerID = containerID
		cs.State = apiv1.ContainerStateRunning
	}

	container.Status = cs
	r.runningContainers.Store(containerID, container.NamespacedName(), err)

	return statusChanged
}

func (r *ContainerReconciler) updateContainerStatus(container *apiv1.Container, inspected *ct.InspectedContainer) objectChange {
	status := container.Status
	oldState := status.State

	switch inspected.Status {
	case ct.ContainerStatusCreated, ct.ContainerStatusRunning, ct.ContainerStatusRestarting:
		status.State = apiv1.ContainerStateRunning
	case ct.ContainerStatusPaused:
		status.State = apiv1.ContainerStatePaused
	case ct.ContainerStatusExited, ct.ContainerStatusDead:
		status.State = apiv1.ContainerStateExited
		status.ExitCode = inspected.ExitCode
		if !inspected.FinishedAt.IsZero() {
			status.FinishTimestamp = metav1.NewTime(inspected.FinishedAt)
		} else {
			status.FinishTimestamp = metav1.Now()
		}
		r.runningContainers.DeleteBySecondKey(container.NamespacedName())
	}

	if oldState != status.State {
		container.Status = status
		return statusChanged
	} else {
		return noChange
	}
}

func (r *ContainerReconciler) ensureContainerWatch(log logr.Logger) {
	r.lock.Lock()
	defer r.lock.Unlock()

	select {
	case <-r.lifetimeCtx.Done():
		return // Do not start a container watch if we are done
	default:
		if r.containerEvtSub != nil {
			return // We are already watching container events
		}
	}

	r.containerEvtWorkerStop = make(chan struct{})
	go r.containerEventWorker(r.containerEvtWorkerStop)

	sub, err := r.orchestrator.WatchContainers(r.lifetimeCtx, r.containerEvtCh.In)
	if err != nil {
		log.Error(err, "could not subscribe to containter events")
		close(r.containerEvtWorkerStop)
		r.containerEvtWorkerStop = nil
		return
	}

	r.containerEvtSub = sub

	// CONSIDER cancelling the container event watch if no containers are running.
	// E.g. using ResourceSemaphore idea, which would have, in addition to regular Inc and Dec operations,
	// a set of callbacks that are invoked on increment, on decrement, on increment from zero,
	// on decrement from zero, and a "cleanup" one invoked when the semaphore stayed at zero for a while
	// (configurable delay).
}

func (r *ContainerReconciler) containerEventWorker(stopCh chan struct{}) {
	for {
		select {
		case em := <-r.containerEvtCh.Out:
			if em.Source != ct.EventSourceContainer {
				continue
			}

			r.processContainerEvent(em)

		case <-stopCh:
			return
		}
	}
}

func (r *ContainerReconciler) processContainerEvent(em ct.EventMessage) {
	switch em.Action {
	// Any event that means the container has been started, stopped, or was removed, is interesting
	case ct.EventActionDestroy, ct.EventActionDie, ct.EventActionKill, ct.EventActionOom, ct.EventActionStop, ct.EventActionRestart, ct.EventActionStart, ct.EventActionPrune:
		containerID := em.Actor.ID
		owner, _, found := r.runningContainers.FindByFirstKey(containerID)
		if !found {
			// We are not tracking this container
			return
		}

		err := r.debouncer.ReconciliationNeeded(owner, containerID, r.scheduleContainerReconciliation)
		if err != nil {
			r.Log.Error(err, "could not schedule reconcilation for Container object")
		}
	}
}

func (r *ContainerReconciler) scheduleContainerReconciliation(rti reconcileTriggerInput[string]) error {
	event := ctrl_event.GenericEvent{
		Object: &apiv1.Container{
			ObjectMeta: metav1.ObjectMeta{
				Name:      rti.target.Name,
				Namespace: rti.target.Namespace,
			},
		},
	}

	select {
	case r.notifyContainerChanged <- event:
		return nil // Reconciliation scheduled successfully

	default:
		err := fmt.Errorf("could not schedule reconciliation for Container whose state has changed")
		r.Log.Error(err, "Container", rti.target.Name, "ContainerID", rti.input)
		return err
	}
}

func (r *ContainerReconciler) cancelContainerWatch() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.containerEvtWorkerStop != nil {
		close(r.containerEvtWorkerStop)
		r.containerEvtWorkerStop = nil
	}
	if r.containerEvtSub != nil {
		_ = r.containerEvtSub.Cancel()
		r.containerEvtSub = nil
	}
}

func (r *ContainerReconciler) onShutdown() {
	<-r.lifetimeCtx.Done()
	r.cancelContainerWatch()
}
