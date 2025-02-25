// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
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
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/pubsub"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/internal/version"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

const (
	containerEventChanBuffer                = 20 // Container events tend to come in bursts, so we can help a bit with buffering
	DefaultMaxParallelContainerStarts uint8 = 6

	startupRetryDelay = 1 * time.Second
	noDelay           = 0 * time.Second

	// How long the container orchestrator will wait for a container to stop before killing it
	stopContainerTimeoutSeconds = 10
	containerInspectionTimeout  = 6 * time.Second

	ownerKey          = ".metadata.controllerOwner" // client index key for child ContainerNetworkConnections
	dcpBuildLabel     = "com.microsoft.developer.usvc-dev.build"
	groupVersionLabel = "com.microsoft.developer.usvc-dev.group-version"
	nameLabel         = "com.microsoft.developer.usvc-dev.name"
	uidLabel          = "com.microsoft.developer.usvc-dev.uid"
	lifecycleKeyLabel = "com.microsoft.developer.usvc-dev.lifecycle-key"
	envLabel          = "com.microsoft.developer.usvc-dev.env"
	mountsLabel       = "com.microsoft.developer.usvc-dev.mountsLabel"
	portsLabel        = "com.microsoft.developer.usvc-dev.ports"
)

var (
	containerFinalizer string = fmt.Sprintf("%s/container-reconciler", apiv1.GroupVersion.Group)

	containerStateInitializers = map[apiv1.ContainerState]containerStateInitializerFunc{
		apiv1.ContainerStateEmpty:            handleNewContainer,
		apiv1.ContainerStatePending:          handleNewContainer,
		apiv1.ContainerStateRuntimeUnhealthy: handleNewContainer,
		apiv1.ContainerStateBuilding:         ensureContainerBuildingState,
		apiv1.ContainerStateStarting:         ensureContainerStartingState,
		apiv1.ContainerStateFailedToStart:    ensureContainerFailedToStartState,
		apiv1.ContainerStateRunning:          ensureContainerRunningState,
		apiv1.ContainerStatePaused:           ensureContainerRunningState,
		apiv1.ContainerStateExited:           ensureContainerExitedState,
		apiv1.ContainerStateUnknown:          ensureContainerUnknownState,
		apiv1.ContainerStateStopping:         ensureContainerStoppingState,
	}

	winNamedPipeRegex = regexp.MustCompile(`^(\\\\.pipe\\|//.pipe/)`)
)

type ContainerReconcilerConfig struct {
	MaxParallelContainerStarts      uint8
	ContainerStartupTimeoutOverride time.Duration
}

type containerStateInitializerFunc = stateInitializerFunc[
	apiv1.Container, *apiv1.Container,
	ContainerReconciler, *ContainerReconciler,
	apiv1.ContainerState,
	runningContainerData, *runningContainerData,
]

type runningContainersMap = ObjectStateMap[string, runningContainerData, *runningContainerData]

type ContainerReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	orchestrator        containers.ContainerOrchestrator

	// Channel used to trigger reconciliation when underlying containers change
	notifyContainerChanged *concurrency.UnboundedChan[ctrl_event.GenericEvent]

	// A map that stores information about containers (the real things run by the container orchestrator).
	// It is searchable by Container object name (first key) or container ID (second key).
	// Usually both keys are valid, but when the container is starting, we do not have the real container ID yet,
	// so we use a "placeholder" random string that is replaced by real container ID once we know the container outcome.
	runningContainers *runningContainersMap

	// A WorkQueue used for building, starting and stopping containers,
	// which are long-running operations that we do in parallel, with limited concurrency.
	startupQueue *resiliency.WorkQueue

	// Container events subscription
	containerEvtSub *pubsub.Subscription[containers.EventMessage]
	// Network events subscription
	networkEvtSub *pubsub.Subscription[containers.EventMessage]
	// Channel to receive container change events
	containerEvtCh *concurrency.UnboundedChan[containers.EventMessage]
	// Channel to receive network change events
	networkEvtCh *concurrency.UnboundedChan[containers.EventMessage]
	// Channel to stop the event worker
	containerEvtWorkerStop chan struct{}

	// Debouncer used to schedule reconciliation. Extra data is the running container ID whose state changed.
	debouncer *reconcilerDebouncer[string]

	// Count of existing Container resources
	watchingResources *syncmap.Map[types.UID, bool]

	// Lock to protect the reconciler data that requires synchronized access
	lock *sync.Mutex

	// Reconciler lifetime context, used to cancel container watch during reconciler shutdown
	lifetimeCtx context.Context

	// Additional configuration for the reconciler
	config ContainerReconcilerConfig
}

func NewContainerReconciler(lifetimeCtx context.Context, client ctrl_client.Client, log logr.Logger, orchestrator containers.ContainerOrchestrator, config ContainerReconcilerConfig) *ContainerReconciler {
	r := ContainerReconciler{
		Client:                 client,
		orchestrator:           orchestrator,
		notifyContainerChanged: concurrency.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx),
		runningContainers:      NewObjectStateMap[string, runningContainerData](),
		startupQueue:           resiliency.NewWorkQueue(lifetimeCtx, config.MaxParallelContainerStarts),
		containerEvtSub:        nil,
		networkEvtSub:          nil,
		containerEvtCh:         nil,
		networkEvtCh:           nil,
		containerEvtWorkerStop: nil,
		debouncer:              newReconcilerDebouncer[string](),
		watchingResources:      &syncmap.Map[types.UID, bool]{},
		lock:                   &sync.Mutex{},
		lifetimeCtx:            lifetimeCtx,
		config:                 config,
		Log:                    log,
	}

	go r.onShutdown()

	return &r
}

func (r *ContainerReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	// Setup a client side index to allow quickly finding all ContainerNetworkConnections owned by a Container.
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
		Named(name).
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
	if container.DeletionTimestamp != nil && !container.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, &container, log)
	} else if change = ensureFinalizer(&container, containerFinalizer, log); change != noChange {
		// If we need to put the finalizer on the Container object, we'll do any additional changes during next reconciliation.
	} else {
		change = r.manageContainer(ctx, &container, log)
	}

	reconciliationDelay := defaultAdditionalReconciliationDelay
	if container.Status.State == apiv1.ContainerStateRuntimeUnhealthy && (change&additionalReconciliationNeeded) == 0 {
		log.V(1).Info("container runtime is not healthy, retrying reconciliation later...")
		reconciliationDelay = time.Duration(rand.Intn(5)+5) * time.Second
		change |= additionalReconciliationNeeded
	}

	return saveChangesWithCustomReconciliationDelay(r.Client, ctx, &container, patch, change, reconciliationDelay, nil, log)
}

func (r *ContainerReconciler) manageContainer(ctx context.Context, container *apiv1.Container, log logr.Logger) objectChange {
	targetContainerState := container.Status.State
	_, rcd := r.runningContainers.BorrowByNamespacedName(container.NamespacedName())
	if rcd != nil {
		// In-memory container state is not subject to issues related to caching and
		// status updates failed due to conflict, so it is fresher and has precedence.
		targetContainerState = rcd.containerState
	}

	// Even if the new container state is (as it is usually the case) the same as the target state,
	// we still want to run the state handler to ensure that the Container object Status,
	// and the real-world resources associated with the Container object, are up to date.
	initalizer := getStateInitializer(containerStateInitializers, targetContainerState, log)
	change := initalizer(ctx, r, container, targetContainerState, rcd, log)

	if rcd != nil {
		// UNDONE: should we handle runningContainerData deletion here?

		r.runningContainers.Update(container.NamespacedName(), rcd.containerID, rcd)
	}

	return change
}

