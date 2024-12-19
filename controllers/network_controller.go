// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/tklauser/ps"

	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	ct "github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/pubsub"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	usvc_slices "github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

const (
	networkEventChanInitialCapacity = 20
	NetworkResourceNameField        = ".metadata.networkResourceName"

	networkInspectionTimeout = 6 * time.Second
)

type runningNetworkStatus struct {
	state       apiv1.ContainerNetworkState
	id          string
	message     string
	connections map[string]bool
}

type networkHarvesterStatus uint32

const (
	networkHarvesterNotStarted networkHarvesterStatus = 0
	networkHarversterRunning   networkHarvesterStatus = 1
	networkHarvesterDone       networkHarvesterStatus = 2

	PersistentNetworkLabel       = "com.microsoft.developer.usvc-dev.persistent"
	CreatorProcessIdLabel        = "com.microsoft.developer.usvc-dev.creatorProcessId"
	CreatorProcessStartTimeLabel = "com.microsoft.developer.usvc-dev.creatorProcessStartTime"
)

type NetworkReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	orchestrator        ct.ContainerOrchestrator

	existingNetworks *maps.SynchronizedDualKeyMap[string, types.NamespacedName, runningNetworkStatus]

	// Channel used to trigger reconciliation when underlying networks change
	notifyNetworkChanged *concurrency.UnboundedChan[ctrl_event.GenericEvent]
	// Network events subscription
	networkEvtSub *pubsub.Subscription[ct.EventMessage]
	// Channel to receive network change events
	networkEvtCh         *concurrency.UnboundedChan[ct.EventMessage]
	networkEvtWorkerStop chan struct{}
	// Count of existing Container resources
	watchingResources *syncmap.Map[types.UID, bool]
	// Debouncer used to schedule reconciliation. Extra data is the running network ID whose state changed.
	debouncer *reconcilerDebouncer[string]
	// Lock to protect the reconciler data that requires synchronized access
	lock *sync.Mutex

	// Reconciler lifetime context, used to cancel container watch during reconciler shutdown
	lifetimeCtx context.Context

	// True if the container orchestrator is healthy, false otherwise
	orchestratorHealthy *atomic.Bool

	// Status of the (unused) network harvester
	networkHarvesterStatus *atomic.Uint32
}

var (
	networkFinalizer string = fmt.Sprintf("%s/network-reconciler", apiv1.GroupVersion.Group)
)

func NewNetworkReconciler(lifetimeCtx context.Context, client ctrl_client.Client, log logr.Logger, orchestrator ct.ContainerOrchestrator) *NetworkReconciler {
	r := NetworkReconciler{
		Client:                 client,
		orchestrator:           orchestrator,
		existingNetworks:       maps.NewSynchronizedDualKeyMap[string, types.NamespacedName, runningNetworkStatus](),
		notifyNetworkChanged:   concurrency.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx),
		networkEvtSub:          nil,
		networkEvtCh:           nil,
		networkEvtWorkerStop:   nil,
		debouncer:              newReconcilerDebouncer[string](),
		watchingResources:      &syncmap.Map[types.UID, bool]{},
		lock:                   &sync.Mutex{},
		lifetimeCtx:            lifetimeCtx,
		Log:                    log,
		orchestratorHealthy:    &atomic.Bool{},
		networkHarvesterStatus: &atomic.Uint32{},
	}

	r.networkHarvesterStatus.Store(uint32(networkHarvesterNotStarted))

	go r.onShutdown()

	return &r
}

func (r *NetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Setup a client side index to allow quickly finding all ContainerNetworkConnections referencing a specific ContainerNetwork.
	// Behind the scenes this is using listers and informers to keep an index on an internal cache owned by
	// the Manager up to date.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.ContainerNetworkConnection{}, NetworkResourceNameField, func(rawObj ctrl_client.Object) []string {
		cnc := rawObj.(*apiv1.ContainerNetworkConnection)
		if cnc.Spec.ContainerNetworkName == "" {
			return nil
		}

		namespacedName := asNamespacedName(cnc.Spec.ContainerNetworkName, cnc.Namespace)

		return []string{namespacedName.Name}
	}); err != nil {
		r.Log.Error(err, "failed to create index for ContainerNetworkConnection", "indexField", NetworkResourceNameField)
		return err
	}

	src := ctrl_source.Channel(r.notifyNetworkChanged.Out, &handler.EnqueueRequestForObject{})
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.ContainerNetwork{}).
		Watches(&apiv1.ContainerNetworkConnection{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNetwork), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(src).
		Complete(r)
}

