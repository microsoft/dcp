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
	"github.com/microsoft/dcp/pkg/resiliency"
)

var (
	physicalContainerNetworkFinalizer string = fmt.Sprintf("%s/physicalcontainernetwork-reconciler", apiv2.GroupVersion.Group)

	physicalContainerNetworkDataInitializers = map[physicalContainerNetworkState]physicalContainerNetworkDataInitializerFunc{
		physicalContainerNetworkStateNamespace: handlePhysicalContainerNetworkNamespace,
		physicalContainerNetworkStateResolve:   handlePhysicalContainerNetworkResolve,
		physicalContainerNetworkStateCreate:    handlePhysicalContainerNetworkCreateState,
		physicalContainerNetworkStateReplace:   handlePhysicalContainerNetworkCreateState,
		physicalContainerNetworkStateRuntime:   handlePhysicalContainerNetworkRuntime,
		physicalContainerNetworkStateRemove:    handlePhysicalContainerNetworkRemovalState,
		0:                                      handleUnknownPhysicalContainerNetworkDataReason,
	}
)

type physicalContainerNetworkDataInitializerFunc = physicalResourceStateHandlerFunc[
	apiv2.PhysicalContainerNetwork, *apiv2.PhysicalContainerNetwork,
	PhysicalContainerNetworkReconciler, *PhysicalContainerNetworkReconciler,
	physicalContainerNetworkState,
	physicalContainerNetworkDataStateKey,
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
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNamespace(&apiv2.PhysicalContainerNetworkList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
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
	reconciliationDelay := StandardDelay
	patch := ctrl_client.MergeFromWithOptions(network.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		change, reconciliationDelay = r.managePhysicalContainerNetwork(ctx, &network, log)
	} else if change = ensureFinalizer(&network, physicalContainerNetworkFinalizer, log); change != noChange {
		// Make additional changes during the next reconciliation.
	} else {
		change, reconciliationDelay = r.managePhysicalContainerNetwork(ctx, &network, log)
	}

	return r.SaveChangesWithDelay(ctx, &network, patch, change, reconciliationDelay, nil, log)
}

func (r *PhysicalContainerNetworkReconciler) managePhysicalContainerNetwork(
	ctx context.Context,
	network *apiv2.PhysicalContainerNetwork,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	stateKey, data := r.networkData.BorrowByNamespacedName(network.NamespacedName())
	if data == nil {
		data = &physicalContainerNetworkData{
			state:    physicalContainerNetworkStateNamespace,
			progress: physicalResourceProgressNotReady,
		}
		initialStateKey := physicalContainerNetworkDataKey(network)
		stateKey = initialStateKey
		// Store() retains the supplied pointer, so keep an unaliased copy for this reconciliation.
		r.networkData.Store(network.NamespacedName(), initialStateKey, data.Clone())
	}

	handler := getStateHandler(physicalContainerNetworkDataInitializers, data.state, log)
	change := handler(ctx, r, network, data.state, stateKey, data, log)

	if !hasFinalizer(network, physicalContainerNetworkFinalizer) {
		return change, StandardDelay
	}

	_ = r.networkData.Update(network.NamespacedName(), stateKey, data)
	change |= data.applyTo(network)
	delay := physicalContainerNetworkProjections.reconciliationDelay(data.state, data.progress)
	return change, delay
}

func handlePhysicalContainerNetworkNamespace(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	_ physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		return reconciler.beginPhysicalContainerNetworkRemoval(network, stateKey, data, log)
	}
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, reconciler.Client, network.Namespace)
	if !namespaceReady {
		data.state = physicalContainerNetworkStateNamespace
		data.failureMessage = namespaceReadinessMessage(network.Namespace, namespaceReason)
		switch namespaceReason {
		case apiv2.PhysicalResourceReasonNamespaceNotFound:
			data.progress = physicalResourceProgressNotFound
		case apiv2.PhysicalResourceReasonNamespaceTerminating:
			data.progress = physicalResourceProgressTerminating
		case apiv2.PhysicalResourceReasonNamespaceNotActive:
			data.progress = physicalResourceProgressNotActive
		default:
			data.progress = physicalResourceProgressNotReady
		}
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get namespace", "Namespace", network.Namespace)
			data.progress = physicalResourceProgressRetryPending
			data.failureMessage = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
		}
		return noChange
	}

	data.state = physicalContainerNetworkStateResolve
	data.progress = physicalResourceProgressInProgress
	data.failureMessage = ""
	return handlePhysicalContainerNetworkResolve(ctx, reconciler, network, data.state, stateKey, data, log)
}

