/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
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
)

const logicalNetworkOwnerUIDField = ".metadata.logicalNetworkOwnerUID"

var (
	errMultipleLogicalNetworkBackings   = errors.New("multiple owned physical networks")
	logicalContainerNetworkFinalizer    = fmt.Sprintf("%s/logicalcontainernetwork-reconciler", apiv2.GroupVersion.Group)
	logicalContainerNetworkDataHandlers = map[logicalContainerNetworkState]logicalContainerNetworkDataHandlerFunc{
		logicalContainerNetworkStatePending:           handleLogicalContainerNetworkPending,
		logicalContainerNetworkStatePreparationFailed: handleLogicalContainerNetworkPending,
		logicalContainerNetworkStateCreating:          handleLogicalContainerNetworkRunning,
		logicalContainerNetworkStateReady:             handleLogicalContainerNetworkRunning,
		logicalContainerNetworkStateFailed:            handleLogicalContainerNetworkRunning,
		logicalContainerNetworkStateUnknown:           handleLogicalContainerNetworkRunning,
		logicalContainerNetworkStateInvalid:           handleUnknownLogicalContainerNetworkState,
		0:                                             handleUnknownLogicalContainerNetworkState,
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
}

func NewLogicalContainerNetworkReconciler(
	lifetimeCtx context.Context, client ctrl_client.Client, noCacheClient ctrl_client.Reader,
	log logr.Logger, orchestrator containers.ContainerOrchestrator,
	harvester *resourceHarvester, config LogicalContainerNetworkReconcilerConfig,
) *LogicalContainerNetworkReconciler {
	return &LogicalContainerNetworkReconciler{
		ReconcilerBase:   NewReconcilerBase[apiv2.LogicalContainerNetwork](client, noCacheClient, log, lifetimeCtx),
		networkInspector: orchestrator, runtimeName: orchestrator.Name(), harvester: harvester, config: config,
		networkData: NewObjectStateMap[types.UID, logicalContainerNetworkData, *logicalContainerNetworkData, *apiv2.LogicalContainerNetwork](),
	}
}

func (r *LogicalContainerNetworkReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, &apiv2.PhysicalContainerNetwork{}, logicalNetworkOwnerUIDField,
		func(obj ctrl_client.Object) []string {
			owner := metav1.GetControllerOf(obj)
			if owner == nil {
				return nil
			}
			return []string{string(owner.UID)}
		})
	if indexErr != nil {
		return fmt.Errorf("index logical network owner: %w", indexErr)
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.LogicalContainerNetwork{}).
		Owns(&apiv2.PhysicalContainerNetwork{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.LogicalContainerNetworkList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
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
			discardErr := r.discardLogicalContainerNetworkData(ctx, req.NamespacedName)
			if discardErr != nil {
				log.Error(discardErr, "Failed to discard logical network state")
				return ctrl.Result{}, discardErr
			}
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to Get() the LogicalContainerNetwork")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}

	getSucceededCounter.Add(ctx, 1)
	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, network.GetResourceId())

	_, data := r.networkData.BorrowByNamespacedName(req.NamespacedName)

	patch := ctrl_client.MergeFromWithOptions(network.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	delay := StandardDelay

	if !network.DeletionTimestamp.IsZero() {
		change, delay = r.handleDeletionRequest(ctx, &network, data, log)
	} else {
		change = ensureFinalizer(&network, logicalContainerNetworkFinalizer, log)
		if change == noChange {
			change, delay = r.manageLogicalContainerNetwork(ctx, &network, data, log)
		}
	}

	return r.SaveChangesWithDelay(ctx, &network, patch, change, delay, nil, log)
}

func (r *LogicalContainerNetworkReconciler) manageLogicalContainerNetwork(
	ctx context.Context, network *apiv2.LogicalContainerNetwork, data *logicalContainerNetworkData, log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	if data == nil {
		namespaceReady, namespaceReason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, network.Namespace)
		if !namespaceReady {
			change := noChange
			phase := apiv2.LogicalContainerNetworkPhasePending
			message := namespaceReadinessMessage(network.Namespace, namespaceReason)
			if namespaceErr != nil {
				log.Error(namespaceErr, "Failed to get namespace", "Namespace", network.Namespace)
				phase, message = apiv2.LogicalContainerNetworkPhaseUnknown, fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
				change |= additionalReconciliationNeeded
			}
			change |= setValue(&network.Status.Phase, phase)
			change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation,
				metav1.ConditionFalse, namespaceReason, message)
			return change, StandardDelay
		}
		data = &logicalContainerNetworkData{state: logicalContainerNetworkStatePending}
		r.networkData.Store(network.NamespacedName(), network.UID, data.Clone())
	}
	handler := getStateInitializer(logicalContainerNetworkDataHandlers, data.state, log)
	change := handler(ctx, r, network, data.state, data, log)
	_ = r.networkData.Update(network.NamespacedName(), network.UID, data)
	dataChange, delay, valid := data.applyTo(network)
	if !valid {
		log.Error(fmt.Errorf("invalid logical network state %v", data.state), "LogicalContainerNetwork reached invalid reconciliation state")
	}
	return change | dataChange, delay
}

