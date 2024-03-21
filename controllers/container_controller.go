// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
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
	ct "github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
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

// Data that we keep, in memory, about running containers.
type runningContainerData struct {
	// This is the container startup error if container start fails.
	startupError error

	// If the container starts successfully, this is the container ID from the container orchestrator.
	newContainerID string

	// The time the start attempt finished (successfully or not).
	startAttemptFinishedAt metav1.Time

	// The map of ports reserved for services that the Container implements
	reservedPorts map[types.NamespacedName]int32

	// The "run spec" that was used to start the container, after all environment variable and argument substitutions.
	runSpec *apiv1.ContainerSpec
}

func newRunningContainerData(ctr *apiv1.Container) *runningContainerData {
	return &runningContainerData{
		newContainerID: getFailedContainerID(ctr.NamespacedName()),
		reservedPorts:  make(map[types.NamespacedName]int32),
		runSpec:        ctr.Spec.DeepCopy(),
	}
}

type containerNetworkConnectionKey struct {
	Container types.NamespacedName
	Network   types.NamespacedName
}

type ContainerReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	orchestrator        ct.ContainerOrchestrator

	// Channel used to trigger reconciliation when underlying containers change
	notifyContainerChanged *chanx.UnboundedChan[ctrl_event.GenericEvent]

	// A map that stores information about running containers,
	// searchable by container ID (first key), or Container object name (second key).
	// Usually both keys are valid, but when the container is starting, we do not have the real container ID yet,
	// so we use a temporary random string that is replaced by real container ID once we know the container outcome.
	runningContainers *maps.SynchronizedDualKeyMap[string, types.NamespacedName, *runningContainerData]

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
		runningContainers:      maps.NewSynchronizedDualKeyMap[string, types.NamespacedName, *runningContainerData](),
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
	src := ctrl_source.Channel{
		Source: r.notifyContainerChanged.Out,
	}

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

	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Container{}).
		Owns(&apiv1.Endpoint{}).
		Owns(&apiv1.ContainerNetworkConnection{}).
		WatchesRawSource(&src, &ctrl_handler.EnqueueRequestForObject{}).
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
			log.V(1).Info("the Container object was deleted")
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

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(container.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	deletionRequested := container.DeletionTimestamp != nil && !container.DeletionTimestamp.IsZero()
	if deletionRequested && container.Status.State != apiv1.ContainerStateStarting {
		// Note: if the Container object is being deleted, but the correspoinding container is in the process of starting,
		// we need the container startup to finish, before attemptin to delete everything.
		// Otherwise we will be left with a dangling container that no one owns.
		log.V(1).Info("Container object is being deleted")
		r.deleteContainer(ctx, &container, log)
		change = deleteFinalizer(&container, containerFinalizer, log)
		r.releaseContainerWatch(&container, log)
		r.removeContainerNetworkConnections(ctx, &container, log)
		removeEndpointsForWorkload(r, ctx, &container, log)
	} else {
		change = ensureFinalizer(&container, containerFinalizer, log)

		// If we added a finalizer, we'll do the additional reconciliation next call
		if change == noChange {
			change = r.manageContainer(ctx, &container, log)

			switch container.Status.State {
			case apiv1.ContainerStateRunning:
				_, runData, found := r.runningContainers.FindBySecondKey(container.NamespacedName())
				if !found {
					// Should never happen
					log.Error(fmt.Errorf("missing running container data"), "", "ContainerID", container.Status.ContainerID, "ContainerState", container.Status.State)
				} else {
					ensureEndpointsForWorkload(ctx, r, &container, runData.reservedPorts, log)
				}

			case apiv1.ContainerStatePending, apiv1.ContainerStateStarting:
				break // do nothing

			default:
				removeEndpointsForWorkload(r, ctx, &container, log)
			}
		}
	}

	result, err := saveChanges(r, ctx, &container, patch, change, nil, log)
	return result, err
}

