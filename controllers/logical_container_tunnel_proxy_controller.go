/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	stdproto "google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcpproc"
	"github.com/microsoft/dcp/internal/dcptun"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

const (
	logicalTunnelProxyOwnerUIDField = ".metadata.logicalTunnelProxyOwnerUID"
	logicalTunnelProxyNetworkField  = ".spec.logicalTunnelProxyNetwork"
	logicalTunnelProxyServiceField  = ".spec.logicalTunnelProxyServices"
	logicalTunnelProxyFinalizer     = "usvc-dev.developer.microsoft.com/logical-tunnel-proxy-reconciler"
)

type logicalTunnelProxyHandler = stateInitializerFunc[
	apiv2.LogicalContainerTunnelProxy, *apiv2.LogicalContainerTunnelProxy,
	LogicalContainerTunnelProxyReconciler, *LogicalContainerTunnelProxyReconciler,
	logicalTunnelProxyState, logicalTunnelProxyData, *logicalTunnelProxyData,
]

var logicalTunnelProxyHandlers = map[logicalTunnelProxyState]logicalTunnelProxyHandler{
	logicalTunnelProxyPending:  handleLogicalTunnelProxyPending,
	logicalTunnelProxyStarting: handleLogicalTunnelProxyStarting,
	logicalTunnelProxyRunning:  handleLogicalTunnelProxyRunning,
	logicalTunnelProxyStopping: handleLogicalTunnelProxyStopping,
	logicalTunnelProxyExited:   handleLogicalTunnelProxyTerminal,
	logicalTunnelProxyFailed:   handleLogicalTunnelProxyTerminal,
	logicalTunnelProxyUnknown:  handleLogicalTunnelProxyUnknown,
	"":                         handleLogicalTunnelProxyUnknown,
}

type LogicalContainerTunnelProxyReconcilerConfig struct {
	// Launches and stops the host tunnel server process. Mandatory.
	ProcessExecutor              process.Executor
	BuildContextDir              string
	MakeTunnelControlClient      func(grpc.ClientConnInterface) dcptunproto.TunnelControlClient
	MaxTunnelPreparationAttempts uint32
	// Bounds the wait for the server's configuration line; defaults to defaultServerProxyConfigReadTimeout.
	ServerConfigReadTimeout time.Duration
	// Bounds the wait for the server's control readiness line; defaults to defaultControlStartupTimeout.
	ControlStartupTimeout time.Duration
}

type LogicalContainerTunnelProxyReconciler struct {
	*ReconcilerBase[apiv2.LogicalContainerTunnelProxy, *apiv2.LogicalContainerTunnelProxy]
	config         LogicalContainerTunnelProxyReconcilerConfig
	proxies        *ObjectStateMap[types.UID, logicalTunnelProxyData, *logicalTunnelProxyData, *apiv2.LogicalContainerTunnelProxy]
	securityFiles  *usvc_io.FileRegistry
	operationQueue *resiliency.WorkQueue
}

func NewLogicalContainerTunnelProxyReconciler(
	ctx context.Context, client ctrl_client.Client, reader ctrl_client.Reader, log logr.Logger,
	config LogicalContainerTunnelProxyReconcilerConfig,
) *LogicalContainerTunnelProxyReconciler {
	if config.BuildContextDir == "" {
		config.BuildContextDir = usvc_io.DcpTempDir()
	}
	if config.MakeTunnelControlClient == nil {
		config.MakeTunnelControlClient = dcptunproto.NewTunnelControlClient
	}
	if config.ProcessExecutor == nil {
		panic("LogicalContainerTunnelProxyReconcilerConfig.ProcessExecutor must not be nil")
	}
	if config.MaxTunnelPreparationAttempts == 0 {
		config.MaxTunnelPreparationAttempts = defaultMaxTunnelPreparationAttempts
	}
	if config.ServerConfigReadTimeout <= 0 {
		config.ServerConfigReadTimeout = defaultServerProxyConfigReadTimeout
	}
	if config.ControlStartupTimeout <= 0 {
		config.ControlStartupTimeout = defaultControlStartupTimeout
	}
	return &LogicalContainerTunnelProxyReconciler{
		ReconcilerBase: NewReconcilerBase[apiv2.LogicalContainerTunnelProxy](client, reader, log, ctx),
		config:         config, proxies: NewObjectStateMap[types.UID, logicalTunnelProxyData, *logicalTunnelProxyData, *apiv2.LogicalContainerTunnelProxy](),
		securityFiles:  usvc_io.NewFileRegistry(config.BuildContextDir),
		operationQueue: resiliency.NewWorkQueue(ctx, MaxConcurrentReconciles),
	}
}

func (r *LogicalContainerTunnelProxyReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	if shutdownErr := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		if cleanupErr := r.securityFiles.Cleanup(); cleanupErr != nil {
			r.Log.Error(cleanupErr, "Failed to remove logical tunnel control security files during shutdown")
			return cleanupErr
		}
		return nil
	})); shutdownErr != nil {
		return fmt.Errorf("add logical tunnel security cleanup: %w", shutdownErr)
	}
	for _, child := range []ctrl_client.Object{&apiv2.PhysicalContainer{}, &apiv2.PhysicalContainerImage{}, &apiv2.ServiceEndpoint{}} {
		indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, child, logicalTunnelProxyOwnerUIDField, func(object ctrl_client.Object) []string {
			owner := metav1.GetControllerOf(object)
			if owner == nil {
				return nil
			}
			return []string{string(owner.UID)}
		})
		if indexErr != nil {
			return fmt.Errorf("index tunnel proxy child owner: %w", indexErr)
		}
	}
	for field, extractor := range map[string]ctrl_client.IndexerFunc{
		logicalTunnelProxyNetworkField: func(object ctrl_client.Object) []string {
			proxy := object.(*apiv2.LogicalContainerTunnelProxy)
			return []string{commonapi.AsNamespacedName(proxy.Spec.NetworkRef, proxy.Namespace).String()}
		},
		logicalTunnelProxyServiceField: func(object ctrl_client.Object) []string {
			proxy := object.(*apiv2.LogicalContainerTunnelProxy)
			var references []string
			for _, tunnel := range proxy.Spec.Tunnels {
				references = append(references, commonapi.AsNamespacedName(tunnel.ServerServiceRef, proxy.Namespace).String(),
					commonapi.AsNamespacedName(tunnel.ClientServiceRef, proxy.Namespace).String())
			}
			return references
		},
	} {
		indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, &apiv2.LogicalContainerTunnelProxy{}, field, extractor)
		if indexErr != nil {
			return fmt.Errorf("index tunnel proxy reference: %w", indexErr)
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.LogicalContainerTunnelProxy{}).
		Owns(&apiv2.PhysicalContainer{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Owns(&apiv2.PhysicalContainerImage{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Owns(&apiv2.ServiceEndpoint{}).
		Watches(&apiv2.LogicalContainerNetwork{}, handler.EnqueueRequestsFromMapFunc(r.mapTunnelProxyDependency(logicalTunnelProxyNetworkField))).
		Watches(&apiv2.LogicalService{}, handler.EnqueueRequestsFromMapFunc(r.mapTunnelProxyDependency(logicalTunnelProxyServiceField))).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.LogicalContainerTunnelProxyList{}))).
		WatchesRawSource(r.GetReconciliationEventSource()).Named(name).Complete(r)
}

func (r *LogicalContainerTunnelProxyReconciler) mapTunnelProxyDependency(field string) handler.MapFunc {
	return func(ctx context.Context, dependency ctrl_client.Object) []reconcile.Request {
		var proxies apiv2.LogicalContainerTunnelProxyList
		listErr := r.List(ctx, &proxies, ctrl_client.InNamespace(dependency.GetNamespace()),
			ctrl_client.MatchingFields{field: ctrl_client.ObjectKeyFromObject(dependency).String()})
		if listErr != nil {
			r.Log.Error(listErr, "Failed to list tunnel proxies referencing dependency")
			return nil
		}
		var requests []reconcile.Request
		for _, proxy := range proxies.Items {
			requests = append(requests, reconcile.Request{NamespacedName: proxy.NamespacedName()})
		}
		return requests
	}
}

