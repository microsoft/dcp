// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	stdmaps "maps"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/pubsub"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	usvc_slices "github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

const (
	networkEventChanInitialCapacity = 20
	NetworkResourceNameField        = ".metadata.networkResourceName"

	networkInspectionTimeout = 6 * time.Second

	// How many concurrent failures connecting or disconnecting from a network should we tolerate before we log an error
	logAfterFailures = 4
)

type connectionState struct {
	// Number of attempts made to connect to the network
	// This will be reset to 0 when a connection is established
	attempts int
}

type runningNetworkState struct {
	state       apiv1.ContainerNetworkState
	id          string
	message     string
	connections map[string]*connectionState
	expected    int
	connected   int
}

func (rnc *runningNetworkState) Clone() *runningNetworkState {
	return &runningNetworkState{
		state:       rnc.state,
		id:          rnc.id,
		message:     rnc.message,
		connections: stdmaps.Clone(rnc.connections),
		expected:    rnc.expected,
		connected:   rnc.connected,
	}
}

func (rnc *runningNetworkState) UpdateFrom(other *runningNetworkState) bool {
	changed := false
	if rnc.state != other.state {
		rnc.state = other.state
		changed = true
	}
	if rnc.id != other.id {
		rnc.id = other.id
		changed = true
	}
	if rnc.message != other.message {
		rnc.message = other.message
		changed = true
	}
	if !stdmaps.Equal(rnc.connections, other.connections) {
		rnc.connections = stdmaps.Clone(other.connections)
		changed = true
	}
	if rnc.expected != other.expected {
		rnc.expected = other.expected
		changed = true
	}
	if rnc.connected != other.connected {
		rnc.connected = other.connected
		changed = true
	}
	return changed
}

var _ Cloner[*runningNetworkState] = (*runningNetworkState)(nil)
var _ UpdateableFrom[*runningNetworkState] = (*runningNetworkState)(nil)

type networkStateMap = ObjectStateMap[string, runningNetworkState, *runningNetworkState]

type NetworkReconciler struct {
	*ReconcilerBase[apiv1.ContainerNetwork, *apiv1.ContainerNetwork]

	orchestrator containers.ContainerOrchestrator

	existingNetworks *networkStateMap

	// Network events subscription
	networkEvtSub *pubsub.Subscription[containers.EventMessage]
	// Channel to receive network change events
	networkEvtCh         *concurrency.UnboundedChan[containers.EventMessage]
	networkEvtWorkerStop chan struct{}

	// Count of existing Container resources
	watchingResources *syncmap.Map[types.UID, bool]
	// Lock to protect the reconciler data that requires synchronized access
	lock *sync.Mutex

	// True if the container orchestrator is healthy, false otherwise
	orchestratorHealthy *atomic.Bool

	// The resource harvester used to clean up abandoned resources on startup
	harvester *resourceHarvester
}

var (
	networkFinalizer string = fmt.Sprintf("%s/network-reconciler", apiv1.GroupVersion.Group)
)

func NewNetworkReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	orchestrator containers.ContainerOrchestrator,
	harvester *resourceHarvester,
) *NetworkReconciler {
	base := NewReconcilerBase[apiv1.ContainerNetwork, *apiv1.ContainerNetwork](client, noCacheClient, log, lifetimeCtx)

	r := NetworkReconciler{
		ReconcilerBase:       base,
		orchestrator:         orchestrator,
		existingNetworks:     NewObjectStateMap[string, runningNetworkState](),
		networkEvtSub:        nil,
		networkEvtCh:         nil,
		networkEvtWorkerStop: nil,
		watchingResources:    &syncmap.Map[types.UID, bool]{},
		lock:                 &sync.Mutex{},
		orchestratorHealthy:  &atomic.Bool{},
		harvester:            harvester,
	}

	go r.onShutdown()

	return &r
}

