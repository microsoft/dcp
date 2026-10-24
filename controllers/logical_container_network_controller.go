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
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcpclient"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/internal/version"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

var (
	logicalContainerNetworkFinalizer    = fmt.Sprintf("%s/logicalcontainernetwork-reconciler", apiv2.GroupVersion.Group)
	logicalContainerNetworkDataHandlers = map[logicalContainerNetworkState]logicalContainerNetworkDataHandlerFunc{
		logicalContainerNetworkStateNamespace: handleLogicalContainerNetworkNamespace,
		logicalContainerNetworkStateLifecycle: handleLogicalContainerNetworkLifecycle,
		logicalContainerNetworkStateLookup:    handleLogicalContainerNetworkLookup,
		logicalContainerNetworkStatePhysical:  handleLogicalContainerNetworkPhysical,
		logicalContainerNetworkStateRuntime:   handleLogicalContainerNetworkRuntime,
		logicalContainerNetworkStateDelete:    handleLogicalContainerNetworkDeleting,
		logicalContainerNetworkStateInvalid:   handleUnknownLogicalContainerNetworkState,
		0:                                     handleUnknownLogicalContainerNetworkState,
	}
)

type logicalContainerNetworkDataHandlerFunc = stateInitializerFunc[
	apiv2.LogicalContainerNetwork, *apiv2.LogicalContainerNetwork,
	LogicalContainerNetworkReconciler, *LogicalContainerNetworkReconciler,
	logicalContainerNetworkState, logicalContainerNetworkData, *logicalContainerNetworkData,
]

type LogicalContainerNetworkReconcilerConfig struct {
	StateStore         *statestore.Store
	ResourceLeaseOwner process.ProcessHandle
	WorkloadID         commonapi.WorkloadID
}

type LogicalContainerNetworkReconciler struct {
	*ReconcilerBase[apiv2.LogicalContainerNetwork, *apiv2.LogicalContainerNetwork]

	networkInspector containers.InspectNetworks
	runtimeName      string
	harvester        *resourceHarvester
	config           LogicalContainerNetworkReconcilerConfig
	networkData      *ObjectStateMap[types.UID, logicalContainerNetworkData, *logicalContainerNetworkData, *apiv2.LogicalContainerNetwork]
	operationQueue   *resiliency.WorkQueue
}

func NewLogicalContainerNetworkReconciler(
	lifetimeCtx context.Context, client ctrl_client.Client, noCacheClient ctrl_client.Reader,
	log logr.Logger, orchestrator containers.ContainerOrchestrator,
	harvester *resourceHarvester, config LogicalContainerNetworkReconcilerConfig,
) *LogicalContainerNetworkReconciler {
	return &LogicalContainerNetworkReconciler{
		ReconcilerBase:   NewReconcilerBase[apiv2.LogicalContainerNetwork](client, noCacheClient, log, lifetimeCtx),
		networkInspector: orchestrator, runtimeName: orchestrator.Name(), harvester: harvester, config: config,
		networkData:    NewObjectStateMap[types.UID, logicalContainerNetworkData, *logicalContainerNetworkData, *apiv2.LogicalContainerNetwork](),
		operationQueue: resiliency.NewWorkQueue(lifetimeCtx, MaxConcurrentReconciles),
	}
}

