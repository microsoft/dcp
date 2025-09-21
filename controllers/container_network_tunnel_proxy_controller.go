// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	stdproto "google.golang.org/protobuf/proto"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	"github.com/microsoft/usvc-apiserver/internal/dcppaths"
	"github.com/microsoft/usvc-apiserver/internal/dcpproc"
	"github.com/microsoft/usvc-apiserver/internal/dcptun"
	dcptunproto "github.com/microsoft/usvc-apiserver/internal/dcptun/proto"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type tunnelProxyStateInitializerFunc = stateInitializerFunc[
	apiv1.ContainerNetworkTunnelProxy, *apiv1.ContainerNetworkTunnelProxy,
	ContainerNetworkTunnelProxyReconciler, *ContainerNetworkTunnelProxyReconciler,
	apiv1.ContainerNetworkTunnelProxyState,
	containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData,
]

// In case of ContainerNetworkTunnelProxy, the "state key" for its ObjectStateMap is the ContainerNetworkTunnelProxy's namespaced name;
// we do not use the state key for manipulating the tunnel proxy data, but it must be unique for each tunnel proxy.
type tunnelProxyDataMap = ObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData]

const (
	containerNetworkNameKey = ".metadata.containerNetworkName"
	serviceReferencesKey    = ".metadata.serviceReferences"

	clientProxyContainerCleanupTimeout = 5 * time.Second
	serverProxyConfigReadTimeout       = 10 * time.Second

	// Timeout for tunnel operations (like preparation or deletion of a tunnel)
	tunnelOperationTimeout = 5 * time.Second

	maxTunnelPreparationAttempts = 20

	// Annotation for an Endpoint object that links it to a specific tunnel that serves it.
	TunnelIdAnnotation = "container-network-tunnel-proxy.usvc-dev.developer.microsoft.com/tunnel-id"
)

var (
	tunnelProxyFinalizer string = fmt.Sprintf("%s/tunnel-proxy-reconciler", apiv1.GroupVersion.Group)

	tunnelProxyStateInitializers = map[apiv1.ContainerNetworkTunnelProxyState]tunnelProxyStateInitializerFunc{
		apiv1.ContainerNetworkTunnelProxyStateEmpty:         handleNewTunnelProxy,
		apiv1.ContainerNetworkTunnelProxyStatePending:       handleNewTunnelProxy,
		apiv1.ContainerNetworkTunnelProxyStateBuildingImage: ensureTunnelProxyBuildingImageState,
		apiv1.ContainerNetworkTunnelProxyStateStarting:      ensureTunnelProxyStartingState,
		apiv1.ContainerNetworkTunnelProxyStateRunning:       ensureTunnelProxyRunningState,
		apiv1.ContainerNetworkTunnelProxyStateFailed:        ensureTunnelProxyFailedState,
	}
)

type ContainerNetworkTunnelProxyReconcilerConfig struct {
	Orchestrator    containers.ContainerOrchestrator // Mandatory
	ProcessExecutor process.Executor                 // Mandatory

	// The factory function to create a TunnelControlClient used to control the proxy pair.
	// Normal execution uses "real" gRPC client, tests use a stub since most tests do not run real tunnels.
	// Mandatory.
	MakeTunnelControlClient func(grpc.ClientConnInterface) dcptunproto.TunnelControlClient

	// Overrides the most recent image builds file path.
	// Used primarily for testing purposes.
	MostRecentImageBuildsFilePath string
}

type ContainerNetworkTunnelProxyReconciler struct {
	*ReconcilerBase[apiv1.ContainerNetworkTunnelProxy, *apiv1.ContainerNetworkTunnelProxy]

	config ContainerNetworkTunnelProxyReconcilerConfig

	// In-memory state map for ContainerNetworkTunnelProxy objects.
	proxyData *tunnelProxyDataMap

	// A work queue for long-running operations.
	workQueue *resiliency.WorkQueue
}

func NewContainerNetworkTunnelProxyReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	config ContainerNetworkTunnelProxyReconcilerConfig,
	log logr.Logger,
) *ContainerNetworkTunnelProxyReconciler {
	if config.Orchestrator == nil {
		panic("ContainerNetworkTunnelProxyReconcilerConfig.Orchestrator must not be nil")
	}
	if config.ProcessExecutor == nil {
		panic("ContainerNetworkTunnelProxyReconcilerConfig.ProcessExecutor must not be nil")
	}
	if config.MakeTunnelControlClient == nil {
		panic("ContainerNetworkTunnelProxyReconcilerConfig.TunnelControlClientFactory must not be nil")
	}

	base := NewReconcilerBase[apiv1.ContainerNetworkTunnelProxy](client, noCacheClient, log, lifetimeCtx)

	return &ContainerNetworkTunnelProxyReconciler{
		ReconcilerBase: base,
		config:         config,
		proxyData:      NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData](),
		workQueue:      resiliency.NewWorkQueue(lifetimeCtx, resiliency.DefaultConcurrency),
	}
}

func (r *ContainerNetworkTunnelProxyReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	indexer := mgr.GetFieldIndexer()

	err := indexer.IndexField(context.Background(), &apiv1.ContainerNetworkTunnelProxy{}, containerNetworkNameKey, func(rawObj ctrl_client.Object) []string {
		cntp := rawObj.(*apiv1.ContainerNetworkTunnelProxy)
		if cntp.Spec.ContainerNetworkName == "" {
			return nil
		} else {
			return []string{cntp.Spec.ContainerNetworkName}
		}
	})
	if err != nil {
		r.Log.Error(err, "Failed to create index for finding ContainerNetworkTunnelProxies using specific ContainerNetwork")
		return err
	}

	err = indexer.IndexField(context.Background(), &apiv1.ContainerNetworkTunnelProxy{}, serviceReferencesKey, func(rawObj ctrl_client.Object) []string {
		cntp := rawObj.(*apiv1.ContainerNetworkTunnelProxy)
		if len(cntp.Spec.Tunnels) == 0 {
			return nil
		}

		serverServiceNames := slices.Map[apiv1.TunnelConfiguration, string](cntp.Spec.Tunnels, func(t apiv1.TunnelConfiguration) string { return t.ServerServiceName })
		clientServiceNames := slices.Map[apiv1.TunnelConfiguration, string](cntp.Spec.Tunnels, func(t apiv1.TunnelConfiguration) string { return t.ClientServiceName })

		svcUsed := slices.Unique(append(serverServiceNames, clientServiceNames...))
		return svcUsed
	})
	if err != nil {
		r.Log.Error(err, "Failed to create index for finding ContainerNetworkTunnelProxies referencing a Service via one or more of the tunnels")
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv1.ContainerNetworkTunnelProxy{}).
		Owns(&apiv1.Endpoint{}).
		Watches(&apiv1.Service{}, handler.EnqueueRequestsFromMapFunc(r.reconcileProxiesUsingService), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv1.ContainerNetwork{}, handler.EnqueueRequestsFromMapFunc(r.reconcileProxiesUsingNetwork), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

// Create reconciliation requests for all ContainerNetworkTunnelProxies using the given ContainerNetwork
func (r *ContainerNetworkTunnelProxyReconciler) reconcileProxiesUsingNetwork(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	network := obj.(*apiv1.ContainerNetwork)

	var tunnelProxies apiv1.ContainerNetworkTunnelProxyList
	listOpts := []ctrl_client.ListOption{
		ctrl_client.MatchingFields{containerNetworkNameKey: network.Name},
		ctrl_client.InNamespace(network.GetNamespace()),
	}

	if err := r.List(ctx, &tunnelProxies, listOpts...); err != nil {
		r.Log.Error(err, "Failed to list ContainerNetworkTunnelProxies using ContainerNetwork", "ContainerNetwork", network.Name)
		return nil
	}

	requests := slices.Map[apiv1.ContainerNetworkTunnelProxy, reconcile.Request](tunnelProxies.Items, func(tunnelProxy apiv1.ContainerNetworkTunnelProxy) reconcile.Request {
		return reconcile.Request{NamespacedName: tunnelProxy.NamespacedName()}
	})

	if len(requests) > 0 {
		proxyNames := slices.Map[reconcile.Request, string](requests, func(req reconcile.Request) string { return req.NamespacedName.String() })
		r.Log.V(1).Info("Enqueuing ContainerNetworkTunnelProxy reconciliation requests due to ContainerNetwork change",
			"ContainerNetwork", network.Name,
			"AffectedTunnelProxies", proxyNames,
		)
	}

	return requests
}

func (r *ContainerNetworkTunnelProxyReconciler) reconcileProxiesUsingService(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	service := obj.(*apiv1.Service)

	var tunnelProxies apiv1.ContainerNetworkTunnelProxyList
	listOpts := []ctrl_client.ListOption{
		ctrl_client.MatchingFields{serviceReferencesKey: service.Name},
		ctrl_client.InNamespace(service.GetNamespace()),
	}

	if err := r.List(ctx, &tunnelProxies, listOpts...); err != nil {
		r.Log.Error(err, "Failed to list ContainerNetworkTunnelProxies referencing Service", "Service", service.Name)
		return nil
	}

	requests := slices.Map[apiv1.ContainerNetworkTunnelProxy, reconcile.Request](tunnelProxies.Items, func(tunnelProxy apiv1.ContainerNetworkTunnelProxy) reconcile.Request {
		return reconcile.Request{NamespacedName: tunnelProxy.NamespacedName()}
	})

	if len(requests) > 0 {
		proxyNames := slices.Map[reconcile.Request, string](requests, func(req reconcile.Request) string { return req.NamespacedName.String() })
		r.Log.V(1).Info("Enqueuing ContainerNetworkTunnelProxy reconciliation requests due to Service change",
			"Service", service.Name,
			"AffectedTunnelProxies", proxyNames,
		)
	}

	return requests
}

func (r *ContainerNetworkTunnelProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	tproxy := apiv1.ContainerNetworkTunnelProxy{}
	err := reader.Get(ctx, req.NamespacedName, &tproxy)

	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.V(1).Info("ContainerNetworkTunnelProxy object was not found")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "Failed to Get() the ContainerNetworkTunnelProxy object")
			getFailedCounter.Add(ctx, 1)
			return ctrl.Result{}, err
		}
	} else {
		getSucceededCounter.Add(ctx, 1)
	}

	r.proxyData.RunDeferredOps(req.NamespacedName)

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(tproxy.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if tproxy.DeletionTimestamp != nil && !tproxy.DeletionTimestamp.IsZero() {
		log.Info("ContainerNetworkTunnelProxy object is being deleted")
		change = r.handleDeletionRequest(ctx, &tproxy, log)
	} else {
		change = ensureFinalizer(&tproxy, tunnelProxyFinalizer, log)
		if change == noChange {
			change = r.manageTunnelProxy(ctx, &tproxy, log)
		}
	}

	result, err := r.SaveChangesWithDelay(ctx, &tproxy, patch, change, StandardDelay, nil, log)
	return result, err
}

func (r *ContainerNetworkTunnelProxyReconciler) handleDeletionRequest(ctx context.Context, tunnelProxy *apiv1.ContainerNetworkTunnelProxy, log logr.Logger) objectChange {
	namespacedName := tunnelProxy.NamespacedName()
	_, pd := r.proxyData.BorrowByNamespacedName(namespacedName)
	var change objectChange = noChange

	switch {
	case pd == nil || pd.State == apiv1.ContainerNetworkTunnelProxyStateFailed || pd.State == apiv1.ContainerNetworkTunnelProxyStateEmpty || pd.State == apiv1.ContainerNetworkTunnelProxyStatePending:
		log.V(1).Info("ContainerNetworkTunnelProxy is being deleted (no resources to clean up, deleting finalizer only)...")
		change = deleteFinalizer(tunnelProxy, tunnelProxyFinalizer, log)

	case pd.State == apiv1.ContainerNetworkTunnelProxyStateBuildingImage || pd.State == apiv1.ContainerNetworkTunnelProxyStateStarting:
		log.V(1).Info("ContainerNetworkTunnelProxy is being deleted; waiting for it to exit transient state...")
		change = r.manageTunnelProxy(ctx, tunnelProxy, log)

	case pd.ServerProxyProcessID == nil && pd.ClientProxyContainerID == "":
		log.V(1).Info("ContainerNetworkTunnelProxy is being deleted (resource cleanup finished, deleting finalizer)...")
		change = deleteFinalizer(tunnelProxy, tunnelProxyFinalizer, log)

	default:
		if !pd.cleanupScheduled {
			pd.cleanupScheduled = true
			r.proxyData.Update(namespacedName, namespacedName, pd)

			log.V(1).Info("ContainerNetworkTunnelProxy is being deleted (scheduling resource cleanup)...")
			cleanupErr := r.workQueue.Enqueue(r.cleanupProxyPair(tunnelProxy, pd.Clone(), log))
			if cleanupErr != nil {
				// Should never happen. This means we (the reconciler) have been shut down via lifetime context
				// with some tunnel proxy instances still running. Just give up on the cleanup here
				// and rely on the dcpproc to do the cleanup instead.
				log.Error(cleanupErr, "Failed to schedule tunnel proxy cleanup work, deleting instance without cleanup...")
				change = deleteFinalizer(tunnelProxy, tunnelProxyFinalizer, log)
			} else {
				log.V(1).Info("Scheduled asynchronous cleanup for ContainerNetworkTunnelProxy proxy pair")
			}
		}
	}

	return change
}

