/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
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
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcptun"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
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
	BuildContextDir              string
	MakeTunnelControlClient      func(grpc.ClientConnInterface) dcptunproto.TunnelControlClient
	MaxTunnelPreparationAttempts uint32
}

type LogicalContainerTunnelProxyReconciler struct {
	*ReconcilerBase[apiv2.LogicalContainerTunnelProxy, *apiv2.LogicalContainerTunnelProxy]
	config  LogicalContainerTunnelProxyReconcilerConfig
	proxies *ObjectStateMap[types.UID, logicalTunnelProxyData, *logicalTunnelProxyData, *apiv2.LogicalContainerTunnelProxy]
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
	if config.MaxTunnelPreparationAttempts == 0 {
		config.MaxTunnelPreparationAttempts = defaultMaxTunnelPreparationAttempts
	}
	return &LogicalContainerTunnelProxyReconciler{
		ReconcilerBase: NewReconcilerBase[apiv2.LogicalContainerTunnelProxy](client, reader, log, ctx),
		config:         config, proxies: NewObjectStateMap[types.UID, logicalTunnelProxyData, *logicalTunnelProxyData, *apiv2.LogicalContainerTunnelProxy](),
	}
}

func (r *LogicalContainerTunnelProxyReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	for _, child := range []ctrl_client.Object{&apiv2.PhysicalContainer{}, &apiv2.PhysicalContainerImage{}, &apiv2.PhysicalProcess{}, &apiv2.ServiceEndpoint{}} {
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
		Owns(&apiv2.PhysicalProcess{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
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
	data.reconcileErr = nil
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
	if data.control != nil {
		if closeErr := data.control.Close(); closeErr != nil {
			return closeErr
		}
	}
	r.proxies.DeleteByNamespacedName(name)
	logger.ReleaseResourceLog((&apiv2.LogicalContainerTunnelProxy{ObjectMeta: metav1.ObjectMeta{UID: uid}}).GetResourceId())
	return nil
}

func handleLogicalTunnelProxyPending(
	ctx context.Context, r *LogicalContainerTunnelProxyReconciler, proxy *apiv2.LogicalContainerTunnelProxy,
	_ logicalTunnelProxyState, data *logicalTunnelProxyData, log logr.Logger,
) objectChange {
	data.reason, data.message = "", ""
	container, containerErr := r.findPhysicalContainer(ctx, proxy, data)
	image, imageErr := r.findPhysicalContainerImage(ctx, proxy, data)
	if errors.Join(containerErr, imageErr) != nil {
		return additionalReconciliationNeeded
	}
	change := applyLogicalTunnelProxyContainerStatus(proxy, container)
	change |= applyLogicalTunnelProxyImageStatus(proxy, image)
	if container == nil && data.containerUID != "" {
		data.message = "Waiting to observe the owned PhysicalContainer."
		return change | additionalReconciliationNeeded
	}
	ready, namespaceReason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, proxy.Namespace)
	if namespaceErr != nil {
		data.reconcileErr = namespaceErr
		return change | additionalReconciliationNeeded
	}
	if !ready {
		data.message = namespaceReadinessMessage(proxy.Namespace, namespaceReason)
		return change
	}
	baseImage := proxy.Spec.BaseImage
	if baseImage == "" {
		baseImage = dcptun.DefaultBaseImage
	}
	name := "logical-" + string(proxy.UID)
	if image == nil && (data.imageUID != "" || container != nil) {
		data.message = "Waiting to observe the owned PhysicalContainerImage."
		return change | additionalReconciliationNeeded
	}
	if image == nil {
		plan, planErr := dcptun.PrepareClientProxyImageBuild(ctx, r.config.BuildContextDir, baseImage)
		if planErr != nil {
			data.reconcileErr = planErr
			log.Error(planErr, "Failed to prepare tunnel image build context")
			return change | additionalReconciliationNeeded
		}
		desiredImage := &apiv2.PhysicalContainerImage{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: proxy.Namespace},
			Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
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
			} else {
				data.reconcileErr = createErr
			}
			return change | additionalReconciliationNeeded
		}
		image = createdImage
		change |= applyLogicalTunnelProxyImageStatus(proxy, image)
		removeUnusedTunnelProxyBuildContext(plan.BuildContextArchive.Source, createdImage, log)
	}
	if container == nil {
		network := &apiv2.LogicalContainerNetwork{}
		networkName := commonapi.AsNamespacedName(proxy.Spec.NetworkRef, proxy.Namespace)
		networkErr := r.Get(ctx, networkName, network)
		if networkErr != nil {
			if apierrors.IsNotFound(networkErr) {
				data.message = fmt.Sprintf("LogicalContainerNetwork %q does not exist.", networkName)
				return change
			}
			data.reconcileErr = networkErr
			log.Error(networkErr, "Failed to read tunnel proxy logical network")
			return change | additionalReconciliationNeeded
		}
		if network.Status.Phase == apiv2.LogicalContainerNetworkPhaseFailed {
			data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart,
				fmt.Sprintf("LogicalContainerNetwork %q failed: %s", networkName, physicalResourceStatusMessage(network.Status.Conditions)))
			return change | additionalReconciliationNeeded
		}
		if !network.DeletionTimestamp.IsZero() {
			data.message = fmt.Sprintf("LogicalContainerNetwork %q is terminating.", networkName)
			return change
		}
		if network.Status.PhysicalNetworkRef == "" {
			data.message = fmt.Sprintf("LogicalContainerNetwork %q has not published its backing physical network reference.", networkName)
			return change
		}
		if !data.security.HasCompleteCertificateData() {
			security, securityErr := dcptun.NewTunnelProxySecurityConfig()
			if securityErr != nil {
				data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart, securityErr.Error())
				return change | additionalReconciliationNeeded
			}
			data.security = security
		}
		desiredContainer := &apiv2.PhysicalContainer{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: proxy.Namespace},
			Spec: apiv2.PhysicalContainerSpec{Container: &apiv2.PhysicalContainerConfig{
				ImageRef: image.GetName(), ContainerName: "tunnel-" + string(proxy.UID), Entrypoint: dcptun.ClientProxyBinaryPath,
				Command: append([]string{"client", "--client-control-address", networking.IPv4AllInterfaceAddress,
					"--client-control-port", strconv.Itoa(dcptun.DefaultContainerProxyControlPort),
					"--client-data-address", networking.IPv4AllInterfaceAddress, "--client-data-port", strconv.Itoa(dcptun.DefaultContainerProxyDataPort)}, tunnelProxySecurityArgs(data.security)...),
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
			} else {
				data.reconcileErr = createErr
			}
			return change | additionalReconciliationNeeded
		}
		container = createdContainer
	}
	if data.imageUID == "" {
		data.message = "Waiting to observe the owned PhysicalContainerImage."
		return change | additionalReconciliationNeeded
	}
	if !observeTunnelContainer(container, data) {
		if data.state != logicalTunnelProxyPending {
			change |= additionalReconciliationNeeded
		}
		return change
	}
	server, processErr := r.findPhysicalProcess(ctx, proxy, data)
	if processErr != nil {
		return change | additionalReconciliationNeeded
	}
	if server != nil {
		change |= applyLogicalTunnelProxyProcessStatus(proxy, server)
		data.state, data.reason, data.message = logicalTunnelProxyStarting, "", ""
		return change
	}
	if data.processUID != "" {
		data.message = "Waiting to observe the owned PhysicalProcess."
		return change | additionalReconciliationNeeded
	}
	controlPort, controlPortErr := physicalContainerHostPort(container.Status.PortMappings, dcptun.DefaultContainerProxyControlPort)
	dataPort, dataPortErr := physicalContainerHostPort(container.Status.PortMappings, dcptun.DefaultContainerProxyDataPort)
	if errors.Join(controlPortErr, dataPortErr) != nil {
		data.message = "Waiting for client proxy container port mappings."
		return change | additionalReconciliationNeeded
	}
	if data.control == nil {
		connection, connectionErr := dcptun.NewReverseControlConnection(r.LifetimeCtx, data.security, func() { r.ScheduleReconciliation(proxy.NamespacedName()) }, log)
		if connectionErr != nil {
			data.reconcileErr = connectionErr
			log.Error(connectionErr, "Failed to open reverse tunnel control listener")
			return change | additionalReconciliationNeeded
		}
		data.control = connection
	}
	executablePath, pathErr := dcppaths.GetDcpExePath()
	if pathErr != nil {
		data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart, pathErr.Error())
		return change | additionalReconciliationNeeded
	}
	process := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(proxy.UID), Namespace: proxy.Namespace},
		Spec: apiv2.PhysicalProcessSpec{Process: &apiv2.PhysicalProcessConfig{
			ExecutablePath: executablePath,
			Args: append([]string{"tunnel-server", "--control-callback", data.control.Address(),
				networking.IPv4LocalhostDefaultAddress, strconv.Itoa(int(controlPort)),
				networking.IPv4LocalhostDefaultAddress, strconv.Itoa(int(dataPort))}, tunnelProxySecurityArgs(data.security)...),
			Env: []commonapi.EnvVar{{Name: logger.DCP_LOG_SESSION_ID, Value: logger.SessionId()}},
		}},
	}
	_, createErr := createLogicalBacking(ctx, r.Client, r.NoCacheClient, proxy, process, physicalProcessFinalizer,
		&data.processUID, &data.unobservedChildren, func(existing *apiv2.PhysicalProcess) error {
			if !reflect.DeepEqual(existing.Spec, process.Spec) {
				return fmt.Errorf("physical process %q has a different spec", existing.NamespacedName())
			}
			return nil
		})
	if createErr != nil {
		if apierrors.IsAlreadyExists(createErr) {
			data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart, createErr.Error())
		} else {
			data.reconcileErr = createErr
		}
		log.Error(createErr, "Failed to declare tunnel server process")
		return change | additionalReconciliationNeeded
	}
	data.state, data.reason, data.message = logicalTunnelProxyStarting, "", ""
	return change
}

