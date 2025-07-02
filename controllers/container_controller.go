// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	ctrl_handler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/health"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/pubsub"
	"github.com/microsoft/usvc-apiserver/internal/templating"
	"github.com/microsoft/usvc-apiserver/internal/version"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
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
	createFilesLabel  = "com.microsoft.developer.usvc-dev.createFiles"
)

var (
	containerFinalizer string = fmt.Sprintf("%s/container-reconciler", apiv1.GroupVersion.Group)
	containerKind             = apiv1.GroupVersion.WithKind(reflect.TypeOf(apiv1.Container{}).Name())

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

type runningContainersMap = ObjectStateMap[containerID, runningContainerData, *runningContainerData]

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

	// Health probe set used to execute health probes on containers.
	hpSet *health.HealthProbeSet
	// Channel used to receive health probe events
	healthProbeCh *concurrency.UnboundedChan[health.HealthProbeReport]

	// Debouncer used to schedule reconciliation. Extra data is the running container ID whose state changed.
	debouncer *reconcilerDebouncer[containerID]

	// Effectively a set of UIDs of the Container objects that are being watched.
	// When this set becomes empty, we cancel the container watch.
	watchingResources *syncmap.Map[types.UID, bool]

	// Lock to protect the reconciler data that requires synchronized access
	lock *sync.Mutex

	// Reconciler lifetime context, used to cancel container watch during reconciler shutdown
	lifetimeCtx context.Context

	// Additional configuration for the reconciler
	config ContainerReconcilerConfig
}

func NewContainerReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	log logr.Logger,
	orchestrator containers.ContainerOrchestrator,
	healthProbeSet *health.HealthProbeSet,
	config ContainerReconcilerConfig,
) *ContainerReconciler {
	r := ContainerReconciler{
		Client:                 client,
		orchestrator:           orchestrator,
		notifyContainerChanged: concurrency.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx),
		runningContainers:      NewObjectStateMap[containerID, runningContainerData](),
		startupQueue:           resiliency.NewWorkQueue(lifetimeCtx, config.MaxParallelContainerStarts),
		containerEvtSub:        nil,
		networkEvtSub:          nil,
		containerEvtCh:         nil,
		networkEvtCh:           nil,
		containerEvtWorkerStop: nil,
		hpSet:                  healthProbeSet,
		healthProbeCh:          concurrency.NewUnboundedChan[health.HealthProbeReport](lifetimeCtx),
		debouncer:              newReconcilerDebouncer[containerID](),
		watchingResources:      &syncmap.Map[types.UID, bool]{},
		lock:                   &sync.Mutex{},
		lifetimeCtx:            lifetimeCtx,
		config:                 config,
		Log:                    log,
	}

	go r.onShutdown()
	go r.handleHealthProbeResults()
	_, subErr := r.hpSet.Subscribe(r.healthProbeCh.In, containerKind)
	if subErr != nil {
		// Should never happen
		log.Error(subErr, "could not subscribe to health probe results, the health of Containers will never be correctly reported")
	}

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
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv1.Container{}).
		Owns(&apiv1.Endpoint{}).
		Owns(&apiv1.ContainerNetworkConnection{}).
		WatchesRawSource(src).
		Named(name).
		Complete(r)
}

func (r *ContainerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues(
		"Container", req.NamespacedName.String(),
		"Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1),
	)

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

	if container.Status.ContainerID != "" {
		log = log.WithValues("ContainerID", GetShortId(container.Status.ContainerID))
	}

	if container.Status.ContainerName != "" {
		log = log.WithValues("ContainerName", container.Status.ContainerName)
	} else if strings.TrimSpace(container.Spec.ContainerName) != "" {
		log = log.WithValues("ContainerName", strings.TrimSpace(container.Spec.ContainerName))
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

	reconciliationDelay, reconciliationJitter, finalChange := computeAdditionalReconciliationDelay(change, container.Status.State == apiv1.ContainerStateRuntimeUnhealthy)

	result, saveErr := saveChangesWithCustomReconciliationDelay(r.Client, ctx, &container, patch, finalChange, reconciliationDelay, reconciliationJitter, nil, log)
	return result, saveErr
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
		r.runningContainers.Update(container.NamespacedName(), containerID(rcd.containerID), rcd)
	}

	return change
}

