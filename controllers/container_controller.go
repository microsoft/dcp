// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/smallnest/chanx"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	ctrl_handler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	ct "github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

const (
	containerEventChanInitialCapacity = 20
	MaxParallelContainerStarts        = 6
	startupRetryDelay                 = 1 * time.Second
	noDelay                           = 0 * time.Second
	stopContainerTimeoutSeconds       = 10                          // How long the container orchestrator will wait for a container to stop before killing it
	ownerKey                          = ".metadata.controllerOwner" // client index key for child ContainerNetworkConnections
)

var (
	containerFinalizer string = fmt.Sprintf("%s/container-reconciler", apiv1.GroupVersion.Group)
)

type containerNetworkConnectionKey struct {
	Container types.NamespacedName
	Network   types.NamespacedName
}

type containerStateInitializerFunc = stateInitializerFunc[
	apiv1.Container, *apiv1.Container,
	ContainerReconciler, *ContainerReconciler,
	apiv1.ContainerState,
]

var containerStateInitializers = map[apiv1.ContainerState]containerStateInitializerFunc{
	apiv1.ContainerStateEmpty:         handleNewContainer,
	apiv1.ContainerStatePending:       handleNewContainer,
	apiv1.ContainerStateBuilding:      ensureBuildingState,
	apiv1.ContainerStateStarting:      ensureStartingState,
	apiv1.ContainerStateFailedToStart: ensureFailedToStartState,
	apiv1.ContainerStateRunning:       updateContainerData,
	apiv1.ContainerStatePaused:        updateContainerData,
	apiv1.ContainerStateExited:        ensureExitedState,
	apiv1.ContainerStateUnknown:       ensureUnknownState,
	apiv1.ContainerStateStopping:      ensureStoppingState,
}

type ContainerReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	orchestrator        ct.ContainerOrchestrator

	// Channel used to trigger reconciliation when underlying containers change
	notifyContainerChanged *chanx.UnboundedChan[ctrl_event.GenericEvent]

	// A map that stores information about containers (the real things run by the container orchestrator).
	// It is searchable by Container object name (first key) or container ID (second key).
	// Usually both keys are valid, but when the container is starting, we do not have the real container ID yet,
	// so we use a "placeholder" random string that is replaced by real container ID once we know the container outcome.
	runningContainers *maps.SynchronizedDualKeyMap[types.NamespacedName, string, *runningContainerData]

	networkConnections syncmap.Map[containerNetworkConnectionKey, bool]

	// A WorkerQueue used for starting containers, which is a long-running operation that we do in parallel,
	// with limited concurrency.
	startupQueue *resiliency.WorkQueue

	// Container events subscription
	containerEvtSub *ct.EventSubscription
	// Network events subscription
	networkEvtSub *ct.EventSubscription
	// Channel to receive container change events
	containerEvtCh *chanx.UnboundedChan[ct.EventMessage]
	// Channel to receive network change events
	networkEvtCh *chanx.UnboundedChan[ct.EventMessage]
	// Channel to stop the event worker
	containerEvtWorkerStop chan struct{}

	// Debouncer used to schedule reconciliation. Extra data is the running container ID whose state changed.
	debouncer *reconcilerDebouncer[string]

	// Count of existing Container resources
	watchingResources syncmap.Map[types.UID, bool]

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
		runningContainers:      maps.NewSynchronizedDualKeyMap[types.NamespacedName, string, *runningContainerData](),
		networkConnections:     syncmap.Map[containerNetworkConnectionKey, bool]{},
		startupQueue:           resiliency.NewWorkQueue(lifetimeCtx, MaxParallelContainerStarts),
		containerEvtSub:        nil,
		networkEvtSub:          nil,
		containerEvtCh:         nil,
		networkEvtCh:           nil,
		containerEvtWorkerStop: nil,
		debouncer:              newReconcilerDebouncer[string](reconciliationDebounceDelay),
		watchingResources:      syncmap.Map[types.UID, bool]{},
		lock:                   &sync.Mutex{},
		lifetimeCtx:            lifetimeCtx,
		Log:                    log,
	}

	go r.onShutdown()

	return &r
}

func (r *ContainerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Setup a client side index to allow quickly finding all ContainerNetworkConnections owned by a Continer.
	// Behind the scenes this is using listers and informers to keep an index on an internal cache owned by
	// the Manager up to date.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.ContainerNetworkConnection{}, ownerKey, func(rawObj ctrl_client.Object) []string {
		cnc := rawObj.(*apiv1.ContainerNetworkConnection)
		owner := metav1.GetControllerOf(cnc)

		if owner == nil {
			return nil
		}

		// Ignore any ContainerNetworkConnections that aren't owned by a Container
		if owner.APIVersion != apiv1.GroupVersion.String() || owner.Kind != "Container" {
			return nil
		}

		return []string{owner.Name}
	}); err != nil {
		r.Log.Error(err, "failed to create index for ContainerNetworkConnection", "indexField", ownerKey)
		return err
	}

	src := ctrl_source.Channel(r.notifyContainerChanged.Out, &ctrl_handler.EnqueueRequestForObject{})
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Container{}).
		Owns(&apiv1.Endpoint{}).
		Owns(&apiv1.ContainerNetworkConnection{}).
		WatchesRawSource(src).
		Complete(r)
}

func (r *ContainerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("Container", req.NamespacedName).WithValues("Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1))

	r.debouncer.OnReconcile(req.NamespacedName)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	container := apiv1.Container{}
	err := r.Get(ctx, req.NamespacedName, &container)

	if err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("the Container object was not found")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the Container object")
			getFailedCounter.Add(ctx, 1)
			return ctrl.Result{}, err
		}
	} else {
		getSucceededCounter.Add(ctx, 1)
	}

	r.runningContainers.RunDeferredOps(req.NamespacedName)

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(container.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	// Check for deletion first; it trumps all other types of state changes.
	if container.DeletionTimestamp != nil && !container.DeletionTimestamp.IsZero() && r.canBeDeleted(&container) {
		change = r.handleDeletionRequest(ctx, &container, log)
	} else if change = ensureFinalizer(&container, containerFinalizer, log); change != noChange {
		// If we need to put the finalizer on the Container object, we'll do any additional changes during next reconciliation.
	} else {
		change = r.manageContainer(ctx, &container, log)
	}

	result, err := saveChanges(r, ctx, &container, patch, change, nil, log)
	return result, err
}