func (r *ContainerNetworkTunnelProxyReconciler) manageTunnelProxy(ctx context.Context, tunnelProxy *apiv1.ContainerNetworkTunnelProxy, log logr.Logger) objectChange {
	targetProxyState := tunnelProxy.Status.State
	_, pd := r.proxyData.BorrowByNamespacedName(tunnelProxy.NamespacedName())
	if pd != nil {
		targetProxyState = pd.State
	}

	initializer := getStateInitializer(tunnelProxyStateInitializers, targetProxyState, log)
	change := initializer(ctx, r, tunnelProxy, targetProxyState, pd, log)

	if pd != nil {
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	}

	return change
}

func (r *ContainerNetworkTunnelProxyReconciler) setTunnelProxyState(tproxy *apiv1.ContainerNetworkTunnelProxy, state apiv1.ContainerNetworkTunnelProxyState) objectChange {
	change := noChange

	if tproxy.Status.State != state {
		tproxy.Status.State = state
		change = statusChanged
	}

	return change
}

// STATE INITIALIZER FUNCTIONS

func handleNewTunnelProxy(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	containerNetworkName := commonapi.AsNamespacedName(tunnelProxy.Spec.ContainerNetworkName, tunnelProxy.Namespace)
	containerNetwork := apiv1.ContainerNetwork{}
	tryAgain := false
	err := r.Get(ctx, containerNetworkName, &containerNetwork)

	switch {
	case apimachinery_errors.IsNotFound(err):
		tryAgain = true
		log.V(1).Info("Referenced ContainerNetwork not found", "ContainerNetwork", containerNetworkName.String())

	case err != nil:
		tryAgain = true
		log.Error(err, "Failed to get referenced ContainerNetwork", "ContainerNetwork", containerNetworkName.String())

	case containerNetwork.Status.State != apiv1.ContainerNetworkStateRunning || containerNetwork.Status.ID == "":
		tryAgain = true
		log.V(1).Info("Referenced ContainerNetwork is not in Running state",
			"ContainerNetwork", containerNetworkName.String(),
			"NetworkState", containerNetwork.Status.State,
			"NetworkID", containerNetwork.Status.ID)
	}

	if tryAgain {
		change := r.setTunnelProxyState(tunnelProxy, apiv1.ContainerNetworkTunnelProxyStatePending)
		return change | additionalReconciliationNeeded
	}

	return r.setTunnelProxyState(tunnelProxy, apiv1.ContainerNetworkTunnelProxyStateBuildingImage)
}

func ensureTunnelProxyBuildingImageState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	change := noChange

	if pd == nil {
		log.V(1).Info("Making sure the container proxy image is up to date...")
		pd = newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateBuildingImage)

		startImgCheckErr := r.workQueue.Enqueue(r.ensureContainerProxyImage(tunnelProxy, pd.Clone(), log))
		if startImgCheckErr != nil {
			log.Error(startImgCheckErr, "Container image check for container network tunnel could not be queued, possibly because the workload is shutting down")
			change |= additionalReconciliationNeeded
		}

		r.proxyData.Store(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	}

	// Regardless whether we just scheduled an image check, or it has been going for a while,
	// we need to ensure that the object state is correct.
	return change | pd.applyTo(tunnelProxy)
}

func ensureTunnelProxyStartingState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	change := noChange

	if pd == nil { // Should never happen when we reach this state
		log.Error(fmt.Errorf("Data about ContainerNetworkTunnelProxy object is missing"), "",
			"CurrentState", apiv1.ContainerNetworkTunnelProxyStateStarting,
		)
		return r.setTunnelProxyState(tunnelProxy, apiv1.ContainerNetworkTunnelProxyStateFailed)
	}

	if !pd.startupScheduled {
		log.V(1).Info("Starting tunnel proxy...")

		startupErr := r.workQueue.Enqueue(r.startProxyPair(tunnelProxy, pd.Clone(), log))
		if startupErr != nil {
			log.Error(startupErr, "Failed to start tunnel proxy pair, possibly because the workload is shutting down")
			change |= additionalReconciliationNeeded
		} else {
			pd.startupScheduled = true
			_ = r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
		}
	}

	return change | pd.applyTo(tunnelProxy)
}

func ensureTunnelProxyRunningState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	if pd == nil { // Should never happen when we reach this state
		log.Error(fmt.Errorf("Data about ContainerNetworkTunnelProxy object is missing"), "",
			"CurrentState", apiv1.ContainerNetworkTunnelProxyStateRunning,
		)
		return r.setTunnelProxyState(tunnelProxy, apiv1.ContainerNetworkTunnelProxyStateFailed)
	}

	change := r.manageTunnels(ctx, tunnelProxy, pd, log)
	ensureEndpointsForWorkload(ctx, r, tunnelProxy, nil, pd, log)

	return change | pd.applyTo(tunnelProxy)
}

func ensureTunnelProxyFailedState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	// TODO: delete other resources as necessary
	removeEndpointsForWorkload(ctx, r, tunnelProxy, log)
	return pd.applyTo(tunnelProxy)
}

// TUNNEL MANAGEMENT HELPER METHODS

