/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

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
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcpproc"
	"github.com/microsoft/dcp/internal/dcptun"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/slices"
)

type tunnelProxyStateInitializerFunc = stateInitializerFunc[
	apiv1.ContainerNetworkTunnelProxy, *apiv1.ContainerNetworkTunnelProxy,
	ContainerNetworkTunnelProxyReconciler, *ContainerNetworkTunnelProxyReconciler,
	apiv1.ContainerNetworkTunnelProxyState,
	containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData,
]

// In case of ContainerNetworkTunnelProxy, the "state key" for its ObjectStateMap is the ContainerNetworkTunnelProxy's namespaced name;
// we do not use the state key for manipulating the tunnel proxy data, but it must be unique for each tunnel proxy.
type tunnelProxyDataMap = ObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy]

const (
	containerNetworkNameKey = ".metadata.containerNetworkName"
	serviceReferencesKey    = ".metadata.serviceReferences"

	clientProxyContainerCleanupTimeout = 5 * time.Second
	serverProxyConfigReadTimeout       = 10 * time.Second

	// Timeout for tunnel operations (like preparation or deletion of a tunnel)
	tunnelOperationTimeout = 5 * time.Second

	defaultMaxTunnelPreparationAttempts uint32 = 20

	// Annotation for an Endpoint object that links it to a specific tunnel that serves it.
	TunnelIdAnnotation = "container-network-tunnel-proxy.usvc-dev.developer.microsoft.com/tunnel-id"

	tunnelProxyOwnerNameAnnotation      = "container-network-tunnel-proxy.usvc-dev.developer.microsoft.com/owner-name"
	tunnelProxyOwnerNamespaceAnnotation = "container-network-tunnel-proxy.usvc-dev.developer.microsoft.com/owner-namespace"
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

	// Specifies how many attempts to prepare a tunnel will be made before giving up and marking the tunnel as failed.
	// Defaults to defaultMaxTunnelPreparationAttempts, but much lower value is used for tests to simulate failures quickly.
	MaxTunnelPreparationAttempts uint32
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
	if config.MaxTunnelPreparationAttempts == 0 {
		config.MaxTunnelPreparationAttempts = defaultMaxTunnelPreparationAttempts
	}

	base := NewReconcilerBase[apiv1.ContainerNetworkTunnelProxy](client, noCacheClient, log, lifetimeCtx)

	r := ContainerNetworkTunnelProxyReconciler{
		ReconcilerBase: base,
		config:         config,
		proxyData:      NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
		workQueue:      resiliency.NewWorkQueue(lifetimeCtx, resiliency.DefaultConcurrency),
	}

	return &r
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

		serverServiceNames := slices.Map[string](cntp.Spec.Tunnels, func(t apiv1.TunnelConfiguration) string { return t.ServerServiceName })
		clientServiceNames := slices.Map[string](cntp.Spec.Tunnels, func(t apiv1.TunnelConfiguration) string { return t.ClientServiceName })

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
		Watches(&apiv2.PhysicalContainerImage{}, handler.EnqueueRequestsFromMapFunc(r.reconcileProxyForPhysicalResource), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.PhysicalContainer{}, handler.EnqueueRequestsFromMapFunc(r.reconcileProxyForPhysicalResource), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *ContainerNetworkTunnelProxyReconciler) reconcileProxyForPhysicalResource(_ context.Context, obj ctrl_client.Object) []reconcile.Request {
	if obj.GetNamespace() != V1PhysicalResourcesNamespaceName {
		return nil
	}

	annotations := obj.GetAnnotations()
	ownerName := annotations[tunnelProxyOwnerNameAnnotation]
	if ownerName == "" {
		return nil
	}

	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: annotations[tunnelProxyOwnerNamespaceAnnotation],
			Name:      ownerName,
		},
	}}
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

	requests := slices.Map[reconcile.Request](tunnelProxies.Items, func(tunnelProxy apiv1.ContainerNetworkTunnelProxy) reconcile.Request {
		return reconcile.Request{NamespacedName: tunnelProxy.NamespacedName()}
	})

	if len(requests) > 0 {
		proxyNames := slices.Map[string](requests, func(req reconcile.Request) string { return req.NamespacedName.String() })
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

	requests := slices.Map[reconcile.Request](tunnelProxies.Items, func(tunnelProxy apiv1.ContainerNetworkTunnelProxy) reconcile.Request {
		return reconcile.Request{NamespacedName: tunnelProxy.NamespacedName()}
	})

	if len(requests) > 0 {
		proxyNames := slices.Map[string](requests, func(req reconcile.Request) string { return req.NamespacedName.String() })
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

	r.proxyData.RunDeferredOps(req.NamespacedName, &tproxy)

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
	if pd == nil {
		pd = newContainerNetworkTunnelProxyData(tunnelProxy.Status.State)
		pd.ContainerNetworkTunnelProxyStatus = *tunnelProxy.Status.DeepCopy()
		r.proxyData.Store(namespacedName, namespacedName, pd)
	}
	var change objectChange = noChange

	switch {
	case pd.State == apiv1.ContainerNetworkTunnelProxyStateBuildingImage || pd.State == apiv1.ContainerNetworkTunnelProxyStateStarting:
		log.V(1).Info("ContainerNetworkTunnelProxy is being deleted; waiting for it to exit transient state...")
		change = r.manageTunnelProxy(ctx, tunnelProxy, log)

	case pd.cleanupScheduled && pd.ServerProxyProcessID == nil && pd.ClientProxyContainerID == "":
		log.V(1).Info("ContainerNetworkTunnelProxy is being deleted (resource cleanup finished, deleting finalizer)...")
		change = deleteFinalizer(tunnelProxy, tunnelProxyFinalizer, log)

	default:
		if !pd.cleanupScheduled {
			pd.cleanupScheduled = true
			r.proxyData.Update(namespacedName, namespacedName, pd)

			log.V(1).Info("ContainerNetworkTunnelProxy is being deleted (scheduling resource cleanup)...")
			cleanupErr := r.workQueue.Enqueue(r.startProxyPairCleanup(tunnelProxy, pd.Clone(), log))
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
		r.proxyData.Store(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	}

	if pd.ClientProxyContainerImage == "" && !pd.imagePreparationScheduled {
		pd.imagePreparationScheduled = true
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
		startImgCheckErr := r.workQueue.Enqueue(r.ensureContainerProxyImage(tunnelProxy, pd.Clone(), log))
		if startImgCheckErr != nil {
			log.Error(startImgCheckErr, "Container image check for container network tunnel could not be queued, possibly because the workload is shutting down")
			pd.imagePreparationScheduled = false
			r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
			change |= additionalReconciliationNeeded
		}
	}

	if pd.ClientProxyContainerImage != "" {
		physicalImage := apiv2.PhysicalContainerImage{}
		getImageErr := r.Get(ctx, tunnelProxyPhysicalResourceName(tunnelProxy), &physicalImage)
		switch {
		case apimachinery_errors.IsNotFound(getImageErr):
			if pd.imageBuildContextArchiveSource != "" {
				removeArchiveErr := os.Remove(pd.imageBuildContextArchiveSource)
				if removeArchiveErr != nil && !errors.Is(removeArchiveErr, os.ErrNotExist) {
					log.Error(removeArchiveErr, "Failed to remove orphaned tunnel proxy image build context archive", "Path", pd.imageBuildContextArchiveSource)
					change |= additionalReconciliationNeeded
					break
				}
				pd.imageBuildContextArchiveSource = ""
			}
			pd.ClientProxyContainerImage = ""
			change |= additionalReconciliationNeeded
		case getImageErr != nil:
			log.Error(getImageErr, "Failed to get tunnel proxy PhysicalContainerImage")
			change |= additionalReconciliationNeeded
		default:
			if physicalImage.Spec.Image != nil &&
				physicalImage.Spec.Image.Build != nil &&
				physicalImage.Spec.Image.Build.ContextArchive != nil {
				pd.imageBuildContextArchiveSource = physicalImage.Spec.Image.Build.ContextArchive.Source
			}
			switch physicalImage.Status.Phase {
			case apiv2.PhysicalContainerImagePhaseFailed:
				pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
				pd.Message = fmt.Sprintf("PhysicalContainerImage for container network tunnel failed: %s", physicalResourceStatusMessage(physicalImage.Status.Conditions))
			case apiv2.PhysicalContainerImagePhaseReady:
				pd.State = apiv1.ContainerNetworkTunnelProxyStateStarting
			default:
				change |= additionalReconciliationNeeded
			}
		}
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
		log.Error(fmt.Errorf("data about ContainerNetworkTunnelProxy object is missing"), "",
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
		log.Error(fmt.Errorf("data about ContainerNetworkTunnelProxy object is missing"), "",
			"CurrentState", apiv1.ContainerNetworkTunnelProxyStateRunning,
		)
		return r.setTunnelProxyState(tunnelProxy, apiv1.ContainerNetworkTunnelProxyStateFailed)
	}

	clientContainerReady, clientContainerDelay := r.updateClientProxyContainerStatus(ctx, tunnelProxy, pd, log)
	if !clientContainerReady {
		change := pd.applyTo(tunnelProxy)
		if clientContainerDelay != NoDelay {
			change |= additionalReconciliationNeeded
		}
		return change
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
	change := r.failAllExistingTunnels(tunnelProxy, pd)
	pd.cleanupScheduled = true
	r.cleanupProxyPair(ctx, pd, tunnelProxy.UID, log)
	if !pd.cleanupScheduled {
		change |= additionalReconciliationNeeded
	}
	removeEndpointsForWorkload(ctx, r, tunnelProxy, log)
	return change | pd.applyTo(tunnelProxy)
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

		// Attempt to delete the tunnel once; there is no real benefit in retrying
		_ = r.deleteTunnel(ctx, tunnelProxy, tunnelStatus, pd, tlog)
		pd.removeTunnelStatus(tunnelName)
		change |= statusChanged
	}

	// Add or update tunnels from the spec
	// Note that tunnels cannot be redefined in the spec, our type validation prevents that.
	for tunnelName, tunnelConfig := range specTunnels {
		tlog := log.WithValues("TunnelName", tunnelName)
		tunnelStatus, found := currentTunnels[tunnelName]

		if found {
			tlog.V(1).Info("Making sure exiting tunnel is ready to server traffic...")
			change |= r.manageSingleTunnel(ctx, tunnelProxy, tunnelConfig, tunnelStatus, pd, tlog)
		} else {
			tlog.V(1).Info("Preparing new tunnel...")
			tunnelStatus = apiv1.TunnelStatus{
				Name:      tunnelName,
				State:     apiv1.TunnelStateNotReady,
				Timestamp: metav1.NewMicroTime(time.Now()),
			}
			pd.setTunnelStatus(tunnelStatus)
			change |= statusChanged // Added new tunnel, so we definitively have a status change
			change |= r.manageSingleTunnel(ctx, tunnelProxy, tunnelConfig, tunnelStatus, pd, tlog)
		}
	}

	if (change & statusChanged) == statusChanged {
		pd.TunnelConfigurationVersion++
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	}

	return change
}

// Manages a single tunnel, which involves two main use cases:
//
//  1. For tunnels that are not ready: attempt to prepare a tunnel by checking required services
//     and calling the tunnel proxy's PrepareTunnel API.
//  2. For tunnels that are ready: check that the server service is still ready, otherwise delete the tunnel.
//
// Returns objectChange value indicating whether any changes have been made to tunnel status.
func (r *ContainerNetworkTunnelProxyReconciler) manageSingleTunnel(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	tunnelConfig apiv1.TunnelConfiguration,
	originalTunnelStatus apiv1.TunnelStatus,
	pd *containerNetworkTunnelProxyData,
	tlog logr.Logger,
) objectChange {
	if originalTunnelStatus.State == apiv1.TunnelStateFailed {
		// Failed is a final state; we do not attempt to recover from it.
		// Failed tunnels must be deleted and recreated to be retried.
		return noChange
	}

	clientSvc := r.getTunnelClientService(ctx, tunnelConfig, tlog)
	if clientSvc == nil {
		// The Service may be created later.
		return additionalReconciliationNeeded
	}
	if clientSvc.Spec.AddressAllocationMode != apiv1.AddressAllocationModeProxyless {
		// The client service is not usable--as of today, we do not have proxies running in container space,
		// so the service must be proxyless.
		tlog.Info("Client service is not proxyless, cannot use it with a container network tunnel", "AddressAllocationMode", clientSvc.Spec.AddressAllocationMode)
		return additionalReconciliationNeeded
	}

	serverSvc, serverServiceHasAddress := r.getTunnelServerService(ctx, tunnelConfig, tlog)

	if originalTunnelStatus.State == apiv1.TunnelStateReady {
		if serverServiceHasAddress {
			return noChange // All good, nothing to do
		}

		tlog.V(1).Info("Server service is no longer ready, deleting the tunnel...")

		if r.deleteTunnel(ctx, tunnelProxy, originalTunnelStatus, pd, tlog) {
			pd.setTunnelStatus(apiv1.TunnelStatus{
				Name:      originalTunnelStatus.Name,
				State:     apiv1.TunnelStateNotReady,
				Timestamp: metav1.NewMicroTime(time.Now()),
			})
		} else {
			tlog.V(1).Info("Failed to delete the tunnel after server service became not ready")
			// Remove the tunnel status and treat it as not prepared (create new tunnel when service becomes ready again)
			pd.removeTunnelStatus(originalTunnelStatus.Name)
		}
		return statusChanged
	}

	// The rest of the method handles the main use case: the tunnel is NOT READY and we need to prepare it.

	if !serverServiceHasAddress {
		return additionalReconciliationNeeded
	}

	te, found := pd.tunnelExtra[tunnelConfig.Name]
	if found && !te.nextPreparationNoEarlierThan.IsZero() && time.Now().Before(te.nextPreparationNoEarlierThan) {
		// We do not want to busy-loop on preparation attempts
		return additionalReconciliationNeeded
	}

	// CONSIDER: having a spec property for choosing server proxy control address
	// (the one that server proxy listens on for control commands)

	te.preparationAttempts++
	if te.preparationAttempts > r.config.MaxTunnelPreparationAttempts {
		tlog.Error(errors.New("maximum number of preparation attempts reached"), "Failed to prepare tunnel")
		pd.setTunnelStatus(failedTunnelStatus(originalTunnelStatus, "Failed to prepare tunnel (maximum number of preparation attempts reached)"))
		return statusChanged
	}

	// Set the next preparation earliest time to 90% of the standard delay for additional reconciliation.
	te.nextPreparationNoEarlierThan = time.Now().Add(delayDuration(StandardDelay) / 9 * 10)
	pd.tunnelExtra[tunnelConfig.Name] = te
	r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)

	serverProxyClient, serverProxyClientErr := r.createProxyClient(pd)
	if serverProxyClientErr != nil {
		// This should really never happen. No I/O is performed here; the error most likely indicates misconfiguration of the gRPC client.
		tlog.Error(serverProxyClientErr, "Failed to create gRPC connection to server proxy control endpoint")
		pd.setTunnelStatus(failedTunnelStatus(originalTunnelStatus, fmt.Sprintf("Failed to create gRPC connection to server proxy control endpoint: %v", serverProxyClientErr)))
		return additionalReconciliationNeeded
	}

	tunnelReq := &dcptunproto.TunnelReq{
		ServerAddress: stdproto.String(serverSvc.Status.EffectiveAddress),
		ServerPort:    stdproto.Int32(serverSvc.Status.EffectivePort),
		// Try to expose the tunnel on the same port as the host service first.
		// If binding fails, we retry below with the OS assigning a random port.
		ClientProxyPort: stdproto.Int32(serverSvc.Status.EffectivePort),
	}
	prepareCtx, prepareCtxCancel := context.WithTimeout(ctx, tunnelOperationTimeout)
	defer prepareCtxCancel()
	tSpec, prepareErr := serverProxyClient.PrepareTunnel(prepareCtx, tunnelReq, grpc.WaitForReady(true))
	if prepareErr != nil {
		tlog.V(1).Info("Failed to prepare tunnel on preferred port, falling back to random port", "Error", prepareErr)
		fallbackTunnelReq := &dcptunproto.TunnelReq{
			ServerAddress: stdproto.String(serverSvc.Status.EffectiveAddress),
			ServerPort:    stdproto.Int32(serverSvc.Status.EffectivePort),
		}
		tSpec, prepareErr = serverProxyClient.PrepareTunnel(prepareCtx, fallbackTunnelReq, grpc.WaitForReady(true))
		if prepareErr != nil {
			tlog.Error(prepareErr, "Failed to prepare tunnel, will retry...")
			return additionalReconciliationNeeded
		}
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
	_ *apiv1.ContainerNetworkTunnelProxy,
	tunnelStatus apiv1.TunnelStatus,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) bool {
	serverProxyClient, serverProxyClientErr := r.createProxyClient(pd)
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

func (r *ContainerNetworkTunnelProxyReconciler) failAllExistingTunnels(
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
) objectChange {
	change := noChange

	for _, ts := range tunnelProxy.Status.TunnelStatuses {
		if ts.State != apiv1.TunnelStateFailed {
			pd.setTunnelStatus(failedTunnelStatus(ts, "The container proxy failed and tunnels are no longer usable"))
			change |= statusChanged
		}
	}

	return change
}

// Checks if the server Service used by the tunnel exists and has effective address and port assigned.
// Note that we do not check if the Service is actually in ready state. This allows us to complete tunnel preparation
// as the Server service becomes ready and relaxes the dependency between tunnels (and tunnel client services) and server services.
func (r *ContainerNetworkTunnelProxyReconciler) getTunnelServerService(
	ctx context.Context,
	tunnelConfig apiv1.TunnelConfiguration,
	tlog logr.Logger,
) (*apiv1.Service, bool) {
	serverSvcNN := types.NamespacedName{Name: tunnelConfig.ServerServiceName, Namespace: tunnelConfig.ServerServiceNamespace}
	serverService := apiv1.Service{}
	err := r.Get(ctx, serverSvcNN, &serverService)
	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			tlog.V(1).Info("Server service required by the tunnel not found", "ServerService", serverSvcNN.String())
		} else {
			tlog.Error(err, "Failed to get information about server service required by the tunnel", "ServerService", serverSvcNN.String())
		}
		return nil, false
	}

	if serverService.Status.EffectiveAddress == "" || !networking.IsValidPort((int)(serverService.Status.EffectivePort)) {
		tlog.V(1).Info("Server service required by the tunnel does not have a valid address yet", "ServerService", serverSvcNN.String())
		return &serverService, false
	}

	return &serverService, true
}

// Returns the client Service used by the tunnel.
func (r *ContainerNetworkTunnelProxyReconciler) getTunnelClientService(
	ctx context.Context,
	tunnelConfig apiv1.TunnelConfiguration,
	tlog logr.Logger,
) *apiv1.Service {
	clientSvcNN := types.NamespacedName{Name: tunnelConfig.ClientServiceName, Namespace: tunnelConfig.ClientServiceNamespace}
	clientService := apiv1.Service{}
	err := r.Get(ctx, clientSvcNN, &clientService)
	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			tlog.V(1).Info("Client service required by the tunnel not found", "ClientService", clientSvcNN.String())
		} else {
			tlog.Error(err, "Failed to get information about client service required by the tunnel", "ClientService", clientSvcNN.String())
		}
		return nil
	}

	return &clientService
}

func failedTunnelStatus(original apiv1.TunnelStatus, errorMessage string) apiv1.TunnelStatus {
	ts := original.Clone()
	ts.ErrorMessage = errorMessage
	ts.Timestamp = metav1.NewMicroTime(time.Now())
	ts.State = apiv1.TunnelStateFailed
	return ts
}

func (r *ContainerNetworkTunnelProxyReconciler) createProxyClient(
	pd *containerNetworkTunnelProxyData,
) (dcptunproto.TunnelControlClient, error) {
	if pd.securityConfig == nil {
		return nil, fmt.Errorf("cannot create tunnel proxy client: security configuration is missing") // Should never happen
	}

	clientCertPool, certPoolErr := pd.securityConfig.GetClientPool()
	if certPoolErr != nil {
		return nil, certPoolErr
	}

	serverProxyConn, serverProxyErr := grpc.NewClient(
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, pd.ServerProxyControlPort),
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(clientCertPool, "")),
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
		reconciliationDelay := NoDelay
		opts := dcptun.BuildClientProxyImageOptions{
			// TODO: set StreamCommandOptions here to capture the logs of the image build process
			MostRecentImageBuildsFilePath: r.config.MostRecentImageBuildsFilePath,
		}

		imagePlan, imageCheckErr := dcptun.PrepareClientProxyImage(ctx, opts, r.config.Orchestrator, log)

		if imageCheckErr != nil {
			var rtUnhealthyErr *dcptun.ErrContainerRuntimeUnhealthy
			if errors.As(imageCheckErr, &rtUnhealthyErr) {
				log.V(1).Info("Container runtime is unhealthy, will retry client proxy image check later")
				reconciliationDelay = LongDelay
			} else {
				log.Error(imageCheckErr, "Container image for container network tunnel could not be built, or its presence could not be verified")
				pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
				pd.Message = fmt.Sprintf("Container image for container network tunnel could not be built, or its presence could not be verified: %v", imageCheckErr)
			}
		} else {
			namespaceErr := EnsureV1PhysicalResourcesNamespace(ctx, r.Client)
			if namespaceErr != nil {
				cleanupErr := imagePlan.Cleanup()
				if cleanupErr != nil {
					log.Error(cleanupErr, "Failed to clean up tunnel proxy image build context")
				}
				log.Error(namespaceErr, "Failed to ensure V1 physical resources namespace")
				pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
				pd.Message = fmt.Sprintf("Failed to ensure V1 physical resources namespace: %v", namespaceErr)
			} else {
				imageResourceErr := r.ensureTunnelProxyPhysicalContainerImage(ctx, tunnelProxy, imagePlan)
				if imageResourceErr != nil {
					cleanupErr := imagePlan.Cleanup()
					if cleanupErr != nil {
						log.Error(cleanupErr, "Failed to clean up tunnel proxy image build context")
					}
					log.Error(imageResourceErr, "Failed to create tunnel proxy PhysicalContainerImage")
					pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
					pd.Message = fmt.Sprintf("Failed to create tunnel proxy PhysicalContainerImage: %v", imageResourceErr)
				} else {
					log.V(1).Info("Created PhysicalContainerImage for container network tunnel", "Image", imagePlan.Image)
					pd.ClientProxyContainerImage = imagePlan.Image
				}
			}
		}

		pd.imagePreparationScheduled = false
		nn := tunnelProxy.NamespacedName()
		pdMap := r.proxyData
		pdMap.QueueDeferredOp(nn, func(types.NamespacedName, types.NamespacedName, *apiv1.ContainerNetworkTunnelProxy) {
			pdMap.Update(nn, nn, pd)
		})
		r.ScheduleReconciliationWithDelay(nn, reconciliationDelay)
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
		nn := tunnelProxy.NamespacedName()
		reconciliationDelay := NoDelay

		certErr := r.createProxyConnectionCertificates(pd, log)
		if certErr != nil {
			log.Error(certErr, "Failed to create tunnel proxy connection certificates")
			pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
			pd.Message = fmt.Sprintf("Failed to create tunnel proxy connection certificates: %v", certErr)
		} else {
			var clientCtrCreated bool
			clientCtrCreated, reconciliationDelay = r.startClientProxy(ctx, tunnelProxy, pd, log)

			if clientCtrCreated {
				// Start server proxy now that client proxy ports are known
				serverStarted := r.startServerProxy(ctx, tunnelProxy, pd, log)
				if serverStarted {
					log.V(1).Info("Server proxy started successfully, scheduling reconciliation")
					pd.State = apiv1.ContainerNetworkTunnelProxyStateRunning
				} else {
					reconciliationDelay = StandardDelay
				}
			}
		}

		pd.startupScheduled = false // Reset startupScheduled flag to allow retries
		pdMap := r.proxyData
		pdMap.QueueDeferredOp(nn, func(types.NamespacedName, types.NamespacedName, *apiv1.ContainerNetworkTunnelProxy) {
			pdMap.Update(nn, nn, pd)
		})
		r.ScheduleReconciliationWithDelay(nn, reconciliationDelay)
	}
}