func (r *ContainerReconciler) manageContainer(ctx context.Context, container *apiv1.Container, log logr.Logger) objectChange {
	currentContainerState := container.Status.State
	_, rcd, found := r.runningContainers.FindByFirstKey(container.NamespacedName())
	if found {
		// In-memory container state is not subject to issues related to caching and
		// status updates failed due to conflict, so it is fresher and has precedence.
		currentContainerState = rcd.containerState
	}

	newContainerState := currentContainerState

	// Even if the new container state is (as it is usually the case) the same as the current state,
	// we still want to run the state handler to ensure that the Container object Status,
	// and the real-world resources associated with the Container object, are up to date.
	initalizer := getStateInitializer(containerStateInitializers, newContainerState, log)
	change := initalizer(ctx, r, container, newContainerState, log)
	return change
}

// STATE INITIALIZER FUNCIONS

func (r *ContainerReconciler) canBeDeleted(container *apiv1.Container) bool {
	// Note: if the Container object is being deleted, but the correspoinding container is in the process of starting or stopping,
	// we need the container startup/shutdown to finish, before attemptin to delete the Container object.
	// Otherwise we will be left with a dangling container that no one owns.
	//
	// Also if the container is running or paused, we need to stop it first.
	// (the latter is handled by the Running/Paused state initializer.)

	_, rcd, found := r.runningContainers.FindByFirstKey(container.NamespacedName())

	retval := !found ||
		(rcd.containerState == apiv1.ContainerStateStarting && !rcd.startupAttempted) || // Container finished building but isn't starting yet
		(rcd.containerState != apiv1.ContainerStateBuilding &&
			rcd.containerState != apiv1.ContainerStateStarting &&
			rcd.containerState != apiv1.ContainerStateStopping &&
			rcd.containerState != apiv1.ContainerStateRunning &&
			rcd.containerState != apiv1.ContainerStatePaused)

	return retval
}

func (r *ContainerReconciler) handleDeletionRequest(ctx context.Context, container *apiv1.Container, log logr.Logger) objectChange {
	log.V(1).Info("Container object is being deleted")
	r.cleanupContainerResources(ctx, container, log)

	// Note that we are not going to make any other changes to the Container object.
	// It is being deleted, and any changes not only will be lost,
	// but may trigger addtional reconciliations that are not needed.

	change := deleteFinalizer(container, containerFinalizer, log)
	return change
}

func handleNewContainer(
	_ context.Context,
	_ *ContainerReconciler,
	container *apiv1.Container,
	_ apiv1.ContainerState,
	_ logr.Logger,
) objectChange {
	if container.Spec.Stop {
		// The container was started with a desired state of stopped, don't attempt to start it.
		container.Status.State = apiv1.ContainerStateFailedToStart
	} else if container.Spec.Build != nil {
		// Container has a build context, so need to build it first.
		container.Status.State = apiv1.ContainerStateBuilding
	} else {
		// Initiate startup sequence.
		container.Status.State = apiv1.ContainerStateStarting
	}
	return statusChanged
}

func ensureBuildingState(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	_ apiv1.ContainerState,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, apiv1.ContainerStateBuilding)

	_, rcd, found := r.runningContainers.FindByFirstKey(container.NamespacedName())

	if !found {
		// This is a brand new Container and we need to build it.

		rcd = newRunningContainerData(container)
		rcd.containerState = apiv1.ContainerStateBuilding
		rcd.ensureStartupOutputFiles(container, log)
		r.runningContainers.Store(container.NamespacedName(), rcd.containerID, rcd)
		r.ensureContainerWatch(container, log)

		r.buildImage(ctx, container, rcd, log)
		change |= statusChanged
	}

	// The attempt to build the container is generally asynchronous, but it may fail or succeed immediately.
	// The former could be due to some non-trainsient error from the container orchestrator.
	// The latter could be because we are dealing with a persistent Container and we found a matching, existing container resource.
	// Either way, even if we are just waiting for the container to build, the Status of the Container object may be stale.
	// It might be just a caching issue, but it also might be because of a write conflict during last update,
	// so we need to make sure it is what it should be.
	// Bottom line we always want to apply the changes to the Container object.
	change |= rcd.applyTo(container)

	if rcd.containerState != apiv1.ContainerStateBuilding {
		// Transitioning to a different state, build attempt(s) are done.
		rcd.closeStartupLogFiles(log)
	}

	return change
}

func ensureStartingState(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	_ apiv1.ContainerState,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, apiv1.ContainerStateStarting)

	_, rcd, found := r.runningContainers.FindByFirstKey(container.NamespacedName())

	if !found {
		// This is brand new Container and we need to start it.

		rcd = newRunningContainerData(container)
		rcd.containerState = apiv1.ContainerStateStarting
		rcd.ensureStartupOutputFiles(container, log)
		r.runningContainers.Store(container.NamespacedName(), rcd.containerID, rcd)
		r.ensureContainerWatch(container, log)

		r.createContainer(ctx, container, rcd, log, noDelay)
		change |= statusChanged

	} else if !rcd.startupAttempted {
		// We haven't attempted to start the Container yet (likely we're here after build completed).

		rcd.ensureStartupOutputFiles(container, log)

		r.createContainer(ctx, container, rcd, log, noDelay)
		change |= statusChanged

	} else if isTransientTemplateError(rcd.startupError) {
		// Retry startup after transient error.

		rcd.startupError = nil
		rcd.startAttemptFinishedAt = metav1.Time{}
		rcd.containerName = ""

		r.createContainer(ctx, container, rcd, log, startupRetryDelay)
		change |= statusChanged

	} else if container.Spec.Networks != nil && rcd.hasValidContainerID() {
		// The second portion of startup sequence of a container with custom networks.
		// Need to create ContainerNetworkConnection objects and start the container resource.

		started, err := r.handleInitialNetworkConnections(ctx, container, rcd.containerID, log)
		switch {
		case err != nil:
			rcd.startupError = err
			rcd.containerState = apiv1.ContainerStateFailedToStart
			rcd.startAttemptFinishedAt = metav1.Now()
			change |= statusChanged
		case started:
			rcd.containerState = apiv1.ContainerStateRunning
			rcd.startAttemptFinishedAt = metav1.Now()
			change |= statusChanged
		default:
			// We are waiting for the network connections to be established.
			change |= additionalReconciliationNeeded
		}
	}

	// The attempt to start the container is generally asynchronous, but it may fail or succeed immediately.
	// The former could be due to some non-trainsient error from the container orchestrator.
	// The latter could be because we are dealing with a persistent Container and we found a matching, existing container resource.
	// Either way, even if we are just waiting for the container to start, the Status of the Container object may be stale.
	// It might be just a caching issue, but it also might be because of a write conflict during last update,
	// so we need to make sure it is what it should be.
	// Bottom line we always want to apply the changes to the Container object.
	change |= rcd.applyTo(container)

	if rcd.containerState != apiv1.ContainerStateStarting {
		// Transitioning to a different state, startup attempt(s) are done.
		rcd.closeStartupLogFiles(log)
	}

	return change
}