// Compares the current tunnel configuration with the desired configuration.
// Attempts to prepare new tunnels and deletes removed ones.
// This method is called as part of the reconciliation loop and is responsible
// for saving changes to containerNetworkTunnelProxyData as needed.
func (r *ContainerNetworkTunnelProxyReconciler) manageTunnels(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	change := noChange

	// Convert to maps for easier lookup
	specTunnels := maps.SliceToMap(tunnelProxy.Spec.Tunnels, apiv1.TunnelConfiguration.KV)
	currentTunnels := maps.SliceToMap(pd.TunnelStatuses, apiv1.TunnelStatus.KV)

	// Remove tunnels that are no longer in the spec
	for tunnelName, tunnelStatus := range currentTunnels {
		if _, found := specTunnels[tunnelName]; found {
			continue
		}

		tlog := log.WithValues("TunnelName", tunnelName)
		tlog.V(1).Info("Deleting tunnel that is no longer in spec...")

		if r.deleteTunnel(ctx, tunnelProxy, tunnelStatus, pd, tlog) {
			pd.removeTunnelStatus(tunnelName)
			change |= statusChanged
		} else {
			change |= additionalReconciliationNeeded
		}
	}

	// Add or update tunnels from the spec
	// Note that tunnels cannot be redefined in the spec, our type validation prevents that.
	for tunnelName, tunnelConfig := range specTunnels {
		tlog := log.WithValues("TunnelName", tunnelName)
		tunnelStatus, found := currentTunnels[tunnelName]

		if found {
			if tunnelStatus.State == apiv1.TunnelStateEmpty {
				tlog.V(1).Info("Attempting to prepare exiting tunnel...")
				change |= r.prepareTunnel(ctx, tunnelProxy, tunnelConfig, tunnelStatus, pd, tlog)
			}
		} else {
			tlog.V(1).Info("Preparing new tunnel...")
			tunnelStatus = apiv1.TunnelStatus{
				Name:      tunnelName,
				State:     apiv1.TunnelStateEmpty,
				Timestamp: metav1.NewMicroTime(time.Now()),
			}
			pd.setTunnelStatus(tunnelStatus)
			change |= statusChanged // Added new tunnel, so we definitively have a status change
			change |= r.prepareTunnel(ctx, tunnelProxy, tunnelConfig, tunnelStatus, pd, tlog)
		}
	}

	if (change & statusChanged) == statusChanged {
		pd.TunnelConfigurationVersion++
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	}

	return change
}

// Attempts to prepare a tunnel by checking required services and calling the tunnel proxy's PrepareTunnel API.
// Returns objectChange value indicating whether any changes have been made to tunnel status.
func (r *ContainerNetworkTunnelProxyReconciler) prepareTunnel(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	tunnelConfig apiv1.TunnelConfiguration,
	originalTunnelStatus apiv1.TunnelStatus,
	pd *containerNetworkTunnelProxyData,
	tlog logr.Logger,
) objectChange {
	te, found := pd.tunnelExtra[tunnelConfig.Name]
	if found && !te.nextPreparationNoEarlierThan.IsZero() && time.Now().Before(te.nextPreparationNoEarlierThan) {
		// We do not want to busy-loop on preparation attempts
		return additionalReconciliationNeeded
	}

	serverSvc, _, servicesReady := r.getTunnelServices(ctx, tunnelConfig, tlog)
	if !servicesReady {
		return additionalReconciliationNeeded
	}

	// CONSIDER: having a spec property for choosing server proxy control address
	// (the one that server proxy listens on for control commands)

	te.preparationAttempts++
	if te.preparationAttempts > maxTunnelPreparationAttempts {
		tlog.Error(errors.New("maximum number of preparation attempts reached"), "Failed to prepare tunnel")
		pd.setTunnelStatus(failedTunnelStatus(originalTunnelStatus, "Failed to prepare tunnel (maximum number of preparation attempts reached)"))
		return statusChanged
	}

	// Set the next preparation earliest time to 90% of the standard delay for additional reconciliation.
	te.nextPreparationNoEarlierThan = time.Now().Add(delayDuration(StandardDelay) / 9 * 10)
	pd.tunnelExtra[tunnelConfig.Name] = te
	r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)

	serverProxyClient, serverProxyClientErr := r.createProxyClient(tunnelProxy)
	if serverProxyClientErr != nil {
		// This should really never happen. No I/O is performed here; the error most likely indicates misconfiguration of the gRPC client.
		tlog.Error(serverProxyClientErr, "Failed to create gRPC connection to server proxy control endpoint")
		pd.setTunnelStatus(failedTunnelStatus(originalTunnelStatus, fmt.Sprintf("Failed to create gRPC connection to server proxy control endpoint: %v", serverProxyClientErr)))
		return additionalReconciliationNeeded
	}

	tunnelReq := &dcptunproto.TunnelReq{
		ServerAddress: stdproto.String(serverSvc.Status.EffectiveAddress),
		ServerPort:    stdproto.Int32(serverSvc.Status.EffectivePort),
		// ClientProxyAddress and ClientProxyPort are omitted; we rely on dcptun defaults,
		// which are 0.0.0.0 (all IPv4 interfaces) and 0 (random port assigned by OS).
	}
	prepareCtx, prepareCtxCancel := context.WithTimeout(ctx, tunnelOperationTimeout)
	defer prepareCtxCancel()
	tSpec, prepareErr := serverProxyClient.PrepareTunnel(prepareCtx, tunnelReq, grpc.WaitForReady(true))
	if prepareErr != nil {
		tlog.Error(prepareErr, "Failed to prepare tunnel, will retry...")
		return additionalReconciliationNeeded
	}

	tlog.V(1).Info("Tunnel prepared successfully")
	ts := originalTunnelStatus.Clone()
	ts.State = apiv1.TunnelStateReady
	ts.TunnelID = tSpec.GetTunnelRef().GetTunnelId()
	ts.Timestamp = metav1.NewMicroTime(time.Now())
	ts.ClientProxyAddresses = tSpec.GetClientProxyAddresses()
	ts.ClientProxyPort = tSpec.GetClientProxyPort()
	pd.setTunnelStatus(ts)

	te.preparationAttempts = 0
	te.nextPreparationNoEarlierThan = time.Time{}
	pd.tunnelExtra[tunnelConfig.Name] = te
	r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)

	return statusChanged
}

