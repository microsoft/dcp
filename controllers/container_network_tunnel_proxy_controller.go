/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	std_slices "slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	stdproto "google.golang.org/protobuf/proto"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcpproc"
	"github.com/microsoft/dcp/internal/dcptun"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/concurrency"
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

type serverProxyExitType uint8

const (
	serverProxyExitTypeExpected serverProxyExitType = iota
	serverProxyExitTypeUnexpected
)

// serverProxyExit records a server proxy process's exit outcome and any associated error.
type serverProxyExit struct {
	exitType serverProxyExitType
	exitCode int32
	err      error
}

// serverProxyRun tracks a server proxy process's identity, lifecycle, and output cleanup.
type serverProxyRun struct {
	handle              process.ProcessHandle                      // Handle for managing the proxy process.
	resourceUID         types.UID                                  // UID of the resource associated with this run.
	pairGeneration      uint64                                     // Proxy pair generation associated with this run.
	exit                *concurrency.ValuePromise[serverProxyExit] // Records the expected or unexpected exit outcome.
	removeOutputOnClose atomic.Bool                                // Whether to remove process output when closing the handle.
}

func newServerProxyRun() *serverProxyRun {
	return &serverProxyRun{
		exit: concurrency.NewValuePromise[serverProxyExit](),
	}
}

func (run *serverProxyRun) expectExit() {
	run.exit.Set(serverProxyExit{exitType: serverProxyExitTypeExpected})
}

func (run *serverProxyRun) recordUnexpectedExit(exitCode int32, err error) bool {
	return run.exit.Set(serverProxyExit{
		exitType: serverProxyExitTypeUnexpected,
		exitCode: exitCode,
		err:      err,
	})
}

func (run *serverProxyRun) getExit() (serverProxyExit, bool) {
	if !run.exit.IsSet() {
		return serverProxyExit{}, false
	}
	return run.exit.Get(), true
}

func (run *serverProxyRun) requestOutputRemoval() {
	run.removeOutputOnClose.Store(true)
}

const (
	containerNetworkNameKey = ".metadata.containerNetworkName"
	serviceReferencesKey    = ".metadata.serviceReferences"

	defaultServerProxyConfigReadTimeout = 10 * time.Second
	defaultControlStartupTimeout        = 60 * time.Second

	// Kept well below defaultManagerGracefulShutdownTimeout so clientSecurityFiles.Cleanup()
	// always runs after workQueue finishes draining and before controller-runtime exits.
	defaultWorkQueueDrainTimeout = 20 * time.Second

	// Default cap on exponential backoff delays between consecutive proxy-pair replacement attempts.
	defaultPairRestartMaxInterval = 30 * time.Second

	// Default minimum stable run duration after control readiness required to reset the pair restart backoff.
	defaultPairRestartBackoffResetAfter = 2 * time.Minute

	// Pause between abandoned proxy-pair cleanup retries while draining during controller shutdown.
	abandonedCleanupShutdownRetryDelay = 200 * time.Millisecond

	// Maximum bytes of server proxy stderr logged when server startup fails.
	maxLoggedServerStderrBytes = 4 * 1024

	// Container-local destination for private credentials injected at runtime, never into the shared image.
	clientProxySecurityDirectory = "/var/run/dcp"
	// Client-role bootstrap payload, copied with owner-only file permissions.
	clientProxySecurityFileName = "tunnel-control-security.json"
	// Bootstrap path passed to the client instead of certificate, private-key, or token values.
	clientProxySecurityFilePath = clientProxySecurityDirectory + "/" + clientProxySecurityFileName

	// Timeout for tunnel operations (like preparation or deletion of a tunnel)
	tunnelOperationTimeout = 5 * time.Second

	defaultMaxTunnelPreparationAttempts uint32 = 20
	defaultMaxNeverReadyGenerations     uint32 = 4

	// Annotation for an Endpoint object that links it to a specific tunnel that serves it.
	TunnelIdAnnotation = "container-network-tunnel-proxy.usvc-dev.developer.microsoft.com/tunnel-id"

	tunnelProxyOwnerNameAnnotation      = "container-network-tunnel-proxy.usvc-dev.developer.microsoft.com/owner-name"
	tunnelProxyOwnerNamespaceAnnotation = "container-network-tunnel-proxy.usvc-dev.developer.microsoft.com/owner-namespace"
	tunnelProxyOwnerUIDAnnotation       = "container-network-tunnel-proxy.usvc-dev.developer.microsoft.com/owner-uid"
)

const (
	// Dispatching a cleanup attempt to a worker or shutdown fallback.
	tunnelProxyCleanupStageSchedule = "schedule"

	// Stopping the host-side tunnel server process.
	tunnelProxyCleanupStageServerProcessStop = "server-process-stop"

	// Confirming or requesting client PhysicalContainer removal.
	tunnelProxyCleanupStagePhysicalContainerDelete = "physical-container-delete"

	// Removing client credentials after container removal is confirmed.
	tunnelProxyCleanupStageRemoveSecurityFile = "remove-security-file"

	// Closing local server stdout and stderr handles.
	tunnelProxyCleanupStageCloseOutput = "close-output"

	// Removing output files from an unsuccessful server startup.
	tunnelProxyCleanupStageRemoveOutput = "remove-output"

	// Cleaning up after server control configuration could not be read.
	tunnelProxyCleanupStageReadServerConfiguration = "read-server-configuration"

	// Cleaning up after the server failed to establish its first control stream.
	tunnelProxyCleanupStageWaitControlReadiness = "wait-control-readiness"

	// A cleanup attempt finished; CleanupConfirmed reports whether it succeeded.
	tunnelProxyCleanupStageComplete = "complete"
)

var (
	tunnelProxyFinalizer string = fmt.Sprintf("%s/tunnel-proxy-reconciler", apiv1.GroupVersion.Group)

	// Allowed state transitions for a proxy pair ("/" separates alternative source states):
	//
	// Empty -> Pending -> BuildingImage -> Starting -> Started -> Running
	//                      ^               |                      |
	//                      |               +--- replacement ----->|
	//                      |                                      v
	//                      +-- RecoveryWait <- RecoveryCleanup <- RecoveryPending
	//
	// Startup failures:
	// Starting --+-- never ready, below retry limit --> RecoveryPending
	//            |
	//            +-- never ready, retry limit reached --> Failed
	// BuildingImage/Starting/Started -- other initial failure --> Failed
	// BuildingImage/Starting -- retryable replacement failure --> RecoveryPending
	// BuildingImage/Starting -- non-retryable failure (any generation) --> Failed
	//
	// Deletion (after a deletion request):
	// <any state except RecoveryWait, Deleting, or Deleted> -> Deleting
	// Deleting -- cleanup incomplete/failed --> DeletePending -- retry --> Deleting
	// Deleting -- cleanup complete --> Deleted
	// RecoveryWait -- deletion requested --> Deleted
	//
	// Cleanup returns to DeletePending while PhysicalContainer deletion is unconfirmed,
	// or if resource deletion, server-process shutdown, or security-file removal fails.
	// Deleted records completed pair cleanup until object removal, so separately saved
	// status/finalizer updates can be retried without rescheduling cleanup.
	// Failed state is terminal except for deletion.

	// Recovery replaces the failed pair rather than restarting the initial startup sequence unchanged.
	// In particular, replacement startup transitions from Starting directly to Running, without Started.
	// Tunnel statuses survive recovery: RecoveryPending marks operational tunnels NotReady, and the replacement
	// pair starts with NotReady statuses of desired tunnels (keeping their published ports) and prepares all of
	// them again, including tunnels that failed on the replaced pair.
	//
	// Server-proxy startup distinguishes pre-configuration failures (malformed configuration output or
	// server process exit before writing configuration, which transition immediately to Failed in any
	// generation) from timeouts reading configuration while the server process is still running or
	// failures/exits after configuration before the first control stream is established. Those never-ready
	// attempts return to RecoveryPending to try another generation, and enter Failed after
	// MaxNeverReadyGenerations consecutive generations fail without establishing a control stream.
	//
	// Other initial startup failures, as well as non-retryable failures in any generation, cause transition to Failed state.
	// During replacement startup, retryable tunnel proxy failures return to RecoveryPending
	// so that recovery can be attempted again.
	//
	// If owned in-memory state is missing, Empty/Pending initialize as Pending and transitions to BuildingImage;
	// any other persisted state enters Failed unless deletion was requested.
	// Deletion without in-memory state starts at DeletePending. Otherwise, deletion enters Deleting,
	// or goes directly from RecoveryWait to Deleted because recovery cleanup already completed.

	tunnelProxyStateInitializers = map[apiv1.ContainerNetworkTunnelProxyState]tunnelProxyStateInitializerFunc{
		apiv1.ContainerNetworkTunnelProxyStateEmpty:           handleNewTunnelProxy,
		apiv1.ContainerNetworkTunnelProxyStatePending:         handleNewTunnelProxy,
		apiv1.ContainerNetworkTunnelProxyStateBuildingImage:   ensureTunnelProxyBuildingImageState,
		apiv1.ContainerNetworkTunnelProxyStateStarting:        ensureTunnelProxyStartingState,
		apiv1.ContainerNetworkTunnelProxyStateStarted:         ensureTunnelProxyRunningState,
		apiv1.ContainerNetworkTunnelProxyStateRunning:         ensureTunnelProxyRunningState,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryPending: ensureTunnelProxyRecoveryPendingState,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryCleanup: ensureTunnelProxyRecoveryCleanupState,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryWait:    ensureTunnelProxyRecoveryWaitState,
		apiv1.ContainerNetworkTunnelProxyStateFailed:          ensureTunnelProxyFailedState,
	}
)

type ContainerNetworkTunnelProxyReconcilerConfig struct {
	ProcessExecutor process.Executor // Mandatory

	// Directory that owns the lifetime of generated tunnel proxy image build contexts.
	// Defaults to the DCP temporary directory (usvc_io.DcpTempDir()).
	BuildContextDir string

	// The factory function to create a TunnelControlClient used to control the proxy pair.
	// Normal execution uses "real" gRPC client, tests use a stub since most tests do not run real tunnels.
	// Mandatory.
	MakeTunnelControlClient func(grpc.ClientConnInterface) dcptunproto.TunnelControlClient

	// Specifies how many attempts to prepare a tunnel will be made before giving up and marking the tunnel as failed.
	// Defaults to defaultMaxTunnelPreparationAttempts, but much lower value is used for tests to simulate failures quickly.
	MaxTunnelPreparationAttempts uint32

	// Limits consecutive pair generations whose server proxy fails to establish its first control stream.
	// Client container failures during replacement startup are retried without limit.
	// Defaults to defaultMaxNeverReadyGenerations.
	MaxNeverReadyGenerations uint32

	// Creates a per-proxy backoff policy for pair restarts; defaults to resiliency.NewExponentialBackoff() capped at defaultPairRestartMaxInterval without a lifetime limit.
	PairRestartBackoffFactory func() backoff.BackOff

	// How long a pair generation must run after establishing its first control stream for the pair restart backoff to reset
	// (start over from its initial delay when that generation fails). Defaults to defaultPairRestartBackoffResetAfter.
	PairRestartBackoffResetAfter time.Duration

	// Bounds the wait for a new server proxy to print its initial configuration JSON line; when the wait expires
	// while the process is still running, the server proxy is stopped and its pair generation counts as never-ready.
	// Defaults to defaultServerProxyConfigReadTimeout.
	ServerConfigReadTimeout time.Duration

	// Bounds the wait for a new server proxy to establish its first control stream; when the wait expires,
	// the server proxy is stopped and its pair generation counts as never-ready. Defaults to defaultControlStartupTimeout.
	ControlStartupTimeout time.Duration

	// Bounds shutdown's wait for queued and running operations to finish before cancelling them. Defaults to defaultWorkQueueDrainTimeout.
	WorkQueueDrainTimeout time.Duration

	// Optional per-reconciliation timeout passed to controller-runtime; zero keeps controller-runtime's default.
	ReconciliationTimeout time.Duration
}

type ContainerNetworkTunnelProxyReconciler struct {
	*ReconcilerBase[apiv1.ContainerNetworkTunnelProxy, *apiv1.ContainerNetworkTunnelProxy]

	config ContainerNetworkTunnelProxyReconcilerConfig

	// In-memory state map for ContainerNetworkTunnelProxy objects.
	proxyData *tunnelProxyDataMap

	// A work queue for background operations.
	workQueue *resiliency.ShutdownAwareWorkQueue

	// Prevents concurrent preparation of shared proxy images.
	sharedImagePreparationLock *concurrency.ContextAwareLock

	// Owns client control security files until pair cleanup or controller shutdown removes them.
	clientSecurityFiles *usvc_io.FileRegistry
}

func NewContainerNetworkTunnelProxyReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	config ContainerNetworkTunnelProxyReconcilerConfig,
	log logr.Logger,
) *ContainerNetworkTunnelProxyReconciler {
	if config.ProcessExecutor == nil {
		panic("ContainerNetworkTunnelProxyReconcilerConfig.ProcessExecutor must not be nil")
	}
	if config.BuildContextDir == "" {
		config.BuildContextDir = usvc_io.DcpTempDir()
	}
	if config.MakeTunnelControlClient == nil {
		panic("ContainerNetworkTunnelProxyReconcilerConfig.TunnelControlClientFactory must not be nil")
	}
	if config.MaxTunnelPreparationAttempts == 0 {
		config.MaxTunnelPreparationAttempts = defaultMaxTunnelPreparationAttempts
	}
	if config.MaxNeverReadyGenerations == 0 {
		config.MaxNeverReadyGenerations = defaultMaxNeverReadyGenerations
	}
	if config.ServerConfigReadTimeout <= 0 {
		config.ServerConfigReadTimeout = defaultServerProxyConfigReadTimeout
	}
	if config.ControlStartupTimeout <= 0 {
		config.ControlStartupTimeout = defaultControlStartupTimeout
	}
	if config.WorkQueueDrainTimeout <= 0 {
		config.WorkQueueDrainTimeout = defaultWorkQueueDrainTimeout
	}
	if config.PairRestartBackoffFactory == nil {
		config.PairRestartBackoffFactory = func() backoff.BackOff {
			policy := resiliency.NewExponentialBackoff()
			policy.MaxInterval = defaultPairRestartMaxInterval
			policy.MaxElapsedTime = 0
			return policy
		}
	}
	if config.PairRestartBackoffResetAfter <= 0 {
		config.PairRestartBackoffResetAfter = defaultPairRestartBackoffResetAfter
	}

	base := NewReconcilerBase[apiv1.ContainerNetworkTunnelProxy](client, noCacheClient, log, lifetimeCtx)

	r := ContainerNetworkTunnelProxyReconciler{
		ReconcilerBase:             base,
		config:                     config,
		proxyData:                  NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
		workQueue:                  resiliency.NewShutdownAwareWorkQueue(lifetimeCtx, resiliency.DefaultConcurrency, config.WorkQueueDrainTimeout),
		sharedImagePreparationLock: concurrency.NewContextAwareLock(),
		clientSecurityFiles:        usvc_io.NewFileRegistry(usvc_io.DcpTempDir()),
	}

	return &r
}

func (r *ContainerNetworkTunnelProxyReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	addShutdownErr := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		r.clientSecurityFiles.Close()
		r.workQueue.Shutdown()
		completionErr, received := <-r.workQueue.CompletionErr()
		if !received {
			completionErr = errors.New("tunnel proxy work queue completion channel closed without a result")
		}
		if completionErr != nil {
			r.Log.Error(completionErr, "Failed to drain tunnel proxy work queue", "Timeout", r.config.WorkQueueDrainTimeout)
		}
		fileCleanupErr := r.clientSecurityFiles.Cleanup()
		if fileCleanupErr != nil {
			r.Log.Error(fileCleanupErr, "Failed to remove client control security files during controller shutdown")
		}
		return fileCleanupErr
	}))
	if addShutdownErr != nil {
		return fmt.Errorf("add tunnel proxy shutdown: %w", addShutdownErr)
	}

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
		WithOptions(controller.Options{
			MaxConcurrentReconciles: MaxConcurrentReconciles,
			ReconciliationTimeout:   r.config.ReconciliationTimeout,
		}).
		For(&apiv1.ContainerNetworkTunnelProxy{}).
		Owns(&apiv1.Endpoint{}).
		Watches(&apiv1.Service{}, handler.EnqueueRequestsFromMapFunc(r.reconcileProxiesUsingService), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv1.ContainerNetwork{}, handler.EnqueueRequestsFromMapFunc(r.reconcileProxiesUsingNetwork), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.PhysicalContainerImage{}, handler.EnqueueRequestsFromMapFunc(r.reconcileProxyForPhysicalResource), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.PhysicalContainer{}, handler.EnqueueRequestsFromMapFunc(r.reconcileProxyForPhysicalResource), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.PhysicalContainerNetwork{}, handler.EnqueueRequestsFromMapFunc(r.reconcileProxyForPhysicalResource), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *ContainerNetworkTunnelProxyReconciler) reconcileProxyForPhysicalResource(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	if obj.GetNamespace() != V1PhysicalResourcesNamespaceName {
		return nil
	}

	if _, isImage := obj.(*apiv2.PhysicalContainerImage); isImage {
		if obj.GetName() != V1TunnelProxyPhysicalContainerImageName {
			return nil
		}

		tunnelProxies := apiv1.ContainerNetworkTunnelProxyList{}
		if listErr := r.List(ctx, &tunnelProxies); listErr != nil {
			r.Log.Error(listErr, "Failed to list ContainerNetworkTunnelProxies for shared PhysicalContainerImage")
			return nil
		}

		return slices.Map[reconcile.Request](tunnelProxies.Items, func(tunnelProxy apiv1.ContainerNetworkTunnelProxy) reconcile.Request {
			return reconcile.Request{NamespacedName: tunnelProxy.NamespacedName()}
		})
	}

	annotations := obj.GetAnnotations()
	if _, isNetwork := obj.(*apiv2.PhysicalContainerNetwork); isNetwork {
		sourceNetworkName := annotations[V1ContainerNetworkNameAnnotation]
		if sourceNetworkName == "" {
			return nil
		}

		tunnelProxies := apiv1.ContainerNetworkTunnelProxyList{}
		listErr := r.List(ctx, &tunnelProxies, ctrl_client.MatchingFields{containerNetworkNameKey: sourceNetworkName})
		if listErr != nil {
			r.Log.Error(listErr, "Failed to list ContainerNetworkTunnelProxies for shared PhysicalContainerNetwork")
			return nil
		}

		return slices.Map[reconcile.Request](tunnelProxies.Items, func(tunnelProxy apiv1.ContainerNetworkTunnelProxy) reconcile.Request {
			return reconcile.Request{NamespacedName: tunnelProxy.NamespacedName()}
		})
	}

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
			r.discardMissingTunnelProxyState(req.NamespacedName, log)
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
	if r.resetRecreatedTunnelProxyState(&tproxy, log) {
		return ctrl.Result{RequeueAfter: delayDuration(StandardDelay)}, nil
	}

	var change objectChange
	reconciliationDelay := StandardDelay
	patch := ctrl_client.MergeFromWithOptions(tproxy.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if tproxy.DeletionTimestamp != nil && !tproxy.DeletionTimestamp.IsZero() {
		log.Info("ContainerNetworkTunnelProxy object is being deleted")
		change = r.handleDeletionRequest(&tproxy, log)
	} else {
		change = ensureFinalizer(&tproxy, tunnelProxyFinalizer, log)
		if change == noChange {
			change, reconciliationDelay = r.manageTunnelProxy(ctx, &tproxy, log)
		}
	}

	result, err := r.SaveChangesWithDelay(ctx, &tproxy, patch, change, reconciliationDelay, nil, log)
	return result, err
}

func (r *ContainerNetworkTunnelProxyReconciler) handleDeletionRequest(tunnelProxy *apiv1.ContainerNetworkTunnelProxy, log logr.Logger) objectChange {
	namespacedName := tunnelProxy.NamespacedName()
	_, pd := r.proxyData.BorrowByNamespacedName(namespacedName)
	if pd == nil {
		pd = newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateDeletePending, tunnelProxy.UID)
		pd.ContainerNetworkTunnelProxyStatus = *tunnelProxy.Status.DeepCopy()
		// Setting ContainerNetworkTunnelProxyStatus might have overridden the embedded State field; ensure DeletePending.
		pd.State = apiv1.ContainerNetworkTunnelProxyStateDeletePending
		r.proxyData.Store(namespacedName, namespacedName, pd.Clone())
	}

	_, releaseStartup, startupAvailable := pd.startup.TryAcquire(r.LifetimeCtx)
	if !startupAvailable {
		pd.startup.CancelActive()
		log.V(1).Info("ContainerNetworkTunnelProxy is being deleted; canceled active startup work and waiting for it to finish...")
		return additionalReconciliationNeeded
	}
	defer releaseStartup()

	var change objectChange = noChange
	switch pd.State {
	case apiv1.ContainerNetworkTunnelProxyStateDeleted, apiv1.ContainerNetworkTunnelProxyStateRecoveryWait:
		log.V(1).Info("ContainerNetworkTunnelProxy is being deleted (resource cleanup finished, deleting finalizer)...")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateDeleted
		// Remember that resources have been cleaned up until the object is removed to avoid scheduling cleanup again.
		r.proxyData.Update(namespacedName, namespacedName, pd)
		change = deleteFinalizer(tunnelProxy, tunnelProxyFinalizer, log)

	case apiv1.ContainerNetworkTunnelProxyStateDeleting:
		log.V(1).Info("ContainerNetworkTunnelProxy cleanup is still in progress")

	default:
		pd.State = apiv1.ContainerNetworkTunnelProxyStateDeleting
		r.proxyData.Update(namespacedName, namespacedName, pd)

		log.V(1).Info("ContainerNetworkTunnelProxy is being deleted (scheduling resource cleanup)...")
		cleanupErr := r.workQueue.Enqueue(r.startProxyPairCleanup(tunnelProxy, pd.Clone(), log))
		if cleanupErr != nil {
			// Should never happen. This means we (the reconciler) have been shut down via lifetime context
			// with some tunnel proxy instances still running. Just give up on the cleanup here
			// and rely on the dcpproc to do the cleanup instead.
			log.Error(cleanupErr, "Failed to schedule tunnel proxy cleanup work, deleting instance without cleanup...")
			r.proxyData.DeleteByNamespacedName(namespacedName)
			change = deleteFinalizer(tunnelProxy, tunnelProxyFinalizer, log)
		} else {
			log.V(1).Info("Scheduled asynchronous cleanup for ContainerNetworkTunnelProxy proxy pair")
		}
	}

	return change | pd.applyTo(tunnelProxy)
}

func (r *ContainerNetworkTunnelProxyReconciler) manageTunnelProxy(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	namespacedName := tunnelProxy.NamespacedName()
	_, pd := r.proxyData.BorrowByNamespacedName(namespacedName)
	if pd == nil {
		pd = newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStatePending, tunnelProxy.UID)
		switch tunnelProxy.Status.State {
		case apiv1.ContainerNetworkTunnelProxyStateEmpty, apiv1.ContainerNetworkTunnelProxyStatePending:
			// Pair startup has not begun; retain Pending so reconciliation waits for the
			// referenced network to be running before preparing the pair.
		default:
			log.Error(errors.New("in-memory tunnel proxy state is missing"), "Cannot resume tunnel proxy without owned tunnel proxy state")
			pd.ContainerNetworkTunnelProxyStatus = *tunnelProxy.Status.DeepCopy()
			// Setting ContainerNetworkTunnelProxyStatus might have overridden the embedded State field; ensure Failed.
			pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		}
		r.proxyData.Store(namespacedName, namespacedName, pd.Clone())
	}

	initializer := getStateInitializer(tunnelProxyStateInitializers, pd.State, log)
	change := initializer(ctx, r, tunnelProxy, pd.State, pd, log)
	r.proxyData.Update(namespacedName, namespacedName, pd)

	reconciliationDelay := StandardDelay
	useLongDelay := pd.State == apiv1.ContainerNetworkTunnelProxyStateRecoveryWait && (change&additionalReconciliationNeeded) != 0 && time.Until(pd.recovery.retryAfter) >= delayDurations[LongDelay].Duration
	if useLongDelay {
		reconciliationDelay = LongDelay
	}

	return change, reconciliationDelay
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
		return pd.applyTo(tunnelProxy) | additionalReconciliationNeeded
	}

	pd.State = apiv1.ContainerNetworkTunnelProxyStateBuildingImage
	return additionalReconciliationNeeded | pd.applyTo(tunnelProxy)
}

func ensureTunnelProxyBuildingImageState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	state apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	change := noChange

	physicalImage := apiv2.PhysicalContainerImage{}
	getImageErr := r.Get(ctx, tunnelProxyPhysicalImageName(), &physicalImage)
	imageReady := false
	switch {
	case apimachinery_errors.IsNotFound(getImageErr):
		pd.Message = fmt.Sprintf("Waiting for shared tunnel proxy PhysicalContainerImage %q to be created", tunnelProxyPhysicalImageName().String())
		namespaceErr := EnsureV1PhysicalResourcesNamespace(ctx, r.Client)
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to ensure V1 physical resources namespace")
			pd.Message = fmt.Sprintf("Failed to ensure V1 physical resources namespace: %v", namespaceErr)
			change |= additionalReconciliationNeeded
			break
		}
		scheduleImageErr := r.scheduleTunnelProxyPhysicalContainerImageCreation(tunnelProxy.NamespacedName(), log)
		if scheduleImageErr != nil {
			log.Error(scheduleImageErr, "Failed to schedule shared tunnel proxy PhysicalContainerImage creation")
			pd.Message = fmt.Sprintf("Failed to schedule shared tunnel proxy PhysicalContainerImage creation: %v", scheduleImageErr)
			change |= additionalReconciliationNeeded
			break
		}
		change |= additionalReconciliationNeeded
	case getImageErr != nil:
		log.Error(getImageErr, "Failed to get shared tunnel proxy PhysicalContainerImage")
		pd.Message = fmt.Sprintf("Failed to get shared tunnel proxy PhysicalContainerImage: %v", getImageErr)
		change |= additionalReconciliationNeeded
	default:
		switch physicalImage.Status.Phase {
		case apiv2.PhysicalContainerImagePhaseFailed:
			pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
			pd.Message = fmt.Sprintf("PhysicalContainerImage for container network tunnel failed: %s", physicalResourceStatusMessage(physicalImage.Status.Conditions))
		case apiv2.PhysicalContainerImagePhaseReady:
			pd.ClientProxyContainerImage = physicalImage.Status.Image
			imageReady = true
		default:
			pd.Message = fmt.Sprintf(
				"Waiting for shared tunnel proxy PhysicalContainerImage %q: %s",
				tunnelProxyPhysicalImageName().String(),
				physicalResourceStatusMessage(physicalImage.Status.Conditions),
			)
			change |= additionalReconciliationNeeded
		}
	}

	if pd.State != apiv1.ContainerNetworkTunnelProxyStateFailed {
		securityConfigErr := r.createProxyControlSecurityConfig(ctx, pd, log)
		if securityConfigErr != nil {
			// Context cancellation or deadline expiration means this reconciliation pass was interrupted
			// (or the controller is shutting down), not that credential setup failed permanently:
			// retry on a fresh reconciliation while the controller is still running, and only mark
			// the proxy Failed on actual key-generation or credential-file creation errors.
			if ctx.Err() != nil || errors.Is(securityConfigErr, context.Canceled) || errors.Is(securityConfigErr, context.DeadlineExceeded) {
				if r.LifetimeCtx.Err() == nil {
					change |= additionalReconciliationNeeded
				}
			} else {
				pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
				pd.Message = fmt.Sprintf("Failed to create tunnel proxy control security configuration: %v", securityConfigErr)
			}
		} else {
			_, containerDelay := r.startClientProxy(ctx, tunnelProxy, pd, log)
			if containerDelay != NoDelay {
				change |= additionalReconciliationNeeded
			}
			if pd.State == apiv1.ContainerNetworkTunnelProxyStateBuildingImage && imageReady {
				pd.State = apiv1.ContainerNetworkTunnelProxyStateStarting
			}
		}
	}

	if pd.State != state {
		// The initializer of the new state must run even if the status projection does not change.
		change |= additionalReconciliationNeeded
	}

	// Ensure the object reflects the latest physical resource state.
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

	startupCtx, releaseStartup, acquiredStartup := pd.startup.TryAcquire(r.LifetimeCtx)
	if acquiredStartup {
		log.V(1).Info("Starting tunnel proxy...")

		startupErr := r.workQueue.Enqueue(r.startProxyPair(startupCtx, tunnelProxy, pd.Clone(), releaseStartup, log))
		if startupErr != nil {
			releaseStartup()
			log.Error(startupErr, "Failed to start tunnel proxy pair, possibly because the workload is shutting down")
			change |= additionalReconciliationNeeded
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
	containerNetwork := apiv1.ContainerNetwork{}
	containerNetworkName := types.NamespacedName{Name: tunnelProxy.Spec.ContainerNetworkName}
	getNetworkErr := r.Get(ctx, containerNetworkName, &containerNetwork)
	if getNetworkErr != nil {
		log.Error(getNetworkErr, "Failed to get ContainerNetwork for running tunnel proxy", "ContainerNetwork", containerNetworkName)
		return additionalReconciliationNeeded | pd.applyTo(tunnelProxy)
	}
	clientContainerReady, _ := r.updateClientProxyContainerStatus(ctx, tunnelProxy, &containerNetwork, pd, log)
	if !clientContainerReady {
		return additionalReconciliationNeeded | pd.applyTo(tunnelProxy)
	}

	change := r.manageTunnels(ctx, tunnelProxy, pd, log)
	pd.State = apiv1.ContainerNetworkTunnelProxyStateRunning
	change |= pd.applyTo(tunnelProxy)
	change |= ensureEndpointsForWorkload(ctx, r, tunnelProxy, nil, pd, log)

	return change
}

func ensureTunnelProxyFailedState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	change := failAllExistingTunnels(pd)
	r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	if !r.cleanupProxyPair(ctx, pd, tunnelProxy.UID, log) {
		change |= additionalReconciliationNeeded
	}
	dataChanged, endpointRemovalErr := r.removeAllTunnelProxyEndpoints(ctx, tunnelProxy, pd, log)
	if dataChanged {
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	}
	if endpointRemovalErr != nil {
		change |= additionalReconciliationNeeded
	}
	return change | pd.applyTo(tunnelProxy)
}

func ensureTunnelProxyRecoveryPendingState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	if r.LifetimeCtx.Err() != nil || commonapi.ResourceCreationProhibited.Load() {
		return pd.applyTo(tunnelProxy)
	}
	log.Info("Recovering container network tunnel proxy", "Reason", pd.Message, "PairGeneration", pd.pairGeneration)
	if pd.recovery.backoff != nil && !pd.controlReadyAt.IsZero() {
		readyFor := time.Since(pd.controlReadyAt)
		if readyFor >= r.config.PairRestartBackoffResetAfter {
			log.V(1).Info("Resetting pair restart backoff after a stable pair generation", "ReadyFor", readyFor.String())
			pd.recovery.backoff.Reset()
		}
	}
	for _, ts := range pd.TunnelStatuses {
		// Failed tunnels keep their failure reason and deleting tunnels stay deleting until the replacement pair is created.
		if ts.State != apiv1.TunnelStateFailed && ts.State != apiv1.TunnelStateDeleting {
			markTunnelNotReady(pd, ts, pd.Message)
		}
	}
	pd.State = apiv1.ContainerNetworkTunnelProxyStateRecoveryCleanup
	return additionalReconciliationNeeded | pd.applyTo(tunnelProxy)
}

