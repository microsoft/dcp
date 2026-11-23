/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
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

const physicalContainerVolumeRemovalRetryTimeout = 30 * time.Second

var (
	physicalContainerVolumeFinalizer string = fmt.Sprintf("%s/physicalcontainervolume-reconciler", apiv2.GroupVersion.Group)

	physicalContainerVolumeDataInitializers = map[physicalContainerVolumeState]physicalContainerVolumeDataInitializerFunc{
		physicalContainerVolumeStateCreate:           handlePhysicalContainerVolumeCreating,
		physicalContainerVolumeStateCreated:          handlePhysicalContainerVolumeCreated,
		physicalContainerVolumeStateCreateRetry:      handlePhysicalContainerVolumeRecoverableCreateFailed,
		physicalContainerVolumeStateCreateFailed:     handlePhysicalContainerVolumeCreateFailed,
		physicalContainerVolumeStateReady:            handlePhysicalContainerVolumeReady,
		physicalContainerVolumeStateMissing:          handlePhysicalContainerVolumeUnknown,
		physicalContainerVolumeStateInspectFailed:    handlePhysicalContainerVolumeUnknown,
		physicalContainerVolumeStateRemove:           handlePhysicalContainerVolumeRemovalInProgress,
		physicalContainerVolumeStateRemoveRetry:      handlePhysicalContainerVolumeRemovalFailed,
		physicalContainerVolumeStateRemoved:          handlePhysicalContainerVolumeRemovalCompleted,
		physicalContainerVolumeStateRemovalAbandoned: handlePhysicalContainerVolumeRemovalAbandoned,
		physicalContainerVolumeStateInvalid:          handlePhysicalContainerVolumeTerminal,
		0:                                            handleUnknownPhysicalContainerVolumeDataReason,
	}
)

type physicalContainerVolumeDataInitializerFunc = stateInitializerFunc[
	apiv2.PhysicalContainerVolume, *apiv2.PhysicalContainerVolume,
	PhysicalContainerVolumeReconciler, *PhysicalContainerVolumeReconciler,
	physicalContainerVolumeState,
	physicalContainerVolumeData, *physicalContainerVolumeData,
]

type PhysicalContainerVolumeReconciler struct {
	*ReconcilerBase[apiv2.PhysicalContainerVolume, *apiv2.PhysicalContainerVolume]

	orchestrator   containers.VolumeOrchestrator
	volumeData     *ObjectStateMap[physicalContainerVolumeDataStateKey, physicalContainerVolumeData, *physicalContainerVolumeData, *apiv2.PhysicalContainerVolume]
	operationQueue *resiliency.WorkQueue
}

func NewPhysicalContainerVolumeReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	orchestrator containers.VolumeOrchestrator,
) *PhysicalContainerVolumeReconciler {
	return &PhysicalContainerVolumeReconciler{
		ReconcilerBase: NewReconcilerBase[apiv2.PhysicalContainerVolume](client, noCacheClient, log, lifetimeCtx),
		orchestrator:   orchestrator,
		volumeData:     NewObjectStateMap[physicalContainerVolumeDataStateKey, physicalContainerVolumeData, *physicalContainerVolumeData, *apiv2.PhysicalContainerVolume](),
		operationQueue: resiliency.NewWorkQueue(lifetimeCtx, MaxConcurrentReconciles),
	}
}

func (r *PhysicalContainerVolumeReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.PhysicalContainerVolume{}).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.PhysicalContainerVolumeList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *PhysicalContainerVolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	volume := apiv2.PhysicalContainerVolume{}
	getErr := reader.Get(ctx, req.NamespacedName, &volume)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			log.V(1).Info("PhysicalContainerVolume not found, nothing to do...")
			r.volumeData.DeleteByNamespacedName(req.NamespacedName)
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}

		log.Error(getErr, "Failed to Get() the PhysicalContainerVolume")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)

	r.volumeData.RunDeferredOps(req.NamespacedName, &volume)
	_, data := r.volumeData.BorrowByNamespacedName(volume.NamespacedName())

	var change objectChange
	reconciliationDelay := StandardDelay
	patch := ctrl_client.MergeFromWithOptions(volume.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if !volume.DeletionTimestamp.IsZero() && !data.operationInProgress() {
		change, reconciliationDelay = r.handleDeletionRequest(ctx, &volume, data, log)
	} else {
		change = ensureFinalizer(&volume, physicalContainerVolumeFinalizer, log)
		if change == noChange {
			change, reconciliationDelay = r.managePhysicalContainerVolume(ctx, &volume, data, log)
		}
	}

	return r.SaveChangesWithDelay(ctx, &volume, patch, change, reconciliationDelay, nil, log)
}

func (r *PhysicalContainerVolumeReconciler) managePhysicalContainerVolume(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	data *physicalContainerVolumeData,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	if data == nil {
		namespaceReady, namespaceReason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, volume.Namespace)
		if !namespaceReady {
			namespaceChange := noChange
			namespacePhase := apiv2.PhysicalContainerVolumePhasePending
			namespaceMessage := namespaceReadinessMessage(volume.Namespace, namespaceReason)
			if namespaceErr != nil {
				log.Error(namespaceErr, "Failed to get namespace", "Namespace", volume.Namespace)
				namespacePhase = apiv2.PhysicalContainerVolumePhaseUnknown
				namespaceMessage = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
				namespaceChange |= additionalReconciliationNeeded
			}
			namespaceChange |= setValue(&volume.Status.Phase, namespacePhase)
			namespaceChange |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady,
				volume.Generation, metav1.ConditionFalse, namespaceReason, namespaceMessage)
			return namespaceChange, StandardDelay
		}
		data = &physicalContainerVolumeData{state: physicalContainerVolumeStateCreate}
		r.volumeData.Store(volume.NamespacedName(), physicalContainerVolumeDataKey(volume), data.Clone())
	}
	handler := getStateInitializer(physicalContainerVolumeDataInitializers, data.state, log)
	change := handler(ctx, r, volume, data.state, data, log)
	_ = r.volumeData.Update(volume.NamespacedName(), physicalContainerVolumeDataKey(volume), data)
	dataChange, delay, valid := data.applyTo(volume)
	if !valid {
		log.Error(fmt.Errorf("invalid physical container volume state %v", data.state),
			"PhysicalContainerVolume reached invalid reconciliation state")
	}
	return change | dataChange, delay
}

func handlePhysicalContainerVolumeReady(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	_ physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	volumeID := data.volumeID
	inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, reconciler.orchestrator, volumeID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		data.state = physicalContainerVolumeStateMissing
		data.volumeID = volumeID
		data.failureMessage = ""
		return noChange
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect runtime volume", "VolumeID", volumeID)
		data.state = physicalContainerVolumeStateInspectFailed
		data.volumeID = volumeID
		data.failureMessage = fmt.Sprintf("Failed to inspect runtime volume: %v", inspectErr)
		return noChange
	}

	data.state = physicalContainerVolumeStateReady
	data.volumeID = inspectedVolume.Name
	data.failureMessage = ""
	return applyReadyPhysicalContainerVolumeStatus(volume, inspectedVolume)
}

func handlePhysicalContainerVolumeUnknown(
	ctx context.Context, reconciler *PhysicalContainerVolumeReconciler, volume *apiv2.PhysicalContainerVolume,
	_ physicalContainerVolumeState, data *physicalContainerVolumeData, log logr.Logger,
) objectChange {
	inspected, inspectErr := inspectPhysicalContainerVolume(ctx, reconciler.orchestrator, data.volumeID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		data.state, data.failureMessage = physicalContainerVolumeStateMissing, ""
		return noChange
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect unavailable volume", "VolumeID", data.volumeID)
		data.state = physicalContainerVolumeStateInspectFailed
		data.failureMessage = fmt.Sprintf("Failed to inspect runtime volume: %v", inspectErr)
		return noChange
	}
	data.state, data.volumeID, data.failureMessage = physicalContainerVolumeStateReady, inspected.Name, ""
	return applyReadyPhysicalContainerVolumeStatus(volume, inspected)
}