func tunnelProxySecurityArgs(security dcptun.TunnelProxySecurityConfig) []string {
	return []string{"--ca-cert", security.CACertBase64, "--server-cert", security.ServerCertBase64, "--server-key", security.ServerKeyBase64}
}

func (r *LogicalContainerTunnelProxyReconciler) findPhysicalContainer(
	ctx context.Context, proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData,
) (*apiv2.PhysicalContainer, error) {
	var children apiv2.PhysicalContainerList
	listErr := r.List(ctx, &children, ctrl_client.InNamespace(proxy.Namespace),
		ctrl_client.MatchingFields{logicalTunnelProxyOwnerUIDField: string(proxy.UID)})
	if listErr != nil {
		lookupErr := fmt.Errorf("list owned physical containers: %w", listErr)
		data.reason, data.message, data.reconcileErr = apiv2.LogicalContainerTunnelProxyReasonLookupFailed, lookupErr.Error(), lookupErr
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
		if !data.security.HasCompleteCertificateData() && owned.Spec.Container != nil {
			for index := 0; index+1 < len(owned.Spec.Container.Command); index++ {
				switch owned.Spec.Container.Command[index] {
				case "--ca-cert":
					data.security.CACertBase64 = owned.Spec.Container.Command[index+1]
				case "--server-cert":
					data.security.ServerCertBase64 = owned.Spec.Container.Command[index+1]
				case "--server-key":
					data.security.ServerKeyBase64 = owned.Spec.Container.Command[index+1]
				}
			}
		}
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
		data.reason, data.message, data.reconcileErr = apiv2.LogicalContainerTunnelProxyReasonLookupFailed, lookupErr.Error(), lookupErr
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

func (r *LogicalContainerTunnelProxyReconciler) findPhysicalProcess(
	ctx context.Context, proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData,
) (*apiv2.PhysicalProcess, error) {
	var children apiv2.PhysicalProcessList
	listErr := r.List(ctx, &children, ctrl_client.InNamespace(proxy.Namespace),
		ctrl_client.MatchingFields{logicalTunnelProxyOwnerUIDField: string(proxy.UID)})
	if listErr != nil {
		lookupErr := fmt.Errorf("list owned physical processes: %w", listErr)
		data.reason, data.message, data.reconcileErr = apiv2.LogicalContainerTunnelProxyReasonLookupFailed, lookupErr.Error(), lookupErr
		return nil, lookupErr
	}
	var owned *apiv2.PhysicalProcess
	for index := range children.Items {
		child := &children.Items[index]
		if !metav1.IsControlledBy(child, proxy) {
			continue
		}
		if owned != nil {
			multipleErr := fmt.Errorf("more than one PhysicalProcess belongs to %q", proxy.NamespacedName())
			data.failMissing(multipleErr.Error())
			return nil, multipleErr
		}
		owned = child
	}
	if owned != nil {
		data.unobservedChildren.observeCached(owned)
		if (data.processUID != "" && owned.UID != data.processUID) || !hasFinalizer(owned, LogicalBackingFinalizer) {
			identityErr := fmt.Errorf("physical process %q UID or logical hold changed", owned.NamespacedName())
			data.failMissing(identityErr.Error())
			return nil, identityErr
		}
		data.processUID = owned.UID
	}
	return owned, nil
}

func tunnelProxyChildren(list ctrl_client.ObjectList) []ctrl_client.Object {
	var children []ctrl_client.Object
	switch typed := list.(type) {
	case *apiv2.PhysicalContainerList:
		for index := range typed.Items {
			children = append(children, &typed.Items[index])
		}
	case *apiv2.PhysicalContainerImageList:
		for index := range typed.Items {
			children = append(children, &typed.Items[index])
		}
	case *apiv2.PhysicalProcessList:
		for index := range typed.Items {
			children = append(children, &typed.Items[index])
		}
	}
	return children
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
	server, processErr := r.findPhysicalProcess(ctx, proxy, data)
	if processErr != nil {
		log.Error(processErr, "Failed to get starting tunnel server process")
		return change | additionalReconciliationNeeded
	}
	if server == nil {
		data.message = "Waiting to observe the owned PhysicalProcess."
		return change | additionalReconciliationNeeded
	}
	change |= applyLogicalTunnelProxyProcessStatus(proxy, server)
	if !server.DeletionTimestamp.IsZero() {
		data.failMissing("The owned PhysicalProcess is being deleted.")
		return change | additionalReconciliationNeeded
	}
	if data.control == nil {
		data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart, "Server process exists without its controller-owned callback listener.")
		return change | additionalReconciliationNeeded
	}
	switch server.Status.Phase {
	case apiv2.PhysicalProcessPhaseFailed, apiv2.PhysicalProcessPhaseExited:
		data.fail(apiv2.LogicalContainerTunnelProxyReasonFailedToStart,
			"Server process is unavailable: "+physicalResourceStatusMessage(server.Status.Conditions))
		return change | additionalReconciliationNeeded
	case apiv2.PhysicalProcessPhaseUnknown:
		data.reconcileErr = errors.New("server process state is unknown: " + physicalResourceStatusMessage(server.Status.Conditions))
	case apiv2.PhysicalProcessPhaseRunning:
		if data.control.Ready() {
			data.state, data.started = logicalTunnelProxyRunning, true
			return change | additionalReconciliationNeeded
		}
		data.message = "Waiting for the authenticated reverse tunnel control connection."
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
			data.reason = apiv2.LogicalContainerTunnelProxyReasonPaused
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
		data.reconcileErr = errors.New("client container state is unknown: " + physicalResourceStatusMessage(container.Status.Conditions))
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
	ctx context.Context, proxy *apiv2.LogicalContainerTunnelProxy, data *logicalTunnelProxyData,
) (objectChange, bool) {
	image, imageErr := r.findPhysicalContainerImage(ctx, proxy, data)
	if imageErr != nil {
		return noChange, false
	}
	container, containerErr := r.findPhysicalContainer(ctx, proxy, data)
	server, processErr := r.findPhysicalProcess(ctx, proxy, data)
	if errors.Join(containerErr, processErr) != nil {
		return noChange, false
	}
	if container == nil {
		data.failMissing("The owned PhysicalContainer no longer exists.")
		return noChange, false
	}
	if server == nil {
		data.failMissing("The owned PhysicalProcess no longer exists.")
		return noChange, false
	}
	change := applyLogicalTunnelProxyContainerStatus(proxy, container)
	change |= applyLogicalTunnelProxyImageStatus(proxy, image)
	change |= applyLogicalTunnelProxyProcessStatus(proxy, server)
	if image == nil {
		data.failMissing("The owned PhysicalContainerImage no longer exists.")
		return change, false
	}
	if !observeTunnelContainer(container, data) {
		return change, false
	}
	if !server.DeletionTimestamp.IsZero() {
		data.failMissing("The owned PhysicalProcess is being deleted.")
		return change, false
	}
	switch server.Status.Phase {
	case apiv2.PhysicalProcessPhaseExited:
		data.stopAfterExit(tunnelBackingExitMessage("Host server process", server.Status.ExitCode))
		return change, false
	case apiv2.PhysicalProcessPhaseFailed:
		data.failMissing("Host server process is unavailable: " + physicalResourceStatusMessage(server.Status.Conditions))
		return change, false
	case apiv2.PhysicalProcessPhaseRunning:
		return change, true
	case apiv2.PhysicalProcessPhaseUnknown:
		data.reconcileErr = errors.New("server process state is unknown: " + physicalResourceStatusMessage(server.Status.Conditions))
		return change, false
	default:
		return change, false
	}
}

func handleLogicalTunnelProxyRunning(
	ctx context.Context, r *LogicalContainerTunnelProxyReconciler, proxy *apiv2.LogicalContainerTunnelProxy,
	_ logicalTunnelProxyState, data *logicalTunnelProxyData, log logr.Logger,
) objectChange {
	data.reason, data.message = "", ""
	change, available := r.observeTunnelPair(ctx, proxy, data)
	if !available {
		if data.state == logicalTunnelProxyRunning && data.reason == apiv2.LogicalContainerTunnelProxyReasonPaused {
			return change
		}
		if endpointErr := syncOwnedServiceEndpoints(ctx, r.Client, r.NoCacheClient, proxy, logicalTunnelProxyOwnerUIDField, &data.unobservedChildren, nil); endpointErr != nil {
			log.Error(endpointErr, "Failed to withdraw unavailable tunnel endpoints")
		}
		return change | additionalReconciliationNeeded
	}
	if data.control == nil || !data.control.Ready() {
		data.reconcileErr = errors.New("waiting for authenticated reverse tunnel control connection")
		return change | additionalReconciliationNeeded
	}
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
	if data.control != nil {
		if closeErr := data.control.Close(); closeErr != nil {
			log.Error(closeErr, "Failed to close terminal tunnel control connection")
		}
		data.control = nil
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
	for _, list := range []ctrl_client.ObjectList{&apiv2.PhysicalProcessList{}, &apiv2.PhysicalContainerList{}} {
		if listErr := r.List(ctx, list, ctrl_client.InNamespace(proxy.Namespace), ctrl_client.MatchingFields{logicalTunnelProxyOwnerUIDField: string(proxy.UID)}); listErr != nil {
			log.Error(listErr, "Failed to list terminal tunnel backings")
			change |= additionalReconciliationNeeded
			continue
		}
		for _, child := range tunnelProxyChildren(list) {
			if !metav1.IsControlledBy(child, proxy) || !child.GetDeletionTimestamp().IsZero() {
				continue
			}
			patch := ctrl_client.MergeFromWithOptions(child.DeepCopyObject().(ctrl_client.Object), ctrl_client.MergeFromWithOptimisticLock{})
			switch typed := child.(type) {
			case *apiv2.PhysicalContainer:
				if typed.Spec.Stop {
					continue
				}
				typed.Spec.Stop = true
			case *apiv2.PhysicalProcess:
				if typed.Spec.Stop {
					continue
				}
				typed.Spec.Stop = true
			}
			if stopErr := r.Patch(ctx, child, patch); stopErr != nil {
				log.Error(stopErr, "Failed to stop terminal tunnel backing")
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
	server, processErr := r.findPhysicalProcess(ctx, proxy, data)
	if errors.Join(containerErr, processErr) != nil {
		return change | additionalReconciliationNeeded
	}
	if data.state != logicalTunnelProxyStopping {
		return change | additionalReconciliationNeeded
	}
	change |= applyLogicalTunnelProxyContainerStatus(proxy, container)
	change |= applyLogicalTunnelProxyProcessStatus(proxy, server)
	containerStopped := container == nil || container.Status.Phase == apiv2.PhysicalContainerPhaseExited || container.Status.Phase == apiv2.PhysicalContainerPhaseFailed
	serverStopped := server == nil || server.Status.Phase == apiv2.PhysicalProcessPhaseExited || server.Status.Phase == apiv2.PhysicalProcessPhaseFailed
	if containerStopped && serverStopped {
		data.state, data.reason = logicalTunnelProxyExited, apiv2.LogicalContainerTunnelProxyReasonCompleted
	}
	return change
}

// handleLogicalTunnelProxyUnknown restarts discovery from Pending after an invalid in-memory state.
func handleLogicalTunnelProxyUnknown(
	ctx context.Context, r *LogicalContainerTunnelProxyReconciler, proxy *apiv2.LogicalContainerTunnelProxy,
	state logicalTunnelProxyState, data *logicalTunnelProxyData, log logr.Logger,
) objectChange {
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
	client := r.config.MakeTunnelControlClient(data.control.Connection)
	desired := make(map[string]apiv2.LogicalContainerTunnel)
	for _, tunnel := range proxy.Spec.Tunnels {
		desired[tunnel.Name] = tunnel
	}
	change := noChange
	for name, record := range data.tunnels {
		if _, wanted := desired[name]; wanted {
			continue
		}
		if record.status.TunnelID != 0 {
			if deleteErr := deleteLogicalTunnel(ctx, client, record.status.TunnelID); deleteErr != nil {
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
				if deleteErr := deleteLogicalTunnel(ctx, client, record.status.TunnelID); deleteErr != nil {
					log.Error(deleteErr, "Failed to delete unavailable tunnel", "Tunnel", tunnel.Name)
					change |= additionalReconciliationNeeded
					record.status.State = apiv2.LogicalContainerTunnelStateNotReady
				} else {
					record.status = apiv2.LogicalContainerTunnelStatus{Name: tunnel.Name, State: apiv2.LogicalContainerTunnelStateNotReady, Timestamp: metav1.NowMicro()}
					record.attempts = 0
				}
			}
			if record.status.State == apiv2.LogicalContainerTunnelStateNotReady && serverErr == nil && !time.Now().Before(record.retryAfter) {
				if record.status.TunnelID != 0 {
					if deleteErr := deleteLogicalTunnel(ctx, client, record.status.TunnelID); deleteErr != nil {
						log.Error(deleteErr, "Failed to retire previous tunnel")
						data.tunnels[tunnel.Name] = record
						change |= additionalReconciliationNeeded
						continue
					}
					record.status.TunnelID = 0
				}
				record.attempts++
				record.retryAfter = time.Now().Add(delayDuration(StandardDelay))
				if record.attempts > r.config.MaxTunnelPreparationAttempts {
					record.status.State, record.status.ErrorMessage = apiv2.LogicalContainerTunnelStateFailed, "Maximum tunnel preparation attempts reached."
				} else {
					operationCtx, cancel := context.WithTimeout(ctx, tunnelOperationTimeout)
					request := &dcptunproto.TunnelReq{ServerAddress: stdproto.String(server.Address), ServerPort: stdproto.Int32(server.Port), ClientProxyPort: stdproto.Int32(server.Port)}
					prepared, prepareErr := client.PrepareTunnel(operationCtx, request)
					if prepareErr != nil {
						request.ClientProxyPort = nil
						prepared, prepareErr = client.PrepareTunnel(operationCtx, request)
					}
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
						record.server, record.attempts = server, 0
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

func deleteLogicalTunnel(ctx context.Context, client dcptunproto.TunnelControlClient, id uint32) error {
	operationCtx, cancel := context.WithTimeout(ctx, tunnelOperationTimeout)
	defer cancel()
	_, deleteErr := client.DeleteTunnel(operationCtx, &dcptunproto.TunnelRef{TunnelId: stdproto.Uint32(id)})
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
		for _, child := range []commonapi.DcpModelObject{&apiv2.PhysicalContainer{}, &apiv2.PhysicalContainerImage{}, &apiv2.PhysicalProcess{}} {
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
	if data.state == logicalTunnelProxyRunning {
		data.state, data.reason, data.message = logicalTunnelProxyStopping, "", "Stopping the tunnel proxy pair for deletion."
	}
	data.reconcileErr = nil
	if data.control != nil {
		if closeErr := data.control.Close(); closeErr != nil {
			log.Error(closeErr, "Failed to close deleting tunnel control connection")
		}
		data.control = nil
	}
	endpointsComplete, endpointErr := cleanupLogicalEndpoints(ctx, r.Client, r.NoCacheClient, proxy, logicalTunnelProxyOwnerUIDField, data.unobservedChildren)
	if endpointErr != nil {
		log.Error(endpointErr, "Failed to clean up tunnel endpoints")
		data.reconcileErr = endpointErr
	}
	waiting := !endpointsComplete || endpointErr != nil
	workloadsComplete := true
	for _, list := range []ctrl_client.ObjectList{&apiv2.PhysicalProcessList{}, &apiv2.PhysicalContainerList{}, &apiv2.PhysicalContainerImageList{}} {
		_, isImage := list.(*apiv2.PhysicalContainerImageList)
		if isImage && !workloadsComplete {
			waiting = true
			continue
		}
		listErr := r.List(ctx, list, ctrl_client.InNamespace(proxy.Namespace), ctrl_client.MatchingFields{logicalTunnelProxyOwnerUIDField: string(proxy.UID)})
		if listErr == nil {
			switch typed := list.(type) {
			case *apiv2.PhysicalProcessList:
				listErr = resolveUnobservedLogicalChildren(ctx, r.NoCacheClient, proxy, data.unobservedChildren, &typed.Items)
			case *apiv2.PhysicalContainerList:
				listErr = resolveUnobservedLogicalChildren(ctx, r.NoCacheClient, proxy, data.unobservedChildren, &typed.Items)
			case *apiv2.PhysicalContainerImageList:
				listErr = resolveUnobservedLogicalChildren(ctx, r.NoCacheClient, proxy, data.unobservedChildren, &typed.Items)
			}
		}
		if listErr != nil {
			data.reconcileErr = listErr
			log.Error(listErr, "Failed to discover tunnel backings for deletion")
			waiting = true
			if !isImage {
				workloadsComplete = false
			}
			continue
		}
		for _, child := range tunnelProxyChildren(list) {
			if !metav1.IsControlledBy(child, proxy) {
				continue
			}
			waiting = true
			if !isImage {
				workloadsComplete = false
			}
			if child.GetDeletionTimestamp().IsZero() {
				uid := child.GetUID()
				if deleteErr := r.Delete(ctx, child, ctrl_client.Preconditions{UID: &uid}); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
					data.reconcileErr = deleteErr
					log.Error(deleteErr, "Failed to delete tunnel backing")
				}
				continue
			}
			finalizer := physicalProcessFinalizer
			switch child.(type) {
			case *apiv2.PhysicalContainer:
				finalizer = physicalContainerFinalizer
			case *apiv2.PhysicalContainerImage:
				finalizer = physicalContainerImageFinalizer
			}
			if !hasFinalizer(child, finalizer) {
				if releaseErr := ReleaseLogicalBackingFinalizer(ctx, r.Client, child); releaseErr != nil && !apierrors.IsNotFound(releaseErr) {
					data.reconcileErr = releaseErr
					log.Error(releaseErr, "Failed to release logical tunnel backing hold")
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
		data.reconcileErr = discardErr
		return additionalReconciliationNeeded, delay
	}
	data = nil
	return deleteFinalizer(proxy, logicalTunnelProxyFinalizer, log), delay
}