func (r *NetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("NetworkName", req.NamespacedName).WithValues("Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1))

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	if !r.orchestratorHealthy.Load() {
		status := r.orchestrator.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed)
		if status.IsHealthy() {
			r.orchestratorHealthy.Store(true)
		}
	}

	network := apiv1.ContainerNetwork{}
	err := r.Get(ctx, req.NamespacedName, &network)

	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.V(1).Info("the ContainerNetwork object was not found")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the ContainerNetwork object")
			getFailedCounter.Add(ctx, 1)
			return ctrl.Result{}, err
		}
	} else {
		getSucceededCounter.Add(ctx, 1)
	}

	if !r.orchestratorHealthy.Load() {
		log.V(1).Info("container runtime is not healthy, retrying reconciliation later...")
		// Retry after five to ten seconds
		return ctrl.Result{RequeueAfter: time.Duration(rand.Intn(5)+5) * time.Second}, nil
	}

	if r.networkHarvesterStatus.CompareAndSwap(uint32(networkHarvesterNotStarted), uint32(networkHarversterRunning)) {
		go r.harvestUnusedNetworks()
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(network.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		log.Info("ContainerNetwork object is being deleted", "NetworkName", network.Status.NetworkName)

		err = r.deleteNetwork(ctx, &network)
		if err != nil {
			// deleteNetwork() logged the error already
			change = additionalReconciliationNeeded
		} else {
			// We've successfully deleted the network, so stop tracking it
			r.existingNetworks.DeleteBySecondKey(network.NamespacedName())
			change = deleteFinalizer(&network, networkFinalizer, log)
			r.releaseNetworkWatch(&network, log)
		}
	} else {
		change = ensureFinalizer(&network, networkFinalizer, log)
		if change == noChange {
			change = r.manageNetwork(ctx, &network, log)
		}
	}

	reconciliationDelay := defaultAdditionalReconciliationDelay
	if (change & additionalReconciliationNeeded) == 0 {
		// Schedule followup reconciliation on a random delay between 5 to 10 seconds (to avoid stampedes).
		// The goal is to enable periodic reconciliation polling.
		reconciliationDelay = time.Duration(rand.Intn(5)+5) * time.Second
		change |= additionalReconciliationNeeded
	}

	result, err := saveChangesWithCustomReconciliationDelay(r.Client, ctx, &network, patch, change, reconciliationDelay, nil, log)
	return result, err
}

