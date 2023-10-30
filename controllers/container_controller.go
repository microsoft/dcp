// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

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
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

const (
	containerEventChanInitialCapacity = 20
	MaxParallelContainerStarts        = 6
)

var (
	containerFinalizer string = fmt.Sprintf("%s/container-reconciler", apiv1.GroupVersion.Group)
)

// Data that we keep, in memory, about running containers.
type runningContainerData struct {
	// This is the container startup error if container start fails.
	startupError error

	// If the container starts successfully, this is the container ID from the container orchestrator.
	newContainerID string

	// The time the start attempt finished (successfully or not).
	startAttemptFinishedAt metav1.Time
}

type ContainerReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	orchestrator        ct.ContainerOrchestrator

	// Channel uset to trigger reconciliation when underlying containers change
	notifyContainerChanged *chanx.UnboundedChan[ctrl_event.GenericEvent]

	// A map that stores information about running containers,
	// searchable by container ID (first key), or Container object name (second key).
	// Usually both keys are valid, but when the container is starting, we do not have the real container ID yet,
	// so we use a temporary random string that is replaced by real container ID once we know the container outcome.
	runningContainers *maps.SynchronizedDualKeyMap[string, types.NamespacedName, runningContainerData]

	// A WorkerQueue used for starting containers, which is a long-running operation that we do in parallel,
	// with limited concurrency.
	startupQueue *resiliency.WorkQueue

	// Container events subscription
	containerEvtSub ct.EventSubscription
	// Channel to receive container change events
	containerEvtCh         *chanx.UnboundedChan[ct.EventMessage]
	containerEvtWorkerStop chan struct{}

	// Debouncer used to schedule reconciliation. Extra data is the running container ID whose state changed.
	debouncer *reconcilerDebouncer[string]

	// Lock to protect the reconciler data that requires synchronized access
	lock *sync.Mutex

	// Reconciler lifetime context, used to cancel container watch during reconciler shutdown
	lifetimeCtx context.Context
}

func NewContainerReconciler(lifetimeCtx context.Context, client ctrl_client.Client, log logr.Logger, orchestrator ct.ContainerOrchestrator) *ContainerReconciler {
	r := ContainerReconciler{
		Client:                 client,
		orchestrator:           orchestrator,
		notifyContainerChanged: chanx.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx, 1),
		runningContainers:      maps.NewSynchronizedDualKeyMap[string, types.NamespacedName, runningContainerData](),
		startupQueue:           resiliency.NewWorkQueue(lifetimeCtx, MaxParallelContainerStarts),
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
		Source: r.notifyContainerChanged.Out,
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Container{}).
		Owns(&apiv1.Endpoint{}).
		WatchesRawSource(&src, &ctrl_handler.EnqueueRequestForObject{}).
		Complete(r)
}