func handlePhysicalContainerNetworkResolve(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	_ physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		return reconciler.beginPhysicalContainerNetworkRemoval(network, stateKey, data, log)
	}

	networkID := network.Spec.NetworkID
	if networkID == "" {
		networkID = data.networkID
	}
	if networkID == "" {
		return reconciler.schedulePhysicalContainerNetworkCreate(network, stateKey, data, log)
	}
	return reconciler.applyRuntimeNetworkStatus(ctx, network, stateKey, data, networkID, log)
}

func handlePhysicalContainerNetworkRuntime(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	_ physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		return reconciler.beginPhysicalContainerNetworkRemoval(network, stateKey, data, log)
	}
	return reconciler.applyRuntimeNetworkStatus(ctx, network, stateKey, data, data.networkID, log)
}

// Inspects the runtime network and records the resulting reconciliation state.
func (r *PhysicalContainerNetworkReconciler) applyRuntimeNetworkStatus(
	ctx context.Context,
	network *apiv2.PhysicalContainerNetwork,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	networkID string,
	log logr.Logger,
) objectChange {
	inspectedNetwork, inspectErr := inspectPhysicalContainerNetwork(ctx, r.orchestrator, networkID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		data.state = physicalContainerNetworkStateRuntime
		data.progress = physicalResourceProgressMissing
		data.networkID = networkID
		data.failureMessage = ""
		return noChange
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect runtime network", "NetworkID", networkID)
		data.state = physicalContainerNetworkStateRuntime
		data.progress = physicalResourceProgressRetryPending
		data.networkID = networkID
		data.failureMessage = fmt.Sprintf("Failed to inspect runtime network: %v", inspectErr)
		return noChange
	}

	data.state = physicalContainerNetworkStateRuntime
	data.progress = physicalResourceProgressCompleted
	data.networkID = inspectedNetwork.Id
	data.failureMessage = ""
	return applyReadyPhysicalContainerNetworkStatus(network, inspectedNetwork)
}