func (r *LogicalContainerNetworkReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.LogicalContainerNetwork{}).
		Owns(&apiv2.PhysicalContainerNetwork{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.LogicalContainerNetworkList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *LogicalContainerNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)
	if ctx.Err() != nil {
		return ctrl.Result{}, nil
	}
	var network apiv2.LogicalContainerNetwork
	getErr := reader.Get(ctx, req.NamespacedName, &network)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			r.discardLogicalContainerNetworkData(req.NamespacedName, log)
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to Get() the LogicalContainerNetwork")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)
	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, network.GetResourceId())
	stateKey, previousData := r.networkData.BorrowByNamespacedName(req.NamespacedName)
	if previousData != nil && stateKey != network.UID {
		r.discardLogicalContainerNetworkData(req.NamespacedName, log)
	}
	r.networkData.RunDeferredOps(req.NamespacedName, &network)
	stateKey, data := r.networkData.BorrowByNamespacedName(req.NamespacedName)
	if data == nil {
		stateKey = network.UID
		data = &logicalContainerNetworkData{state: logicalContainerNetworkStateNamespace, progress: physicalResourceProgressNotReady}
		r.networkData.Store(req.NamespacedName, stateKey, data.Clone())
	}
	deleting := !network.DeletionTimestamp.IsZero()
	if deleting && data.operation != nil && data.state != logicalContainerNetworkStateDelete {
		data.operation.cancel()
	}
	patch := ctrl_client.MergeFromWithOptions(network.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	delay := StandardDelay
	finalizerAdded := false
	if deleting && data.operation == nil {
		change = r.handleDeletionRequest(ctx, &network, data, log)
	} else if change = ensureFinalizer(&network, logicalContainerNetworkFinalizer, log); change != noChange {
		finalizerAdded = true
	} else {
		stateHandler := getStateInitializer(logicalContainerNetworkDataHandlers, data.state, log)
		change = stateHandler(ctx, r, &network, data.state, data, log)
	}
	if !finalizerAdded && hasFinalizer(&network, logicalContainerNetworkFinalizer) {
		_ = r.networkData.Update(req.NamespacedName, stateKey, data)
		dataChange, projectionDelay, valid := data.applyTo(&network)
		change |= dataChange
		delay = projectionDelay
		if !valid {
			log.Error(fmt.Errorf("invalid logical network state %v with progress %v", data.state, data.progress),
				"LogicalContainerNetwork reached invalid reconciliation state")
		}
	}
	if !hasFinalizer(&network, logicalContainerNetworkFinalizer) {
		defer logger.ReleaseResourceLog(network.GetResourceId())
	}
	return r.SaveChangesWithDelay(ctx, &network, patch, change, delay, nil, log)
}

func handleLogicalContainerNetworkNamespace(
	ctx context.Context, reconciler *LogicalContainerNetworkReconciler, network *apiv2.LogicalContainerNetwork,
	_ logicalContainerNetworkState, data *logicalContainerNetworkData, log logr.Logger,
) objectChange {
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, reconciler.Client, network.Namespace)
	if !namespaceReady {
		data.progress, data.reason = physicalResourceProgressNotReady, namespaceReason
		data.message = namespaceReadinessMessage(network.Namespace, namespaceReason)
		if namespaceReason == apiv2.PhysicalResourceReasonNamespaceNotFound {
			data.progress = physicalResourceProgressNotFound
		}
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get namespace", "Namespace", network.Namespace)
			data.progress = physicalResourceProgressRetryPending
			data.message = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
		}
		return noChange
	}
	if network.Spec.EffectiveMode() == apiv2.LogicalContainerNetworkModeExisting {
		return reconciler.scheduleLogicalContainerNetworkOperation(network, data, logicalContainerNetworkStateLookup, reconciler.lookupLogicalContainerNetwork, log)
	}
	return reconciler.scheduleLogicalContainerNetworkOperation(network, data, logicalContainerNetworkStateLifecycle, reconciler.prepareLogicalContainerNetworkLifecycle, log)
}

func handleLogicalContainerNetworkLifecycle(
	_ context.Context, reconciler *LogicalContainerNetworkReconciler, network *apiv2.LogicalContainerNetwork,
	_ logicalContainerNetworkState, data *logicalContainerNetworkData, log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalResourceProgressInProgress:
		return noChange
	case physicalResourceProgressFailed:
		if data.lease != nil {
			return reconciler.scheduleLogicalContainerNetworkLeaseSettlement(network, data, nil, log)
		}
		return noChange
	case physicalResourceProgressCompleted:
		if network.Spec.EffectiveMode() == apiv2.LogicalContainerNetworkModePersistent {
			return reconciler.scheduleLogicalContainerNetworkOperation(network, data, logicalContainerNetworkStateLookup, reconciler.lookupLogicalContainerNetwork, log)
		}
		data.state, data.progress = logicalContainerNetworkStatePhysical, physicalResourceProgressNotReady
		data.reason, data.message = "", ""
		return additionalReconciliationNeeded
	default:
		if time.Now().Before(data.retryAfter) {
			return additionalReconciliationNeeded
		}
		return reconciler.scheduleLogicalContainerNetworkOperation(network, data, logicalContainerNetworkStateLifecycle, reconciler.prepareLogicalContainerNetworkLifecycle, log)
	}
}