func ensureFailedToStartState(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	_ apiv1.ContainerState,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, apiv1.ContainerStateFailedToStart)

	_, rcd, found := r.runningContainers.FindByFirstKey(container.NamespacedName())
	if !found {
		// Can happen if the container was created with Spec.Stop = true
		if container.Status.FinishTimestamp.IsZero() {
			container.Status.FinishTimestamp = metav1.Now()
			change |= statusChanged
		}
	} else {
		change |= rcd.applyTo(container)
	}

	return change
}

func updateContainerData(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	desiredState apiv1.ContainerState,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, desiredState)

	containerID, rcd, found := r.runningContainers.FindByFirstKey(container.NamespacedName())
	if !found {
		// Should never happen--the runningContaienrs map should have the data about the Container object.
		log.Error(fmt.Errorf("the data about the container resource is missing"), "")
		return ensureUnknownState(ctx, r, container, desiredState, log)
	}

	if container.Spec.Stop || (container.DeletionTimestamp != nil && !container.DeletionTimestamp.IsZero()) {
		// Start the stopping sequence
		rcd.containerState = apiv1.ContainerStateStopping
		change |= setContainerState(container, apiv1.ContainerStateStopping)
		return change
	}

	if desiredState != apiv1.ContainerStateRunning {
		removeEndpointsForWorkload(r, ctx, container, log)
	} else {
		ensureEndpointsForWorkload(ctx, r, container, rcd.reservedPorts, log)
	}

	log.V(1).Info("inspecting container resource...", "ContainerID", containerID)
	inspected, err := r.findContainer(ctx, containerID)
	if err != nil {
		log.Info("container resource could not be inspected, might have been removed... ",
			"ContainerID", containerID,
			"Error", err.Error(),
		)
		// Could be a transient error, so for know we keep the rest of the status as-is.
		return change
	}

	rcd.updateFromInspectedContainer(inspected)
	change |= rcd.applyTo(container)

	if container.Spec.Networks != nil {
		connectedNetworks, networkRelatedChange := r.handleRunningContainerNetworkConnections(ctx, container, inspected, log)
		change |= networkRelatedChange
		if len(connectedNetworks) > 0 {
			container.Status.Networks = connectedNetworks
		}
	}

	return change
}

func ensureExitedState(ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	desiredState apiv1.ContainerState,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, apiv1.ContainerStateExited)

	containerID, rcd, found := r.runningContainers.FindByFirstKey(container.NamespacedName())
	if !found {
		// Should never happen--the runningContaienrs map should have the data about the Container object.
		log.Error(fmt.Errorf("the data about the container resource is missing"), "")
		return ensureUnknownState(ctx, r, container, desiredState, log)
	}

	removeEndpointsForWorkload(r, ctx, container, log)

	if len(container.Status.Networks) > 0 {
		container.Status.Networks = nil
		change |= statusChanged
	}

	log.V(1).Info("inspecting container resource...", "ContainerID", containerID)
	inspected, err := r.findContainer(ctx, containerID)
	if err != nil {
		log.Info("container resource could not be inspected, might have been removed... ",
			"ContainerID", containerID,
			"Error", err.Error(),
		)
		return change // Best effort--skipping the rest.
	}

	rcd.updateFromInspectedContainer(inspected)
	change |= rcd.applyTo(container)
	return change
}

func ensureUnknownState(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	desiredState apiv1.ContainerState,
	log logr.Logger,
) objectChange {
	log.Error(fmt.Errorf("the state of the Container became undetermined"), "", "RequestedContainerState", desiredState)

	change := setContainerState(container, apiv1.ContainerStateUnknown)
	if container.Status.FinishTimestamp.IsZero() {
		container.Status.FinishTimestamp = metav1.Now()
		change |= statusChanged
	}

	if len(container.Status.Networks) > 0 {
		container.Status.Networks = nil
		change |= statusChanged
	}

	// We do not really know what has happened to the container, so we clean up DCP-managed resources here,
	// and do best-effort clean up of orchestrator-managed resources when the Container object is deleted,
	// or when DCP is shutting down.
	r.cleanupDcpContainerResources(ctx, container, log)

	return change
}

func ensureStoppingState(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	newState apiv1.ContainerState,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, apiv1.ContainerStateStopping)

	_, rcd, found := r.runningContainers.FindByFirstKey(container.NamespacedName())
	if !found {
		// Should never happen--the runningContaienrs map should have the data about the Container object.
		log.Error(fmt.Errorf("the data about the container resource is missing"), "")
		return ensureUnknownState(ctx, r, container, apiv1.ContainerStateStopping, log)
	}

	removeEndpointsForWorkload(r, ctx, container, log)

	if rcd.stopAttemptInitiated {
		// We just need to wait for the container to stop. Once that is done, we will transition to Exited state.
		change |= rcd.applyTo(container)
	} else {
		rcd.stopAttemptInitiated = true
		err := r.startupQueue.Enqueue(r.stopContainer(container, rcd, log))
		if err != nil {
			log.Error(err, "could not stop the container")
			change |= ensureUnknownState(ctx, r, container, apiv1.ContainerStateStopping, log)
		}
	}

	return change
}

// CONTAINER STARTUP HELPER METHODS

func (r *ContainerReconciler) checkForExistingPersistentContainer(
	ctx context.Context,
	container *apiv1.Container,
	rcd *runningContainerData,
	containerName string,
	log logr.Logger,
) bool {
	if container.Spec.Persistent {
		inspected, err := r.findContainer(ctx, containerName)
		if err == nil {
			log.Info("found existing Container", "ContainerName", containerName, "ContainerID", inspected.Id)
			placeholderID := rcd.containerID
			rcd.updateFromInspectedContainer(inspected)
			rcd.startAttemptFinishedAt = metav1.Now()
			rcd.containerState = apiv1.ContainerStateRunning
			r.runningContainers.UpdateChangingSecondKey(container.NamespacedName(), placeholderID, rcd.containerID, rcd)
			return true
		}
	}

	return false
}

// Schedules build of a container image. If Container is persistent, it will attempt to find and reuse an existing container.
func (r *ContainerReconciler) buildImage(
	ctx context.Context,
	container *apiv1.Container,
	rcd *runningContainerData,
	log logr.Logger,
) {
	containerName := strings.TrimSpace(container.Spec.ContainerName)

	// Determine whether we need to check for an existing container
	if r.checkForExistingPersistentContainer(ctx, container, rcd, containerName, log) {
		return
	}

	log.V(1).Info("scheduling image build")

	err := r.startupQueue.Enqueue(r.buildImageWithOrchestrator(container, rcd, log))
	if err != nil {
		log.Error(err, "image was not built, possibly because the workload is shutting down")
		rcd.containerState = apiv1.ContainerStateFailedToStart
		rcd.startupError = err
		rcd.startAttemptFinishedAt = metav1.Now()
	}
}