func handleLogicalContainerNetworkPending(
	ctx context.Context, reconciler *LogicalContainerNetworkReconciler, network *apiv2.LogicalContainerNetwork,
	_ logicalContainerNetworkState, data *logicalContainerNetworkData, log logr.Logger,
) objectChange {
	if data.state == logicalContainerNetworkStatePreparationFailed {
		if data.lease != nil {
			return reconciler.settleLogicalNetworkLease(ctx, network, data, nil, log)
		}
		return noChange
	}
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	data.reconcileErr = nil
	data.reason, data.message, data.retryAfter = "", "", time.Time{}
	physical, lookupErr := reconciler.findLogicalNetworkBacking(ctx, network, reconciler.Client,
		ctrl_client.MatchingFields{logicalNetworkOwnerUIDField: string(network.UID)})
	if lookupErr != nil {
		log.Error(lookupErr, "Failed to discover backing physical network")
		data.reason, data.reconcileErr, data.message = "PhysicalNetworkPreparationFailed", lookupErr, lookupErr.Error()
		if errors.Is(lookupErr, errMultipleLogicalNetworkBackings) {
			data.state, data.reason, data.reconcileErr = logicalContainerNetworkStateInvalid, "MultiplePhysicalNetworks", nil
		}
		return additionalReconciliationNeeded
	}
	if physical != nil {
		if (data.physicalNetworkUID != "" && physical.UID != data.physicalNetworkUID) ||
			!hasFinalizer(physical, LogicalBackingFinalizer) {
			data.state, data.reason = logicalContainerNetworkStateInvalid, "PhysicalNetworkChanged"
			data.message = "The backing PhysicalContainerNetwork UID or logical hold changed."
			log.Error(errors.New(data.message), "Logical network backing changed")
			return noChange
		}
		data.physicalNetworkUID = physical.UID
		if physical.Spec.Network != nil {
			for _, label := range physical.Spec.Network.Labels {
				if label.Key == lifecycleKeyLabel {
					data.lifecycleKey = label.Value
				}
			}
		}
	} else if data.physicalNetworkUID != "" {
		data.message = "Waiting to observe the owned PhysicalContainerNetwork."
		return additionalReconciliationNeeded
	} else {
		mode := network.Spec.EffectiveMode()
		if mode == apiv2.LogicalContainerNetworkModePersistent && data.lease == nil {
			if reconciler.config.StateStore == nil {
				data.state, data.reason, data.message = logicalContainerNetworkStatePreparationFailed, "StateStoreNotConfigured", "State store is not configured."
				log.Error(errors.New(data.message), "Cannot prepare persistent logical network")
				return noChange
			}
			lease, leaseErr := reconciler.config.StateStore.AcquireResourceLease(ctx, network,
				reconciler.config.ResourceLeaseOwner, resourceLeaseRevalidationInterval, statestore.ExclusiveResourceLease)
			if leaseErr != nil {
				data.reason, data.reconcileErr, data.message = "ResourceLeaseAcquisitionFailed", leaseErr, leaseErr.Error()
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				if errors.Is(leaseErr, statestore.ErrResourceLeaseHeld) {
					logResourceLeaseHeld(log, leaseErr, network.GetLeaseKey(), "Persistent network creation lease is held, retrying")
					data.reason = "ResourceLeaseHeld"
				} else {
					log.Error(leaseErr, "Failed to acquire persistent logical network lease")
				}
				return additionalReconciliationNeeded
			}
			data.lease = lease
		}
		var inspected *containers.InspectedNetwork
		if mode != apiv2.LogicalContainerNetworkModeSession {
			if !reconciler.harvester.TryProtectNetwork(ctx, network.Spec.NetworkName) {
				data.reason, data.message = "NetworkHarvestingInProgress", "Waiting for resource harvesting before selecting the runtime network."
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				return additionalReconciliationNeeded
			}
			lookupCtx, cancelLookup := context.WithTimeout(ctx, networkInspectionTimeout)
			inspection, inspectErr := inspectNetworkIfExists(lookupCtx, reconciler.networkInspector, network.Spec.NetworkName)
			cancelLookup()
			if inspectErr == nil && (inspection == nil || inspection.Id == "") {
				inspectErr = errors.New("network inspection returned no runtime identity")
			}
			if inspectErr != nil && !errors.Is(inspectErr, containers.ErrNotFound) {
				log.Error(inspectErr, "Failed to inspect logical network by name")
				data.reason, data.reconcileErr, data.message = "NetworkLookupFailed", inspectErr, inspectErr.Error()
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				return additionalReconciliationNeeded
			}
			if inspectErr == nil {
				inspected = inspection
			}
		}
		if mode == apiv2.LogicalContainerNetworkModeExisting && inspected == nil {
			data.reason, data.message = "ExistingNetworkNotFound", fmt.Sprintf("Runtime network %q was not found.", network.Spec.NetworkName)
			data.retryAfter = time.Now().Add(delayDurations[MonitoringDelay].Duration)
			return additionalReconciliationNeeded
		}
		lifecycleKey, networkID, managed := "", "", false
		if inspected != nil {
			networkID, lifecycleKey = inspected.Id, inspected.Labels[lifecycleKeyLabel]
			_, managed = inspected.Labels[dcpBuildLabel]
		}
		replaceExisting := false
		if mode != apiv2.LogicalContainerNetworkModeExisting && (inspected == nil || managed) {
			desiredKey, _, keyErr := network.Spec.GetLifecycleKey()
			if keyErr != nil {
				log.Error(keyErr, "Failed to calculate logical network lifecycle key")
				data.state, data.reason, data.message = logicalContainerNetworkStatePreparationFailed, "LifecycleKeyCalculationFailed", keyErr.Error()
				return noChange
			}
			replaceExisting = inspected != nil && lifecycleKey != desiredKey
			lifecycleKey = desiredKey
			if replaceExisting {
				networkID = ""
			}
		}
		spec := apiv2.PhysicalContainerNetworkSpec{NetworkID: networkID}
		if networkID == "" {
			runtimeName := network.Spec.NetworkName
			if runtimeName == "" {
				generatedName, _, nameErr := MakeUniqueName(network.Name)
				if nameErr != nil {
					log.Error(nameErr, "Failed to generate logical network runtime name")
					data.reason, data.reconcileErr, data.message = "LifecyclePreparationFailed", nameErr, nameErr.Error()
					return additionalReconciliationNeeded
				}
				runtimeName = generatedName
			}
			desired := network.Spec.Network.DeepCopy()
			labels := append(desired.Labels,
				commonapi.Label{Key: dcpBuildLabel, Value: version.ProductVersion},
				commonapi.Label{Key: groupVersionLabel, Value: apiv2.GroupVersion.String()},
				commonapi.Label{Key: nameLabel, Value: network.Name},
				commonapi.Label{Key: lifecycleKeyLabel, Value: lifecycleKey},
			)
			spec.Network = &apiv2.PhysicalContainerNetworkConfig{
				NetworkName: runtimeName, IPv6: desired.IPv6, Labels: labels,
				RetainRuntimeNetwork: mode == apiv2.LogicalContainerNetworkModePersistent, ReplaceExisting: replaceExisting,
			}
		}
		if data.lease != nil {
			verifyErr := reconciler.config.StateStore.VerifyResourceLeaseHeld(ctx, network, reconciler.config.ResourceLeaseOwner)
			if verifyErr != nil {
				log.Error(verifyErr, "Failed to verify persistent network creation lease")
				data.state, data.reason, data.message = logicalContainerNetworkStatePreparationFailed, "ResourceLeaseLost", verifyErr.Error()
				return noChange
			}
		}
		created, createErr := reconciler.createLogicalNetworkBacking(ctx, network, spec)
		if created != nil && created.UID != "" {
			data.physicalNetworkUID, data.lifecycleKey = created.UID, lifecycleKey
		}
		if createErr != nil {
			log.Error(createErr, "Failed to prepare backing physical network")
			data.reconcileErr, data.message = createErr, createErr.Error()
			if errors.Is(createErr, errMultipleLogicalNetworkBackings) {
				data.state, data.reason, data.reconcileErr = logicalContainerNetworkStateInvalid, "MultiplePhysicalNetworks", nil
			}
			return additionalReconciliationNeeded
		}
		physical = created
		data.lifecycleKey = lifecycleKey
	}
	if physical.Spec.Network == nil && data.lease != nil {
		change := reconciler.settleLogicalNetworkLease(ctx, network, data, nil, log)
		if data.lease != nil {
			return change
		}
	}
	data.state = logicalContainerNetworkStateCreating
	return additionalReconciliationNeeded
}