func handleLogicalContainerNetworkLookup(
	_ context.Context, reconciler *LogicalContainerNetworkReconciler, network *apiv2.LogicalContainerNetwork,
	_ logicalContainerNetworkState, data *logicalContainerNetworkData, log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalResourceProgressInProgress:
		return noChange
	case physicalResourceProgressCompleted:
		data.state, data.progress = logicalContainerNetworkStatePhysical, physicalResourceProgressNotReady
		data.reason, data.message = "", ""
		return additionalReconciliationNeeded
	default:
		if time.Now().Before(data.retryAfter) {
			return additionalReconciliationNeeded
		}
		return reconciler.scheduleLogicalContainerNetworkOperation(network, data, logicalContainerNetworkStateLookup, reconciler.lookupLogicalContainerNetwork, log)
	}
}

func handleLogicalContainerNetworkPhysical(
	ctx context.Context, reconciler *LogicalContainerNetworkReconciler, network *apiv2.LogicalContainerNetwork,
	_ logicalContainerNetworkState, data *logicalContainerNetworkData, log logr.Logger,
) objectChange {
	if data.operation != nil {
		return noChange
	}
	if data.progress == physicalResourceProgressFailed {
		if data.lease != nil {
			return reconciler.scheduleLogicalContainerNetworkLeaseSettlement(network, data, nil, log)
		}
		return noChange
	}
	data.progress, data.reason, data.message = physicalResourceProgressNotReady, "", ""
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, reconciler.Client, network.Namespace)
	if !namespaceReady {
		data.reason, data.message = namespaceReason, namespaceReadinessMessage(network.Namespace, namespaceReason)
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get namespace before preparing physical network")
			data.progress = physicalResourceProgressRetryPending
			data.message = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
		}
		return noChange
	}
	if data.lease != nil {
		verifyErr := reconciler.config.StateStore.VerifyResourceLeaseHeld(ctx, network, reconciler.config.ResourceLeaseOwner)
		if verifyErr != nil {
			log.Error(verifyErr, "Failed to verify persistent network startup lease")
			data.progress, data.reason = physicalResourceProgressFailed, "ResourceLeaseLost"
			data.message = fmt.Sprintf("Persistent network startup lease was lost: %v", verifyErr)
			return noChange
		}
	}
	physicalSpec := logicalContainerNetworkPhysicalSpec(network, data)
	physical := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(network.UID), Namespace: network.Namespace},
		Spec:       physicalSpec,
	}
	ownerErr := ctrl.SetControllerReference(network, physical, reconciler.Scheme())
	if ownerErr != nil {
		log.Error(ownerErr, "Failed to set physical network owner")
		data.progress = physicalResourceProgressFailed
		data.message = fmt.Sprintf("Failed to set physical network owner: %v", ownerErr)
		return noChange
	}
	createErr := reconciler.Create(ctx, physical)
	if apierrors.IsAlreadyExists(createErr) {
		getErr := reconciler.NoCacheClient.Get(ctx, physical.NamespacedName(), physical)
		if getErr != nil {
			createErr = getErr
		} else if !metav1.IsControlledBy(physical, network) || !reflect.DeepEqual(physical.Spec, physicalSpec) {
			createErr = fmt.Errorf("physical network %q has a different owner or spec", physical.NamespacedName())
			data.progress = physicalResourceProgressFailed
		} else {
			createErr = nil
		}
	}
	if createErr != nil {
		log.Error(createErr, "Failed to prepare backing physical network")
		if data.progress != physicalResourceProgressFailed {
			data.progress = physicalResourceProgressRetryPending
		}
		data.message = fmt.Sprintf("Failed to prepare backing physical network: %v", createErr)
		return noChange
	}
	data.physicalNetworkUID = physical.UID
	data.pendingNetworkID = ""
	data.replaceExisting = false
	data.state, data.progress = logicalContainerNetworkStateRuntime, physicalResourceProgressNotReady
	return handleLogicalContainerNetworkRuntime(ctx, reconciler, network, data.state, data, log)
}