func (r *NetworkReconciler) deleteNetwork(ctx context.Context, network *apiv1.ContainerNetwork) error {
	if network.Spec.Persistent {
		return nil
	}

	_, networkStatus, found := r.existingNetworks.FindBySecondKey(network.NamespacedName())
	if found && networkStatus.state == apiv1.ContainerNetworkStateRunning {
		err := removeNetwork(ctx, r.orchestrator, networkStatus.id)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *NetworkReconciler) manageNetwork(ctx context.Context, network *apiv1.ContainerNetwork, log logr.Logger) objectChange {
	change := noChange

	networkID, networkStatus, found := r.existingNetworks.FindBySecondKey(network.NamespacedName())
	if found {
		if network.Status.ID != networkID {
			network.Status.ID = networkID
			change |= statusChanged
		}

		if network.Status.State != networkStatus.state {
			network.Status.State = networkStatus.state
			network.Status.Message = networkStatus.message
			change |= statusChanged
		}
	} else {
		// If we are not tracking this network, we need to ensure it exists
		return r.ensureNetwork(ctx, network, log)
	}

	if network.Status.State == apiv1.ContainerNetworkStateRunning {
		change |= r.updateNetworkStatus(ctx, network, log)
		change |= r.ensureConnections(ctx, network, networkStatus, log)
	}

	return change
}

func (r *NetworkReconciler) ensureNetwork(ctx context.Context, network *apiv1.ContainerNetwork, log logr.Logger) objectChange {
	r.ensureNetworkWatch(network, log)

	networkName := strings.TrimSpace(network.Spec.NetworkName)

	if network.Spec.Persistent {
		existing, err := inspectNetworkIfExists(ctx, r.orchestrator, networkName)
		if err == nil {
			// We found an existing network
			r.existingNetworks.Store(existing.Id, network.NamespacedName(), runningNetworkStatus{state: apiv1.ContainerNetworkStateRunning, id: existing.Id})
			network.Status.ID = existing.Id
			network.Status.State = apiv1.ContainerNetworkStateRunning
			network.Status.NetworkName = existing.Name
			network.Status.Driver = existing.Driver
			network.Status.IPv6 = existing.IPv6
			network.Status.Subnets = existing.Subnets
			network.Status.Gateways = existing.Gateways
			return statusChanged
		}
	}

	if networkName == "" {
		uniqueNetworkName, _, err := MakeUniqueName(network.Name)
		if err != nil {
			return additionalReconciliationNeeded
		}

		networkName = uniqueNetworkName
	}

	createOptions := ct.CreateNetworkOptions{
		Name: networkName,
		IPv6: network.Spec.IPv6,
		Labels: map[string]string{
			PersistentNetworkLabel: fmt.Sprintf("%t", network.Spec.Persistent),
		},
	}

	thisProcess, thisProcessErr := process.This()
	if thisProcessErr != nil {
		log.Error(thisProcessErr, "could not get the current process information; container network will not have creator process information")
	} else {
		createOptions.Labels[CreatorProcessIdLabel] = fmt.Sprintf("%d", thisProcess.Pid)
		createOptions.Labels[CreatorProcessStartTimeLabel] = thisProcess.CreationTime.Format(osutil.RFC3339MiliTimestampFormat)
	}

	cnet, err := createNetwork(ctx, r.orchestrator, createOptions)
	if network.Spec.Persistent && errors.Is(err, ct.ErrAlreadyExists) {
		log.V(1).Info("persistent network already exists, but initial inspection failed, retrying...", "Network", networkName)
		return additionalReconciliationNeeded
	} else if errors.Is(err, ct.ErrCouldNotAllocate) {
		log.Error(err, "could not create the network as all available subnet ranges from the default pool are allocated, retrying...", "Network", networkName)
		return additionalReconciliationNeeded
	} else if errors.Is(err, ct.ErrRuntimeNotHealthy) {
		log.Error(err, "could not create the network as the container runtime is not healthy, retrying...", "Network", networkName)
		return additionalReconciliationNeeded
	} else if err != nil {
		log.Error(err, "could not create a network", "Network", networkName)
		r.existingNetworks.Store(networkName, network.NamespacedName(), runningNetworkStatus{state: apiv1.ContainerNetworkStateFailedToStart, message: err.Error()})
		network.Status.State = apiv1.ContainerNetworkStateFailedToStart
		network.Status.Message = err.Error()
		return statusChanged
	}

	log.Info("network created", "Network", networkName)

	r.existingNetworks.Store(cnet.Id, network.NamespacedName(), runningNetworkStatus{state: apiv1.ContainerNetworkStateRunning, id: cnet.Id})

	network.Status.ID = cnet.Id
	network.Status.State = apiv1.ContainerNetworkStateRunning
	network.Status.NetworkName = cnet.Name
	network.Status.Driver = cnet.Driver
	network.Status.IPv6 = cnet.IPv6
	network.Status.Subnets = cnet.Subnets
	network.Status.Gateways = cnet.Gateways

	return statusChanged
}

func (r *NetworkReconciler) ensureConnections(ctx context.Context, network *apiv1.ContainerNetwork, networkStatus runningNetworkStatus, log logr.Logger) objectChange {
	change := noChange

	namespacedName := network.NamespacedName()

	var networkConnections apiv1.ContainerNetworkConnectionList
	if err := r.List(ctx, &networkConnections, ctrl_client.InNamespace(network.GetNamespace()), ctrl_client.MatchingFields{NetworkResourceNameField: namespacedName.Name}); err != nil {
		log.Error(err, "failed to list child ContainerNetworkConnection objects", "Network", namespacedName.String())
		return additionalReconciliationNeeded
	}

	// Initialize the connected network containers map if needed
	if networkStatus.connections == nil {
		networkStatus.connections = make(map[string]bool)

		for _, containerID := range network.Status.ContainerIDs {
			networkStatus.connections[containerID] = true
		}
	}

	// Disconnect unexpected containers from the network
	for i := range network.Status.ContainerIDs {
		containerID := network.Status.ContainerIDs[i]

		found := slices.ContainsFunc(networkConnections.Items, func(cnc apiv1.ContainerNetworkConnection) bool {
			return cnc.Spec.ContainerID == containerID
		})

		if found {
			// If we expect to still be connected to this network, do nothing
			continue
		}

		if _, knownConnection := networkStatus.connections[containerID]; !knownConnection && network.Spec.Persistent {
			// If this is a persistent network, we shouldn't disconnect any containers that we didn't explicitly connect
			continue
		}

		if err := disconnectNetwork(
			ctx,
			r.orchestrator,
			ct.DisconnectNetworkOptions{
				Network:   network.Status.ID,
				Container: containerID,
			},
		); err != nil && !errors.Is(err, ct.ErrNotFound) {
			log.Error(err, "could not disconnect a container from the network", "Container", containerID, "Network", network.Status.NetworkName)
			change |= additionalReconciliationNeeded
		} else {
			delete(networkStatus.connections, containerID)
			log.Info("disconnected a container from the network", "Container", containerID, "Network", network.Status.NetworkName)
		}
	}

	for i := range networkConnections.Items {
		connection := networkConnections.Items[i]
		containerID := connection.Spec.ContainerID

		_, found := networkStatus.connections[containerID]

		if found {
			// Container is already connected to the network, do nothing
			continue
		}

		err := connectNetwork(ctx, r.orchestrator, ct.ConnectNetworkOptions{
			Network:   network.Status.ID,
			Container: containerID,
			Aliases:   connection.Spec.Aliases,
		})
		if err != nil {
			log.Error(err, "could not connect a container to the network", "Container", containerID, "Network", network.Status.NetworkName)
			change |= additionalReconciliationNeeded
		} else {
			networkStatus.connections[containerID] = true
			log.Info("connected a container to the network", "Container", containerID, "Network", network.Status.NetworkName)
		}
	}

	r.existingNetworks.Update(network.Status.ID, network.NamespacedName(), networkStatus)

	return change
}

func (r *NetworkReconciler) updateNetworkStatus(ctx context.Context, network *apiv1.ContainerNetwork, log logr.Logger) objectChange {
	cnet, err := inspectNetwork(ctx, r.orchestrator, network.Status.ID)
	if errors.Is(err, ct.ErrNotFound) {
		network.Status.State = apiv1.ContainerNetworkStateRemoved
		r.existingNetworks.Update(network.Status.ID, network.NamespacedName(), runningNetworkStatus{state: apiv1.ContainerNetworkStateRemoved})

		return statusChanged
	} else if err != nil {
		log.Error(err, "could not inspect a network", "NetworkID", network.Status.ID)
		return additionalReconciliationNeeded
	}

	change := noChange

	if network.Status.NetworkName != cnet.Name {
		network.Status.NetworkName = cnet.Name
		change |= statusChanged
	}
	if network.Status.Driver != cnet.Driver {
		network.Status.Driver = cnet.Driver
		change |= statusChanged
	}
	if network.Status.IPv6 != cnet.IPv6 {
		network.Status.IPv6 = cnet.IPv6
		change |= statusChanged
	}
	if !slices.Equal(network.Status.Subnets, cnet.Subnets) {
		network.Status.Subnets = cnet.Subnets
		change |= statusChanged
	}
	if !slices.Equal(network.Status.Gateways, cnet.Gateways) {
		network.Status.Gateways = cnet.Gateways
		change |= statusChanged
	}
	// Get the list of container IDs connected to the network
	newContainerIds := usvc_slices.Map[ct.InspectedNetworkContainer, string](cnet.Containers, func(c ct.InspectedNetworkContainer) string {
		return c.Id
	})
	if !slices.Equal(network.Status.ContainerIDs, newContainerIds) {
		network.Status.ContainerIDs = newContainerIds
		change |= statusChanged
	}

	return change
}

func (r *NetworkReconciler) ensureNetworkWatch(network *apiv1.ContainerNetwork, log logr.Logger) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.lifetimeCtx.Err() != nil {
		return // Do not start a container watch if we are done
	}

	_, _ = r.watchingResources.LoadOrStore(network.UID, true)

	if r.networkEvtSub != nil {
		return // We are already watching container events
	}

	r.networkEvtCh = concurrency.NewUnboundedChanBuffered[ct.EventMessage](
		r.lifetimeCtx,
		containerEventChanBuffer,
		containerEventChanBuffer,
	)

	r.networkEvtWorkerStop = make(chan struct{})
	go r.networkEventWorker(r.networkEvtWorkerStop, r.networkEvtCh.Out)

	log.V(1).Info("subscribing to container events...")
	sub, err := r.orchestrator.WatchNetworks(r.networkEvtCh.In)
	if err != nil {
		log.Error(err, "could not subscribe to network events")
		close(r.networkEvtWorkerStop)
		r.networkEvtWorkerStop = nil
		return
	}

	r.networkEvtSub = sub
}