func (r *LogicalContainerTunnelProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)
	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	var proxy apiv2.LogicalContainerTunnelProxy
	getErr := reader.Get(ctx, req.NamespacedName, &proxy)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			// Applying pending server results first lets discard release any process they hand over.
			r.proxies.RunDeferredOps(req.NamespacedName, &proxy)
			discardErr := r.discardLogicalTunnelProxyData(req.NamespacedName)
			if discardErr != nil {
				log.Error(discardErr, "Failed to discard logical tunnel proxy state")
				return ctrl.Result{}, discardErr
			}
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to Get() the LogicalContainerTunnelProxy")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}

	getSucceededCounter.Add(ctx, 1)
	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, proxy.GetResourceId())
	r.proxies.RunDeferredOps(req.NamespacedName, &proxy)

	stateKey, previous := r.proxies.BorrowByNamespacedName(req.NamespacedName)
	if previous != nil && stateKey != proxy.UID {
		discardErr := r.discardLogicalTunnelProxyData(req.NamespacedName)
		if discardErr != nil {
			log.Error(discardErr, "Failed to discard previous logical tunnel proxy state")
			return ctrl.Result{}, discardErr
		}
	}

	_, data := r.proxies.BorrowByNamespacedName(req.NamespacedName)
	patch := ctrl_client.MergeFromWithOptions(proxy.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	var change objectChange
	delay := StandardDelay
	if !proxy.DeletionTimestamp.IsZero() {
		change, delay = r.handleLogicalTunnelProxyDeletion(ctx, &proxy, data, log)
	} else {
		change = ensureFinalizer(&proxy, logicalTunnelProxyFinalizer, log)
		if change == noChange {
			change, delay = r.manageLogicalTunnelProxy(ctx, &proxy, data, log)
		}
	}

	return r.SaveChangesWithDelay(ctx, &proxy, patch, change, delay, nil, log)
}

func (r *LogicalContainerTunnelProxyReconciler) manageLogicalTunnelProxy(
	ctx context.Context, proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData, log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	if data == nil {
		ready, namespaceReason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, proxy.Namespace)
		if !ready {
			change := noChange
			phase, reason := apiv2.LogicalContainerTunnelProxyPhasePending, apiv2.LogicalContainerTunnelProxyReasonStarting
			message := namespaceReadinessMessage(proxy.Namespace, namespaceReason)
			if namespaceErr != nil {
				log.Error(namespaceErr, "Failed to get namespace", "Namespace", proxy.Namespace)
				phase, reason = apiv2.LogicalContainerTunnelProxyPhaseUnknown, apiv2.LogicalContainerTunnelProxyReasonLookupFailed
				message = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
				change |= additionalReconciliationNeeded
			}
			change |= setValue(&proxy.Status.Phase, phase)
			change |= setCondition(&proxy.Status.Conditions, apiv2.ConditionReady, proxy.Generation, metav1.ConditionFalse, reason, message)
			return change, StandardDelay
		}
		data = &logicalTunnelProxyData{state: logicalTunnelProxyPending, tunnels: make(map[string]logicalTunnelRecord)}
		r.proxies.Store(proxy.NamespacedName(), proxy.UID, data.Clone())
	}
	stateHandler := getStateInitializer(logicalTunnelProxyHandlers, data.state, log)
	change := stateHandler(ctx, r, proxy, data.state, data, log)
	_ = r.proxies.Update(proxy.NamespacedName(), proxy.UID, data)
	projectionChange, delay, valid := data.applyTo(proxy)
	if !valid {
		log.Error(fmt.Errorf("invalid logical tunnel proxy state %v", data.state), "LogicalContainerTunnelProxy reached invalid reconciliation state")
	}
	return change | projectionChange, delay
}

func (r *LogicalContainerTunnelProxyReconciler) discardLogicalTunnelProxyData(name types.NamespacedName) error {
	uid, data := r.proxies.BorrowByNamespacedName(name)
	if data == nil {
		return nil
	}
	if cleanupErr := removeLogicalTunnelProxyBuildContext(data); cleanupErr != nil {
		return cleanupErr
	}
	if removeErr := r.securityFiles.Remove(data.clientSecurityFile); removeErr != nil {
		return removeErr
	}
	r.proxies.DeleteByNamespacedName(name)
	r.abandonLogicalTunnelServer(data.server, r.Log.WithValues("LogicalContainerTunnelProxy", name))
	logger.ReleaseResourceLog((&apiv2.LogicalContainerTunnelProxy{ObjectMeta: metav1.ObjectMeta{UID: uid}}).GetResourceId())
	return nil
}

func handleLogicalTunnelProxyPending(
	ctx context.Context, r *LogicalContainerTunnelProxyReconciler, proxy *apiv2.LogicalContainerTunnelProxy,
	_ logicalTunnelProxyState, data *logicalTunnelProxyData, log logr.Logger,
) objectChange {
	data.reason, data.message = "", ""
	image, change := r.ensurePhysicalContainerImage(ctx, proxy, data, log)
	if image == nil {
		return change
	}
	container, ensureChange := r.ensurePhysicalContainer(ctx, proxy, data, image.GetName(), log)
	change |= ensureChange
	if container == nil {
		return change
	}
	if !r.ensureLogicalTunnelServer(proxy, data, container, log) {
		return change | additionalReconciliationNeeded
	}
	data.state, data.reason, data.message = logicalTunnelProxyStarting, "", ""
	return change
}

// ensurePhysicalContainerImage finds or declares the owned image and returns it once its identity is known.
func (r *LogicalContainerTunnelProxyReconciler) ensurePhysicalContainerImage(
	ctx context.Context, proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData, log logr.Logger,
) (*apiv2.PhysicalContainerImage, objectChange) {
	image, imageErr := r.findPhysicalContainerImage(ctx, proxy, data)
	if imageErr != nil {
		log.Error(imageErr, "Failed to get pending tunnel image")
		return nil, additionalReconciliationNeeded
	}
	change := applyLogicalTunnelProxyImageStatus(proxy, image)
	if image == nil && (data.imageUID != "" || data.containerUID != "") {
		data.message = "Waiting to observe the owned PhysicalContainerImage."
		return nil, change | additionalReconciliationNeeded
	}
	if image == nil {
		baseImage := proxy.Spec.BaseImage
		if baseImage == "" {
			baseImage = dcptun.DefaultBaseImage
		}
		if data.imageBuildPlan == nil {
			plan, planErr := dcptun.PrepareClientProxyImageBuild(ctx, r.config.BuildContextDir, baseImage)
			if planErr != nil {
				log.Error(planErr, "Failed to prepare tunnel image build context")
				return nil, change | additionalReconciliationNeeded
			}
			data.imageBuildPlan = &plan
		}
		plan := data.imageBuildPlan
		desiredImage := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(proxy.UID), Namespace: proxy.Namespace},
			Spec: apiv2.PhysicalContainerImageSpec{RemoveBuildContextArchive: true, Image: &apiv2.PhysicalContainerImageConfig{
				Image: plan.Image, PullPolicy: apiv2.PullPolicyBestEffort, BuildPolicy: apiv2.BuildPolicyIfNeeded,
				Build: &apiv2.ContainerBuildContext{Digest: plan.BuildContextDigest, Dockerfile: plan.Dockerfile, BaseImages: []string{baseImage},
					ContextArchive: &apiv2.ContainerBuildContextArchive{Source: plan.BuildContextArchive.Source, SHA256: plan.BuildContextArchive.SHA256}},
			}},
		}
		createdImage, createErr := createLogicalBacking(ctx, r.Client, r.NoCacheClient, proxy, desiredImage, physicalContainerImageFinalizer,
			&data.imageUID, &data.unobservedChildren, func(existing *apiv2.PhysicalContainerImage) error {
				expected := desiredImage.Spec.DeepCopy()
				if existing.Spec.Image != nil && existing.Spec.Image.Build != nil {
					// The generated archive's path and tar timestamps are transport details, not build identity.
					expected.Image.Build.ContextArchive = existing.Spec.Image.Build.ContextArchive
				}
				if !reflect.DeepEqual(*expected, existing.Spec) {
					return fmt.Errorf("physical image %q has a different spec", existing.NamespacedName())
				}
				return nil
			})
		if createErr != nil {
			log.Error(createErr, "Failed to declare tunnel image")
			if apierrors.IsAlreadyExists(createErr) {
				data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart, createErr.Error())
			}
			return nil, change | additionalReconciliationNeeded
		}
		image = createdImage
		change |= applyLogicalTunnelProxyImageStatus(proxy, image)
	}
	if data.imageUID == "" {
		data.message = "Waiting to observe the owned PhysicalContainerImage."
		return nil, change | additionalReconciliationNeeded
	}
	if data.imageBuildPlan != nil {
		archive := image.Spec.Image.Build.ContextArchive
		if archive != nil && archive.Source == data.imageBuildPlan.BuildContextArchive.Source {
			if image.Spec.RemoveBuildContextArchive {
				data.imageBuildPlan = nil
			}
		} else if cleanupErr := removeLogicalTunnelProxyBuildContext(data); cleanupErr != nil {
			log.Error(cleanupErr, "Failed to remove unused tunnel image build context")
			return nil, change | additionalReconciliationNeeded
		}
	}
	return image, change
}