func logicalContainerNetworkPhysicalSpec(
	network *apiv2.LogicalContainerNetwork, data *logicalContainerNetworkData,
) apiv2.PhysicalContainerNetworkSpec {
	selectedID := data.pendingNetworkID
	if data.replaceExisting {
		selectedID = ""
	}
	physicalSpec := apiv2.PhysicalContainerNetworkSpec{NetworkID: selectedID}
	if selectedID == "" {
		desired := network.Spec.Network.DeepCopy()
		labels := setRuntimeLabel(desired.Labels, dcpBuildLabel, version.ProductVersion)
		labels = setRuntimeLabel(labels, groupVersionLabel, apiv2.GroupVersion.String())
		labels = setRuntimeLabel(labels, nameLabel, network.Name)
		labels = setRuntimeLabel(labels, lifecycleKeyLabel, data.lifecycleKey)
		runtimeName := network.Spec.NetworkName
		if runtimeName == "" {
			runtimeName = data.generatedNetworkName
		}
		physicalSpec.Network = &apiv2.PhysicalContainerNetworkConfig{
			NetworkName: runtimeName, IPv6: desired.IPv6, Labels: labels,
			RetainRuntimeNetwork: network.Spec.EffectiveMode() == apiv2.LogicalContainerNetworkModePersistent,
			ReplaceExisting:      data.replaceExisting,
		}
	}
	return physicalSpec
}

func handleLogicalContainerNetworkRuntime(
	ctx context.Context, reconciler *LogicalContainerNetworkReconciler, network *apiv2.LogicalContainerNetwork,
	_ logicalContainerNetworkState, data *logicalContainerNetworkData, log logr.Logger,
) objectChange {
	physical := &apiv2.PhysicalContainerNetwork{}
	physicalName := types.NamespacedName{Namespace: network.Namespace, Name: "logical-" + string(network.UID)}
	getErr := reconciler.Get(ctx, physicalName, physical)
	if apierrors.IsNotFound(getErr) {
		getErr = reconciler.NoCacheClient.Get(ctx, physicalName, physical)
	}
	if apierrors.IsNotFound(getErr) {
		data.progress, data.reason, data.message = physicalResourceProgressMissing, "", "The backing PhysicalContainerNetwork no longer exists."
		change := applyLogicalContainerNetworkPhysicalDetails(network, &apiv2.PhysicalContainerNetworkStatus{})
		if data.lease != nil {
			return change | reconciler.scheduleLogicalContainerNetworkLeaseSettlement(network, data, nil, log)
		}
		return change
	}
	if getErr != nil {
		log.Error(getErr, "Failed to get backing physical network")
		data.progress, data.reason = physicalResourceProgressRetryPending, ""
		data.message = fmt.Sprintf("Failed to get backing PhysicalContainerNetwork: %v", getErr)
		return noChange
	}
	if physical.UID != data.physicalNetworkUID || !metav1.IsControlledBy(physical, network) {
		log.Error(fmt.Errorf("physical network %q identity or owner changed", physicalName), "LogicalContainerNetwork backing resource changed")
		data.progress, data.reason, data.message = physicalResourceProgressFailed, "PhysicalNetworkChanged", "The backing PhysicalContainerNetwork identity or owner changed."
		if data.lease != nil {
			return reconciler.scheduleLogicalContainerNetworkLeaseSettlement(network, data, nil, log)
		}
		return noChange
	}
	change := applyLogicalContainerNetworkPhysicalDetails(network, &physical.Status)
	data.reason, data.message = "", ""
	if !physical.DeletionTimestamp.IsZero() {
		data.progress, data.reason, data.message = physicalResourceProgressUnknown, "PhysicalNetworkTerminating", "The backing PhysicalContainerNetwork is being deleted."
		return change
	}
	ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	if ready == nil || ready.ObservedGeneration != physical.Generation {
		data.progress, data.message = physicalResourceProgressNotReady, "Waiting for the physical controller to observe the desired network configuration."
		return change
	}
	data.progress = physicalResourceProgressNotReady
	switch physical.Status.Phase {
	case apiv2.PhysicalContainerNetworkPhaseReady:
		if ready.Status == metav1.ConditionTrue && physical.Status.NetworkID != "" {
			data.progress = physicalResourceProgressCompleted
		}
	case apiv2.PhysicalContainerNetworkPhaseFailed:
		data.progress = physicalResourceProgressFailed
	case apiv2.PhysicalContainerNetworkPhaseUnknown:
		data.progress = physicalResourceProgressUnknown
	}
	if data.progress != physicalResourceProgressCompleted {
		data.message = fmt.Sprintf("PhysicalContainerNetwork reports %s: %s", ready.Reason, ready.Message)
	}
	if data.lease != nil && (data.progress == physicalResourceProgressCompleted || data.progress == physicalResourceProgressFailed) {
		var record *statestore.PersistentNetworkRecord
		if data.progress == physicalResourceProgressCompleted {
			record = &statestore.PersistentNetworkRecord{
				ResourceKey: network.GetLeaseKey(), NetworkID: physical.Status.NetworkID, NetworkName: physical.Status.NetworkName,
				RuntimeName: reconciler.runtimeName,
			}
		}
		return change | reconciler.scheduleLogicalContainerNetworkLeaseSettlement(network, data, record, log)
	}
	return change
}