func ensureTunnelProxyRecoveryCleanupState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	if r.LifetimeCtx.Err() != nil || commonapi.ResourceCreationProhibited.Load() {
		return pd.applyTo(tunnelProxy)
	}
	_, releaseStartup, acquired := pd.startup.TryAcquire(r.LifetimeCtx)
	if !acquired {
		return additionalReconciliationNeeded | pd.applyTo(tunnelProxy)
	}
	defer releaseStartup()
	cleanupCompleted := r.cleanupProxyPair(ctx, pd, tunnelProxy.UID, log)
	dataChanged, endpointRemovalErr := r.removeAllTunnelProxyEndpoints(ctx, tunnelProxy, pd, log)
	if dataChanged {
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	}
	if !cleanupCompleted || endpointRemovalErr != nil {
		return additionalReconciliationNeeded | pd.applyTo(tunnelProxy)
	}
	if pd.recovery.backoff == nil {
		pd.recovery.backoff = r.config.PairRestartBackoffFactory()
	}
	pd.recovery.retryAfter = time.Now().Add(pd.recovery.backoff.NextBackOff())
	pd.State = apiv1.ContainerNetworkTunnelProxyStateRecoveryWait
	return additionalReconciliationNeeded | pd.applyTo(tunnelProxy)
}

func ensureTunnelProxyRecoveryWaitState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	if r.LifetimeCtx.Err() != nil || commonapi.ResourceCreationProhibited.Load() {
		return pd.applyTo(tunnelProxy)
	}
	if time.Now().Before(pd.recovery.retryAfter) {
		return additionalReconciliationNeeded | pd.applyTo(tunnelProxy)
	}

	replacement := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateBuildingImage, tunnelProxy.UID)
	replacement.pairGeneration = pd.pairGeneration + 1
	replacement.recovery.backoff = pd.recovery.backoff
	replacement.recovery.neverReadyGenerations = pd.recovery.neverReadyGenerations
	replacement.TunnelConfigurationVersion = pd.TunnelConfigurationVersion
	previousStatuses := maps.SliceToMap(pd.TunnelStatuses, apiv1.TunnelStatus.KV)
	replacementTimestamp := metav1.NewMicroTime(time.Now())
	for _, tunnelConfig := range tunnelProxy.Spec.Tunnels {
		if port, published := pd.publishedPorts[tunnelConfig.Name]; published {
			replacement.publishedPorts[tunnelConfig.Name] = port
		}

		// Desired tunnels keep reporting the port that the replacement pair will rebind. Tunnel IDs and listener
		// addresses belonged to the failed pair. Every desired tunnel, including one that failed on that pair,
		// is prepared again with a fresh attempt budget; a failed tunnel keeps its failure reason until then.
		previous, tracked := previousStatuses[tunnelConfig.Name]
		if !tracked {
			continue
		}
		errorMessage := pd.Message
		if previous.State == apiv1.TunnelStateFailed {
			errorMessage = previous.ErrorMessage
		}
		replacement.setTunnelStatus(apiv1.TunnelStatus{
			Name:            tunnelConfig.Name,
			State:           apiv1.TunnelStateNotReady,
			ErrorMessage:    errorMessage,
			ClientProxyPort: replacement.publishedPorts[tunnelConfig.Name],
			Timestamp:       replacementTimestamp,
		})
	}
	// Advancing a generation is a coordinator operation. Ordinary state merges
	// reject different generations so a delayed old result cannot roll it back.
	name := tunnelProxy.NamespacedName()
	r.proxyData.Store(name, name, replacement)
	return additionalReconciliationNeeded | replacement.applyTo(tunnelProxy)
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
	dataChanged := false

	// Convert to maps for easier lookup
	specTunnels := maps.SliceToMap(tunnelProxy.Spec.Tunnels, apiv1.TunnelConfiguration.KV)
	currentTunnels := maps.SliceToMap(pd.TunnelStatuses, apiv1.TunnelStatus.KV)

	// Release only port reservations that are absent from both the desired configuration
	// and the tracked tunnel statuses (which include pending deletions).
	for tunnelName := range pd.publishedPorts {
		_, desired := specTunnels[tunnelName]
		_, tracked := currentTunnels[tunnelName]
		if !desired && !tracked {
			delete(pd.publishedPorts, tunnelName)
			delete(pd.tunnelExtra, tunnelName)
			dataChanged = true
		}
	}

	// Remove tunnels that are no longer in the spec
	for tunnelName, tunnelStatus := range currentTunnels {
		if _, found := specTunnels[tunnelName]; found {
			continue
		}

		tlog := log.WithValues("TunnelName", tunnelName)
		tlog.V(1).Info("Deleting tunnel that is no longer in spec...")

		if tunnelStatus.State != apiv1.TunnelStateDeleting {
			markTunnelDeleting(pd, tunnelStatus, "")
			change |= statusChanged | additionalReconciliationNeeded
			continue
		}

		deleteErr := r.deleteTunnel(ctx, tunnelProxy, tunnelStatus, pd)
		if deleteErr != nil {
			tlog.Error(deleteErr, "Failed to delete tunnel, will retry")
			if markTunnelDeleting(pd, tunnelStatus, fmt.Sprintf("Failed to delete tunnel: %v", deleteErr)) {
				change |= statusChanged
			}
			dataChanged = true
			change |= additionalReconciliationNeeded
			continue
		}

		pd.removeTunnelStatus(tunnelName)
		delete(pd.publishedPorts, tunnelName)
		delete(pd.tunnelExtra, tunnelName)
		change |= statusChanged
	}

	// Add or update tunnels from the spec
	// Note that tunnels cannot be redefined in the spec, our type validation prevents that.
	// Rebind published ports before new tunnels can claim a preferred port
	// that an existing consumer already has in its environment.
	orderedTunnels := make([]apiv1.TunnelConfiguration, 0, len(specTunnels))
	for _, tunnelConfig := range tunnelProxy.Spec.Tunnels {
		if _, published := pd.publishedPorts[tunnelConfig.Name]; published {
			orderedTunnels = append(orderedTunnels, tunnelConfig)
		}
	}
	for _, tunnelConfig := range tunnelProxy.Spec.Tunnels {
		if _, published := pd.publishedPorts[tunnelConfig.Name]; !published {
			orderedTunnels = append(orderedTunnels, tunnelConfig)
		}
	}
	for _, tunnelConfig := range orderedTunnels {
		tunnelName := tunnelConfig.Name
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
			if publishedPort, published := pd.publishedPorts[tunnelName]; published {
				tunnelStatus.ClientProxyPort = publishedPort
			}
			pd.setTunnelStatus(tunnelStatus)
			change |= statusChanged // Added new tunnel, so we definitively have a status change
			change |= r.manageSingleTunnel(ctx, tunnelProxy, tunnelConfig, tunnelStatus, pd, tlog)
		}
	}

	if (change & statusChanged) == statusChanged {
		pd.TunnelConfigurationVersion++
		dataChanged = true
	}
	if dataChanged {
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	}

	return change
}

// Manages a single tunnel, which involves three main use cases:
//
//  1. For new tunnels: prepare a listener after checking required services.
//  2. For existing tunnels: suspend or resume the listener as the server Service changes.
//  3. For removed tunnels: delete the proxy tunnel and its endpoints.
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
		// Failed is final for the current proxy pair. The tunnel is prepared again only by a replacement pair,
		// or after it is removed from the spec and added back.
		return noChange
	}

	if originalTunnelStatus.State == apiv1.TunnelStateDeleting {
		deleteErr := r.deleteTunnel(ctx, tunnelProxy, originalTunnelStatus, pd)
		if deleteErr != nil {
			tlog.Error(deleteErr, "Failed to delete tunnel, will retry")
			change := additionalReconciliationNeeded
			if markTunnelDeleting(pd, originalTunnelStatus, fmt.Sprintf("Failed to delete tunnel: %v", deleteErr)) {
				change |= statusChanged
			}
			r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
			return change
		}

		delete(pd.publishedPorts, tunnelConfig.Name)
		delete(pd.tunnelExtra, tunnelConfig.Name)
		pd.setTunnelStatus(apiv1.TunnelStatus{
			Name:      tunnelConfig.Name,
			State:     apiv1.TunnelStateNotReady,
			Timestamp: metav1.NewMicroTime(time.Now()),
		})
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
		return statusChanged | additionalReconciliationNeeded
	}

	serverSvc, serverServiceHasAddress := r.getTunnelServerService(ctx, tunnelConfig, tlog)
	te, found := pd.tunnelExtra[tunnelConfig.Name]
	if te.clientServiceName == "" {
		te.clientServiceName = tunnelConfig.ClientServiceName
	}
	if found && !te.nextPreparationNoEarlierThan.IsZero() && time.Now().Before(te.nextPreparationNoEarlierThan) {
		return additionalReconciliationNeeded
	}

	// A prepared listener must stop accepting new streams as soon as its backend loses its address,
	// even when the client Service is missing, so suspension precedes the client Service checks.
	if originalTunnelStatus.TunnelID != 0 && !serverServiceHasAddress {
		needsSuspension := originalTunnelStatus.State == apiv1.TunnelStateReady ||
			!te.acceptanceStateKnown ||
			te.acceptingNewStreams
		var suspendErr error
		if needsSuspension {
			te.nextPreparationNoEarlierThan = time.Now().Add(delayDurations[StandardDelay].Duration * 9 / 10)
			pd.tunnelExtra[tunnelConfig.Name] = te
			r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)

			suspendErr = r.updateTunnel(ctx, originalTunnelStatus, nil, false, pd)
			if suspendErr == nil {
				te.acceptingNewStreams = false
				te.acceptanceStateKnown = true
				te.nextPreparationNoEarlierThan = time.Time{}
			} else {
				te.acceptanceStateKnown = false
			}
		}

		var endpointWithdrawalErr error
		te, endpointWithdrawalErr = r.withdrawTunnelEndpoints(ctx, tunnelProxy, tunnelConfig.ClientServiceName, te)
		if endpointWithdrawalErr != nil {
			tlog.Error(endpointWithdrawalErr, "Failed to withdraw tunnel Endpoints, will retry")
		}

		serverServiceName := types.NamespacedName{
			Namespace: tunnelConfig.ServerServiceNamespace,
			Name:      tunnelConfig.ServerServiceName,
		}
		message := fmt.Sprintf(
			"Waiting for server Service %q; client listener remains reserved on port %d and rejects new connections",
			serverServiceName.String(),
			originalTunnelStatus.ClientProxyPort,
		)
		if suspendErr != nil {
			message = fmt.Sprintf("Failed to suspend tunnel while server Service %q is unavailable: %v", serverServiceName.String(), suspendErr)
			tlog.Error(suspendErr, "Failed to suspend tunnel, will retry")
		}
		if endpointWithdrawalErr != nil {
			message = fmt.Sprintf("%s; failed to withdraw client Endpoints: %v", message, endpointWithdrawalErr)
		}

		pd.tunnelExtra[tunnelConfig.Name] = te
		statusUpdated := markTunnelNotReady(pd, originalTunnelStatus, message)
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
		if statusUpdated {
			return statusChanged | additionalReconciliationNeeded
		}
		return additionalReconciliationNeeded
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

	if originalTunnelStatus.TunnelID != 0 {
		backendChanged := te.serverAddress != serverSvc.Status.EffectiveAddress ||
			te.serverPort != serverSvc.Status.EffectivePort
		needsResume := originalTunnelStatus.State != apiv1.TunnelStateReady ||
			!te.acceptanceStateKnown ||
			!te.acceptingNewStreams ||
			backendChanged
		if !needsResume {
			return noChange
		}

		te.nextPreparationNoEarlierThan = time.Now().Add(delayDurations[StandardDelay].Duration * 9 / 10)
		pd.tunnelExtra[tunnelConfig.Name] = te
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)

		resumeErr := r.updateTunnel(ctx, originalTunnelStatus, serverSvc, true, pd)
		if resumeErr != nil {
			te.acceptanceStateKnown = false
			pd.tunnelExtra[tunnelConfig.Name] = te
			message := fmt.Sprintf("Failed to resume tunnel with server Service endpoint %s:%d: %v",
				serverSvc.Status.EffectiveAddress,
				serverSvc.Status.EffectivePort,
				resumeErr,
			)
			tlog.Error(resumeErr, "Failed to resume tunnel, will retry")
			statusUpdated := markTunnelNotReady(pd, originalTunnelStatus, message)
			r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
			if statusUpdated {
				return statusChanged | additionalReconciliationNeeded
			}
			return additionalReconciliationNeeded
		}

		te.serverAddress = serverSvc.Status.EffectiveAddress
		te.serverPort = serverSvc.Status.EffectivePort
		te.acceptingNewStreams = true
		te.acceptanceStateKnown = true
		te.nextPreparationNoEarlierThan = time.Time{}
		pd.tunnelExtra[tunnelConfig.Name] = te
		readyStatus := originalTunnelStatus.Clone()
		readyStatus.State = apiv1.TunnelStateReady
		readyStatus.ErrorMessage = ""
		readyStatus.Timestamp = metav1.NewMicroTime(time.Now())
		pd.setTunnelStatus(readyStatus)
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
		return statusChanged
	}

	// The remaining path prepares a tunnel that has not published a listener yet.
	// An in-flight request is retried unchanged even when the current backend is unavailable.
	if !serverServiceHasAddress && te.pendingPreparationRequest == nil {
		return additionalReconciliationNeeded
	}

	// CONSIDER: having a spec property for choosing server proxy control address
	// (the one that server proxy listens on for control commands)

	publishedPort, wasPublished := pd.publishedPorts[tunnelConfig.Name]
	if !wasPublished && te.preparationAttempts >= r.config.MaxTunnelPreparationAttempts {
		tlog.Error(errors.New("maximum number of preparation attempts reached"), "Failed to prepare tunnel")
		pd.setTunnelStatus(failedTunnelStatus(originalTunnelStatus, "Failed to prepare tunnel (maximum number of preparation attempts reached)"))
		return statusChanged
	}

	// Set the next preparation earliest time to 90% of the minimum standard delay for additional reconciliation.
	te.nextPreparationNoEarlierThan = time.Now().Add(delayDurations[StandardDelay].Duration * 9 / 10)
	pd.tunnelExtra[tunnelConfig.Name] = te
	r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)

	serverProxyClient, serverProxyConn, serverProxyClientErr := r.createProxyClient(pd)
	if serverProxyClientErr != nil {
		// This should really never happen. No I/O is performed here; the error most likely indicates misconfiguration of the gRPC client.
		tlog.Error(serverProxyClientErr, "Failed to create gRPC connection to server proxy control endpoint")
		pd.setTunnelStatus(failedTunnelStatus(originalTunnelStatus, fmt.Sprintf("Failed to create gRPC connection to server proxy control endpoint: %v", serverProxyClientErr)))
		return additionalReconciliationNeeded
	}
	defer func() {
		closeErr := serverProxyConn.Close()
		if closeErr != nil {
			tlog.Error(closeErr, "Failed to close gRPC connection to server proxy control endpoint")
		}
	}()

	prepareCtx, prepareCtxCancel := context.WithTimeout(ctx, tunnelOperationTimeout)
	defer prepareCtxCancel()

	if te.pendingPreparationRequest == nil {
		tunnelReq := &dcptunproto.TunnelReq{
			TunnelName:    stdproto.String(tunnelConfig.Name),
			ServerAddress: stdproto.String(serverSvc.Status.EffectiveAddress),
			ServerPort:    stdproto.Int32(serverSvc.Status.EffectivePort),
		}
		if wasPublished {
			tunnelReq.ClientProxyPort = stdproto.Int32(publishedPort)
		} else {
			// Prefer exposing the tunnel on the same port as the server Service. The client proxy falls back
			// to a random port within the same request, so retrying the request after a lost response
			// cannot leave behind a tunnel that the controller does not know about.
			tunnelReq.ClientProxyPort = stdproto.Int32(serverSvc.Status.EffectivePort)
			tunnelReq.AllowClientProxyPortFallback = stdproto.Bool(true)
			tunnelReq.ExcludedClientProxyPorts = reservedClientProxyPorts(pd, tunnelConfig.Name)
		}
		te.pendingPreparationRequest = tunnelReq
		pd.tunnelExtra[tunnelConfig.Name] = te
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	}

	tSpec, prepareErr := serverProxyClient.PrepareTunnel(
		prepareCtx,
		stdproto.CloneOf(te.pendingPreparationRequest),
		grpc.WaitForReady(true),
	)
	if prepareErr != nil {
		te.preparationAttempts++
		pd.tunnelExtra[tunnelConfig.Name] = te
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
		if !wasPublished {
			tlog.Error(prepareErr, "Failed to prepare tunnel, will retry...")
			return additionalReconciliationNeeded
		}

		tlog.Error(prepareErr, "Failed to rebind the published tunnel port, will retry", "Port", publishedPort)
		statusUpdated := markTunnelNotReady(
			pd,
			originalTunnelStatus,
			fmt.Sprintf("Failed to rebind published tunnel port %d: %v", publishedPort, prepareErr),
		)
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
		if statusUpdated {
			return statusChanged | additionalReconciliationNeeded
		}
		return additionalReconciliationNeeded
	}
	if wasPublished && tSpec.GetClientProxyPort() != publishedPort {
		portMismatchErr := fmt.Errorf("proxy returned port %d instead of published port %d", tSpec.GetClientProxyPort(), publishedPort)
		tlog.Error(portMismatchErr, "Refusing to change an existing tunnel URL")
		statusUpdated := markTunnelNotReady(pd, originalTunnelStatus, portMismatchErr.Error())
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
		if statusUpdated {
			return statusChanged | additionalReconciliationNeeded
		}
		return additionalReconciliationNeeded
	}

	tlog.V(1).Info("Tunnel prepared successfully")
	ts := originalTunnelStatus.Clone()
	ts.State = apiv1.TunnelStateNotReady
	ts.ErrorMessage = ""
	ts.TunnelID = tSpec.GetTunnelRef().GetTunnelId()
	ts.Timestamp = metav1.NewMicroTime(time.Now())
	ts.ClientProxyAddresses = tSpec.GetClientProxyAddresses()
	ts.ClientProxyPort = tSpec.GetClientProxyPort()
	if pd.publishedPorts == nil {
		pd.publishedPorts = make(map[string]int32)
	}
	pd.publishedPorts[tunnelConfig.Name] = ts.ClientProxyPort
	pd.setTunnelStatus(ts)

	te.preparationAttempts = 0
	te.nextPreparationNoEarlierThan = time.Time{}
	te.pendingPreparationRequest = nil
	te.serverAddress = tSpec.GetServerAddress()
	te.serverPort = tSpec.GetServerPort()
	te.acceptingNewStreams = true
	te.acceptanceStateKnown = true
	pd.tunnelExtra[tunnelConfig.Name] = te
	r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)

	if !serverServiceHasAddress {
		suspendErr := r.updateTunnel(ctx, ts, nil, false, pd)
		if suspendErr != nil {
			te.acceptanceStateKnown = false
			ts.ErrorMessage = fmt.Sprintf("Prepared tunnel but failed to suspend it while the server Service is unavailable: %v", suspendErr)
		} else {
			te.acceptingNewStreams = false
			te.acceptanceStateKnown = true
			ts.ErrorMessage = "Prepared tunnel is waiting for its server Service endpoint"
		}
		ts.Timestamp = metav1.NewMicroTime(time.Now())
		pd.setTunnelStatus(ts)
		pd.tunnelExtra[tunnelConfig.Name] = te
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
		return statusChanged | additionalReconciliationNeeded
	}

	backendChanged := te.serverAddress != serverSvc.Status.EffectiveAddress ||
		te.serverPort != serverSvc.Status.EffectivePort
	if backendChanged {
		retargetErr := r.updateTunnel(ctx, ts, serverSvc, true, pd)
		if retargetErr != nil {
			te.acceptanceStateKnown = false
			ts.ErrorMessage = fmt.Sprintf(
				"Prepared tunnel but failed to retarget it to server Service endpoint %s:%d: %v",
				serverSvc.Status.EffectiveAddress,
				serverSvc.Status.EffectivePort,
				retargetErr,
			)
			ts.Timestamp = metav1.NewMicroTime(time.Now())
			pd.setTunnelStatus(ts)
			pd.tunnelExtra[tunnelConfig.Name] = te
			r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
			return statusChanged | additionalReconciliationNeeded
		}
		te.serverAddress = serverSvc.Status.EffectiveAddress
		te.serverPort = serverSvc.Status.EffectivePort
	}

	ts.State = apiv1.TunnelStateReady
	ts.Timestamp = metav1.NewMicroTime(time.Now())
	pd.setTunnelStatus(ts)
	te.acceptingNewStreams = true
	te.acceptanceStateKnown = true
	pd.tunnelExtra[tunnelConfig.Name] = te
	r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	return statusChanged
}