func (r *LogicalContainerNetworkReconciler) createLogicalNetworkBacking(
	ctx context.Context, network *apiv2.LogicalContainerNetwork, spec apiv2.PhysicalContainerNetworkSpec,
) (*apiv2.PhysicalContainerNetwork, error) {
	physical := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(network.UID), Namespace: network.Namespace}, Spec: spec,
	}
	InitializeLogicalBackingFinalizers(physical, physicalContainerNetworkFinalizer)
	ownerErr := ctrl.SetControllerReference(network, physical, r.Scheme())
	if ownerErr != nil {
		return nil, fmt.Errorf("set physical network owner: %w", ownerErr)
	}
	createErr := r.Create(ctx, physical)
	if createErr != nil {
		existing, lookupErr := r.findLogicalNetworkBacking(ctx, network, r.NoCacheClient)
		if lookupErr != nil {
			return physical, fmt.Errorf("create physical network and verify owned resources: %w", errors.Join(createErr, lookupErr))
		}
		if existing == nil {
			return physical, createErr
		}
		return existing, nil
	}
	return physical, nil
}

func (r *LogicalContainerNetworkReconciler) findLogicalNetworkBacking(
	ctx context.Context, network *apiv2.LogicalContainerNetwork, reader ctrl_client.Reader, options ...ctrl_client.ListOption,
) (*apiv2.PhysicalContainerNetwork, error) {
	var children apiv2.PhysicalContainerNetworkList
	listOptions := append([]ctrl_client.ListOption{ctrl_client.InNamespace(network.Namespace)}, options...)
	listErr := reader.List(ctx, &children, listOptions...)
	if listErr != nil {
		return nil, fmt.Errorf("list owned physical networks: %w", listErr)
	}
	var owned *apiv2.PhysicalContainerNetwork
	for i := range children.Items {
		child := &children.Items[i]
		if !metav1.IsControlledBy(child, network) {
			continue
		}
		if owned != nil {
			return nil, fmt.Errorf("%w: more than one PhysicalContainerNetwork belongs to %q", errMultipleLogicalNetworkBackings, network.NamespacedName())
		}
		owned = child
	}
	return owned, nil
}