func applyLogicalContainerNetworkPhysicalDetails(network *apiv2.LogicalContainerNetwork, physical *apiv2.PhysicalContainerNetworkStatus) objectChange {
	change := setValue(&network.Status.NetworkID, physical.NetworkID)
	change |= setValue(&network.Status.NetworkName, physical.NetworkName)
	change |= setValue(&network.Status.Driver, physical.Driver)
	change |= setValue(&network.Status.IPv6, physical.IPv6)
	change |= setValue(&network.Status.CreatedAt, physical.CreatedAt)
	if !slices.Equal(network.Status.Subnets, physical.Subnets) {
		network.Status.Subnets = slices.Clone(physical.Subnets)
		change |= statusChanged
	}
	if !slices.Equal(network.Status.Gateways, physical.Gateways) {
		network.Status.Gateways = slices.Clone(physical.Gateways)
		change |= statusChanged
	}
	if !slices.Equal(network.Status.ContainerIDs, physical.ContainerIDs) {
		network.Status.ContainerIDs = slices.Clone(physical.ContainerIDs)
		change |= statusChanged
	}
	return change
}

func handleUnknownLogicalContainerNetworkState(
	_ context.Context, _ *LogicalContainerNetworkReconciler, _ *apiv2.LogicalContainerNetwork,
	state logicalContainerNetworkState, data *logicalContainerNetworkData, log logr.Logger,
) objectChange {
	log.Error(fmt.Errorf("unknown logical network state %v with progress %v", state, data.progress), "LogicalContainerNetwork has an invalid reconciliation state")
	data.state, data.progress = logicalContainerNetworkStateInvalid, physicalResourceProgressFailed
	data.reason, data.message = apiv2.PhysicalResourceReasonOperationStateInvalid, "Logical network reached an invalid reconciliation state."
	return noChange
}

func handleLogicalContainerNetworkDeleting(
	ctx context.Context, reconciler *LogicalContainerNetworkReconciler, network *apiv2.LogicalContainerNetwork,
	state logicalContainerNetworkState, data *logicalContainerNetworkData, log logr.Logger,
) objectChange {
	if data.operation == nil {
		return handleUnknownLogicalContainerNetworkState(ctx, reconciler, network, state, data, log)
	}
	return additionalReconciliationNeeded
}