// deleteTunnel attempts to delete an existing tunnel and its owned endpoints.
func (r *ContainerNetworkTunnelProxyReconciler) deleteTunnel(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	tunnelStatus apiv1.TunnelStatus,
	pd *containerNetworkTunnelProxyData,
) error {
	te, hasExtra := pd.tunnelExtra[tunnelStatus.Name]
	var proxyDeleteErr error
	var tunnelRef *dcptunproto.TunnelRef
	if tunnelStatus.TunnelID != 0 {
		tunnelRef = &dcptunproto.TunnelRef{TunnelId: stdproto.Uint32(tunnelStatus.TunnelID)}
	} else if hasExtra && te.pendingPreparationRequest != nil {
		tunnelRef = &dcptunproto.TunnelRef{TunnelName: stdproto.String(tunnelStatus.Name)}
	}
	if tunnelRef != nil {
		serverProxyClient, serverProxyConn, serverProxyClientErr := r.createProxyClient(pd)
		if serverProxyClientErr != nil {
			proxyDeleteErr = fmt.Errorf("create gRPC connection to server proxy control endpoint: %w", serverProxyClientErr)
		} else {
			deleteCtx, deleteCtxCancel := context.WithTimeout(ctx, tunnelOperationTimeout)
			_, deleteErr := serverProxyClient.DeleteTunnel(deleteCtx, tunnelRef, grpc.WaitForReady(true))
			deleteCtxCancel()
			closeErr := serverProxyConn.Close()
			if deleteErr != nil && status.Code(deleteErr) != codes.NotFound {
				proxyDeleteErr = fmt.Errorf("delete tunnel from proxy pair: %w", deleteErr)
			} else if hasExtra && te.pendingPreparationRequest != nil {
				te.pendingPreparationRequest = nil
				pd.tunnelExtra[tunnelStatus.Name] = te
			}
			if closeErr != nil {
				proxyDeleteErr = errors.Join(proxyDeleteErr, fmt.Errorf("close gRPC connection to server proxy control endpoint: %w", closeErr))
			}
		}
	}

	// We also need to remove the Endpoint objects created for this tunnel.
	// ensureEndpointsForWorkload() will not do this because the TunnelConfiguration no longer exists in the spec
	// and our DynamicEndpointProducer will not say that this ContainerNetworkTunnelProxy produces
	// the Service associated with deleted TunnelConfiguration.
	var endpointDeleteErr error
	if hasExtra {
		var updatedExtra tunnelExtraData
		updatedExtra, endpointDeleteErr = r.withdrawTunnelEndpoints(ctx, tunnelProxy, te.clientServiceName, te)
		pd.tunnelExtra[tunnelStatus.Name] = updatedExtra
	}

	return errors.Join(proxyDeleteErr, endpointDeleteErr)
}

func failAllExistingTunnels(pd *containerNetworkTunnelProxyData) objectChange {
	change := noChange

	for i, ts := range pd.TunnelStatuses {
		if ts.State != apiv1.TunnelStateFailed {
			pd.TunnelStatuses[i] = failedTunnelStatus(ts, "The container proxy failed and tunnels are no longer usable")
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

func markTunnelDeleting(
	pd *containerNetworkTunnelProxyData,
	original apiv1.TunnelStatus,
	errorMessage string,
) bool {
	if original.State == apiv1.TunnelStateDeleting && original.ErrorMessage == errorMessage {
		return false
	}

	ts := original.Clone()
	ts.State = apiv1.TunnelStateDeleting
	ts.ErrorMessage = errorMessage
	ts.Timestamp = metav1.NewMicroTime(time.Now())
	pd.setTunnelStatus(ts)
	return true
}

func markTunnelNotReady(
	pd *containerNetworkTunnelProxyData,
	original apiv1.TunnelStatus,
	errorMessage string,
) bool {
	if original.State == apiv1.TunnelStateNotReady && original.ErrorMessage == errorMessage {
		return false
	}

	tunnelStatus := original.Clone()
	tunnelStatus.State = apiv1.TunnelStateNotReady
	tunnelStatus.ErrorMessage = errorMessage
	tunnelStatus.Timestamp = metav1.NewMicroTime(time.Now())
	pd.setTunnelStatus(tunnelStatus)
	return true
}

func (r *ContainerNetworkTunnelProxyReconciler) withdrawTunnelEndpoints(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	clientServiceName string,
	extra tunnelExtraData,
) (tunnelExtraData, error) {
	remainingEndpointNames := make([]types.NamespacedName, 0, len(extra.clientServiceEndpointNames))
	var withdrawalErr error
	for _, endpointName := range extra.clientServiceEndpointNames {
		endpoint := &apiv1.Endpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:      endpointName.Name,
				Namespace: endpointName.Namespace,
			},
		}
		deleteErr := r.Client.Delete(ctx, endpoint, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground))
		if deleteErr != nil && !apimachinery_errors.IsNotFound(deleteErr) {
			remainingEndpointNames = append(remainingEndpointNames, endpointName)
			withdrawalErr = errors.Join(
				withdrawalErr,
				fmt.Errorf("delete Endpoint %q: %w", endpointName.String(), deleteErr),
			)
		}
	}

	extra.clientServiceEndpointNames = remainingEndpointNames
	if len(remainingEndpointNames) == 0 && clientServiceName != "" {
		workloadEndpointCache.Delete(serviceWorkloadEndpointKey(r.Scheme(), tunnelProxy, clientServiceName))
	}
	return extra, withdrawalErr
}

// removeAllTunnelProxyEndpoints deletes both tracked per-tunnel Endpoints and any child Endpoints
// visible in the controller cache, returning whether pd.tunnelExtra changed alongside any error.
func (r *ContainerNetworkTunnelProxyReconciler) removeAllTunnelProxyEndpoints(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) (bool, error) {
	var trackedWithdrawalErr error
	dataChanged := false
	for tunnelName, extra := range pd.tunnelExtra {
		if len(extra.clientServiceEndpointNames) == 0 && extra.clientServiceName == "" {
			continue
		}
		updatedExtra, withdrawErr := r.withdrawTunnelEndpoints(ctx, tunnelProxy, extra.clientServiceName, extra)
		if !updatedExtra.Equal(extra) {
			pd.tunnelExtra[tunnelName] = updatedExtra
			dataChanged = true
		}
		if withdrawErr != nil {
			log.Error(withdrawErr, "Failed to withdraw tracked tunnel Endpoints", "TunnelName", tunnelName)
			trackedWithdrawalErr = errors.Join(trackedWithdrawalErr, withdrawErr)
		}
	}

	workloadRemovalErr := removeEndpointsForWorkload(ctx, r, tunnelProxy, log)
	return dataChanged, errors.Join(trackedWithdrawalErr, workloadRemovalErr)
}

func reservedClientProxyPorts(pd *containerNetworkTunnelProxyData, excludedTunnelName string) []int32 {
	reservedSet := make(map[int32]struct{}, len(pd.publishedPorts))
	for tunnelName, publishedPort := range pd.publishedPorts {
		if tunnelName != excludedTunnelName {
			reservedSet[publishedPort] = struct{}{}
		}
	}

	reservedPorts := make([]int32, 0, len(reservedSet))
	for publishedPort := range reservedSet {
		reservedPorts = append(reservedPorts, publishedPort)
	}
	std_slices.Sort(reservedPorts)
	return reservedPorts
}

func (r *ContainerNetworkTunnelProxyReconciler) updateTunnel(
	ctx context.Context,
	tunnelStatus apiv1.TunnelStatus,
	serverService *apiv1.Service,
	acceptNewStreams bool,
	pd *containerNetworkTunnelProxyData,
) error {
	serverProxyClient, serverProxyConn, serverProxyClientErr := r.createProxyClient(pd)
	if serverProxyClientErr != nil {
		return fmt.Errorf("create gRPC connection to server proxy control endpoint: %w", serverProxyClientErr)
	}

	update := &dcptunproto.TunnelUpdate{
		TunnelRef:        &dcptunproto.TunnelRef{TunnelId: stdproto.Uint32(tunnelStatus.TunnelID)},
		AcceptNewStreams: stdproto.Bool(acceptNewStreams),
	}
	if acceptNewStreams {
		if serverService == nil {
			_ = serverProxyConn.Close()
			return errors.New("server Service is required when resuming a tunnel")
		}
		update.ServerAddress = stdproto.String(serverService.Status.EffectiveAddress)
		update.ServerPort = stdproto.Int32(serverService.Status.EffectivePort)
	}

	updateCtx, updateCtxCancel := context.WithTimeout(ctx, tunnelOperationTimeout)
	_, updateErr := serverProxyClient.UpdateTunnel(updateCtx, update, grpc.WaitForReady(true))
	updateCtxCancel()
	closeErr := serverProxyConn.Close()
	if updateErr != nil {
		updateErr = fmt.Errorf("update tunnel in proxy pair: %w", updateErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close gRPC connection to server proxy control endpoint: %w", closeErr)
	}
	return errors.Join(updateErr, closeErr)
}

func (r *ContainerNetworkTunnelProxyReconciler) createProxyClient(
	pd *containerNetworkTunnelProxyData,
) (dcptunproto.TunnelControlClient, *grpc.ClientConn, error) {
	if pd.securityConfig == nil {
		return nil, nil, fmt.Errorf("cannot create tunnel proxy client: security configuration is missing") // Should never happen
	}

	controllerSecurityConfig := pd.securityConfig.ControllerConfig()
	dialOptions, dialOptionsErr := controllerSecurityConfig.ClientDialOptions()
	if dialOptionsErr != nil {
		return nil, nil, dialOptionsErr
	}

	serverProxyConn, serverProxyErr := grpc.NewClient(
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, pd.ServerProxyControlPort),
		dialOptions...,
	)
	if serverProxyErr != nil {
		return nil, nil, serverProxyErr
	}

	serverProxyClient := r.config.MakeTunnelControlClient(serverProxyConn)
	return serverProxyClient, serverProxyConn, nil
}

// INITIALIZATION AND SHUTDOWN HELPER METHODS