func (r *NetworkReconciler) releaseNetworkWatch(network *apiv1.ContainerNetwork, log logr.Logger) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.watchingResources.Delete(network.UID)

	var found bool
	r.watchingResources.Range(func(_ types.UID, _ bool) bool {
		found = true
		return false
	})
	if found {
		return // There are still other resources being watched, nothing to do
	}

	log.Info("no more ContainerNetwork resources are being watched, cancelling network watch")
	r.cancelNetworkWatch()
}

func (r *NetworkReconciler) networkEventWorker(stopCh chan struct{}, eventCh <-chan ct.EventMessage) {
	for {
		select {
		case em, isOpen := <-eventCh:
			if !isOpen {
				return
			}

			if em.Source != ct.EventSourceNetwork {
				continue
			}

			r.processNetworkEvent(em)

		case <-stopCh:
			return
		}
	}
}

func (r *NetworkReconciler) processNetworkEvent(em ct.EventMessage) {
	switch em.Action {
	// Any event that means the container has been started, stopped, or was removed, is interesting
	case ct.EventActionCreate, ct.EventActionDestroy, ct.EventActionConnect, ct.EventActionDisconnect:
		networkId := em.Actor.ID
		owner, _, found := r.existingNetworks.FindByFirstKey(networkId)
		if !found {
			// We are not tracking this container
			return
		}

		r.Log.V(1).Info("detected network update, scheduling reconciliation for Network object", "Network", owner.Name)
		r.debouncer.ReconciliationNeeded(r.lifetimeCtx, owner, networkId, r.scheduleNetworkReconciliation)
	}
}

