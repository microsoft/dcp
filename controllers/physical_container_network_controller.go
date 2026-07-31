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

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

var (
	physicalContainerNetworkFinalizer string = fmt.Sprintf("%s/physicalcontainernetwork-reconciler", apiv2.GroupVersion.Group)

	physicalContainerNetworkDataInitializers = map[string]physicalContainerNetworkDataInitializerFunc{
		apiv2.PhysicalContainerNetworkReasonCreating:     handlePhysicalContainerNetworkCreating,
		apiv2.PhysicalContainerNetworkReasonCreated:      handlePhysicalContainerNetworkCreated,
		apiv2.PhysicalContainerNetworkReasonCreateFailed: handlePhysicalContainerNetworkCreateFailed,
		"": handleUnknownPhysicalContainerNetworkDataReason,
	}
)

type physicalContainerNetworkDataInitializerFunc = stateInitializerFunc[
	apiv2.PhysicalContainerNetwork, *apiv2.PhysicalContainerNetwork,
	PhysicalContainerNetworkReconciler, *PhysicalContainerNetworkReconciler,
	string,
	physicalContainerNetworkData, *physicalContainerNetworkData,
]

type PhysicalContainerNetworkReconciler struct {
	*ReconcilerBase[apiv2.PhysicalContainerNetwork, *apiv2.PhysicalContainerNetwork]

	orchestrator   containers.NetworkOrchestrator
	networkData    *ObjectStateMap[physicalContainerNetworkDataStateKey, physicalContainerNetworkData, *physicalContainerNetworkData, *apiv2.PhysicalContainerNetwork]
	operationQueue *resiliency.WorkQueue
}

func NewPhysicalContainerNetworkReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	orchestrator containers.NetworkOrchestrator,
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
	patch := ctrl_client.MergeFromWithOptions(network.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, &network, log)
	} else if change = ensureFinalizer(&network, physicalContainerNetworkFinalizer, log); change != noChange {
		// Make additional changes during the next reconciliation.
	} else {
		change = r.managePhysicalContainerNetwork(ctx, &network, log)
	}

	// A ready network is in a steady state. There is no runtime event subscription for networks,
	// so reconcile it on a slow cadence to notice a network that was removed outside of DCP.
	additionalReconcileDelay := StandardDelay
	if network.Status.Phase == apiv2.PhysicalContainerNetworkPhaseReady {
		additionalReconcileDelay = MonitoringDelay
	}

	return r.SaveChangesWithDelay(ctx, &network, patch, change, additionalReconcileDelay, nil, log)
}

func (r *PhysicalContainerNetworkReconciler) managePhysicalContainerNetwork(ctx context.Context, network *apiv2.PhysicalContainerNetwork, log logr.Logger) objectChange {
	if namespaceReady, change := ensureNamespace(ctx, r.Client, network.Namespace, func(message string) objectChange {
		change := setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhasePending)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonPending, message)
		return change
	}, func(message string) objectChange {
		change := setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseFailed)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonReconciliationFailed, message)
		return change
	}, log); !namespaceReady {
		return change
	}

	change := noChange
	_, data := r.networkData.BorrowByNamespacedName(network.NamespacedName())
	if data != nil {
		change |= data.applyTo(network)
		initializer := getStateInitializer(physicalContainerNetworkDataInitializers, data.conditionReason, log)
		return change | initializer(ctx, r, network, data.conditionReason, data, log)
	}

	if physicalContainerNetworkCreateFailedTerminally(network) {
		return change
	}

	networkID := network.Spec.NetworkID
	if networkID == "" {
		networkID = network.Status.NetworkID
	}
	if networkID == "" {
		return r.schedulePhysicalContainerNetworkCreate(network, log)
	}

	return change | r.applyRuntimeNetworkStatus(ctx, network, networkID, log)
}

// Reports whether the network already recorded a terminal creation failure. The spec is immutable,
// so re-entering the create path could never produce a different outcome.
func physicalContainerNetworkCreateFailedTerminally(network *apiv2.PhysicalContainerNetwork) bool {
	if network.Status.Phase != apiv2.PhysicalContainerNetworkPhaseFailed {
		return false
	}

	readyCondition := apimeta.FindStatusCondition(network.Status.Conditions, apiv2.ConditionReady)
	if readyCondition == nil {
		return false
	}

	return readyCondition.Reason == apiv2.PhysicalContainerNetworkReasonCreateFailed
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
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonRuntimeNetworkMissing, "Runtime network was not found.")
		return change
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect runtime network", "NetworkID", networkID)
		change := setValue(&network.Status.NetworkID, networkID)
		change |= setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseFailed)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonReconciliationFailed, fmt.Sprintf("Failed to inspect runtime network: %v", inspectErr))
		return change
	}

	return applyReadyPhysicalContainerNetworkStatus(network, inspectedNetwork)
}