// Returns a function that starts the tunnel proxy pair.
// The method is called as part of the reconciliation loop, but the returned function is executed asynchronously.
// The passed proxy data is a state clone that shares startup coordination and owned resources.
func (r *ContainerNetworkTunnelProxyReconciler) startProxyPair(
	startupCtx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	releaseStartup func(),
	log logr.Logger,
) func(context.Context) {
	return func(ctx context.Context) {
		nn := tunnelProxy.NamespacedName()
		reconciliationDelay := NoDelay
		var serverRun *serverProxyRun
		var serverStarted bool

		securityConfigErr := r.createProxyControlSecurityConfig(startupCtx, pd, log)
		if securityConfigErr != nil {
			startupInterrupted := startupCtx.Err() != nil || commonapi.ResourceCreationProhibited.Load()
			if !startupInterrupted && !errors.Is(securityConfigErr, context.Canceled) && !errors.Is(securityConfigErr, context.DeadlineExceeded) {
				log.Error(securityConfigErr, "Failed to create tunnel proxy control security configuration")
				pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
				pd.Message = fmt.Sprintf("Failed to create tunnel proxy control security configuration: %v", securityConfigErr)
			} else if !startupInterrupted {
				reconciliationDelay = StandardDelay
			}
		} else {
			var clientCtrCreated bool
			clientCtrCreated, reconciliationDelay = r.startClientProxy(startupCtx, tunnelProxy, pd, log)
			if !clientCtrCreated && (startupCtx.Err() != nil || commonapi.ResourceCreationProhibited.Load()) {
				reconciliationDelay = NoDelay
			}

			if clientCtrCreated {
				// Start server proxy now that client proxy ports are known.
				// startupCtx aborts startup waits promptly when deletion or shutdown begins,
				// while ctx preserves the work queue's drain budget for stopping the process.
				serverRun, serverStarted = r.startServerProxy(startupCtx, ctx, tunnelProxy, pd, log)
				if serverStarted {
					log.V(1).Info("Server proxy started successfully, scheduling reconciliation")
					if pd.pairGeneration != 0 {
						pd.State = apiv1.ContainerNetworkTunnelProxyStateRunning
					} else {
						pd.State = apiv1.ContainerNetworkTunnelProxyStateStarted
					}
				} else if pd.State == apiv1.ContainerNetworkTunnelProxyStateStarting &&
					startupCtx.Err() == nil &&
					!commonapi.ResourceCreationProhibited.Load() {
					reconciliationDelay = StandardDelay
				}
			}
		}

		published := r.publishProxyPairStartupResult(ctx, nn, tunnelProxy.UID, pd, serverRun, serverStarted, releaseStartup, reconciliationDelay)
		if !published {
			cleanup := r.startAbandonedProxyPairCleanup(nn, tunnelProxy.UID, pd, serverRun, log)
			cleanup(ctx)
		}
	}
}

func (r *ContainerNetworkTunnelProxyReconciler) publishProxyPairStartupResult(
	ctx context.Context,
	proxyName types.NamespacedName,
	proxyUID types.UID,
	result *containerNetworkTunnelProxyData,
	run *serverProxyRun,
	serverStarted bool,
	releaseStartup func(),
	reconciliationDelay AdditionalReconciliationDelay,
) bool {
	pdMap := r.proxyData
	publication := concurrency.NewValuePromise[bool]()
	var publishOnce sync.Once
	queued := pdMap.QueueDeferredOp(proxyName, func(_ types.NamespacedName, _ types.NamespacedName, proxy *apiv1.ContainerNetworkTunnelProxy) {
		publishOnce.Do(func() {
			defer releaseStartup()
			if proxy.UID != proxyUID {
				_, current := pdMap.BorrowByNamespacedName(proxyName)
				if current != nil && current.resourceUID == proxyUID {
					pdMap.DeleteByNamespacedName(proxyName)
				}
				publication.Set(false)
				return
			}
			_, current := pdMap.BorrowByNamespacedName(proxyName)
			if current == nil || current.resourceUID != proxyUID || current.pairGeneration != result.pairGeneration ||
				current.State != apiv1.ContainerNetworkTunnelProxyStateStarting {
				publication.Set(false)
				return
			}
			// A failed startup already classified its server exit; only a newly ready server needs this transition.
			if serverStarted {
				exit, exited := run.getExit()
				if exited && exit.exitType == serverProxyExitTypeUnexpected {
					current.UpdateFrom(result)
					applyUnexpectedServerProxyExit(current, run.handle.Pid, exit)
					pdMap.Update(proxyName, proxyName, current)
					publication.Set(true)
					return
				}
			}
			pdMap.Update(proxyName, proxyName, result)
			publication.Set(true)
		})
	})
	if queued {
		r.ScheduleReconciliationWithDelay(proxyName, reconciliationDelay)
		select {
		case <-publication.Wait():
		case <-r.LifetimeCtx.Done():
		case <-ctx.Done():
		}
	}
	// Ownership must transfer exactly once, even if shutdown races with deferred publication.
	publishOnce.Do(func() {
		releaseStartup()
		publication.Set(false)
	})
	return publication.Get()
}

func (r *ContainerNetworkTunnelProxyReconciler) startAbandonedProxyPairCleanup(
	proxyName types.NamespacedName,
	proxyUID types.UID,
	result *containerNetworkTunnelProxyData,
	run *serverProxyRun,
	log logr.Logger,
) func(context.Context) {
	tunnelProxyAbandonedStartupResultCounter.Add(context.Background(), 1)
	if run != nil {
		run.expectExit()
	}

	cleanupData := result.Clone()
	containerName := tunnelProxyPhysicalContainerNameForUID(proxyUID, result.pairGeneration)
	serverHandle, _ := serverProxyHandle(cleanupData)
	cleanupLog := log.WithValues(
		"OldProxyUID", proxyUID,
		"PhysicalContainer", containerName.String(),
		"ServerPID", serverHandle.Pid,
		"ServerIdentityTime", process.FormatIdentityTime(serverHandle.IdentityTime),
	)
	var cleanup resiliency.WorkQueueItem
	cleanup = func(ctx context.Context) {
		if !abandonedProxyPairHasCleanupWork(cleanupData) {
			cleanupLog.V(1).Info(
				"Abandoned tunnel proxy startup result owns no cleanup resources",
				"CleanupStage", tunnelProxyCleanupStageComplete,
				"CleanupConfirmed", true,
			)
			return
		}
		for {
			completed, submitted := r.cleanupAbandonedProxyPair(ctx, cleanupData, proxyName, proxyUID, cleanupLog)
			if completed || ctx.Err() != nil {
				return
			}
			// Once controller shutdown has begun, PhysicalContainerController stops reconciling finalizers;
			// after host process stop and API Delete submissions succeed, allow workQueue to finish draining
			// so clientSecurityFiles.Cleanup() can sweep remaining credential files promptly.
			if r.LifetimeCtx.Err() != nil && submitted {
				return
			}
			if r.LifetimeCtx.Err() == nil {
				if retryErr := r.workQueue.EnqueueAfter(delayDuration(StandardDelay), cleanup); retryErr == nil {
					return
				}
			}
			retryTimer := time.NewTimer(abandonedCleanupShutdownRetryDelay)
			select {
			case <-retryTimer.C:
			case <-ctx.Done():
				retryTimer.Stop()
				return
			}
		}
	}
	return cleanup
}

func (r *ContainerNetworkTunnelProxyReconciler) cleanupAbandonedProxyPair(
	ctx context.Context,
	pd *containerNetworkTunnelProxyData,
	proxyName types.NamespacedName,
	proxyUID types.UID,
	log logr.Logger,
) (cleanupCompleted bool, submissionCompleted bool) {
	cleanupCompleted = true
	submissionCompleted = true
	cleanupUnconfirmed := false
	defer func() {
		outputCleanupErr := removeStoppedServerProxyOutputFiles(ctx, pd)
		if outputCleanupErr != nil {
			cleanupCompleted = false
			submissionCompleted = false
			cleanupUnconfirmed = true
			log.Error(
				outputCleanupErr,
				"Failed to clean up local output files for abandoned tunnel proxy startup result",
				"CleanupStage", tunnelProxyCleanupStageCloseOutput,
				"CleanupConfirmed", false,
			)
		}
		if cleanupUnconfirmed {
			tunnelProxyCleanupUnconfirmedCounter.Add(context.Background(), 1)
		}
		log.Info(
			"Completed cleanup attempt for abandoned tunnel proxy startup resources",
			"CleanupStage", tunnelProxyCleanupStageComplete,
			"CleanupConfirmed", cleanupCompleted,
		)
	}()

	if _, found := serverProxyHandle(pd); found {
		stopCtx, stopCancel := process.WithStopTimeout(ctx)
		stopErr := r.stopServerProxyProcess(stopCtx, pd)
		stopCancel()
		if stopErr != nil {
			cleanupCompleted = false
			submissionCompleted = false
			cleanupUnconfirmed = true
			tunnelProxyProcessStopFailureCounter.Add(context.Background(), 1)
			log.Error(
				stopErr,
				"Failed to stop server process for abandoned tunnel proxy startup result",
				"CleanupStage", tunnelProxyCleanupStageServerProcessStop,
				"CleanupConfirmed", false,
			)
		}
	}

	deleteCtx, deleteCancel := context.WithTimeout(ctx, tunnelOperationTimeout)
	physicalResourcesRemoved, deleteErr := r.cleanupClientPhysicalResources(deleteCtx, proxyUID, pd)
	if proxyName.Name != "" && proxyUID != "" {
		ownerStub := &apiv1.ContainerNetworkTunnelProxy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      proxyName.Name,
				Namespace: proxyName.Namespace,
				UID:       proxyUID,
			},
		}
		if _, endpointRemovalErr := r.removeAllTunnelProxyEndpoints(deleteCtx, ownerStub, pd, log); endpointRemovalErr != nil {
			cleanupCompleted = false
			submissionCompleted = false
			cleanupUnconfirmed = true
		}
	}
	deleteCancel()
	if deleteErr != nil && !apimachinery_errors.IsNotFound(deleteErr) {
		cleanupCompleted = false
		submissionCompleted = false
		cleanupUnconfirmed = true
		tunnelProxyDeleteSubmissionFailureCounter.Add(context.Background(), 1)
		log.Error(
			deleteErr,
			"Failed to submit PhysicalContainer deletion for abandoned tunnel proxy startup result",
			"CleanupStage", tunnelProxyCleanupStagePhysicalContainerDelete,
			"CleanupConfirmed", false,
		)
	}
	if deleteErr == nil && !physicalResourcesRemoved {
		cleanupCompleted = false
	}
	if deleteErr == nil && physicalResourcesRemoved {
		securityFileErr := r.removeClientProxySecurityFile(pd)
		if securityFileErr != nil {
			cleanupCompleted = false
			submissionCompleted = false
			cleanupUnconfirmed = true
			log.Error(
				securityFileErr,
				"Failed to remove client control security file for abandoned tunnel proxy startup result",
				"CleanupStage", tunnelProxyCleanupStageRemoveSecurityFile,
				"CleanupConfirmed", false,
			)
		}
	}

	return cleanupCompleted, submissionCompleted
}

func (r *ContainerNetworkTunnelProxyReconciler) discardMissingTunnelProxyState(
	name types.NamespacedName,
	log logr.Logger,
) {
	_, data := r.proxyData.BorrowByNamespacedName(name)
	if data == nil {
		return
	}

	tombstone := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name.Name,
			Namespace: name.Namespace,
			UID:       data.resourceUID,
		},
	}
	r.proxyData.RunDeferredOps(name, tombstone)
	_, data = r.proxyData.BorrowByNamespacedName(name)
	if data == nil {
		return
	}

	_, releaseStartup, acquiredStartup := data.startup.TryAcquire(r.LifetimeCtx)
	if !acquiredStartup {
		data.startup.CancelActive()
		r.ScheduleReconciliationWithDelay(name, StandardDelay)
		return
	}
	releaseStartup()

	if data.State == apiv1.ContainerNetworkTunnelProxyStateDeleted {
		r.proxyData.DeleteByNamespacedName(name)
		return
	}
	cleanupErr := r.workQueue.Enqueue(r.startAbandonedProxyPairCleanup(name, data.resourceUID, data, nil, log))
	if cleanupErr != nil {
		log.Error(cleanupErr, "Failed to queue abandoned tunnel proxy startup cleanup",
			"CleanupStage", tunnelProxyCleanupStageSchedule)
	}
	r.proxyData.DeleteByNamespacedName(name)
}

// resetRecreatedTunnelProxyState removes state owned by a previous object with the same name.
// It returns true while startup publication for the previous object is still in flight.
func (r *ContainerNetworkTunnelProxyReconciler) resetRecreatedTunnelProxyState(
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	log logr.Logger,
) bool {
	name := tunnelProxy.NamespacedName()
	_, data := r.proxyData.BorrowByNamespacedName(name)
	if data == nil || data.resourceUID == "" || data.resourceUID == tunnelProxy.UID {
		return false
	}

	_, releaseStartup, acquiredStartup := data.startup.TryAcquire(r.LifetimeCtx)
	if !acquiredStartup {
		data.startup.CancelActive()
		log.V(1).Info(
			"Waiting for startup work owned by a previous ContainerNetworkTunnelProxy instance",
			"PreviousUID", data.resourceUID,
			"CurrentUID", tunnelProxy.UID,
		)
		return true
	}
	releaseStartup()

	if data.State == apiv1.ContainerNetworkTunnelProxyStateDeleted {
		r.proxyData.DeleteByNamespacedName(name)
		return false
	}
	cleanupErr := r.workQueue.Enqueue(r.startAbandonedProxyPairCleanup(name, data.resourceUID, data, nil, log))
	if cleanupErr != nil {
		log.Error(cleanupErr, "Failed to queue abandoned tunnel proxy startup cleanup",
			"CleanupStage", tunnelProxyCleanupStageSchedule)
	}
	r.proxyData.DeleteByNamespacedName(name)
	return false
}