// Schedules creation of a container resource. If Container is persistent, it will attempt to find and reuse an existing container.
func (r *ContainerReconciler) createContainer(
	ctx context.Context,
	container *apiv1.Container,
	rcd *runningContainerData,
	log logr.Logger,
	delay time.Duration,
) {
	containerName := strings.TrimSpace(container.Spec.ContainerName)

	rcd.startupAttempted = true

	// Determine whether we need to check for an existing container
	if r.checkForExistingPersistentContainer(ctx, container, rcd, containerName, log) {
		return
	}

	for _, volume := range container.Spec.VolumeMounts {
		if volume.Type == apiv1.BindMount {
			_, err := os.Stat(volume.Source)
			if errors.Is(err, os.ErrNotExist) {
				err = os.MkdirAll(volume.Source, osutil.PermissionDirectoryOthersRead)
				if err != nil {
					log.Error(err, "could not create bind mount source path", "Source", volume.Source, "Target", volume.Target)
					rcd.containerState = apiv1.ContainerStateFailedToStart
					rcd.startupError = err
					rcd.startAttemptFinishedAt = metav1.Now()
					return
				}
			} else if err != nil {
				log.Error(err, "could not verify existence of bind mount source path", "Volume", volume.Source, "Target", volume.Target)
				rcd.containerState = apiv1.ContainerStateFailedToStart
				rcd.startupError = err
				rcd.startAttemptFinishedAt = metav1.Now()
				return
			}
		}
	}

	log.V(1).Info("scheduling container start", "image", container.SpecifiedImageNameOrDefault())

	if containerName == "" {
		uniqueContainerName, err := MakeUniqueName(container.Name)
		if err != nil {
			log.Error(err, "could not generate a unique container name")
			rcd.containerState = apiv1.ContainerStateFailedToStart
			rcd.startupError = err
			rcd.startAttemptFinishedAt = metav1.Now()
			return
		}

		containerName = uniqueContainerName
	}

	err := r.startupQueue.Enqueue(r.startContainerWithOrchestrator(container, rcd, containerName, log, delay))
	if err != nil {
		log.Error(err, "container was not started, probably because the workload is shutting down")
		rcd.containerState = apiv1.ContainerStateFailedToStart
		rcd.startupError = err
		rcd.startAttemptFinishedAt = metav1.Now()
	}
}

func (r *ContainerReconciler) buildImageWithOrchestrator(container *apiv1.Container, originalRCD *runningContainerData, log logr.Logger) func(context.Context) {
	return func(buildCtx context.Context) {
		rcd := originalRCD.clone()

		err := func() error {
			log.V(1).Info("building image", "dockerfile", container.Spec.Build.Dockerfile, "context", container.Spec.Build.Context)

			tags := []string{container.SpecifiedImageNameOrDefault()}

			buildOptions := ct.BuildImageOptions{
				Tags:                  tags,
				ContainerBuildContext: container.Spec.Build,
				StreamCommandOptions: ct.StreamCommandOptions{
					// Always append timestamp to startup logs; we'll strip them out if the streaming request doesn't ask for them
					StdOutStream: usvc_io.NewTimestampWriter(rcd.startupStdOutFile),
					StdErrStream: usvc_io.NewTimestampWriter(rcd.startupStdErrFile),
				},
			}

			buildErr := r.orchestrator.BuildImage(buildCtx, buildOptions)
			if buildErr != nil {
				log.Error(buildErr, "could not build the image")
				return buildErr
			}

			rcd.containerState = apiv1.ContainerStateStarting

			return nil
		}()

		if err != nil {
			rcd.startupError = err
			rcd.startAttemptFinishedAt = metav1.Now()
			rcd.containerState = apiv1.ContainerStateFailedToStart
		}

		containerObjectName := container.NamespacedName()
		r.runningContainers.QueueDeferredOp(containerObjectName, func(runningContainers *maps.DualKeyMap[types.NamespacedName, string, *runningContainerData]) {
			runningContainers.Update(containerObjectName, rcd.containerID, rcd)
		})
		r.scheduleContainerReconciliation(container.NamespacedName(), rcd.containerID)
	}
}