func (r *LogicalContainerNetworkReconciler) handleDeletionRequest(
	ctx context.Context, network *apiv2.LogicalContainerNetwork, data *logicalContainerNetworkData, log logr.Logger,
) objectChange {
	data.state, data.progress = logicalContainerNetworkStateDelete, physicalResourceProgressNotReady
	data.reason, data.message = "", ""
	physical := &apiv2.PhysicalContainerNetwork{}
	physicalName := types.NamespacedName{Namespace: network.Namespace, Name: "logical-" + string(network.UID)}
	getErr := r.NoCacheClient.Get(ctx, physicalName, physical)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		log.Error(getErr, "Failed to get physical network for deletion")
		data.progress, data.message = physicalResourceProgressRetryPending, getErr.Error()
		return noChange
	}
	if getErr == nil {
		if !metav1.IsControlledBy(physical, network) || (data.physicalNetworkUID != "" && data.physicalNetworkUID != physical.UID) {
			ownershipErr := fmt.Errorf("backing physical network %q has a different owner or identity", physicalName)
			log.Error(ownershipErr, "Failed to delete backing physical network")
			data.progress, data.message = physicalResourceProgressRetryPending, ownershipErr.Error()
			return noChange
		}
		if data.lease != nil {
			if data.physicalNetworkUID == "" {
				if !reflect.DeepEqual(physical.Spec, logicalContainerNetworkPhysicalSpec(network, data)) {
					specErr := fmt.Errorf("backing physical network %q has a different spec", physicalName)
					log.Error(specErr, "Failed to settle backing physical network ownership")
					data.progress, data.message = physicalResourceProgressRetryPending, specErr.Error()
					return noChange
				}
				data.physicalNetworkUID = physical.UID
				data.pendingNetworkID = ""
				data.replaceExisting = false
			}
			ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
			if ready == nil || ready.ObservedGeneration != physical.Generation {
				return additionalReconciliationNeeded
			}
			if physical.Status.Phase == apiv2.PhysicalContainerNetworkPhaseFailed {
				return r.scheduleLogicalContainerNetworkLeaseSettlement(network, data, nil, log)
			}
			if physical.Status.Phase == apiv2.PhysicalContainerNetworkPhasePending &&
				ready.Reason == string(apiv2.PhysicalResourceReasonNamespaceTerminating) && physical.Status.NetworkID == "" {
				// Namespace termination prevents the physical controller from starting runtime work.
				return r.scheduleLogicalContainerNetworkLeaseSettlement(network, data, nil, log)
			}
			if physical.Status.Phase != apiv2.PhysicalContainerNetworkPhaseReady ||
				ready.Status != metav1.ConditionTrue || physical.Status.NetworkID == "" {
				return additionalReconciliationNeeded
			}
			record := &statestore.PersistentNetworkRecord{
				ResourceKey: network.GetLeaseKey(), NetworkID: physical.Status.NetworkID, NetworkName: physical.Status.NetworkName,
				RuntimeName: r.runtimeName,
			}
			return r.scheduleLogicalContainerNetworkLeaseSettlement(network, data, record, log)
		}
		if physical.DeletionTimestamp.IsZero() {
			uid := physical.UID
			deleteErr := r.Delete(ctx, physical, ctrl_client.Preconditions{UID: &uid})
			if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				log.Error(deleteErr, "Failed to delete backing physical network")
				data.progress, data.message = physicalResourceProgressRetryPending, deleteErr.Error()
				return noChange
			}
		}
		return additionalReconciliationNeeded
	}
	if data.lease != nil {
		return r.scheduleLogicalContainerNetworkLeaseSettlement(network, data, nil, log)
	}
	r.networkData.DeleteByNamespacedName(network.NamespacedName())
	return deleteFinalizer(network, logicalContainerNetworkFinalizer, log)
}

func (r *LogicalContainerNetworkReconciler) discardLogicalContainerNetworkData(name types.NamespacedName, log logr.Logger) {
	resourceUID, data := r.networkData.BorrowByNamespacedName(name)
	if data == nil {
		return
	}
	discarded := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{UID: resourceUID}}
	defer logger.ReleaseResourceLog(discarded.GetResourceId())
	if data.operation != nil {
		data.operation.cancel()
	}
	r.networkData.DeleteByNamespacedName(name)
	if data.lease != nil {
		enqueueErr := r.operationQueue.Enqueue(func(cleanupCtx context.Context) {
			releaseErr := data.lease.Release(cleanupCtx)
			if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
				log.Error(releaseErr, "Failed to release discarded logical network lease")
			}
		})
		if enqueueErr != nil {
			log.Error(enqueueErr, "Failed to queue discarded logical network lease release")
		}
	}
}