// deleteTunnel attempts to delete an existing tunnel.
// Returns true if the tunnel was successfully deleted, false if retry is needed.
func (r *ContainerNetworkTunnelProxyReconciler) deleteTunnel(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	tunnelStatus apiv1.TunnelStatus,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) bool {
	serverProxyClient, serverProxyClientErr := r.createProxyClient(tunnelProxy)
	if serverProxyClientErr != nil {
		// This should really never happen. No I/O is performed here; the error most likely indicates misconfiguration of the gRPC client.
		log.Error(serverProxyClientErr, "Failed to create gRPC connection to server proxy control endpoint")
		return false
	}

	tunnelRef := &dcptunproto.TunnelRef{TunnelId: stdproto.Uint32(tunnelStatus.TunnelID)}
	deleteCtx, deleteCtxCancel := context.WithTimeout(ctx, tunnelOperationTimeout)
	defer deleteCtxCancel()
	_, deleteErr := serverProxyClient.DeleteTunnel(deleteCtx, tunnelRef, grpc.WaitForReady(true))
	if deleteErr != nil {
		log.Error(deleteErr, "Failed to delete a tunnel")
		return false
	}

	// We also need to remove the Endpoint objects created for this tunnel.
	// ensureEndpointsForWorkload() will not do this because the TunnelConfiguration no longer exists in the spec
	// and our DynamicEndpointProducer will not say that this ContainerNetworkTunnelProxy produces
	// the Service associated with deleted TunnelConfiguration.

	te := pd.tunnelExtra[tunnelStatus.Name]
	endpoints := te.clientServiceEndpointNames
	for _, epNN := range endpoints {
		ep := &apiv1.Endpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:      epNN.Name,
				Namespace: epNN.Namespace,
			},
		}

		epErr := r.Client.Delete(ctx, ep, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground))
		if epErr != nil && !apimachinery_errors.IsNotFound(epErr) {
			log.Error(epErr, "Failed to delete Endpoint associated with deleted tunnel", "Endpoint", epNN.String())
			return false
		}
	}

	// Successfully deleted all endpoints, we can now delete the tunnel extra data
	delete(pd.tunnelExtra, tunnelStatus.Name)

	return true
}

// Checks if the Services used by the tunnel exist and are in the correct state.
// Returns both services (if available), and a flag indicating whether both services
// meet requirements for preparing the tunnel.
func (r *ContainerNetworkTunnelProxyReconciler) getTunnelServices(
	ctx context.Context,
	tunnelConfig apiv1.TunnelConfiguration,
	tlog logr.Logger,
) (*apiv1.Service, *apiv1.Service, bool) {
	serverSvcNN := types.NamespacedName{Name: tunnelConfig.ServerServiceName, Namespace: tunnelConfig.ServerServiceNamespace}
	serverService := apiv1.Service{}
	err := r.Get(ctx, serverSvcNN, &serverService)
	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			tlog.V(1).Info("Server service required by the tunnel not found", "ServerService", serverSvcNN.String())
		} else {
			tlog.Error(err, "Failed to get information about server service required by the tunnel", "ServerService", serverSvcNN.String())
		}
		return nil, nil, false
	}

	if serverService.Status.State != apiv1.ServiceStateReady {
		tlog.V(1).Info("Server service required by the tunnel is not in Ready state", "ServerService", serverSvcNN.String())
		return &serverService, nil, false
	}

	clientServiceNN := types.NamespacedName{Name: tunnelConfig.ClientServiceName, Namespace: tunnelConfig.ClientServiceNamespace}
	clientService := apiv1.Service{}
	err = r.Get(ctx, clientServiceNN, &clientService)
	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			tlog.V(1).Info("Client service required by the tunnel not found", "ClientService", clientServiceNN.String())
		} else {
			tlog.Error(err, "Failed to get information about client service required by the tunnel", "ClientService", clientServiceNN.String())
		}
		return &serverService, nil, false
	}

	return &serverService, &clientService, true
}

func failedTunnelStatus(original apiv1.TunnelStatus, errorMessage string) apiv1.TunnelStatus {
	ts := original.Clone()
	ts.ErrorMessage = errorMessage
	ts.Timestamp = metav1.NewMicroTime(time.Now())
	ts.State = apiv1.TunnelStateFailed
	return ts
}