func (r *ContainerReconciler) startContainerWithOrchestrator(container *apiv1.Container, originalRCD *runningContainerData, containerName string, log logr.Logger, delay time.Duration) func(context.Context) {
	return func(startupCtx context.Context) {
		if delay > 0 {
			time.Sleep(delay)
		}

		rcd := originalRCD.clone()
		placeholderContainerID := rcd.containerID

		err := func() error {
			log.V(1).Info("starting container", "image", container.SpecifiedImageNameOrDefault())

			err := r.computeEffectiveEnvironment(startupCtx, container, rcd, log)
			if err != nil {
				if isTransientTemplateError(err) {
					log.Info("could not compute effective environment for the Container, retrying startup...", "Cause", err.Error())
				} else {
					log.Error(err, "could not compute effective environment for the Container")
				}

				return err
			}

			err = r.computeEffectiveInvocationArgs(startupCtx, container, rcd, log)
			if err != nil {
				if isTransientTemplateError(err) {
					log.Info("could not compute effective invocation arguments for the Container, retrying startup...", "Cause", err.Error())
				} else {
					log.Error(err, "could not compute effective invocation arguments for the Container")
				}

				return err
			}

			defaultNetwork := ""
			if rcd.runSpec.Networks != nil {
				// See comment below why we create the container with default network explicitly enabled here.
				defaultNetwork = r.orchestrator.DefaultNetworkName()
			}

			creationOptions := ct.CreateContainerOptions{
				ContainerSpec: *rcd.runSpec,
				Name:          containerName,
				Network:       defaultNetwork,
				StreamCommandOptions: ct.StreamCommandOptions{
					// Always append timestamp to startup logs; we'll strip them out if the streaming request doesn't ask for them
					StdOutStream: usvc_io.NewTimestampWriter(rcd.startupStdOutFile),
					StdErrStream: usvc_io.NewTimestampWriter(rcd.startupStdErrFile),
				},
			}
			containerID, err := r.orchestrator.CreateContainer(startupCtx, creationOptions)

			// This starting attempt is done; make sure that the output file(s) have a couple of blank lines
			// separating this attempt from the next (best effort).
			if rcd.startupStdOutFile != nil {
				_, _ = rcd.startupStdOutFile.Write(osutil.WithNewline(osutil.WithNewline(nil)))
			}
			if rcd.startupStdErrFile != nil {
				_, _ = rcd.startupStdErrFile.Write(osutil.WithNewline(osutil.WithNewline(nil)))
			}

			// There are errors that can still result in a valid container ID, so we need to store it if one was returned
			rcd.containerID = containerID

			if err != nil {
				log.Error(err, "could not create the container")
				return err
			}
			log.V(1).Info("container created", "ContainerID", containerID)

			inspected, err := r.findContainer(startupCtx, containerID)
			if err != nil {
				log.Error(err, "could not inspect the container")
				return err
			}

			rcd.updateFromInspectedContainer(inspected)

			if rcd.runSpec.Networks == nil {
				_, err = r.orchestrator.StartContainers(startupCtx, []string{containerID})
				if err != nil {
					log.Error(err, "could not start the container", "ContainerID", containerID)
					return err
				}

				log.V(1).Info("container started", "ContainerID", containerID)
				rcd.containerState = apiv1.ContainerStateRunning
			} else {
				// If a container resource is created without a network, it cannot be connected to a network later (orchestrator limitation).
				// So for Containers that request attaching to custom networks via Spec, we create the corresponding ocontainer resource
				// attached to default network(s) (usually one: "bridge" for Docker or "podman" for Podman).
				// Here we detach it from the default network(s). Then we leave the Container object in "starting" state,
				// and save the changes.
				//
				// During next reconciliation loop we create ContainerNetworkConnection objects and start the container resource.
				// The Network controller takes care of connecting the container resournce to requested networks.
				for i := range inspected.Networks {
					network := inspected.Networks[i].Id
					err = r.orchestrator.DisconnectNetwork(startupCtx, ct.DisconnectNetworkOptions{Network: network, Container: containerID, Force: true})
					if err != nil {
						log.Error(err, "could not detach network from the container", "ContainerID", containerID, "Network", network)
						return err
					}
				}
			}

			return nil
		}()

		if err != nil {
			rcd.startupError = err
			rcd.startAttemptFinishedAt = metav1.Now()

			// Keep in "starting" state if the error is a transient error, otherwise initiate the transition to "failed to start".
			if !isTransientTemplateError(err) {
				rcd.containerState = apiv1.ContainerStateFailedToStart
			}
		}

		containerObjectName := container.NamespacedName()
		r.runningContainers.QueueDeferredOp(containerObjectName, func(runningContainers *maps.DualKeyMap[types.NamespacedName, string, *runningContainerData]) {
			runningContainers.UpdateChangingSecondKey(containerObjectName, placeholderContainerID, rcd.containerID, rcd)
		})
		r.scheduleContainerReconciliation(container.NamespacedName(), placeholderContainerID)
	}
}

// CONTAINER STOP/SHUTDOWN HELPER METHODS

func (r *ContainerReconciler) stopContainer(container *apiv1.Container, originalRCD *runningContainerData, log logr.Logger) func(context.Context) {
	return func(stopCtx context.Context) {
		rcd := originalRCD.clone()

		log.V(1).Info("calling container orchestrator to stop the container...",
			"Container", container.NamespacedName().String(),
			"ContainerID", rcd.containerID,
		)
		_, err := r.orchestrator.StopContainers(stopCtx, []string{rcd.containerID}, stopContainerTimeoutSeconds)
		if err != nil {
			log.Error(err, "could not stop the running container corresponding to Container object",
				"Container", container.NamespacedName().String(),
				"ContainerID", rcd.containerID,
			)
			rcd.containerState = apiv1.ContainerStateUnknown
		} else {
			rcd.containerState = apiv1.ContainerStateExited
		}

		containerObjectName := container.NamespacedName()
		r.runningContainers.QueueDeferredOp(containerObjectName, func(runningContainers *maps.DualKeyMap[types.NamespacedName, string, *runningContainerData]) {
			runningContainers.Update(containerObjectName, rcd.containerID, rcd)
		})
		r.scheduleContainerReconciliation(container.NamespacedName(), rcd.containerID)
	}

}

func (r *ContainerReconciler) deleteContainer(ctx context.Context, container *apiv1.Container, log logr.Logger) {
	// This method is called only when we never attempted to start the container,
	// or if the container has already finished starting/stopping and we know the outcome of either.

	containerID, rcd, found := r.runningContainers.FindByFirstKey(container.NamespacedName())
	if !found {
		// We either never started the container, or we already attempted to remove the container
		// and the current reconciliation call is just the cache catching up.
		// Either way there is nothing to do.
		log.V(1).Info("running container data is not available, nothing to remove...")
		return
	}

	rcd.closeStartupLogFiles(log)

	if container.Spec.Persistent {
		log.V(1).Info("Container is not using Managed mode, leaving underlying resources")
		return
	}

	if !rcd.hasValidContainerID() {
		log.V(1).Info("container resource was never created, nothing to remove...")
		return
	}

	// We want to stop the container first to give it a chance to clean up
	log.V(1).Info("calling container orchestrator to stop the container...", "ContainerID", containerID)
	_, err := r.orchestrator.StopContainers(ctx, []string{containerID}, stopContainerTimeoutSeconds)
	if err != nil {
		log.Error(err, "could not stop the running container corresponding to Container object", "ContainerID", containerID)
	}

	log.V(1).Info("calling container orchestrator to remove the container...", "ContainerID", containerID)
	_, err = r.orchestrator.RemoveContainers(ctx, []string{containerID}, true /*force*/)
	if err != nil {
		log.Error(err, "could not remove the running container corresponding to Container object", "ContainerID", containerID)
	}
}

// Removes all resources associated with the Container object, both DCP-managed, as well as orchestrator-managed,
// including the running container.
func (r *ContainerReconciler) cleanupContainerResources(ctx context.Context, container *apiv1.Container, log logr.Logger) {
	r.cleanupDcpContainerResources(ctx, container, log)
	r.removeContainerNetworkConnections(ctx, container, log)
	r.deleteContainer(ctx, container, log)
	r.runningContainers.DeleteByFirstKey(container.NamespacedName())
}

// Removes any resources that DCP is managing for the running container.
// Does not attempt to remove the actual running container, or any orchestrator-managed resource.
func (r *ContainerReconciler) cleanupDcpContainerResources(ctx context.Context, container *apiv1.Container, log logr.Logger) {
	removeEndpointsForWorkload(r, ctx, container, log)
	r.releaseContainerWatch(container, log)
}

// NETWORKING SUPPORT METHODS