func removeLogicalTunnelProxyBuildContext(data *logicalTunnelProxyData) error {
	if data.imageBuildPlan == nil {
		return nil
	}
	source := data.imageBuildPlan.BuildContextArchive.Source
	if removeErr := os.Remove(source); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove tunnel image build context %q: %w", source, removeErr)
	}
	data.imageBuildPlan = nil
	return nil
}

// ensurePhysicalContainer finds or declares the owned container and observes its startup.
func (r *LogicalContainerTunnelProxyReconciler) ensurePhysicalContainer(
	ctx context.Context, proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData, imageRef string, log logr.Logger,
) (*apiv2.PhysicalContainer, objectChange) {
	container, containerErr := r.findPhysicalContainer(ctx, proxy, data)
	if containerErr != nil {
		log.Error(containerErr, "Failed to get pending tunnel container")
		return nil, additionalReconciliationNeeded
	}
	change := applyLogicalTunnelProxyContainerStatus(proxy, container)
	if container == nil && data.containerUID != "" {
		data.message = "Waiting to observe the owned PhysicalContainer."
		return nil, change | additionalReconciliationNeeded
	}
	if container == nil {
		network := &apiv2.LogicalContainerNetwork{}
		networkName := commonapi.AsNamespacedName(proxy.Spec.NetworkRef, proxy.Namespace)
		networkErr := r.Get(ctx, networkName, network)
		if networkErr != nil {
			if apierrors.IsNotFound(networkErr) {
				data.message = fmt.Sprintf("LogicalContainerNetwork %q does not exist.", networkName)
				return nil, change
			}
			log.Error(networkErr, "Failed to read tunnel proxy logical network")
			return nil, change | additionalReconciliationNeeded
		}
		if network.Status.Phase == apiv2.LogicalContainerNetworkPhaseFailed {
			data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart,
				fmt.Sprintf("LogicalContainerNetwork %q failed: %s", networkName, physicalResourceStatusMessage(network.Status.Conditions)))
			return nil, change | additionalReconciliationNeeded
		}
		if !network.DeletionTimestamp.IsZero() {
			data.message = fmt.Sprintf("LogicalContainerNetwork %q is terminating.", networkName)
			return nil, change
		}
		if network.Status.PhysicalNetworkRef == "" {
			data.message = fmt.Sprintf("LogicalContainerNetwork %q has not published its backing physical network reference.", networkName)
			return nil, change
		}
		if data.security == nil {
			security, securityErr := dcptun.NewTunnelProxySecurityConfig()
			if securityErr != nil {
				log.Error(securityErr, "Failed to prepare tunnel security configuration")
				data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart, securityErr.Error())
				return nil, change | additionalReconciliationNeeded
			}
			data.security = &security
		}
		if data.clientSecurityFile == "" {
			path, securityFileErr := r.createLogicalTunnelProxySecurityFile(ctx, data.security.ClientProxyConfig())
			if securityFileErr != nil {
				log.Error(securityFileErr, "Failed to prepare client tunnel security file")
				return nil, change | additionalReconciliationNeeded
			}
			data.clientSecurityFile = path
		}
		desiredContainer := &apiv2.PhysicalContainer{
			ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(proxy.UID), Namespace: proxy.Namespace},
			Spec: apiv2.PhysicalContainerSpec{Container: &apiv2.PhysicalContainerConfig{
				ImageRef: imageRef, ContainerName: "tunnel-" + string(proxy.UID), Entrypoint: dcptun.ClientProxyBinaryPath,
				Command: []string{"client", "--client-control-address", networking.IPv4AllInterfaceAddress,
					"--client-control-port", strconv.Itoa(dcptun.DefaultContainerProxyControlPort),
					"--client-data-address", networking.IPv4AllInterfaceAddress, "--client-data-port", strconv.Itoa(dcptun.DefaultContainerProxyDataPort),
					"--control-security-file", clientProxySecurityFilePath},
				CreateFiles: []apiv2.CreateFileSystem{{
					Destination: clientProxySecurityDirectory,
					Entries:     []apiv2.FileSystemEntry{{Name: clientProxySecurityFileName, Mode: 0o600, Source: data.clientSecurityFile}},
				}},
				Ports:    []apiv2.ContainerPort{{ContainerPort: dcptun.DefaultContainerProxyControlPort}, {ContainerPort: dcptun.DefaultContainerProxyDataPort}},
				Networks: []apiv2.ContainerNetworkConnectionConfig{{NetworkRef: network.Status.PhysicalNetworkRef, Aliases: slices.Clone(proxy.Spec.Aliases)}},
			}},
		}
		createdContainer, createErr := createLogicalBacking(ctx, r.Client, r.NoCacheClient, proxy, desiredContainer, physicalContainerFinalizer,
			&data.containerUID, &data.unobservedChildren, nil)
		if createErr != nil {
			log.Error(createErr, "Failed to declare tunnel container")
			if apierrors.IsAlreadyExists(createErr) {
				data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart, createErr.Error())
			}
			return nil, change | additionalReconciliationNeeded
		}
		container = createdContainer
		change |= applyLogicalTunnelProxyContainerStatus(proxy, container)
	}
	if !observeTunnelContainer(container, data) {
		if data.state != logicalTunnelProxyPending {
			return nil, change | additionalReconciliationNeeded
		}
		return nil, change
	}
	return container, change
}

// ensureLogicalTunnelServer schedules the host tunnel server launch and reports whether a server is launching or launched.
func (r *LogicalContainerTunnelProxyReconciler) ensureLogicalTunnelServer(
	proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData, container *apiv2.PhysicalContainer, log logr.Logger,
) bool {
	if data.server.run != nil || data.server.operation != nil {
		return true
	}
	controlPort, controlPortErr := physicalContainerHostPort(container.Status.PortMappings, dcptun.DefaultContainerProxyControlPort)
	dataPort, dataPortErr := physicalContainerHostPort(container.Status.PortMappings, dcptun.DefaultContainerProxyDataPort)
	if errors.Join(controlPortErr, dataPortErr) != nil {
		data.message = "Waiting for client proxy container port mappings."
		return false
	}
	if data.security == nil {
		data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart, "Tunnel control security configuration is unavailable.")
		return false
	}
	executablePath, pathErr := dcppaths.GetDcpExePath()
	if pathErr != nil {
		log.Error(pathErr, "Failed to resolve tunnel server executable")
		data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart, pathErr.Error())
		return false
	}
	launch := logicalTunnelServerLaunch{
		proxyName: proxy.NamespacedName(), proxyUID: proxy.UID, executablePath: executablePath,
		clientControlPort: controlPort, clientDataPort: dataPort, security: *data.security,
	}
	operationCtx, cancel := context.WithCancel(r.LifetimeCtx)
	operation := &logicalTunnelServerOperation{cancel: cancel}
	enqueueErr := r.operationQueue.Enqueue(func(_ context.Context) {
		defer cancel()
		r.launchLogicalTunnelServer(operationCtx, operation, launch, log)
	})
	if enqueueErr != nil {
		cancel()
		log.Error(enqueueErr, "Failed to queue tunnel server launch")
		return false
	}
	data.server = logicalTunnelServer{operation: operation}
	return true
}

// logicalTunnelServerLaunch is the reconciliation snapshot a background server launch works from.
type logicalTunnelServerLaunch struct {
	proxyName         types.NamespacedName
	proxyUID          types.UID
	executablePath    string
	clientControlPort int32
	clientDataPort    int32
	security          dcptun.TunnelProxySecurityConfig
}

