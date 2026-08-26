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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

var (
	physicalContainerNetworkFinalizer string = fmt.Sprintf("%s/physicalcontainernetwork-reconciler", apiv2.GroupVersion.Group)

	physicalContainerNetworkDataInitializers = map[apiv2.ConditionReason]physicalContainerNetworkDataInitializerFunc{
		apiv2.PhysicalContainerNetworkReasonCreating:                   handlePhysicalContainerNetworkCreating,
		apiv2.PhysicalContainerNetworkReasonCreated:                    handlePhysicalContainerNetworkCreated,
		apiv2.PhysicalContainerNetworkReasonCreateFailed:               handlePhysicalContainerNetworkCreateFailed,
		apiv2.PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable: handlePhysicalContainerNetworkBuiltInNetworkNotRemovable,
		apiv2.PhysicalContainerNetworkReasonReconciliationFailed:       handlePhysicalContainerNetworkRecoverableCreateFailed,
		"": handleUnknownPhysicalContainerNetworkDataReason,
	}
)

type physicalContainerNetworkDataInitializerFunc = stateInitializerFunc[
	apiv2.PhysicalContainerNetwork, *apiv2.PhysicalContainerNetwork,
	PhysicalContainerNetworkReconciler, *PhysicalContainerNetworkReconciler,
	apiv2.ConditionReason,
	physicalContainerNetworkData, *physicalContainerNetworkData,
]

type PhysicalContainerNetworkReconciler struct {
	*ReconcilerBase[apiv2.PhysicalContainerNetwork, *apiv2.PhysicalContainerNetwork]

	orchestrator   containers.NetworkAttachmentOrchestrator
	networkData    *ObjectStateMap[physicalContainerNetworkDataStateKey, physicalContainerNetworkData, *physicalContainerNetworkData, *apiv2.PhysicalContainerNetwork]
	operationQueue *resiliency.WorkQueue
}

func NewPhysicalContainerNetworkReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	orchestrator containers.NetworkAttachmentOrchestrator,
) *PhysicalContainerNetworkReconciler {
	return &PhysicalContainerNetworkReconciler{
		ReconcilerBase: NewReconcilerBase[apiv2.PhysicalContainerNetwork](client, noCacheClient, log, lifetimeCtx),
		orchestrator:   orchestrator,
		networkData:    NewObjectStateMap[physicalContainerNetworkDataStateKey, physicalContainerNetworkData, *physicalContainerNetworkData, *apiv2.PhysicalContainerNetwork](),
		operationQueue: resiliency.NewWorkQueue(lifetimeCtx, MaxConcurrentReconciles),
	}
}

func (r *PhysicalContainerNetworkReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.PhysicalContainerNetwork{}).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNamespace), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *PhysicalContainerNetworkReconciler) requestReconcileForNamespace(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	namespace := obj.(*apiv2.Namespace)
	var networkList apiv2.PhysicalContainerNetworkList
	listErr := r.List(ctx, &networkList, ctrl_client.InNamespace(namespace.Name))
	if listErr != nil {
		r.Log.Error(listErr, "Failed to list PhysicalContainerNetworks for namespace", "Namespace", namespace.Name)
		return nil
	}

	requests := make([]reconcile.Request, len(networkList.Items))
	for i := range networkList.Items {
		requests[i] = reconcile.Request{NamespacedName: networkList.Items[i].NamespacedName()}
	}

	r.Log.V(1).Info("Namespace updated, requesting PhysicalContainerNetwork reconciliation", "Namespace", namespace.Name, "Networks", len(requests))
	return requests
}