func (r *ContainerReconciler) handleDeletionRequest(ctx context.Context, container *apiv1.Container, log logr.Logger) objectChange {
	_, rcd := r.runningContainers.BorrowByNamespacedName(container.NamespacedName())

	var change objectChange

	switch {
	case rcd == nil:
		log.V(1).Info("Container is being deleted (deleting finalizer only)...")
		change = deleteFinalizer(container, containerFinalizer, log)

	case rcd.containerState == apiv1.ContainerStateBuilding || rcd.containerState == apiv1.ContainerStateStarting || rcd.containerState == apiv1.ContainerStateStopping:
		log.V(1).Info("Container is being deleted, waiting for it to exit transient state...", "CurrentState", rcd.containerState)
		change = r.manageContainer(ctx, container, log)

	case (rcd.containerState == apiv1.ContainerStateRunning || rcd.containerState == apiv1.ContainerStatePaused) && !container.Spec.Persistent:
		log.V(1).Info("Container is being deleted, but it needs to be stopped first...", "CurrentState", rcd.containerState)
		rcd.containerState = apiv1.ContainerStateStopping
		stoppingInitializer := getStateInitializer(containerStateInitializers, apiv1.ContainerStateStopping, log)
		change = stoppingInitializer(ctx, r, container, apiv1.ContainerStateStopping, rcd, log)
		r.runningContainers.Update(container.NamespacedName(), rcd.containerID, rcd)

	default:
		log.V(1).Info("Container is being deleted (in initial or final state; releasing resources and deleting finalizer)...", "CurrentState", rcd.containerState)
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
	log logr.Logger,
) objectChange {
	change := noChange

	if container.Spec.Stop {
		// The container was started with a desired state of stopped, don't attempt to start it.
		return r.setContainerState(container, apiv1.ContainerStateFailedToStart)
	}

	status := r.orchestrator.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed)
	if !status.IsHealthy() {
		// If the runtime isn't healthy, we will attempt to start the container later (in case the runtime recovers).
		log.V(1).Info("container runtime is not healthy, retrying reconciliation later...")
		return r.setContainerState(container, apiv1.ContainerStateRuntimeUnhealthy)
	}

	lifecycleKey, hasDefaultLifecycleKey, hashErr := container.Spec.GetLifecycleKey()
	if hashErr != nil {
		log.Error(hashErr, "could not calculate lifecycle key")
		return change | additionalReconciliationNeeded
	}

	if container.Status.LifecycleKey == "" {
		container.Status.LifecycleKey = lifecycleKey
		change |= statusChanged
	}

	if container.Spec.Persistent {
		// Check for an existing persistent container
		inspected, inspectedErr := inspectContainerIfExists(ctx, r.orchestrator, container.Spec.ContainerName)
		if inspectedErr != nil && !errors.Is(inspectedErr, containers.ErrNotFound) {
			log.Error(inspectedErr, "could not inspect existing container")
			return change | additionalReconciliationNeeded
		}

		if inspected != nil {
			rcd := newRunningContainerData(container)
			rcd.updateFromInspectedContainer(inspected)

			log = log.WithValues("ContainerID", GetShortId(inspected.Id))

			_, dcpManaged := inspected.Labels[dcpBuildLabel]
			oldLifecycleKey, found := inspected.Labels[lifecycleKeyLabel]
			if dcpManaged && ((found && oldLifecycleKey != lifecycleKey) || (!found && lifecycleKey != "")) {
				// We need to recreate this DCP managed container because the lifecycle key has changed
				if hasDefaultLifecycleKey {
					mounts, ports, env, other := calculatePersistentContainerChanges(rcd, inspected)
					log.Info("found existing Container, but calculated lifecycle key doesn't match", "OldLifecycleKey", oldLifecycleKey, "NewLifecycleKey", lifecycleKey, "MountChanges", mounts, "PortChanges", ports, "EnvChanges", env, "OtherChanges", other)
				} else {
					log.Info("found existing Container, but custom lifecycle key doesn't match", "OldLifecycleKey", oldLifecycleKey, "NewLifecycleKey", lifecycleKey)
				}
			} else if dcpManaged && inspected.Status != containers.ContainerStatusRunning {
				log.V(1).Info("found existing Container that is not running", "ContainerStatus", inspected.Status)
			} else {
				log.Info("found existing Container")

				rcd.ensureStartupLogFiles(container, log)
				rcd.startAttemptFinishedAt = metav1.NewMicroTime(inspected.StartedAt)
				rcd.containerState = apiv1.ContainerStateRunning

				r.runningContainers.Store(container.NamespacedName(), rcd.containerID, rcd)
				r.ensureContainerWatch(container, log)

				change |= rcd.applyTo(container, log)

				return change | r.setContainerState(container, apiv1.ContainerStateRunning)
			}

			if container.ShouldStart() {
				log.V(1).Info("removing existing container")
				if removeErr := r.removeExistingContainer(ctx, containerID(inspected.Id), inspected, log); removeErr != nil {
					log.Error(removeErr, "could not remove existing container")
					return change | additionalReconciliationNeeded
				}
			}
		}
	}

	if !container.ShouldStart() {
		// We should wait to create a container until the user clears Start = false
		log.V(1).Info("waiting for the container to be started")
		return change
	}

	if container.Spec.Build != nil {
		// Container has a build context, so need to build it first.
		return change | r.setContainerState(container, apiv1.ContainerStateBuilding)
	} else {
		// Initiate startup sequence.
		return change | r.setContainerState(container, apiv1.ContainerStateStarting)
	}
}

