// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/smallnest/chanx"
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
	ct "github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

const (
	networkEventChanInitialCapacity = 20
	NetworkResourceNameField        = ".metadata.networkResourceName"
)

type runningNetworkStatus struct {
	state       apiv1.ContainerNetworkState
	id          string
	message     string
	connections map[string]bool
}

type NetworkReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	orchestrator        ct.NetworkOrchestrator

	existingNetworks *maps.SynchronizedDualKeyMap[string, types.NamespacedName, runningNetworkStatus]

	// Channel used to trigger reconciliation when underlying networks change
	notifyNetworkChanged *chanx.UnboundedChan[ctrl_event.GenericEvent]
	// Network events subscription
	networkEvtSub *ct.EventSubscription
	// Channel to receive network change events
	networkEvtCh         *chanx.UnboundedChan[ct.EventMessage]
	networkEvtWorkerStop chan struct{}
	// Count of existing Container resources
	watchingResources syncmap.Map[types.UID, bool]
	// Debouncer used to schedule reconciliation. Extra data is the running network ID whose state changed.
	debouncer *reconcilerDebouncer[string]
	// Lock to protect the reconciler data that requires synchronized access
	lock *sync.Mutex

	// Reconciler lifetime context, used to cancel container watch during reconciler shutdown
	lifetimeCtx context.Context
}

var (
	networkFinalizer string = fmt.Sprintf("%s/network-reconciler", apiv1.GroupVersion.Group)
)

func NewNetworkReconciler(lifetimeCtx context.Context, client ctrl_client.Client, log logr.Logger, orchestrator ct.NetworkOrchestrator) *NetworkReconciler {
	r := NetworkReconciler{
		Client:               client,
		Log:                  log,
		orchestrator:         orchestrator,
		existingNetworks:     maps.NewSynchronizedDualKeyMap[string, types.NamespacedName, runningNetworkStatus](),
		notifyNetworkChanged: chanx.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx, 1),
		networkEvtSub:        nil,
		networkEvtCh:         nil,
		networkEvtWorkerStop: nil,
		debouncer:            newReconcilerDebouncer[string](reconciliationDebounceDelay),
		watchingResources:    syncmap.Map[types.UID, bool]{},
		lock:                 &sync.Mutex{},
		lifetimeCtx:          lifetimeCtx,
	}

	r.Log = log.WithValues("Controller", networkFinalizer)

	go r.onShutdown()

	return &r
}

func (r *NetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	src := ctrl_source.Channel{
		Source: r.notifyNetworkChanged.Out,
	}

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

	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.ContainerNetwork{}).
		Watches(&apiv1.ContainerNetworkConnection{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNetwork), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(&src, &handler.EnqueueRequestForObject{}).
		Complete(r)
}