func (r *PhysicalContainerNetworkReconciler) schedulePhysicalContainerNetworkCreate(network *apiv2.PhysicalContainerNetwork, log logr.Logger) objectChange {
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
		log.Error(enqueueErr, "Failed to queue PhysicalContainerNetwork create", "NetworkName", network.Spec.NetworkName)
		change := setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseFailed)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonCreateFailed, fmt.Sprintf("Failed to queue runtime network create: %v", enqueueErr))
		return change
	}

	log.V(1).Info("Queued PhysicalContainerNetwork create", "NetworkName", network.Spec.NetworkName)
	return data.applyTo(network)
}

func (r *PhysicalContainerNetworkReconciler) createPhysicalContainerNetwork(
	ctx context.Context,
	network *apiv2.PhysicalContainerNetwork,
	stateKey physicalContainerNetworkDataStateKey,
	data *physicalContainerNetworkData,
	log logr.Logger,
) {
	networkID, createErr := r.orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name:   network.Spec.NetworkName,
		IPv6:   network.Spec.IPv6,
		Labels: physicalContainerNetworkCreationLabels(network, log),
	})
	if createErr != nil {
		log.Error(createErr, "Failed to create runtime network", "NetworkName", network.Spec.NetworkName)
		data.conditionReason = apiv2.PhysicalContainerNetworkReasonCreateFailed
		data.failureMessage = fmt.Sprintf("Failed to create runtime network: %v", createErr)
	} else if networkID == "" {
		log.Error(errors.New("runtime network create succeeded without returning a network ID"), "Runtime network create succeeded without returning a network ID", "NetworkName", network.Spec.NetworkName)
		data.conditionReason = apiv2.PhysicalContainerNetworkReasonCreateFailed
		data.failureMessage = "Runtime network create succeeded without returning a network ID."
	} else {
		data.conditionReason = apiv2.PhysicalContainerNetworkReasonCreated
		data.networkID = networkID
		data.failureMessage = ""
	}

	r.queuePhysicalContainerNetworkDataResult(network, stateKey, data)
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
	_ string,
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
	_ string,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	networkID := data.networkID
	reconciler.networkData.DeleteByNamespacedName(network.NamespacedName())
	log.V(1).Info("Runtime network created; saving network status", "NetworkID", networkID)
	return reconciler.applyRuntimeNetworkStatus(ctx, network, networkID, log)
}

func handlePhysicalContainerNetworkCreateFailed(
	_ context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	_ string,
	data *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	reconciler.networkData.DeleteByNamespacedName(network.NamespacedName())
	log.V(1).Info("Runtime network creation failed; saving network status", "Message", data.failureMessage)
	// The failure is terminal: spec is immutable, so no further reconciliation can make progress.
	return noChange
}

func handleUnknownPhysicalContainerNetworkDataReason(
	_ context.Context,
	reconciler *PhysicalContainerNetworkReconciler,
	network *apiv2.PhysicalContainerNetwork,
	conditionReason string,
	_ *physicalContainerNetworkData,
	log logr.Logger,
) objectChange {
	reconciler.networkData.DeleteByNamespacedName(network.NamespacedName())
	message := fmt.Sprintf("Runtime network operation reached unknown condition reason %q.", conditionReason)
	log.Error(fmt.Errorf("unknown physical network condition reason %q", conditionReason), "Runtime network operation reached unknown condition reason")
	change := setValue(&network.Status.Phase, apiv2.PhysicalContainerNetworkPhaseFailed)
	change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerNetworkReasonReconciliationFailed, message)
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

	if !network.Spec.PreserveOnDeletion && networkID != "" {
		if !r.removeRuntimeNetwork(ctx, networkID, log) {
			return additionalReconciliationNeeded
		}
	}

	r.networkData.DeleteByNamespacedName(network.NamespacedName())
	return deleteFinalizer(network, physicalContainerNetworkFinalizer, log)
}

// Removes the runtime network, reporting whether it is gone.
func (r *PhysicalContainerNetworkReconciler) removeRuntimeNetwork(ctx context.Context, networkID string, log logr.Logger) bool {
	_, removeErr := r.orchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
		Networks: []string{networkID},
		Force:    true,
	})
	if removeErr == nil {
		return true
	}

	// Removal reports a partial failure both for a network that is already gone and for one that
	// still has endpoints attached, so confirm the outcome by inspecting instead of interpreting
	// the error. A network that still exists is retried; containers detach as they are deleted.
	_, inspectErr := inspectPhysicalContainerNetwork(ctx, r.orchestrator, networkID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		return true
	}

	log.Error(removeErr, "Failed to remove runtime network", "NetworkID", networkID)
	return false
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
	labels := map[string]string{}
	for _, label := range network.Spec.Labels {
		labels[label.Key] = label.Value
	}
	labels[PersistentLabel] = fmt.Sprintf("%t", network.Spec.PreserveOnDeletion)

	thisProcess, thisProcessErr := process.This()
	if thisProcessErr != nil {
		log.Error(thisProcessErr, "Could not get the current process information; runtime network will not have creator process information")
		return labels
	}

	labels[CreatorProcessIdLabel] = fmt.Sprintf("%d", thisProcess.Pid)
	labels[CreatorProcessStartTimeLabel] = thisProcess.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat)
	return labels
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
	change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionTrue, apiv2.PhysicalContainerNetworkReasonNetworkReady, "Runtime network is available.")
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