func ensureContainerBuildingState(
	ctx context.Context,
	r *ContainerReconciler,
	container *apiv1.Container,
	_ apiv1.ContainerState,
	rcd *runningContainerData,
	log logr.Logger,
) objectChange {
	change := r.setContainerState(container, apiv1.ContainerStateBuilding)

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
	change |= rcd.applyTo(container, log)

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
	change := r.setContainerState(container, apiv1.ContainerStateStarting)

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

	} else if templating.IsTransientTemplateError(rcd.startupError) {
		// Retry startup after transient error.

		rcd.startupError = nil
		rcd.startAttemptFinishedAt = metav1.MicroTime{}
		rcd.containerName = ""

		r.scheduleContainerCreation(container, rcd, log, startupRetryDelay)
		change |= statusChanged

	} else if container.Spec.Networks != nil && rcd.hasValidContainerID() {
		// The second portion of startup sequence of a container with custom networks.
		// Need to create ContainerNetworkConnection objects and start the container resource.

		inspected, err := r.handleInitialNetworkConnections(ctx, container, rcd, log)

		switch {
		case err != nil:
			rcd.startupError = err
			rcd.containerState = apiv1.ContainerStateFailedToStart
			rcd.startAttemptFinishedAt = metav1.NowMicro()
			change |= statusChanged
		case inspected != nil && inspected.Status == containers.ContainerStatusRunning:
			rcd.containerState = apiv1.ContainerStateRunning
			rcd.startAttemptFinishedAt = metav1.NowMicro()
			change |= statusChanged
		case inspected != nil && inspected.Status == containers.ContainerStatusExited:
			rcd.containerState = apiv1.ContainerStateExited
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
	change |= rcd.applyTo(container, log)

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
	change := r.setContainerState(container, apiv1.ContainerStateFailedToStart)

	if rcd == nil {
		// Can happen if the container was created with Spec.Stop = true
		if container.Status.FinishTimestamp.IsZero() {
			container.Status.FinishTimestamp = metav1.NowMicro()
			change |= statusChanged
		}
	} else {
		change |= rcd.applyTo(container, log)
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
	if rcd == nil {
		// Should never happen--the runningContainers map should have the data about the Container object.
		log.Error(fmt.Errorf("the data about the container resource is missing (current container state is '%s')", desiredState), "")
		return ensureContainerUnknownState(ctx, r, container, desiredState, nil, log)
	}

	log.V(1).Info("inspecting container resource...")
	inspected, err := inspectContainer(ctx, r.orchestrator, string(rcd.containerID))
	if err != nil {
		if errors.Is(err, containers.ErrNotFound) {
			log.Info("container resource not found, assuming it was removed...")
			return ensureContainerUnknownState(ctx, r, container, desiredState, rcd, log)
		} else {
			log.Info("container resource could not be inspected", "Error", err.Error())
			// Could be a transient error, so for know we keep the rest of the status as-is.
			// Don't try to reconcile again unconditionally (that might result in an infinite loop),
			// but instead wait for another event from the container watcher.
			return noChange
		}
	}

	// We're able to inspect the container, so we can update the runningContainerData.
	// This lets us reconcile against the latest container state
	rcd.updateFromInspectedContainer(inspected)
	change := noChange

	if container.Spec.Stop {
		// Start the stopping sequence
		rcd.containerState = apiv1.ContainerStateStopping
		change |= r.setContainerState(container, apiv1.ContainerStateStopping)
		return change
	}

	if desiredState != apiv1.ContainerStateRunning {
		r.disableEndpointsAndHealthProbes(ctx, container, rcd, log)
	} else {
		if inspected.Status != containers.ContainerStatusRunning {
			log.V(1).Info("container is not running, delaying Endpoint creation...")
			change |= additionalReconciliationNeeded
		} else {
			r.enableEndpointsAndHealthProbes(ctx, container, rcd, inspected, log)
		}
	}

	change |= rcd.applyTo(container, log)

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
	change := r.setContainerState(container, apiv1.ContainerStateExited)

	if rcd == nil {
		// Should never happen--the runningContainers map should have the data about the Container object.
		log.Error(fmt.Errorf("the data about the container resource is missing (current container state is '%s')", apiv1.ContainerStateExited), "")
		return ensureContainerUnknownState(ctx, r, container, apiv1.ContainerStateUnknown, nil, log)
	}

	rcd.closeStartupLogFiles(log)
	r.disableEndpointsAndHealthProbes(ctx, container, rcd, log)

	log.V(1).Info("inspecting container resource...")
	inspected, err := inspectContainer(ctx, r.orchestrator, string(rcd.containerID))
	if err != nil {
		log.Info("container resource could not be inspected, might have been removed...", "Error", err.Error())
		return change // Best effort--skipping the rest.
	}

	rcd.updateFromInspectedContainer(inspected)
	change |= rcd.applyTo(container, log)

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
	change := r.setContainerState(container, apiv1.ContainerStateUnknown)
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
	change := r.setContainerState(container, apiv1.ContainerStateStopping)

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

	change |= rcd.applyTo(container, log)
	r.disableEndpointsAndHealthProbes(ctx, container, rcd, log)
	return change
}

// CONTAINER START/STOP HELPER METHODS

// Stops the container resource. If the container resource is already in one of the "effectively stopped"
// states (Exited, Stopped, or Dead), the method does nothing and returns no error.
// Returns containers.ErrNotFound if the container resource was not found.
// Returns an error if the container resource could not be stopped.
func (r *ContainerReconciler) stopContainerIfNecessary(
	ctx context.Context,
	containerID containerID,
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
		inspected, inspectErr = inspectContainer(ctx, r.orchestrator, string(containerID))
		if inspectErr != nil {
			if errors.Is(inspectErr, containers.ErrNotFound) {
				log.Info("container resource not found, assuming it was removed... ")
				return containers.ErrNotFound
			}
		}
	}

	if needsStopping() {
		log.V(1).Info("calling container orchestrator to stop the existing container...")
		stopErr := stopContainer(ctx, r.orchestrator, string(containerID))
		if stopErr != nil {
			log.Error(stopErr, "could not stop the running container")
		}
		return stopErr
	}

	return nil
}

func (r *ContainerReconciler) removeExistingContainer(
	ctx context.Context,
	containerID containerID,
	inspected *containers.InspectedContainer,
	log logr.Logger,
) error {
	stopErr := r.stopContainerIfNecessary(ctx, containerID, inspected, log)
	if errors.Is(stopErr, containers.ErrNotFound) {
		// Already logged (at info level) by stopExistingContainer()
		return nil // Nothing to do
	}

	// Try to remove the container even if stop error occurred.
	removeErr := removeContainer(ctx, r.orchestrator, string(containerID))
	if removeErr != nil {
		// Log any unexpected error, but attempt to continue with creation of the new container
		log.Error(removeErr, "could not remove the running container")
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

		log = log.WithValues("ContainerName", containerName)
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
			log.V(1).Info("building image", "Dockerfile", container.Spec.Build.Dockerfile, "Context", container.Spec.Build.Context)

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
				Pull:                  container.Spec.PullPolicy == apiv1.PullPolicyAlways,
				ContainerBuildContext: rcd.runSpec.Build,
			}

			startupStdoutWriter, startupStderrWriter := rcd.getStartupLogWriters()
			buildOptions.StreamCommandOptions = containers.StreamCommandOptions{
				StdOutStream: startupStdoutWriter,
				StdErrStream: startupStderrWriter,
			}

			buildErr := r.orchestrator.BuildImage(buildCtx, buildOptions)
			startupTaskFinished(startupStdoutWriter, startupStderrWriter)
			if buildErr != nil {
				log.Error(buildErr, "could not build the image")
				return buildErr
			}

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
			func(containerObjectName types.NamespacedName, id containerID) {
				r.runningContainers.Update(containerObjectName, id, rcd)
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

	if createFilesLabelHash, found := inspected.Labels[createFilesLabel]; found && len(rcd.runSpec.CreateFiles) > 0 {
		specCreateFilesHash := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%v", rcd.runSpec.CreateFiles))))

		if createFilesLabelHash != specCreateFilesHash {
			changeList = append(changeList, "container create files entries changed")
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
				if templating.IsTransientTemplateError(err) {
					log.Info("could not compute effective environment for the Container, retrying startup...", "Cause", err.Error())
				} else {
					log.Error(err, "could not compute effective environment for the Container")
				}

				return err
			}

			err = r.computeEffectiveInvocationArgs(startupCtx, container, rcd, log)
			if err != nil {
				if templating.IsTransientTemplateError(err) {
					log.Info("could not compute effective invocation arguments for the Container, retrying startup...", "Cause", err.Error())
				} else {
					log.Error(err, "could not compute effective invocation arguments for the Container")
				}

				return err
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

			log.V(1).Info("starting container", "Image", container.SpecifiedImageNameOrDefault())

			defaultNetwork := ""
			if rcd.runSpec.Networks != nil {
				// See comment below why we create the container with default network explicitly enabled here.
				defaultNetwork = r.orchestrator.DefaultNetworkName()
			}

			lifecycleKey, _, hashErr := rcd.runSpec.GetLifecycleKey()
			if hashErr != nil {
				log.Error(hashErr, "could not compute lifecycle key for the container")
				return hashErr
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

			if len(rcd.runSpec.CreateFiles) > 0 {
				rcd.runSpec.Labels = append(rcd.runSpec.Labels, apiv1.ContainerLabel{
					Key:   createFilesLabel,
					Value: fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%v", rcd.runSpec.CreateFiles)))),
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

			log = log.WithValues("ContainerID", GetShortId(inspected.Id))

			log.V(1).Info("container created")
			rcd.updateFromInspectedContainer(inspected)

			for _, createFileRequest := range rcd.runSpec.CreateFiles {
				umask := osutil.DefaultUmaskBitmask
				if createFileRequest.Umask != nil {
					umask = *createFileRequest.Umask
				}

				createFilesOptions := containers.CreateFilesOptions{
					Container:    inspected.Id,
					Entries:      createFileRequest.Entries,
					Destination:  createFileRequest.Destination,
					DefaultOwner: createFileRequest.DefaultOwner,
					DefaultGroup: createFileRequest.DefaultGroup,
					Umask:        umask,
					ModTime:      time.Now(),
				}

				copyErr := r.orchestrator.CreateFiles(startupCtx, createFilesOptions)
				if copyErr != nil {
					log.Error(copyErr, "could not copy files to the container", "Destination", createFileRequest.Destination)
					return copyErr
				}

				log.V(1).Info("files copied to the container", "Destination", createFileRequest.Destination)
			}

			if rcd.runSpec.Networks == nil {
				inspected, err = r.startContainerWithTimeout(startupCtx, containerName, rcd.containerID, streamOptions)
				rcd.startAttemptFinishedAt = metav1.NowMicro()
				startupTaskFinished(startupStdoutWriter, startupStderrWriter)
				if err != nil {
					log.Error(err, "could not start the container")
					return err
				}

				if inspected.Status == containers.ContainerStatusRunning {
					log.V(1).Info("container started")
					rcd.containerState = apiv1.ContainerStateRunning
				} else {
					log.V(1).Info("container started and exited shortly after", "ContainerStatus", inspected.Status)
					rcd.containerState = apiv1.ContainerStateExited
				}
			} else {
				// If a container resource is created without a network, it cannot be connected to a network later (orchestrator limitation).
				// So for Containers that request attaching to custom networks via Spec, we create the corresponding container resource
				// attached to default network(s) (usually one: "bridge" for Docker or "podman" for Podman).
				// Here we detach it from the default network(s). Then we leave the Container object in "starting" state,
				// and save the changes.
				//
				// During next reconciliation loop we create ContainerNetworkConnection objects and start the container resource.
				// The Network controller takes care of connecting the container resource to requested networks.
				for i := range inspected.Networks {
					networkID := inspected.Networks[i].Id
					err = disconnectNetwork(startupCtx, r.orchestrator, containers.DisconnectNetworkOptions{Network: networkID, Container: string(rcd.containerID), Force: true})
					if err != nil {
						log.Error(err, "could not detach network from the container", "NetworkID", networkID)
						return err
					}
				}
			}

			return nil
		}()

		if err != nil {
			rcd.startupError = err
			rcd.startAttemptFinishedAt = metav1.NowMicro()

			// Keep in "starting" state if the error is a transient error, otherwise initiate the transition to "failed to start".
			if !templating.IsTransientTemplateError(err) {
				rcd.containerState = apiv1.ContainerStateFailedToStart
			}
		}

		r.runningContainers.QueueDeferredOp(
			container.NamespacedName(),
			func(containerObjectName types.NamespacedName, containerID containerID) {
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
		log.V(1).Info("calling container orchestrator to stop the container...")
		err := r.stopContainerIfNecessary(stopCtx, rcd.containerID, nil, log)
		if err != nil {
			log.Error(err, "could not stop the running container corresponding to Container object")
			rcd.containerState = apiv1.ContainerStateUnknown
		} else {
			rcd.containerState = apiv1.ContainerStateExited
		}

		r.runningContainers.QueueDeferredOp(
			container.NamespacedName(),
			func(containerObjectName types.NamespacedName, containerID containerID) {
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
	r.disableEndpointsAndHealthProbes(ctx, container, nil, log)
	r.releaseContainerWatch(container, log)
}

func (r *ContainerReconciler) startContainerWithTimeout(
	parentCtx context.Context,
	containerObjectName string,
	containerID containerID,
	streamOptions containers.StreamCommandOptions,
) (*containers.InspectedContainer, error) {
	startContainerCallCtx := parentCtx
	if r.config.ContainerStartupTimeoutOverride > 0 {
		var startContainerCallCtxCancel context.CancelFunc
		startContainerCallCtx, startContainerCallCtxCancel = context.WithTimeout(parentCtx, r.config.ContainerStartupTimeoutOverride)
		defer startContainerCallCtxCancel()
	}
	inspected, err := startContainer(startContainerCallCtx, r.orchestrator, containerObjectName, string(containerID), streamOptions)
	return inspected, err
}

func (r *ContainerReconciler) disableEndpointsAndHealthProbes(
	ctx context.Context,
	ctr *apiv1.Container,
	rcd *runningContainerData,
	log logr.Logger,
) {
	if len(ctr.Spec.HealthProbes) > 0 && (rcd == nil || pointers.TrueValue(rcd.healthProbesEnabled)) {
		log.V(1).Info("disabling health probes for Container...")
		r.hpSet.DisableProbes(ctr)
		if rcd != nil {
			pointers.Make(&rcd.healthProbesEnabled, false)
		}
	}

	removeEndpointsForWorkload(r, ctx, ctr, log)
}

func (r *ContainerReconciler) enableEndpointsAndHealthProbes(
	ctx context.Context,
	ctr *apiv1.Container,
	rcd *runningContainerData,
	inspected *containers.InspectedContainer,
	log logr.Logger,
) {
	if len(ctr.Spec.HealthProbes) > 0 && pointers.NotTrue(rcd.healthProbesEnabled) {
		log.V(1).Info("enabling health probes for Container...")
		pointers.Make(&rcd.healthProbesEnabled, true)
		probeErr := r.hpSet.EnableProbes(ctr, ctr.Spec.HealthProbes)
		if probeErr != nil {
			// Should never happen (unless lifetime context is cancelled, but we should not be here in that case).
			log.Error(probeErr, "could not enable health probes for Container")
		}
	}
	ensureEndpointsForWorkload(ctx, r, ctr, nil, inspected, log)
}

// NETWORKING SUPPORT METHODS

// Creates initial set of ContainerNetworkConnection objects for this Container,
// and if all connections are satisfied, starts the container.
// Returns the inspected container data (if the container has been started successfully), and an error, if any.
// The error should be treated as permanent startup failure. If a startup error is returned, the inspected container data
// might be missing.
// This method is executed as part of the reconciliation loop.
func (r *ContainerReconciler) handleInitialNetworkConnections(
	ctx context.Context,
	container *apiv1.Container,
	rcd *runningContainerData,
	log logr.Logger,
) (*containers.InspectedContainer, error) {
	connected, connectionErr := r.ensureContainerNetworkConnections(ctx, container, nil, rcd, log)
	if connectionErr != nil {
		return nil, connectionErr
	}

	// Check to see if we are connected to all the ContainerNetworks listed in the Container object spec
	if len(connected) != len(*container.Spec.Networks) && !container.Spec.Stop && container.DeletionTimestamp.IsZero() {
		log.V(1).Info("container not connected to expected number of networks, scheduling additional reconciliation...", "Expected", len(*container.Spec.Networks), "Connected", len(connected))
		return nil, nil
	}

	containerID := rcd.containerID
	startupStdoutWriter, startupStderrWriter := rcd.getStartupLogWriters()
	streamOptions := containers.StreamCommandOptions{
		StdOutStream: startupStdoutWriter,
		StdErrStream: startupStderrWriter,
	}

	inspected, startupErr := r.startContainerWithTimeout(ctx, container.Name, containerID, streamOptions)
	startupTaskFinished(startupStdoutWriter, startupStderrWriter)
	if startupErr != nil {
		log.Error(startupErr, "failed to start Container")
		return nil, startupErr
	}

	return inspected, nil
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
	if container.Spec.Persistent {
		return
	}

	var childNetworkConnections apiv1.ContainerNetworkConnectionList
	if err := r.List(ctx, &childNetworkConnections, ctrl_client.InNamespace(container.GetNamespace()), ctrl_client.MatchingFields{ownerKey: string(container.Name)}); err != nil {
		log.Error(err, "failed to list child ContainerNetworkConnection objects")
		return
	}

	for i := range childNetworkConnections.Items {
		if err := r.Delete(ctx, &childNetworkConnections.Items[i], ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground)); ctrl_client.IgnoreNotFound(err) != nil {
			log.Error(err, "could not delete ContainerNetworkConnection object", "ContainerNetworkConnection", childNetworkConnections.Items[i].NamespacedName().String())
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
		log.Error(err, "failed to list child ContainerNetworkConnection objects")
		return []*apiv1.ContainerNetwork{}, err
	}

	if container.Spec.Networks == nil || container.Spec.Stop || !container.DeletionTimestamp.IsZero() || rcd.containerState == apiv1.ContainerStateFailedToStart {
		// Delete all connections if no networks are defined, stop has been requested, or the container is being deleted.
		//
		// We also delete all connections if the container fails to start, because the container orchestrator (Docker/Podman)
		// may not be able to attach a "dead" container to a network. Specifically, in dead state, inspecting the container
		// may result in NetworkSetting populated with desired network information, but inspecting the network will reveal
		// that the container is not connected to the network. Our ContainerNetwork controller is using network inspection
		// to determine if the container is connected to the network, so it will forever try to re-attach it, unsuccessfully,
		// if the ContainerNetworkConnection object is not deleted.
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
		if i, err := inspectContainer(ctx, r.orchestrator, string(containerID)); err != nil {
			log.Error(err, "could not inspect the container")
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
			return commonapi.AsNamespacedName(cnc.Spec.ContainerNetworkName, container.GetNamespace()) == network.NamespacedName()
		})

		if index >= 0 {
			expectedNetworks = append(expectedNetworks, network)
		}
	}

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
			Network:   commonapi.AsNamespacedName(connection.Name, container.Namespace),
		}

		found := slices.Any(*container.Spec.Networks, func(network apiv1.ContainerNetworkConnectionConfig) bool {
			return commonapi.AsNamespacedName(network.Name, container.GetNamespace()) == commonapi.AsNamespacedName(connection.Spec.ContainerNetworkName, container.GetNamespace())
		})

		if found {
			continue
		}

		if err := r.Delete(ctx, &connection, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground)); ctrl_client.IgnoreNotFound(err) != nil {
			log.Error(err, "could not delete ContainerNetworkConnection object", "Network", connection.Spec.ContainerNetworkName)
		} else {
			log.Info("Removed a ContainerNetworkConnection connection", "Network", connection.Spec.ContainerNetworkName)
			delete(rcd.networkConnections, networkConnectionKey)
		}
	}

	// Create new ContainerNetworkConnections for any expected networks that don't have one
	for i := range *container.Spec.Networks {
		network := (*container.Spec.Networks)[i]

		namespacedNetworkName := commonapi.AsNamespacedName(network.Name, container.Namespace)

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
			namespacedConnection := commonapi.AsNamespacedName(cnc.Spec.ContainerNetworkName, container.Namespace)

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
				ContainerID:          string(containerID),
				Aliases:              network.Aliases,
			},
		}

		if err = ctrl.SetControllerReference(container, connection, r.Scheme()); err != nil {
			log.Error(err, "failed to set owner for network connection",
				"Network", namespacedNetworkName.String(),
			)
		}

		if err = r.Create(ctx, connection); err != nil {
			// Do not log an error if resource cleanup has started.
			if !apiv1.ResourceCreationProhibited.Load() {
				log.Error(err, "could not persist ContainerNetworkConnection object",
					"Network", namespacedNetworkName.String(),
				)
			}
		} else {
			log.Info("Added new ContainerNetworkConnection", "Network", namespacedNetworkName.String())
			rcd.networkConnections[networkConnectionKey] = true
		}
	}

	return validConnectedNetworks, nil
}

func (r *ContainerReconciler) createEndpoint(
	ctx context.Context,
	owner ctrl_client.Object,
	serviceProducer commonapi.ServiceProducer,
	inspected *containers.InspectedContainer,
	log logr.Logger,
) (*apiv1.Endpoint, error) {
	endpointName, _, err := MakeUniqueName(owner.GetName())
	if err != nil {
		log.Error(err, "could not generate unique name for Endpoint object")
		return nil, err
	}

	hostAddress, hostPort, err := r.getHostAddressAndPortForContainerPort(owner.(*apiv1.Container), serviceProducer.Port, inspected, log)
	if err != nil {
		log.Error(err, "could not determine host address and port for container port")
		return nil, err
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

func (r *ContainerReconciler) validateExistingEndpoint(
	ctx context.Context,
	owner ctrl_client.Object,
	serviceProducer commonapi.ServiceProducer,
	inspected *containers.InspectedContainer,
	endpoint *apiv1.Endpoint,
	log logr.Logger,
) error {
	hostAddress, hostPort, err := r.getHostAddressAndPortForContainerPort(owner.(*apiv1.Container), serviceProducer.Port, inspected, log)
	if err != nil {
		log.Error(err, "could not determine host address and port for container port")
		return err
	}

	if endpoint.Spec.Address != hostAddress || endpoint.Spec.Port != hostPort {
		log.V(1).Info("Existing Endpoint does not match host address and port derived from inspected Container port information", "EffectiveHostAddress", hostAddress, "HostPort", hostPort, "EndpointAddress", endpoint.Spec.Address, "EndpointPort", endpoint.Spec.Port)
		return fmt.Errorf("endpoint configuration does not match inspected Container port information")
	}

	return nil
}

func (r *ContainerReconciler) getHostAddressAndPortForContainerPort(
	ctr *apiv1.Container,
	serviceProducerPort int32,
	inspected *containers.InspectedContainer,
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
		hostAddress := normalizeHostAddress(matchedPort.HostIP)
		// If the spec contains a port matching the desired container port, just use that
		log.V(1).Info("found matching port in Container spec", "ServiceProducerPort", serviceProducerPort, "HostPort", matchedPort.HostPort, "HostIP", matchedPort.HostIP, "EffectiveHostAddress", hostAddress)
		return hostAddress, matchedPort.HostPort, nil
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

	hostAddress := normalizeHostAddress(matchedHostPort.HostIp)
	log.V(1).Info("matched service producer port to one of the container host ports", "ServiceProducerPort", serviceProducerPort, "HostPort", hostPort, "HostIP", matchedHostPort.HostIp, "EffectiveHostAddress", hostAddress)
	return hostAddress, int32(hostPort), nil
}

func normalizeHostAddress(hostIP string) string {
	hostAddress := hostIP

	switch hostAddress {
	case "", networking.IPv4AllInterfaceAddress:
		hostAddress = networking.IPv4LocalhostDefaultAddress
	case networking.IPv6AllInterfaceAddress:
		hostAddress = networking.IPv6LocalhostDefaultAddress
	}

	return hostAddress
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
	case containers.EventActionCreate, containers.EventActionDestroy, containers.EventActionDie, containers.EventActionDied, containers.EventActionKill, containers.EventActionOom, containers.EventActionStop, containers.EventActionRestart, containers.EventActionStart, containers.EventActionPrune, containers.EventActionExecDie, containers.EventActionHealthStatus:
		containerID := containerID(em.Actor.ID)
		owner, rcd := r.runningContainers.BorrowByStateKey(containerID)
		if rcd == nil {
			// We are not tracking this container
			return
		}

		if r.Log.V(1).Enabled() {
			r.Log.V(1).Info("container event received, scheduling reconciliation", "ContainerID", GetShortId(string(containerID)), "Event", em.String())
		}

		r.scheduleContainerReconciliation(owner, containerID)
	}
}

func (r *ContainerReconciler) processNetworkEvent(em containers.EventMessage) {
	switch em.Action {
	case containers.EventActionConnect, containers.EventActionDisconnect:
		val, found := em.Attributes["container"]
		if !found {
			// We could not identify the container this event applies to
			return
		}
		containerID := containerID(val)

		owner, rcd := r.runningContainers.BorrowByStateKey(containerID)
		if rcd == nil {
			// We are not tracking this container
			return
		}

		if r.Log.V(1).Enabled() {
			r.Log.V(1).Info("network event received, scheduling reconciliation", "ContainerID", GetShortId(string(containerID)), "Event", em.String())
		}

		r.scheduleContainerReconciliation(owner, containerID)
	}
}

// MISCELLANEOUS HELPER METHODS

func (r *ContainerReconciler) setContainerState(container *apiv1.Container, state apiv1.ContainerState) objectChange {
	change := noChange

	if container.Status.State != state {
		container.Status.State = state
		change = statusChanged
	}

	change |= updateContainerHealthStatus(container, state, r.Log)

	return change
}

func updateContainerHealthStatus(ctr *apiv1.Container, state apiv1.ContainerState, log logr.Logger) objectChange {
	var newHealthStatus apiv1.HealthStatus

	switch state {

	case apiv1.ContainerStateEmpty, apiv1.ContainerStatePending, apiv1.ContainerStateBuilding, apiv1.ContainerStateStarting, apiv1.ContainerStatePaused, apiv1.ContainerStateUnknown, apiv1.ContainerStateStopping:
		newHealthStatus = apiv1.HealthStatusCaution

	case apiv1.ContainerStateRunning:
		if len(ctr.Spec.HealthProbes) == 0 && len(ctr.Status.HealthProbeResults) == 0 {
			// The container has no configured health probes and no results (i.e. no configured healthcheck)
			newHealthStatus = apiv1.HealthStatusHealthy
		} else {
			newHealthStatus = health.HealthStatusFromProbeResults(ctr.Status.HealthProbeResults)
		}

	case apiv1.ContainerStateFailedToStart, apiv1.ContainerStateRuntimeUnhealthy:
		newHealthStatus = apiv1.HealthStatusUnhealthy

	case apiv1.ContainerStateExited:
		if ctr.Status.ExitCode == apiv1.UnknownExitCode || *ctr.Status.ExitCode == 0 {
			newHealthStatus = apiv1.HealthStatusCaution
		} else {
			newHealthStatus = apiv1.HealthStatusUnhealthy
		}

	default:
		// This should never happen and would indicate we failed to account for some Container state.
		// Report the status as unhealthy, but do not panic. This should be pretty visible for clients and cause a bug report.
		newHealthStatus = apiv1.HealthStatusUnhealthy
	}

	if ctr.Status.HealthStatus == newHealthStatus {
		return noChange
	}

	log.V(1).Info("Container health status changed",
		"NewHealthStatus", newHealthStatus,
		"OldHealthStatus", ctr.Status.HealthStatus,
	)
	ctr.Status.HealthStatus = newHealthStatus
	return statusChanged
}

func (r *ContainerReconciler) handleHealthProbeResults() {
	for {
		select {
		case <-r.lifetimeCtx.Done():
			return

		case report, isOpen := <-r.healthProbeCh.Out:
			if !isOpen {
				return
			}

			if report.Owner.Kind != containerKind {
				r.Log.Error(fmt.Errorf("container reconciler received health probe report for some other type of object"), "", "Kind", report.Owner.Kind)
				continue
			}

			containerName := report.Owner.NamespacedName
			cid, rcd := r.runningContainers.BorrowByNamespacedName(containerName)
			if rcd == nil {
				// Not tracking this Container anymore, most likely Container was deleted and we got a stale report.
				// We disable probes when the Container reaches final state AND just before removing the finalizer,
				// so it is very unlikely any Container health probes will go orphaned long-term.
				// See ExecutableReconciler.handleHealthProbeResults() for why calling DisableProbes(containerName) here
				// is not a good idea.

				continue
			}

			rcd.healthProbeResults[report.Probe.Name] = report.Result

			r.runningContainers.QueueDeferredOp(containerName, func(types.NamespacedName, containerID) {
				// The run may have been deleted by the time we get here, so we do not care if Update() returns false.
				_ = r.runningContainers.Update(containerName, cid, rcd)
			})

			r.scheduleContainerReconciliation(containerName, cid)
		}
	}
}

func (r *ContainerReconciler) computeEffectiveEnvironment(
	ctx context.Context,
	ctr *apiv1.Container,
	acquiredRCD *runningContainerData,
	log logr.Logger,
) error {
	// Note: there is no value substitution by DCP for .env files, these are handled by container orchestrator directly.

	tmpl, err := templating.NewSpecValueTemplate(ctx, r, ctr, nil, log)
	if err != nil {
		return err
	}

	for i, envVar := range acquiredRCD.runSpec.Env {
		substitutionCtx := fmt.Sprintf("environment variable %s", envVar.Name)
		effectiveValue, templateErr := templating.ExecuteTemplate(tmpl, ctr, envVar.Value, substitutionCtx, log)
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
	tmpl, err := templating.NewSpecValueTemplate(ctx, r, ctr, nil, log)
	if err != nil {
		return err
	}

	for i, arg := range acquiredRCD.runSpec.Args {
		substitutionCtx := fmt.Sprintf("argument %d", i)
		effectiveValue, templateErr := templating.ExecuteTemplate(tmpl, ctr, arg, substitutionCtx, log)
		if templateErr != nil {
			return templateErr
		}

		acquiredRCD.runSpec.Args[i] = effectiveValue
	}

	return nil
}

func (r *ContainerReconciler) scheduleContainerReconciliation(containerName types.NamespacedName, id containerID) {
	r.debouncer.ReconciliationNeeded(r.lifetimeCtx, containerName, id, r.doReconcileContainer)
}

func (r *ContainerReconciler) doReconcileContainer(rti reconcileTriggerInput[containerID]) {
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

	r.runningContainers.Range(func(containerName types.NamespacedName, id containerID, rcd *runningContainerData) bool {
		rcd.closeStartupLogFiles(r.Log)
		rcd.deleteStartupLogFiles(r.Log)
		_ = r.runningContainers.Update(containerName, id, rcd)
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