func (r *NetworkReconciler) requestReconcileForNetwork(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	cnc := obj.(*apiv1.ContainerNetworkConnection)
	networkNamespacedName := asNamespacedName(cnc.Spec.ContainerNetworkName, cnc.Namespace)

	r.Log.V(1).Info("network connection updated, requesting network reconciliation", "NetworkConnection", cnc, "Network", networkNamespacedName)
	return []reconcile.Request{
		{
			NamespacedName: networkNamespacedName,
		},
	}
}

func (r *NetworkReconciler) scheduleNetworkReconciliation(rti reconcileTriggerInput[string]) {
	event := ctrl_event.GenericEvent{
		Object: &apiv1.ContainerNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:      rti.target.Name,
				Namespace: rti.target.Namespace,
			},
		},
	}
	r.notifyNetworkChanged.In <- event
}

func (r *NetworkReconciler) cancelNetworkWatch() {
	if r.networkEvtWorkerStop != nil {
		close(r.networkEvtWorkerStop)
		r.networkEvtWorkerStop = nil
	}
	if r.networkEvtSub != nil {
		r.networkEvtSub.Cancel()
		r.networkEvtSub = nil
	}
}

func (r *NetworkReconciler) onShutdown() {
	<-r.lifetimeCtx.Done()
	r.lock.Lock()
	defer r.lock.Unlock()
	r.cancelNetworkWatch()
}

func (r *NetworkReconciler) harvestUnusedNetworks() {
	defer r.networkHarvesterStatus.Store(uint32(networkHarvesterDone))
	log := r.Log.WithName("NetworkHarvester")

	// Errors, if any, will be logged by the harvester function.
	_ = DoHarvestUnusedNetworks(r.lifetimeCtx, r.orchestrator, log)
}