// Creates the credentials and client bootstrap file for authenticated tunnel proxy control traffic.
// The passed containerNetworkTunnelProxy data will be updated with the created configuration if the method is successful.
// Regardless of the outcome, the caller should schedule a reconciliation of the given tunnel proxy object.
func (r *ContainerNetworkTunnelProxyReconciler) createProxyControlSecurityConfig(
	ctx context.Context,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) error {
	if cancellationErr := ctx.Err(); cancellationErr != nil {
		return cancellationErr
	}
	if pd.securityConfig != nil && pd.clientSecurityFile != "" {
		log.V(1).Info("Tunnel proxy control security configuration is already available, nothing to do...")
		return nil
	}

	securityConfig := pd.securityConfig
	if securityConfig == nil {
		generatedSecurityConfig, secConfErr := dcptun.NewTunnelProxySecurityConfig()
		if secConfErr != nil {
			log.Error(secConfErr, "Failed to create security configuration for tunnel proxy connection")
			pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
			pd.Message = fmt.Sprintf("Failed to create security configuration for tunnel proxy connection: %v", secConfErr)
			return secConfErr
		}
		securityConfig = &generatedSecurityConfig
		pd.securityConfig = securityConfig
	}

	clientSecurityJSON, marshalErr := json.Marshal(securityConfig.ClientProxyConfig())
	if marshalErr != nil {
		return fmt.Errorf("encode client proxy control security configuration: %w", marshalErr)
	}
	securityFilePath, createErr := r.clientSecurityFiles.Create(
		ctx,
		fmt.Sprintf("tunnel-control-%s-%d-", pd.resourceUID, pd.pairGeneration),
		clientProxySecurityFileName,
		clientSecurityJSON,
	)
	if createErr != nil {
		return fmt.Errorf("create client proxy control security file: %w", createErr)
	}
	pd.clientSecurityFile = securityFilePath
	return nil
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
		pd.Message = fmt.Sprintf("Failed to retrieve ContainerNetwork %q necessary for starting the client proxy container: %v", containerNetworkName.String(), cnErr)
		return false, StandardDelay
	}
	if containerNetwork.Status.State != apiv1.ContainerNetworkStateRunning || containerNetwork.Status.ID == "" || containerNetwork.Status.NetworkName == "" {
		log.V(1).Info("Referenced ContainerNetwork is not in Running state, cannot start the client proxy container")
		pd.Message = fmt.Sprintf("Waiting for referenced ContainerNetwork %q to be running", containerNetworkName.String())
		return false, StandardDelay
	}

	networkReferenceErr := EnsureV1PhysicalContainerNetworkReference(ctx, r.Client, &containerNetwork)
	if networkReferenceErr != nil {
		log.Error(networkReferenceErr, "Failed to ensure client proxy PhysicalContainerNetwork")
		pd.Message = fmt.Sprintf("Failed to ensure client proxy PhysicalContainerNetwork: %v", networkReferenceErr)
		return false, StandardDelay
	}

	physicalContainerName := tunnelProxyPhysicalContainerNameForUID(tunnelProxy.UID, pd.pairGeneration)
	physicalContainer := apiv2.PhysicalContainer{}
	getContainerErr := r.Get(ctx, physicalContainerName, &physicalContainer)
	if apimachinery_errors.IsNotFound(getContainerErr) {
		if pd.securityConfig == nil {
			pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
			pd.Message = "Cannot create client proxy PhysicalContainer: control security configuration is missing"
			return false, NoDelay
		}
		if pd.clientSecurityFile == "" {
			pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
			pd.Message = "Cannot create client proxy PhysicalContainer: client control security file is missing"
			return false, NoDelay
		}
		physicalContainer = apiv2.PhysicalContainer{
			ObjectMeta: metav1.ObjectMeta{
				Name:        physicalContainerName.Name,
				Namespace:   physicalContainerName.Namespace,
				Annotations: tunnelProxyPhysicalResourceAnnotations(tunnelProxy),
			},
			Spec: apiv2.PhysicalContainerSpec{
				Container: &apiv2.PhysicalContainerConfig{
					ImageRef:      V1TunnelProxyPhysicalContainerImageName,
					ContainerName: physicalContainerName.Name,
					Entrypoint:    dcptun.ClientProxyBinaryPath,
					Command: []string{
						"client",
						"--client-control-address", networking.IPv4AllInterfaceAddress,
						"--client-control-port", strconv.Itoa(dcptun.DefaultContainerProxyControlPort),
						"--client-data-address", networking.IPv4AllInterfaceAddress,
						"--client-data-port", strconv.Itoa(dcptun.DefaultContainerProxyDataPort),
						"--control-security-file", clientProxySecurityFilePath,
					},
					Ports: []apiv2.ContainerPort{
						{
							ContainerPort: int32(dcptun.DefaultContainerProxyControlPort),
							HostIP:        networking.IPv4LocalhostDefaultAddress,
						},
						{
							ContainerPort: int32(dcptun.DefaultContainerProxyDataPort),
							HostIP:        networking.IPv4LocalhostDefaultAddress,
						},
					},
					CreateFiles: []apiv2.CreateFileSystem{
						{
							Destination: clientProxySecurityDirectory,
							Entries: []apiv2.FileSystemEntry{
								{
									Name:   clientProxySecurityFileName,
									Mode:   0o600,
									Source: pd.clientSecurityFile,
								},
							},
						},
					},
					Networks: []apiv2.ContainerNetworkConnectionConfig{
						{
							NetworkRef: V1PhysicalContainerNetworkReferenceName(containerNetwork.UID).Name,
							Aliases:    tunnelProxy.Spec.Aliases,
						},
					},
				},
			},
		}
		createContainerErr := r.Client.Create(ctx, &physicalContainer)
		switch {
		case createContainerErr == nil:
			log.V(1).Info("Created client proxy PhysicalContainer", "PhysicalContainer", physicalContainerName)
			pd.clientPhysicalUID = physicalContainer.UID

		case apimachinery_errors.IsAlreadyExists(createContainerErr):
			existingContainer := apiv2.PhysicalContainer{}
			lookupErr := r.NoCacheClient.Get(ctx, physicalContainerName, &existingContainer)
			if lookupErr != nil {
				log.Error(lookupErr, "Failed to resolve existing client proxy PhysicalContainer after create conflict")
				pd.Message = fmt.Sprintf(
					"Waiting to resolve existing client proxy PhysicalContainer %q after create conflict: %v",
					physicalContainerName.String(),
					lookupErr,
				)
				return false, StandardDelay
			}

			if !tunnelProxyPhysicalResourceOwnedBy(&existingContainer, tunnelProxy) {
				pd.attemptProxyRecovery(fmt.Sprintf(
					"Client proxy PhysicalContainer %q is owned by a different tunnel proxy",
					physicalContainerName.String(),
				))
				return false, NoDelay
			}
			if pd.clientPhysicalUID != "" && pd.clientPhysicalUID != existingContainer.UID {
				pd.attemptProxyRecovery(fmt.Sprintf(
					"Client proxy PhysicalContainer %q was replaced",
					physicalContainerName.String(),
				))
				return false, NoDelay
			}
			pd.clientPhysicalUID = existingContainer.UID
			log.V(1).Info("Client proxy PhysicalContainer already exists", "PhysicalContainer", physicalContainerName)

		default:
			if !commonapi.ResourceCreationProhibited.Load() {
				log.Error(createContainerErr, "Failed to create client proxy PhysicalContainer")
			}
			pd.Message = fmt.Sprintf("Failed to create client proxy PhysicalContainer %q: %v", physicalContainerName.String(), createContainerErr)
			return false, StandardDelay
		}

		pd.Message = fmt.Sprintf("Waiting for client proxy PhysicalContainer %q to start", physicalContainerName.String())
		return false, StandardDelay
	}
	if getContainerErr != nil {
		log.Error(getContainerErr, "Failed to get client proxy PhysicalContainer")
		pd.Message = fmt.Sprintf("Failed to get client proxy PhysicalContainer %q: %v", physicalContainerName.String(), getContainerErr)
		return false, StandardDelay
	}

	return r.updateClientProxyContainerStatus(ctx, tunnelProxy, &containerNetwork, pd, log)
}

func tunnelProxyPhysicalImageName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: V1PhysicalResourcesNamespaceName,
		Name:      V1TunnelProxyPhysicalContainerImageName,
	}
}

func tunnelProxyPhysicalContainerNameForUID(tunnelProxyUID types.UID, generation uint64) types.NamespacedName {
	name := fmt.Sprintf("tunnel-proxy-%s", tunnelProxyUID)
	if generation != 0 {
		// Base 36 keeps the full uint64 generation within the DNS-label limit.
		name += "-" + strconv.FormatUint(generation, 36)
	}
	return types.NamespacedName{
		Namespace: V1PhysicalResourcesNamespaceName,
		Name:      name,
	}
}

func tunnelProxyPhysicalResourceAnnotations(tunnelProxy *apiv1.ContainerNetworkTunnelProxy) map[string]string {
	return map[string]string{
		tunnelProxyOwnerNameAnnotation:      tunnelProxy.Name,
		tunnelProxyOwnerNamespaceAnnotation: tunnelProxy.Namespace,
		tunnelProxyOwnerUIDAnnotation:       string(tunnelProxy.UID),
	}
}

func tunnelProxyPhysicalResourceOwnedBy(
	physicalContainer *apiv2.PhysicalContainer,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
) bool {
	if physicalContainer == nil || tunnelProxy == nil {
		return false
	}
	annotations := physicalContainer.GetAnnotations()
	return annotations[tunnelProxyOwnerNameAnnotation] == tunnelProxy.Name &&
		annotations[tunnelProxyOwnerNamespaceAnnotation] == tunnelProxy.Namespace &&
		annotations[tunnelProxyOwnerUIDAnnotation] == string(tunnelProxy.UID)
}

func (r *ContainerNetworkTunnelProxyReconciler) ensureTunnelProxyPhysicalContainerImage(
	startupCtx context.Context,
	cleanupCtx context.Context,
	log logr.Logger,
) error {
	imageName := tunnelProxyPhysicalImageName()
	existingImage := apiv2.PhysicalContainerImage{}
	getErr := r.NoCacheClient.Get(startupCtx, imageName, &existingImage)
	if getErr == nil {
		return nil
	}
	if !apimachinery_errors.IsNotFound(getErr) {
		return fmt.Errorf("get PhysicalContainerImage %q: %w", imageName.String(), getErr)
	}

	imagePlan, prepareErr := dcptun.PrepareClientProxyImageBuild(startupCtx, r.config.BuildContextDir)
	if prepareErr != nil {
		return prepareErr
	}
	if startupCancelErr := startupCtx.Err(); startupCancelErr != nil {
		removeUnusedTunnelProxyBuildContext(imagePlan.BuildContextArchive.Source, nil, log)
		return startupCancelErr
	}
	imageConfig := &apiv2.PhysicalContainerImageConfig{
		Image:       imagePlan.Image,
		PullPolicy:  apiv2.PullPolicyBestEffort,
		BuildPolicy: apiv2.BuildPolicyIfNeeded,
		Build: &apiv2.ContainerBuildContext{
			Digest: imagePlan.BuildContextDigest,
			ContextArchive: &apiv2.ContainerBuildContextArchive{
				Source:      imagePlan.BuildContextArchive.Source,
				SHA256:      imagePlan.BuildContextArchive.SHA256,
				RawContents: imagePlan.BuildContextArchive.RawContents,
			},
			Dockerfile: imagePlan.Dockerfile,
			BaseImages: []string{
				dcptun.DefaultBaseImage,
			},
		},
	}
	physicalImage := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      imageName.Name,
			Namespace: imageName.Namespace,
		},
		Spec: apiv2.PhysicalContainerImageSpec{
			Image: imageConfig,
		},
	}

	createErr := r.Client.Create(startupCtx, physicalImage)
	if createErr != nil {
		lookupCtx, lookupCancel := context.WithTimeout(cleanupCtx, tunnelOperationTimeout)
		defer lookupCancel()
		currentImage := apiv2.PhysicalContainerImage{}
		lookupErr := r.NoCacheClient.Get(lookupCtx, imageName, &currentImage)
		if lookupErr == nil {
			removeUnusedTunnelProxyBuildContext(imagePlan.BuildContextArchive.Source, &currentImage, log)
			return nil
		}
		if apimachinery_errors.IsNotFound(lookupErr) {
			removeUnusedTunnelProxyBuildContext(imagePlan.BuildContextArchive.Source, nil, log)
		}

		return fmt.Errorf(
			"create PhysicalContainerImage %q and verify whether it exists: %w",
			imageName.String(),
			errors.Join(createErr, lookupErr),
		)
	}

	log.V(1).Info("Created shared tunnel proxy PhysicalContainerImage", "PhysicalContainerImage", imageName)
	return nil
}

func (r *ContainerNetworkTunnelProxyReconciler) scheduleTunnelProxyPhysicalContainerImageCreation(
	tunnelProxyName types.NamespacedName,
	log logr.Logger,
) error {
	if !r.sharedImagePreparationLock.TryLock() {
		return nil
	}

	enqueueErr := r.workQueue.Enqueue(func(drainCtx context.Context) {
		defer func() {
			r.sharedImagePreparationLock.Unlock()
			r.ScheduleReconciliation(tunnelProxyName)
		}()
		imageResourceErr := r.ensureTunnelProxyPhysicalContainerImage(r.LifetimeCtx, drainCtx, log)
		if imageResourceErr != nil && r.LifetimeCtx.Err() == nil {
			log.Error(imageResourceErr, "Failed to ensure shared tunnel proxy PhysicalContainerImage")
		}
	})
	if enqueueErr != nil {
		r.sharedImagePreparationLock.Unlock()
		return fmt.Errorf("queue shared tunnel proxy PhysicalContainerImage creation: %w", enqueueErr)
	}

	log.V(1).Info("Scheduled shared tunnel proxy PhysicalContainerImage creation")
	return nil
}

func removeUnusedTunnelProxyBuildContext(
	buildContextSource string,
	physicalImage *apiv2.PhysicalContainerImage,
	log logr.Logger,
) {
	if buildContextSource == "" {
		return
	}
	if physicalImage != nil &&
		physicalImage.Spec.Image != nil &&
		physicalImage.Spec.Image.Build != nil &&
		physicalImage.Spec.Image.Build.ContextArchive != nil &&
		physicalImage.Spec.Image.Build.ContextArchive.Source == buildContextSource {
		return
	}

	removeErr := os.Remove(buildContextSource)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		log.Error(removeErr, "Failed to remove unused tunnel proxy image build context", "Source", buildContextSource)
	}
}

func (r *ContainerNetworkTunnelProxyReconciler) updateClientProxyContainerStatus(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	containerNetwork *apiv1.ContainerNetwork,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) (bool, AdditionalReconciliationDelay) {
	containerName := tunnelProxyPhysicalContainerNameForUID(tunnelProxy.UID, pd.pairGeneration)
	physicalContainer := apiv2.PhysicalContainer{}
	getContainerErr := r.Get(ctx, containerName, &physicalContainer)
	if apimachinery_errors.IsNotFound(getContainerErr) {
		if pd.State == apiv1.ContainerNetworkTunnelProxyStateRunning || pd.State == apiv1.ContainerNetworkTunnelProxyStateStarted {
			pd.attemptProxyRecovery(fmt.Sprintf("Client proxy PhysicalContainer %q no longer exists", containerName.String()))
			return false, NoDelay
		}
		pd.Message = fmt.Sprintf("Waiting for client proxy PhysicalContainer %q to be created", containerName.String())
		return false, StandardDelay
	}
	if getContainerErr != nil {
		log.Error(getContainerErr, "Failed to get client proxy PhysicalContainer")
		pd.Message = fmt.Sprintf("Failed to get client proxy PhysicalContainer %q: %v", containerName.String(), getContainerErr)
		return false, StandardDelay
	}

	if !tunnelProxyPhysicalResourceOwnedBy(&physicalContainer, tunnelProxy) {
		pd.attemptProxyRecovery(fmt.Sprintf("Client proxy PhysicalContainer %q is owned by a different tunnel proxy", containerName.String()))
		return false, NoDelay
	}
	if pd.clientPhysicalUID != "" && pd.clientPhysicalUID != physicalContainer.UID {
		pd.attemptProxyRecovery(fmt.Sprintf("Client proxy PhysicalContainer %q was replaced", containerName.String()))
		return false, NoDelay
	}
	pd.clientPhysicalUID = physicalContainer.UID
	switch physicalContainer.Status.Phase {
	case apiv2.PhysicalContainerPhaseRunning:
		if physicalResourceReadyConditionReason(physicalContainer.Status.Conditions) == apiv2.PhysicalContainerReasonPortMappingResolutionFailed {
			pd.attemptProxyRecovery(fmt.Sprintf("Client proxy PhysicalContainer failed: %s", physicalResourceStatusMessage(physicalContainer.Status.Conditions)))
			return false, NoDelay
		}
	case apiv2.PhysicalContainerPhaseFailed, apiv2.PhysicalContainerPhaseExited:
		pd.attemptProxyRecovery(fmt.Sprintf("Client proxy PhysicalContainer failed: %s", physicalResourceStatusMessage(physicalContainer.Status.Conditions)))
		return false, NoDelay
	case apiv2.PhysicalContainerPhasePaused:
		pd.attemptProxyRecovery(fmt.Sprintf("Client proxy PhysicalContainer failed: %s", physicalResourceStatusMessage(physicalContainer.Status.Conditions)))
		return false, NoDelay
	case apiv2.PhysicalContainerPhaseUnknown:
		if physicalResourceReadyConditionReason(physicalContainer.Status.Conditions) == apiv2.PhysicalContainerReasonRuntimeContainerMissing {
			pd.attemptProxyRecovery(fmt.Sprintf("Client proxy PhysicalContainer failed: %s", physicalResourceStatusMessage(physicalContainer.Status.Conditions)))
			return false, NoDelay
		}
		pd.Message = fmt.Sprintf("Waiting for client proxy PhysicalContainer %q: %s", containerName.String(), physicalResourceStatusMessage(physicalContainer.Status.Conditions))
		return false, StandardDelay
	default:
		pd.Message = fmt.Sprintf("Waiting for client proxy PhysicalContainer %q: %s", containerName.String(), physicalResourceStatusMessage(physicalContainer.Status.Conditions))
		return false, StandardDelay
	}

	networkName := V1PhysicalContainerNetworkReferenceName(containerNetwork.UID)
	physicalNetwork := apiv2.PhysicalContainerNetwork{}
	getNetworkErr := r.Get(ctx, networkName, &physicalNetwork)
	if apimachinery_errors.IsNotFound(getNetworkErr) {
		pd.Message = fmt.Sprintf("Waiting for client proxy PhysicalContainerNetwork %q to be created", networkName.String())
		return false, StandardDelay
	}
	if getNetworkErr != nil {
		log.Error(getNetworkErr, "Failed to get client proxy PhysicalContainerNetwork")
		pd.Message = fmt.Sprintf("Failed to get client proxy PhysicalContainerNetwork %q: %v", networkName.String(), getNetworkErr)
		return false, StandardDelay
	}
	if physicalNetwork.Status.Phase != apiv2.PhysicalContainerNetworkPhaseReady {
		pd.Message = fmt.Sprintf("Waiting for client proxy PhysicalContainerNetwork %q: %s", networkName.String(), physicalResourceStatusMessage(physicalNetwork.Status.Conditions))
		return false, StandardDelay
	}
	if !slices.Contains(physicalNetwork.Status.ContainerIDs, physicalContainer.Status.ContainerID) {
		pd.Message = fmt.Sprintf(
			"Waiting for client proxy PhysicalContainer %q to connect to PhysicalContainerNetwork %q",
			containerName.String(),
			networkName.String(),
		)
		return false, StandardDelay
	}

	controlPort, controlPortErr := physicalContainerHostPort(physicalContainer.Status.PortMappings, int32(dcptun.DefaultContainerProxyControlPort))
	if controlPortErr != nil {
		log.Error(controlPortErr, "Failed to determine control connection host port for the client proxy PhysicalContainer")
		pd.attemptProxyRecovery(fmt.Sprintf("Failed to determine control connection host port for client proxy PhysicalContainer %q: %v", containerName.String(), controlPortErr))
		return false, NoDelay
	}
	dataPort, dataPortErr := physicalContainerHostPort(physicalContainer.Status.PortMappings, int32(dcptun.DefaultContainerProxyDataPort))
	if dataPortErr != nil {
		log.Error(dataPortErr, "Failed to determine data connection host port for the client proxy PhysicalContainer")
		pd.attemptProxyRecovery(fmt.Sprintf("Failed to determine data connection host port for client proxy PhysicalContainer %q: %v", containerName.String(), dataPortErr))
		return false, NoDelay
	}

	pd.ClientProxyContainerID = physicalContainer.Status.ContainerID
	pd.ClientProxyControlPort = controlPort
	pd.ClientProxyDataPort = dataPort
	pd.Message = ""
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
	readyCondition := apimeta.FindStatusCondition(conditions, string(apiv2.ConditionReady))
	if readyCondition != nil && readyCondition.Message != "" {
		return readyCondition.Message
	}
	return "the physical resource did not report an error message"
}