func (r *ContainerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("ContainerName", req.NamespacedName).WithValues("Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1))

	r.debouncer.OnReconcile(req.NamespacedName)

	select {
	case _, isOpen := <-ctx.Done():
		if !isOpen {
			log.V(1).Info("Request context expired, nothing to do...")
			return ctrl.Result{}, nil
		}
	default: // not done, proceed
	}

	container := apiv1.Container{}
	err := r.Get(ctx, req.NamespacedName, &container)

	if err != nil {
		if errors.IsNotFound(err) {
			log.V(1).Info("the Container object was deleted")
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the Container object")
			return ctrl.Result{}, err
		}
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(container.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	deletionRequested := container.DeletionTimestamp != nil && !container.DeletionTimestamp.IsZero()
	if deletionRequested && container.Status.State != apiv1.ContainerStateStarting {
		// Note: if the Container object is being deleted, but the correspoinding container is in the process of starting,
		// we need the container startup to finish, before attemptin to delete everything.
		// Otherwise we will be left with a dangling container that no one owns.
		log.Info("Container object is being deleted...")
		r.deleteContainer(ctx, &container, log)
		change = deleteFinalizer(&container, containerFinalizer)
		removeEndpointsForWorkload(r, ctx, &container, log)
	} else {
		change = ensureFinalizer(&container, containerFinalizer)

		// If we added a finalizer, we'll do the additional reconciliation next call
		if change == noChange {
			change = r.manageContainer(ctx, &container, log)

			switch container.Status.State {
			case apiv1.ContainerStateRunning:
				ensureEndpointsForWorkload(ctx, r, &container, nil, log)
			case apiv1.ContainerStatePending, apiv1.ContainerStateStarting:
				break // do nothing
			default:
				removeEndpointsForWorkload(r, ctx, &container, log)
			}
		}
	}

	result, err := saveChanges(r, ctx, &container, patch, change, log)
	return result, err
}

func (r *ContainerReconciler) deleteContainer(ctx context.Context, container *apiv1.Container, log logr.Logger) {
	// r.runningContainers should have the latest data
	containerID, _, found := r.runningContainers.FindBySecondKey(container.NamespacedName())
	if !found {
		containerID = container.Status.ContainerID
		log.V(1).Info("running container data missing, using container ID from Container object status", "ContainerID", containerID)
	}

	// Since the container is being removed, we want to remove it from runningContainers map now
	if found {
		r.runningContainers.DeleteBySecondKey(container.NamespacedName())
	}

	if containerID == "" {
		// This can happen if the container was never started -- nothing to do
		log.V(1).Info("running container ID is not available; proceeding with Container object deletion...")
		return
	}

	log.V(1).Info("calling container orchestrator to remove the container...", "ContainerID", containerID)
	_, err := r.orchestrator.RemoveContainers(ctx, []string{containerID}, true /*force*/)
	if err != nil {
		log.Error(err, "could not remove the running container corresponding to Container object", "ContainerID", containerID)
	}
}

func (r *ContainerReconciler) manageContainer(ctx context.Context, container *apiv1.Container, log logr.Logger) objectChange {
	containerID, rcd, found := r.runningContainers.FindBySecondKey(container.NamespacedName())

	switch {

	case container.Status.State == "" || container.Status.State == apiv1.ContainerStatePending:

		// Check if we haven't already started this (sometimes we might get stale data from the object cache).
		if found {
			// Just wait a bit for the cache to catch up and reconcile again
			return additionalReconciliationNeeded
		} else {
			// This is brand new Container and we need to start it.
			r.ensureContainerWatch(log)

			return r.startContainer(ctx, container, log)
		}

	case container.Status.State == apiv1.ContainerStateStarting:

		if !found {
			// We are still waiting for the container to start.
			// Whatever triggered reconciliation, it was not a container event and does not matter.
			return noChange
		}

		if rcd.startupError == nil {
			log.Info("container has started successfully", "ContainerID", rcd.newContainerID)
			container.Status.ContainerID = rcd.newContainerID
			container.Status.State = apiv1.ContainerStateRunning
			container.Status.StartupTimestamp = rcd.startAttemptFinishedAt
		} else {
			log.Error(rcd.startupError, "container has failed to start")
			container.Status.ContainerID = ""
			container.Status.State = apiv1.ContainerStateFailedToStart
			container.Status.Message = fmt.Sprintf("Container could not be started: %s", rcd.startupError.Error())
			container.Status.FinishTimestamp = rcd.startAttemptFinishedAt
		}

		return statusChanged

	case !found:

		// This should never really happen--we should be tracking this container via our runningContainers map
		// from the startup attempt till it is deleted. Not much we can do at this point, let's mark it as finished-unknown state
		log.Error(fmt.Errorf("missing running container data"), "", "ContainerID", container.Status.ContainerID, "ContainerState", container.Status.State)
		container.Status.State = apiv1.ContainerStateUnknown
		container.Status.FinishTimestamp = metav1.Now()
		return statusChanged

	case container.Status.State != apiv1.ContainerStateFailedToStart:

		// The fact that something triggered reconciliation means we need to take a closer look at this container.
		log.V(1).Info("inspecting running container...", "ContainerID", containerID)
		res, err := r.orchestrator.InspectContainers(ctx, []string{containerID})
		if err != nil || len(res) == 0 {
			// The container was probably removed
			log.Info("running container was not found, marking Container object as finished", "ContainerID", containerID)
			container.Status.State = apiv1.ContainerStateRemoved
			container.Status.FinishTimestamp = metav1.Now()
			return statusChanged
		} else {
			inspected := res[0]
			return r.updateContainerStatus(container, &inspected)
		}

	default:
		// Container failed to  start and is marked as such, so nothing more to do.
		return noChange

	}
}

func (r *ContainerReconciler) startContainer(ctx context.Context, container *apiv1.Container, log logr.Logger) objectChange {

	container.Status.ExitCode = apiv1.UnknownExitCode
	log.Info("scheduling container start", "image", container.Spec.Image)

	err := r.startupQueue.Enqueue(func(_ context.Context) {
		log.Info("starting container", "image", container.Spec.Image)
		opts := ct.RunContainerOptions{ContainerSpec: container.Spec}

		var rcd runningContainerData
		containerID, startupErr := r.orchestrator.RunContainer(ctx, opts)

		rcd.startAttemptFinishedAt = metav1.Now()
		if startupErr != nil {
			log.Error(startupErr, "could not start the container")
			rcd.startupError = startupErr

			// For the sake of storing the runningContainerData in the runningContainers map we need
			// to have a unique container ID, so we generate a fake one here.
			// Since the container startup failed, it won't be used for any real work.
			containerID = fmt.Sprintf("failed-to-start-%s", container.NamespacedName().String())
		} else {
			log.Info("container started", "ContainerID", containerID)
			rcd.newContainerID = containerID
		}
		r.runningContainers.Store(containerID, container.NamespacedName(), rcd)

		err := r.debouncer.ReconciliationNeeded(container.NamespacedName(), containerID, r.scheduleContainerReconciliation)
		if err != nil {
			r.Log.Error(err, "could not schedule reconcilation for Container object", "ContainerID", containerID)
		}
	})

	if err != nil {
		log.Error(err, "could not enqueue container start operation")
		container.Status.State = apiv1.ContainerStateFailedToStart
		container.Status.Message = fmt.Sprintf("Container could not be started: could not enqueue start operation: %s", err.Error())
		container.Status.FinishTimestamp = metav1.Now()
	} else {
		container.Status.State = apiv1.ContainerStateStarting
	}

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
		if status.ExitCode == apiv1.UnknownExitCode {
			status.ExitCode = new(int32)
		}
		*status.ExitCode = inspected.ExitCode
		if !inspected.FinishedAt.IsZero() {
			status.FinishTimestamp = metav1.NewTime(inspected.FinishedAt)
		} else {
			status.FinishTimestamp = metav1.Now()
		}
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

	log.V(1).Info("subscribing to container events...")
	sub, err := r.orchestrator.WatchContainers(r.containerEvtCh.In)
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
	r.notifyContainerChanged.In <- event
	return nil
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

func (r *ContainerReconciler) createEndpoint(
	ctx context.Context,
	owner ctrl_client.Object,
	serviceProducer ServiceProducer,
	log logr.Logger,
) (*apiv1.Endpoint, error) {
	endpointName, err := MakeUniqueName(owner.GetName())
	if err != nil {
		log.Error(err, "could not generate unique name for Endpoint object")
		return nil, err
	}

	if serviceProducer.Address != "" {
		err = fmt.Errorf("address cannot be specified for Container objects")
		log.Error(err, serviceProducerIsInvalid)
		return nil, err
	}

	hostAddress, hostPort, err := r.getHostAddressAndPortForContainerPort(ctx, owner.(*apiv1.Container), serviceProducer.Port, log)
	if err != nil {
		log.Error(err, "could not determine host address and port for container port")
		return nil, err
	}

	if hostAddress == "" || hostAddress == "0.0.0.0" {
		hostAddress = "127.0.0.1"
	}

	// Otherwise, create a new Endpoint object.
	endpoint := &apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      endpointName,
			Namespace: owner.GetNamespace(),
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: owner.GetNamespace(),
			ServiceName:      serviceProducer.ServiceName,
			Address:          hostAddress,
			Port:             hostPort,
		},
	}

	return endpoint, nil
}

func (r *ContainerReconciler) getHostAddressAndPortForContainerPort(
	ctx context.Context,
	ctr *apiv1.Container,
	serviceProducerPort int32,
	log logr.Logger,
) (string, int32, error) {
	matchedPorts := slices.Select(ctr.Spec.Ports, func(p apiv1.ContainerPort) bool {
		return p.ContainerPort == serviceProducerPort || p.HostPort == serviceProducerPort
	})

	if len(matchedPorts) > 0 {
		matchedPort := matchedPorts[0]

		if matchedPort.HostPort != 0 {
			// If the spec contains a port matching the desired container port, just use that
			log.V(1).Info("found matching port in Container spec", "ServiceProducerPort", serviceProducerPort, "HostPort", matchedPort.HostPort)
			return matchedPort.HostIP, matchedPort.HostPort, nil
		}
	}

	// Otherwise, need to inspect the container
	log.V(1).Info("inspecting running container to get its port information...", "ContainerID", ctr.Status.ContainerID)
	ci, err := r.orchestrator.InspectContainers(ctx, []string{ctr.Status.ContainerID})
	if err != nil {
		return "", 0, err
	}

	if len(ci) == 0 {
		return "", 0, fmt.Errorf("could not find container with ID %s", ctr.Status.ContainerID)
	}

	inspected := ci[0]

	var matchedHostPort ct.InspectedContainerHostPortConfig
	for k, v := range inspected.Ports {
		ctrPort := strings.Split(k, "/")[0]

		if ctrPort == fmt.Sprintf("%d", serviceProducerPort) {
			matchedHostPort = v[0]
			break
		}
	}

	hostPort, err := strconv.ParseInt(matchedHostPort.HostPort, 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("could not parse host port '%s' as integer", matchedHostPort.HostPort)
	} else if hostPort == 0 {
		return "", 0, fmt.Errorf("could not find host port for container port %d", serviceProducerPort)
	}

	log.V(1).Info("matched service producer port to one of the container host ports", "ServiceProducerPort", serviceProducerPort, "HostPort", hostPort, "HostIP", matchedHostPort.HostIp)
	return matchedHostPort.HostIp, int32(hostPort), nil
}