func (r *ContainerReconciler) handleDeletionRequest(ctx context.Context, container *apiv1.Container, log logr.Logger) objectChange {
	_, rcd := r.runningContainers.BorrowByNamespacedName(container.NamespacedName())

	var change objectChange

	switch {
	case rcd == nil:
		change = deleteFinalizer(container, containerFinalizer, log)

	case rcd.containerState == apiv1.ContainerStateBuilding || rcd.containerState == apiv1.ContainerStateStarting || rcd.containerState == apiv1.ContainerStateStopping:
		// Wait for Container to exit this transient state.
		change = r.manageContainer(ctx, container, log)

	case (rcd.containerState == apiv1.ContainerStateRunning || rcd.containerState == apiv1.ContainerStatePaused) && !container.Spec.Persistent:
		// Need to stop the container first.
		rcd.containerState = apiv1.ContainerStateStopping
		stoppingInitializer := getStateInitializer(containerStateInitializers, apiv1.ContainerStateStopping, log)
		change = stoppingInitializer(ctx, r, container, apiv1.ContainerStateStopping, rcd, log)
		r.runningContainers.Update(container.NamespacedName(), rcd.containerID, rcd)

	default: // In initial or final state
		log.V(1).Info("Container object is being deleted...")
		r.cleanupContainerResources(ctx, container, rcd, log)
		change = deleteFinalizer(container, containerFinalizer, log)
		r.runningContainers.DeleteByNamespacedName(container.NamespacedName())
	}

	return change
}

// STATE INITIALIZER FUNCTIONS

func handleNewContainer(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	_ apiv1.ContainerState,
	_ *runningContainerData,
	_ logr.Logger,
) objectChange {
	var change objectChange

	if container.Spec.Stop {
		// The container was started with a desired state of stopped, don't attempt to start it.
		return setContainerState(container, apiv1.ContainerStateFailedToStart)
	}

	status := r.orchestrator.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed)
	if !status.IsHealthy() {
		// If the runtime isn't healthy, we will attempt to start the container later (in case the runtime recovers).
		return setContainerState(container, apiv1.ContainerStateRuntimeUnhealthy)
	}

	if container.Spec.Build != nil {
		// Container has a build context, so need to build it first.
		change = setContainerState(container, apiv1.ContainerStateBuilding)
	} else {
		// Initiate startup sequence.
		change = setContainerState(container, apiv1.ContainerStateStarting)
	}

	return change
}

func ensureContainerBuildingState(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	_ apiv1.ContainerState,
	rcd *runningContainerData,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, apiv1.ContainerStateBuilding)

	if rcd == nil {
		// This is a brand new Container and we need to build it.

		rcd = newRunningContainerData(container)
		rcd.containerState = apiv1.ContainerStateBuilding
		rcd.ensureStartupLogFiles(container, log)
		r.ensureContainerWatch(container, log)

		log.V(1).Info("scheduling image build")
		err := r.startupQueue.Enqueue(r.buildImageWithOrchestrator(container, rcd.Clone(), log))
		if err != nil {
			log.Error(err, "image was not built, possibly because the workload is shutting down")
			rcd.containerState = apiv1.ContainerStateFailedToStart
			rcd.startupError = err
			rcd.startAttemptFinishedAt = metav1.NowMicro()
		}

		r.runningContainers.Store(container.NamespacedName(), rcd.containerID, rcd)
		change |= statusChanged
	}

	// The attempt to build the container is generally asynchronous, but it may fail or succeed immediately.
	// The former could be due to some non-transient error from the container orchestrator.
	// The latter could be because we are dealing with a persistent Container and we found a matching, existing container resource.
	// Either way, even if we are just waiting for the container to build, the Status of the Container object may be stale.
	// It might be just a caching issue, but it also might be because of a write conflict during last update,
	// so we need to make sure it is what it should be.
	// Bottom line we always want to apply the changes to the Container object.
	change |= rcd.applyTo(container)

	return change
}

func ensureContainerStartingState(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	_ apiv1.ContainerState,
	rcd *runningContainerData,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, apiv1.ContainerStateStarting)

	if rcd == nil {
		// This is brand new Container and we need to start it.
		rcd = newRunningContainerData(container)
		rcd.containerState = apiv1.ContainerStateStarting
		rcd.ensureStartupLogFiles(container, log)

		r.runningContainers.Store(container.NamespacedName(), rcd.containerID, rcd.Clone())
		r.ensureContainerWatch(container, log)
		r.scheduleContainerCreation(container, rcd, log, noDelay)

		// We need to update the runningContainers map with the new data here
		// because the caller did not have a runningContainerData instance.
		_ = r.runningContainers.Update(container.NamespacedName(), rcd.containerID, rcd)

		change |= statusChanged

	} else if !rcd.startupAttempted {
		// We haven't attempted to start the Container yet (likely we're here after build completed).

		rcd.ensureStartupLogFiles(container, log)

		r.scheduleContainerCreation(container, rcd, log, noDelay)
		change |= statusChanged

	} else if isTransientTemplateError(rcd.startupError) {
		// Retry startup after transient error.

		rcd.startupError = nil
		rcd.startAttemptFinishedAt = metav1.MicroTime{}
		rcd.containerName = ""

		r.scheduleContainerCreation(container, rcd, log, startupRetryDelay)
		change |= statusChanged

	} else if container.Spec.Networks != nil && rcd.hasValidContainerID() {
		// The second portion of startup sequence of a container with custom networks.
		// Need to create ContainerNetworkConnection objects and start the container resource.

		started, err := r.handleInitialNetworkConnections(ctx, container, rcd, log)

		switch {
		case err != nil:
			rcd.startupError = err
			rcd.containerState = apiv1.ContainerStateFailedToStart
			rcd.startAttemptFinishedAt = metav1.NowMicro()
			change |= statusChanged
		case started:
			rcd.containerState = apiv1.ContainerStateRunning
			rcd.startAttemptFinishedAt = metav1.NowMicro()
			change |= statusChanged
		default:
			// We are waiting for the network connections to be established.
			change |= additionalReconciliationNeeded
		}
	}

	// The attempt to start the container is generally asynchronous, but it may fail or succeed immediately.
	// The former could be due to some non-transient error from the container orchestrator.
	// The latter could be because we are dealing with a persistent Container and we found a matching, existing container resource.
	// Either way, even if we are just waiting for the container to start, the Status of the Container object may be stale.
	// It might be just a caching issue, but it also might be because of a write conflict during last update,
	// so we need to make sure it is what it should be.
	// Bottom line we always want to apply the changes to the Container object.
	change |= rcd.applyTo(container)

	return change
}