func (r *ContainerNetworkTunnelProxyReconciler) createProxyClient(
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
) (dcptunproto.TunnelControlClient, error) {
	serverProxyConn, serverProxyErr := grpc.NewClient(
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, tunnelProxy.Status.ServerProxyControlPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if serverProxyErr != nil {
		return nil, serverProxyErr
	}
	serverProxyClient := r.config.MakeTunnelControlClient(serverProxyConn)
	return serverProxyClient, nil
}

// INITIALIZATION AND SHUTDOWN HELPER METHODS

// Returns a function that ensures the container proxy image is up to date.
// The method is called as part of the reconciliation loop, but the returned function is executed asynchronously.
// The passed proxy data is a clone independent from what is stored in r.proxyData map.
func (r *ContainerNetworkTunnelProxyReconciler) ensureContainerProxyImage(
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) func(context.Context) {
	return func(ctx context.Context) {
		opts := dcptun.BuildClientProxyImageOptions{
			// TODO: set StreamCommandOptions here to capture the logs of the image build process
			MostRecentImageBuildsFilePath: r.config.MostRecentImageBuildsFilePath,
		}

		image, imageCheckErr := dcptun.EnsureClientProxyImage(ctx, opts, r.config.Orchestrator, log)
		if imageCheckErr != nil {
			log.Error(imageCheckErr, "Container image check for container network tunnel could not be queued")
			pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		} else {
			log.V(1).Info("Container image check for container network tunnel completed successfully", "Image", image)
			pd.State = apiv1.ContainerNetworkTunnelProxyStateStarting
			pd.ClientProxyContainerImage = image
		}

		nn := tunnelProxy.NamespacedName()
		pdMap := r.proxyData
		pdMap.QueueDeferredOp(nn, func(_ types.NamespacedName, _ types.NamespacedName) {
			pdMap.Update(nn, nn, pd)
		})
		r.ScheduleReconciliation(nn)
	}
}

// Returns a function that starts the tunnel proxy pair.
// The method is called as part of the reconciliation loop, but the returned function is executed asynchronously.
// The passed proxy data is a clone independent from what is stored in r.proxyData map.
func (r *ContainerNetworkTunnelProxyReconciler) startProxyPair(
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) func(context.Context) {
	return func(ctx context.Context) {
		clientCtrCreated, reconciliationDelay := r.startClientProxy(ctx, tunnelProxy, pd, log)

		if clientCtrCreated {
			// Start server proxy now that client proxy ports are known
			serverStarted := r.startServerProxy(ctx, tunnelProxy, pd, log)
			if serverStarted {
				log.V(1).Info("Server proxy started successfully, scheduling reconciliation")
				pd.State = apiv1.ContainerNetworkTunnelProxyStateRunning
			}
		}

		nn := tunnelProxy.NamespacedName()
		pdMap := r.proxyData
		pdMap.QueueDeferredOp(nn, func(_ types.NamespacedName, _ types.NamespacedName) {
			pdMap.Update(nn, nn, pd)
		})
		r.ScheduleReconciliationWithDelay(nn, reconciliationDelay)
	}
}

// Starts the client proxy container.
// The passed containerNetworkTunnelProxy data will be updated, reflecting success or failure of the client proxy start.
// In either case the caller should schedule a reconciliation of the given tunnel proxy object.
// Return value indicates whether the start was successful or not,
// and whether the reconciliation should be scheduled immediately, or after a delay.
func (r *ContainerNetworkTunnelProxyReconciler) startClientProxy(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) (bool, AdditionalReconciliationDelay) {
	clientProxyCtrName, _, nameErr := MakeUniqueName(tunnelProxy.Name)
	if nameErr != nil {
		// This would be quite unusual and mean the random number generator failed.
		log.Error(nameErr, "Failed to create a unique name for the client proxy container")
		pd.startupScheduled = false // Reset startupScheduled flag as means of forcing a retry after potentially transient error.
		return false, StandardDelay
	}

	containerNetworkName := commonapi.AsNamespacedName(tunnelProxy.Spec.ContainerNetworkName, tunnelProxy.Namespace)
	containerNetwork := apiv1.ContainerNetwork{}
	cnErr := r.Get(ctx, containerNetworkName, &containerNetwork)
	if cnErr != nil {
		log.Error(cnErr, "Failed to retrieve ContainerNetwork data necessary for starting the client proxy container")
		pd.startupScheduled = false
		return false, StandardDelay
	}
	if containerNetwork.Status.State != apiv1.ContainerNetworkStateRunning || containerNetwork.Status.ID == "" {
		log.V(1).Info("Referenced ContainerNetwork is not in Running state, cannot start the client proxy container")
		pd.startupScheduled = false
		return false, StandardDelay
	}

	log.V(1).Info("Starting client proxy container...")
	createOpts := containers.CreateContainerOptions{
		ContainerSpec: apiv1.ContainerSpec{
			Image:   pd.ClientProxyContainerImage,
			Command: dcptun.ClientProxyBinaryPath,
			Args: []string{
				"client",
				"--client-control-address", networking.IPv4AllInterfaceAddress,
				"--client-control-port", strconv.Itoa(dcptun.DefaultContainerProxyControlPort),
				"--client-data-address", networking.IPv4AllInterfaceAddress,
				"--client-data-port", strconv.Itoa(dcptun.DefaultContainerProxyDataPort),
			},
			Ports: []apiv1.ContainerPort{
				{ContainerPort: dcptun.DefaultContainerProxyControlPort},
				{ContainerPort: dcptun.DefaultContainerProxyDataPort},
			},
		},
		Name:    clientProxyCtrName,
		Network: containerNetwork.Status.ID,
	}

	thisProcess, thisProcessErr := process.This()
	if thisProcessErr != nil {
		log.Error(thisProcessErr, "could not get the current process information; container will not have creator process information")
	} else {
		createOpts.ContainerSpec.Labels = append(createOpts.ContainerSpec.Labels, apiv1.ContainerLabel{
			Key:   CreatorProcessIdLabel,
			Value: fmt.Sprintf("%d", thisProcess.Pid),
		})
		createOpts.ContainerSpec.Labels = append(createOpts.ContainerSpec.Labels, apiv1.ContainerLabel{
			Key:   CreatorProcessStartTimeLabel,
			Value: thisProcess.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		})
	}

	created, createErr := createContainer(ctx, r.config.Orchestrator, createOpts)
	if createErr != nil {
		log.Error(createErr, "Failed to create client proxy container")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false, NoDelay
	}

	pd.ClientProxyContainerID = created.Id
	cleanupContainer := false
	defer func() {
		if !cleanupContainer {
			return
		}
		r.cleanupClientContainer(ctx, created.Id)
		pd.ClientProxyContainerID = ""
	}()

	started, startErr := startContainer(ctx, r.config.Orchestrator, clientProxyCtrName, created.Id, containers.StreamCommandOptions{})
	if startErr != nil {
		log.Error(startErr, "Failed to start client proxy container")
		cleanupContainer = true
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false, NoDelay
	}

	_, controlEndpointHostPort, controlEndpointErr := getHostAddressAndPortForContainerPort(
		createOpts.ContainerSpec, dcptun.DefaultContainerProxyControlPort, started, log,
	)
	if controlEndpointErr != nil {
		// Error already logged
		cleanupContainer = true
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false, NoDelay
	}

	_, dataEndpointHostPort, dataEndpointErr := getHostAddressAndPortForContainerPort(
		createOpts.ContainerSpec, dcptun.DefaultContainerProxyDataPort, started, log,
	)
	if dataEndpointErr != nil {
		// Error already logged
		cleanupContainer = true
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false, NoDelay
	}

	dcpproc.RunContainerWatcher(r.config.ProcessExecutor, created.Id, log)

	pd.ClientProxyControlPort = controlEndpointHostPort
	pd.ClientProxyDataPort = dataEndpointHostPort
	return true, NoDelay
}

// Starts the server proxy as an OS process.
// Assumes that the client proxy container has been started and data about it has already been applied
// to the passed containerNetworkTunnelProxyData instance.
// Updates the provided proxy data with process ID, startup timestamp, stdout/stderr capture files, and server control port.
// Returns true if everything went well and the server proxy has been started successfully.
func (r *ContainerNetworkTunnelProxyReconciler) startServerProxy(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) bool {
	binDir, binDirErr := dcppaths.GetDcpBinDir()
	if binDirErr != nil {
		log.Error(binDirErr, "Failed to locate DCP bin directory for container tunnel server proxy binary")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false
	}
	dcptunPath := filepath.Join(binDir, dcptun.ServerBinaryName)

	startFailed := false
	defer func() {
		if !startFailed {
			return
		}
		if pd.serverStdout != nil {
			_ = pd.serverStdout.Close()
			pd.serverStdout = nil
			pd.ServerProxyStdOutFile = ""
		}
		if pd.serverStderr != nil {
			_ = pd.serverStderr.Close()
			pd.serverStderr = nil
			pd.ServerProxyStdErrFile = ""
		}
	}()

	stdoutFile, stdoutErr := usvc_io.OpenTempFile(fmt.Sprintf("%s_out_%s", tunnelProxy.Name, tunnelProxy.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stdoutErr != nil {
		startFailed = true
		log.Error(stdoutErr, "Failed to create stdout temp file for container tunnel server proxy")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false
	} else {
		pd.ServerProxyStdOutFile = stdoutFile.Name()
		pd.serverStdout = stdoutFile
	}

	stderrFile, stderrErr := usvc_io.OpenTempFile(fmt.Sprintf("%s_err_%s", tunnelProxy.Name, tunnelProxy.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stderrErr != nil {
		startFailed = true
		log.Error(stderrErr, "Failed to create stderr temp file for container tunnel server proxy")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false
	} else {
		pd.ServerProxyStdErrFile = stderrFile.Name()
		pd.serverStderr = stderrFile
	}

	args := []string{
		"server",
		// We rely on the defaults for server control address and port (localhost:0, i.e. auto-allocated port), so not specifying them here.
		networking.IPv4LocalhostDefaultAddress, // Client control address--as exposed by container orchestrator
		strconv.Itoa(int(pd.ClientProxyControlPort)),
		networking.IPv4LocalhostDefaultAddress, // Client data address--as exposed by container orchestrator
		strconv.Itoa(int(pd.ClientProxyDataPort)),
	}

	cmd := exec.Command(dcptunPath, args...)
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile
	cmd.Env = os.Environ()
	logger.WithSessionId(cmd)

	// Start process and wait until the first JSON line is printed to stdout indicating server control address/port
	exitHandler := process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
		if err != nil {
			log.Error(err, "Tunnel server proxy process exited with error", "PID", pid, "ExitCode", exitCode)
		} else if exitCode != 0 {
			log.Error(fmt.Errorf("tunnel server proxy process exited with non-zero exit code %d", exitCode), "Tunnel server proxy process exited abnormally", "PID", pid)
		}
		if closeErr := stdoutFile.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			log.Error(closeErr, "Failed to close stdout file for tunnel server proxy process", "PID", pid)
		}
		if closeErr := stderrFile.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			log.Error(closeErr, "Failed to close stderr file for tunnel server proxy process", "PID", pid)
		}
	})
	pid, startTime, startWaitForExit, startErr := r.config.ProcessExecutor.StartProcess(ctx, cmd, exitHandler, process.CreationFlagsNone)
	if startErr != nil {
		log.Error(startErr, "Failed to start server proxy process")
		startFailed = true
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false
	}
	startWaitForExit()

	tc, tcErr := readServerProxyConfig(ctx, stdoutFile.Name())
	if tcErr != nil {
		log.Error(tcErr, "Failed to read connection information from the server proxy")
		stopProcessErr := r.config.ProcessExecutor.StopProcess(pid, startTime)
		if stopProcessErr != nil {
			log.Error(stopProcessErr, "Failed to stop server proxy process after being unable to read its configuration")
		}
		startFailed = true
		return false
	}

	dcpproc.RunProcessWatcher(r.config.ProcessExecutor, pid, startTime, log)

	pointers.SetValue(&pd.ServerProxyProcessID, int64(pid))
	pd.ServerProxyControlPort = tc.ServerControlPort
	pd.ServerProxyStartupTimestamp = metav1.NewMicroTime(startTime)
	pd.ServerProxyStdOutFile = stdoutFile.Name()
	pd.ServerProxyStdErrFile = stderrFile.Name()

	return true
}

func readServerProxyConfig(ctx context.Context, path string) (dcptun.TunnelProxyConfig, error) {
	configCtx, configCtxCancel := context.WithTimeout(ctx, serverProxyConfigReadTimeout)
	defer configCtxCancel()

	config, err := resiliency.RetryGet(configCtx, backoff.NewConstantBackOff(200*time.Millisecond), func() (dcptun.TunnelProxyConfig, error) {
		f, fErr := usvc_io.OpenFile(path, os.O_RDONLY, 0)
		if fErr != nil {
			return dcptun.TunnelProxyConfig{}, fErr
		}
		defer func() { _ = f.Close() }()

		s := bufio.NewScanner(f)
		if !s.Scan() {
			scanErr := s.Err()
			if scanErr != nil {
				return dcptun.TunnelProxyConfig{}, scanErr
			} else {
				return dcptun.TunnelProxyConfig{}, io.EOF
			}
		}
		var config dcptun.TunnelProxyConfig
		umErr := json.Unmarshal(s.Bytes(), &config)
		if umErr != nil {
			return dcptun.TunnelProxyConfig{}, umErr
		}
		return config, nil
	})

	return config, err
}

// Removes the client proxy container and stops the server proxy process if they exist.
// The method is called as part of the reconciliation loop, but the returned function is executed asynchronously.
// The passed proxy data is a clone independent from what is stored in r.proxyData map.
func (r *ContainerNetworkTunnelProxyReconciler) cleanupProxyPair(
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) func(context.Context) {
	return func(ctx context.Context) {
		if pd.ClientProxyContainerID != "" {
			log.V(1).Info("Removing client proxy container...")

			cleanupCtx, cleanupCancel := context.WithTimeout(ctx, clientProxyContainerCleanupTimeout)
			defer cleanupCancel()

			_, removeErr := r.config.Orchestrator.RemoveContainers(cleanupCtx, containers.RemoveContainersOptions{
				Containers: []string{pd.ClientProxyContainerID},
				Force:      true,
			})

			if removeErr != nil {
				log.Error(removeErr, "Failed to remove client proxy container")
			} else {
				log.V(1).Info("Successfully removed client proxy container")
			}

			// Clear the container ID regardless of whether removal was successful or not
			pd.ClientProxyContainerID = ""
		}

		if pd.ServerProxyProcessID != nil && *pd.ServerProxyProcessID > 0 {
			pid := process.Pid_t(*pd.ServerProxyProcessID)
			startTime := pd.ServerProxyStartupTimestamp.Time

			log.V(1).Info("Stopping server proxy process...")

			stopErr := r.config.ProcessExecutor.StopProcess(pid, startTime)
			if stopErr != nil {
				log.Error(stopErr, "Failed to stop server proxy process")
			} else {
				log.V(1).Info("Successfully stopped server proxy process")
			}

			pd.ServerProxyProcessID = nil
			pd.ServerProxyStartupTimestamp = metav1.MicroTime{} // Zero value
		}

		if pd.serverStdout != nil {
			if closeErr := pd.serverStdout.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
				log.V(1).Info("Error closing server stdout file", "error", closeErr)
			}
			pd.serverStdout = nil
		}
		if pd.serverStderr != nil {
			if closeErr := pd.serverStderr.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
				log.V(1).Info("Error closing server stderr file", "error", closeErr)
			}
			pd.serverStderr = nil
		}

		log.V(1).Info("Completed cleanup of ContainerNetworkTunnelProxy proxy pair")
		nn := tunnelProxy.NamespacedName()
		pdMap := r.proxyData
		pdMap.QueueDeferredOp(nn, func(_ types.NamespacedName, _ types.NamespacedName) {
			pdMap.Update(nn, nn, pd)
		})
		r.ScheduleReconciliation(nn)
	}
}