// Creates certificates for security tunnel proxy control connection.
// The passed containerNetworkTunnelProxy data will be updated with the created certificates if the method is successful.
// Regardless of the outcome, the caller should schedule a reconciliation of the given tunnel proxy object.
func (r *ContainerNetworkTunnelProxyReconciler) createProxyConnectionCertificates(
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) error {
	if pd.securityConfig != nil {
		log.V(1).Info("Tunnel proxy connection certificates are already available, nothing to do...")
		return nil
	}

	securityConfig, secConfErr := dcptun.NewTunnelProxySecurityConfig()
	if secConfErr != nil {
		log.Error(secConfErr, "Failed to create security configuration for tunnel proxy connection")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		pd.Message = fmt.Sprintf("Failed to create security configuration for tunnel proxy connection: %v", secConfErr)
		return secConfErr
	}

	pd.securityConfig = &securityConfig
	return nil
}

func (r *ContainerNetworkTunnelProxyReconciler) createProxySecurityArgs(
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) []string {
	certArgs := []string{}

	if pd.securityConfig != nil {
		certArgs = []string{
			"--ca-cert", pd.securityConfig.CACertBase64,
			"--server-cert", pd.securityConfig.ServerCertBase64,
			"--server-key", pd.securityConfig.ServerKeyBase64,
		}
		log.V(1).Info("Configuring client proxy with TLS certificates for secure control connection")
	} else {
		// Should never happen
		log.Error(fmt.Errorf("tunnel certificates are missing"), "Client proxy will use insecure control connection")
	}

	return certArgs
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
	containerNetworkName := commonapi.AsNamespacedName(tunnelProxy.Spec.ContainerNetworkName, tunnelProxy.Namespace)
	containerNetwork := apiv1.ContainerNetwork{}
	cnErr := r.Get(ctx, containerNetworkName, &containerNetwork)
	if cnErr != nil {
		log.Error(cnErr, "Failed to retrieve ContainerNetwork data necessary for starting the client proxy container")
		pd.startupScheduled = false
		return false, StandardDelay
	}
	if containerNetwork.Status.State != apiv1.ContainerNetworkStateRunning || containerNetwork.Status.ID == "" || containerNetwork.Status.NetworkName == "" {
		log.V(1).Info("Referenced ContainerNetwork is not in Running state, cannot start the client proxy container")
		pd.startupScheduled = false
		return false, StandardDelay
	}

	physicalContainerName := tunnelProxyPhysicalResourceName(tunnelProxy)
	physicalContainer := apiv2.PhysicalContainer{}
	getContainerErr := r.Get(ctx, physicalContainerName, &physicalContainer)
	if apimachinery_errors.IsNotFound(getContainerErr) {
		physicalContainer = apiv2.PhysicalContainer{
			ObjectMeta: metav1.ObjectMeta{
				Name:        physicalContainerName.Name,
				Namespace:   physicalContainerName.Namespace,
				Annotations: tunnelProxyPhysicalResourceAnnotations(tunnelProxy),
			},
			Spec: apiv2.PhysicalContainerSpec{
				Container: &apiv2.PhysicalContainerConfig{
					ImageRef:      physicalContainerName.Name,
					ContainerName: physicalContainerName.Name,
					Entrypoint:    dcptun.ClientProxyBinaryPath,
					Command: append([]string{
						"client",
						"--client-control-address", networking.IPv4AllInterfaceAddress,
						"--client-control-port", strconv.Itoa(dcptun.DefaultContainerProxyControlPort),
						"--client-data-address", networking.IPv4AllInterfaceAddress,
						"--client-data-port", strconv.Itoa(dcptun.DefaultContainerProxyDataPort),
					}, r.createProxySecurityArgs(pd, log)...),
					Ports: []apiv2.ContainerPort{
						{ContainerPort: int32(dcptun.DefaultContainerProxyControlPort)},
						{ContainerPort: int32(dcptun.DefaultContainerProxyDataPort)},
					},
					Networks: []apiv2.ContainerNetworkConnectionConfig{
						{
							Name:    containerNetwork.Status.ID,
							Aliases: tunnelProxy.Spec.Aliases,
						},
					},
				},
			},
		}
		createContainerErr := r.Client.Create(ctx, &physicalContainer)
		if createContainerErr != nil && !apimachinery_errors.IsAlreadyExists(createContainerErr) {
			log.Error(createContainerErr, "Failed to create client proxy PhysicalContainer")
			pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
			pd.Message = fmt.Sprintf("Failed to create client proxy PhysicalContainer: %v", createContainerErr)
			return false, NoDelay
		}

		log.V(1).Info("Created client proxy PhysicalContainer", "PhysicalContainer", physicalContainerName)
		return false, StandardDelay
	}
	if getContainerErr != nil {
		log.Error(getContainerErr, "Failed to get client proxy PhysicalContainer")
		return false, StandardDelay
	}

	return r.updateClientProxyContainerStatus(ctx, tunnelProxy, pd, log)
}