func ensureContainerFailedToStartState(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	_ apiv1.ContainerState,
	rcd *runningContainerData,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, apiv1.ContainerStateFailedToStart)

	if rcd == nil {
		// Can happen if the container was created with Spec.Stop = true
		if container.Status.FinishTimestamp.IsZero() {
			container.Status.FinishTimestamp = metav1.NowMicro()
			change |= statusChanged
		}
	} else {
		change |= rcd.applyTo(container)
		rcd.closeStartupLogFiles(log)

		// The process of connecting the container to desired network(s) begins before startup attempt,
		// so we need to update the Container.Status accordingly even if the container failed to start.
		if rcd.hasValidContainerID() && container.Spec.Networks != nil {
			connectedNetworks, networkRelatedChange := r.handleContainerNetworkConnections(ctx, container, nil, rcd, log)
			change |= networkRelatedChange
			if len(connectedNetworks) > 0 {
				container.Status.Networks = connectedNetworks
			}
		}
	}

	return change
}

// Handles both Running and Paused states.
func ensureContainerRunningState(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	desiredState apiv1.ContainerState,
	rcd *runningContainerData,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, desiredState)

	if rcd == nil {
		// Should never happen--the runningContainers map should have the data about the Container object.
		log.Error(fmt.Errorf("the data about the container resource is missing (current container state is '%s')", desiredState), "")
		return ensureContainerUnknownState(ctx, r, container, desiredState, nil, log)
	}

	if container.Spec.Stop {
		// Start the stopping sequence
		rcd.containerState = apiv1.ContainerStateStopping
		change |= setContainerState(container, apiv1.ContainerStateStopping)
		return change
	}

	if desiredState != apiv1.ContainerStateRunning {
		removeEndpointsForWorkload(r, ctx, container, log)
	} else {
		reservedPorts := rcd.reservedPorts
		ensureEndpointsForWorkload(ctx, r, container, reservedPorts, log)
	}

	log.V(1).Info("inspecting container resource...", "ContainerID", rcd.containerID)
	inspected, err := inspectContainer(ctx, r.orchestrator, rcd.containerID)
	if err != nil {
		if errors.Is(err, containers.ErrNotFound) {
			log.Info("container resource not found, assuming it was removed... ", "ContainerID", rcd.containerID)
			return ensureContainerUnknownState(ctx, r, container, desiredState, rcd, log)
		} else {
			log.Info("container resource could not be inspected ",
				"ContainerID", rcd.containerID,
				"Error", err.Error(),
			)
			// Could be a transient error, so for know we keep the rest of the status as-is.
			// Don't try to reconcile again unconditionally (that might result in an infinite loop),
			// but instead wait for another event from the container watcher.
			return change
		}
	}

	rcd.updateFromInspectedContainer(inspected)
	change |= rcd.applyTo(container)

	if container.Spec.Networks != nil {
		connectedNetworks, networkRelatedChange := r.handleContainerNetworkConnections(ctx, container, inspected, rcd, log)
		change |= networkRelatedChange
		if len(connectedNetworks) > 0 {
			container.Status.Networks = connectedNetworks
		}
	}

	return change
}

func ensureContainerExitedState(ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	_ apiv1.ContainerState,
	rcd *runningContainerData,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, apiv1.ContainerStateExited)

	if rcd == nil {
		// Should never happen--the runningContainers map should have the data about the Container object.
		log.Error(fmt.Errorf("the data about the container resource is missing (current container state is '%s')", apiv1.ContainerStateExited), "")
		return ensureContainerUnknownState(ctx, r, container, apiv1.ContainerStateUnknown, nil, log)
	}

	rcd.closeStartupLogFiles(log)
	removeEndpointsForWorkload(r, ctx, container, log)

	log.V(1).Info("inspecting container resource...", "ContainerID", rcd.containerID)
	inspected, err := inspectContainer(ctx, r.orchestrator, rcd.containerID)
	if err != nil {
		log.Info("container resource could not be inspected, might have been removed... ",
			"ContainerID", rcd.containerID,
			"Error", err.Error(),
		)
		return change // Best effort--skipping the rest.
	}

	rcd.updateFromInspectedContainer(inspected)
	change |= rcd.applyTo(container)

	if container.Spec.Networks != nil {
		connectedNetworks, networkRelatedChange := r.handleContainerNetworkConnections(ctx, container, inspected, rcd, log)
		change |= networkRelatedChange
		if len(connectedNetworks) > 0 {
			container.Status.Networks = connectedNetworks
		}
	}

	return change
}

func ensureContainerUnknownState(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	_ apiv1.ContainerState,
	rcd *runningContainerData,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, apiv1.ContainerStateUnknown)
	if change != noChange {
		log.Error(fmt.Errorf("the state of the Container became undetermined"), "")
	}
	if container.Status.FinishTimestamp.IsZero() {
		container.Status.FinishTimestamp = metav1.NowMicro()
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
	if rcd != nil {
		rcd.containerState = apiv1.ContainerStateUnknown
		rcd.closeStartupLogFiles(log)
	}

	return change
}

func ensureContainerStoppingState(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	_ apiv1.ContainerState,
	rcd *runningContainerData,
	log logr.Logger,
) objectChange {
	change := setContainerState(container, apiv1.ContainerStateStopping)

	if rcd == nil {
		// Should never happen--the runningContainers map should have the data about the Container object.
		log.Error(fmt.Errorf("the data about the container resource is missing (current container state is '%s')", apiv1.ContainerStateStopping), "")
		return ensureContainerUnknownState(ctx, r, container, apiv1.ContainerStateUnknown, nil, log)
	}

	rcd.closeStartupLogFiles(log)

	if !rcd.stopAttemptInitiated {
		rcd.stopAttemptInitiated = true
		err := r.startupQueue.Enqueue(r.stopContainerFunc(container, rcd.Clone(), log))
		if err != nil {
			log.Error(err, "could not stop the container")
			change |= ensureContainerUnknownState(ctx, r, container, apiv1.ContainerStateUnknown, rcd, log)
			return change
		}
	}

	change |= rcd.applyTo(container)

	removeEndpointsForWorkload(r, ctx, container, log)

	return change
}

// CONTAINER START/STOP HELPER METHODS

// Stops the container resource. If the container resource is already in one of the "effectively stopped"
// states (Exited, Stopped, or Dead), the method does nothing and returns no error.
// Returns containers.ErrNotFound if the container resource was not found.
// Returns an error if the container resource could not be stopped.
func (r *ContainerReconciler) stopContainerIfNecessary(
	ctx context.Context,
	id string,
	inspected *containers.InspectedContainer,
	log logr.Logger,
) error {
	needsStopping := func() bool {
		if inspected == nil {
			return true // Assume the container needs stopping, worst case the stop attempt will fail/time out.
		}
		return inspected.Status == containers.ContainerStatusRunning ||
			inspected.Status == containers.ContainerStatusPaused ||
			inspected.Status == containers.ContainerStatusRestarting
	}

	if inspected == nil {
		var inspectErr error
		inspected, inspectErr = inspectContainer(ctx, r.orchestrator, id)
		if inspectErr != nil {
			if errors.Is(inspectErr, containers.ErrNotFound) {
				log.Info("container resource not found, assuming it was removed... ", "ContainerID", id)
				return containers.ErrNotFound
			}
		}
	}

	if needsStopping() {
		log.V(1).Info("calling container orchestrator to stop the existing container...", "ContainerID", id)
		stopErr := stopContainer(ctx, r.orchestrator, id)
		if stopErr != nil {
			log.Error(stopErr, "could not stop the running container", "ContainerID", id)
		}
		return stopErr
	}

	return nil
}