func (r *ContainerReconciler) stopContainer(ctx context.Context, container *apiv1.Container, log logr.Logger) {
	// This method is called only when we never attempted to start the container,
	// or if the has already finished starting and we know the outcome.

	containerID, _, found := r.runningContainers.FindBySecondKey(container.NamespacedName())
	if !found {
		log.V(1).Info("running container data is not available; nothing to stop")
		return
	}

	log.V(1).Info("calling container orchestrator to stop the container...", "ContainerID", containerID)
	_, err := r.orchestrator.StopContainers(ctx, []string{containerID}, stopContainerTimeoutSeconds)
	if err != nil {
		log.Error(err, "could not stop the running container corresponding to Container object", "ContainerID", containerID)
	}
}

func (r *ContainerReconciler) deleteContainer(ctx context.Context, container *apiv1.Container, log logr.Logger) {
	// This method is called only when we never attempted to start the container,
	// or if the container has already finished starting and we know the outcome.

	containerID, _, found := r.runningContainers.FindBySecondKey(container.NamespacedName())
	if !found {
		// We either never started the container, or we already attempted to remove the container
		// and the current reconciliation call is just the cache catching up.
		// Either way there is nothing to do.
		log.V(1).Info("running container data is not available; proceeding with Container object deletion...")
		return
	}

	// Since the container is being removed, we want to remove it from runningContainers map now
	r.runningContainers.DeleteBySecondKey(container.NamespacedName())

	if !container.Spec.Persistent {
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
	} else {
		log.V(1).Info("Container is not using Managed mode, leaving underlying resources")
	}
}