func (r *NetworkReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	// Setup a client side index to allow quickly finding all ContainerNetworkConnections referencing a specific ContainerNetwork.
	// Behind the scenes this is using listers and informers to keep an index on an internal cache owned by
	// the Manager up to date.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.ContainerNetworkConnection{}, NetworkResourceNameField, func(rawObj ctrl_client.Object) []string {
		cnc := rawObj.(*apiv1.ContainerNetworkConnection)
		if cnc.Spec.ContainerNetworkName == "" {
			return nil
		}

		namespacedName := commonapi.AsNamespacedName(cnc.Spec.ContainerNetworkName, cnc.Namespace)

		return []string{namespacedName.Name}
	}); err != nil {
		r.Log.Error(err, "Failed to create index for ContainerNetworkConnection", "IndexField", NetworkResourceNameField)
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv1.ContainerNetwork{}).
		Watches(&apiv1.ContainerNetworkConnection{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNetwork), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *NetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)

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
	err := reader.Get(ctx, req.NamespacedName, &network)

	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.V(1).Info("The ContainerNetwork object was not found")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "Failed to Get() the ContainerNetwork object")
			getFailedCounter.Add(ctx, 1)
			return ctrl.Result{}, err
		}
	} else {
		getSucceededCounter.Add(ctx, 1)
	}

	// Add common metadata to the log
	if network.Status.NetworkName != "" {
		log = log.WithValues("NetworkName", network.Status.NetworkName)
	} else if strings.TrimSpace(network.Spec.NetworkName) != "" {
		log = log.WithValues("NetworkName", strings.TrimSpace(network.Spec.NetworkName))
	}

	if network.Status.ID != "" {
		log = log.WithValues("NetworkID", GetShortId(network.Status.ID))
	}

	if !r.orchestratorHealthy.Load() {
		log.V(1).Info("Container runtime is not healthy, retrying reconciliation later...")
		// Retry after five to ten seconds
		return ctrl.Result{RequeueAfter: time.Duration(rand.Intn(5)+5) * time.Second}, nil
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(network.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		log.Info("ContainerNetwork object is being deleted")

		err = r.deleteNetwork(ctx, &network, log)
		if err != nil {
			// deleteNetwork() logged the error already
			change = additionalReconciliationNeeded
		} else {
			// We've successfully deleted the network, so stop tracking it
			r.existingNetworks.DeleteByNamespacedName(network.NamespacedName())
			change = deleteFinalizer(&network, networkFinalizer, log)
			r.releaseNetworkWatch(&network, log)
		}
	} else {
		change = ensureFinalizer(&network, networkFinalizer, log)
		if change == noChange {
			change = r.manageNetwork(ctx, &network, log)
		}
	}

	reconciliationDelay := StandardDelay
	if (change & additionalReconciliationNeeded) == 0 {
		// Schedule followup reconciliation on a long to enable periodic network polling.
		reconciliationDelay = LongDelay
		change |= additionalReconciliationNeeded
	}

	result, err := r.SaveChangesWithDelay(ctx, &network, patch, change, reconciliationDelay, nil, log)
	return result, err
}

func (r *NetworkReconciler) deleteNetwork(ctx context.Context, network *apiv1.ContainerNetwork, log logr.Logger) error {
	if network.Spec.Persistent {
		return nil
	}

	_, networkState := r.existingNetworks.BorrowByNamespacedName(network.NamespacedName())
	if networkState != nil && networkState.state == apiv1.ContainerNetworkStateRunning {
		inspectedNetwork, err := r.orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
			Networks: []string{networkState.id},
		})

		if errors.Is(err, containers.ErrNotFound) {
			return nil
		}

		if err != nil {
			return err
		}

		for _, container := range inspectedNetwork[0].Containers {
			disconnectErr := r.orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
				Network:   networkState.id,
				Container: container.Id,
			})

			if disconnectErr != nil && !errors.Is(disconnectErr, containers.ErrNotFound) {
				err = errors.Join(err, disconnectErr)
			}
		}

		if err != nil {
			log.Info("Could not disconnect all containers from the network, retrying...")
			return err
		}

		err = removeNetwork(ctx, r.orchestrator, networkState.id, log)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *NetworkReconciler) manageNetwork(ctx context.Context, network *apiv1.ContainerNetwork, log logr.Logger) objectChange {
	change := noChange

	networkID, networkState := r.existingNetworks.BorrowByNamespacedName(network.NamespacedName())
	if networkState == nil {
		// If we are not tracking this network, we need to ensure it exists
		return r.ensureNetwork(ctx, network, log)
	}

	if network.Status.ID != networkID {
		network.Status.ID = networkID
		change |= statusChanged
	}

	if network.Status.State != networkState.state {
		network.Status.State = networkState.state
		network.Status.Message = networkState.message
		change |= statusChanged
	}

	if network.Status.State == apiv1.ContainerNetworkStateRunning {
		change |= r.updateNetworkStatus(ctx, network, networkState, log)
		change |= r.ensureConnections(ctx, network, networkState, log)
		r.existingNetworks.Update(network.NamespacedName(), networkState.id, networkState)
	}

	return change
}