// launchLogicalTunnelServer starts the server and publishes the result for the next reconciliation.
func (r *LogicalContainerTunnelProxyReconciler) launchLogicalTunnelServer(
	ctx context.Context, operation *logicalTunnelServerOperation, launch logicalTunnelServerLaunch, log logr.Logger,
) {
	server, failure := r.startLogicalTunnelServer(ctx, launch, log)
	queued := r.proxies.QueueDeferredOpForStateKey(launch.proxyName, launch.proxyUID,
		func(name types.NamespacedName, _ types.UID, proxy *apiv2.LogicalContainerTunnelProxy) {
			_, current := r.proxies.BorrowByNamespacedName(name)
			if current == nil || current.server.operation != operation {
				r.abandonLogicalTunnelServer(server, log)
				return
			}
			if server.run != nil {
				if exit, exited := server.run.getExit(); exited && exit.exitType == serverProxyExitTypeUnexpected {
					// The exit callback ran before this result was applied, so it could not match the published run.
					server.exited = true
					if exit.err == nil && exit.exitCode != process.UnknownExitCode {
						server.exitCode = &exit.exitCode
					}
				}
			}
			current.server = server
			// Deletion cancels startup, so a failure observed while deleting is not a startup failure.
			starting := current.state == logicalTunnelProxyPending || current.state == logicalTunnelProxyStarting
			if failure != "" && starting && proxy.DeletionTimestamp.IsZero() {
				current.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart, failure)
			}
			_ = r.proxies.Update(name, launch.proxyUID, current)
		})
	if !queued {
		r.abandonLogicalTunnelServer(server, log)
		return
	}
	r.ScheduleReconciliation(launch.proxyName)
}

// startLogicalTunnelServer launches tunnel-server with stdin credentials and waits for its stdout configuration and readiness.
// A failed start returns a non-empty failure; the returned server retains a process only when stopping it failed.
func (r *LogicalContainerTunnelProxyReconciler) startLogicalTunnelServer(
	ctx context.Context, launch logicalTunnelServerLaunch, log logr.Logger,
) (logicalTunnelServer, string) {
	stdoutFile, stdoutErr := usvc_io.CreateNewTempFile(fmt.Sprintf("logical-tunnel_out_%s", launch.proxyUID), osutil.PermissionOnlyOwnerReadWrite)
	if stdoutErr != nil {
		log.Error(stdoutErr, "Failed to create tunnel server stdout file")
		return logicalTunnelServer{}, fmt.Sprintf("Failed to create tunnel server stdout file: %v", stdoutErr)
	}
	stderrFile, stderrErr := usvc_io.CreateNewTempFile(fmt.Sprintf("logical-tunnel_err_%s", launch.proxyUID), osutil.PermissionOnlyOwnerReadWrite)
	if stderrErr != nil {
		log.Error(stderrErr, "Failed to create tunnel server stderr file")
		if cleanupErr := closeAndRemoveServerProxyOutputFiles(stdoutFile, nil); cleanupErr != nil {
			log.Error(cleanupErr, "Failed to remove tunnel server output files")
		}
		return logicalTunnelServer{}, fmt.Sprintf("Failed to create tunnel server stderr file: %v", stderrErr)
	}
	securityJSON, securityErr := json.Marshal(launch.security.ServerProxyConfig())
	if securityErr != nil {
		if cleanupErr := closeAndRemoveServerProxyOutputFiles(stdoutFile, stderrFile); cleanupErr != nil {
			log.Error(cleanupErr, "Failed to remove tunnel server output files")
		}
		return logicalTunnelServer{}, fmt.Sprintf("Failed to encode tunnel server control security: %v", securityErr)
	}
	cmd := exec.Command(launch.executablePath, "tunnel-server",
		networking.IPv4LocalhostDefaultAddress, strconv.Itoa(int(launch.clientControlPort)),
		networking.IPv4LocalhostDefaultAddress, strconv.Itoa(int(launch.clientDataPort)),
		"--server-control-address", networking.IPv4LocalhostDefaultAddress,
		"--control-security-stdin")
	// Stdin keeps server credentials out of process arguments and avoids a persistent host credential file.
	cmd.Stdin = bytes.NewReader(securityJSON)
	cmd.Stdout, cmd.Stderr = stdoutFile, stderrFile
	cmd.Env = os.Environ()
	logger.WithSessionId(cmd)
	run := newServerProxyRun()
	run.resourceUID = launch.proxyUID
	exitHandler := process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, exitErr error) {
		r.logicalTunnelServerExited(launch, run, stdoutFile, stderrFile, pid, exitCode, exitErr, log)
	})
	handle, startWaitForExit, startErr := r.config.ProcessExecutor.StartProcess(
		context.Background(), cmd, exitHandler, process.CreationFlagEnsureKillOnDispose, nil)
	if startErr != nil {
		log.Error(startErr, "Failed to start tunnel server process")
		if cleanupErr := closeAndRemoveServerProxyOutputFiles(stdoutFile, stderrFile); cleanupErr != nil {
			log.Error(cleanupErr, "Failed to remove tunnel server output files")
		}
		return logicalTunnelServer{}, fmt.Sprintf("Failed to start the host tunnel server process: %v", startErr)
	}
	// Record the process identity before enabling its exit callback.
	run.handle = handle
	startWaitForExit()
	dcpproc.RunProcessWatcher(r.config.ProcessExecutor, handle, log)
	server := logicalTunnelServer{run: run, stdout: stdoutFile, stderr: stderrFile}

	config, configErr := readServerProxyConfigOrExit(ctx, run, stdoutFile.Name(), r.config.ServerConfigReadTimeout)
	var startupErr error
	if configErr != nil {
		startupErr = fmt.Errorf("read tunnel server configuration: %w", configErr)
	} else if readyErr := waitServerProxyReadyOrExit(ctx, run, stdoutFile.Name(), r.config.ControlStartupTimeout); readyErr != nil {
		startupErr = fmt.Errorf("wait for tunnel server control readiness: %w", readyErr)
	} else if dialOptions, dialOptionsErr := launch.security.ControllerConfig().ClientDialOptions(); dialOptionsErr != nil {
		startupErr = fmt.Errorf("prepare tunnel control client: %w", dialOptionsErr)
	} else if connection, connectionErr := grpc.NewClient(
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, config.ServerControlPort), dialOptions...); connectionErr != nil {
		startupErr = fmt.Errorf("create tunnel control client: %w", connectionErr)
	} else {
		server.connection = connection
		return server, ""
	}

	failure := fmt.Sprintf("The host tunnel server did not become ready: %v", startupErr)
	if exit, exited := run.getExit(); exited && exit.exitType == serverProxyExitTypeUnexpected {
		failure = fmt.Sprintf("%s %s", tunnelBackingExitMessage("Host server process", &exit.exitCode), failure)
	}
	if stderrExcerpt := readBoundedServerProxyStderr(stderrFile.Name()); stderrExcerpt != "" {
		log.Error(startupErr, "Tunnel server startup failed", "ServerStderr", stderrExcerpt)
	} else {
		log.Error(startupErr, "Tunnel server startup failed")
	}
	if stopErr := r.stopLogicalTunnelServerProcess(run); stopErr != nil {
		log.Error(stopErr, "Failed to stop tunnel server after startup failure", "PID", handle.Pid)
		return server, failure
	}
	if cleanupErr := closeAndRemoveServerProxyOutputFiles(stdoutFile, stderrFile); cleanupErr != nil {
		log.Error(cleanupErr, "Failed to remove tunnel server output files")
	}
	return logicalTunnelServer{}, failure
}

// logicalTunnelServerExited releases the exited process's output handles and records unexpected exits.
func (r *LogicalContainerTunnelProxyReconciler) logicalTunnelServerExited(
	launch logicalTunnelServerLaunch, run *serverProxyRun, stdoutFile *os.File, stderrFile *os.File,
	pid process.Pid_t, exitCode int32, exitErr error, log logr.Logger,
) {
	if closeErr := closeServerProxyOutputFiles(stdoutFile, stderrFile, false); closeErr != nil {
		log.Error(closeErr, "Failed to close tunnel server output files", "PID", pid)
	}
	if !run.recordUnexpectedExit(exitCode, exitErr) {
		return
	}
	// Output files are removed once the exit is applied, so capture the server's diagnostics now.
	exitFailure := errors.Join(fmt.Errorf("tunnel server process exited unexpectedly with exit code %d", exitCode), exitErr)
	if stderrExcerpt := readBoundedServerProxyStderr(stderrFile.Name()); stderrExcerpt != "" {
		log.Error(exitFailure, "Tunnel server process exited unexpectedly", "PID", pid, "ServerStderr", stderrExcerpt)
	} else {
		log.Error(exitFailure, "Tunnel server process exited unexpectedly", "PID", pid)
	}
	queued := r.proxies.QueueDeferredOpForStateKey(launch.proxyName, launch.proxyUID,
		func(name types.NamespacedName, _ types.UID, _ *apiv2.LogicalContainerTunnelProxy) {
			_, current := r.proxies.BorrowByNamespacedName(name)
			if current == nil || current.server.run != run {
				return
			}
			current.server.exited = true
			if exitErr == nil && exitCode != process.UnknownExitCode {
				current.server.exitCode = &exitCode
			}
			_ = r.proxies.Update(name, launch.proxyUID, current)
		})
	if queued {
		r.ScheduleReconciliation(launch.proxyName)
	}
}