func physicalResourceReadyConditionReason(conditions []metav1.Condition) apiv2.ConditionReason {
	readyCondition := apimeta.FindStatusCondition(conditions, string(apiv2.ConditionReady))
	if readyCondition == nil {
		return ""
	}
	return apiv2.ConditionReason(readyCondition.Reason)
}

// Starts the server proxy as an OS process.
// Assumes that the client proxy container has been started and data about it has already been applied
// to the passed containerNetworkTunnelProxyData instance.
// Updates the provided proxy data with process ID, startup timestamp, stdout/stderr capture files, and server control port.
// Returns the server proxy run, if a process was started, and whether startup completed successfully.
func (r *ContainerNetworkTunnelProxyReconciler) startServerProxy(
	startupCtx context.Context,
	cleanupCtx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) (*serverProxyRun, bool) {
	if startupCtx.Err() != nil {
		return nil, false
	}
	dcpExePath, dcpExePathErr := dcppaths.GetDcpExePath()
	if dcpExePathErr != nil {
		log.Error(dcpExePathErr, "Failed to get DCP executable path")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		pd.Message = fmt.Sprintf("Failed to get DCP executable path: %v", dcpExePathErr)
		return nil, false
	}

	startCompleted := false
	var stdoutFile *os.File
	var stderrFile *os.File
	var serverHandle process.ProcessHandle
	defer func() {
		if startCompleted {
			return
		}
		cleanupErr := closeAndRemoveServerProxyOutputFiles(stdoutFile, stderrFile)
		if cleanupErr != nil {
			tunnelProxyCleanupUnconfirmedCounter.Add(context.Background(), 1)
			log.WithValues(
				"OldProxyUID", tunnelProxy.UID,
				"PhysicalContainer", tunnelProxyPhysicalContainerNameForUID(tunnelProxy.UID, pd.pairGeneration).String(),
				"ServerPID", serverHandle.Pid,
				"ServerIdentityTime", process.FormatIdentityTime(serverHandle.IdentityTime),
			).Error(
				cleanupErr,
				"Failed to remove output files from unsuccessful tunnel server proxy startup",
				"CleanupStage", tunnelProxyCleanupStageRemoveOutput,
				"CleanupConfirmed", false,
			)
		}
	}()

	var stdoutErr error
	stdoutFile, stdoutErr = usvc_io.CreateNewTempFile(fmt.Sprintf("%s_out_%s_%d", tunnelProxy.Name, tunnelProxy.UID, pd.pairGeneration), osutil.PermissionOnlyOwnerReadWrite)
	if stdoutErr != nil {
		log.Error(stdoutErr, "Failed to create stdout temp file for container tunnel server proxy")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		pd.Message = fmt.Sprintf("Failed to create stdout temp file for container tunnel server proxy: %v", stdoutErr)
		return nil, false
	}

	var stderrErr error
	stderrFile, stderrErr = usvc_io.CreateNewTempFile(fmt.Sprintf("%s_err_%s_%d", tunnelProxy.Name, tunnelProxy.UID, pd.pairGeneration), osutil.PermissionOnlyOwnerReadWrite)
	if stderrErr != nil {
		log.Error(stderrErr, "Failed to create stderr temp file for container tunnel server proxy")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		pd.Message = fmt.Sprintf("Failed to create stderr temp file for container tunnel server proxy: %v", stderrErr)
		return nil, false
	}

	if pd.securityConfig == nil {
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		pd.Message = "Cannot start server proxy: control security configuration is missing"
		return nil, false
	}
	serverSecurityJSON, serverSecurityErr := json.Marshal(pd.securityConfig.ServerProxyConfig())
	if serverSecurityErr != nil {
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		pd.Message = fmt.Sprintf("Failed to encode server proxy control security configuration: %v", serverSecurityErr)
		return nil, false
	}

	args := []string{
		"tunnel-server",
		networking.IPv4LocalhostDefaultAddress, // Client control address--as exposed by container orchestrator
		strconv.Itoa(int(pd.ClientProxyControlPort)),
		networking.IPv4LocalhostDefaultAddress, // Client data address--as exposed by container orchestrator
		strconv.Itoa(int(pd.ClientProxyDataPort)),
		"--server-control-address",
		networking.IPv4LocalhostDefaultAddress,
		"--control-security-stdin",
	}

	cmd := exec.Command(dcpExePath, args...)
	// Stdin keeps server credentials out of process arguments and avoids a persistent host credential file.
	cmd.Stdin = bytes.NewReader(serverSecurityJSON)
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile
	cmd.Env = os.Environ()
	logger.WithSessionId(cmd)
	proxyName := tunnelProxy.NamespacedName()
	run := newServerProxyRun()
	run.resourceUID = tunnelProxy.UID
	run.pairGeneration = pd.pairGeneration
	exitHandler := process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
		r.onServerProcessExit(proxyName, run, pid, exitCode, err, stdoutFile, stderrFile)
	})

	// StartProcess establishes kill-on-dispose ownership before returning. On Windows it
	// fails closed and reaps the suspended child if cleanup-job ownership cannot be established.
	handle, startWaitForExit, startErr := r.config.ProcessExecutor.StartProcess(
		context.Background(),
		cmd,
		exitHandler,
		process.CreationFlagEnsureKillOnDispose,
		nil,
	)
	if startErr != nil {
		log.Error(startErr, "Failed to start server proxy process")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		pd.Message = fmt.Sprintf("Failed to start server proxy process: %v", startErr)
		return nil, false
	}
	// Record the process identity before enabling its exit callback.
	serverHandle = handle
	run.handle = handle
	pointers.SetValue(&pd.ServerProxyProcessID, int64(handle.Pid))
	pd.ServerProxyStartupTimestamp = metav1.NewMicroTime(handle.IdentityTime)
	startWaitForExit()
	dcpproc.RunProcessWatcher(r.config.ProcessExecutor, handle, log)

	cleanupFailedStart := func(failure error, cleanupStage string) {
		cleanupLog := log.WithValues(
			"OldProxyUID", tunnelProxy.UID,
			"PhysicalContainer", tunnelProxyPhysicalContainerNameForUID(tunnelProxy.UID, pd.pairGeneration).String(),
			"ServerPID", handle.Pid,
			"ServerIdentityTime", process.FormatIdentityTime(handle.IdentityTime),
		)
		run.expectExit()
		stopCtx, stopCancel := process.WithStopTimeout(cleanupCtx)
		stopProcessErr := r.stopServerProxyProcess(stopCtx, pd)
		stopCancel()
		stderrExcerpt := readBoundedServerProxyStderr(stderrFile.Name())
		run.requestOutputRemoval()
		if stderrExcerpt != "" {
			cleanupLog.Error(
				failure,
				"Tunnel server proxy startup failed",
				"CleanupStage", cleanupStage,
				"CleanupConfirmed", stopProcessErr == nil,
				"ServerStderr", stderrExcerpt,
			)
		} else {
			cleanupLog.Error(
				failure,
				"Tunnel server proxy startup failed",
				"CleanupStage", cleanupStage,
				"CleanupConfirmed", stopProcessErr == nil,
			)
		}
		if stopProcessErr != nil {
			tunnelProxyProcessStopFailureCounter.Add(context.Background(), 1)
			tunnelProxyCleanupUnconfirmedCounter.Add(context.Background(), 1)
			cleanupLog.Error(
				stopProcessErr,
				"Failed to stop server proxy process after startup failure",
				"CleanupStage", tunnelProxyCleanupStageServerProcessStop,
				"CleanupConfirmed", false,
			)
		}
	}

	// Wait until the first JSON line is printed to stdout indicating server control address/port

	tc, tcErr := r.readServerProxyConfigOrExit(startupCtx, run, stdoutFile.Name())
	if tcErr != nil {
		startupInterrupted := startupCtx.Err() != nil || commonapi.ResourceCreationProhibited.Load()
		if !startupInterrupted {
			if !run.exit.IsSet() && !errors.Is(tcErr, errMalformedServerProxyConfig) && errors.Is(tcErr, context.DeadlineExceeded) {
				pd.recovery.neverReadyGenerations++
				generationProgress := fmt.Sprintf("generation %d of %d", pd.recovery.neverReadyGenerations, r.config.MaxNeverReadyGenerations)
				pd.Message = fmt.Sprintf("Timed out reading server proxy configuration (%s): %v", generationProgress, tcErr)
				if pd.recovery.neverReadyGenerations >= r.config.MaxNeverReadyGenerations {
					pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
				} else {
					pd.State = apiv1.ContainerNetworkTunnelProxyStateRecoveryPending
				}
			} else {
				pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
				pd.Message = fmt.Sprintf("Failed to read server proxy configuration: %v", tcErr)
			}
		}
		cleanupFailedStart(tcErr, tunnelProxyCleanupStageReadServerConfiguration)
		return run, false
	}

	readyErr := r.waitServerProxyReadyOrExit(startupCtx, run, stdoutFile.Name())
	if readyErr != nil {
		startupFailure := readyErr
		startupInterrupted := startupCtx.Err() != nil || commonapi.ResourceCreationProhibited.Load()
		if !startupInterrupted {
			pd.recovery.neverReadyGenerations++
			generationProgress := fmt.Sprintf("generation %d of %d", pd.recovery.neverReadyGenerations, r.config.MaxNeverReadyGenerations)
			if exit, exited := run.getExit(); exited && exit.exitType == serverProxyExitTypeUnexpected {
				startupFailure = errors.Join(
					fmt.Errorf("server proxy process exited with exit code %d before establishing its initial control stream", exit.exitCode),
					exit.err,
				)
				pd.Message = fmt.Sprintf(
					"Server proxy process '%d' exited with exit code %d before establishing its initial control stream (%s)",
					handle.Pid,
					exit.exitCode,
					generationProgress,
				)
				if exit.err != nil {
					pd.Message = fmt.Sprintf("%s: %v", pd.Message, exit.err)
				}
			} else {
				pd.Message = fmt.Sprintf("Server proxy did not establish its initial control stream (%s): %v", generationProgress, readyErr)
			}
			if pd.recovery.neverReadyGenerations >= r.config.MaxNeverReadyGenerations {
				pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
			} else {
				pd.State = apiv1.ContainerNetworkTunnelProxyStateRecoveryPending
			}
		}
		cleanupFailedStart(startupFailure, tunnelProxyCleanupStageWaitControlReadiness)
		return run, false
	}

	pd.recovery.neverReadyGenerations = 0
	pd.controlReadyAt = time.Now()
	pd.ServerProxyControlPort = tc.ServerControlPort
	pd.ServerProxyStdOutFile = stdoutFile.Name()
	pd.ServerProxyStdErrFile = stderrFile.Name()
	pd.serverStdout = stdoutFile
	pd.serverStderr = stderrFile
	startCompleted = true

	return run, true
}

// Waits until the server proxy reports its first control stream, its process exits, or ControlStartupTimeout elapses.
func (r *ContainerNetworkTunnelProxyReconciler) waitServerProxyReadyOrExit(ctx context.Context, run *serverProxyRun, stdoutPath string) error {
	readyCtx, readyCancel := context.WithTimeout(ctx, r.config.ControlStartupTimeout)
	exitWatcherDone := make(chan struct{})
	go func() {
		defer close(exitWatcherDone)
		select {
		case <-run.exit.Wait():
			readyCancel()
		case <-readyCtx.Done():
		}
	}()

	readyErr := waitServerProxyReady(readyCtx, stdoutPath)
	readyCancel()
	<-exitWatcherDone
	if readyErr != nil && ctx.Err() == nil && run.exit.IsSet() {
		if ready, finalErr := readServerProxyReadyOnce(stdoutPath); finalErr == nil && ready {
			return nil
		}
	}
	return readyErr
}

var errMalformedServerProxyConfig = errors.New("malformed server proxy configuration")

func (r *ContainerNetworkTunnelProxyReconciler) readServerProxyConfigOrExit(
	ctx context.Context,
	run *serverProxyRun,
	path string,
) (dcptun.TunnelProxyConfig, error) {
	configCtx, configCtxCancel := context.WithTimeout(ctx, r.config.ServerConfigReadTimeout)
	exitWatcherDone := make(chan struct{})
	go func() {
		defer close(exitWatcherDone)
		select {
		case <-run.exit.Wait():
			configCtxCancel()
		case <-configCtx.Done():
		}
	}()

	config, readErr := resiliency.RetryGet(configCtx, backoff.NewConstantBackOff(200*time.Millisecond), func() (dcptun.TunnelProxyConfig, error) {
		if commonapi.ResourceCreationProhibited.Load() {
			return dcptun.TunnelProxyConfig{}, backoff.Permanent(context.Canceled)
		}
		return readServerProxyConfigOnce(path)
	})
	configCtxCancel()
	<-exitWatcherDone

	if readErr != nil && ctx.Err() == nil && !commonapi.ResourceCreationProhibited.Load() && run.exit.IsSet() {
		finalConfig, finalErr := readServerProxyConfigOnce(path)
		if finalErr == nil {
			return finalConfig, nil
		}
		if exit, exited := run.getExit(); exited {
			return dcptun.TunnelProxyConfig{}, errors.Join(
				fmt.Errorf("server proxy process exited with exit code %d before writing configuration", exit.exitCode),
				exit.err,
				finalErr,
			)
		}
		return dcptun.TunnelProxyConfig{}, finalErr
	}
	return config, readErr
}