func tunnelProxyPhysicalResourceName(tunnelProxy *apiv1.ContainerNetworkTunnelProxy) types.NamespacedName {
	return tunnelProxyPhysicalResourceNameForUID(tunnelProxy.UID)
}

func tunnelProxyPhysicalResourceNameForUID(tunnelProxyUID types.UID) types.NamespacedName {
	return types.NamespacedName{
		Namespace: V1PhysicalResourcesNamespaceName,
		Name:      fmt.Sprintf("tunnel-proxy-%s", tunnelProxyUID),
	}
}

func tunnelProxyPhysicalResourceAnnotations(tunnelProxy *apiv1.ContainerNetworkTunnelProxy) map[string]string {
	return map[string]string{
		tunnelProxyOwnerNameAnnotation:      tunnelProxy.Name,
		tunnelProxyOwnerNamespaceAnnotation: tunnelProxy.Namespace,
	}
}

func (r *ContainerNetworkTunnelProxyReconciler) ensureTunnelProxyPhysicalContainerImage(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	imagePlan dcptun.ClientProxyImagePlan,
) error {
	imageName := tunnelProxyPhysicalResourceName(tunnelProxy)
	imageConfig := &apiv2.PhysicalContainerImageConfig{
		Image:      imagePlan.Image,
		PullPolicy: apiv2.PullPolicyNever,
	}
	if imagePlan.BuildContextArchive != nil {
		imageConfig.PullPolicy = apiv2.PullPolicyMissing
		imageConfig.Build = &apiv2.ContainerBuildContext{
			ContextArchive: &apiv2.ContainerBuildContextArchive{
				Digest:      imagePlan.BuildContextArchive.Digest,
				Source:      imagePlan.BuildContextArchive.Source,
				SHA256:      imagePlan.BuildContextArchive.SHA256,
				RawContents: imagePlan.BuildContextArchive.RawContents,
			},
			Dockerfile: imagePlan.Dockerfile,
			Labels:     imagePlan.Labels,
		}
	}
	physicalImage := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:        imageName.Name,
			Namespace:   imageName.Namespace,
			Annotations: tunnelProxyPhysicalResourceAnnotations(tunnelProxy),
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image: imageConfig,
		},
	}

	createErr := r.Client.Create(ctx, physicalImage)
	if apimachinery_errors.IsAlreadyExists(createErr) {
		return imagePlan.Cleanup()
	}
	if createErr != nil {
		return fmt.Errorf("create PhysicalContainerImage %q: %w", imageName.String(), createErr)
	}

	return nil
}