func handleLogicalContainerNetworkRunning(
	ctx context.Context, reconciler *LogicalContainerNetworkReconciler, network *apiv2.LogicalContainerNetwork,
	_ logicalContainerNetworkState, data *logicalContainerNetworkData, log logr.Logger,
) objectChange {
	data.reconcileErr = nil
	physical, lookupErr := reconciler.findLogicalNetworkBacking(ctx, network, reconciler.Client,
		ctrl_client.MatchingFields{logicalNetworkOwnerUIDField: string(network.UID)})
	if lookupErr != nil {
		log.Error(lookupErr, "Failed to observe backing physical network")
		data.reason, data.reconcileErr, data.message = "PhysicalNetworkLookupFailed", lookupErr, lookupErr.Error()
		if errors.Is(lookupErr, errMultipleLogicalNetworkBackings) {
			data.state, data.reason, data.reconcileErr = logicalContainerNetworkStateInvalid, "MultiplePhysicalNetworks", nil
		}
		return additionalReconciliationNeeded
	}
	if physical == nil {
		data.state, data.reason, data.message = logicalContainerNetworkStateCreating, "", "Waiting to observe the owned PhysicalContainerNetwork."
		return additionalReconciliationNeeded
	}
	if (data.physicalNetworkUID != "" && physical.UID != data.physicalNetworkUID) || !hasFinalizer(physical, LogicalBackingFinalizer) {
		data.state, data.reason, data.message = logicalContainerNetworkStateInvalid, "PhysicalNetworkChanged", "The backing PhysicalContainerNetwork UID or logical hold changed."
		log.Error(errors.New(data.message), "Logical network backing changed")
		if data.lease != nil {
			return reconciler.settleLogicalNetworkLease(ctx, network, data, nil, log)
		}
		return noChange
	}
	data.physicalNetworkUID = physical.UID
	ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	change := noChange
	if data.lease != nil && physical.Status.NetworkID != "" {
		var record *statestore.PersistentNetworkRecord
		if physical.Spec.Network != nil && physical.Status.Phase != apiv2.PhysicalContainerNetworkPhaseFailed &&
			(ready == nil || ready.Reason != string(apiv2.PhysicalContainerNetworkReasonRuntimeNetworkMissing)) {
			record = &statestore.PersistentNetworkRecord{
				ResourceKey: network.GetLeaseKey(), NetworkID: physical.Status.NetworkID,
				NetworkName: physical.Spec.Network.NetworkName, RuntimeName: reconciler.runtimeName,
			}
		}
		change |= reconciler.settleLogicalNetworkLease(ctx, network, data, record, log)
	}
	data.reason, data.message = "", ""
	switch physical.Status.Phase {
	case apiv2.PhysicalContainerNetworkPhaseReady:
		data.state = logicalContainerNetworkStateReady
		if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != physical.Generation || physical.Status.NetworkID == "" {
			data.state = logicalContainerNetworkStateCreating
		}
	case apiv2.PhysicalContainerNetworkPhaseFailed:
		data.state = logicalContainerNetworkStateFailed
	case apiv2.PhysicalContainerNetworkPhaseUnknown:
		data.state = logicalContainerNetworkStateUnknown
	default:
		data.state = logicalContainerNetworkStateCreating
	}
	if data.state != logicalContainerNetworkStateReady && ready != nil {
		data.message = fmt.Sprintf("PhysicalContainerNetwork reports %s: %s", ready.Reason, ready.Message)
	}
	if data.lease != nil && data.state == logicalContainerNetworkStateFailed && physical.Status.NetworkID == "" {
		change |= reconciler.settleLogicalNetworkLease(ctx, network, data, nil, log)
	}
	change |= setValue(&network.Status.PhysicalNetworkRef, physical.Name)
	return change | applyLogicalContainerNetworkPhysicalDetails(network, &physical.Status)
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
	ctx context.Context, reconciler *LogicalContainerNetworkReconciler, network *apiv2.LogicalContainerNetwork,
	state logicalContainerNetworkState, data *logicalContainerNetworkData, log logr.Logger,
) objectChange {
	if state == logicalContainerNetworkStateInvalid {
		if data.lease != nil {
			return reconciler.settleLogicalNetworkLease(ctx, network, data, nil, log)
		}
		return noChange
	}
	log.Error(fmt.Errorf("unknown logical network state %v", state), "LogicalContainerNetwork has an invalid reconciliation state")
	data.state, data.reason, data.message = logicalContainerNetworkStateInvalid, apiv2.PhysicalResourceReasonOperationStateInvalid, "Logical network reached an invalid reconciliation state."
	return noChange
}