func (r *LogicalContainerTunnelProxyReconciler) stopLogicalTunnelServerProcess(run *serverProxyRun) error {
	run.expectExit()
	stopCtx, cancel := process.WithDetachedStopTimeout(r.LifetimeCtx)
	defer cancel()
	stopErr := r.config.ProcessExecutor.StopProcess(stopCtx, run.handle)
	if stopErr != nil && !process.IsProcessGoneErr(stopErr) {
		return stopErr
	}
	return nil
}

// releaseLogicalTunnelServer stops the host tunnel server and removes its output, reporting whether the server is fully released.
// An in-flight launch is cancelled and awaited; a running process is stopped in the background.
func (r *LogicalContainerTunnelProxyReconciler) releaseLogicalTunnelServer(
	proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData, log logr.Logger,
) bool {
	server := &data.server
	if server.operation != nil {
		server.operation.cancel()
		return false
	}
	if server.connection != nil {
		if closeErr := server.connection.Close(); closeErr != nil && !errors.Is(closeErr, grpc.ErrClientConnClosing) {
			log.Error(closeErr, "Failed to close tunnel control connection")
		}
		server.connection = nil
	}
	if server.run != nil && !server.exited {
		if time.Now().Before(server.stopRetryAfter) {
			return false
		}
		run, stdoutFile, stderrFile := server.run, server.stdout, server.stderr
		operation := &logicalTunnelServerOperation{cancel: func() {}}
		name, uid := proxy.NamespacedName(), proxy.UID
		enqueueErr := r.operationQueue.Enqueue(func(_ context.Context) {
			stopErr := r.stopLogicalTunnelServerProcess(run)
			if stopErr != nil {
				log.Error(stopErr, "Failed to stop tunnel server process", "PID", run.handle.Pid)
			} else if cleanupErr := closeAndRemoveServerProxyOutputFiles(stdoutFile, stderrFile); cleanupErr != nil {
				log.Error(cleanupErr, "Failed to remove tunnel server output files")
				stopErr = cleanupErr
			}
			queued := r.proxies.QueueDeferredOpForStateKey(name, uid, func(currentName types.NamespacedName, _ types.UID, _ *apiv2.LogicalContainerTunnelProxy) {
				_, current := r.proxies.BorrowByNamespacedName(currentName)
				if current == nil || current.server.operation != operation {
					return
				}
				current.server.operation = nil
				if stopErr == nil {
					current.server.exited, current.server.stdout, current.server.stderr = true, nil, nil
					current.server.stopRetryAfter = time.Time{}
				} else {
					current.server.stopRetryAfter = time.Now().Add(delayDuration(LongDelay))
				}
				_ = r.proxies.Update(currentName, uid, current)
			})
			if queued {
				r.ScheduleReconciliation(name)
			}
		})
		if enqueueErr != nil {
			log.Error(enqueueErr, "Failed to queue tunnel server stop")
			return false
		}
		server.operation = operation
		return false
	}
	if server.stdout != nil || server.stderr != nil {
		if cleanupErr := closeAndRemoveServerProxyOutputFiles(server.stdout, server.stderr); cleanupErr != nil {
			log.Error(cleanupErr, "Failed to remove tunnel server output files")
			return false
		}
		server.stdout, server.stderr = nil, nil
	}
	return true
}

// abandonLogicalTunnelServer releases a server whose proxy state no longer tracks it.
func (r *LogicalContainerTunnelProxyReconciler) abandonLogicalTunnelServer(server logicalTunnelServer, log logr.Logger) {
	if server.operation != nil {
		// The in-flight operation releases its own result once it can no longer publish it.
		server.operation.cancel()
	}
	if server.connection != nil {
		_ = server.connection.Close()
	}
	running := server.run != nil && !server.exited
	if !running && server.stdout == nil && server.stderr == nil {
		return
	}
	enqueueErr := r.operationQueue.Enqueue(func(_ context.Context) {
		if running {
			if stopErr := r.stopLogicalTunnelServerProcess(server.run); stopErr != nil {
				log.Error(stopErr, "Failed to stop abandoned tunnel server process", "PID", server.run.handle.Pid)
				return
			}
		}
		if cleanupErr := closeAndRemoveServerProxyOutputFiles(server.stdout, server.stderr); cleanupErr != nil {
			log.Error(cleanupErr, "Failed to remove abandoned tunnel server output files")
		}
	})
	if enqueueErr != nil {
		log.Error(enqueueErr, "Failed to queue abandoned tunnel server cleanup")
	}
}

func (r *LogicalContainerTunnelProxyReconciler) createLogicalTunnelProxySecurityFile(
	ctx context.Context, security dcptun.TunnelControlSecurityConfig,
) (string, error) {
	contents, marshalErr := json.Marshal(security)
	if marshalErr != nil {
		return "", fmt.Errorf("encode logical tunnel control security: %w", marshalErr)
	}
	return r.securityFiles.Create(ctx, "logical-tunnel-control-", clientProxySecurityFileName, contents)
}

func (r *LogicalContainerTunnelProxyReconciler) findPhysicalContainer(
	ctx context.Context, proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData,
) (*apiv2.PhysicalContainer, error) {
	var children apiv2.PhysicalContainerList
	listErr := r.List(ctx, &children, ctrl_client.InNamespace(proxy.Namespace),
		ctrl_client.MatchingFields{logicalTunnelProxyOwnerUIDField: string(proxy.UID)})
	if listErr != nil {
		lookupErr := fmt.Errorf("list owned physical containers: %w", listErr)
		return nil, lookupErr
	}
	var owned *apiv2.PhysicalContainer
	for index := range children.Items {
		child := &children.Items[index]
		if !metav1.IsControlledBy(child, proxy) {
			continue
		}
		if owned != nil {
			multipleErr := fmt.Errorf("more than one PhysicalContainer belongs to %q", proxy.NamespacedName())
			data.failMissing(multipleErr.Error())
			return nil, multipleErr
		}
		owned = child
	}
	if owned != nil {
		data.unobservedChildren.observeCached(owned)
		if (data.containerUID != "" && owned.UID != data.containerUID) || !hasFinalizer(owned, LogicalBackingFinalizer) {
			identityErr := fmt.Errorf("physical container %q UID or logical hold changed", owned.NamespacedName())
			data.failMissing(identityErr.Error())
			return nil, identityErr
		}
		data.containerUID = owned.UID
	}
	return owned, nil
}

func (r *LogicalContainerTunnelProxyReconciler) findPhysicalContainerImage(
	ctx context.Context, proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData,
) (*apiv2.PhysicalContainerImage, error) {
	var children apiv2.PhysicalContainerImageList
	listErr := r.List(ctx, &children, ctrl_client.InNamespace(proxy.Namespace),
		ctrl_client.MatchingFields{logicalTunnelProxyOwnerUIDField: string(proxy.UID)})
	if listErr != nil {
		lookupErr := fmt.Errorf("list owned physical images: %w", listErr)
		return nil, lookupErr
	}
	var owned *apiv2.PhysicalContainerImage
	for index := range children.Items {
		child := &children.Items[index]
		if !metav1.IsControlledBy(child, proxy) {
			continue
		}
		if owned != nil {
			multipleErr := fmt.Errorf("more than one PhysicalContainerImage belongs to %q", proxy.NamespacedName())
			data.failMissing(multipleErr.Error())
			return nil, multipleErr
		}
		owned = child
	}
	if owned != nil {
		data.unobservedChildren.observeCached(owned)
		if (data.imageUID != "" && owned.UID != data.imageUID) || !hasFinalizer(owned, LogicalBackingFinalizer) {
			identityErr := fmt.Errorf("physical image %q UID or logical hold changed", owned.NamespacedName())
			data.failMissing(identityErr.Error())
			return nil, identityErr
		}
		data.imageUID = owned.UID
	}
	return owned, nil
}