func (r *PhysicalContainerNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	network := apiv2.PhysicalContainerNetwork{}
	getErr := reader.Get(ctx, req.NamespacedName, &network)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			log.V(1).Info("PhysicalContainerNetwork not found, nothing to do...")
			// The finalizer normally guarantees the deletion is observed, but drop any lingering
			// state in case the object disappeared without it (for example a forced deletion).
			r.networkData.DeleteByNamespacedName(req.NamespacedName)
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}

		log.Error(getErr, "Failed to Get() the PhysicalContainerNetwork")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)

	r.networkData.RunDeferredOps(req.NamespacedName, &network)

	var change objectChange
	var onSuccessfulSave func()
	patch := ctrl_client.MergeFromWithOptions(network.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, &network, log)
	} else if change = ensureFinalizer(&network, physicalContainerNetworkFinalizer, log); change != noChange {
		// Make additional changes during the next reconciliation.
	} else {
		change, onSuccessfulSave = r.managePhysicalContainerNetwork(ctx, &network, log)
	}

	return r.SaveChangesWithDelay(ctx, &network, patch, change, physicalContainerNetworkReconcileDelay(&network), onSuccessfulSave, log)
}

// Chooses the cadence for the next reconciliation. Networks have no runtime event subscription,
// so every non-terminal phase keeps observing the runtime: an available network so that removal
// outside of DCP is noticed, and a recoverable failure so that reconciliation resumes once the
// runtime recovers. All delays carry jitter, so many networks do not poll the runtime in lockstep.
func physicalContainerNetworkReconcileDelay(network *apiv2.PhysicalContainerNetwork) AdditionalReconciliationDelay {
	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		return StandardDelay
	}

	switch network.Status.Phase {
	case apiv2.PhysicalContainerNetworkPhaseReady, apiv2.PhysicalContainerNetworkPhaseMissing:
		return MonitoringDelay
	case apiv2.PhysicalContainerNetworkPhaseFailed:
		if physicalContainerNetworkFailedTerminally(network) {
			return StandardDelay
		}
		// Retry sooner than steady-state monitoring, matching how V1 paces an unhealthy runtime.
		return LongDelay
	default:
		return StandardDelay
	}
}

// Acknowledges a completed create record once its result is durable in status.
func (r *PhysicalContainerNetworkReconciler) physicalContainerNetworkDataSaveCallback(
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	change objectChange,
) func() {
	if data == nil {
		return nil
	}

	switch data.conditionReason {
	case apiv2.PhysicalContainerNetworkReasonCreated:
		if data.networkID == "" {
			return nil
		}
	case apiv2.PhysicalContainerNetworkReasonCreateFailed:
	case apiv2.PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable:
	default:
		return nil
	}

	expectedReason := data.conditionReason
	expectedNetworkID := data.networkID
	expectedFailureMessage := data.failureMessage
	expectedRetryAfter := data.retryAfter
	return func() {
		r.networkData.DeleteByStateKeyIf(stateKey, func(current *physicalContainerNetworkData) bool {
			return current.conditionReason == expectedReason &&
				current.networkID == expectedNetworkID &&
				current.failureMessage == expectedFailureMessage &&
				current.retryAfter.Equal(expectedRetryAfter)
		})
	}
}

func (r *PhysicalContainerNetworkReconciler) managePhysicalContainerNetwork(
	ctx context.Context,
	network *apiv2.PhysicalContainerNetwork,
	log logr.Logger,
) (objectChange, func()) {
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, r.Client, network.Namespace)
	if !namespaceReady {
		namespacePhase := apiv2.PhysicalContainerNetworkPhasePending
		namespaceMessage := namespaceReadinessMessage(network.Namespace, namespaceReason)
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get namespace", "Namespace", network.Namespace)
			namespacePhase = apiv2.PhysicalContainerNetworkPhaseFailed
			namespaceMessage = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
		}
		change := setValue(&network.Status.Phase, namespacePhase)
		change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionFalse, namespaceReason, namespaceMessage)
		change |= additionalReconciliationNeeded
		return change, nil
	}

	change := noChange
	stateKey, data := r.networkData.BorrowByNamespacedName(network.NamespacedName())
	if data != nil {
		change |= data.applyTo(network)
		initializer := getStateInitializer(physicalContainerNetworkDataInitializers, data.conditionReason, log)
		change |= initializer(ctx, r, network, data.conditionReason, data, log)
		if data.conditionReason == apiv2.PhysicalContainerNetworkReasonCreated ||
			data.conditionReason == apiv2.PhysicalContainerNetworkReasonCreateFailed ||
			data.conditionReason == apiv2.PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable {
			return change, r.physicalContainerNetworkDataSaveCallback(stateKey, data, change)
		}
		return change, nil
	}

	if physicalContainerNetworkFailedTerminally(network) {
		return change, nil
	}

	networkID := network.Spec.NetworkID
	if networkID == "" {
		networkID = network.Status.NetworkID
	}
	if networkID == "" {
		return r.schedulePhysicalContainerNetworkCreate(network, log), nil
	}

	return change | r.applyRuntimeNetworkStatus(ctx, network, networkID, log), nil
}