// Creates initial set of ContainerNetworkConnection objects for this Container, and if all connections are satisfied,
// starts the container.
// Returns a value indicating if the container has been started, and an error, if any.
// The error should be treated as permanent startup failure.
func (r *ContainerReconciler) handleInitialNetworkConnections(
	ctx context.Context,
	container *apiv1.Container,
	containerID string,
	log logr.Logger,
) (bool, error) {
	connected, err := r.ensureContainerNetworkConnections(ctx, container, nil, containerID, log)
	if err != nil {
		return false, err
	}

	// Check to see if we are connected to all the ContainerNetworks listed in the Container object spec
	if len(connected) != len(*container.Spec.Networks) {
		log.V(1).Info("container not connected to expected number of networks, scheduling additional reconciliation...", "ContainerID", containerID, "Expected", len(*container.Spec.Networks), "Connected", len(connected))
		return false, nil
	}

	if _, err = r.orchestrator.StartContainers(ctx, []string{containerID}); err != nil {
		log.Error(err, "failed to start Container", "ContainerID", containerID)
		return false, err
	}

	return true, nil
}

func (r *ContainerReconciler) handleRunningContainerNetworkConnections(
	ctx context.Context,
	container *apiv1.Container,
	inspected *ct.InspectedContainer,
	log logr.Logger,
) ([]string, objectChange) {
	connected, connectionErr := r.ensureContainerNetworkConnections(ctx, container, inspected, inspected.Id, log)
	if connectionErr != nil {
		// The error was already logged by ensureContainerNetworkConnections()
		log.V(1).Info("an error occurred while managing container network connections, scheduling additional reconciliation...")
		return nil, additionalReconciliationNeeded
	}

	connectedNetworkNames := slices.Map[*apiv1.ContainerNetwork, string](connected, func(n *apiv1.ContainerNetwork) string {
		return n.NamespacedName().String()
	})

	notConnected, newlyConnected := slices.Diff(container.Status.Networks, connectedNetworkNames)
	if len(notConnected) > 0 {
		log.V(1).Info("container became disconnected from some networks, updating status...", "DisonnectedNetworks", notConnected)
	}
	if len(newlyConnected) > 0 {
		log.V(1).Info("container become connected to new networks, updating status...", "ConnectedNetworks", newlyConnected)
	}

	if len(notConnected) > 0 || len(newlyConnected) > 0 {
		return connectedNetworkNames, statusChanged
	} else {
		return connectedNetworkNames, noChange
	}
}

func (r *ContainerReconciler) removeContainerNetworkConnections(
	ctx context.Context,
	container *apiv1.Container,
	log logr.Logger,
) {
	var childNetworkConnections apiv1.ContainerNetworkConnectionList
	if err := r.List(ctx, &childNetworkConnections, ctrl_client.InNamespace(container.GetNamespace()), ctrl_client.MatchingFields{ownerKey: string(container.Name)}); err != nil {
		log.Error(err, "failed to list child ContainerNetworkConnection objects", "Container", container.NamespacedName().String())
		return
	}

	for i := range childNetworkConnections.Items {
		if err := r.Delete(ctx, &childNetworkConnections.Items[i], ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground)); ctrl_client.IgnoreNotFound(err) != nil {
			log.Error(err, "could not delete ContainerNetworkConnection object", "Container", container.NamespacedName().String(), "ContainerNetworkConnection", childNetworkConnections.Items[i].NamespacedName().String())
		}
	}
}