//
// ENDPOINT OWNER (CREATOR) METHODS
//

// Creates Endpoint object(s) for the given service producer by finding corresponding tunnel
// and ensuring it is in Ready state.
func (r *ContainerNetworkTunnelProxyReconciler) createEndpoints(
	ctx context.Context,
	owner ctrl_client.Object,
	serviceProducer commonapi.ServiceProducer,
	existingEndpoints []*apiv1.Endpoint,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) ([]*apiv1.Endpoint, error) {
	tunnelProxy := owner.(*apiv1.ContainerNetworkTunnelProxy)
	csName := serviceProducer.ServiceNamespacedName()
	csTunnels := pd.tunnelsForClientService(tunnelProxy.Spec.Tunnels, csName)
	if len(csTunnels) == 0 {
		// May be because we did not get to the point of creating the corresponding tunnel yet.
		log.V(1).Info("There are no tunnels that support given client service", "ClientService", csName.String())
		return nil, nil
	}

	readyTunnels := slices.Select(csTunnels, func(t apiv1.TunnelStatus) bool {
		return t.State == apiv1.TunnelStateReady
	})
	if len(readyTunnels) == 0 {
		log.V(1).Info("There are no tunnels in Ready state that support given client service", "ClientService", csName.String())
		return nil, nil
	}

	var retval []*apiv1.Endpoint

	for _, t := range readyTunnels {
		for _, addr := range t.ClientProxyAddresses {
			exists := slices.Any(existingEndpoints, func(ep *apiv1.Endpoint) bool {
				return ep.Spec.Port == t.ClientProxyPort && ep.Spec.Address == addr
				// No need to check service name/namespace as existingEndpoints/readyTunnels is already filtered by that
			})
			if exists {
				continue
			}

			endpointName, _, nameErr := MakeUniqueName(tunnelProxy.Name)
			if nameErr != nil {
				// Should never happen
				log.Error(nameErr, "Failed to create a unique name for the Endpoint object")
				return nil, nameErr
			}

			retval = append(retval, &apiv1.Endpoint{
				ObjectMeta: metav1.ObjectMeta{
					Name:      endpointName,
					Namespace: tunnelProxy.Namespace,
					Annotations: map[string]string{
						TunnelIdAnnotation: strconv.FormatUint(uint64(t.TunnelID), 10),
					},
				},
				Spec: apiv1.EndpointSpec{
					ServiceNamespace: csName.Namespace,
					ServiceName:      csName.Name,
					Address:          addr,
					Port:             t.ClientProxyPort,
				},
			})

			te := pd.tunnelExtra[t.Name]
			endpointNN := types.NamespacedName{Name: endpointName, Namespace: tunnelProxy.Namespace}
			if !slices.Contains(te.clientServiceEndpointNames, endpointNN) {
				te.clientServiceEndpointNames = append(te.clientServiceEndpointNames, endpointNN)
				pd.tunnelExtra[t.Name] = te
				r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
			}
		}

	}
	return retval, nil
}