// Reports whether the network recorded a terminal failure. The spec is immutable, so
// reconciliation cannot produce a different outcome.
func physicalContainerNetworkFailedTerminally(network *apiv2.PhysicalContainerNetwork) bool {
	if network.Status.Phase != apiv2.PhysicalContainerNetworkPhaseFailed {
		return false
	}

	readyCondition := apimeta.FindStatusCondition(network.Status.Conditions, string(apiv2.ConditionReady))
	if readyCondition == nil {
		return false
	}

	return readyCondition.Reason == string(apiv2.PhysicalContainerNetworkReasonCreateFailed) ||
		readyCondition.Reason == string(apiv2.PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable)
}

// Inspects the runtime network and projects the result onto the resource status.
func (r *PhysicalContainerNetworkReconciler) applyRuntimeNetworkStatus(
	ctx context.Context,
	network *apiv2.PhysicalContainerNetwork,
	networkID string,
	log logr.Logger,
) objectChange {
	inspectedNetwork, inspectErr := inspectPhysicalContainerNetwork(ctx, r.orchestrator, networkID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		change := setValue(&network.Status.NetworkID, networkID)
		change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseMissing)
		change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonRuntimeNetworkMissing, "Runtime network was not found.")
		// Keep observing: a tracked network may not have been created yet, and a runtime that is
		// only reporting the network as absent because it is unhealthy recovers on its own.
		return change | additionalReconciliationNeeded
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect runtime network", "NetworkID", networkID)
		change := setValue(&network.Status.NetworkID, networkID)
		change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseFailed)
		change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonReconciliationFailed, fmt.Sprintf("Failed to inspect runtime network: %v", inspectErr))
		// Inspection failures are usually transient, and repeating an identical failure produces
		// no status change, so retry explicitly rather than settling into a permanent failure.
		return change | additionalReconciliationNeeded
	}

	return applyReadyPhysicalContainerNetworkStatus(network, inspectedNetwork)
}

func (r *PhysicalContainerNetworkReconciler) schedulePhysicalContainerNetworkCreate(network *apiv2.PhysicalContainerNetwork, log logr.Logger) objectChange {
	networkConfig := network.Spec.Network
	stateKey := physicalContainerNetworkDataKey(network)
	data := &physicalContainerNetworkData{conditionReason: apiv2.PhysicalContainerNetworkReasonCreating}
	r.networkData.Store(network.NamespacedName(), stateKey, data)
	networkSnapshot := network.DeepCopy()
	dataSnapshot := data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.createPhysicalContainerNetwork(operationCtx, networkSnapshot, stateKey, dataSnapshot, log)
	})
	if enqueueErr != nil {
		r.networkData.DeleteByNamespacedName(network.NamespacedName())
		log.Error(enqueueErr, "Failed to queue PhysicalContainerNetwork create", "NetworkName", networkConfig.NetworkName)
		change := setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseFailed)
		change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonCreateFailed, fmt.Sprintf("Failed to queue runtime network create: %v", enqueueErr))
		return change
	}

	log.V(1).Info("Queued PhysicalContainerNetwork create", "NetworkName", networkConfig.NetworkName)
	return data.applyTo(network)
}