func (r *ContainerReconciler) removeExistingContainer(
	ctx context.Context,
	id string,
	inspected *containers.InspectedContainer,
	log logr.Logger,
) error {
	stopErr := r.stopContainerIfNecessary(ctx, id, inspected, log)
	if errors.Is(stopErr, containers.ErrNotFound) {
		// Already logged (at info level) by stopExistingContainer()
		return nil // Nothing to do
	}

	// Try to remove the container even if stop error occurred.
	removeErr := removeContainer(ctx, r.orchestrator, id)
	if removeErr != nil {
		// Log any unexpected error, but attempt to continue with creation of the new container
		log.Error(removeErr, "could not remove the running container", "ContainerID", id)
		return removeErr
	}

	return nil
}

// Schedules creation of a container resource. If Container is persistent, it will attempt to find and reuse an existing container.
func (r *ContainerReconciler) scheduleContainerCreation(
	container *apiv1.Container,
	rcd *runningContainerData,
	log logr.Logger,
	delay time.Duration,
) {
	containerName := strings.TrimSpace(container.Spec.ContainerName)

	rcd.startupAttempted = true

	log.V(1).Info("scheduling container start", "image", container.SpecifiedImageNameOrDefault())

	if containerName == "" {
		uniqueContainerName, _, err := MakeUniqueName(container.Name)
		if err != nil {
			log.Error(err, "could not generate a unique container name")
			rcd.containerState = apiv1.ContainerStateFailedToStart
			rcd.startupError = err
			rcd.startAttemptFinishedAt = metav1.NowMicro()
			return
		}

		containerName = uniqueContainerName
	}

	err := r.startupQueue.Enqueue(r.startContainerWithOrchestrator(container, rcd.Clone(), containerName, log, delay))
	if err != nil {
		log.Error(err, "container was not started, probably because the workload is shutting down")
		rcd.containerState = apiv1.ContainerStateFailedToStart
		rcd.startupError = err
		rcd.startAttemptFinishedAt = metav1.NowMicro()
	}
}

// Returns a function that builds the Container image.
// The method is called as part of the reconciliation loop, but the returned function is executed asynchronously.
// The passed runningContainerData should be a clone independent from what is stored in the runningContainers map.
func (r *ContainerReconciler) buildImageWithOrchestrator(container *apiv1.Container, rcd *runningContainerData, log logr.Logger) func(context.Context) {
	return func(buildCtx context.Context) {
		err := func() error {
			log.V(1).Info("building image", "dockerfile", container.Spec.Build.Dockerfile, "context", container.Spec.Build.Context)

			rcd.runSpec.Build.Tags = append(rcd.runSpec.Build.Tags, container.SpecifiedImageNameOrDefault())
			rcd.runSpec.Build.Labels = append(rcd.runSpec.Build.Labels, []apiv1.ContainerLabel{
				{
					Key:   dcpBuildLabel,
					Value: version.ProductVersion,
				},
				{
					Key:   groupVersionLabel,
					Value: apiv1.GroupVersion.String(),
				},
			}...)

			buildOptions := containers.BuildImageOptions{
				IidFile:               filepath.Join(usvc_io.DcpTempDir(), fmt.Sprintf("%s_iid_%s", container.Name, container.UID)),
				Pull:                  container.Spec.PullPolicy == apiv1.PullPolicyAlways,
				ContainerBuildContext: rcd.runSpec.Build,
			}

			startupStdoutWriter, startupStderrWriter := rcd.getStartupLogWriters()
			buildOptions.StreamCommandOptions = containers.StreamCommandOptions{
				StdOutStream: startupStdoutWriter,
				StdErrStream: startupStderrWriter,
			}

			imageId, buildErr := buildImage(buildCtx, r.orchestrator, buildOptions)
			startupTaskFinished(startupStdoutWriter, startupStderrWriter)
			if buildErr != nil {
				log.Error(buildErr, "could not build the image")
				return buildErr
			}

			// We know the actual Image ID, so use that instead of the original name
			rcd.runSpec.Image = imageId
			rcd.containerState = apiv1.ContainerStateStarting

			return nil
		}()

		if err != nil {
			rcd.startupError = err
			rcd.startAttemptFinishedAt = metav1.NowMicro()
			rcd.containerState = apiv1.ContainerStateFailedToStart
		}

		r.runningContainers.QueueDeferredOp(
			container.NamespacedName(),
			func(containerObjectName types.NamespacedName, containerID string) {
				r.runningContainers.Update(containerObjectName, containerID, rcd)
			},
		)
		r.scheduleContainerReconciliation(container.NamespacedName(), rcd.containerID)
	}
}

func calculatePersistentContainerChanges(rcd *runningContainerData, inspected *containers.InspectedContainer) (mounts []string, ports []string, env []string, other []string) {
	// For the default lifecycle key behavior, we want to provide diagnostic logging on what configurations seem to have changed
	changeList := []string{}

	// Calculate differences in the image name
	if rcd.runSpec.Image != inspected.Image {
		if rcd.runSpec.Build != nil {
			changeList = append(changeList, "container was rebuilt due to Dockerfile or context changes")
		} else {
			changeList = append(changeList, fmt.Sprintf("container image name changed from %s to %s", inspected.Image, rcd.runSpec.Image))
		}
	}

	// Track the set of changed mounts (added, modified, or removed)
	changedMounts := map[string]bool{}

	// Calculate differences between mounts
	missingSpecMounts := slices.DiffFunc(rcd.runSpec.VolumeMounts, inspected.Mounts, func(a, b apiv1.VolumeMount) bool {
		return a.Type == b.Type && a.Source == b.Source && a.Target == b.Target && a.ReadOnly == b.ReadOnly
	})

	for _, mount := range missingSpecMounts {
		changedMounts[fmt.Sprintf("type=%s,src=%s", mount.Type, mount.Source)] = true
	}

	containerSpecMount := map[string]bool{}
	if mountsLabelKeys, found := inspected.Labels[mountsLabel]; found && mountsLabelKeys != "" {
		for _, mount := range strings.Split(mountsLabelKeys, "\n") {
			containerSpecMount[mount] = true
		}
	}

	for containerMount := range containerSpecMount {
		if !slices.Any(rcd.runSpec.VolumeMounts, func(mount apiv1.VolumeMount) bool {
			return fmt.Sprintf("type=%s,src=%s", mount.Type, mount.Source) == containerMount
		}) {
			changedMounts[containerMount] = true
		}
	}

	// Track the set of changed ports (added, modified, or removed)
	changedPorts := map[string]bool{}

	// Calculate differences between ports
	for _, port := range rcd.runSpec.Ports {
		protocol := "tcp"
		if port.Protocol != "" {
			protocol = strings.ToLower(string(port.Protocol))
		}
		containerBinding := fmt.Sprintf("%d/%s", port.ContainerPort, protocol)

		hostIP := networking.IPv4LocalhostDefaultAddress
		if port.HostIP != "" {
			hostIP = port.HostIP
		}

		hostPort := fmt.Sprintf("%d", port.HostPort)

		found := false
		for inspectedContainerBinding, inspectedHostBinding := range inspected.Ports {
			if containerBinding == inspectedContainerBinding {
				if hostIP == inspectedHostBinding[0].HostIp && (port.HostPort == 0 || hostPort == inspectedHostBinding[0].HostPort) {
					found = true
					break
				}
			}
		}

		if !found {
			changedPorts[containerBinding] = true
		}
	}

	containerSpecPort := map[string]bool{}
	if portsLabelKeys, found := inspected.Labels[portsLabel]; found && portsLabelKeys != "" {
		for _, port := range strings.Split(portsLabelKeys, "\n") {
			containerSpecPort[port] = true
		}
	}

	for portBinding := range containerSpecPort {
		if !slices.Any(rcd.runSpec.Ports, func(port apiv1.ContainerPort) bool {
			protocol := "tcp"
			if port.Protocol != "" {
				protocol = strings.ToLower(string(port.Protocol))
			}
			containerBinding := fmt.Sprintf("%d/%s", port.ContainerPort, protocol)

			return containerBinding == portBinding
		}) {
			containerSpecPort[portBinding] = true
		}
	}

	// Track the set of changed environment variables (added, modified, or removed)
	changedEnv := map[string]bool{}

	// Calculate differences between environment variables
	specEnv := map[string]string{}
	for _, env := range rcd.runSpec.Env {
		specEnv[env.Name] = env.Value
	}

	containerSpecEnv := map[string]bool{}
	if envLabelKeys, found := inspected.Labels[envLabel]; found && envLabelKeys != "" {
		for _, env := range strings.Split(envLabelKeys, "\n") {
			containerSpecEnv[env] = true
		}
	}

	// Look for missing or modified environment
	for k, v := range specEnv {
		inspectedValue, found := inspected.Env[k]
		if !found {
			changedEnv[k] = true
		} else if v != inspectedValue {
			changedEnv[k] = true
		}
	}

	// Look for environment that was removed between runs
	for k := range containerSpecEnv {
		_, found := specEnv[k]
		if !found {
			changedEnv[k] = true
		}
	}

	return maps.Keys(changedMounts), maps.Keys(changedPorts), maps.Keys(changedEnv), changeList
}