func (r *LogicalContainerNetworkReconciler) handleDeletionRequest(
	ctx context.Context, network *apiv2.LogicalContainerNetwork, data *logicalContainerNetworkData, log logr.Logger,
) (change objectChange, delay AdditionalReconciliationDelay) {
	delay = StandardDelay
	if data == nil {
		data = &logicalContainerNetworkData{state: logicalContainerNetworkStateDelete}
		r.networkData.Store(network.NamespacedName(), network.UID, data.Clone())
	}
	defer func() {
		if data != nil {
			_ = r.networkData.Update(network.NamespacedName(), network.UID, data)
			dataChange, projectionDelay, valid := data.applyTo(network)
			change |= dataChange
			delay = projectionDelay
			if !valid {
				log.Error(fmt.Errorf("invalid logical network state %v", data.state), "LogicalContainerNetwork reached invalid reconciliation state")
			}
		}
	}()
	data.state, data.reason, data.message, data.reconcileErr = logicalContainerNetworkStateDelete, "", "", nil
	var children apiv2.PhysicalContainerNetworkList
	listErr := r.List(ctx, &children, ctrl_client.InNamespace(network.Namespace),
		ctrl_client.MatchingFields{logicalNetworkOwnerUIDField: string(network.UID)})
	if listErr != nil {
		log.Error(listErr, "Failed to list physical networks before deletion")
		data.reconcileErr, data.message = listErr, listErr.Error()
		return additionalReconciliationNeeded, delay
	}
	if len(children.Items) == 0 && data.physicalNetworkUID != "" {
		verifyErr := r.NoCacheClient.List(ctx, &children, ctrl_client.InNamespace(network.Namespace))
		if verifyErr != nil {
			log.Error(verifyErr, "Failed to verify physical network absence before deletion")
			data.reconcileErr, data.message = verifyErr, verifyErr.Error()
			return additionalReconciliationNeeded, delay
		}
		children.Items = slices.DeleteFunc(children.Items, func(child apiv2.PhysicalContainerNetwork) bool {
			return !metav1.IsControlledBy(&child, network)
		})
	}
	if len(children.Items) != 0 {
		if len(children.Items) == 1 && data.physicalNetworkUID == "" {
			data.physicalNetworkUID = children.Items[0].UID
		}
		waiting := false
		for i := range children.Items {
			physical := &children.Items[i]
			if physical.DeletionTimestamp.IsZero() {
				uid := physical.UID
				deleteErr := r.Delete(ctx, physical, ctrl_client.Preconditions{UID: &uid})
				if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
					log.Error(deleteErr, "Failed to request physical network deletion")
					data.reconcileErr, data.message = deleteErr, deleteErr.Error()
					waiting = true
				}
			}
			if physical.DeletionTimestamp.IsZero() || hasFinalizer(physical, physicalContainerNetworkFinalizer) {
				waiting = true
			}
		}
		if waiting {
			return additionalReconciliationNeeded, delay
		}
		if data.lease != nil {
			var record *statestore.PersistentNetworkRecord
			for i := range children.Items {
				physical := &children.Items[i]
				ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
				if physical.UID == data.physicalNetworkUID && physical.Spec.Network != nil && physical.Spec.Network.RetainRuntimeNetwork &&
					physical.Status.NetworkID != "" && ready != nil &&
					physical.Status.Phase != apiv2.PhysicalContainerNetworkPhaseFailed &&
					ready.Reason != string(apiv2.PhysicalContainerNetworkReasonRuntimeNetworkMissing) {
					record = &statestore.PersistentNetworkRecord{
						ResourceKey: network.GetLeaseKey(), NetworkID: physical.Status.NetworkID,
						NetworkName: physical.Spec.Network.NetworkName, RuntimeName: r.runtimeName,
					}
				}
			}
			return r.settleLogicalNetworkLease(ctx, network, data, record, log), delay
		}
		for i := range children.Items {
			physical := &children.Items[i]
			releaseErr := ReleaseLogicalBackingFinalizer(ctx, r.Client, physical)
			if releaseErr != nil && !apierrors.IsNotFound(releaseErr) {
				log.Error(releaseErr, "Failed to release logical network backing")
				data.reconcileErr, data.message = releaseErr, releaseErr.Error()
			}
		}
		return additionalReconciliationNeeded, delay
	}
	if data.lease != nil {
		return r.settleLogicalNetworkLease(ctx, network, data, nil, log), delay
	}
	discardErr := r.discardLogicalContainerNetworkData(ctx, network.NamespacedName())
	if discardErr != nil {
		log.Error(discardErr, "Failed to discard logical network state")
		data.reconcileErr, data.message = discardErr, discardErr.Error()
		return additionalReconciliationNeeded, delay
	}
	data = nil
	return deleteFinalizer(network, logicalContainerNetworkFinalizer, log), delay
}