func (r *PhysicalContainerNetworkReconciler) createPhysicalContainerNetwork(
	ctx context.Context,
	network *apiv2.PhysicalContainerNetwork,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) {
	networkConfig := network.Spec.Network
	if networkConfig.ReplaceExisting {
		replaced, replaceErr := r.replacePhysicalContainerNetwork(ctx, network, data, log)
		if replaceErr != nil {
			log.Error(replaceErr, "Failed to replace existing runtime network", "NetworkName", networkConfig.NetworkName)
			data.conditionReason = apiv2.PhysicalContainerNetworkReasonReconciliationFailed
			data.failureMessage = fmt.Sprintf("Failed to replace existing runtime network: %v", replaceErr)
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			r.queuePhysicalContainerNetworkDataResult(network, stateKey, data)
			return
		}
		if !replaced {
			r.queuePhysicalContainerNetworkDataResult(network, stateKey, data)
			return
		}
	}

	networkID, createErr := r.orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name:   networkConfig.NetworkName,
		IPv6:   networkConfig.IPv6,
		Labels: physicalContainerNetworkCreationLabels(network, log),
	})
	r.applyPhysicalContainerNetworkCreateResult(ctx, network, data, networkID, createErr, log)
	r.queuePhysicalContainerNetworkDataResult(network, stateKey, data)
}

func (r *PhysicalContainerNetworkReconciler) replacePhysicalContainerNetwork(
	ctx context.Context,
	network *apiv2.PhysicalContainerNetwork,
	data *physicalContainerNetworkData,
	log logr.Logger,
) (bool, error) {
	networkConfig := network.Spec.Network
	inspectedNetwork, inspectErr := inspectPhysicalContainerNetwork(ctx, r.orchestrator, networkConfig.NetworkName)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		return true, nil
	}
	if inspectErr != nil {
		return false, fmt.Errorf("inspect runtime network %q: %w", networkConfig.NetworkName, inspectErr)
	}
	if inspectedNetwork.Id == "" {
		return false, fmt.Errorf("inspect runtime network %q returned an empty ID", networkConfig.NetworkName)
	}
	if r.orchestrator.IsBuiltInNetwork(inspectedNetwork.Name) {
		data.conditionReason = apiv2.PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable
		data.networkID = inspectedNetwork.Id
		data.failureMessage = fmt.Sprintf("Runtime network %q is built in and cannot be replaced.", inspectedNetwork.Name)
		data.retryAfter = time.Time{}
		return false, nil
	}
	if physicalContainerNetworkBelongsToResource(inspectedNetwork, network) {
		data.conditionReason = apiv2.PhysicalContainerNetworkReasonCreated
		data.networkID = inspectedNetwork.Id
		data.failureMessage = ""
		data.retryAfter = time.Time{}
		log.V(1).Info("Adopted runtime network created by an earlier attempt", "NetworkID", inspectedNetwork.Id)
		return false, nil
	}

	removeErr := r.removeRuntimeNetwork(ctx, inspectedNetwork.Id, log)
	if removeErr != nil {
		return false, fmt.Errorf("remove runtime network %q: %w", inspectedNetwork.Id, removeErr)
	}

	log.V(1).Info(
		"Removed existing runtime network before replacement",
		"NetworkID", inspectedNetwork.Id,
		"NetworkName", inspectedNetwork.Name,
	)
	return true, nil
}