// Returns a function that attempts to start a container resource.
// If Container is persistent, it will attempt to find and reuse an existing container.
// The method is called as part of the reconciliation loop, but the returned function is executed asynchronously.
// The passed runningContainerData should be a clone independent from what is stored in the runningContainers map.
func (r *ContainerReconciler) startContainerWithOrchestrator(container *apiv1.Container, rcd *runningContainerData, containerName string, log logr.Logger, delay time.Duration) func(context.Context) {
	return func(startupCtx context.Context) {
		if delay > 0 {
			time.Sleep(delay)
		}

		placeholderContainerID := rcd.containerID

		err := func() error {
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

			lifecycleKey, hasDefaultLifecycleKey := rcd.getLifecycleKey()

			if container.Spec.Persistent {
				// Check for an existing persistent container
				inspected, inspectedErr := inspectContainerIfExists(startupCtx, r.orchestrator, containerName)
				if inspectedErr != nil && !errors.Is(inspectedErr, containers.ErrNotFound) {
					log.Error(inspectedErr, "could not inspect existing container", "ContainerName", containerName)
					return inspectedErr
				}

				if inspected != nil {
					_, dcpManaged := inspected.Labels[dcpBuildLabel]
					oldLifecycleKey, found := inspected.Labels[lifecycleKeyLabel]
					if dcpManaged && ((found && oldLifecycleKey != lifecycleKey) || (!found && lifecycleKey != "")) {
						// We need to recreate this DCP managed container because the lifecycle key has changed
						if hasDefaultLifecycleKey {
							mounts, ports, env, other := calculatePersistentContainerChanges(rcd, inspected)
							log.Info("found existing Container, but the lifecycle key doesn't match, recreating container", "ContainerName", containerName, "ContainerID", inspected.Id, "OldLifecycleKey", oldLifecycleKey, "NewLifecycleKey", lifecycleKey, "MountChanges", mounts, "PortChanges", ports, "EnvChanges", env, "OtherChanges", other)
						} else {
							log.Info("found existing Container, but the lifecycle key doesn't match, recreating container", "ContainerName", containerName, "ContainerID", inspected.Id, "OldLifecycleKey", oldLifecycleKey, "NewLifecycleKey", lifecycleKey)
						}

						if removeErr := r.removeExistingContainer(startupCtx, inspected.Id, inspected, log); removeErr != nil {
							return removeErr
						}
					} else if dcpManaged && inspected.Status != containers.ContainerStatusRunning {
						// We need to recreate this DCP managed container because it is not running
						log.Info("found existing Container that is not running, recreating container", "ContainerName", containerName, "ContainerID", inspected.Id, "ContainerStatus", inspected.Status)
						if removeErr := r.removeExistingContainer(startupCtx, inspected.Id, inspected, log); removeErr != nil {
							return removeErr
						}
					} else {
						log.Info("found existing Container", "ContainerName", containerName, "ContainerID", inspected.Id)
						rcd.updateFromInspectedContainer(inspected)
						rcd.startAttemptFinishedAt = metav1.NowMicro()
						rcd.containerState = apiv1.ContainerStateRunning
						return nil
					}
				}
			}

			for _, volume := range container.Spec.VolumeMounts {
				if volume.Type == apiv1.BindMount {
					isValid := filepath.IsAbs(volume.Source)
					if runtime.GOOS == "windows" {
						isValid = isValid && !winNamedPipeRegex.MatchString(volume.Source)

					}
					if !isValid {
						// This seems to be an invalid bind mount or a named pipe, so don't try to create it
						// May be a reference to a linux mount point on Windows
						continue
					}

					_, volErr := os.Stat(volume.Source)
					if errors.Is(volErr, os.ErrNotExist) {
						volErr = os.MkdirAll(volume.Source, osutil.PermissionDirectoryOthersRead)
						if volErr != nil {
							log.Error(volErr, "could not create bind mount source path", "Source", volume.Source, "Target", volume.Target)
							return volErr
						}
					} else if volErr != nil {
						log.Error(volErr, "could not verify existence of bind mount source path", "Volume", volume.Source, "Target", volume.Target)
						return volErr
					}
				}
			}

			log.V(1).Info("starting container", "image", container.SpecifiedImageNameOrDefault())

			defaultNetwork := ""
			if rcd.runSpec.Networks != nil {
				// See comment below why we create the container with default network explicitly enabled here.
				defaultNetwork = r.orchestrator.DefaultNetworkName()
			}

			rcd.runSpec.Labels = append(rcd.runSpec.Labels, []apiv1.ContainerLabel{
				{
					Key:   dcpBuildLabel,
					Value: version.ProductVersion,
				},
				{
					Key:   groupVersionLabel,
					Value: apiv1.GroupVersion.String(),
				},
				{
					Key:   uidLabel,
					Value: string(container.UID),
				},
				{
					Key:   nameLabel,
					Value: string(container.Name),
				},
				{
					Key:   lifecycleKeyLabel,
					Value: lifecycleKey,
				},
				{
					Key:   PersistentLabel,
					Value: fmt.Sprintf("%t", rcd.runSpec.Persistent),
				},
			}...)

			thisProcess, thisProcessErr := process.This()
			if thisProcessErr != nil {
				log.Error(thisProcessErr, "could not get the current process information; container will not have creator process information")
			} else {
				rcd.runSpec.Labels = append(rcd.runSpec.Labels, apiv1.ContainerLabel{
					Key:   CreatorProcessIdLabel,
					Value: fmt.Sprintf("%d", thisProcess.Pid),
				})
				rcd.runSpec.Labels = append(rcd.runSpec.Labels, apiv1.ContainerLabel{
					Key:   CreatorProcessStartTimeLabel,
					Value: thisProcess.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
				})
			}

			if len(rcd.runSpec.Env) > 0 {
				rcd.runSpec.Labels = append(rcd.runSpec.Labels, apiv1.ContainerLabel{
					Key: envLabel,
					Value: strings.Join(slices.Map[apiv1.EnvVar, string](rcd.runSpec.Env, func(env apiv1.EnvVar) string {
						return env.Name
					}), "\n"),
				})
			}

			if len(rcd.runSpec.Ports) > 0 {
				rcd.runSpec.Labels = append(rcd.runSpec.Labels, apiv1.ContainerLabel{
					Key: portsLabel,
					Value: strings.Join(slices.Map[apiv1.ContainerPort, string](rcd.runSpec.Ports, func(port apiv1.ContainerPort) string {
						protocol := "tcp"
						if port.Protocol != "" {
							protocol = strings.ToLower(string(port.Protocol))
						}
						return fmt.Sprintf("%d/%s", port.ContainerPort, protocol)
					}), "\n"),
				})
			}

			if len(rcd.runSpec.VolumeMounts) > 0 {
				rcd.runSpec.Labels = append(rcd.runSpec.Labels, apiv1.ContainerLabel{
					Key: mountsLabel,
					Value: strings.Join(slices.Map[apiv1.VolumeMount, string](rcd.runSpec.VolumeMounts, func(mount apiv1.VolumeMount) string {
						return fmt.Sprintf("type=%s,src=%s", mount.Type, mount.Source)
					}), "\n"),
				})
			}

			startupStdoutWriter, startupStderrWriter := rcd.getStartupLogWriters()
			streamOptions := containers.StreamCommandOptions{
				StdOutStream: startupStdoutWriter,
				StdErrStream: startupStderrWriter,
			}
			creationOptions := containers.CreateContainerOptions{
				ContainerSpec:        *rcd.runSpec,
				Name:                 containerName,
				Network:              defaultNetwork,
				StreamCommandOptions: streamOptions,
			}
			inspected, err := createContainer(startupCtx, r.orchestrator, creationOptions)
			startupTaskFinished(startupStdoutWriter, startupStderrWriter)
			if err != nil {
				log.Error(err, "could not create the container")
				return err
			}

			log.V(1).Info("container created", "ContainerID", inspected.Id)
			rcd.updateFromInspectedContainer(inspected)

			if rcd.runSpec.Networks == nil {
				err = r.startContainerWithTimeout(startupCtx, rcd.containerID, streamOptions)
				rcd.startAttemptFinishedAt = metav1.NowMicro()
				startupTaskFinished(startupStdoutWriter, startupStderrWriter)
				if err != nil {
					log.Error(err, "could not start the container", "ContainerID", rcd.containerID)
					return err
				}

				log.V(1).Info("container started", "ContainerID", rcd.containerID)
				rcd.containerState = apiv1.ContainerStateRunning
			} else {
				// If a container resource is created without a network, it cannot be connected to a network later (orchestrator limitation).
				// So for Containers that request attaching to custom networks via Spec, we create the corresponding container resource
				// attached to default network(s) (usually one: "bridge" for Docker or "podman" for Podman).
				// Here we detach it from the default network(s). Then we leave the Container object in "starting" state,
				// and save the changes.
				//
				// During next reconciliation loop we create ContainerNetworkConnection objects and start the container resource.
				// The Network controller takes care of connecting the container resource to requested networks.
				if !container.Spec.Persistent {
					for i := range inspected.Networks {
						networkID := inspected.Networks[i].Id
						err = disconnectNetwork(startupCtx, r.orchestrator, containers.DisconnectNetworkOptions{Network: networkID, Container: rcd.containerID, Force: true})
						if err != nil {
							log.Error(err, "could not detach network from the container", "ContainerID", rcd.containerID, "Network", networkID)
							return err
						}
					}
				}
			}

			return nil
		}()

		if err != nil {
			rcd.startupError = err
			rcd.startAttemptFinishedAt = metav1.NowMicro()

			// Keep in "starting" state if the error is a transient error, otherwise initiate the transition to "failed to start".
			if !isTransientTemplateError(err) {
				rcd.containerState = apiv1.ContainerStateFailedToStart
			}
		}

		r.runningContainers.QueueDeferredOp(
			container.NamespacedName(),
			func(containerObjectName types.NamespacedName, containerID string) {
				r.runningContainers.UpdateChangingStateKey(containerObjectName, placeholderContainerID, rcd.containerID, rcd)
			},
		)
		r.scheduleContainerReconciliation(container.NamespacedName(), placeholderContainerID)
	}
}