func (r *PhysicalContainerNetworkReconciler) schedulePhysicalContainerNetworkCreate(
	network *apiv2.PhysicalContainerNetwork,
	stateKey physicalContainerNetworkDataStateKey,
	currentData *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	networkConfig := network.Spec.Network
	data := &physicalContainerNetworkData{
		state:    physicalContainerNetworkStateCreate,
		progress: physicalContainerNetworkOperationInProgress,
	}
	if !r.networkData.Update(network.NamespacedName(), stateKey, data) {
		return additionalReconciliationNeeded
	}
	currentData.UpdateFrom(data)
	networkSnapshot := network.DeepCopy()
	dataSnapshot := data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.createPhysicalContainerNetwork(operationCtx, networkSnapshot, stateKey, dataSnapshot, log)
	})
	if enqueueErr != nil {
		log.Error(enqueueErr, "Failed to queue PhysicalContainerNetwork create", "NetworkName", networkConfig.NetworkName)
		data.progress = physicalResourceProgressFailed
		data.failureMessage = fmt.Sprintf("Failed to queue runtime network create: %v", enqueueErr)
		currentData.UpdateFrom(data)
		return noChange
	}

	log.V(1).Info("Queued PhysicalContainerNetwork create", "NetworkName", networkConfig.NetworkName)
	return noChange
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
			data.state = physicalContainerNetworkStateReplace
			data.progress = physicalContainerNetworkOperationRetryPending
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
		data.state = physicalContainerNetworkStateReplace
		data.progress = physicalContainerNetworkOperationFailed
		data.networkID = inspectedNetwork.Id
		data.failureMessage = fmt.Sprintf("Runtime network %q is built in and cannot be replaced.", inspectedNetwork.Name)
		data.retryAfter = time.Time{}
		return false, nil
	}
	if physicalContainerNetworkBelongsToResource(inspectedNetwork, network) {
		data.state = physicalContainerNetworkStateCreate
		data.progress = physicalContainerNetworkOperationCompleted
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
			data.state = physicalContainerNetworkStateReplace
			data.progress = physicalContainerNetworkOperationFailed
			data.networkID = inspectedNetwork.Id
			data.failureMessage = fmt.Sprintf(
				"Runtime network %q is built in and cannot be replaced.",
				inspectedNetwork.Name,
			)
			data.retryAfter = time.Time{}
		} else if inspectErr == nil && physicalContainerNetworkBelongsToResource(inspectedNetwork, network) {
			data.state = physicalContainerNetworkStateCreate
			data.progress = physicalContainerNetworkOperationCompleted
			data.networkID = inspectedNetwork.Id
			data.failureMessage = ""
			data.retryAfter = time.Time{}
		} else if inspectErr == nil {
			if networkConfig.ReplaceExisting {
				data.state = physicalContainerNetworkStateCreate
				data.progress = physicalContainerNetworkOperationRetryPending
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			} else {
				data.state = physicalContainerNetworkStateCreate
				data.progress = physicalContainerNetworkOperationFailed
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
			data.state = physicalContainerNetworkStateCreate
			data.progress = physicalContainerNetworkOperationRetryPending
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		}
	} else if networkID == "" {
		log.Error(errors.New("runtime network create succeeded without returning a network ID"), "Runtime network create succeeded without returning a network ID", "NetworkName", networkConfig.NetworkName)
		data.state = physicalContainerNetworkStateCreate
		data.progress = physicalContainerNetworkOperationFailed
		data.failureMessage = "Runtime network create succeeded without returning a network ID."
		data.retryAfter = time.Time{}
	} else {
		data.state = physicalContainerNetworkStateCreate
		data.progress = physicalContainerNetworkOperationCompleted
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

func handlePhysicalContainerNetworkCreateState(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		return handlePhysicalContainerNetworkCreateStateDuringDeletion(ctx, reconciler, network, state, stateKey, data, log)
	}
	if state == physicalContainerNetworkStateReplace &&
		data.progress == physicalContainerNetworkOperationFailed {
		return handlePhysicalContainerNetworkBuiltInNetworkNotRemovable(ctx, reconciler, network, state, stateKey, data, log)
	}
	switch data.progress {
	case physicalContainerNetworkOperationInProgress:
		return handlePhysicalContainerNetworkCreating(ctx, reconciler, network, state, stateKey, data, log)
	case physicalContainerNetworkOperationCompleted:
		return handlePhysicalContainerNetworkCreated(ctx, reconciler, network, state, stateKey, data, log)
	case physicalContainerNetworkOperationRetryPending,
		physicalContainerNetworkOperationFailed:
		return handlePhysicalContainerNetworkCreateFailure(ctx, reconciler, network, state, stateKey, data, log)
	default:
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}
}

func handlePhysicalContainerNetworkCreating(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerNetworkOperationInProgress {
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}

	log.V(1).Info("Runtime network creation is still in progress")
	return noChange
}

func handlePhysicalContainerNetworkCreated(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	_ physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerNetworkOperationCompleted {
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, physicalContainerNetworkStateCreate, stateKey, data, log)
	}

	networkID := data.networkID
	log.V(1).Info("Runtime network created; saving network status", "NetworkID", networkID)
	return reconciler.applyRuntimeNetworkStatus(ctx, network, stateKey, data, networkID, log)
}

func handlePhysicalContainerNetworkCreateFailure(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalContainerNetworkOperationRetryPending:
		return handlePhysicalContainerNetworkRecoverableCreateFailed(ctx, reconciler, network, state, stateKey, data, log)
	case physicalContainerNetworkOperationFailed:
		return handlePhysicalContainerNetworkCreateFailed(ctx, reconciler, network, state, stateKey, data, log)
	default:
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}
}