func (r *ContainerReconciler) handleInitialNetworkConnections(ctx context.Context, container *apiv1.Container, rcd *runningContainerData, log logr.Logger) error {
	connected, err := r.ensureContainerNetworkConnections(ctx, container, nil, rcd, log)
	if err != nil {
		// We should have logged the error in ensureContainerNetworkConnections, so don't re-log here
		return err
	}

	// Check to see if we are connected to all the ContainerNetworks listed in the Container object spec
	if len(connected) != len(*container.Spec.Networks) {
		log.V(1).Info("container not connected to expected number of networks, scheduling additional reconciliation...", "ContainerID", rcd.newContainerID, "Expected", len(*container.Spec.Networks), "Connected", len(connected))
		return fmt.Errorf("container not connected to expected number of networks")
	}

	if _, err = r.orchestrator.StartContainers(ctx, []string{rcd.newContainerID}); err != nil {
		log.Error(err, "failed to start Container", "ContainerID", rcd.newContainerID)
		// We failed to start the container, so record the error and schedule additional reconciliation
		rcd.startupError = err
		rcd.startAttemptFinishedAt = metav1.Now()
		return err
	}

	return nil
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
			if container.Spec.Stop {
				// The container was started with a desired state of stopped, don't attempt to start
				container.Status.State = apiv1.ContainerStateFailedToStart
				container.Status.FinishTimestamp = metav1.Now()

				return statusChanged
			}

			// This is brand new Container and we need to start it.
			r.ensureContainerWatch(container, log)

			_ = r.createContainer(ctx, container, log, noDelay) // The error, if any, will be handled by createContainer
			return statusChanged
		}

	case container.Status.State == apiv1.ContainerStateStarting:

		if !found {
			// We are still waiting for the container to start.
			// Whatever triggered reconciliation, it was not a container event and does not matter.
			return noChange
		}

		if rcd.startupError == nil {
			container.Status.ContainerID = rcd.newContainerID
			container.Status.EffectiveEnv = rcd.runSpec.Env
			container.Status.EffectiveArgs = rcd.runSpec.Args

			// We haven't started the container yet
			if rcd.startAttemptFinishedAt.IsZero() {
				if container.Spec.Networks == nil {
					// We should never be in this situation, as without the networks property set,
					// the container should either be started or in an error state when we get here.
					err := fmt.Errorf("container in unxepcted state")
					log.Error(err, "")
					// Set startup error so the next reconciliation loop will properly update
					rcd.startupError = err
					rcd.startAttemptFinishedAt = metav1.Now()
					// Schedule additional reconciliation to handle the error
					return additionalReconciliationNeeded
				}

				if err := r.handleInitialNetworkConnections(ctx, container, rcd, log); err != nil {
					// handleInitialNetworkConnections logged the error, so don't re-log here
					return additionalReconciliationNeeded
				}

				log.V(1).Info("container started", "ContainerID", rcd.newContainerID)
				rcd.startAttemptFinishedAt = metav1.Now()
				container.Status.State = apiv1.ContainerStateRunning
				container.Status.StartupTimestamp = rcd.startAttemptFinishedAt

				return statusChanged
			} else {
				log.V(1).Info("container has started successfully", "ContainerID", rcd.newContainerID)

				container.Status.State = apiv1.ContainerStateRunning
				container.Status.StartupTimestamp = rcd.startAttemptFinishedAt

				return statusChanged
			}
		} else if isTransientTemplateError(rcd.startupError) {
			log.Info("scheduling another startup attempt for the container...")
			r.runningContainers.DeleteBySecondKey(container.NamespacedName())
			err := r.createContainer(ctx, container, log, startupRetryDelay)
			if err != nil {
				return statusChanged // Startup attempt failed and Container object reached its final state
			} else {
				return noChange // We are still "starting", so there is nothing to be updated for the Container object
			}
		} else {
			log.Error(rcd.startupError, "container has failed to start")
			container.Status.ContainerID = ""
			container.Status.State = apiv1.ContainerStateFailedToStart
			container.Status.Message = fmt.Sprintf("Container could not be started: %s", rcd.startupError.Error())
			container.Status.FinishTimestamp = rcd.startAttemptFinishedAt
			return statusChanged
		}

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
		inspected, err := r.findContainer(ctx, containerID)
		if err != nil {
			// The container was probably removed
			log.Info("running container was not found, marking Container object as finished", "ContainerID", containerID)
			container.Status.State = apiv1.ContainerStateRemoved
			container.Status.FinishTimestamp = metav1.Now()
			return statusChanged
		}

		changed := r.updateContainerStatus(container, inspected)
		if container.Status.State == apiv1.ContainerStateRunning && container.Spec.Stop {
			r.stopContainer(ctx, container, log)
			container.Status.State = apiv1.ContainerStateExited
			container.Status.FinishTimestamp = metav1.Now()
			return statusChanged
		} else if container.Spec.Networks != nil {
			connected, connectionErr := r.ensureContainerNetworkConnections(ctx, container, inspected, rcd, log)
			if connectionErr != nil {
				// The error was already logged by ensureContainerNetworkConnections()
				log.V(1).Info("an error occurred while managing container network connections, scheduling additional reconciliation...")
				changed |= additionalReconciliationNeeded
			}

			var notConnected []string
			for _, network := range container.Status.Networks {
				if !slices.Any(connected, func(n *apiv1.ContainerNetwork) bool {
					return n.NamespacedName().String() == network
				}) {
					notConnected = append(notConnected, network)
				}
			}

			// If the set of connected networks has changed, update the Container object status
			if len(notConnected) > 0 || len(connected) != len(container.Status.Networks) {
				if len(notConnected) > 0 {
					log.V(1).Info("container became disconnected from some networks, updating status...", "DisonnectedNetworks", notConnected)
				}

				connectedNetworkNames := slices.Map[*apiv1.ContainerNetwork, string](connected, func(n *apiv1.ContainerNetwork) string {
					return n.NamespacedName().String()
				})

				if len(connected) != len(container.Status.Networks) {
					log.V(1).Info("container become connected to new networks, updating status...", "ConnectedNetworks", connected)
				}

				container.Status.Networks = connectedNetworkNames
				changed |= statusChanged
			}
		}

		return changed

	default:
		// Container failed to  start and is marked as such, so nothing more to do.
		return noChange

	}
}

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