func handleLogicalTunnelProxyStarting(
	ctx context.Context, r *LogicalContainerTunnelProxyReconciler, proxy *apiv2.LogicalContainerTunnelProxy,
	_ logicalTunnelProxyState, data *logicalTunnelProxyData, log logr.Logger,
) objectChange {
	data.reason, data.message = "", ""
	container, containerErr := r.findPhysicalContainer(ctx, proxy, data)
	if containerErr != nil {
		log.Error(containerErr, "Failed to get starting tunnel container")
		return additionalReconciliationNeeded
	}
	if container == nil {
		data.message = "Waiting to observe the owned PhysicalContainer."
		return additionalReconciliationNeeded
	}
	change := applyLogicalTunnelProxyContainerStatus(proxy, container)
	image, imageErr := r.findPhysicalContainerImage(ctx, proxy, data)
	if imageErr != nil {
		log.Error(imageErr, "Failed to get starting tunnel image")
		return change | additionalReconciliationNeeded
	}
	change |= applyLogicalTunnelProxyImageStatus(proxy, image)
	if !observeTunnelContainer(container, data) {
		if data.state != logicalTunnelProxyStarting {
			change |= additionalReconciliationNeeded
		}
		return change
	}
	switch {
	case data.server.operation != nil:
		data.message = "Waiting for the host tunnel server to report control readiness."
	case data.server.run == nil:
		// No launch is in flight or recorded, so restart from discovery to launch the server.
		data.state = logicalTunnelProxyPending
		change |= additionalReconciliationNeeded
	case data.server.exited:
		data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart, tunnelBackingExitMessage("Host server process", data.server.exitCode))
		change |= additionalReconciliationNeeded
	default:
		data.state, data.started = logicalTunnelProxyRunning, true
		change |= additionalReconciliationNeeded
	}
	return change
}

func observeTunnelContainer(container *apiv2.PhysicalContainer, data *logicalTunnelProxyData) bool {
	if !container.DeletionTimestamp.IsZero() {
		data.failMissing("The owned PhysicalContainer is being deleted.")
		return false
	}
	switch container.Status.Phase {
	case apiv2.PhysicalContainerPhaseRunning:
		return container.Status.ContainerID != ""
	case apiv2.PhysicalContainerPhasePaused:
		data.message = "The client container is paused."
		if data.started {
			data.state, data.reason = logicalTunnelProxyRunning, apiv2.LogicalContainerTunnelProxyReasonPaused
		}
	case apiv2.PhysicalContainerPhaseExited:
		message := tunnelBackingExitMessage("Client container", container.Status.ExitCode)
		if data.started {
			data.stopAfterExit(message)
		} else {
			data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart, message)
		}
	case apiv2.PhysicalContainerPhaseFailed:
		data.failMissing("Client container is unavailable: " + physicalResourceStatusMessage(container.Status.Conditions))
	case apiv2.PhysicalContainerPhaseUnknown:
		data.message = "Client container state is unknown: " + physicalResourceStatusMessage(container.Status.Conditions)
		if data.started {
			data.state, data.reason = logicalTunnelProxyUnknown, apiv2.LogicalContainerTunnelProxyReasonLookupFailed
		}
	}
	return false
}

func tunnelBackingExitMessage(backing string, exitCode *int32) string {
	if exitCode == nil {
		return backing + " exited."
	}
	return fmt.Sprintf("%s exited with code %d.", backing, *exitCode)
}

func (r *LogicalContainerTunnelProxyReconciler) observeTunnelPair(
	ctx context.Context, proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData, log logr.Logger,
) (objectChange, bool) {
	image, imageErr := r.findPhysicalContainerImage(ctx, proxy, data)
	if imageErr != nil {
		log.Error(imageErr, "Failed to get tunnel image")
		return noChange, false
	}
	container, containerErr := r.findPhysicalContainer(ctx, proxy, data)
	if containerErr != nil {
		log.Error(containerErr, "Failed to get tunnel container")
		return noChange, false
	}
	change := applyLogicalTunnelProxyContainerStatus(proxy, container)
	change |= applyLogicalTunnelProxyImageStatus(proxy, image)
	var missing []string
	if container == nil {
		missing = append(missing, "The owned PhysicalContainer no longer exists.")
	}
	if image == nil {
		missing = append(missing, "The owned PhysicalContainerImage no longer exists.")
	}
	if data.server.run == nil || (data.server.connection == nil && !data.server.exited) {
		missing = append(missing, "The host tunnel server process is not running.")
	}
	if len(missing) != 0 {
		data.failMissing(strings.Join(missing, " "))
		return change, false
	}
	switch {
	case !container.DeletionTimestamp.IsZero():
		data.failMissing("The owned PhysicalContainer is being deleted.")
	case container.Status.Phase == apiv2.PhysicalContainerPhaseExited:
		data.stopAfterExit(tunnelBackingExitMessage("Client container", container.Status.ExitCode))
	case container.Status.Phase == apiv2.PhysicalContainerPhaseFailed:
		data.failMissing("Client container is unavailable: " + physicalResourceStatusMessage(container.Status.Conditions))
	case data.server.exited:
		data.stopAfterExit(tunnelBackingExitMessage("Host server process", data.server.exitCode))
	case container.Status.Phase == apiv2.PhysicalContainerPhaseUnknown:
		data.state, data.reason, data.message = logicalTunnelProxyUnknown, apiv2.LogicalContainerTunnelProxyReasonLookupFailed,
			"Client container state is unknown: "+physicalResourceStatusMessage(container.Status.Conditions)
	case container.Status.Phase == apiv2.PhysicalContainerPhasePaused:
		data.state, data.reason, data.message = logicalTunnelProxyRunning, apiv2.LogicalContainerTunnelProxyReasonPaused,
			"The client container is paused."
	case container.Status.Phase == apiv2.PhysicalContainerPhaseRunning:
		return change, container.Status.ContainerID != ""
	}
	return change, false
}

func handleLogicalTunnelProxyRunning(
	ctx context.Context, r *LogicalContainerTunnelProxyReconciler, proxy *apiv2.LogicalContainerTunnelProxy,
	_ logicalTunnelProxyState, data *logicalTunnelProxyData, log logr.Logger,
) objectChange {
	data.reason, data.message = "", ""
	change, available := r.observeTunnelPair(ctx, proxy, data, log)
	if !available {
		if data.state == logicalTunnelProxyRunning && data.reason == apiv2.LogicalContainerTunnelProxyReasonPaused {
			return change
		}
		if endpointErr := syncOwnedServiceEndpoints(ctx, r.Client, r.NoCacheClient, proxy, logicalTunnelProxyOwnerUIDField, &data.unobservedChildren, nil); endpointErr != nil {
			log.Error(endpointErr, "Failed to withdraw unavailable tunnel endpoints")
		}
		return change | additionalReconciliationNeeded
	}
	data.state = logicalTunnelProxyRunning
	change |= r.manageLogicalTunnels(ctx, proxy, data, log)
	desired := make(map[string]apiv2.ServiceEndpointSpec)
	for _, tunnel := range proxy.Spec.Tunnels {
		record, found := data.tunnels[tunnel.Name]
		if !found || record.status.State != apiv2.LogicalContainerTunnelStateReady {
			continue
		}
		var addresses []apiv2.ServiceAddress
		for _, address := range record.status.ClientProxyAddresses {
			addresses = append(addresses, apiv2.ServiceAddress{Address: address, Port: record.status.ClientProxyPort})
		}
		desired[logicalPortResourceName(proxy, tunnel.Name)] = apiv2.ServiceEndpointSpec{ServiceRef: tunnel.ClientServiceRef, Addresses: addresses}
	}
	if endpointErr := syncOwnedServiceEndpoints(ctx, r.Client, r.NoCacheClient, proxy, logicalTunnelProxyOwnerUIDField, &data.unobservedChildren, desired); endpointErr != nil {
		log.Error(endpointErr, "Failed to synchronize tunnel endpoints")
		change |= additionalReconciliationNeeded
	}
	return change
}