func (r *LogicalContainerNetworkReconciler) scheduleLogicalContainerNetworkOperation(
	network *apiv2.LogicalContainerNetwork, currentData *logicalContainerNetworkData, state logicalContainerNetworkState,
	work func(context.Context, *apiv2.LogicalContainerNetwork, *logicalContainerNetworkData, logr.Logger), log logr.Logger,
) objectChange {
	operationCtx, cancelOperation := context.WithCancel(r.LifetimeCtx)
	data := currentData.Clone()
	if state == logicalContainerNetworkStatePersistence {
		data.leaseFailureMessage = ""
	} else {
		data.state, data.progress = state, physicalResourceProgressInProgress
		data.reason, data.message = "", ""
	}
	data.retryAfter = time.Time{}
	data.operation = &logicalContainerNetworkOperation{cancel: cancelOperation}
	if !r.networkData.Update(network.NamespacedName(), network.UID, data) {
		cancelOperation()
		return additionalReconciliationNeeded
	}
	currentData.UpdateFrom(data)
	snapshot, result := network.DeepCopy(), data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(context.Context) {
		defer cancelOperation()
		if operationCtx.Err() != nil {
			if state == logicalContainerNetworkStatePersistence {
				result.leaseFailureMessage = operationCtx.Err().Error()
			} else {
				result.progress, result.message = physicalResourceProgressRetryPending, operationCtx.Err().Error()
			}
		} else {
			work(operationCtx, snapshot, result, log)
		}
		operation := result.operation
		result.operation = nil
		queued := r.networkData.QueueDeferredOpForStateKey(snapshot.NamespacedName(), snapshot.UID,
			func(name types.NamespacedName, stateKey types.UID, current *apiv2.LogicalContainerNetwork) {
				_, active := r.networkData.BorrowByNamespacedName(name)
				if current.UID == snapshot.UID && active != nil && active.operation == operation {
					if state == logicalContainerNetworkStatePersistence {
						active.operation, active.lease = nil, result.lease
						active.leaseFailureMessage, active.retryAfter = result.leaseFailureMessage, result.retryAfter
						_ = r.networkData.Update(name, stateKey, active)
					} else {
						_ = r.networkData.Update(name, stateKey, result)
					}
				}
			})
		if queued {
			r.ScheduleReconciliation(snapshot.NamespacedName())
		} else if result.lease != nil {
			releaseErr := result.lease.Release(r.LifetimeCtx)
			if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
				log.Error(releaseErr, "Failed to release stale logical network operation lease")
			}
		}
	})
	if enqueueErr != nil {
		cancelOperation()
		log.Error(enqueueErr, "Failed to queue logical network operation", "State", state)
		data.operation = nil
		if state == logicalContainerNetworkStatePersistence {
			data.leaseFailureMessage = fmt.Sprintf("Failed to queue persistent network settlement: %v", enqueueErr)
		} else {
			data.progress, data.message = physicalResourceProgressRetryPending, fmt.Sprintf("Failed to queue logical network operation: %v", enqueueErr)
		}
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		currentData.UpdateFrom(data)
	}
	return noChange
}

func (r *LogicalContainerNetworkReconciler) prepareLogicalContainerNetworkLifecycle(
	ctx context.Context, network *apiv2.LogicalContainerNetwork, data *logicalContainerNetworkData, log logr.Logger,
) {
	if network.Spec.NetworkName == "" && data.generatedNetworkName == "" {
		generatedName, _, nameErr := MakeUniqueName(network.Name)
		if nameErr != nil {
			log.Error(nameErr, "Failed to generate logical network runtime name")
			data.progress, data.message = physicalResourceProgressRetryPending, fmt.Sprintf("Failed to generate network name: %v", nameErr)
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			return
		}
		data.generatedNetworkName = generatedName
	}
	lifecycleKey, _, keyErr := network.Spec.GetLifecycleKey()
	if keyErr != nil {
		log.Error(keyErr, "Failed to calculate logical network lifecycle key")
		data.progress, data.reason, data.message = physicalResourceProgressFailed, "LifecycleKeyCalculationFailed", keyErr.Error()
		return
	}
	data.lifecycleKey = lifecycleKey
	if network.Spec.EffectiveMode() == apiv2.LogicalContainerNetworkModePersistent && data.lease == nil {
		if r.config.StateStore == nil {
			storeErr := errors.New("state store is not configured")
			log.Error(storeErr, "Cannot prepare persistent logical network")
			data.progress, data.reason, data.message = physicalResourceProgressFailed, "StateStoreNotConfigured", storeErr.Error()
			return
		}
		lease, leaseErr := r.config.StateStore.AcquireResourceLease(
			ctx, network, r.config.ResourceLeaseOwner, resourceLeaseRevalidationInterval, statestore.ExclusiveResourceLease,
		)
		if leaseErr != nil {
			data.progress, data.reason = physicalResourceProgressRetryPending, "ResourceLeaseAcquisitionFailed"
			data.message = fmt.Sprintf("Failed to acquire persistent network startup lease: %v", leaseErr)
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			if errors.Is(leaseErr, statestore.ErrResourceLeaseHeld) {
				logResourceLeaseHeld(log, leaseErr, network.GetLeaseKey(), "Persistent network startup lease is held, retrying")
				data.reason = "ResourceLeaseHeld"
			} else {
				log.Error(leaseErr, "Failed to acquire persistent logical network lease")
			}
			return
		}
		data.lease = lease
	}
	data.progress = physicalResourceProgressCompleted
}