// If we made it here, we need to attempt to start the container
func (r *ContainerReconciler) doStartContainer(container *apiv1.Container, containerName string, log logr.Logger, delay time.Duration) func(context.Context) {
	return func(startupCtx context.Context) {
		if delay > 0 {
			time.Sleep(delay)
		}

		rcd, err := func() (*runningContainerData, error) {
			log.V(1).Info("starting container", "image", container.Spec.Image)

			var rcd = newRunningContainerData(container)

			err := r.computeEffectiveEnvironment(startupCtx, container, rcd, log)
			if err != nil {
				if isTransientTemplateError(err) {
					log.Info("could not compute effective environment for the Container, retrying startup...", "Cause", err.Error())
				} else {
					log.Error(err, "could not compute effective environment for the Container")
				}

				return rcd, err
			}

			err = r.computeEffectiveInvocationArgs(startupCtx, container, rcd, log)
			if err != nil {
				if isTransientTemplateError(err) {
					log.Info("could not compute effective invocation arguments for the Container, retrying startup...", "Cause", err.Error())
				} else {
					log.Error(err, "could not compute effective invocation arguments for the Container")
				}

				return rcd, err
			}

			var defaultNetwork string
			if rcd.runSpec.Networks != nil {
				defaultNetwork = "bridge"
			}

			creationOptions := ct.CreateContainerOptions{
				ContainerSpec: *rcd.runSpec,
				Name:          containerName,
				Network:       defaultNetwork,
			}
			containerID, err := r.orchestrator.CreateContainer(startupCtx, creationOptions)
			// There are errors that can still result in a valid container ID, so we need to store it if one was returned
			rcd.newContainerID = containerID

			if err != nil {
				log.Error(err, "could not create the container")
				return rcd, err
			}
			log.V(1).Info("container created", "ContainerID", containerID)

			inspected, err := r.findContainer(startupCtx, containerID)
			if err != nil {
				log.Error(err, "could not inspect the container")
				return rcd, err
			}

			rcd.runSpec.Env = maps.MapToSlice[string, string, apiv1.EnvVar](inspected.Env, func(key, value string) apiv1.EnvVar {
				return apiv1.EnvVar{Name: key, Value: value}
			})

			if rcd.runSpec.Networks == nil {
				_, err = r.orchestrator.StartContainers(startupCtx, []string{containerID})
				if err != nil {
					log.Error(err, "could not start the container", "ContainerID", containerID)
					return rcd, err
				}

				log.V(1).Info("container started", "ContainerID", containerID)
				rcd.startAttemptFinishedAt = metav1.Now()
			} else {
				for i := range inspected.Networks {
					network := inspected.Networks[i].Id
					// Containers are attached to a default network when created ("bridge" for Docker or "podman" for Podman). We need to detach
					// from any initial networks here so that we can fully control the network connections.
					err = r.orchestrator.DisconnectNetwork(startupCtx, ct.DisconnectNetworkOptions{Network: network, Container: containerID, Force: true})
					if err != nil {
						log.Error(err, "could not detach network from the container", "ContainerID", containerID, "Network", network)
						return rcd, err
					}
				}
			}

			return rcd, nil
		}()

		if err != nil {
			rcd.startupError = err
			rcd.startAttemptFinishedAt = metav1.Now()
		}

		r.runningContainers.Store(rcd.newContainerID, container.NamespacedName(), rcd)
		err = r.debouncer.ReconciliationNeeded(container.NamespacedName(), rcd.newContainerID, r.scheduleContainerReconciliation)
		if err != nil {
			log.Error(err, "could not schedule reconcilation for Container object")
		}
	}
}