func (r *PhysicalContainerVolumeReconciler) schedulePhysicalContainerVolumeCreate(
	volume *apiv2.PhysicalContainerVolume,
	currentData *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	stateKey := physicalContainerVolumeDataKey(volume)
	volumeConfig := &volume.Spec
	data := &physicalContainerVolumeData{state: physicalContainerVolumeStateCreate}
	data.workInProgress = true
	if !r.volumeData.Update(volume.NamespacedName(), stateKey, data) {
		return additionalReconciliationNeeded
	}
	currentData.UpdateFrom(data)
	volumeSnapshot := volume.DeepCopy()
	dataSnapshot := data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.createPhysicalContainerVolume(operationCtx, volumeSnapshot, stateKey, dataSnapshot, log)
	})
	if enqueueErr != nil {
		log.Error(enqueueErr, "Failed to queue PhysicalContainerVolume create", "VolumeName", volumeConfig.VolumeName)
		data.state = physicalContainerVolumeStateCreateFailed
		data.workInProgress = false
		data.failureMessage = fmt.Sprintf("Failed to queue runtime volume create: %v", enqueueErr)
		currentData.UpdateFrom(data)
		return noChange
	}

	log.V(1).Info("Queued PhysicalContainerVolume create", "VolumeName", volumeConfig.VolumeName)
	return noChange
}

func (r *PhysicalContainerVolumeReconciler) createPhysicalContainerVolume(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	stateKey physicalContainerVolumeDataStateKey,
	data *physicalContainerVolumeData,
	log logr.Logger,
) {
	volumeConfig := &volume.Spec
	inspected, inspectErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volumeConfig.VolumeName)
	if inspectErr == nil {
		data.state, data.volumeID, data.failureMessage = physicalContainerVolumeStateCreated, inspected.Name, ""
		log.V(1).Info("Using existing runtime volume", "VolumeID", inspected.Name)
		r.queuePhysicalContainerVolumeDataResult(volume, stateKey, data)
		return
	}
	if !errors.Is(inspectErr, containers.ErrNotFound) {
		log.Error(inspectErr, "Failed to inspect runtime volume before creation", "VolumeName", volumeConfig.VolumeName)
		data.state = physicalContainerVolumeStateCreateRetry
		data.failureMessage = fmt.Sprintf("Failed to inspect runtime volume before creation: %v", inspectErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		r.queuePhysicalContainerVolumeDataResult(volume, stateKey, data)
		return
	}

	createErr := r.orchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
		Name:   volumeConfig.VolumeName,
		Labels: physicalContainerVolumeCreationLabels(volume, log),
	})
	applyPhysicalContainerVolumeCreateResult(volume, data, createErr, log)
	r.queuePhysicalContainerVolumeDataResult(volume, stateKey, data)
}