func (r *PhysicalContainerNetworkReconciler) applyPhysicalContainerNetworkCreateResult(
	ctx context.Context,
	network *apiv2.PhysicalContainerNetwork,
	data *physicalContainerNetworkData,
	networkID string,
	createErr error,
	log logr.Logger,
) {
	networkConfig := network.Spec.Network
	if createErr != nil {
		log.Error(createErr, "Failed to create runtime network", "NetworkName", networkConfig.NetworkName)
		data.failureMessage = fmt.Sprintf("Failed to create runtime network: %v", createErr)
		inspectedNetwork, inspectErr := inspectPhysicalContainerNetwork(ctx, r.orchestrator, networkConfig.NetworkName)
		if inspectErr == nil &&
			networkConfig.ReplaceExisting &&
			r.orchestrator.IsBuiltInNetwork(inspectedNetwork.Name) {
			data.conditionReason = apiv2.PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable
			data.networkID = inspectedNetwork.Id
			data.failureMessage = fmt.Sprintf(
				"Runtime network %q is built in and cannot be replaced.",
				inspectedNetwork.Name,
			)
			data.retryAfter = time.Time{}
		} else if inspectErr == nil && physicalContainerNetworkBelongsToResource(inspectedNetwork, network) {
			data.conditionReason = apiv2.PhysicalContainerNetworkReasonCreated
			data.networkID = inspectedNetwork.Id
			data.failureMessage = ""
			data.retryAfter = time.Time{}
		} else if inspectErr == nil {
			if networkConfig.ReplaceExisting {
				data.conditionReason = apiv2.PhysicalContainerNetworkReasonReconciliationFailed
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			} else {
				data.conditionReason = apiv2.PhysicalContainerNetworkReasonCreateFailed
				data.retryAfter = time.Time{}
			}
		} else {
			if !errors.Is(inspectErr, containers.ErrNotFound) {
				data.failureMessage = fmt.Sprintf(
					"Failed to create runtime network: %v; failed to verify whether creation succeeded: %v",
					createErr,
					inspectErr,
				)
			}
			data.conditionReason = apiv2.PhysicalContainerNetworkReasonReconciliationFailed
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		}
	} else if networkID == "" {
		log.Error(errors.New("runtime network create succeeded without returning a network ID"), "Runtime network create succeeded without returning a network ID", "NetworkName", networkConfig.NetworkName)
		data.conditionReason = apiv2.PhysicalContainerNetworkReasonCreateFailed
		data.failureMessage = "Runtime network create succeeded without returning a network ID."
		data.retryAfter = time.Time{}
	} else {
		data.conditionReason = apiv2.PhysicalContainerNetworkReasonCreated
		data.networkID = networkID
		data.failureMessage = ""
		data.retryAfter = time.Time{}
	}
}

func (r *PhysicalContainerNetworkReconciler) queuePhysicalContainerNetworkDataResult(
	network *apiv2.PhysicalContainerNetwork,
	stateKey physicalContainerNetworkDataStateKey,
	result *physicalContainerNetworkData,
) {
	queued := r.networkData.QueueDeferredOpForStateKey(network.NamespacedName(), stateKey, func(name types.NamespacedName, currentStateKey physicalContainerNetworkDataStateKey, _ *apiv2.PhysicalContainerNetwork) {
		_ = r.networkData.Update(name, currentStateKey, result)
	})
	if queued {
		r.ScheduleReconciliation(network.NamespacedName())
	}
}

func handlePhysicalContainerNetworkCreating(
	_ context.Context,
	_ *PhysicalContainerNetworkReconciler,
	_ *apiv2.PhysicalContainerNetwork,
	_ apiv2.ConditionReason,
	_ *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	log.V(1).Info("Runtime network creation is still in progress")
	return noChange
}

func handlePhysicalContainerNetworkCreated(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	_ apiv2.ConditionReason,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	networkID := data.networkID
	log.V(1).Info("Runtime network created; saving network status", "NetworkID", networkID)
	return reconciler.applyRuntimeNetworkStatus(ctx, network, networkID, log)
}

func handlePhysicalContainerNetworkCreateFailed(
	_ context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	_ apiv2.ConditionReason,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	log.V(1).Info("Runtime network creation failed; saving network status", "Message", data.failureMessage)
	// The failure is terminal: spec is immutable, so no further reconciliation can make progress.
	return noChange
}

func handlePhysicalContainerNetworkBuiltInNetworkNotRemovable(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	_ apiv2.ConditionReason,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	log.V(1).Info("Built-in runtime network cannot be removed; saving network status", "NetworkID", data.networkID)
	inspectedNetwork, inspectErr := inspectPhysicalContainerNetwork(ctx, reconciler.orchestrator, data.networkID)
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect built-in runtime network", "NetworkID", data.networkID)
		return data.applyTo(network)
	}

	change := applyReadyPhysicalContainerNetworkStatus(network, inspectedNetwork)
	change &^= additionalReconciliationNeeded
	change |= data.applyTo(network)
	return change
}

