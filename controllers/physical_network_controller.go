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
	physicalNetworkFinalizer string = fmt.Sprintf("%s/physicalnetwork-reconciler", apiv2.GroupVersion.Group)

	physicalNetworkDataInitializers = map[string]physicalNetworkDataInitializerFunc{
		apiv2.PhysicalNetworkReasonCreating:     handlePhysicalNetworkCreating,
		apiv2.PhysicalNetworkReasonCreated:      handlePhysicalNetworkCreated,
		apiv2.PhysicalNetworkReasonCreateFailed: handlePhysicalNetworkCreateFailed,
		"":                                      handleUnknownPhysicalNetworkDataReason,
	}
)

type physicalNetworkDataInitializerFunc = stateInitializerFunc[
	apiv2.PhysicalNetwork, *apiv2.PhysicalNetwork,
	PhysicalNetworkReconciler, *PhysicalNetworkReconciler,
	string,
	physicalNetworkData, *physicalNetworkData,
]

type PhysicalNetworkReconciler struct {
	*ReconcilerBase[apiv2.PhysicalNetwork, *apiv2.PhysicalNetwork]

	orchestrator   containers.NetworkOrchestrator
	networkData    *ObjectStateMap[physicalNetworkDataStateKey, physicalNetworkData, *physicalNetworkData, *apiv2.PhysicalNetwork]
	operationQueue *resiliency.WorkQueue
}

func NewPhysicalNetworkReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	orchestrator containers.NetworkOrchestrator,
) *PhysicalNetworkReconciler {
	return &PhysicalNetworkReconciler{
		ReconcilerBase: NewReconcilerBase[apiv2.PhysicalNetwork](client, noCacheClient, log, lifetimeCtx),
		orchestrator:   orchestrator,
		networkData:    NewObjectStateMap[physicalNetworkDataStateKey, physicalNetworkData, *physicalNetworkData, *apiv2.PhysicalNetwork](),
		operationQueue: resiliency.NewWorkQueue(lifetimeCtx, MaxConcurrentReconciles),
	}
}

func (r *PhysicalNetworkReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.PhysicalNetwork{}).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *PhysicalNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	network := apiv2.PhysicalNetwork{}
	getErr := reader.Get(ctx, req.NamespacedName, &network)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			log.V(1).Info("PhysicalNetwork not found, nothing to do...")
			// The finalizer normally guarantees the deletion is observed, but drop any lingering
			// state in case the object disappeared without it (for example a forced deletion).
			r.networkData.DeleteByNamespacedName(req.NamespacedName)
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}

		log.Error(getErr, "Failed to Get() the PhysicalNetwork")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)

	r.networkData.RunDeferredOps(req.NamespacedName, &network)

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(network.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if network.DeletionTimestamp != nil && !network.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, &network, log)
	} else if change = ensureFinalizer(&network, physicalNetworkFinalizer, log); change != noChange {
		// Make additional changes during the next reconciliation.
	} else {
		change = r.managePhysicalNetwork(ctx, &network, log)
	}

	// A ready network is in a steady state. There is no runtime event subscription for networks,
	// so reconcile it on a slow cadence to notice a network that was removed outside of DCP.
	additionalReconcileDelay := StandardDelay
	if network.Status.Phase == apiv2.PhysicalNetworkPhaseReady {
		additionalReconcileDelay = MonitoringDelay
	}

	return r.SaveChangesWithDelay(ctx, &network, patch, change, additionalReconcileDelay, nil, log)
}

func (r *PhysicalNetworkReconciler) managePhysicalNetwork(ctx context.Context, network *apiv2.PhysicalNetwork, log logr.Logger) objectChange {
	if namespaceReady, change := ensureNamespace(ctx, r.Client, network.Namespace, func(message string) objectChange {
		change := setValue(&network.Status.Phase, apiv2.PhysicalNetworkPhasePending)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalNetworkReasonPending, message)
		return change
	}, func(message string) objectChange {
		change := setValue(&network.Status.Phase, apiv2.PhysicalNetworkPhaseFailed)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalNetworkReasonReconciliationFailed, message)
		return change
	}, log); !namespaceReady {
		return change
	}

	change := noChange
	_, data := r.networkData.BorrowByNamespacedName(network.NamespacedName())
	if data != nil {
		change |= data.applyTo(network)
		initializer := getStateInitializer(physicalNetworkDataInitializers, data.conditionReason, log)
		return change | initializer(ctx, r, network, data.conditionReason, data, log)
	}

	if physicalNetworkCreateFailedTerminally(network) {
		return change
	}

	networkID := network.Spec.NetworkID
	if networkID == "" {
		networkID = network.Status.NetworkID
	}
	if networkID == "" {
		return r.schedulePhysicalNetworkCreate(network, log)
	}

	return change | r.applyRuntimeNetworkStatus(ctx, network, networkID, log)
}