func applyPhysicalContainerVolumeCreateResult(
	volume *apiv2.PhysicalContainerVolume,
	data *physicalContainerVolumeData,
	createErr error,
	log logr.Logger,
) {
	volumeConfig := &volume.Spec
	if createErr != nil {
		log.Error(createErr, "Runtime volume create returned an error; reconciliation will verify the outcome", "VolumeName", volumeConfig.VolumeName)
		data.state = physicalContainerVolumeStateCreateRetry
		data.failureMessage = fmt.Sprintf("Runtime volume create returned an error: %v", createErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return
	}
	data.state, data.volumeID, data.failureMessage, data.retryAfter = physicalContainerVolumeStateCreated, volumeConfig.VolumeName, "", time.Time{}
}

func (r *PhysicalContainerVolumeReconciler) queuePhysicalContainerVolumeDataResult(
	volume *apiv2.PhysicalContainerVolume,
	stateKey physicalContainerVolumeDataStateKey,
	result *physicalContainerVolumeData,
) {
	result.workInProgress = false

	queued := r.volumeData.QueueDeferredOpForStateKey(volume.NamespacedName(), stateKey, func(name types.NamespacedName, currentStateKey physicalContainerVolumeDataStateKey, _ *apiv2.PhysicalContainerVolume) {
		_ = r.volumeData.Update(name, currentStateKey, result)
	})
	if queued {
		r.ScheduleReconciliation(volume.NamespacedName())
	}
}

func handlePhysicalContainerVolumeCreating(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.state != physicalContainerVolumeStateCreate {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}
	if !data.operationInProgress() {
		return reconciler.schedulePhysicalContainerVolumeCreate(volume, data, log)
	}

	log.V(1).Info("Runtime volume creation is still in progress")
	return noChange
}

func handlePhysicalContainerVolumeCreated(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	_ physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.state != physicalContainerVolumeStateCreated {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, physicalContainerVolumeStateCreate, data, log)
	}

	log.V(1).Info("Runtime volume resolved; saving volume status", "VolumeID", data.volumeID)
	inspected, inspectErr := inspectPhysicalContainerVolume(ctx, reconciler.orchestrator, data.volumeID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		data.state, data.failureMessage = physicalContainerVolumeStateMissing, ""
		return additionalReconciliationNeeded
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect resolved volume", "VolumeID", data.volumeID)
		data.state, data.failureMessage = physicalContainerVolumeStateInspectFailed, inspectErr.Error()
		return additionalReconciliationNeeded
	}
	data.state, data.volumeID, data.failureMessage = physicalContainerVolumeStateReady, inspected.Name, ""
	return applyReadyPhysicalContainerVolumeStatus(volume, inspected)
}

func handlePhysicalContainerVolumeCreateFailed(
	_ context.Context,
	_ *PhysicalContainerVolumeReconciler,
	_ *apiv2.PhysicalContainerVolume,
	_ physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	log.V(1).Info("Runtime volume creation failed; saving volume status", "Message", data.failureMessage)
	// The failure is terminal: spec is immutable, so no further reconciliation can make progress.
	return noChange
}

func handlePhysicalContainerVolumeRecoverableCreateFailed(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.state != physicalContainerVolumeStateCreateRetry {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}

	volumeConfig := &volume.Spec
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}

	inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, reconciler.orchestrator, volumeConfig.VolumeName)
	if inspectErr == nil {
		previousFailure := data.failureMessage
		data.state = physicalContainerVolumeStateCreated
		data.volumeID = inspectedVolume.Name
		data.failureMessage = ""
		data.retryAfter = time.Time{}
		log.Info("Resolved runtime volume after a failed attempt", "VolumeID", inspectedVolume.Name, "PreviousFailure", previousFailure)
		return additionalReconciliationNeeded
	}
	if !errors.Is(inspectErr, containers.ErrNotFound) {
		log.Error(inspectErr, "Failed to verify runtime volume creation outcome", "VolumeName", volumeConfig.VolumeName)
		data.failureMessage = fmt.Sprintf("Failed to verify whether runtime volume creation succeeded: %v", inspectErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return additionalReconciliationNeeded
	}

	log.V(1).Info("Retrying runtime volume creation", "VolumeName", volumeConfig.VolumeName)
	return reconciler.schedulePhysicalContainerVolumeCreate(volume, data, log)
}