// Returns a function that stops the container resource by calling the container orchestrator.
// The method is called as part of the reconciliation loop, but the returned function is executed asynchronously.
// The passed runningContainerData should be a clone independent from what is stored in the runningContainers map.
func (r *ContainerReconciler) stopContainerFunc(container *apiv1.Container, rcd *runningContainerData, log logr.Logger) func(context.Context) {
	return func(stopCtx context.Context) {
		log.V(1).Info("calling container orchestrator to stop the container...",
			"Container", container.NamespacedName().String(),
			"ContainerID", rcd.containerID,
		)
		err := r.stopContainerIfNecessary(stopCtx, rcd.containerID, nil, log)
		if err != nil {
			log.Error(err, "could not stop the running container corresponding to Container object",
				"Container", container.NamespacedName().String(),
				"ContainerID", rcd.containerID,
			)
			rcd.containerState = apiv1.ContainerStateUnknown
		} else {
			rcd.containerState = apiv1.ContainerStateExited
		}

		r.runningContainers.QueueDeferredOp(
			container.NamespacedName(),
			func(containerObjectName types.NamespacedName, containerID string) {
				r.runningContainers.Update(containerObjectName, containerID, rcd)
			},
		)
		r.scheduleContainerReconciliation(container.NamespacedName(), rcd.containerID)
	}

}

func (r *ContainerReconciler) deleteContainer(ctx context.Context, container *apiv1.Container, rcd *runningContainerData, log logr.Logger) {
	// This method is called only when we never attempted to start the container,
	// or if the container has already finished starting/stopping and we know the outcome of either.

	rcd.closeStartupLogFiles(log)
	defer rcd.deleteStartupLogFiles(log)

	if container.Spec.Persistent {
		log.V(1).Info("Container is not using Managed mode, leaving underlying resources")
		return
	}

	if !rcd.hasValidContainerID() {
		log.V(1).Info("container resource was never created, nothing to remove...")
		return
	}

	// We want to stop the container first to give it a chance to clean up
	_ = r.removeExistingContainer(ctx, rcd.containerID, nil, log)
}

// Removes all resources associated with the Container object, both DCP-managed, as well as orchestrator-managed,
// including the running container.
func (r *ContainerReconciler) cleanupContainerResources(ctx context.Context, container *apiv1.Container, rcd *runningContainerData, log logr.Logger) {
	r.cleanupDcpContainerResources(ctx, container, log)
	r.removeContainerNetworkConnections(ctx, container, log)
	r.deleteContainer(ctx, container, rcd, log)
}