func (r *LogicalContainerNetworkReconciler) lookupLogicalContainerNetwork(
	ctx context.Context, network *apiv2.LogicalContainerNetwork, data *logicalContainerNetworkData, log logr.Logger,
) {
	if !r.harvester.TryProtectNetwork(ctx, network.Spec.NetworkName) {
		data.progress, data.reason = physicalResourceProgressRetryPending, "NetworkHarvestingInProgress"
		data.message = "Waiting for resource harvesting before selecting the runtime network."
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return
	}
	inspected, inspectErr := inspectNetworkIfExists(ctx, r.networkInspector, network.Spec.NetworkName)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		data.pendingNetworkID, data.replaceExisting = "", false
		if network.Spec.EffectiveMode() == apiv2.LogicalContainerNetworkModeExisting {
			data.progress = physicalResourceProgressNotFound
			data.message = fmt.Sprintf("Runtime network %q was not found.", network.Spec.NetworkName)
			data.retryAfter = time.Now().Add(delayDurations[MonitoringDelay].Duration)
		} else {
			data.progress = physicalResourceProgressCompleted
		}
		return
	}
	if inspectErr != nil || inspected == nil || inspected.Id == "" {
		if inspectErr == nil {
			inspectErr = errors.New("network inspection returned no runtime identity")
		}
		log.Error(inspectErr, "Failed to inspect logical network by name")
		data.progress, data.message = physicalResourceProgressRetryPending, fmt.Sprintf("Failed to look up runtime network: %v", inspectErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return
	}
	_, managed := inspected.Labels[dcpBuildLabel]
	actualKey := inspected.Labels[lifecycleKeyLabel]
	if network.Spec.EffectiveMode() == apiv2.LogicalContainerNetworkModeExisting || !managed || actualKey == data.lifecycleKey {
		data.pendingNetworkID = inspected.Id
		data.lifecycleKey, data.replaceExisting = actualKey, false
		log.V(1).Info("Reusing existing runtime network", "NetworkID", GetShortId(inspected.Id))
	} else {
		data.pendingNetworkID = ""
		data.replaceExisting = true
		log.Info("Delegating persistent network replacement to physical controller", "NetworkID", GetShortId(inspected.Id))
	}
	data.progress = physicalResourceProgressCompleted
}

func (r *LogicalContainerNetworkReconciler) scheduleLogicalContainerNetworkLeaseSettlement(
	network *apiv2.LogicalContainerNetwork, data *logicalContainerNetworkData,
	record *statestore.PersistentNetworkRecord, log logr.Logger,
) objectChange {
	if data.operation != nil {
		return noChange
	}
	if data.leaseFailureMessage != "" && time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	return r.scheduleLogicalContainerNetworkOperation(network, data, logicalContainerNetworkStatePersistence,
		func(ctx context.Context, snapshot *apiv2.LogicalContainerNetwork, result *logicalContainerNetworkData, operationLog logr.Logger) {
			r.persistLogicalContainerNetwork(ctx, snapshot, result, record, operationLog)
		}, log)
}

func (r *LogicalContainerNetworkReconciler) persistLogicalContainerNetwork(
	ctx context.Context, network *apiv2.LogicalContainerNetwork, data *logicalContainerNetworkData,
	record *statestore.PersistentNetworkRecord, log logr.Logger,
) {
	if data.lease == nil {
		return
	}
	if record != nil {
		workloadID, resolveErr := dcpclient.ResolveNamespaceWorkloadID(ctx, r.NoCacheClient, network.Namespace, r.config.WorkloadID)
		if resolveErr != nil {
			log.Error(resolveErr, "Failed to resolve persistent network workload ownership")
			data.leaseFailureMessage = fmt.Sprintf("Failed to resolve persistent network workload ownership: %v", resolveErr)
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			return
		}
		if workloadID != "" {
			verifyErr := r.config.StateStore.VerifyResourceLeaseHeld(ctx, network, r.config.ResourceLeaseOwner)
			if verifyErr != nil {
				log.Error(verifyErr, "Failed to verify persistent network lease before recording ownership")
				data.leaseFailureMessage = fmt.Sprintf("Failed to verify persistent network lease: %v", verifyErr)
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				return
			}
			record.WorkloadID = workloadID
			recordErr := r.config.StateStore.UpsertPersistentNetwork(ctx, *record)
			if recordErr != nil {
				log.Error(recordErr, "Failed to record persistent logical network")
				data.leaseFailureMessage = fmt.Sprintf("Failed to record persistent network: %v", recordErr)
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				return
			}
		}
	}
	releaseErr := data.lease.Release(ctx)
	if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
		log.Error(releaseErr, "Failed to release persistent network startup lease")
		data.leaseFailureMessage = fmt.Sprintf("Failed to release persistent network lease: %v", releaseErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return
	}
	data.lease = nil
}