func handleUnknownPhysicalContainerVolumeDataReason(
	_ context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {

	data.state = physicalContainerVolumeStateInvalid
	data.workInProgress = false
	data.failureMessage = fmt.Sprintf("Physical container volume reached invalid reconciliation state %v.", state)
	log.Error(fmt.Errorf("invalid physical volume state %v", state), "Runtime volume operation reached invalid state")
	return noChange
}

func handlePhysicalContainerVolumeTerminal(
	_ context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	_ physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	return noChange
}

func (r *PhysicalContainerVolumeReconciler) handleDeletionRequest(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	data *physicalContainerVolumeData,
	log logr.Logger,
) (change objectChange, delay AdditionalReconciliationDelay) {
	delay = StandardDelay
	if data == nil {
		return deleteFinalizer(volume, physicalContainerVolumeFinalizer, log), delay
	}
	defer func() {
		if data != nil {
			_ = r.volumeData.Update(volume.NamespacedName(), physicalContainerVolumeDataKey(volume), data)
			dataChange, projectionDelay, valid := data.applyTo(volume)
			change |= dataChange
			delay = projectionDelay
			if !valid {
				log.Error(fmt.Errorf("invalid physical container volume state %v", data.state),
					"PhysicalContainerVolume reached invalid reconciliation state")
			}
		}
	}()
	if data.state == physicalContainerVolumeStateRemovalAbandoned || data.state == physicalContainerVolumeStateRemove || data.state == physicalContainerVolumeStateRemoveRetry || data.state == physicalContainerVolumeStateRemoved {
		change = handlePhysicalContainerVolumeRemovalState(ctx, r, volume, data.state, data, log)
		if !hasFinalizer(volume, physicalContainerVolumeFinalizer) {
			data = nil
		}
		return change, delay
	}
	createFailed := data.state == physicalContainerVolumeStateCreateFailed
	if !volume.Spec.RemoveRuntimeVolumeOnDelete || createFailed {
		dataChange, _, valid := data.applyTo(volume)
		if !valid || dataChange&statusChanged != 0 {
			return dataChange | additionalReconciliationNeeded, delay
		}
		r.volumeData.DeleteByNamespacedName(volume.NamespacedName())
		data = nil
		return deleteFinalizer(volume, physicalContainerVolumeFinalizer, log), delay
	}

	volumeID := ""
	if data != nil {
		volumeID = data.volumeID
	}
	resolveOwnedVolumeByName := volumeID == ""
	return r.schedulePhysicalContainerVolumeRemoval(volume, data, volumeID, resolveOwnedVolumeByName, log), delay
}

func handlePhysicalContainerVolumeRemovalState(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	switch data.state {
	case physicalContainerVolumeStateRemove:
		return handlePhysicalContainerVolumeRemovalInProgress(ctx, reconciler, volume, state, data, log)
	case physicalContainerVolumeStateRemoveRetry:
		return handlePhysicalContainerVolumeRemovalFailed(ctx, reconciler, volume, state, data, log)
	case physicalContainerVolumeStateRemoved:
		return handlePhysicalContainerVolumeRemovalCompleted(ctx, reconciler, volume, state, data, log)
	case physicalContainerVolumeStateRemovalAbandoned:
		return handlePhysicalContainerVolumeRemovalAbandoned(ctx, reconciler, volume, state, data, log)
	default:
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}
}

func handlePhysicalContainerVolumeRemovalInProgress(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.state != physicalContainerVolumeStateRemove {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}

	log.V(1).Info("Runtime volume removal is still in progress", "VolumeID", data.volumeID)
	return additionalReconciliationNeeded
}

func handlePhysicalContainerVolumeRemovalFailed(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.state != physicalContainerVolumeStateRemoveRetry {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}

	if reconciler.namespaceDeletionVolumeRemovalTimeoutExpired(ctx, volume, log) {

		data.state = physicalContainerVolumeStateRemovalAbandoned
		data.failureMessage = fmt.Sprintf(
			"Stopped retrying runtime volume removal after reaching the namespace cleanup deadline; the runtime volume was retained. Last failure: %s",
			data.failureMessage,
		)
		data.retryAfter = time.Time{}
		log.Info(
			"Stopped retrying runtime volume removal after namespace cleanup deadline",
			"Namespace", volume.Namespace,
			"VolumeID", data.volumeID,
		)
		return additionalReconciliationNeeded
	}

	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	return reconciler.schedulePhysicalContainerVolumeRemoval(volume, data, data.volumeID, data.resolveByName, log)
}

func handlePhysicalContainerVolumeRemovalCompleted(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.state != physicalContainerVolumeStateRemoved {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}

	dataChange, _, valid := data.applyTo(volume)
	if !valid || dataChange&statusChanged != 0 {
		return dataChange | additionalReconciliationNeeded
	}
	reconciler.volumeData.DeleteByNamespacedName(volume.NamespacedName())
	return deleteFinalizer(volume, physicalContainerVolumeFinalizer, log)
}

func handlePhysicalContainerVolumeRemovalAbandoned(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.state != physicalContainerVolumeStateRemovalAbandoned {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}

	dataChange, _, valid := data.applyTo(volume)
	if !valid || dataChange&statusChanged != 0 {
		return dataChange | additionalReconciliationNeeded
	}
	reconciler.volumeData.DeleteByNamespacedName(volume.NamespacedName())
	return deleteFinalizer(volume, physicalContainerVolumeFinalizer, log)
}

func (r *PhysicalContainerVolumeReconciler) namespaceDeletionVolumeRemovalTimeoutExpired(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	log logr.Logger,
) bool {
	namespace := apiv2.Namespace{}
	namespaceGetErr := r.Client.Get(ctx, types.NamespacedName{Name: volume.Namespace}, &namespace)
	if apierrors.IsNotFound(namespaceGetErr) {
		// A namespace cannot normally disappear while a child finalizer remains. If its finalizer
		// was removed manually, do not leave the orphaned child blocked on runtime cleanup.
		log.Info("Namespace no longer exists; stopping runtime volume removal", "Namespace", volume.Namespace)
		return true
	}
	if namespaceGetErr != nil {
		log.Error(namespaceGetErr, "Failed to check namespace deletion deadline", "Namespace", volume.Namespace)
		return false
	}
	if namespace.DeletionTimestamp == nil || namespace.DeletionTimestamp.IsZero() {
		return false
	}

	if volume.DeletionTimestamp == nil || volume.DeletionTimestamp.IsZero() {
		return false
	}
	removalDeadline := volume.DeletionTimestamp.Add(physicalContainerVolumeRemovalRetryTimeout)
	return !time.Now().Before(removalDeadline)
}

func (r *PhysicalContainerVolumeReconciler) schedulePhysicalContainerVolumeRemoval(
	volume *apiv2.PhysicalContainerVolume,
	currentData *physicalContainerVolumeData,
	volumeID string,
	resolveOwnedVolumeByName bool,
	log logr.Logger,
) objectChange {
	stateKey := physicalContainerVolumeDataKey(volume)
	data := &physicalContainerVolumeData{
		state: physicalContainerVolumeStateRemove,

		volumeID:      volumeID,
		resolveByName: resolveOwnedVolumeByName, workInProgress: true,
	}
	if !r.volumeData.Update(volume.NamespacedName(), stateKey, data) {
		return additionalReconciliationNeeded
	}
	currentData.UpdateFrom(data)
	volumeSnapshot := volume.DeepCopy()
	dataSnapshot := data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.removePhysicalContainerVolume(operationCtx, volumeSnapshot, stateKey, dataSnapshot, log)
	})
	if enqueueErr == nil {
		log.V(1).Info("Queued PhysicalContainerVolume removal", "VolumeID", volumeID)
		return additionalReconciliationNeeded
	}

	log.Error(enqueueErr, "Failed to queue PhysicalContainerVolume removal", "VolumeID", volumeID)
	data.state = physicalContainerVolumeStateRemoveRetry
	data.workInProgress = false
	data.failureMessage = fmt.Sprintf("Failed to queue runtime volume removal: %v", enqueueErr)
	data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
	currentData.UpdateFrom(data)
	return additionalReconciliationNeeded
}