func handlePhysicalContainerNetworkCreateFailed(
	_ context.Context,
	_ *PhysicalContainerNetworkReconciler,
	_ *apiv2.PhysicalContainerNetwork,
	_ physicalContainerNetworkState,
	_ physicalContainerNetworkDataStateKey,
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
	_ physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		return handlePhysicalContainerNetworkFailedCreateDuringDeletion(ctx, reconciler, network, physicalContainerNetworkStateReplace, stateKey, data, log)
	}
	if data.progress != physicalContainerNetworkOperationFailed {
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, physicalContainerNetworkStateReplace, stateKey, data, log)
	}

	log.V(1).Info("Built-in runtime network cannot be removed; saving network status", "NetworkID", data.networkID)
	inspectedNetwork, inspectErr := inspectPhysicalContainerNetwork(ctx, reconciler.orchestrator, data.networkID)
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect built-in runtime network", "NetworkID", data.networkID)
		return noChange
	}

	change := applyReadyPhysicalContainerNetworkStatus(network, inspectedNetwork)
	change &^= additionalReconciliationNeeded
	return change
}

func handlePhysicalContainerNetworkRecoverableCreateFailed(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerNetworkOperationRetryPending {
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}

	networkConfig := network.Spec.Network
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}

	inspectedNetwork, inspectErr := inspectPhysicalContainerNetwork(ctx, reconciler.orchestrator, networkConfig.NetworkName)
	if inspectErr == nil {
		if networkConfig.ReplaceExisting &&
			reconciler.orchestrator.IsBuiltInNetwork(inspectedNetwork.Name) {
			data.state = physicalContainerNetworkStateReplace
			data.progress = physicalContainerNetworkOperationFailed
			data.networkID = inspectedNetwork.Id
			data.failureMessage = fmt.Sprintf(
				"Runtime network %q is built in and cannot be replaced.",
				inspectedNetwork.Name,
			)
			data.retryAfter = time.Time{}
			return noChange
		}

		belongsToResource := physicalContainerNetworkBelongsToResource(inspectedNetwork, network)
		if !belongsToResource && !networkConfig.ReplaceExisting {
			data.state = physicalContainerNetworkStateCreate
			data.progress = physicalContainerNetworkOperationFailed
			data.failureMessage = fmt.Sprintf("Runtime network name %q is already in use.", networkConfig.NetworkName)
			data.retryAfter = time.Time{}
			return noChange
		}
		if !belongsToResource {
			log.V(1).Info("Retrying runtime network replacement", "NetworkID", inspectedNetwork.Id, "NetworkName", inspectedNetwork.Name)
			return reconciler.schedulePhysicalContainerNetworkCreate(network, stateKey, data, log)
		}

		data.state = physicalContainerNetworkStateRuntime
		data.progress = physicalContainerNetworkOperationCompleted
		data.networkID = inspectedNetwork.Id
		data.failureMessage = ""
		data.retryAfter = time.Time{}
		log.V(1).Info("Adopted runtime network created by an earlier attempt", "NetworkID", inspectedNetwork.Id)
		return applyReadyPhysicalContainerNetworkStatus(network, inspectedNetwork)
	}
	if !errors.Is(inspectErr, containers.ErrNotFound) {
		data.failureMessage = fmt.Sprintf("Failed to verify whether runtime network creation succeeded: %v", inspectErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return additionalReconciliationNeeded
	}

	log.V(1).Info("Retrying runtime network creation", "NetworkName", networkConfig.NetworkName)
	return reconciler.schedulePhysicalContainerNetworkCreate(network, stateKey, data, log)
}

func handleUnknownPhysicalContainerNetworkDataReason(
	_ context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		return reconciler.beginPhysicalContainerNetworkRemoval(network, stateKey, data, log)
	}
	log.Error(fmt.Errorf("invalid physical network state %v with progress %v", state, data.progress), "Runtime network operation reached invalid state")
	return additionalReconciliationNeeded
}