func (r *ContainerNetworkTunnelProxyReconciler) updateClientProxyContainerStatus(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) (bool, AdditionalReconciliationDelay) {
	containerName := tunnelProxyPhysicalResourceName(tunnelProxy)
	physicalContainer := apiv2.PhysicalContainer{}
	getContainerErr := r.Get(ctx, containerName, &physicalContainer)
	if apimachinery_errors.IsNotFound(getContainerErr) {
		if pd.State == apiv1.ContainerNetworkTunnelProxyStateRunning {
			pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
			pd.Message = fmt.Sprintf("Client proxy PhysicalContainer %q no longer exists", containerName.String())
			return false, NoDelay
		}
		return false, StandardDelay
	}
	if getContainerErr != nil {
		log.Error(getContainerErr, "Failed to get client proxy PhysicalContainer")
		return false, StandardDelay
	}

	switch physicalContainer.Status.Phase {
	case apiv2.PhysicalContainerPhaseRunning:
		// Continue below.
	case apiv2.PhysicalContainerPhaseFailed, apiv2.PhysicalContainerPhaseExited:
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		pd.Message = fmt.Sprintf("Client proxy PhysicalContainer failed: %s", physicalResourceStatusMessage(physicalContainer.Status.Conditions))
		return false, NoDelay
	case apiv2.PhysicalContainerPhaseUnknown:
		if physicalResourceReadyConditionReason(physicalContainer.Status.Conditions) == apiv2.PhysicalContainerReasonRuntimeContainerMissing {
			pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
			pd.Message = fmt.Sprintf("Client proxy PhysicalContainer failed: %s", physicalResourceStatusMessage(physicalContainer.Status.Conditions))
			return false, NoDelay
		}
		return false, StandardDelay
	default:
		return false, StandardDelay
	}

	controlPort, controlPortErr := physicalContainerHostPort(physicalContainer.Status.PortMappings, int32(dcptun.DefaultContainerProxyControlPort))
	if controlPortErr != nil {
		log.Error(controlPortErr, "Failed to determine control connection host port for the client proxy PhysicalContainer")
		return false, StandardDelay
	}
	dataPort, dataPortErr := physicalContainerHostPort(physicalContainer.Status.PortMappings, int32(dcptun.DefaultContainerProxyDataPort))
	if dataPortErr != nil {
		log.Error(dataPortErr, "Failed to determine data connection host port for the client proxy PhysicalContainer")
		return false, StandardDelay
	}

	pd.ClientProxyContainerID = physicalContainer.Status.ContainerID
	pd.ClientProxyControlPort = controlPort
	pd.ClientProxyDataPort = dataPort
	return true, NoDelay
}