func (r *PhysicalContainerVolumeReconciler) removePhysicalContainerVolume(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	stateKey physicalContainerVolumeDataStateKey,
	data *physicalContainerVolumeData,
	log logr.Logger,
) {
	volumeID := data.volumeID
	var removeErr error
	if data.resolveByName {
		// An unresolved create may have succeeded, but an unobserved existing volume was not adopted.
		inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volume.Spec.VolumeName)
		switch {
		case errors.Is(inspectErr, containers.ErrNotFound):
			volumeID = ""
		case inspectErr != nil:
			removeErr = fmt.Errorf("verify whether runtime volume creation succeeded: %w", inspectErr)
		case !physicalContainerVolumeBelongsToResource(inspectedVolume, volume):
			volumeID = ""
		case inspectedVolume.Name == "":
			removeErr = errors.New("owned runtime volume inspection returned an empty name")
		default:
			volumeID = inspectedVolume.Name
		}
	}

	if removeErr == nil && volumeID != "" {
		removeErr = r.removeRuntimeVolume(ctx, volumeID)
	}

	data.volumeID = volumeID
	if removeErr != nil {
		log.Error(removeErr, "Failed to remove runtime volume", "VolumeID", volumeID)

		data.state = physicalContainerVolumeStateRemoveRetry
		data.failureMessage = fmt.Sprintf("Failed to remove runtime volume: %v", removeErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
	} else {

		data.state = physicalContainerVolumeStateRemoved
		data.failureMessage = ""
		data.retryAfter = time.Time{}
	}
	r.queuePhysicalContainerVolumeDataResult(volume, stateKey, data)
}