func (r *PhysicalContainerNetworkReconciler) beginPhysicalContainerNetworkRemoval(
	network *apiv2.PhysicalContainerNetwork,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	networkConfig := network.Spec.Network
	if networkConfig == nil || networkConfig.RetainRuntimeNetwork {
		r.networkData.DeleteByNamespacedName(network.NamespacedName())
		return deleteFinalizer(network, physicalContainerNetworkFinalizer, log)
	}
	if data != nil &&
		data.state == physicalContainerNetworkStateReplace &&
		data.progress == physicalContainerNetworkOperationFailed {
		r.networkData.DeleteByNamespacedName(network.NamespacedName())
		return deleteFinalizer(network, physicalContainerNetworkFinalizer, log)
	}

	networkID := ""
	if data != nil {
		networkID = data.networkID
	}
	resolveOwnedNetworkByName := networkID == "" &&
		data != nil
	if networkID == "" && !resolveOwnedNetworkByName {
		r.networkData.DeleteByNamespacedName(network.NamespacedName())
		return deleteFinalizer(network, physicalContainerNetworkFinalizer, log)
	}

	return r.schedulePhysicalContainerNetworkRemoval(network, stateKey, data, networkID, resolveOwnedNetworkByName, log)
}

func handlePhysicalContainerNetworkCreateInProgressDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerNetworkOperationInProgress {
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}

	// Waiting rather than cancelling: a cancelled create can still produce a runtime network,
	// and its ID would be lost, leaving the owned network behind.
	log.V(1).Info("PhysicalContainerNetwork is being deleted while creation is in progress")
	return additionalReconciliationNeeded
}

func handlePhysicalContainerNetworkCreatedDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerNetworkOperationCompleted {
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}

	return reconciler.beginPhysicalContainerNetworkRemoval(network, stateKey, data, log)
}

func handlePhysicalContainerNetworkFailedCreateDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerNetworkOperationFailed {
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}

	reconciler.networkData.DeleteByNamespacedName(network.NamespacedName())
	return deleteFinalizer(network, physicalContainerNetworkFinalizer, log)
}

func handlePhysicalContainerNetworkCreateStateDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalContainerNetworkOperationInProgress:
		return handlePhysicalContainerNetworkCreateInProgressDuringDeletion(ctx, reconciler, network, state, stateKey, data, log)
	case physicalContainerNetworkOperationCompleted:
		return handlePhysicalContainerNetworkCreatedDuringDeletion(ctx, reconciler, network, state, stateKey, data, log)
	case physicalContainerNetworkOperationRetryPending:
		return handlePhysicalContainerNetworkRecoverableCreateFailureDuringDeletion(ctx, reconciler, network, state, stateKey, data, log)
	case physicalContainerNetworkOperationFailed:
		return handlePhysicalContainerNetworkFailedCreateDuringDeletion(ctx, reconciler, network, state, stateKey, data, log)
	default:
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}
}

func handlePhysicalContainerNetworkRecoverableCreateFailureDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerNetworkOperationRetryPending {
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}

	return reconciler.beginPhysicalContainerNetworkRemoval(network, stateKey, data, log)
}

func handlePhysicalContainerNetworkRemovalState(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalContainerNetworkOperationInProgress:
		return handlePhysicalContainerNetworkRemovalInProgress(ctx, reconciler, network, state, stateKey, data, log)
	case physicalContainerNetworkOperationRetryPending:
		return handlePhysicalContainerNetworkRemovalFailed(ctx, reconciler, network, state, stateKey, data, log)
	case physicalContainerNetworkOperationCompleted:
		return handlePhysicalContainerNetworkRemovalCompleted(ctx, reconciler, network, state, stateKey, data, log)
	default:
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}
}

func handlePhysicalContainerNetworkRemovalInProgress(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerNetworkOperationInProgress {
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}

	log.V(1).Info("Runtime network removal is still in progress", "NetworkID", data.networkID)
	return additionalReconciliationNeeded
}

func handlePhysicalContainerNetworkRemovalFailed(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerNetworkOperationRetryPending {
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}

	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	return reconciler.schedulePhysicalContainerNetworkRemoval(network, stateKey, data, data.networkID, data.resolveByName, log)
}