func handlePhysicalContainerNetworkRecoverableCreateFailed(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	_ apiv2.ConditionReason,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	networkConfig := network.Spec.Network
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}

	inspectedNetwork, inspectErr := inspectPhysicalContainerNetwork(ctx, reconciler.orchestrator, networkConfig.NetworkName)
	if inspectErr == nil {
		if networkConfig.ReplaceExisting &&
			reconciler.orchestrator.IsBuiltInNetwork(inspectedNetwork.Name) {
			data.conditionReason = apiv2.PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable
			data.networkID = inspectedNetwork.Id
			data.failureMessage = fmt.Sprintf(
				"Runtime network %q is built in and cannot be replaced.",
				inspectedNetwork.Name,
			)
			data.retryAfter = time.Time{}
			stateKey, _ := reconciler.networkData.BorrowByNamespacedName(network.NamespacedName())
			if reconciler.networkData.Update(network.NamespacedName(), stateKey, data) {
				return data.applyTo(network)
			}
			return additionalReconciliationNeeded
		}

		belongsToResource := physicalContainerNetworkBelongsToResource(inspectedNetwork, network)
		if !belongsToResource && !networkConfig.ReplaceExisting {
			data.conditionReason = apiv2.PhysicalContainerNetworkReasonCreateFailed
			data.failureMessage = fmt.Sprintf("Runtime network name %q is already in use.", networkConfig.NetworkName)
			data.retryAfter = time.Time{}
			stateKey, _ := reconciler.networkData.BorrowByNamespacedName(network.NamespacedName())
			if reconciler.networkData.Update(network.NamespacedName(), stateKey, data) {
				return data.applyTo(network)
			}
			return additionalReconciliationNeeded
		}
		if !belongsToResource {
			log.V(1).Info("Retrying runtime network replacement", "NetworkID", inspectedNetwork.Id, "NetworkName", inspectedNetwork.Name)
			return reconciler.schedulePhysicalContainerNetworkCreate(network, log)
		}

		data.conditionReason = apiv2.PhysicalContainerNetworkReasonCreated
		data.networkID = inspectedNetwork.Id
		data.failureMessage = ""
		data.retryAfter = time.Time{}
		stateKey, _ := reconciler.networkData.BorrowByNamespacedName(network.NamespacedName())
		if reconciler.networkData.Update(network.NamespacedName(), stateKey, data) {
			log.V(1).Info("Adopted runtime network created by an earlier attempt", "NetworkID", inspectedNetwork.Id)
			return data.applyTo(network) | applyReadyPhysicalContainerNetworkStatus(network, inspectedNetwork)
		}
		return additionalReconciliationNeeded
	}
	if !errors.Is(inspectErr, containers.ErrNotFound) {
		data.failureMessage = fmt.Sprintf("Failed to verify whether runtime network creation succeeded: %v", inspectErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		stateKey, _ := reconciler.networkData.BorrowByNamespacedName(network.NamespacedName())
		if reconciler.networkData.Update(network.NamespacedName(), stateKey, data) {
			return data.applyTo(network) | additionalReconciliationNeeded
		}
		return additionalReconciliationNeeded
	}

	log.V(1).Info("Retrying runtime network creation", "NetworkName", networkConfig.NetworkName)
	return reconciler.schedulePhysicalContainerNetworkCreate(network, log)
}

func handleUnknownPhysicalContainerNetworkDataReason(
	_ context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	conditionReason apiv2.ConditionReason,
	_ *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	reconciler.networkData.DeleteByNamespacedName(network.NamespacedName())
	message := fmt.Sprintf("Runtime network operation reached unknown condition reason %q.", conditionReason)
	log.Error(fmt.Errorf("unknown physical network condition reason %q", conditionReason), "Runtime network operation reached unknown condition reason")
	change := setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseFailed)
	change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonReconciliationFailed, message)
	return change | additionalReconciliationNeeded
}