func (r *PhysicalContainerVolumeReconciler) removeRuntimeVolume(ctx context.Context, volumeID string) error {
	_, inspectErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volumeID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		return nil
	}
	if inspectErr != nil {
		return fmt.Errorf("inspect runtime volume before removal: %w", inspectErr)
	}

	_, removeErr := r.orchestrator.RemoveVolumes(ctx, containers.RemoveVolumesOptions{
		Volumes: []string{volumeID},
		Force:   false,
	})
	if removeErr == nil || errors.Is(removeErr, containers.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("remove runtime volume: %w", removeErr)
}

func inspectPhysicalContainerVolume(
	ctx context.Context,
	orchestrator containers.VolumeOrchestrator,
	volume string,
) (*containers.InspectedVolume, error) {
	inspectedVolumes, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
		Volumes: []string{volume},
	})
	// Orchestrators report ErrIncomplete alongside successfully inspected volumes, so prefer the
	// result over the error.
	if len(inspectedVolumes) > 0 {
		return &inspectedVolumes[0], nil
	}
	if inspectErr != nil {
		return nil, inspectErr
	}

	return nil, containers.ErrNotFound
}

func physicalContainerVolumeCreationLabels(volume *apiv2.PhysicalContainerVolume, log logr.Logger) map[string]string {
	volumeConfig := &volume.Spec
	creationLabels := physicalResourceCreationLabels(
		volumeConfig.Labels,
		!volumeConfig.RemoveRuntimeVolumeOnDelete,
		volume.UID,
		log,
	)
	return v2LabelsToMap(creationLabels)
}

func physicalContainerVolumeBelongsToResource(
	inspectedVolume *containers.InspectedVolume,
	volume *apiv2.PhysicalContainerVolume,
) bool {
	return volume.UID != "" && inspectedVolume.Labels[uidLabel] == string(volume.UID)
}

func applyReadyPhysicalContainerVolumeStatus(
	volume *apiv2.PhysicalContainerVolume,
	inspectedVolume *containers.InspectedVolume,
) objectChange {
	change := setValue(&volume.Status.VolumeID, inspectedVolume.Name)
	change |= setValue(&volume.Status.Driver, inspectedVolume.Driver)
	change |= setValue(&volume.Status.MountPoint, inspectedVolume.MountPoint)
	change |= setValue(&volume.Status.Scope, inspectedVolume.Scope)
	change |= setTimestamp(&volume.Status.CreatedAt, metav1.NewMicroTime(inspectedVolume.CreatedAt))
	change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhaseReady)
	change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionTrue, apiv2.PhysicalContainerVolumeReasonVolumeAvailable, "Runtime volume is available.")
	return change | additionalReconciliationNeeded
}