func physicalContainerHostPort(portMappings []apiv2.PhysicalContainerPortMapping, containerPort int32) (int32, error) {
	for _, portMapping := range portMappings {
		if portMapping.ContainerPort == containerPort &&
			(portMapping.Protocol == "" || portMapping.Protocol == commonapi.PortProtocolTCP) &&
			portMapping.HostPort > 0 {
			return portMapping.HostPort, nil
		}
	}

	return 0, fmt.Errorf("no host port mapping exists for TCP container port %d", containerPort)
}

func physicalResourceStatusMessage(conditions []metav1.Condition) string {
	for i := len(conditions) - 1; i >= 0; i-- {
		if conditions[i].Type == string(apiv2.ConditionReady) && conditions[i].Message != "" {
			return conditions[i].Message
		}
	}
	return "the physical resource did not report an error message"
}

func physicalResourceReadyConditionReason(conditions []metav1.Condition) apiv2.ConditionReason {
	for i := len(conditions) - 1; i >= 0; i-- {
		if conditions[i].Type == string(apiv2.ConditionReady) {
			return apiv2.ConditionReason(conditions[i].Reason)
		}
	}
	return ""
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
	dcpExePath, dcpExePathErr := dcppaths.GetDcpExePath()
	if dcpExePathErr != nil {
		log.Error(dcpExePathErr, "Failed to get DCP executable path")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		pd.Message = fmt.Sprintf("Failed to get DCP executable path: %v", dcpExePathErr)
		return false
	}

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
		pd.Message = fmt.Sprintf("Failed to create stdout temp file for container tunnel server proxy: %v", stdoutErr)
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
		pd.Message = fmt.Sprintf("Failed to create stderr temp file for container tunnel server proxy: %v", stderrErr)
		return false
	} else {
		pd.ServerProxyStdErrFile = stderrFile.Name()
		pd.serverStderr = stderrFile
	}

	args := append([]string{
		"tunnel-server",
		// We rely on the defaults for server control address and port (localhost:0, i.e. auto-allocated port), so not specifying them here.
		networking.IPv4LocalhostDefaultAddress, // Client control address--as exposed by container orchestrator
		strconv.Itoa(int(pd.ClientProxyControlPort)),
		networking.IPv4LocalhostDefaultAddress, // Client data address--as exposed by container orchestrator
		strconv.Itoa(int(pd.ClientProxyDataPort)),
	}, r.createProxySecurityArgs(pd, log)...)

	cmd := exec.Command(dcpExePath, args...)
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile
	cmd.Env = os.Environ()
	logger.WithSessionId(cmd)
	exitHandler := process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
		r.onServerProcessExit(tunnelProxy.NamespacedName(), pid, exitCode, err, stdoutFile, stderrFile)
	})

	handle, startWaitForExit, startErr := r.config.ProcessExecutor.StartProcess(context.Background(), cmd, exitHandler, process.CreationFlagsNone, nil)
	if startErr != nil {
		log.Error(startErr, "Failed to start server proxy process")
		startFailed = true
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		pd.Message = fmt.Sprintf("Failed to start server proxy process: %v", startErr)
		return false
	}
	startWaitForExit()

	// Wait until the first JSON line is printed to stdout indicating server control address/port

	tc, tcErr := readServerProxyConfig(ctx, stdoutFile.Name())
	if tcErr != nil {
		log.Error(tcErr, "Failed to read connection information from the server proxy")
		stopProcessErr := r.config.ProcessExecutor.StopProcess(handle)
		if stopProcessErr != nil {
			log.Error(stopProcessErr, "Failed to stop server proxy process after being unable to read its configuration")
		}
		startFailed = true
		return false
	}

	dcpproc.RunProcessWatcher(r.config.ProcessExecutor, handle, log)

	pointers.SetValue(&pd.ServerProxyProcessID, int64(handle.Pid))
	pd.ServerProxyControlPort = tc.ServerControlPort
	pd.ServerProxyStartupTimestamp = metav1.NewMicroTime(handle.IdentityTime)
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