// This method compares the ContainerNetworkConnection objects for a container (indicating the ContainerNetworks a Container
// expects to be connected to) against the networks a container is actually connected to. It returns a list of ContainerNetworks
// the Container is connected to via a ContainerNetworkConnection. In addition, it creates or deletes ContainerNetworkConnection
// entries based on the networks property of the Container.
func (r *ContainerReconciler) ensureContainerNetworkConnections(
	ctx context.Context,
	container *apiv1.Container,
	inspected *ct.InspectedContainer,
	containerID string,
	log logr.Logger,
) ([]*apiv1.ContainerNetwork, error) {
	var childNetworkConnections apiv1.ContainerNetworkConnectionList
	if err := r.List(ctx, &childNetworkConnections, ctrl_client.InNamespace(container.GetNamespace()), ctrl_client.MatchingFields{ownerKey: string(container.Name)}); err != nil {
		log.Error(err, "failed to list child ContainerNetworkConnection objects", "Container", container.NamespacedName().String())
		return []*apiv1.ContainerNetwork{}, err
	}

	if container.Spec.Networks == nil || !container.Status.FinishTimestamp.IsZero() {
		// If no networks are defined or the FinishTimestamp is set for the container, delete all connections
		var err error
		for i := range childNetworkConnections.Items {
			if deleteErr := r.Delete(ctx, &childNetworkConnections.Items[i], ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground)); ctrl_client.IgnoreNotFound(err) != nil {
				err = errors.Join(err, deleteErr)
			}
		}

		if err != nil {
			log.Error(err, "could not delete some child ContainerNetworkConnection objects")
		}

		return []*apiv1.ContainerNetwork{}, nil
	}

	var networks apiv1.ContainerNetworkList
	if err := r.List(ctx, &networks, ctrl_client.InNamespace(container.GetNamespace())); err != nil {
		log.Error(err, "failed to list ContainerNetwork objects")
		return []*apiv1.ContainerNetwork{}, err
	}

	if inspected == nil {
		if containerID == "" {
			err := fmt.Errorf("could not ensure ContainerNetworkConnections because the data about running container is missing")
			log.Error(err, "")
			return []*apiv1.ContainerNetwork{}, err
		}
		if i, err := r.findContainer(ctx, containerID); err != nil {
			log.Error(err, "could not inspect the container", "ContainerID", containerID)
			return []*apiv1.ContainerNetwork{}, err
		} else {
			inspected = i
		}
	}

	validConnectedNetworks := []*apiv1.ContainerNetwork{}
	expectedNetworks := []*apiv1.ContainerNetwork{}

	for i := range networks.Items {
		network := &networks.Items[i]

		index := slices.IndexFunc(childNetworkConnections.Items, func(cnc apiv1.ContainerNetworkConnection) bool {
			return asNamespacedName(cnc.Spec.ContainerNetworkName, container.GetNamespace()) == network.NamespacedName()
		})

		if index >= 0 {
			expectedNetworks = append(expectedNetworks, network)
		}
	}

	// Remove any network connections that don't correspond to an expected ContainerNetwork
	for i := range inspected.Networks {
		existingNetworkConnection := inspected.Networks[i]
		var containerNetwork *apiv1.ContainerNetwork
		found := slices.Any(expectedNetworks, func(network *apiv1.ContainerNetwork) bool {
			if network.Status.NetworkName == existingNetworkConnection.Name {
				containerNetwork = network
				return true
			}

			return false
		})

		if found {
			validConnectedNetworks = append(validConnectedNetworks, containerNetwork)
		}
	}

	// Remove any child ContainerNetworkConnections that don't correspond to an expected network
	for i := range childNetworkConnections.Items {
		connection := childNetworkConnections.Items[i]

		networkConnectionKey := containerNetworkConnectionKey{
			Container: container.NamespacedName(),
			Network:   asNamespacedName(connection.Name, container.Namespace),
		}

		found := slices.Any(*container.Spec.Networks, func(network apiv1.ContainerNetworkConnectionConfig) bool {
			return asNamespacedName(network.Name, container.GetNamespace()) == asNamespacedName(connection.Spec.ContainerNetworkName, container.GetNamespace())
		})

		if found {
			continue
		}

		if err := r.Delete(ctx, &connection, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground)); ctrl_client.IgnoreNotFound(err) != nil {
			log.Error(err, "could not delete ContainerNetworkConnection object",
				"Container", container.NamespacedName().String(),
				"Network", connection.Spec.ContainerNetworkName,
			)
		} else {
			log.Info("Removed a ContainerNetworkConnection connection", "Container", containerID, "ContainerNetworkConnection", connection.NamespacedName().String())
			r.networkConnections.Delete(networkConnectionKey)
		}
	}

	// Create new ContainerNetworkConnections for any expected networks that don't have one
	for i := range *container.Spec.Networks {
		network := (*container.Spec.Networks)[i]

		namespacedNetworkName := asNamespacedName(network.Name, container.Namespace)

		networkConnectionKey := containerNetworkConnectionKey{
			Container: container.NamespacedName(),
			Network:   namespacedNetworkName,
		}

		if _, found := r.networkConnections.Load(networkConnectionKey); found {
			// We should have already created a ContainerNetworkConnection object, wait for it to show up in a subsequent reconciliation
			continue
		}

		found := slices.Any(childNetworkConnections.Items, func(cnc apiv1.ContainerNetworkConnection) bool {
			namespacedConnection := asNamespacedName(cnc.Spec.ContainerNetworkName, container.Namespace)

			return namespacedConnection == namespacedNetworkName
		})

		if found {
			// Connection should exist
			continue
		}

		uniqueName, err := MakeUniqueName(fmt.Sprint(container.Name, "-", namespacedNetworkName.Name))
		if err != nil {
			log.Error(err, "could not generate unique name for ContainerNetworkConnection object")
			continue
		}

		// Otherwise, create a new ContainerNetworkConnection object.
		connection := &apiv1.ContainerNetworkConnection{
			ObjectMeta: metav1.ObjectMeta{
				Name:      uniqueName,
				Namespace: container.Namespace,
			},
			Spec: apiv1.ContainerNetworkConnectionSpec{
				ContainerNetworkName: namespacedNetworkName.String(),
				ContainerID:          containerID,
				Aliases:              network.Aliases,
			},
		}

		if err = ctrl.SetControllerReference(container, connection, r.Scheme()); err != nil {
			log.Error(err, "failed to set owner for network connection",
				"Container", container.NamespacedName().String(),
				"Network", namespacedNetworkName.String(),
			)
		}

		if err = r.Create(ctx, connection); err != nil {
			log.Error(err, "could not persist ContainerNetworkConnection object",
				"Container", container.NamespacedName().String(),
				"Network", namespacedNetworkName.String(),
			)
		} else {
			log.Info("Added new ContainerNetworkConnection", "Container", containerID, "ContainerNetworkConnection", connection.NamespacedName().String())
			r.networkConnections.Store(networkConnectionKey, true)
		}
	}

	return validConnectedNetworks, nil
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
	var matchedPort apiv1.ContainerPort
	found := false

	matchedByHost := slices.Select(ctr.Spec.Ports, func(p apiv1.ContainerPort) bool {
		return p.HostPort == serviceProducerPort
	})
	if len(matchedByHost) > 0 {
		matchedPort = matchedByHost[0]
		found = true
	} else {
		matchedByContainer := slices.Select(ctr.Spec.Ports, func(p apiv1.ContainerPort) bool {
			return p.ContainerPort == serviceProducerPort
		})
		if len(matchedByContainer) > 0 {
			matchedPort = matchedByContainer[0]
			found = true
		}
	}

	if found && matchedPort.HostPort != 0 {
		// If the spec contains a port matching the desired container port, just use that
		log.V(1).Info("found matching port in Container spec", "ServiceProducerPort", serviceProducerPort, "HostPort", matchedPort.HostPort)
		return matchedPort.HostIP, matchedPort.HostPort, nil
	}

	// Need to inspect the container to find the port used by the service
	// (auto-allocated by Docker).
	_, rcd, rcdFound := r.runningContainers.FindByFirstKey(ctr.NamespacedName())
	if !rcdFound || !rcd.hasValidContainerID() {
		// Should never happen--this method should only be called for running container.
		return "", 0, fmt.Errorf("running container data not found for Container '%s'", ctr.NamespacedName())
	}
	log.V(1).Info("inspecting running container resource to get its port information...", "ContainerID", rcd.containerID)
	inspected, err := r.findContainer(ctx, rcd.containerID)
	if err != nil {
		return "", 0, err
	}

	if inspected.Status != containers.ContainerStatusRunning {
		return "", 0, fmt.Errorf("container '%s' is not running: %s", inspected.Name, inspected.Status)
	}

	var matchedHostPort ct.InspectedContainerHostPortConfig
	found = false
	for k, v := range inspected.Ports {
		ctrPort := strings.Split(k, "/")[0]

		if ctrPort == fmt.Sprintf("%d", serviceProducerPort) {
			matchedHostPort = v[0]
			found = true
			break
		}
	}

	if !found {
		return "", 0, fmt.Errorf("could not find host port for container port %d (no matching host port found)", serviceProducerPort)
	}

	hostPort, err := strconv.ParseInt(matchedHostPort.HostPort, 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("could not parse host port '%s' as integer", matchedHostPort.HostPort)
	} else if hostPort <= 0 {
		return "", 0, fmt.Errorf("could not find host port for container port %d (invalid host port value %d reported by container orchestrator)", serviceProducerPort, hostPort)
	}

	log.V(1).Info("matched service producer port to one of the container host ports", "ServiceProducerPort", serviceProducerPort, "HostPort", hostPort, "HostIP", matchedHostPort.HostIp)
	return matchedHostPort.HostIp, int32(hostPort), nil
}

// CONTAINER RESOURCE MANAGEMENT METHODS