func (r *PhysicalContainerNetworkReconciler) handleDeletionRequest(ctx context.Context, network *apiv2.PhysicalContainerNetwork, log logr.Logger) objectChange {
	_, data := r.networkData.BorrowByNamespacedName(network.NamespacedName())
	if data != nil && data.operationInProgress() {
		// Waiting rather than cancelling: a cancelled create can still produce a runtime network,
		// and its ID would be lost, leaving the network to be reclaimed only by startup harvesting.
		log.V(1).Info("PhysicalContainerNetwork is being deleted while creation is in progress")
		return additionalReconciliationNeeded
	}

	networkID := network.Status.NetworkID
	if networkID == "" && data != nil {
		networkID = data.networkID
	}
	if networkID == "" {
		networkID = network.Spec.NetworkID
	}
	networkConfig := network.Spec.Network
	if networkConfig != nil && !networkConfig.RetainRuntimeNetwork &&
		networkID == "" && data != nil &&
		data.conditionReason == apiv2.PhysicalContainerNetworkReasonReconciliationFailed {
		inspectedNetwork, inspectErr := inspectPhysicalContainerNetwork(ctx, r.orchestrator, networkConfig.NetworkName)
		if inspectErr == nil && physicalContainerNetworkBelongsToResource(inspectedNetwork, network) {
			networkID = inspectedNetwork.Id
		} else if inspectErr != nil && !errors.Is(inspectErr, containers.ErrNotFound) {
			verificationErr := fmt.Errorf("verify whether runtime network creation succeeded: %w", inspectErr)
			return applyPhysicalContainerNetworkRemovalFailure(network, verificationErr, log)
		}
	}

	if networkConfig != nil && !networkConfig.RetainRuntimeNetwork && networkID != "" {
		removeErr := r.removeRuntimeNetwork(ctx, networkID, log)
		if removeErr != nil {
			return applyPhysicalContainerNetworkRemovalFailure(network, removeErr, log)
		}
	}

	r.networkData.DeleteByNamespacedName(network.NamespacedName())
	return deleteFinalizer(network, physicalContainerNetworkFinalizer, log)
}

func applyPhysicalContainerNetworkRemovalFailure(
	network *apiv2.PhysicalContainerNetwork,
	removeErr error,
	log logr.Logger,
) objectChange {
	log.Error(removeErr, "Failed to remove runtime network", "NetworkID", network.Status.NetworkID)
	change := setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseFailed)
	change |= setCondition(
		&network.Status.Conditions,
		apiv2.ConditionReady,
		network.Generation,
		metav1.ConditionFalse,
		apiv2.PhysicalContainerNetworkReasonRuntimeNetworkRemoveFailed,
		fmt.Sprintf("Failed to remove runtime network: %v", removeErr),
	)
	return change | additionalReconciliationNeeded
}

// Disconnects all attached containers and removes the runtime network.
func (r *PhysicalContainerNetworkReconciler) removeRuntimeNetwork(ctx context.Context, networkID string, log logr.Logger) error {
	inspectedNetwork, inspectErr := inspectPhysicalContainerNetwork(ctx, r.orchestrator, networkID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		return nil
	}
	if inspectErr != nil {
		return fmt.Errorf("inspect runtime network before removal: %w", inspectErr)
	}

	if r.orchestrator.IsBuiltInNetwork(inspectedNetwork.Name) {
		log.V(1).Info(
			"Skipping removal of built-in runtime network",
			"NetworkID", inspectedNetwork.Id,
			"NetworkName", inspectedNetwork.Name,
		)
		return nil
	}

	listedContainers, listErr := r.orchestrator.ListContainers(ctx, containers.ListContainersOptions{
		All: true,
		Filters: containers.ListContainersFilters{
			NetworkFilters: []string{inspectedNetwork.Id},
		},
	})
	if listErr != nil {
		_, confirmErr := inspectPhysicalContainerNetwork(ctx, r.orchestrator, networkID)
		if errors.Is(confirmErr, containers.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("list containers attached to runtime network: %w", errors.Join(listErr, confirmErr))
	}

	attachedContainerIDs := make(map[string]struct{}, len(inspectedNetwork.Containers)+len(listedContainers))
	for _, attachedContainer := range inspectedNetwork.Containers {
		attachedContainerIDs[attachedContainer.Id] = struct{}{}
	}
	for _, listedContainer := range listedContainers {
		attachedContainerIDs[listedContainer.Id] = struct{}{}
	}

	var disconnectErrors error
	for containerID := range attachedContainerIDs {
		disconnectErr := r.orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
			Network:   inspectedNetwork.Id,
			Container: containerID,
			Force:     true,
		})
		if disconnectErr != nil && !errors.Is(disconnectErr, containers.ErrNotFound) {
			disconnectErrors = errors.Join(disconnectErrors, disconnectErr)
		}
	}
	if disconnectErrors != nil {
		return fmt.Errorf("disconnect all containers from runtime network: %w", disconnectErrors)
	}

	_, removeErr := r.orchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
		Networks: []string{networkID},
	})
	if removeErr == nil {
		return nil
	}

	// Removal reports a partial failure both for a network that is already gone and for one that
	// acquired a new attachment, so confirm the outcome instead of interpreting the error.
	// A network that still exists is retried from inspection and disconnection.
	_, confirmErr := inspectPhysicalContainerNetwork(ctx, r.orchestrator, networkID)
	if errors.Is(confirmErr, containers.ErrNotFound) {
		return nil
	}

	return fmt.Errorf("remove runtime network: %w", errors.Join(removeErr, confirmErr))
}