func readServerProxyConfigOnce(path string) (dcptun.TunnelProxyConfig, error) {
	f, fErr := usvc_io.OpenFileReadOnly(path)
	if fErr != nil {
		return dcptun.TunnelProxyConfig{}, fErr
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReader(f)
	line, readErr := reader.ReadBytes('\n')
	if readErr != nil {
		// Treat io.EOF (empty file or incomplete first line without newline) as transient until completed.
		return dcptun.TunnelProxyConfig{}, readErr
	}
	var config dcptun.TunnelProxyConfig
	umErr := json.Unmarshal(bytes.TrimSpace(line), &config)
	if umErr != nil {
		return dcptun.TunnelProxyConfig{}, backoff.Permanent(fmt.Errorf("%w: %w", errMalformedServerProxyConfig, umErr))
	}
	if !networking.IsValidPort(int(config.ServerControlPort)) {
		return dcptun.TunnelProxyConfig{}, backoff.Permanent(fmt.Errorf("%w: invalid server control port %d", errMalformedServerProxyConfig, config.ServerControlPort))
	}
	return config, nil
}

func waitServerProxyReady(ctx context.Context, path string) error {
	_, readyErr := resiliency.RetryGet(ctx, backoff.NewConstantBackOff(200*time.Millisecond), func() (bool, error) {
		if commonapi.ResourceCreationProhibited.Load() {
			return false, backoff.Permanent(context.Canceled)
		}
		return readServerProxyReadyOnce(path)
	})
	return readyErr
}

func readServerProxyReadyOnce(path string) (bool, error) {
	file, openErr := usvc_io.OpenFileReadOnly(path)
	if openErr != nil {
		return false, openErr
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var readiness dcptun.TunnelProxyReadiness
		if unmarshalErr := json.Unmarshal(scanner.Bytes(), &readiness); unmarshalErr != nil {
			continue
		}
		if readiness.ControlReady {
			return true, nil
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return false, scanErr
	}
	return false, io.EOF
}

func (r *ContainerNetworkTunnelProxyReconciler) stopServerProxyProcess(
	ctx context.Context,
	pd *containerNetworkTunnelProxyData,
) error {
	handle, found := serverProxyHandle(pd)
	if !found {
		return nil
	}

	stopErr := r.config.ProcessExecutor.StopProcess(ctx, handle)
	if stopErr != nil && !process.IsProcessGoneErr(stopErr) {
		return stopErr
	}

	pd.ServerProxyProcessID = nil
	pd.ServerProxyStartupTimestamp = metav1.MicroTime{}
	return nil
}

func serverProxyHandle(pd *containerNetworkTunnelProxyData) (process.ProcessHandle, bool) {
	if pd == nil || pd.ServerProxyProcessID == nil || *pd.ServerProxyProcessID <= 0 {
		return process.ProcessHandle{}, false
	}
	return process.NewHandle(
		process.Pid_t(*pd.ServerProxyProcessID),
		pd.ServerProxyStartupTimestamp.Time,
	), true
}

func abandonedProxyPairHasCleanupWork(pd *containerNetworkTunnelProxyData) bool {
	if pd == nil {
		return false
	}
	if _, found := serverProxyHandle(pd); found {
		return true
	}
	for _, extra := range pd.tunnelExtra {
		if len(extra.clientServiceEndpointNames) > 0 || extra.clientServiceName != "" {
			return true
		}
	}
	return pd.ClientProxyContainerImage != "" ||
		pd.ClientProxyContainerID != "" ||
		pd.ClientProxyControlPort != 0 ||
		pd.ClientProxyDataPort != 0 ||
		pd.ServerProxyControlPort != 0 ||
		pd.ServerProxyStdOutFile != "" ||
		pd.ServerProxyStdErrFile != "" ||
		pd.serverStdout != nil ||
		pd.serverStderr != nil ||
		pd.securityConfig != nil ||
		pd.clientSecurityFile != ""
}

func readBoundedServerProxyStderr(path string) string {
	if path == "" {
		return ""
	}
	file, openErr := usvc_io.OpenFileReadOnly(path)
	if openErr != nil {
		return ""
	}
	defer func() { _ = file.Close() }()

	stderrBytes, readErr := io.ReadAll(io.LimitReader(file, maxLoggedServerStderrBytes))
	if readErr != nil {
		return ""
	}
	return string(bytes.TrimSpace(stderrBytes))
}

func removeStoppedServerProxyOutputFiles(ctx context.Context, pd *containerNetworkTunnelProxyData) error {
	if pd == nil {
		return nil
	}
	closeErr := closeServerProxyLocalFiles(pd)
	if _, stillRunning := serverProxyHandle(pd); stillRunning {
		return closeErr
	}
	if pd.ServerProxyStdOutFile == "" && pd.ServerProxyStdErrFile == "" {
		return closeErr
	}
	removeErr := logs.RemoveOutputFiles(ctx, pd.ServerProxyStdOutFile, pd.ServerProxyStdErrFile)
	if removeErr == nil {
		pd.ServerProxyStdOutFile = ""
		pd.ServerProxyStdErrFile = ""
	}
	return errors.Join(closeErr, removeErr)
}

func closeServerProxyLocalFiles(pd *containerNetworkTunnelProxyData) error {
	if pd == nil {
		return nil
	}
	stdoutFile := pd.serverStdout
	stderrFile := pd.serverStderr
	pd.serverStdout = nil
	pd.serverStderr = nil
	return closeServerProxyOutputFiles(stdoutFile, stderrFile, false)
}

func closeAndRemoveServerProxyOutputFiles(stdoutFile *os.File, stderrFile *os.File) error {
	return closeServerProxyOutputFiles(stdoutFile, stderrFile, true)
}

func closeServerProxyOutputFiles(stdoutFile *os.File, stderrFile *os.File, remove bool) error {
	stdoutErr := closeServerProxyOutputFile(stdoutFile, remove)
	if stdoutErr != nil {
		stdoutErr = fmt.Errorf("clean up server proxy stdout file: %w", stdoutErr)
	}
	stderrErr := closeServerProxyOutputFile(stderrFile, remove)
	if stderrErr != nil {
		stderrErr = fmt.Errorf("clean up server proxy stderr file: %w", stderrErr)
	}
	return errors.Join(stdoutErr, stderrErr)
}

func closeServerProxyOutputFile(file *os.File, remove bool) error {
	if file == nil {
		return nil
	}
	path := file.Name()
	closeErr := file.Close()
	if errors.Is(closeErr, os.ErrClosed) {
		closeErr = nil
	}
	if !remove || osutil.EnvVarSwitchEnabled(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS) {
		return closeErr
	}
	removeErr := os.Remove(path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
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
		pairCleaned := r.cleanupProxyPair(ctx, pd, tunnelProxy.UID, log)
		_, endpointRemovalErr := r.removeAllTunnelProxyEndpoints(ctx, tunnelProxy, pd, log)
		if pairCleaned && endpointRemovalErr == nil {
			pd.State = apiv1.ContainerNetworkTunnelProxyStateDeleted
		} else {
			pd.State = apiv1.ContainerNetworkTunnelProxyStateDeletePending
		}

		log.V(1).Info("Completed cleanup attempt for ContainerNetworkTunnelProxy proxy pair")
		nn := tunnelProxy.NamespacedName()
		pdMap := r.proxyData
		pdMap.QueueDeferredOp(nn, func(_ types.NamespacedName, _ types.NamespacedName, proxy *apiv1.ContainerNetworkTunnelProxy) {
			_, current := pdMap.BorrowByNamespacedName(nn)
			if proxy.UID != pd.resourceUID || current == nil || current.State != apiv1.ContainerNetworkTunnelProxyStateDeleting {
				return
			}
			pdMap.Update(nn, nn, pd)
		})
		reconciliationDelay := NoDelay
		if pd.State != apiv1.ContainerNetworkTunnelProxyStateDeleted {
			reconciliationDelay = StandardDelay
		}
		r.ScheduleReconciliationWithDelay(nn, reconciliationDelay)
	}
}

// Deletes the resources associated with the proxy pair (client container and server process)
// and updates the provided proxy data accordingly.
func (r *ContainerNetworkTunnelProxyReconciler) cleanupProxyPair(
	ctx context.Context,
	pd *containerNetworkTunnelProxyData,
	proxyObjectID types.UID,
	log logr.Logger,
) bool {
	cleanupCompleted := true
	cleanupUnconfirmed := false
	log.V(1).Info("Removing client proxy physical resources...")
	physicalResourcesRemoved, removeErr := r.cleanupClientPhysicalResources(ctx, proxyObjectID, pd)
	if removeErr != nil {
		log.Error(removeErr, "Failed to remove client proxy physical resources")
		tunnelProxyDeleteSubmissionFailureCounter.Add(context.Background(), 1)
		cleanupUnconfirmed = true
		cleanupCompleted = false
	} else if !physicalResourcesRemoved {
		log.V(1).Info("Client proxy physical resource deletion is still in progress")
		cleanupCompleted = false
	} else {
		log.V(1).Info("Successfully removed client proxy physical resources")
		pd.ClientProxyContainerID = ""
		securityFileErr := r.removeClientProxySecurityFile(pd)
		if securityFileErr != nil {
			log.Error(securityFileErr, "Failed to remove client proxy control security file")
			cleanupUnconfirmed = true
			cleanupCompleted = false
		}
	}

	if pd.ServerProxyProcessID != nil && *pd.ServerProxyProcessID > 0 {
		log.V(1).Info("Stopping server proxy process...")

		// The process may have already exited because the client container has been stopped.

		stopErr := r.stopServerProxyProcess(ctx, pd)
		if stopErr != nil {
			log.Error(stopErr, "Failed to stop server proxy process")
			tunnelProxyProcessStopFailureCounter.Add(context.Background(), 1)
			cleanupUnconfirmed = true
			cleanupCompleted = false
		} else {
			log.V(1).Info("Successfully stopped server proxy process")
		}
	}

	if outputCleanupErr := removeStoppedServerProxyOutputFiles(ctx, pd); outputCleanupErr != nil {
		log.Error(outputCleanupErr, "Failed to clean up server proxy output files")
		cleanupUnconfirmed = true
		cleanupCompleted = false
	}

	if cleanupUnconfirmed {
		tunnelProxyCleanupUnconfirmedCounter.Add(context.Background(), 1)
	}
	return cleanupCompleted
}

func (r *ContainerNetworkTunnelProxyReconciler) removeClientProxySecurityFile(pd *containerNetworkTunnelProxyData) error {
	if pd == nil || pd.clientSecurityFile == "" {
		return nil
	}
	removeErr := r.clientSecurityFiles.Remove(pd.clientSecurityFile)
	if removeErr != nil {
		return fmt.Errorf("remove client proxy control security file: %w", removeErr)
	}
	pd.clientSecurityFile = ""
	return nil
}

func (r *ContainerNetworkTunnelProxyReconciler) cleanupClientPhysicalResources(
	ctx context.Context,
	proxyObjectID types.UID,
	pd *containerNetworkTunnelProxyData,
) (bool, error) {
	containerName := tunnelProxyPhysicalContainerNameForUID(proxyObjectID, pd.pairGeneration)
	physicalContainer := &apiv2.PhysicalContainer{}
	getErr := r.NoCacheClient.Get(ctx, containerName, physicalContainer)
	if apimachinery_errors.IsNotFound(getErr) {
		return true, nil
	}
	if getErr != nil {
		return false, fmt.Errorf("get PhysicalContainer %q for cleanup: %w", containerName.String(), getErr)
	}
	if physicalContainer.Annotations[tunnelProxyOwnerUIDAnnotation] != string(proxyObjectID) {
		return true, nil // The expected owner does not own the resource occupying this name.
	}
	if pd.clientPhysicalUID != "" && physicalContainer.UID != pd.clientPhysicalUID {
		return true, nil // The owned container is gone; a different resource now occupies its name.
	}
	deleteErr := r.Client.Delete(ctx, physicalContainer, ctrl_client.Preconditions{UID: &physicalContainer.UID})
	if apimachinery_errors.IsNotFound(deleteErr) {
		return true, nil
	}
	if deleteErr != nil {
		return false, fmt.Errorf("delete PhysicalContainer %q: %w", containerName.String(), deleteErr)
	}

	return false, nil
}

func (r *ContainerNetworkTunnelProxyReconciler) onServerProcessExit(
	pName types.NamespacedName,
	run *serverProxyRun,
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

	outputCleanupErr := closeServerProxyOutputFiles(
		stdoutFile,
		stderrFile,
		run.removeOutputOnClose.Load(),
	)
	if outputCleanupErr != nil {
		r.Log.Error(outputCleanupErr, "Failed to clean up output files for tunnel server proxy process", "PID", pid)
	}

	if !run.recordUnexpectedExit(exitCode, err) {
		return
	}

	pdMap := r.proxyData
	pdMap.QueueDeferredOp(pName, func(_ types.NamespacedName, _ types.NamespacedName, proxy *apiv1.ContainerNetworkTunnelProxy) {
		_, pd := pdMap.BorrowByNamespacedName(pName)
		if pd == nil || proxy.UID != run.resourceUID || pd.resourceUID != run.resourceUID {
			return // ContainerNetworkTunnelProxy object has been deleted, nothing to do
		}
		if pd.pairGeneration != run.pairGeneration || !pd.hasServerProxy(run.handle) {
			return // This callback belongs to a server proxy process that is no longer current.
		}
		switch pd.State {
		case apiv1.ContainerNetworkTunnelProxyStateStarting, apiv1.ContainerNetworkTunnelProxyStateStarted, apiv1.ContainerNetworkTunnelProxyStateRunning:
		default:
			return // Cleanup or another failure already owns the proxy's terminal status.
		}

		// Unexpected exits require cleanup even when the process reports a successful exit code.
		applyUnexpectedServerProxyExit(pd, run.handle.Pid, serverProxyExit{
			exitType: serverProxyExitTypeUnexpected,
			exitCode: exitCode,
			err:      err,
		})
		pdMap.Update(pName, pName, pd)
	})
	r.ScheduleReconciliation(pName)
}

func applyUnexpectedServerProxyExit(pd *containerNetworkTunnelProxyData, pid process.Pid_t, exit serverProxyExit) {
	pd.ServerProxyProcessID = nil
	pd.ServerProxyStartupTimestamp = metav1.MicroTime{}
	message := fmt.Sprintf("Server proxy process '%d' exited unexpectedly with exit code %d", pid, exit.exitCode)
	if exit.err != nil {
		message = fmt.Sprintf(
			"Server proxy process '%d' exited unexpectedly with exit code %d: %v",
			pid,
			exit.exitCode,
			exit.err,
		)
	}
	pd.attemptProxyRecovery(message)
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
) ([]*apiv1.Endpoint, objectChange, error) {
	tunnelProxy := owner.(*apiv1.ContainerNetworkTunnelProxy)
	csName := serviceProducer.ServiceNamespacedName()
	csTunnels := pd.tunnelsForClientService(tunnelProxy.Spec.Tunnels, csName)
	if len(csTunnels) == 0 {
		// May be because we did not get to the point of creating the corresponding tunnel yet.
		log.V(1).Info("There are no tunnels that support given client service", "ClientService", csName.String())
		return nil, noChange, nil
	}

	readyTunnels := slices.Select(csTunnels, func(t apiv1.TunnelStatus) bool {
		return t.State == apiv1.TunnelStateReady
	})
	if len(readyTunnels) == 0 {
		log.V(1).Info("There are no tunnels in Ready state that support given client service", "ClientService", csName.String())
		return nil, noChange, nil
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
				return nil, noChange, nameErr
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
			te.clientServiceName = csName.Name
			endpointNN := types.NamespacedName{Name: endpointName, Namespace: tunnelProxy.Namespace}
			if !slices.Contains(te.clientServiceEndpointNames, endpointNN) {
				te.clientServiceEndpointNames = append(te.clientServiceEndpointNames, endpointNN)
			}
			pd.tunnelExtra[t.Name] = te
			r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
		}

	}
	return retval, noChange, nil
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