func (r *NetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("NetworkName", req.NamespacedName).WithValues("Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1))

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	network := apiv1.ContainerNetwork{}
	err := r.Get(ctx, req.NamespacedName, &network)

	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.V(1).Info("the ContainerNetwork object was deleted")
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

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(network.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		log.Info("ContainerNetwork object is being deleted")

		err := r.deleteNetwork(ctx, &network, log)
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

	result, err := saveChanges(r.Client, ctx, &network, patch, change, nil, log)
	return result, err
}

func (r *NetworkReconciler) deleteNetwork(ctx context.Context, network *apiv1.ContainerNetwork, log logr.Logger) error {
	if network.Spec.Persistent {
		return nil
	}

	networkId, networkStatus, found := r.existingNetworks.FindBySecondKey(network.NamespacedName())
	if found && networkStatus.state == apiv1.ContainerNetworkStateRunning {
		removed, err := r.orchestrator.RemoveNetworks(ctx, ct.RemoveNetworksOptions{Networks: []string{networkStatus.id}, Force: true})
		if err != nil {
			if err != ct.ErrNotFound {
				log.Error(err, "could not remove a container network")
				return err
			} else {
				return nil // If the network is not there, that's the desired state.
			}
		} else if len(removed) != 1 || removed[0] != networkId {
			log.Error(fmt.Errorf("unexpected response received from container network removal request. Number of networks removed: %d", len(removed)), "")
			// .. but it did not fail, so assume the network was removed.
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
		// We need to check for an existing network before we create one
		networks, err := r.orchestrator.InspectNetworks(ctx, ct.InspectNetworksOptions{Networks: []string{networkName}})
		if err == nil {
			// We found an existing network
			r.existingNetworks.Store(networks[0].Id, network.NamespacedName(), runningNetworkStatus{state: apiv1.ContainerNetworkStateRunning, id: networks[0].Id})
			network.Status.ID = networks[0].Id
			network.Status.State = apiv1.ContainerNetworkStateRunning
			network.Status.NetworkName = networks[0].Name
			network.Status.Driver = networks[0].Driver
			network.Status.IPv6 = networks[0].IPv6
			network.Status.Subnets = networks[0].Subnets
			network.Status.Gateways = networks[0].Gateways
			return statusChanged
		}
	}

	if networkName == "" {
		uniqueNetworkName, err := MakeUniqueName(network.Name)
		if err != nil {
			return additionalReconciliationNeeded
		}

		networkName = uniqueNetworkName
	}

	createOptions := ct.CreateNetworkOptions{
		Name: networkName,
		IPv6: network.Spec.IPv6,
	}
	networkID, err := r.orchestrator.CreateNetwork(ctx, createOptions)
	if err != nil {
		log.Error(err, "could not create a network")
		r.existingNetworks.Store(networkName, network.NamespacedName(), runningNetworkStatus{state: apiv1.ContainerNetworkStateFailedToStart, message: err.Error()})
		network.Status.State = apiv1.ContainerNetworkStateFailedToStart
		network.Status.Message = err.Error()
		return statusChanged
	}

	log.Info("network created", "Network", networkName)

	r.existingNetworks.Store(networkID, network.NamespacedName(), runningNetworkStatus{state: apiv1.ContainerNetworkStateRunning, id: networkID})

	change := statusChanged
	network.Status.ID = networkID
	network.Status.State = apiv1.ContainerNetworkStateRunning

	networks, err := r.orchestrator.InspectNetworks(ctx, ct.InspectNetworksOptions{Networks: []string{networkID}})
	if err != nil {
		log.Error(err, "network created, but inspect failed, scheduling another reconciliation")
		change |= additionalReconciliationNeeded
	} else {
		log.V(1).Info("retrieved network information")
		network.Status.NetworkName = networks[0].Name
		network.Status.Driver = networks[0].Driver
		network.Status.IPv6 = networks[0].IPv6
		network.Status.Subnets = networks[0].Subnets
		network.Status.Gateways = networks[0].Gateways
	}

	return change
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

		if err := r.orchestrator.DisconnectNetwork(
			ctx,
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

		networkStatus.connections[containerID] = true

		found := slices.Contains(network.Status.ContainerIDs, containerID)

		if found {
			// Container is already connected to the network, do nothing
			continue
		}

		if err := r.orchestrator.ConnectNetwork(
			ctx,
			ct.ConnectNetworkOptions{
				Network:   network.Status.ID,
				Container: containerID,
				Aliases:   connection.Spec.Aliases,
			},
		); err != nil {
			log.Error(err, "could not connect a container to the network", "Container", containerID, "Network", network.Status.NetworkName)
			change |= additionalReconciliationNeeded
		} else {
			log.Info("connected a container to the network", "Container", containerID, "Network", network.Status.NetworkName)
		}
	}

	r.existingNetworks.Update(network.Status.ID, network.NamespacedName(), networkStatus)

	return change
}

func (r *NetworkReconciler) updateNetworkStatus(ctx context.Context, network *apiv1.ContainerNetwork, log logr.Logger) objectChange {
	networks, err := r.orchestrator.InspectNetworks(ctx, ct.InspectNetworksOptions{Networks: []string{network.Status.ID}})
	if errors.Is(err, ct.ErrNotFound) {
		network.Status.State = apiv1.ContainerNetworkStateRemoved
		r.existingNetworks.Update(network.Status.ID, network.NamespacedName(), runningNetworkStatus{state: apiv1.ContainerNetworkStateRemoved})

		return statusChanged
	} else if err != nil {
		log.Error(err, "could not inspect a network", "NetworkID", network.Status.ID)
		return additionalReconciliationNeeded
	}

	change := noChange

	if network.Status.NetworkName != networks[0].Name {
		network.Status.NetworkName = networks[0].Name
		change |= statusChanged
	}
	if network.Status.Driver != networks[0].Driver {
		network.Status.Driver = networks[0].Driver
		change |= statusChanged
	}
	if network.Status.IPv6 != networks[0].IPv6 {
		network.Status.IPv6 = networks[0].IPv6
		change |= statusChanged
	}
	if !slices.Equal(network.Status.Subnets, networks[0].Subnets) {
		network.Status.Subnets = networks[0].Subnets
		change |= statusChanged
	}
	if !slices.Equal(network.Status.Gateways, networks[0].Gateways) {
		network.Status.Gateways = networks[0].Gateways
		change |= statusChanged
	}
	if !slices.Equal(network.Status.ContainerIDs, networks[0].ContainerIDs) {
		network.Status.ContainerIDs = networks[0].ContainerIDs
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

	r.networkEvtCh = chanx.NewUnboundedChan[ct.EventMessage](r.lifetimeCtx, containerEventChanInitialCapacity)

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
		case em := <-eventCh:
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
		err := r.debouncer.ReconciliationNeeded(owner, networkId, r.scheduleNetworkReconciliation)
		if err != nil {
			r.Log.Error(err, "could not schedule reconcilation for Network object")
		}
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

func (r *NetworkReconciler) scheduleNetworkReconciliation(rti reconcileTriggerInput[string]) error {
	event := ctrl_event.GenericEvent{
		Object: &apiv1.Container{
			ObjectMeta: metav1.ObjectMeta{
				Name:      rti.target.Name,
				Namespace: rti.target.Namespace,
			},
		},
	}
	r.notifyNetworkChanged.In <- event
	return nil
}

func (r *NetworkReconciler) cancelNetworkWatch() {
	if r.networkEvtWorkerStop != nil {
		close(r.networkEvtWorkerStop)
		r.networkEvtWorkerStop = nil
	}
	if r.networkEvtSub != nil {
		_ = r.networkEvtSub.Cancel()
		r.networkEvtSub = nil
	}
}

func (r *NetworkReconciler) onShutdown() {
	<-r.lifetimeCtx.Done()
	r.lock.Lock()
	defer r.lock.Unlock()
	r.cancelNetworkWatch()
}