func (r *ContainerNetworkTunnelProxyReconciler) validateExistingEndpoints(
	ctx context.Context,
	owner ctrl_client.Object,
	serviceProducer commonapi.ServiceProducer,
	existingEndpoints []*apiv1.Endpoint,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) ([]*apiv1.Endpoint, []*apiv1.Endpoint, error) {
	tunnelProxy := owner.(*apiv1.ContainerNetworkTunnelProxy)
	csName := serviceProducer.ServiceNamespacedName()
	csTunnels := pd.tunnelsForClientService(tunnelProxy.Spec.Tunnels, csName)
	if len(csTunnels) == 0 {
		// No new tunnels, and all existing endpoints are invalid
		return nil, existingEndpoints, nil
	}

	var valid, invalid []*apiv1.Endpoint

	for _, ep := range existingEndpoints {
		elog := log.WithValues("Endpoint", ep.NamespacedName().String())

		tunnelIdStr, found := ep.Annotations[TunnelIdAnnotation]
		if !found {
			elog.V(1).Info("Endpoint is missing tunnel ID annotation")
			invalid = append(invalid, ep)
			continue
		}
		tunnelId, parseErr := strconv.ParseUint(tunnelIdStr, 10, 32)
		if parseErr != nil {
			log.V(1).Info("Endpoint has invalid tunnel ID annotation", "TunnelIdAnnotation", tunnelIdStr)
			invalid = append(invalid, ep)
			continue
		}
		i := slices.IndexFunc(csTunnels, func(ts apiv1.TunnelStatus) bool {
			return uint64(ts.TunnelID) == tunnelId
		})
		if i < 0 {
			log.V(1).Info("Endpoint refers to a tunnel that does not exist", "TunnelId", tunnelId)
			invalid = append(invalid, ep)
			continue
		}
		t := csTunnels[i]
		if t.State != apiv1.TunnelStateReady {
			log.V(1).Info("Endpoint refers to a tunnel that is not in Ready state", "TunnelId", tunnelId, "TunnelState", t.State)
			invalid = append(invalid, ep)
			continue
		}
		if ep.Spec.Port != t.ClientProxyPort {
			log.V(1).Info("Endpoint port does not match the port of the tunnel it refers to", "TunnelId", tunnelId, "EndpointPort", ep.Spec.Port, "TunnelPort", t.ClientProxyPort)
			invalid = append(invalid, ep)
			continue
		}
		if !slices.Contains(t.ClientProxyAddresses, ep.Spec.Address) {
			log.V(1).Info("Endpoint address is not among the addresses of the tunnel it refers to", "TunnelId", tunnelId, "EndpointAddress", ep.Spec.Address, "TunnelAddresses", t.ClientProxyAddresses)
			invalid = append(invalid, ep)
			continue
		}

		valid = append(valid, ep)
	}

	return valid, invalid, nil
}

//
// MISCELLANEOUS HELPER METHODS
//

func (r *ContainerNetworkTunnelProxyReconciler) cleanupClientContainer(ctx context.Context, containerID string) {
	removeCtx, removeCtxCancel := context.WithTimeout(ctx, clientProxyContainerCleanupTimeout)
	defer removeCtxCancel()
	_ = removeContainer(removeCtx, r.config.Orchestrator, containerID) // Best effort
}