func inspectPhysicalContainerNetwork(ctx context.Context, orchestrator containers.NetworkOrchestrator, network string) (*containers.InspectedNetwork, error) {
	inspectedNetworks, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: []string{network},
	})
	// Orchestrators report ErrIncomplete alongside successfully inspected networks, so prefer the
	// result over the error.
	if len(inspectedNetworks) > 0 {
		return &inspectedNetworks[0], nil
	}
	if inspectErr != nil {
		return nil, inspectErr
	}

	return nil, containers.ErrNotFound
}

func physicalContainerNetworkCreationLabels(network *apiv2.PhysicalContainerNetwork, log logr.Logger) map[string]string {
	networkConfig := network.Spec.Network
	labels := map[string]string{}
	for _, label := range networkConfig.Labels {
		labels[label.Key] = label.Value
	}
	labels[PersistentLabel] = fmt.Sprintf("%t", networkConfig.RetainRuntimeNetwork)
	if network.UID != "" {
		labels[uidLabel] = string(network.UID)
	}

	thisProcess, thisProcessErr := process.This()
	if thisProcessErr != nil {
		log.Error(thisProcessErr, "Could not get the current process information; runtime network will not have creator process information")
		return labels
	}

	labels[CreatorProcessIdLabel] = fmt.Sprintf("%d", thisProcess.Pid)
	labels[CreatorProcessStartTimeLabel] = thisProcess.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat)
	return labels
}

func physicalContainerNetworkBelongsToResource(
	inspectedNetwork *containers.InspectedNetwork,
	network *apiv2.PhysicalContainerNetwork,
) bool {
	return network.UID != "" && inspectedNetwork.Labels[uidLabel] == string(network.UID)
}

func applyReadyPhysicalContainerNetworkStatus(network *apiv2.PhysicalContainerNetwork, inspectedNetwork *containers.InspectedNetwork) objectChange {
	change := setValue(&network.Status.NetworkID, inspectedNetwork.Id)
	change |= setValue(&network.Status.NetworkName, inspectedNetwork.Name)
	change |= setValue(&network.Status.Driver, inspectedNetwork.Driver)
	change |= setValue(&network.Status.IPv6, inspectedNetwork.IPv6)
	change |= setPhysicalContainerNetworkAddresses(&network.Status.Subnets, inspectedNetwork.Subnets)
	change |= setPhysicalContainerNetworkAddresses(&network.Status.Gateways, inspectedNetwork.Gateways)
	change |= setTimestamp(&network.Status.CreatedAt, metav1.NewMicroTime(inspectedNetwork.CreatedAt))
	change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseReady)
	change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionTrue, apiv2.PhysicalContainerNetworkReasonNetworkReady, "Runtime network is available.")
	// Keep polling slowly so a network removed outside of DCP does not leave a stale Ready status.
	return change | additionalReconciliationNeeded
}

func setPhysicalContainerNetworkAddresses(target *[]string, addresses []string) objectChange {
	if slices.Equal(*target, addresses) {
		return noChange
	}

	*target = append([]string{}, addresses...)
	return statusChanged
}