func (r *ContainerReconciler) createContainer(ctx context.Context, container *apiv1.Container, log logr.Logger, delay time.Duration) error {
	container.Status.ExitCode = apiv1.UnknownExitCode

	containerName := strings.TrimSpace(container.Spec.ContainerName)

	// Determine whether we need to check for an existing container
	if container.Spec.Persistent {
		inspected, err := r.findContainer(ctx, containerName)
		if err == nil {
			// We found a container, update the status and return
			rcd := newRunningContainerData(container)
			rcd.newContainerID = inspected.Id
			rcd.startAttemptFinishedAt = metav1.Now()

			container.Status.ContainerID = inspected.Id
			container.Status.State = apiv1.ContainerStateRunning
			container.Status.ContainerName = strings.TrimLeft(inspected.Name, "/")

			// Tracking actual container environment for status update
			rcd.runSpec.Env = maps.MapToSlice[string, string, apiv1.EnvVar](inspected.Env, func(key, value string) apiv1.EnvVar {
				return apiv1.EnvVar{Name: key, Value: value}
			})
			container.Status.EffectiveEnv = rcd.runSpec.Env

			// Tracking actual container arguments for status update
			rcd.runSpec.Args = inspected.Args
			container.Status.EffectiveArgs = rcd.runSpec.Args

			r.runningContainers.Store(inspected.Id, container.NamespacedName(), rcd)

			log.Info("found existing Container", "ContainerName", container.Spec.ContainerName, "ContainerID", inspected.Id)

			return nil
		}
	}

	log.V(1).Info("scheduling container start", "image", container.Spec.Image)

	if containerName == "" {
		uniqueContainerName, err := MakeUniqueName(container.Name)
		if err != nil {
			return err
		}

		containerName = uniqueContainerName
	}

	err := r.startupQueue.Enqueue(r.doStartContainer(container, containerName, log, delay))

	if err != nil {
		log.Error(err, "container was not started because the workload is shutting down")
		container.Status.State = apiv1.ContainerStateFailedToStart
		container.Status.Message = fmt.Sprintf("Container could not be started: workload is shutting down: %s", err.Error())
		container.Status.FinishTimestamp = metav1.Now()
		return err
	} else {
		container.Status.State = apiv1.ContainerStateStarting
		return nil
	}
}

func (r *ContainerReconciler) updateContainerStatus(container *apiv1.Container, inspected *ct.InspectedContainer) objectChange {
	status := container.Status
	oldState := status.State

	status.ContainerName = strings.TrimLeft(inspected.Name, "/")

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
	}

	return noChange
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
	rcd *runningContainerData,
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
		if i, err := r.findContainer(ctx, rcd.newContainerID); err != nil {
			log.Error(err, "could not inspect the container", "ContainerID", rcd.newContainerID)
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
			log.Info("Removed a ContainerNetworkConnection connection", "Container", container.Status.ContainerID, "ContainerNetworkConnection", connection.NamespacedName().String())
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
				ContainerID:          container.Status.ContainerID,
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
			log.Info("Added new ContainerNetworkConnection", "Container", container.Status.ContainerID, "ContainerNetworkConnection", connection.NamespacedName().String())
			r.networkConnections.Store(networkConnectionKey, true)
		}
	}

	return validConnectedNetworks, nil
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

func (r *ContainerReconciler) processNetworkEvent(em ct.EventMessage) {
	switch em.Action {
	case ct.EventActionConnect, ct.EventActionDisconnect:
		containerID, found := em.Attributes["container"]
		if !found {
			// We could not identify the container this event applies to
			return
		}

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

func (r *ContainerReconciler) onShutdown() {
	<-r.lifetimeCtx.Done()

	r.lock.Lock()
	defer r.lock.Unlock()
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
	log.V(1).Info("inspecting running container to get its port information...", "ContainerID", ctr.Status.ContainerID)
	inspected, err := r.findContainer(ctx, ctr.Status.ContainerID)
	if err != nil {
		return "", 0, err
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

// For the sake of storing the runningContainerData in the runningContainers map we need
// to have a unique container ID, so we generate a fake one here.
// This ID won't be used for any real work.
func getFailedContainerID(containerObjectName types.NamespacedName) string {
	return "__failed-" + containerObjectName.String()
}