// This is a separate, public function to allow for easy testing.
func DoHarvestUnusedNetworks(
	ctx context.Context,
	co containers.ContainerOrchestrator,
	log logr.Logger,
) error {
	log.V(1).Info("Unused container network harvester started...")

	// Even if we encounter errors, we are going to log them at Info level.
	// Unused network harvesting is a best-effort operation,
	// and the end user is not expected to take any action if an error occurs during the process.

	// Do not let a panic in the harvester goroutine bring down the entire process.
	defer func() { _ = resiliency.MakePanicError(recover(), log) }()

	const networksWillNotBeHarvested = "; unused container networks will not be harvested"

	networks, listErr := listNetworks(ctx, co)
	if listErr != nil {
		log.Info("Could not list container networks"+networksWillNotBeHarvested, "Error", listErr)
		return listErr
	}

	// Only consider networks that are not persistent and have the creator process ID/start time label set.
	networks = usvc_slices.Select(networks, func(n ct.ListedNetwork) bool {
		nonPersistent := maps.HasExactValue(n.Labels, PersistentNetworkLabel, "false")
		_, hasValidPid := maps.TryGetValidValue(n.Labels, CreatorProcessIdLabel, func(v string) bool {
			_, err := process.StringToPidT(v)
			return err == nil
		})
		_, hasValidStartTime := maps.TryGetValidValue(n.Labels, CreatorProcessStartTimeLabel, func(v string) bool {
			_, err := time.Parse(osutil.RFC3339MiliTimestampFormat, v)
			return err == nil
		})
		return nonPersistent && hasValidPid && hasValidStartTime
	})

	if len(networks) == 0 {
		log.V(1).Info("No container networks to harvest left after check for persistent networks and creator process ID/start time label")
		return nil
	}

	procs, procsErr := ps.Processes()
	if procsErr != nil {
		log.Info("Could not list current processes"+networksWillNotBeHarvested, "Error", procsErr)
		return procsErr
	}

	// Filter out networks that were created by processes that are still running.
	networks = usvc_slices.Select(networks, func(n ct.ListedNetwork) bool {
		creatorPID, _ := process.StringToPidT(n.Labels[CreatorProcessIdLabel])
		creatorStartTime, _ := time.Parse(osutil.RFC3339MiliTimestampFormat, n.Labels[CreatorProcessStartTimeLabel])

		return !usvc_slices.Any(procs, func(p ps.Process) bool {
			pPid, pPidErr := process.IntToPidT(p.PID())
			if pPidErr != nil {
				return true // Can't convert the PID, so assume it's a running process and the network is off limits.
			}
			return pPid == creatorPID && process.HasExpectedStartTime(p, creatorStartTime)
		})
	})

	if len(networks) == 0 {
		log.V(1).Info("No container networks to harvest left after eliminating networks that belong to running processes")
		return nil
	}

	inspectedNetworks, inspectErr := inspectManyNetworks(
		ctx,
		co,
		usvc_slices.Map[ct.ListedNetwork, string](networks, func(n ct.ListedNetwork) string { return n.ID }),
	)
	if inspectErr != nil {
		log.Info("Could not inspect container networks"+networksWillNotBeHarvested, "Error", inspectErr)
		return inspectErr
	}

	// Filter out networks that have any containers attached to them.
	inspectedNetworks = usvc_slices.Select(inspectedNetworks, func(n ct.InspectedNetwork) bool {
		return len(n.Containers) == 0
	})

	if len(inspectedNetworks) == 0 {
		log.V(1).Info("No container networks to harvest left after eliminating networks with attached containers")
		return nil
	}

	unusedNetworkNames := usvc_slices.Map[ct.InspectedNetwork, string](inspectedNetworks, func(n ct.InspectedNetwork) string { return n.Name })
	log.V(1).Info("Removing unused container networks...", "Networks", unusedNetworkNames)

	removeErr := removeManyNetworks(
		ctx,
		co,
		usvc_slices.Map[ct.InspectedNetwork, string](inspectedNetworks, func(n ct.InspectedNetwork) string { return n.Id }),
	)
	if removeErr != nil {
		log.Info("Could not remove unused container networks"+networksWillNotBeHarvested, "Error", removeErr)
		return removeErr
	} else {
		log.V(1).Info("Unused container networks have been removed")
		return nil
	}
}