func handlePhysicalContainerNetworkRemovalCompleted(
	ctx context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	state physicalContainerNetworkState,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerNetworkOperationCompleted {
		return handleUnknownPhysicalContainerNetworkDataReason(ctx, reconciler, network, state, stateKey, data, log)
	}

	reconciler.networkData.DeleteByNamespacedName(network.NamespacedName())
	return deleteFinalizer(network, physicalContainerNetworkFinalizer, log)
}

func (r *PhysicalContainerNetworkReconciler) schedulePhysicalContainerNetworkRemoval(
	network *apiv2.PhysicalContainerNetwork,
	stateKey physicalContainerNetworkDataStateKey,
	currentData *physicalContainerNetworkData,
	networkID string,
	resolveOwnedNetworkByName bool,
	log logr.Logger,
) objectChange {
	data := &physicalContainerNetworkData{
		state:         physicalContainerNetworkStateRemove,
		progress:      physicalContainerNetworkOperationInProgress,
		networkID:     networkID,
		resolveByName: resolveOwnedNetworkByName,
	}
	if !r.networkData.Update(network.NamespacedName(), stateKey, data) {
		return additionalReconciliationNeeded
	}
	currentData.UpdateFrom(data)
	networkSnapshot := network.DeepCopy()
	dataSnapshot := data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.removePhysicalContainerNetwork(operationCtx, networkSnapshot, stateKey, dataSnapshot, log)
	})
	if enqueueErr == nil {
		log.V(1).Info("Queued PhysicalContainerNetwork removal", "NetworkID", networkID)
		return additionalReconciliationNeeded
	}

	log.Error(enqueueErr, "Failed to queue PhysicalContainerNetwork removal", "NetworkID", networkID)
	data.progress = physicalContainerNetworkOperationRetryPending
	data.failureMessage = fmt.Sprintf("Failed to queue runtime network removal: %v", enqueueErr)
	data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
	currentData.UpdateFrom(data)
	return additionalReconciliationNeeded
}

func (r *PhysicalContainerNetworkReconciler) removePhysicalContainerNetwork(
	ctx context.Context,
	network *apiv2.PhysicalContainerNetwork,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) {
	networkID := data.networkID
	var removeErr error
	if data.resolveByName {
		inspectedNetwork, inspectErr := inspectPhysicalContainerNetwork(ctx, r.orchestrator, network.Spec.Network.NetworkName)
		switch {
		case errors.Is(inspectErr, containers.ErrNotFound):
			networkID = ""
		case inspectErr != nil:
			removeErr = fmt.Errorf("verify whether runtime network creation succeeded: %w", inspectErr)
		case !physicalContainerNetworkBelongsToResource(inspectedNetwork, network):
			networkID = ""
		case inspectedNetwork.Id == "":
			removeErr = errors.New("owned runtime network inspection returned an empty ID")
		default:
			networkID = inspectedNetwork.Id
		}
	}

	if removeErr == nil && networkID != "" {
		removeErr = r.removeRuntimeNetwork(ctx, networkID, log)
	}

	data.networkID = networkID
	if removeErr != nil {
		log.Error(removeErr, "Failed to remove runtime network", "NetworkID", networkID)
		data.state = physicalContainerNetworkStateRemove
		data.progress = physicalContainerNetworkOperationRetryPending
		data.failureMessage = fmt.Sprintf("Failed to remove runtime network: %v", removeErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
	} else {
		data.state = physicalContainerNetworkStateRemove
		data.progress = physicalContainerNetworkOperationCompleted
		data.failureMessage = ""
		data.retryAfter = time.Time{}
	}
	r.queuePhysicalContainerNetworkDataResult(network, stateKey, data)
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
	creationLabels := physicalResourceCreationLabels(
		networkConfig.Labels,
		networkConfig.RetainRuntimeNetwork,
		network.UID,
		log,
	)
	labels := make(map[string]string, len(creationLabels))
	for _, label := range creationLabels {
		labels[label.Key] = label.Value
	}
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
	change |= setCondition(&network.Status.Conditions, apiv2.ConditionReady, network.Generation, metav1.ConditionTrue, apiv2.PhysicalContainerNetworkReasonNetworkAvailable, "Runtime network is available.")
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