func (r *NetworkReconciler) ensureNetwork(ctx context.Context, network *apiv1.ContainerNetwork, log logr.Logger) objectChange {
	r.ensureNetworkWatch(network, log)

	networkName := strings.TrimSpace(network.Spec.NetworkName)

	if network.Spec.Persistent {
		isNetworkSafeToReuse := r.harvester.IsDone() || r.harvester.TryProtectNetwork(ctx, networkName)
		existing, err := inspectNetworkIfExists(ctx, r.orchestrator, networkName)
		if err == nil {
			if !isNetworkSafeToReuse {
				// If the harvester is not done, we need to wait for it to complete before we can safely re-use an
				// existing network.
				log.V(1).Info("Waiting for the resource harvester to finish before re-using persistent network")
				return additionalReconciliationNeeded
			}

			// We found an existing network
			r.existingNetworks.Store(network.NamespacedName(), existing.Id, &runningNetworkState{state: apiv1.ContainerNetworkStateRunning, id: existing.Id})
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

		log = log.WithValues("NetworkName", networkName)
	}

	createOptions := containers.CreateNetworkOptions{
		Name: networkName,
		IPv6: network.Spec.IPv6,
		Labels: map[string]string{
			PersistentLabel: fmt.Sprintf("%t", network.Spec.Persistent),
		},
	}

	thisProcess, thisProcessErr := process.This()
	if thisProcessErr != nil {
		log.Error(thisProcessErr, "Could not get the current process information; container network will not have creator process information")
	} else {
		createOptions.Labels[CreatorProcessIdLabel] = fmt.Sprintf("%d", thisProcess.Pid)
		createOptions.Labels[CreatorProcessStartTimeLabel] = thisProcess.CreationTime.Format(osutil.RFC3339MiliTimestampFormat)
	}

	cnet, err := createNetwork(ctx, r.orchestrator, createOptions)
	if network.Spec.Persistent && errors.Is(err, containers.ErrAlreadyExists) {
		log.V(1).Info("Persistent network already exists, but initial inspection failed, retrying...")
		return additionalReconciliationNeeded
	} else if errors.Is(err, containers.ErrCouldNotAllocate) {
		log.Error(err, "Could not create the network as all available subnet ranges from the default pool are allocated, retrying...")
		return additionalReconciliationNeeded
	} else if errors.Is(err, containers.ErrRuntimeNotHealthy) {
		log.Error(err, "Could not create the network as the container runtime is not healthy, retrying...")
		return additionalReconciliationNeeded
	} else if err != nil {
		log.Error(err, "Could not create a network")
		r.existingNetworks.Store(network.NamespacedName(), networkName, &runningNetworkState{state: apiv1.ContainerNetworkStateFailedToStart, message: err.Error()})
		network.Status.State = apiv1.ContainerNetworkStateFailedToStart
		network.Status.Message = err.Error()
		return statusChanged
	}

	log.Info("Network created")

	r.existingNetworks.Store(network.NamespacedName(), cnet.Id, &runningNetworkState{state: apiv1.ContainerNetworkStateRunning, id: cnet.Id})

	network.Status.ID = cnet.Id
	network.Status.State = apiv1.ContainerNetworkStateRunning
	network.Status.NetworkName = cnet.Name
	network.Status.Driver = cnet.Driver
	network.Status.IPv6 = cnet.IPv6
	network.Status.Subnets = cnet.Subnets
	network.Status.Gateways = cnet.Gateways

	return statusChanged
}

func (r *NetworkReconciler) ensureConnections(ctx context.Context, network *apiv1.ContainerNetwork, networkState *runningNetworkState, log logr.Logger) objectChange {
	change := noChange

	namespacedName := network.NamespacedName()

	var networkConnections apiv1.ContainerNetworkConnectionList
	if err := r.List(ctx, &networkConnections, ctrl_client.InNamespace(network.GetNamespace()), ctrl_client.MatchingFields{NetworkResourceNameField: namespacedName.Name}); err != nil {
		log.Error(err, "Failed to list child ContainerNetworkConnection objects")
		return additionalReconciliationNeeded
	}

	// Initialize the connected network containers map if needed
	if networkState.connections == nil {
		networkState.connections = make(map[string]*connectionState)

		if !network.Spec.Persistent {
			// If this is not a persistent network, we assume these were all connected by us
			for _, containerID := range network.Status.ContainerIDs {
				networkState.connections[containerID] = &connectionState{}
			}
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

		_, knownConnection := networkState.connections[containerID]
		if !knownConnection && network.Spec.Persistent {
			// If this is a persistent network, we shouldn't disconnect any containers that we didn't explicitly connect
			continue
		}

		if !knownConnection {
			networkState.connections[containerID] = &connectionState{}
		}

		err := r.orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
			Network:   network.Status.ID,
			Container: containerID,
		})
		if err != nil && !errors.Is(err, containers.ErrNotFound) {
			networkState.connections[containerID].attempts += 1
			if networkState.connections[containerID].attempts%logAfterFailures == 0 {
				// We only log an error every logAfterFailures attempts to avoid flooding the logs with transient errors
				log.Error(err, "Could not disconnect a container from the network", "Container", containerID)
			}

			change |= additionalReconciliationNeeded
		} else {
			delete(networkState.connections, containerID)
		}
	}

	for i := range networkConnections.Items {
		connection := networkConnections.Items[i]
		containerID := connection.Spec.ContainerID

		found := slices.Contains(network.Status.ContainerIDs, containerID)

		if found {
			// Container is already connected to the network, do nothing
			continue
		}

		if _, knownConnection := networkState.connections[containerID]; !knownConnection {
			networkState.connections[containerID] = &connectionState{}
		}

		err := r.orchestrator.ConnectNetwork(ctx, containers.ConnectNetworkOptions{
			Network:   network.Status.ID,
			Container: containerID,
			Aliases:   connection.Spec.Aliases,
		})

		if err != nil && !errors.Is(err, containers.ErrAlreadyExists) && !errors.Is(err, containers.ErrNotFound) {
			networkState.connections[containerID].attempts += 1
			if networkState.connections[containerID].attempts%logAfterFailures == 0 {
				// We only log an error every logAfterFailures attempts to avoid flooding the logs with transient errors
				log.Error(err, "Could not connect a container to the network", "Container", containerID)
			}

			change |= additionalReconciliationNeeded
		} else {
			networkState.connections[containerID].attempts = 0 // Reset the attempts counter on success
		}
	}

	found := 0
	_, verifyErr := verifyNetworkState(ctx, r.orchestrator, network.Status.ID, func(network *containers.InspectedNetwork) error {
		for i := range networkConnections.Items {
			connection := networkConnections.Items[i]
			containerID := connection.Spec.ContainerID

			if slices.ContainsFunc(network.Containers, func(c containers.InspectedNetworkContainer) bool {
				return c.Name == containerID || strings.HasPrefix(c.Id, containerID)
			}) {
				found += 1
				networkState.connections[containerID] = &connectionState{}
			}
		}

		return nil
	})

	if verifyErr != nil {
		log.Error(verifyErr, "Could not verify network state")
		change |= additionalReconciliationNeeded
	} else {
		changed := false
		if networkState.expected != len(networkConnections.Items) {
			changed = true
			networkState.expected = len(networkConnections.Items)
		}
		if networkState.connected != found {
			changed = true
			networkState.connected = found
		}

		if changed {
			if networkState.connected < networkState.expected {
				log.V(1).Info("Not all expected containers are connected to the network, retrying...", "Expected", networkState.expected, "Found", networkState.connected)
				change |= additionalReconciliationNeeded
			} else {
				log.Info("All expected containers are connected to the network", "Expected", networkState.expected, "Found", networkState.connected)
			}
		}
	}

	return change
}