// Reports whether the network already recorded a terminal creation failure. The spec is immutable,
// so re-entering the create path could never produce a different outcome.
func physicalNetworkCreateFailedTerminally(network *apiv2.PhysicalNetwork) bool {
	if network.Status.Phase != apiv2.PhysicalNetworkPhaseFailed {
		return false
	}

	readyCondition := apimeta.FindStatusCondition(network.Status.Conditions, apiv2.ConditionReady)
	if readyCondition == nil {
		return false
	}

	return readyCondition.Reason == apiv2.PhysicalNetworkReasonCreateFailed
}

// Inspects the runtime network and projects the result onto the resource status.
func (r *PhysicalNetworkReconciler) applyRuntimeNetworkStatus(
	ctx context.Context,
	network *apiv2.PhysicalNetwork,
	networkID string,
	log logr.Logger,
) objectChange {
	inspectedNetwork, inspectErr := inspectPhysicalNetwork(ctx, r.orchestrator, networkID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		change := setValue(&network.Status.NetworkID, networkID)
		change |= setValue(&network.Status.Phase, apiv2.PhysicalNetworkPhaseMissing)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalNetworkReasonRuntimeNetworkMissing, "Runtime network was not found.")
		return change
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect runtime network", "NetworkID", networkID)
		change := setValue(&network.Status.NetworkID, networkID)
		change |= setValue(&network.Status.Phase, apiv2.PhysicalNetworkPhaseFailed)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalNetworkReasonReconciliationFailed, fmt.Sprintf("Failed to inspect runtime network: %v", inspectErr))
		return change
	}

	return applyReadyPhysicalNetworkStatus(network, inspectedNetwork)
}

func (r *PhysicalNetworkReconciler) schedulePhysicalNetworkCreate(network *apiv2.PhysicalNetwork, log logr.Logger) objectChange {
	stateKey := physicalNetworkDataKey(network)
	data := &physicalNetworkData{conditionReason: apiv2.PhysicalNetworkReasonCreating}
	r.networkData.Store(network.NamespacedName(), stateKey, data)
	networkSnapshot := network.DeepCopy()
	dataSnapshot := data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.createPhysicalNetwork(operationCtx, networkSnapshot, stateKey, dataSnapshot, log)
	})
	if enqueueErr != nil {
		r.networkData.DeleteByNamespacedName(network.NamespacedName())
		log.Error(enqueueErr, "Failed to queue PhysicalNetwork create", "NetworkName", network.Spec.NetworkName)
		change := setValue(&network.Status.Phase, apiv2.PhysicalNetworkPhaseFailed)
		change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalNetworkReasonCreateFailed, fmt.Sprintf("Failed to queue runtime network create: %v", enqueueErr))
		return change
	}

	log.V(1).Info("Queued PhysicalNetwork create", "NetworkName", network.Spec.NetworkName)
	return data.applyTo(network)
}

func (r *PhysicalNetworkReconciler) createPhysicalNetwork(
	ctx context.Context,
	network *apiv2.PhysicalNetwork,
	stateKey physicalNetworkDataStateKey,
	data *physicalNetworkData,
	log logr.Logger,
) {
	networkID, createErr := r.orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name:   network.Spec.NetworkName,
		IPv6:   network.Spec.IPv6,
		Labels: physicalNetworkCreationLabels(network, log),
	})
	if createErr != nil {
		log.Error(createErr, "Failed to create runtime network", "NetworkName", network.Spec.NetworkName)
		data.conditionReason = apiv2.PhysicalNetworkReasonCreateFailed
		data.failureMessage = fmt.Sprintf("Failed to create runtime network: %v", createErr)
	} else if networkID == "" {
		log.Error(errors.New("runtime network create succeeded without returning a network ID"), "Runtime network create succeeded without returning a network ID", "NetworkName", network.Spec.NetworkName)
		data.conditionReason = apiv2.PhysicalNetworkReasonCreateFailed
		data.failureMessage = "Runtime network create succeeded without returning a network ID."
	} else {
		data.conditionReason = apiv2.PhysicalNetworkReasonCreated
		data.networkID = networkID
		data.failureMessage = ""
	}

	r.queuePhysicalNetworkDataResult(network, stateKey, data)
}

func (r *PhysicalNetworkReconciler) queuePhysicalNetworkDataResult(
	network *apiv2.PhysicalNetwork,
	stateKey physicalNetworkDataStateKey,
	result *physicalNetworkData,
) {
	queued := r.networkData.QueueDeferredOpForStateKey(network.NamespacedName(), stateKey, func(name types.NamespacedName, currentStateKey physicalNetworkDataStateKey, _ *apiv2.PhysicalNetwork) {
		_ = r.networkData.Update(name, currentStateKey, result)
	})
	if queued {
		r.ScheduleReconciliation(network.NamespacedName())
	}
}