// Returns a function that cleans up the resources associated with the proxy pair (client container and server process).
// The method is called as part of the reconciliation loop, but the returned function is executed asynchronously.
// The passed proxy data is a clone independent from what is stored in r.proxyData map.
func (r *ContainerNetworkTunnelProxyReconciler) startProxyPairCleanup(
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) func(context.Context) {
	return func(ctx context.Context) {
		r.cleanupProxyPair(ctx, pd, tunnelProxy.UID, log)

		log.V(1).Info("Completed cleanup of ContainerNetworkTunnelProxy proxy pair")
		nn := tunnelProxy.NamespacedName()
		pdMap := r.proxyData
		pdMap.QueueDeferredOp(nn, func(types.NamespacedName, types.NamespacedName, *apiv1.ContainerNetworkTunnelProxy) {
			pdMap.Update(nn, nn, pd)
		})
		r.ScheduleReconciliation(nn)
	}
}

// Deletes the resources associated with the proxy pair (client container and server process)
// and updates the provided proxy data accordingly.
func (r *ContainerNetworkTunnelProxyReconciler) cleanupProxyPair(
	ctx context.Context,
	pd *containerNetworkTunnelProxyData,
	proxyObjectID types.UID,
	log logr.Logger,
) {
	if pd.ClientProxyContainerID != "" {
		log.V(1).Info("Removing client proxy PhysicalContainer...")
	}

	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, clientProxyContainerCleanupTimeout)
	defer cleanupCancel()

	removeErr := r.cleanupClientPhysicalResources(cleanupCtx, proxyObjectID, pd.imageBuildContextArchiveSource)
	if removeErr != nil {
		log.Error(removeErr, "Failed to remove client proxy physical resources")
		pd.cleanupScheduled = false
	} else {
		if pd.ClientProxyContainerID != "" {
			log.V(1).Info("Successfully removed client proxy physical resources")
		}
		pd.ClientProxyContainerID = ""
		pd.imageBuildContextArchiveSource = ""
	}

	if pd.ServerProxyProcessID != nil && *pd.ServerProxyProcessID > 0 {
		pid := process.Pid_t(*pd.ServerProxyProcessID)
		startTime := pd.ServerProxyStartupTimestamp.Time

		log.V(1).Info("Stopping server proxy process...")

		// The process may have already exited because the client container has been stopped.

		stopErr := r.config.ProcessExecutor.StopProcess(process.NewHandle(pid, startTime))
		if stopErr != nil && !errors.Is(stopErr, process.ErrorProcessNotFound) {
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
}

func (r *ContainerNetworkTunnelProxyReconciler) cleanupClientPhysicalResources(
	ctx context.Context,
	proxyObjectID types.UID,
	buildContextArchiveSource string,
) error {
	resourceName := tunnelProxyPhysicalResourceNameForUID(proxyObjectID)
	physicalContainer := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName.Name,
			Namespace: resourceName.Namespace,
		},
	}
	deleteContainerErr := r.Client.Delete(ctx, physicalContainer)
	if deleteContainerErr != nil && !apimachinery_errors.IsNotFound(deleteContainerErr) {
		return fmt.Errorf("delete PhysicalContainer %q: %w", resourceName.String(), deleteContainerErr)
	}

	waitContainerErr := resiliency.RetryExponential(ctx, func() error {
		getContainerErr := r.Client.Get(ctx, resourceName, &apiv2.PhysicalContainer{})
		if apimachinery_errors.IsNotFound(getContainerErr) {
			return nil
		}
		if getContainerErr != nil {
			return fmt.Errorf("get deleting PhysicalContainer %q: %w", resourceName.String(), getContainerErr)
		}
		return fmt.Errorf("PhysicalContainer %q still exists", resourceName.String())
	})
	if waitContainerErr != nil {
		return waitContainerErr
	}

	physicalImage := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName.Name,
			Namespace: resourceName.Namespace,
		},
	}
	currentPhysicalImage := &apiv2.PhysicalContainerImage{}
	getImageErr := r.Client.Get(ctx, resourceName, currentPhysicalImage)
	if getImageErr != nil && !apimachinery_errors.IsNotFound(getImageErr) {
		return fmt.Errorf("get PhysicalContainerImage %q before deletion: %w", resourceName.String(), getImageErr)
	}
	if getImageErr == nil &&
		currentPhysicalImage.Spec.Image != nil &&
		currentPhysicalImage.Spec.Image.Build != nil &&
		currentPhysicalImage.Spec.Image.Build.ContextArchive != nil &&
		currentPhysicalImage.Spec.Image.Build.ContextArchive.Source != "" {
		buildContextArchiveSource = currentPhysicalImage.Spec.Image.Build.ContextArchive.Source
	}
	deleteImageErr := r.Client.Delete(ctx, physicalImage)
	if deleteImageErr != nil && !apimachinery_errors.IsNotFound(deleteImageErr) {
		return fmt.Errorf("delete PhysicalContainerImage %q: %w", resourceName.String(), deleteImageErr)
	}

	waitImageErr := resiliency.RetryExponential(ctx, func() error {
		waitGetImageErr := r.Client.Get(ctx, resourceName, &apiv2.PhysicalContainerImage{})
		if apimachinery_errors.IsNotFound(waitGetImageErr) {
			return nil
		}
		if waitGetImageErr != nil {
			return fmt.Errorf("get deleting PhysicalContainerImage %q: %w", resourceName.String(), waitGetImageErr)
		}
		return fmt.Errorf("PhysicalContainerImage %q still exists", resourceName.String())
	})
	if waitImageErr != nil {
		return waitImageErr
	}
	if buildContextArchiveSource != "" {
		removeArchiveErr := os.Remove(buildContextArchiveSource)
		if removeArchiveErr != nil && !errors.Is(removeArchiveErr, os.ErrNotExist) {
			return fmt.Errorf("remove PhysicalContainerImage build context archive %q: %w", buildContextArchiveSource, removeArchiveErr)
		}
	}
	return nil
}

