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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

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
)

type runningNetworkState struct {
	state       apiv1.ContainerNetworkState
	id          string
	message     string
	connections map[string]bool
}

func (rnc *runningNetworkState) Clone() *runningNetworkState {
	return &runningNetworkState{
		state:       rnc.state,
		id:          rnc.id,
		message:     rnc.message,
		connections: stdmaps.Clone(rnc.connections),
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
	return changed
}

var _ Cloner[*runningNetworkState] = (*runningNetworkState)(nil)
var _ UpdateableFrom[*runningNetworkState] = (*runningNetworkState)(nil)

type networkStateMap = ObjectStateMap[string, runningNetworkState, *runningNetworkState]

type NetworkReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	orchestrator        containers.ContainerOrchestrator

	existingNetworks *networkStateMap

	// Channel used to trigger reconciliation when underlying networks change
	notifyNetworkChanged *concurrency.UnboundedChan[ctrl_event.GenericEvent]
	// Network events subscription
	networkEvtSub *pubsub.Subscription[containers.EventMessage]
	// Channel to receive network change events
	networkEvtCh         *concurrency.UnboundedChan[containers.EventMessage]
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
}

var (
	networkFinalizer string = fmt.Sprintf("%s/network-reconciler", apiv1.GroupVersion.Group)
)