func handlePhysicalNetworkCreating(
	_ context.Context,
	_ *PhysicalNetworkReconciler,
	_ *apiv2.PhysicalNetwork,
	_ string,
	_ *physicalNetworkData,
	log logr.Logger,
) objectChange {
	log.V(1).Info("Runtime network creation is still in progress")
	return noChange
}

func handlePhysicalNetworkCreated(
	ctx context.Context,
	reconciler *PhysicalNetworkReconciler,
	network *apiv2.PhysicalNetwork,
	_ string,
	data *physicalNetworkData,
	log logr.Logger,
) objectChange {
	networkID := data.networkID
	reconciler.networkData.DeleteByNamespacedName(network.NamespacedName())
	log.V(1).Info("Runtime network created; saving network status", "NetworkID", networkID)
	return reconciler.applyRuntimeNetworkStatus(ctx, network, networkID, log)
}

func handlePhysicalNetworkCreateFailed(
	_ context.Context,
	reconciler *PhysicalNetworkReconciler,
	network *apiv2.PhysicalNetwork,
	_ string,
	data *physicalNetworkData,
	log logr.Logger,
) objectChange {
	reconciler.networkData.DeleteByNamespacedName(network.NamespacedName())
	log.V(1).Info("Runtime network creation failed; saving network status", "Message", data.failureMessage)
	// The failure is terminal: spec is immutable, so no further reconciliation can make progress.
	return noChange
}

func handleUnknownPhysicalNetworkDataReason(
	_ context.Context,
	reconciler *PhysicalNetworkReconciler,
	network *apiv2.PhysicalNetwork,
	conditionReason string,
	_ *physicalNetworkData,
	log logr.Logger,
) objectChange {
	reconciler.networkData.DeleteByNamespacedName(network.NamespacedName())
	message := fmt.Sprintf("Runtime network operation reached unknown condition reason %q.", conditionReason)
	log.Error(fmt.Errorf("unknown physical network condition reason %q", conditionReason), "Runtime network operation reached unknown condition reason")
	change := setValue(&network.Status.Phase, apiv2.PhysicalNetworkPhaseFailed)
	change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionFalse, apiv2.PhysicalNetworkReasonReconciliationFailed, message)
	return change | additionalReconciliationNeeded
}

func (r *PhysicalNetworkReconciler) handleDeletionRequest(ctx context.Context, network *apiv2.PhysicalNetwork, log logr.Logger) objectChange {
	_, data := r.networkData.BorrowByNamespacedName(network.NamespacedName())
	if data != nil && data.operationInProgress() {
		// Waiting rather than cancelling: a cancelled create can still produce a runtime network,
		// and its ID would be lost, leaving the network to be reclaimed only by startup harvesting.
		log.V(1).Info("PhysicalNetwork is being deleted while creation is in progress")
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
	return deleteFinalizer(network, physicalNetworkFinalizer, log)
}

// Removes the runtime network, reporting whether it is gone.
func (r *PhysicalNetworkReconciler) removeRuntimeNetwork(ctx context.Context, networkID string, log logr.Logger) bool {
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
	_, inspectErr := inspectPhysicalNetwork(ctx, r.orchestrator, networkID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		return true
	}

	log.Error(removeErr, "Failed to remove runtime network", "NetworkID", networkID)
	return false
}

func inspectPhysicalNetwork(ctx context.Context, orchestrator containers.NetworkOrchestrator, network string) (*containers.InspectedNetwork, error) {
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

func physicalNetworkCreationLabels(network *apiv2.PhysicalNetwork, log logr.Logger) map[string]string {
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

func applyReadyPhysicalNetworkStatus(network *apiv2.PhysicalNetwork, inspectedNetwork *containers.InspectedNetwork) objectChange {
	change := setValue(&network.Status.NetworkID, inspectedNetwork.Id)
	change |= setValue(&network.Status.NetworkName, inspectedNetwork.Name)
	change |= setValue(&network.Status.Driver, inspectedNetwork.Driver)
	change |= setValue(&network.Status.IPv6, inspectedNetwork.IPv6)
	change |= setPhysicalNetworkAddresses(&network.Status.Subnets, inspectedNetwork.Subnets)
	change |= setPhysicalNetworkAddresses(&network.Status.Gateways, inspectedNetwork.Gateways)
	change |= setTimestamp(&network.Status.CreatedAt, metav1.NewMicroTime(inspectedNetwork.CreatedAt))
	change |= setValue(&network.Status.Phase, apiv2.PhysicalNetworkPhaseReady)
	change |= setReadyCondition(&network.Status.Conditions, network.Generation, metav1.ConditionTrue, apiv2.PhysicalNetworkReasonNetworkReady, "Runtime network is available.")
	// Keep polling slowly so a network removed outside of DCP does not leave a stale Ready status.
	return change | additionalReconciliationNeeded
}

func setPhysicalNetworkAddresses(target *[]string, addresses []string) objectChange {
	if slices.Equal(*target, addresses) {
		return noChange
	}

	*target = append([]string{}, addresses...)
	return statusChanged
}