func (r *NetworkReconciler) updateNetworkStatus(ctx context.Context, network *apiv1.ContainerNetwork, rns *runningNetworkState, log logr.Logger) objectChange {
	cnet, err := inspectNetwork(ctx, r.orchestrator, network.Status.ID)
	if errors.Is(err, containers.ErrNotFound) {
		network.Status.State = apiv1.ContainerNetworkStateRemoved
		rns.state = apiv1.ContainerNetworkStateRemoved
		return statusChanged
	} else if err != nil {
		log.Error(err, "Could not inspect a network")
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
	newContainerIds := usvc_slices.Map[containers.InspectedNetworkContainer, string](cnet.Containers, func(c containers.InspectedNetworkContainer) string {
		return c.Id
	})
	slices.Sort(newContainerIds) // Sort so we can do set comparison using slices.Equal
	if !slices.Equal(network.Status.ContainerIDs, newContainerIds) {
		network.Status.ContainerIDs = newContainerIds
		change |= statusChanged
	}

	return change
}

func (r *NetworkReconciler) ensureNetworkWatch(network *apiv1.ContainerNetwork, log logr.Logger) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.LifetimeCtx.Err() != nil {
		return // Do not start a container watch if we are done
	}

	_, _ = r.watchingResources.LoadOrStore(network.UID, true)

	if r.networkEvtSub != nil {
		return // We are already watching container events
	}

	r.networkEvtCh = concurrency.NewUnboundedChanBuffered[containers.EventMessage](
		r.LifetimeCtx,
		containerEventChanBuffer,
		containerEventChanBuffer,
	)

	r.networkEvtWorkerStop = make(chan struct{})
	go r.networkEventWorker(r.networkEvtWorkerStop, r.networkEvtCh.Out)

	log.V(1).Info("Subscribing to container events...")
	sub, err := r.orchestrator.WatchNetworks(r.networkEvtCh.In)
	if err != nil {
		log.Error(err, "Could not subscribe to network events")
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

	log.Info("No more ContainerNetwork resources are being watched, cancelling network watch")
	r.cancelNetworkWatch()
}

func (r *NetworkReconciler) networkEventWorker(stopCh chan struct{}, eventCh <-chan containers.EventMessage) {
	for {
		select {
		case em, isOpen := <-eventCh:
			if !isOpen {
				return
			}

			if em.Source != containers.EventSourceNetwork {
				continue
			}

			r.processNetworkEvent(em)

		case <-stopCh:
			return
		}
	}
}

func (r *NetworkReconciler) processNetworkEvent(em containers.EventMessage) {
	switch em.Action {
	// Any event that means the container has been started, stopped, or was removed, is interesting
	case containers.EventActionCreate, containers.EventActionDestroy, containers.EventActionConnect, containers.EventActionDisconnect:
		networkId := em.Actor.ID
		networkObjectName, rns := r.existingNetworks.BorrowByStateKey(networkId)
		if rns == nil {
			// We are not tracking this network
			return
		}

		r.Log.V(1).Info("Detected network update, scheduling reconciliation for Network object", "Network", networkObjectName.String())
		r.ScheduleReconciliation(networkObjectName)
	}
}

func (r *NetworkReconciler) requestReconcileForNetwork(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	cnc := obj.(*apiv1.ContainerNetworkConnection)
	networkNamespacedName := commonapi.AsNamespacedName(cnc.Spec.ContainerNetworkName, cnc.Namespace)

	r.Log.V(1).Info("Network connection updated, requesting network reconciliation", "NetworkConnection", cnc, "Network", networkNamespacedName)
	return []reconcile.Request{
		{
			NamespacedName: networkNamespacedName,
		},
	}
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
	<-r.LifetimeCtx.Done()
	r.lock.Lock()
	defer r.lock.Unlock()
	r.cancelNetworkWatch()
}