func NewNetworkReconciler(lifetimeCtx context.Context, client ctrl_client.Client, log logr.Logger, orchestrator containers.ContainerOrchestrator) *NetworkReconciler {
	r := NetworkReconciler{
		Client:               client,
		orchestrator:         orchestrator,
		existingNetworks:     NewObjectStateMap[string, runningNetworkState](),
		notifyNetworkChanged: concurrency.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx),
		networkEvtSub:        nil,
		networkEvtCh:         nil,
		networkEvtWorkerStop: nil,
		debouncer:            newReconcilerDebouncer[string](),
		watchingResources:    &syncmap.Map[types.UID, bool]{},
		lock:                 &sync.Mutex{},
		lifetimeCtx:          lifetimeCtx,
		Log:                  log,
		orchestratorHealthy:  &atomic.Bool{},
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
		r.Log.Error(err, "failed to create index for ContainerNetworkConnection", "indexField", NetworkResourceNameField)
		return err
	}

	src := ctrl_source.Channel(r.notifyNetworkChanged.Out, &handler.EnqueueRequestForObject{})
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv1.ContainerNetwork{}).
		Watches(&apiv1.ContainerNetworkConnection{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNetwork), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(src).
		Named(name).
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

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(network.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		log.Info("ContainerNetwork object is being deleted", "NetworkName", network.Status.NetworkName)

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

	// Reconcile again after a delay (with some fuzz to avoid stampedes) if needed
	reconciliationDelay := defaultAdditionalReconciliationDelay
	reconciliationJitter := defaultAdditionalReconciliationJitter
	if (change & additionalReconciliationNeeded) == 0 {
		// Schedule followup reconciliation on a random delay between 5 to 10 seconds (to avoid stampedes).
		// The goal is to enable periodic reconciliation polling.
		reconciliationDelay = 5 * time.Second
		reconciliationJitter = 5 * time.Second
		change |= additionalReconciliationNeeded
	}

	result, err := saveChangesWithCustomReconciliationDelay(r.Client, ctx, &network, patch, change, reconciliationDelay, reconciliationJitter, nil, log)
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
			log.Info("could not disconnect all containers from the network, retrying...", "Network", network.Status.NetworkName)
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
		existing, err := inspectNetworkIfExists(ctx, r.orchestrator, networkName)
		if err == nil {
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
		log.Error(thisProcessErr, "could not get the current process information; container network will not have creator process information")
	} else {
		createOptions.Labels[CreatorProcessIdLabel] = fmt.Sprintf("%d", thisProcess.Pid)
		createOptions.Labels[CreatorProcessStartTimeLabel] = thisProcess.CreationTime.Format(osutil.RFC3339MiliTimestampFormat)
	}

	cnet, err := createNetwork(ctx, r.orchestrator, createOptions)
	if network.Spec.Persistent && errors.Is(err, containers.ErrAlreadyExists) {
		log.V(1).Info("persistent network already exists, but initial inspection failed, retrying...", "Network", networkName)
		return additionalReconciliationNeeded
	} else if errors.Is(err, containers.ErrCouldNotAllocate) {
		log.Error(err, "could not create the network as all available subnet ranges from the default pool are allocated, retrying...", "Network", networkName)
		return additionalReconciliationNeeded
	} else if errors.Is(err, containers.ErrRuntimeNotHealthy) {
		log.Error(err, "could not create the network as the container runtime is not healthy, retrying...", "Network", networkName)
		return additionalReconciliationNeeded
	} else if err != nil {
		log.Error(err, "could not create a network", "Network", networkName)
		r.existingNetworks.Store(network.NamespacedName(), networkName, &runningNetworkState{state: apiv1.ContainerNetworkStateFailedToStart, message: err.Error()})
		network.Status.State = apiv1.ContainerNetworkStateFailedToStart
		network.Status.Message = err.Error()
		return statusChanged
	}

	log.Info("network created", "Network", networkName)

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
		log.Error(err, "failed to list child ContainerNetworkConnection objects", "Network", namespacedName.String())
		return additionalReconciliationNeeded
	}

	// Initialize the connected network containers map if needed
	if networkState.connections == nil {
		networkState.connections = make(map[string]bool)

		if !network.Spec.Persistent {
			// If this is not a persistent network, we assume these were all connected by us
			for _, containerID := range network.Status.ContainerIDs {
				networkState.connections[containerID] = true
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

		err := r.orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
			Network:   network.Status.ID,
			Container: containerID,
		})
		if err != nil && !errors.Is(err, containers.ErrNotFound) {
			log.Error(err, "could not disconnect a container from the network", "Container", containerID, "Network", network.Status.NetworkName)
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

		err := r.orchestrator.ConnectNetwork(ctx, containers.ConnectNetworkOptions{
			Network:   network.Status.ID,
			Container: containerID,
			Aliases:   connection.Spec.Aliases,
		})

		if err != nil && !errors.Is(err, containers.ErrAlreadyExists) && !errors.Is(err, containers.ErrNotFound) {
			log.Error(err, "could not connect a container to the network", "Container", containerID, "Network", network.Status.NetworkName)
			change |= additionalReconciliationNeeded
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
				networkState.connections[containerID] = true
			}
		}

		return nil
	})

	if verifyErr != nil {
		log.Error(verifyErr, "could not verify network state", "Network", network.Status.NetworkName)
		change |= additionalReconciliationNeeded
	}

	if found < len(networkConnections.Items) {
		log.Info("not all expected containers are connected to the network, retrying...", "Network", network.Status.NetworkName, "Expected", len(networkConnections.Items), "Found", found)
		change |= additionalReconciliationNeeded
	} else {
		log.Info("all expected containers are connected to the network", "Network", network.Status.NetworkName, "Expected", len(networkConnections.Items), "Found", found)
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
	newContainerIds := usvc_slices.Map[containers.InspectedNetworkContainer, string](cnet.Containers, func(c containers.InspectedNetworkContainer) string {
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

	r.networkEvtCh = concurrency.NewUnboundedChanBuffered[containers.EventMessage](
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

		r.Log.V(1).Info("detected network update, scheduling reconciliation for Network object", "Network", networkObjectName.String())
		r.debouncer.ReconciliationNeeded(r.lifetimeCtx, networkObjectName, networkId, r.scheduleNetworkReconciliation)
	}
}

func (r *NetworkReconciler) requestReconcileForNetwork(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	cnc := obj.(*apiv1.ContainerNetworkConnection)
	networkNamespacedName := commonapi.AsNamespacedName(cnc.Spec.ContainerNetworkName, cnc.Namespace)

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