func handleLogicalTunnelProxyTerminal(
	ctx context.Context, r *LogicalContainerTunnelProxyReconciler, proxy *apiv2.LogicalContainerTunnelProxy,
	_ logicalTunnelProxyState, data *logicalTunnelProxyData, log logr.Logger,
) objectChange {
	change := noChange
	if !r.releaseLogicalTunnelServer(proxy, data, log) {
		change |= additionalReconciliationNeeded
	}
	for name, record := range data.tunnels {
		if record.status.State != apiv2.LogicalContainerTunnelStateFailed {
			record.status.State, record.status.ErrorMessage, record.status.Timestamp = apiv2.LogicalContainerTunnelStateFailed, "The tunnel proxy is unavailable.", metav1.NowMicro()
			data.tunnels[name] = record
			data.configurationVersion++
		}
	}
	if endpointErr := syncOwnedServiceEndpoints(ctx, r.Client, r.NoCacheClient, proxy, logicalTunnelProxyOwnerUIDField, &data.unobservedChildren, nil); endpointErr != nil {
		log.Error(endpointErr, "Failed to withdraw terminal tunnel endpoints")
		change |= additionalReconciliationNeeded
	}
	var containers apiv2.PhysicalContainerList
	containerListErr := r.List(ctx, &containers, ctrl_client.InNamespace(proxy.Namespace),
		ctrl_client.MatchingFields{logicalTunnelProxyOwnerUIDField: string(proxy.UID)})
	if containerListErr != nil {
		log.Error(containerListErr, "Failed to list terminal tunnel containers")
		change |= additionalReconciliationNeeded
	} else {
		for index := range containers.Items {
			container := &containers.Items[index]
			if !metav1.IsControlledBy(container, proxy) || !container.DeletionTimestamp.IsZero() || container.Spec.Stop {
				continue
			}
			containerPatch := ctrl_client.MergeFromWithOptions(container.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
			container.Spec.Stop = true
			if containerStopErr := r.Patch(ctx, container, containerPatch); containerStopErr != nil {
				log.Error(containerStopErr, "Failed to stop terminal tunnel container")
				change |= additionalReconciliationNeeded
			}
		}
	}
	return change
}

func handleLogicalTunnelProxyStopping(
	ctx context.Context, r *LogicalContainerTunnelProxyReconciler, proxy *apiv2.LogicalContainerTunnelProxy,
	state logicalTunnelProxyState, data *logicalTunnelProxyData, log logr.Logger,
) objectChange {
	change := handleLogicalTunnelProxyTerminal(ctx, r, proxy, state, data, log)
	container, containerErr := r.findPhysicalContainer(ctx, proxy, data)
	if containerErr != nil {
		log.Error(containerErr, "Failed to get stopping tunnel container")
		return change | additionalReconciliationNeeded
	}
	if data.state != logicalTunnelProxyStopping {
		return change | additionalReconciliationNeeded
	}
	change |= applyLogicalTunnelProxyContainerStatus(proxy, container)
	containerStopped := container == nil || container.Status.Phase == apiv2.PhysicalContainerPhaseExited || container.Status.Phase == apiv2.PhysicalContainerPhaseFailed
	serverStopped := data.server.operation == nil && (data.server.run == nil || data.server.exited)
	if containerStopped && serverStopped {
		data.state, data.reason = logicalTunnelProxyExited, apiv2.LogicalContainerTunnelProxyReasonCompleted
	}
	return change
}

// handleLogicalTunnelProxyUnknown observes a previously running pair or restarts discovery after an invalid state.
func handleLogicalTunnelProxyUnknown(
	ctx context.Context, r *LogicalContainerTunnelProxyReconciler, proxy *apiv2.LogicalContainerTunnelProxy,
	state logicalTunnelProxyState, data *logicalTunnelProxyData, log logr.Logger,
) objectChange {
	if data.started {
		return handleLogicalTunnelProxyRunning(ctx, r, proxy, state, data, log)
	}
	change := noChange
	if endpointErr := syncOwnedServiceEndpoints(ctx, r.Client, r.NoCacheClient, proxy, logicalTunnelProxyOwnerUIDField, &data.unobservedChildren, nil); endpointErr != nil {
		log.Error(endpointErr, "Failed to withdraw tunnel endpoints after invalid state")
		change |= additionalReconciliationNeeded
	}
	log.Error(fmt.Errorf("invalid tunnel proxy state %q", state), "Restarting logical tunnel proxy discovery")
	data.state, data.reason, data.message = logicalTunnelProxyPending, "", ""
	return change | additionalReconciliationNeeded
}

func (r *LogicalContainerTunnelProxyReconciler) manageLogicalTunnels(
	ctx context.Context, proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData, log logr.Logger,
) objectChange {
	client := r.config.MakeTunnelControlClient(data.server.connection)
	desired := make(map[string]apiv2.LogicalContainerTunnel)
	for _, tunnel := range proxy.Spec.Tunnels {
		desired[tunnel.Name] = tunnel
	}
	change := noChange
	for name, record := range data.tunnels {
		if _, wanted := desired[name]; wanted {
			continue
		}
		if record.status.TunnelID != 0 || record.server != (apiv2.ServiceAddress{}) {
			if deleteErr := deleteLogicalTunnel(ctx, client, record.status.TunnelID, name); deleteErr != nil {
				log.Error(deleteErr, "Failed to delete removed tunnel", "Tunnel", name)
				change |= additionalReconciliationNeeded
				continue
			}
		}
		delete(data.tunnels, name)
		data.configurationVersion++
	}
	for _, tunnel := range proxy.Spec.Tunnels {
		record, exists := data.tunnels[tunnel.Name]
		if !exists {
			record.status = apiv2.LogicalContainerTunnelStatus{Name: tunnel.Name, State: apiv2.LogicalContainerTunnelStateNotReady, Timestamp: metav1.NowMicro()}
		}
		original := record.status.DeepCopy()
		if record.status.State != apiv2.LogicalContainerTunnelStateFailed {
			server, serverErr := r.tunnelServices(ctx, proxy, tunnel)
			if serverErr != nil {
				log.V(1).Info("Tunnel services are unavailable", "Tunnel", tunnel.Name, "Error", serverErr)
				change |= additionalReconciliationNeeded
			}
			if record.status.State == apiv2.LogicalContainerTunnelStateReady && (serverErr != nil || record.server != server) {
				if deleteErr := deleteLogicalTunnel(ctx, client, record.status.TunnelID, tunnel.Name); deleteErr != nil {
					log.Error(deleteErr, "Failed to delete unavailable tunnel", "Tunnel", tunnel.Name)
					change |= additionalReconciliationNeeded
					record.status.State = apiv2.LogicalContainerTunnelStateNotReady
				} else {
					record.status = apiv2.LogicalContainerTunnelStatus{Name: tunnel.Name, State: apiv2.LogicalContainerTunnelStateNotReady, Timestamp: metav1.NowMicro()}
					record.server, record.attempts = apiv2.ServiceAddress{}, 0
				}
			}
			if record.status.State == apiv2.LogicalContainerTunnelStateNotReady && !time.Now().Before(record.retryAfter) {
				// A lost preparation response leaves no ID, but its named runtime tunnel blocks a changed request.
				if record.status.TunnelID != 0 || (record.server != (apiv2.ServiceAddress{}) && record.server != server) {
					if deleteErr := deleteLogicalTunnel(ctx, client, record.status.TunnelID, tunnel.Name); deleteErr != nil {
						log.Error(deleteErr, "Failed to retire previous tunnel")
						change |= additionalReconciliationNeeded
					} else {
						record.status.TunnelID, record.server = 0, apiv2.ServiceAddress{}
					}
				}
				if record.status.TunnelID == 0 && serverErr == nil && (record.server == (apiv2.ServiceAddress{}) || record.server == server) {
					record.attempts++
					record.retryAfter = time.Now().Add(delayDuration(StandardDelay))
					if record.attempts > r.config.MaxTunnelPreparationAttempts {
						record.status.State, record.status.ErrorMessage = apiv2.LogicalContainerTunnelStateFailed, "Maximum tunnel preparation attempts reached."
					} else {
						record.server = server
						operationCtx, cancel := context.WithTimeout(ctx, tunnelOperationTimeout)
						request := &dcptunproto.TunnelReq{ServerAddress: stdproto.String(server.Address), ServerPort: stdproto.Int32(server.Port),
							ClientProxyPort: stdproto.Int32(server.Port), AllowClientProxyPortFallback: stdproto.Bool(true),
							TunnelName: stdproto.String(tunnel.Name)}
						prepared, prepareErr := client.PrepareTunnel(operationCtx, request)
						cancel()
						if prepareErr != nil {
							log.Error(prepareErr, "Failed to prepare tunnel", "Tunnel", tunnel.Name)
							change |= additionalReconciliationNeeded
						} else if prepared.GetTunnelRef().GetTunnelId() == 0 || !networking.IsValidPort(int(prepared.GetClientProxyPort())) || len(prepared.GetClientProxyAddresses()) == 0 {
							log.Error(errors.New("invalid tunnel preparation response"), "Failed to prepare tunnel", "Tunnel", tunnel.Name)
							change |= additionalReconciliationNeeded
						} else {
							record.status.State, record.status.TunnelID = apiv2.LogicalContainerTunnelStateReady, prepared.GetTunnelRef().GetTunnelId()
							record.status.ClientProxyAddresses, record.status.ClientProxyPort = slices.Clone(prepared.GetClientProxyAddresses()), prepared.GetClientProxyPort()
							slices.Sort(record.status.ClientProxyAddresses)
							record.status.ClientProxyAddresses = slices.Compact(record.status.ClientProxyAddresses)
							record.attempts = 0
						}
					}
				}
			}
			if record.status.State == apiv2.LogicalContainerTunnelStateNotReady {
				change |= additionalReconciliationNeeded
			}
		}
		if !exists || !reflect.DeepEqual(original, &record.status) {
			record.status.Timestamp = metav1.NowMicro()
			data.configurationVersion++
		}
		data.tunnels[tunnel.Name] = record
	}
	return change
}

func deleteLogicalTunnel(ctx context.Context, client dcptunproto.TunnelControlClient, id uint32, name string) error {
	operationCtx, cancel := context.WithTimeout(ctx, tunnelOperationTimeout)
	defer cancel()
	reference := &dcptunproto.TunnelRef{TunnelName: stdproto.String(name)}
	if id != 0 {
		reference.TunnelId = stdproto.Uint32(id)
	}
	_, deleteErr := client.DeleteTunnel(operationCtx, reference)
	if status.Code(deleteErr) == codes.NotFound {
		return nil
	}
	return deleteErr
}

func (r *LogicalContainerTunnelProxyReconciler) tunnelServices(
	ctx context.Context, proxy *apiv2.LogicalContainerTunnelProxy, tunnel apiv2.LogicalContainerTunnel,
) (apiv2.ServiceAddress, error) {
	server, client := &apiv2.LogicalService{}, &apiv2.LogicalService{}
	for index, service := range []*apiv2.LogicalService{server, client} {
		reference := tunnel.ServerServiceRef
		if index == 1 {
			reference = tunnel.ClientServiceRef
		}
		if getErr := r.Get(ctx, commonapi.AsNamespacedName(reference, proxy.Namespace), service); getErr != nil {
			return apiv2.ServiceAddress{}, getErr
		}
		if !service.DeletionTimestamp.IsZero() || service.Status.Phase == apiv2.LogicalServicePhaseFailed {
			return apiv2.ServiceAddress{}, errors.New("tunnel service failed or is terminating")
		}
		if service.Spec.EffectiveProtocol() != commonapi.PortProtocolTCP {
			return apiv2.ServiceAddress{}, errors.New("tunnel services must use TCP")
		}
	}
	if client.Spec.AddressAllocationMode != apiv2.ServiceAddressAllocationModeProxyless {
		return apiv2.ServiceAddress{}, errors.New("tunnel client service must be Proxyless")
	}
	address, found, selectionErr := networking.SelectServiceAddress(server.Status.EffectiveAddresses, networking.IpVersionPreferenceNone, networking.GetIpVersionPreference())
	if selectionErr != nil {
		return apiv2.ServiceAddress{}, selectionErr
	}
	if !found {
		return apiv2.ServiceAddress{}, errors.New("host service has no effective address")
	}
	return address, nil
}

func (r *LogicalContainerTunnelProxyReconciler) handleLogicalTunnelProxyDeletion(
	ctx context.Context, proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData, log logr.Logger,
) (change objectChange, delay AdditionalReconciliationDelay) {
	delay = StandardDelay
	if data == nil {
		data = &logicalTunnelProxyData{}
		for _, child := range []commonapi.DcpModelObject{&apiv2.PhysicalContainer{}, &apiv2.PhysicalContainerImage{}} {
			child.SetName("logical-" + string(proxy.UID))
			data.unobservedChildren.recordCreateAttempt(child)
		}
		for _, tunnel := range proxy.Spec.Tunnels {
			data.unobservedChildren.recordCreateAttempt(&apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: logicalPortResourceName(proxy, tunnel.Name)}})
		}
		r.proxies.Store(proxy.NamespacedName(), proxy.UID, data.Clone())
	}
	defer func() {
		if data != nil {
			_ = r.proxies.Update(proxy.NamespacedName(), proxy.UID, data)
			if data.state == "" {
				return
			}
			projectionChange, projectionDelay, valid := data.applyTo(proxy)
			change |= projectionChange
			delay = projectionDelay
			if !valid {
				log.Error(fmt.Errorf("invalid logical tunnel proxy state %v", data.state), "LogicalContainerTunnelProxy reached invalid reconciliation state")
			}
		}
	}()
	if data.state == logicalTunnelProxyRunning || (data.state == logicalTunnelProxyUnknown && data.started) {
		data.state, data.reason, data.message = logicalTunnelProxyStopping, "", "Stopping the tunnel proxy pair for deletion."
	}
	endpointsComplete, endpointErr := cleanupLogicalEndpoints(ctx, r.Client, r.NoCacheClient, proxy, logicalTunnelProxyOwnerUIDField, data.unobservedChildren)
	if endpointErr != nil {
		log.Error(endpointErr, "Failed to clean up tunnel endpoints")
	}
	waiting := !endpointsComplete || endpointErr != nil
	workloadsComplete := true
	if !r.releaseLogicalTunnelServer(proxy, data, log) {
		waiting, workloadsComplete = true, false
	}
	options := []ctrl_client.ListOption{
		ctrl_client.InNamespace(proxy.Namespace),
		ctrl_client.MatchingFields{logicalTunnelProxyOwnerUIDField: string(proxy.UID)},
	}
	var containers apiv2.PhysicalContainerList
	containerListErr := r.List(ctx, &containers, options...)
	if containerListErr != nil {
		log.Error(containerListErr, "Failed to list tunnel containers for deletion")
		waiting, workloadsComplete = true, false
	} else if containerConfirmationErr := resolveUnobservedLogicalChildren(ctx, r.NoCacheClient, proxy, data.unobservedChildren, &containers.Items); containerConfirmationErr != nil {
		log.Error(containerConfirmationErr, "Failed to resolve unobserved tunnel containers for deletion")
		waiting, workloadsComplete = true, false
	} else {
		for index := range containers.Items {
			container := &containers.Items[index]
			if !metav1.IsControlledBy(container, proxy) {
				continue
			}
			waiting, workloadsComplete = true, false
			if container.DeletionTimestamp.IsZero() {
				containerUID := container.UID
				if containerDeleteErr := r.Delete(ctx, container, ctrl_client.Preconditions{UID: &containerUID}); containerDeleteErr != nil && !apierrors.IsNotFound(containerDeleteErr) {
					log.Error(containerDeleteErr, "Failed to delete tunnel container")
				}
				continue
			}
			if !hasFinalizer(container, physicalContainerFinalizer) {
				if containerReleaseErr := ReleaseLogicalBackingFinalizer(ctx, r.Client, container); containerReleaseErr != nil && !apierrors.IsNotFound(containerReleaseErr) {
					log.Error(containerReleaseErr, "Failed to release logical tunnel container hold")
				}
			}
		}
	}
	if workloadsComplete {
		var images apiv2.PhysicalContainerImageList
		imageListErr := r.List(ctx, &images, options...)
		if imageListErr != nil {
			log.Error(imageListErr, "Failed to list tunnel images for deletion")
			waiting = true
		} else if imageConfirmationErr := resolveUnobservedLogicalChildren(ctx, r.NoCacheClient, proxy, data.unobservedChildren, &images.Items); imageConfirmationErr != nil {
			log.Error(imageConfirmationErr, "Failed to resolve unobserved tunnel images for deletion")
			waiting = true
		} else {
			for index := range images.Items {
				image := &images.Items[index]
				if !metav1.IsControlledBy(image, proxy) {
					continue
				}
				waiting = true
				if image.DeletionTimestamp.IsZero() {
					imageUID := image.UID
					if imageDeleteErr := r.Delete(ctx, image, ctrl_client.Preconditions{UID: &imageUID}); imageDeleteErr != nil && !apierrors.IsNotFound(imageDeleteErr) {
						log.Error(imageDeleteErr, "Failed to delete tunnel image")
					}
					continue
				}
				if !hasFinalizer(image, physicalContainerImageFinalizer) {
					if imageReleaseErr := ReleaseLogicalBackingFinalizer(ctx, r.Client, image); imageReleaseErr != nil && !apierrors.IsNotFound(imageReleaseErr) {
						log.Error(imageReleaseErr, "Failed to release logical tunnel image hold")
					}
				}
			}
		}
	}
	if workloadsComplete && data.state == logicalTunnelProxyStopping {
		data.state, data.reason, data.message = logicalTunnelProxyExited, apiv2.LogicalContainerTunnelProxyReasonStopped,
			"DCP stopped the tunnel proxy pair for deletion."
	}
	if waiting {
		return additionalReconciliationNeeded, delay
	}
	if discardErr := r.discardLogicalTunnelProxyData(proxy.NamespacedName()); discardErr != nil {
		log.Error(discardErr, "Failed to discard logical tunnel proxy state")
		return additionalReconciliationNeeded, delay
	}
	data = nil
	return deleteFinalizer(proxy, logicalTunnelProxyFinalizer, log), delay
}