func (r *LogicalContainerNetworkReconciler) discardLogicalContainerNetworkData(ctx context.Context, name types.NamespacedName) error {
	resourceUID, data := r.networkData.BorrowByNamespacedName(name)
	if data == nil {
		return nil
	}
	if data.lease != nil {
		releaseErr := data.lease.Release(ctx)
		if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
			return fmt.Errorf("release discarded logical network lease: %w", releaseErr)
		}
	}
	discarded := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{UID: resourceUID}}
	defer logger.ReleaseResourceLog(discarded.GetResourceId())
	r.networkData.DeleteByNamespacedName(name)
	return nil
}

func (r *LogicalContainerNetworkReconciler) settleLogicalNetworkLease(
	ctx context.Context, network *apiv2.LogicalContainerNetwork, data *logicalContainerNetworkData,
	record *statestore.PersistentNetworkRecord, log logr.Logger,
) objectChange {
	if data.lease == nil {
		return noChange
	}
	if data.leaseFailureMessage != "" && time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	data.leaseFailureMessage = ""
	if record != nil {
		workloadID, resolveErr := dcpclient.ResolveNamespaceWorkloadID(ctx, r.NoCacheClient, network.Namespace, r.config.WorkloadID)
		if resolveErr != nil {
			log.Error(resolveErr, "Failed to resolve persistent network workload ownership")
			data.leaseFailureMessage = fmt.Sprintf("Failed to resolve persistent network workload ownership: %v", resolveErr)
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			return additionalReconciliationNeeded
		}
		if workloadID != "" {
			verifyErr := r.config.StateStore.VerifyResourceLeaseHeld(ctx, network, r.config.ResourceLeaseOwner)
			if verifyErr != nil {
				log.Error(verifyErr, "Failed to verify persistent network lease before recording ownership")
				data.leaseFailureMessage = fmt.Sprintf("Failed to verify persistent network lease: %v", verifyErr)
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				return additionalReconciliationNeeded
			}
			record.WorkloadID = workloadID
			recordErr := r.config.StateStore.UpsertPersistentNetwork(ctx, *record)
			if recordErr != nil {
				log.Error(recordErr, "Failed to record persistent logical network")
				data.leaseFailureMessage = fmt.Sprintf("Failed to record persistent network: %v", recordErr)
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				return additionalReconciliationNeeded
			}
		}
	}
	releaseErr := data.lease.Release(ctx)
	if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
		log.Error(releaseErr, "Failed to release persistent network creation lease")
		data.leaseFailureMessage = fmt.Sprintf("Failed to release persistent network lease: %v", releaseErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return additionalReconciliationNeeded
	}
	data.lease, data.retryAfter = nil, time.Time{}
	return additionalReconciliationNeeded
}