// Removes any resources that DCP is managing for the running container.
// Does not attempt to remove the actual running container, or any orchestrator-managed resource.
func (r *ContainerReconciler) cleanupDcpContainerResources(ctx context.Context, container *apiv1.Container, log logr.Logger) {
	removeEndpointsForWorkload(r, ctx, container, log)
	r.releaseContainerWatch(container, log)
}

func (r *ContainerReconciler) startContainerWithTimeout(
	parentCtx context.Context,
	containerID string,
	streamOptions containers.StreamCommandOptions,
) error {
	startContainerCallCtx := parentCtx
	if r.config.ContainerStartupTimeoutOverride > 0 {
		var startContainerCallCtxCancel context.CancelFunc
		startContainerCallCtx, startContainerCallCtxCancel = context.WithTimeout(parentCtx, r.config.ContainerStartupTimeoutOverride)
		defer startContainerCallCtxCancel()
	}
	_, err := startContainer(startContainerCallCtx, r.orchestrator, containerID, streamOptions)
	return err
}

// NETWORKING SUPPORT METHODS

// Creates initial set of ContainerNetworkConnection objects for this Container, and if all connections are satisfied,
// starts the container.
// Returns a value indicating if the container has been started, and an error, if any.
// The error should be treated as permanent startup failure.
// This method is executed as part of the reconciliation loop.
func (r *ContainerReconciler) handleInitialNetworkConnections(
	ctx context.Context,
	container *apiv1.Container,
	rcd *runningContainerData,
	log logr.Logger,
) (bool, error) {
	connected, err := r.ensureContainerNetworkConnections(ctx, container, nil, rcd, log)
	if err != nil {
		return false, err
	}

	// Check to see if we are connected to all the ContainerNetworks listed in the Container object spec
	if len(connected) != len(*container.Spec.Networks) && !container.Spec.Stop && container.DeletionTimestamp.IsZero() {
		log.V(1).Info("container not connected to expected number of networks, scheduling additional reconciliation...", "ContainerID", rcd.containerID, "Expected", len(*container.Spec.Networks), "Connected", len(connected))
		return false, nil
	}

	containerID := rcd.containerID
	startupStdoutWriter, startupStderrWriter := rcd.getStartupLogWriters()
	streamOptions := containers.StreamCommandOptions{
		StdOutStream: startupStdoutWriter,
		StdErrStream: startupStderrWriter,
	}

	err = r.startContainerWithTimeout(ctx, containerID, streamOptions)
	startupTaskFinished(startupStdoutWriter, startupStderrWriter)
	if err != nil {
		log.Error(err, "failed to start Container", "ContainerID", containerID)
		return false, err
	}

	return true, nil
}