func (r *ContainerNetworkTunnelProxyReconciler) onServerProcessExit(
	pName types.NamespacedName,
	pid process.Pid_t,
	exitCode int32,
	err error,
	stdoutFile *os.File,
	stderrFile *os.File,
) {
	if err != nil {
		r.Log.Error(err, "Tunnel server proxy process exited with error", "PID", pid, "ExitCode", exitCode)
	} else if exitCode != 0 {
		r.Log.Error(fmt.Errorf("tunnel server proxy process exited with non-zero exit code %d", exitCode), "Tunnel server proxy process exited abnormally", "PID", pid)
	}

	if closeErr := stdoutFile.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
		r.Log.Error(closeErr, "Failed to close stdout file for tunnel server proxy process", "PID", pid)
	}

	if closeErr := stderrFile.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
		r.Log.Error(closeErr, "Failed to close stderr file for tunnel server proxy process", "PID", pid)
	}

	pdMap := r.proxyData
	pdMap.QueueDeferredOp(pName, func(types.NamespacedName, types.NamespacedName, *apiv1.ContainerNetworkTunnelProxy) {
		_, pd := pdMap.BorrowByNamespacedName(pName)
		if pd == nil {
			return // ContainerNetworkTunnelProxy object has been deleted, nothing to do
		}
		if pd.cleanupScheduled {
			return // We are cleaning up and just got a callback reporting server process termination, nothing to do
		}

		// Server proxy process exited unexpectedly, so we need to mark the proxy as failed, which will trigger the cleanup.
		pd.ServerProxyProcessID = nil
		pd.ServerProxyStartupTimestamp = metav1.MicroTime{} // Zero value
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		message := fmt.Sprintf("Server proxy process '%d' exited unexpectedly with exit code %d", pid, exitCode)
		if err != nil {
			message = fmt.Sprintf("Server proxy process '%d' exited unexpectedly with exit code %d: %v", pid, exitCode, err)
		}
		pd.Message = message
		pdMap.Update(pName, pName, pd)
	})
	r.ScheduleReconciliation(pName)
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