func (r *ContainerReconciler) findContainer(ctx context.Context, container string) (*ct.InspectedContainer, error) {
	res, err := r.orchestrator.InspectContainers(ctx, []string{container})
	if err != nil {
		return nil, err
	}

	if len(res) == 0 {
		return nil, ct.ErrNotFound
	}

	return &res[0], nil
}

func (r *ContainerReconciler) ensureContainerWatch(container *apiv1.Container, log logr.Logger) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.lifetimeCtx.Err() != nil {
		return // Do not start a container watch if we are done
	}

	_, _ = r.watchingResources.LoadOrStore(container.UID, true)

	if r.containerEvtSub != nil {
		return // We're already watching container events, nothing to do
	}

	r.containerEvtCh = chanx.NewUnboundedChan[ct.EventMessage](r.lifetimeCtx, containerEventChanInitialCapacity)
	r.networkEvtCh = chanx.NewUnboundedChan[ct.EventMessage](r.lifetimeCtx, containerEventChanInitialCapacity)

	r.containerEvtWorkerStop = make(chan struct{})
	go r.containerEventWorker(r.containerEvtWorkerStop, r.containerEvtCh.Out, r.networkEvtCh.Out)

	log.V(1).Info("subscribing to container events...")
	containerSub, containerSubErr := r.orchestrator.WatchContainers(r.containerEvtCh.In)
	networkSub, networkSubErr := r.orchestrator.WatchNetworks(r.networkEvtCh.In)

	err := errors.Join(containerSubErr, networkSubErr)

	if err != nil {
		log.Error(err, "could not subscribe to events")
		close(r.containerEvtWorkerStop)
		r.containerEvtWorkerStop = nil
		return
	}

	r.containerEvtSub = containerSub
	r.networkEvtSub = networkSub
}

func (r *ContainerReconciler) releaseContainerWatch(container *apiv1.Container, log logr.Logger) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.watchingResources.Delete(container.UID)
	if r.watchingResources.Empty() {
		log.Info("no more Container resources are being watched, cancelling container watch")
		r.cancelContainerWatch()
	}
}

func (r *ContainerReconciler) cancelContainerWatch() {
	if r.containerEvtWorkerStop != nil {
		close(r.containerEvtWorkerStop)
		r.containerEvtWorkerStop = nil
	}
	if r.containerEvtSub != nil {
		_ = r.containerEvtSub.Cancel()
		r.containerEvtSub = nil
	}
	if r.networkEvtSub != nil {
		_ = r.networkEvtSub.Cancel()
		r.networkEvtSub = nil
	}
}

func (r *ContainerReconciler) containerEventWorker(
	stopCh chan struct{},
	containerEvtCh <-chan ct.EventMessage,
	networkEvtCh <-chan ct.EventMessage,
) {
	for {
		select {
		case cem := <-containerEvtCh:
			if cem.Source != ct.EventSourceContainer {
				continue
			}

			r.processContainerEvent(cem)
		case nem := <-networkEvtCh:
			if nem.Source != ct.EventSourceNetwork {
				continue
			}

			r.processNetworkEvent(nem)

		case <-stopCh:
			return
		}
	}
}

func (r *ContainerReconciler) processContainerEvent(em ct.EventMessage) {
	switch em.Action {
	// Any event that means the container has been started, stopped, or was removed, is interesting
	case ct.EventActionCreate, ct.EventActionDestroy, ct.EventActionDie, ct.EventActionKill, ct.EventActionOom, ct.EventActionStop, ct.EventActionRestart, ct.EventActionStart, ct.EventActionPrune:
		containerID := em.Actor.ID
		owner, _, running := r.runningContainers.FindBySecondKey(containerID)
		if !running {
			// We are not tracking this container
			return
		}

		r.scheduleContainerReconciliation(owner, containerID)
	}
}

func (r *ContainerReconciler) processNetworkEvent(em ct.EventMessage) {
	switch em.Action {
	case ct.EventActionConnect, ct.EventActionDisconnect:
		containerID, found := em.Attributes["container"]
		if !found {
			// We could not identify the container this event applies to
			return
		}

		owner, _, running := r.runningContainers.FindBySecondKey(containerID)
		if !running {
			// We are not tracking this container
			return
		}

		r.scheduleContainerReconciliation(owner, containerID)
	}
}

// MISCELLANEOUS HELPER METHODS

func setContainerState(container *apiv1.Container, state apiv1.ContainerState) objectChange {
	if container.Status.State == state {
		return noChange
	}

	container.Status.State = state
	return statusChanged
}

func (r *ContainerReconciler) computeEffectiveEnvironment(
	ctx context.Context,
	ctr *apiv1.Container,
	rcd *runningContainerData,
	log logr.Logger,
) error {
	// Note: there is no value substitution by DCP for .env files, these are handled by container orchestrator directly.

	tmpl, err := newSpecValueTemplate(ctx, r, ctr, rcd.reservedPorts, log)
	if err != nil {
		return err
	}

	for i, envVar := range rcd.runSpec.Env {
		substitutionCtx := fmt.Sprintf("environment variable %s", envVar.Name)
		effectiveValue, templateErr := executeTemplate(tmpl, ctr, envVar.Value, substitutionCtx, log)
		if templateErr != nil {
			return templateErr
		}

		rcd.runSpec.Env[i] = apiv1.EnvVar{Name: envVar.Name, Value: effectiveValue}
	}

	return nil
}

func (r *ContainerReconciler) computeEffectiveInvocationArgs(
	ctx context.Context,
	ctr *apiv1.Container,
	rcd *runningContainerData,
	log logr.Logger,
) error {
	tmpl, err := newSpecValueTemplate(ctx, r, ctr, rcd.reservedPorts, log)
	if err != nil {
		return err
	}

	for i, arg := range rcd.runSpec.Args {
		substitutionCtx := fmt.Sprintf("argument %d", i)
		effectiveValue, templateErr := executeTemplate(tmpl, ctr, arg, substitutionCtx, log)
		if templateErr != nil {
			return templateErr
		}

		rcd.runSpec.Args[i] = effectiveValue
	}

	return nil
}

func (r *ContainerReconciler) scheduleContainerReconciliation(containerName types.NamespacedName, containerID string) {
	err := r.debouncer.ReconciliationNeeded(containerName, containerID, r.doReconcileContainer)
	if err != nil {
		r.Log.Error(err, "could not schedule reconcilation for Container object",
			"ContainerName", containerName.String(),
			"ContainerID", containerID,
		)
	}
}

func (r *ContainerReconciler) doReconcileContainer(rti reconcileTriggerInput[string]) error {
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

func (r *ContainerReconciler) onShutdown() {
	<-r.lifetimeCtx.Done()

	r.lock.Lock()
	defer r.lock.Unlock()
	r.cancelContainerWatch()
}