// Connects the Container to networks as necessary, updating status.
// This method is executed as part of the reconciliation loop.
func (r *ContainerReconciler) handleContainerNetworkConnections(
	ctx context.Context,
	container *apiv1.Container,
	inspected *containers.InspectedContainer,
	rcd *runningContainerData,
	log logr.Logger,
) ([]string, objectChange) {
	connected, connectionErr := r.ensureContainerNetworkConnections(ctx, container, inspected, rcd, log)
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
// The method is executed as part of the reconciliation loop.
func (r *ContainerReconciler) ensureContainerNetworkConnections(
	ctx context.Context,
	container *apiv1.Container,
	inspected *containers.InspectedContainer,
	rcd *runningContainerData,
	log logr.Logger,
) ([]*apiv1.ContainerNetwork, error) {
	var childNetworkConnections apiv1.ContainerNetworkConnectionList
	if err := r.List(ctx, &childNetworkConnections, ctrl_client.InNamespace(container.GetNamespace()), ctrl_client.MatchingFields{ownerKey: string(container.Name)}); err != nil {
		log.Error(err, "failed to list child ContainerNetworkConnection objects", "Container", container.NamespacedName().String())
		return []*apiv1.ContainerNetwork{}, err
	}

	if container.Spec.Networks == nil || container.Spec.Stop || !container.DeletionTimestamp.IsZero() {
		// If no networks are defined, stop has been requested, or the container is being deleted, delete all connections
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

	containerID := rcd.containerID

	if inspected == nil {
		if containerID == "" {
			err := fmt.Errorf("could not ensure ContainerNetworkConnections because the data about running container is missing")
			log.Error(err, "")
			return []*apiv1.ContainerNetwork{}, err
		}
		if i, err := inspectContainer(ctx, r.orchestrator, containerID); err != nil {
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
			delete(rcd.networkConnections, networkConnectionKey)
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

		_, found := rcd.networkConnections[networkConnectionKey]
		if found {
			// We should have already created a ContainerNetworkConnection object, wait for it to show up in a subsequent reconciliation
			continue
		}

		found = slices.Any(childNetworkConnections.Items, func(cnc apiv1.ContainerNetworkConnection) bool {
			namespacedConnection := asNamespacedName(cnc.Spec.ContainerNetworkName, container.Namespace)

			return namespacedConnection == namespacedNetworkName
		})

		if found {
			// Connection should exist
			continue
		}

		uniqueName, _, err := MakeUniqueName(fmt.Sprint(container.Name, "-", namespacedNetworkName.Name))
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
			// Do not log an error if resource cleanup has started.
			if !apiv1.ResourceCreationProhibited.Load() {
				log.Error(err, "could not persist ContainerNetworkConnection object",
					"Container", container.NamespacedName().String(),
					"Network", namespacedNetworkName.String(),
				)
			}
		} else {
			log.Info("Added new ContainerNetworkConnection", "Container", containerID, "ContainerNetworkConnection", connection.NamespacedName().String())
			rcd.networkConnections[networkConnectionKey] = true
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
	endpointName, _, err := MakeUniqueName(owner.GetName())
	if err != nil {
		log.Error(err, "could not generate unique name for Endpoint object")
		return nil, err
	}

	hostAddress, hostPort, err := r.getHostAddressAndPortForContainerPort(ctx, owner.(*apiv1.Container), serviceProducer.Port, log)
	if err != nil {
		log.Error(err, "could not determine host address and port for container port")
		return nil, err
	}

	if hostAddress == "" || hostAddress == networking.IPv4AllInterfaceAddress {
		hostAddress = networking.IPv4LocalhostDefaultAddress
	} else if hostAddress == networking.IPv6AllInterfaceAddress {
		hostAddress = networking.IPv6LocalhostDefaultAddress
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

	// Need to inspect the container to find the port used by the service (auto-allocated by Docker).
	_, rcd := r.runningContainers.BorrowByNamespacedName(ctr.NamespacedName())
	if rcd == nil || !rcd.hasValidContainerID() {
		// Should never happen--this method should only be called for running container.
		return "", 0, fmt.Errorf("running container data not found for Container '%s'", ctr.NamespacedName())
	}

	log.V(1).Info("inspecting running container resource to get its port information...", "ContainerID", rcd.containerID)
	inspected, err := inspectContainer(ctx, r.orchestrator, rcd.containerID)
	if err != nil {
		return "", 0, err
	}

	if inspected.Status != containers.ContainerStatusRunning {
		return "", 0, fmt.Errorf("container '%s' is not running: %s", inspected.Name, inspected.Status)
	}

	var matchedHostPort containers.InspectedContainerHostPortConfig
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

	r.containerEvtCh = concurrency.NewUnboundedChanBuffered[containers.EventMessage](
		r.lifetimeCtx,
		containerEventChanBuffer,
		containerEventChanBuffer,
	)
	r.networkEvtCh = concurrency.NewUnboundedChanBuffered[containers.EventMessage](
		r.lifetimeCtx,
		containerEventChanBuffer,
		containerEventChanBuffer,
	)

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
		r.containerEvtSub.Cancel()
		r.containerEvtSub = nil
	}
	if r.networkEvtSub != nil {
		r.networkEvtSub.Cancel()
		r.networkEvtSub = nil
	}
}

func (r *ContainerReconciler) containerEventWorker(
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

			r.processContainerEvent(cem)
		case nem, isOpen := <-networkEvtCh:
			if !isOpen {
				networkEvtCh = nil
				continue
			}

			if nem.Source != containers.EventSourceNetwork {
				continue
			}

			r.processNetworkEvent(nem)

		case <-stopCh:
			return
		}
	}
}

func (r *ContainerReconciler) processContainerEvent(em containers.EventMessage) {
	switch em.Action {
	// Any event that means the container has been started, stopped, or was removed, is interesting
	case containers.EventActionCreate, containers.EventActionDestroy, containers.EventActionDie, containers.EventActionDied, containers.EventActionKill, containers.EventActionOom, containers.EventActionStop, containers.EventActionRestart, containers.EventActionStart, containers.EventActionPrune:
		containerID := em.Actor.ID
		owner, rcd := r.runningContainers.BorrowByStateKey(containerID)
		if rcd == nil {
			// We are not tracking this container
			return
		}

		if r.Log.V(1).Enabled() {
			r.Log.V(1).Info("container event received, scheduling reconciliation", "ContainerID", containerID, "Event", em.String())
		}

		r.scheduleContainerReconciliation(owner, containerID)
	}
}

func (r *ContainerReconciler) processNetworkEvent(em containers.EventMessage) {
	switch em.Action {
	case containers.EventActionConnect, containers.EventActionDisconnect:
		containerID, found := em.Attributes["container"]
		if !found {
			// We could not identify the container this event applies to
			return
		}

		owner, rcd := r.runningContainers.BorrowByStateKey(containerID)
		if rcd == nil {
			// We are not tracking this container
			return
		}

		if r.Log.V(1).Enabled() {
			r.Log.V(1).Info("network event received, scheduling reconciliation", "ContainerID", containerID, "Event", em.String())
		}

		r.scheduleContainerReconciliation(owner, containerID)
	}
}

// MISCELLANEOUS HELPER METHODS

func setContainerState(container *apiv1.Container, state apiv1.ContainerState) objectChange {
	change := noChange
	healthStatus := getDefaultContainerHealthStatus(container, state)

	if container.Status.State != state {
		container.Status.State = state
		change = statusChanged
	}

	if container.Status.HealthStatus != healthStatus {
		container.Status.HealthStatus = healthStatus
		change = statusChanged
	}

	return change
}

func getDefaultContainerHealthStatus(ctr *apiv1.Container, state apiv1.ContainerState) apiv1.HealthStatus {
	switch state {
	case apiv1.ContainerStateEmpty, apiv1.ContainerStatePending, apiv1.ContainerStateBuilding, apiv1.ContainerStateStarting, apiv1.ContainerStatePaused, apiv1.ContainerStateUnknown, apiv1.ContainerStateStopping:
		return apiv1.HealthStatusCaution
	case apiv1.ContainerStateRunning:
		return apiv1.HealthStatusHealthy
	case apiv1.ContainerStateFailedToStart, apiv1.ContainerStateRuntimeUnhealthy:
		return apiv1.HealthStatusUnhealthy
	case apiv1.ContainerStateExited:
		if ctr.Status.ExitCode == apiv1.UnknownExitCode || *ctr.Status.ExitCode == 0 {
			return apiv1.HealthStatusCaution
		} else {
			return apiv1.HealthStatusUnhealthy
		}
	default:
		// This should never happen and would indicate we failed to account for some Container state.
		// Report the status as unhealthy, but do not panic. This should be pretty visible for clients and cause a bug report.
		return apiv1.HealthStatusUnhealthy
	}
}

func (r *ContainerReconciler) computeEffectiveEnvironment(
	ctx context.Context,
	ctr *apiv1.Container,
	acquiredRCD *runningContainerData,
	log logr.Logger,
) error {
	// Note: there is no value substitution by DCP for .env files, these are handled by container orchestrator directly.

	tmpl, err := newSpecValueTemplate(ctx, r, ctr, acquiredRCD.reservedPorts, log)
	if err != nil {
		return err
	}

	for i, envVar := range acquiredRCD.runSpec.Env {
		substitutionCtx := fmt.Sprintf("environment variable %s", envVar.Name)
		effectiveValue, templateErr := executeTemplate(tmpl, ctr, envVar.Value, substitutionCtx, log)
		if templateErr != nil {
			return templateErr
		}

		acquiredRCD.runSpec.Env[i] = apiv1.EnvVar{Name: envVar.Name, Value: effectiveValue}
	}

	return nil
}

func (r *ContainerReconciler) computeEffectiveInvocationArgs(
	ctx context.Context,
	ctr *apiv1.Container,
	acquiredRCD *runningContainerData,
	log logr.Logger,
) error {
	tmpl, err := newSpecValueTemplate(ctx, r, ctr, acquiredRCD.reservedPorts, log)
	if err != nil {
		return err
	}

	for i, arg := range acquiredRCD.runSpec.Args {
		substitutionCtx := fmt.Sprintf("argument %d", i)
		effectiveValue, templateErr := executeTemplate(tmpl, ctr, arg, substitutionCtx, log)
		if templateErr != nil {
			return templateErr
		}

		acquiredRCD.runSpec.Args[i] = effectiveValue
	}

	return nil
}

func (r *ContainerReconciler) scheduleContainerReconciliation(containerName types.NamespacedName, containerID string) {
	r.debouncer.ReconciliationNeeded(r.lifetimeCtx, containerName, containerID, r.doReconcileContainer)
}

func (r *ContainerReconciler) doReconcileContainer(rti reconcileTriggerInput[string]) {
	event := ctrl_event.GenericEvent{
		Object: &apiv1.Container{
			ObjectMeta: metav1.ObjectMeta{
				Name:      rti.target.Name,
				Namespace: rti.target.Namespace,
			},
		},
	}
	r.notifyContainerChanged.In <- event
}

func (r *ContainerReconciler) onShutdown() {
	<-r.lifetimeCtx.Done()

	r.lock.Lock()
	defer r.lock.Unlock()
	r.cancelContainerWatch()

	r.runningContainers.Range(func(containerName types.NamespacedName, containerID string, rcd *runningContainerData) bool {
		rcd.closeStartupLogFiles(r.Log)
		rcd.deleteStartupLogFiles(r.Log)
		_ = r.runningContainers.Update(containerName, containerID, rcd)
		return true
	})
	r.runningContainers.Clear()
}

func startupTaskFinished(writers ...usvc_io.ParagraphWriter) {
	for _, writer := range writers {
		if writer != nil {
			writer.NewParagraph()
		}
	}
}
