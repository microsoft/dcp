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
	"github.com/microsoft/dcp/pkg/resiliency"
)

const (
	physicalContainerVolumeRemovalRetryTimeout      = 30 * time.Second
	physicalContainerVolumeNamespaceDeletionTimeout = 90 * time.Second
)

var (
	physicalContainerVolumeFinalizer string = fmt.Sprintf("%s/physicalcontainervolume-reconciler", apiv2.GroupVersion.Group)

	physicalContainerVolumeDataInitializers = map[apiv2.ConditionReason]physicalContainerVolumeDataInitializerFunc{
		apiv2.PhysicalContainerVolumeReasonCreating:                        handlePhysicalContainerVolumeCreating,
		apiv2.PhysicalContainerVolumeReasonCreated:                         handlePhysicalContainerVolumeCreated,
		apiv2.PhysicalContainerVolumeReasonCreateFailed:                    handlePhysicalContainerVolumeCreateFailure,
		apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed: handlePhysicalContainerVolumeCreateFailure,
		"": handleUnknownPhysicalContainerVolumeDataReason,
	}

	physicalContainerVolumeDeletionDataInitializers = map[apiv2.ConditionReason]physicalContainerVolumeDataInitializerFunc{
		apiv2.PhysicalContainerVolumeReasonCreating:                        handlePhysicalContainerVolumeCreateInProgressDuringDeletion,
		apiv2.PhysicalContainerVolumeReasonCreated:                         handlePhysicalContainerVolumeCreatedDuringDeletion,
		apiv2.PhysicalContainerVolumeReasonCreateFailed:                    handlePhysicalContainerVolumeCreateFailureDuringDeletion,
		apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed: handlePhysicalContainerVolumeRecoverableCreateFailureDuringDeletion,
		apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoving:           handlePhysicalContainerVolumeRemovalInProgress,
		apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoveFailed:       handlePhysicalContainerVolumeRemovalFailed,
		apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoved:            handlePhysicalContainerVolumeRemovalCompleted,
		apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemovalAbandoned:   handlePhysicalContainerVolumeRemovalAbandoned,
		"": handleUnknownPhysicalContainerVolumeDataReason,
	}
)

type physicalContainerVolumeDataInitializerFunc = stateInitializerFunc[
	apiv2.PhysicalContainerVolume, *apiv2.PhysicalContainerVolume,
	PhysicalContainerVolumeReconciler, *PhysicalContainerVolumeReconciler,
	apiv2.ConditionReason,
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
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNamespace), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *PhysicalContainerVolumeReconciler) requestReconcileForNamespace(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	namespace := obj.(*apiv2.Namespace)
	var volumeList apiv2.PhysicalContainerVolumeList
	listErr := r.List(ctx, &volumeList, ctrl_client.InNamespace(namespace.Name))
	if listErr != nil {
		r.Log.Error(listErr, "Failed to list PhysicalContainerVolumes for namespace", "Namespace", namespace.Name)
		return nil
	}

	requests := make([]reconcile.Request, len(volumeList.Items))
	for i := range volumeList.Items {
		requests[i] = reconcile.Request{NamespacedName: volumeList.Items[i].NamespacedName()}
	}

	r.Log.V(1).Info("Namespace updated, requesting PhysicalContainerVolume reconciliation", "Namespace", namespace.Name, "Volumes", len(requests))
	return requests
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

	var change objectChange
	var onStatusDurable func()
	patch := ctrl_client.MergeFromWithOptions(volume.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if volume.DeletionTimestamp != nil && !volume.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, &volume, log)
	} else if change = ensureFinalizer(&volume, physicalContainerVolumeFinalizer, log); change != noChange {
		// Make additional changes during the next reconciliation.
	} else {
		change, onStatusDurable = r.managePhysicalContainerVolume(ctx, &volume, log)
	}

	return r.SaveChangesWithDelay(ctx, &volume, patch, change, physicalContainerVolumeReconcileDelay(&volume), onStatusDurable, log)
}

func physicalContainerVolumeReconcileDelay(volume *apiv2.PhysicalContainerVolume) AdditionalReconciliationDelay {
	readyCondition := apimeta.FindStatusCondition(volume.Status.Conditions, string(apiv2.ConditionReady))
	if volume.DeletionTimestamp != nil && !volume.DeletionTimestamp.IsZero() {
		if readyCondition != nil &&
			readyCondition.Reason == string(apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoveFailed) {
			return LongDelay
		}
		return StandardDelay
	}

	if volume.Status.Phase == apiv2.PhysicalContainerVolumePhaseFailed || readyCondition == nil {
		return StandardDelay
	}

	switch apiv2.ConditionReason(readyCondition.Reason) {
	case apiv2.PhysicalContainerVolumeReasonVolumeAvailable,
		apiv2.PhysicalContainerVolumeReasonRuntimeVolumeMissing:
		return MonitoringDelay
	case apiv2.PhysicalContainerVolumeReasonCreateFailed,
		apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed,
		apiv2.PhysicalContainerVolumeReasonRuntimeVolumeInspectFailed,
		apiv2.PhysicalResourceReasonNamespaceLookupFailed,
		apiv2.PhysicalResourceReasonOperationStateInvalid:
		return LongDelay
	default:
		return StandardDelay
	}
}

func (r *PhysicalContainerVolumeReconciler) onTerminalCreateFailureStatusDurable(
	stateKey physicalContainerVolumeDataStateKey,
	data *physicalContainerVolumeData,
) func() {
	if data.progress != physicalContainerVolumeOperationFailed {
		return nil
	}
	if data.conditionReason != apiv2.PhysicalContainerVolumeReasonCreateFailed {
		return nil
	}

	return func() {
		r.volumeData.DeleteByStateKey(stateKey)
	}
}

func (r *PhysicalContainerVolumeReconciler) managePhysicalContainerVolume(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	log logr.Logger,
) (objectChange, func()) {
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, r.Client, volume.Namespace)
	if !namespaceReady {
		phase := apiv2.PhysicalContainerVolumePhasePending
		message := namespaceReadinessMessage(volume.Namespace, namespaceReason)
		change := noChange
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get namespace", "Namespace", volume.Namespace)
			phase = apiv2.PhysicalContainerVolumePhaseUnknown
			message = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
			change |= additionalReconciliationNeeded
		}
		change |= setValue(&volume.Status.Phase, phase)
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, namespaceReason, message)
		return change, nil
	}

	change := noChange
	stateKey, data := r.volumeData.BorrowByNamespacedName(volume.NamespacedName())
	if data != nil {
		change |= data.applyTo(volume)
		initializer := getStateInitializer(physicalContainerVolumeDataInitializers, data.conditionReason, log)
		change |= initializer(ctx, r, volume, data.conditionReason, data, log)
		return change, r.onTerminalCreateFailureStatusDurable(stateKey, data)
	}

	if volume.Status.Phase == apiv2.PhysicalContainerVolumePhaseFailed {
		return change, nil
	}

	volumeID := volume.Spec.VolumeID
	if volumeID == "" {
		volumeID = volume.Status.VolumeID
	}
	if volumeID == "" {
		return r.schedulePhysicalContainerVolumeCreate(volume, log), nil
	}

	return change | r.applyRuntimeVolumeStatus(ctx, volume, volumeID, log), nil
}

// Inspects the runtime volume and projects the result onto the resource status.
func (r *PhysicalContainerVolumeReconciler) applyRuntimeVolumeStatus(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	volumeID string,
	log logr.Logger,
) objectChange {
	inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volumeID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		change := setValue(&volume.Status.VolumeID, volumeID)
		change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhaseUnknown)
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonRuntimeVolumeMissing, "Runtime volume was not found.")
		// Keep observing: a tracked volume may not have been created yet, and a runtime that is
		// only reporting the volume as absent because it is unhealthy recovers on its own.
		return change | additionalReconciliationNeeded
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect runtime volume", "VolumeID", volumeID)
		change := setValue(&volume.Status.VolumeID, volumeID)
		change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhaseUnknown)
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonRuntimeVolumeInspectFailed, fmt.Sprintf("Failed to inspect runtime volume: %v", inspectErr))
		// Inspection failures are usually transient, and repeating an identical failure produces
		// no status change, so retry explicitly rather than settling into a permanent failure.
		return change | additionalReconciliationNeeded
	}

	return applyReadyPhysicalContainerVolumeStatus(volume, inspectedVolume)
}

func (r *PhysicalContainerVolumeReconciler) schedulePhysicalContainerVolumeCreate(volume *apiv2.PhysicalContainerVolume, log logr.Logger) objectChange {
	volumeConfig := volume.Spec.Volume
	stateKey := physicalContainerVolumeDataKey(volume)
	data := &physicalContainerVolumeData{conditionReason: apiv2.PhysicalContainerVolumeReasonCreating}
	data.progress = physicalContainerVolumeOperationInProgress
	r.volumeData.Store(volume.NamespacedName(), stateKey, data)
	volumeSnapshot := volume.DeepCopy()
	dataSnapshot := data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.createPhysicalContainerVolume(operationCtx, volumeSnapshot, stateKey, dataSnapshot, log)
	})
	if enqueueErr != nil {
		r.volumeData.DeleteByNamespacedName(volume.NamespacedName())
		log.Error(enqueueErr, "Failed to queue PhysicalContainerVolume create", "VolumeName", volumeConfig.VolumeName)
		change := setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhaseFailed)
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonCreateFailed, fmt.Sprintf("Failed to queue runtime volume create: %v", enqueueErr))
		return change
	}

	log.V(1).Info("Queued PhysicalContainerVolume create", "VolumeName", volumeConfig.VolumeName)
	return data.applyTo(volume)
}

func (r *PhysicalContainerVolumeReconciler) createPhysicalContainerVolume(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	stateKey physicalContainerVolumeDataStateKey,
	data *physicalContainerVolumeData,
	log logr.Logger,
) {
	volumeConfig := volume.Spec.Volume
	if volumeConfig.ReplaceExisting {
		replaced, replaceErr := r.replacePhysicalContainerVolume(ctx, volume, data, log)
		if replaceErr != nil {
			log.Error(replaceErr, "Failed to replace existing runtime volume", "VolumeName", volumeConfig.VolumeName)
			data.conditionReason = apiv2.PhysicalContainerVolumeReasonExistingVolumeReplacementFailed
			data.progress = physicalContainerVolumeOperationRetryPending
			data.failureMessage = fmt.Sprintf("Failed to replace existing runtime volume: %v", replaceErr)
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			r.queuePhysicalContainerVolumeDataResult(volume, stateKey, data)
			return
		}
		if !replaced {
			r.queuePhysicalContainerVolumeDataResult(volume, stateKey, data)
			return
		}
	}

	createErr := r.orchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
		Name:   volumeConfig.VolumeName,
		Labels: physicalContainerVolumeCreationLabels(volume, log),
	})
	r.applyPhysicalContainerVolumeCreateResult(ctx, volume, data, createErr, log)
	r.queuePhysicalContainerVolumeDataResult(volume, stateKey, data)
}

func (r *PhysicalContainerVolumeReconciler) replacePhysicalContainerVolume(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	data *physicalContainerVolumeData,
	log logr.Logger,
) (bool, error) {
	volumeName := volume.Spec.Volume.VolumeName
	inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volumeName)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		return true, nil
	}
	if inspectErr != nil {
		return false, fmt.Errorf("inspect runtime volume %q: %w", volumeName, inspectErr)
	}
	if inspectedVolume.Name == "" {
		return false, fmt.Errorf("inspect runtime volume %q returned an empty name", volumeName)
	}
	if physicalContainerVolumeBelongsToResource(inspectedVolume, volume) {
		data.conditionReason = apiv2.PhysicalContainerVolumeReasonCreated
		data.progress = physicalContainerVolumeOperationCompleted
		data.volumeID = inspectedVolume.Name
		data.failureMessage = ""
		data.retryAfter = time.Time{}
		log.V(1).Info("Adopted runtime volume created by an earlier attempt", "VolumeID", inspectedVolume.Name)
		return false, nil
	}

	removeErr := r.removeRuntimeVolume(ctx, inspectedVolume.Name)
	if removeErr != nil {
		return false, fmt.Errorf("remove runtime volume %q: %w", inspectedVolume.Name, removeErr)
	}

	log.V(1).Info(
		"Removed existing runtime volume before replacement",
		"VolumeID", inspectedVolume.Name,
		"VolumeName", inspectedVolume.Name,
	)
	return true, nil
}

func (r *PhysicalContainerVolumeReconciler) applyPhysicalContainerVolumeCreateResult(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	data *physicalContainerVolumeData,
	createErr error,
	log logr.Logger,
) {
	volumeConfig := volume.Spec.Volume
	if createErr != nil {
		log.Error(createErr, "Failed to create runtime volume", "VolumeName", volumeConfig.VolumeName)
		data.failureMessage = fmt.Sprintf("Failed to create runtime volume: %v", createErr)
	}

	inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volumeConfig.VolumeName)
	if inspectErr == nil && physicalContainerVolumeBelongsToResource(inspectedVolume, volume) {
		data.conditionReason = apiv2.PhysicalContainerVolumeReasonCreated
		data.progress = physicalContainerVolumeOperationCompleted
		data.volumeID = inspectedVolume.Name
		data.failureMessage = ""
		data.retryAfter = time.Time{}
		return
	}
	if inspectErr == nil {
		if volumeConfig.ReplaceExisting {
			data.conditionReason = apiv2.PhysicalContainerVolumeReasonCreateFailed
			data.progress = physicalContainerVolumeOperationRetryPending
			if data.failureMessage == "" {
				data.failureMessage = fmt.Sprintf("Runtime volume name %q was claimed during replacement.", volumeConfig.VolumeName)
			}
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		} else {
			data.conditionReason = apiv2.PhysicalContainerVolumeReasonCreateFailed
			data.progress = physicalContainerVolumeOperationFailed
			data.failureMessage = fmt.Sprintf("Runtime volume name %q is already in use.", volumeConfig.VolumeName)
			data.retryAfter = time.Time{}
		}
		return
	}

	if createErr == nil {
		data.failureMessage = fmt.Sprintf("Runtime volume create succeeded, but the volume could not be inspected: %v", inspectErr)
	} else if !errors.Is(inspectErr, containers.ErrNotFound) {
		data.failureMessage = fmt.Sprintf("Failed to create runtime volume: %v; failed to verify whether creation succeeded: %v", createErr, inspectErr)
	} else {
		data.failureMessage = fmt.Sprintf("Failed to create runtime volume: %v", createErr)
	}
	data.conditionReason = apiv2.PhysicalContainerVolumeReasonCreateFailed
	data.progress = physicalContainerVolumeOperationRetryPending
	data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
}

func (r *PhysicalContainerVolumeReconciler) queuePhysicalContainerVolumeDataResult(
	volume *apiv2.PhysicalContainerVolume,
	stateKey physicalContainerVolumeDataStateKey,
	result *physicalContainerVolumeData,
) {
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
	conditionReason apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationInProgress {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, conditionReason, data, log)
	}

	log.V(1).Info("Runtime volume creation is still in progress")
	return noChange
}

func handlePhysicalContainerVolumeCreated(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	_ apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationCompleted {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, apiv2.PhysicalContainerVolumeReasonCreated, data, log)
	}

	log.V(1).Info("Runtime volume created; saving volume status", "VolumeID", data.volumeID)
	return reconciler.applyRuntimeVolumeStatus(ctx, volume, data.volumeID, log)
}

func handlePhysicalContainerVolumeCreateFailure(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	conditionReason apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalContainerVolumeOperationRetryPending:
		return handlePhysicalContainerVolumeRecoverableCreateFailed(ctx, reconciler, volume, conditionReason, data, log)
	case physicalContainerVolumeOperationFailed:
		return handlePhysicalContainerVolumeCreateFailed(ctx, reconciler, volume, conditionReason, data, log)
	default:
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, conditionReason, data, log)
	}
}

func handlePhysicalContainerVolumeCreateFailed(
	_ context.Context,
	_ *PhysicalContainerVolumeReconciler,
	_ *apiv2.PhysicalContainerVolume,
	_ apiv2.ConditionReason,
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
	conditionReason apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationRetryPending {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, conditionReason, data, log)
	}

	volumeConfig := volume.Spec.Volume
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}

	inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, reconciler.orchestrator, volumeConfig.VolumeName)
	if inspectErr == nil {
		belongsToResource := physicalContainerVolumeBelongsToResource(inspectedVolume, volume)
		if !belongsToResource && !volumeConfig.ReplaceExisting {
			data.conditionReason = apiv2.PhysicalContainerVolumeReasonCreateFailed
			data.progress = physicalContainerVolumeOperationFailed
			data.failureMessage = fmt.Sprintf("Runtime volume name %q is already in use.", volumeConfig.VolumeName)
			data.retryAfter = time.Time{}
			stateKey, _ := reconciler.volumeData.BorrowByNamespacedName(volume.NamespacedName())
			if reconciler.volumeData.Update(volume.NamespacedName(), stateKey, data) {
				return data.applyTo(volume)
			}
			return additionalReconciliationNeeded
		}
		if !belongsToResource {
			log.V(1).Info("Retrying runtime volume replacement", "VolumeID", inspectedVolume.Name, "VolumeName", inspectedVolume.Name)
			return reconciler.schedulePhysicalContainerVolumeCreate(volume, log)
		}

		data.conditionReason = apiv2.PhysicalContainerVolumeReasonCreated
		data.progress = physicalContainerVolumeOperationCompleted
		data.volumeID = inspectedVolume.Name
		data.failureMessage = ""
		data.retryAfter = time.Time{}
		stateKey, _ := reconciler.volumeData.BorrowByNamespacedName(volume.NamespacedName())
		if reconciler.volumeData.Update(volume.NamespacedName(), stateKey, data) {
			log.V(1).Info("Adopted runtime volume created by an earlier attempt", "VolumeID", inspectedVolume.Name)
			return data.applyTo(volume) | applyReadyPhysicalContainerVolumeStatus(volume, inspectedVolume)
		}
		return additionalReconciliationNeeded
	}
	if !errors.Is(inspectErr, containers.ErrNotFound) {
		data.failureMessage = fmt.Sprintf("Failed to verify whether runtime volume creation succeeded: %v", inspectErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		stateKey, _ := reconciler.volumeData.BorrowByNamespacedName(volume.NamespacedName())
		if reconciler.volumeData.Update(volume.NamespacedName(), stateKey, data) {
			return data.applyTo(volume) | additionalReconciliationNeeded
		}
		return additionalReconciliationNeeded
	}

	log.V(1).Info("Retrying runtime volume creation", "VolumeName", volumeConfig.VolumeName)
	return reconciler.schedulePhysicalContainerVolumeCreate(volume, log)
}

func handleUnknownPhysicalContainerVolumeDataReason(
	_ context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	conditionReason apiv2.ConditionReason,
	_ *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	reconciler.volumeData.DeleteByNamespacedName(volume.NamespacedName())
	message := fmt.Sprintf("Runtime volume operation reached unknown condition reason %q.", conditionReason)
	log.Error(fmt.Errorf("unknown physical volume condition reason %q", conditionReason), "Runtime volume operation reached unknown condition reason")
	change := setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhaseUnknown)
	change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, apiv2.PhysicalResourceReasonOperationStateInvalid, message)
	return change | additionalReconciliationNeeded
}

func (r *PhysicalContainerVolumeReconciler) handleDeletionRequest(ctx context.Context, volume *apiv2.PhysicalContainerVolume, log logr.Logger) objectChange {
	_, data := r.volumeData.BorrowByNamespacedName(volume.NamespacedName())
	if data == nil {
		readyCondition := apimeta.FindStatusCondition(volume.Status.Conditions, string(apiv2.ConditionReady))
		if readyCondition != nil &&
			readyCondition.Reason == string(apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemovalAbandoned) {
			return deleteFinalizer(volume, physicalContainerVolumeFinalizer, log)
		}
		return r.beginPhysicalContainerVolumeRemoval(volume, nil, log)
	}

	change := data.applyTo(volume)
	initializer := getStateInitializer(physicalContainerVolumeDeletionDataInitializers, data.conditionReason, log)
	change |= initializer(ctx, r, volume, data.conditionReason, data, log)
	return change
}

func (r *PhysicalContainerVolumeReconciler) beginPhysicalContainerVolumeRemoval(
	volume *apiv2.PhysicalContainerVolume,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	volumeConfig := volume.Spec.Volume
	if volumeConfig == nil || volumeConfig.RetainRuntimeVolume {
		r.volumeData.DeleteByNamespacedName(volume.NamespacedName())
		return deleteFinalizer(volume, physicalContainerVolumeFinalizer, log)
	}

	volumeID := volume.Status.VolumeID
	if volumeID == "" && data != nil {
		volumeID = data.volumeID
	}
	resolveOwnedVolumeByName := volumeID == "" &&
		data != nil &&
		data.progress == physicalContainerVolumeOperationRetryPending
	if volumeID == "" && !resolveOwnedVolumeByName {
		r.volumeData.DeleteByNamespacedName(volume.NamespacedName())
		return deleteFinalizer(volume, physicalContainerVolumeFinalizer, log)
	}

	return r.schedulePhysicalContainerVolumeRemoval(volume, volumeID, resolveOwnedVolumeByName, log)
}

func handlePhysicalContainerVolumeCreateInProgressDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	conditionReason apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationInProgress {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, conditionReason, data, log)
	}

	// Waiting rather than cancelling: a cancelled create can still produce a runtime volume,
	// and its ID would be lost, leaving the owned volume behind.
	log.V(1).Info("PhysicalContainerVolume is being deleted while creation is in progress")
	return additionalReconciliationNeeded
}

func handlePhysicalContainerVolumeCreatedDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	conditionReason apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationCompleted {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, conditionReason, data, log)
	}

	return reconciler.beginPhysicalContainerVolumeRemoval(volume, data, log)
}

func handlePhysicalContainerVolumeFailedCreateDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	conditionReason apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationFailed {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, conditionReason, data, log)
	}

	reconciler.volumeData.DeleteByNamespacedName(volume.NamespacedName())
	return deleteFinalizer(volume, physicalContainerVolumeFinalizer, log)
}

func handlePhysicalContainerVolumeCreateFailureDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	conditionReason apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalContainerVolumeOperationRetryPending:
		return handlePhysicalContainerVolumeRecoverableCreateFailureDuringDeletion(ctx, reconciler, volume, conditionReason, data, log)
	case physicalContainerVolumeOperationFailed:
		return handlePhysicalContainerVolumeFailedCreateDuringDeletion(ctx, reconciler, volume, conditionReason, data, log)
	default:
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, conditionReason, data, log)
	}
}

func handlePhysicalContainerVolumeRecoverableCreateFailureDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	conditionReason apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationRetryPending {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, conditionReason, data, log)
	}

	return reconciler.beginPhysicalContainerVolumeRemoval(volume, data, log)
}

func handlePhysicalContainerVolumeRemovalInProgress(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	conditionReason apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationInProgress {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, conditionReason, data, log)
	}

	log.V(1).Info("Runtime volume removal is still in progress", "VolumeID", data.volumeID)
	return additionalReconciliationNeeded
}

func handlePhysicalContainerVolumeRemovalFailed(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	conditionReason apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationRetryPending {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, conditionReason, data, log)
	}

	if reconciler.namespaceDeletionVolumeRemovalTimeoutExpired(ctx, volume, log) {
		data.conditionReason = apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemovalAbandoned
		data.progress = physicalContainerVolumeOperationCompleted
		data.failureMessage = fmt.Sprintf(
			"Stopped retrying runtime volume removal after reaching the namespace cleanup deadline; the runtime volume was retained. Last failure: %s",
			data.failureMessage,
		)
		data.retryAfter = time.Time{}
		stateKey, _ := reconciler.volumeData.BorrowByNamespacedName(volume.NamespacedName())
		if reconciler.volumeData.Update(volume.NamespacedName(), stateKey, data) {
			log.Info(
				"Stopped retrying runtime volume removal after namespace cleanup deadline",
				"Namespace", volume.Namespace,
				"VolumeID", data.volumeID,
			)
			return data.applyTo(volume) | additionalReconciliationNeeded
		}
		return additionalReconciliationNeeded
	}

	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	return reconciler.schedulePhysicalContainerVolumeRemoval(volume, data.volumeID, data.resolveByName, log)
}

func handlePhysicalContainerVolumeRemovalCompleted(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	conditionReason apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationCompleted {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, conditionReason, data, log)
	}

	reconciler.volumeData.DeleteByNamespacedName(volume.NamespacedName())
	return deleteFinalizer(volume, physicalContainerVolumeFinalizer, log)
}

func handlePhysicalContainerVolumeRemovalAbandoned(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	conditionReason apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationCompleted {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, conditionReason, data, log)
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

	removalDeadline := namespace.DeletionTimestamp.Add(physicalContainerVolumeNamespaceDeletionTimeout)
	if volume.DeletionTimestamp != nil {
		volumeRemovalDeadline := volume.DeletionTimestamp.Add(physicalContainerVolumeRemovalRetryTimeout)
		if volumeRemovalDeadline.Before(removalDeadline) {
			removalDeadline = volumeRemovalDeadline
		}
	}
	return !time.Now().Before(removalDeadline)
}

func (r *PhysicalContainerVolumeReconciler) schedulePhysicalContainerVolumeRemoval(
	volume *apiv2.PhysicalContainerVolume,
	volumeID string,
	resolveOwnedVolumeByName bool,
	log logr.Logger,
) objectChange {
	stateKey := physicalContainerVolumeDataKey(volume)
	data := &physicalContainerVolumeData{
		conditionReason: apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoving,
		progress:        physicalContainerVolumeOperationInProgress,
		volumeID:        volumeID,
		resolveByName:   resolveOwnedVolumeByName,
	}
	r.volumeData.Store(volume.NamespacedName(), stateKey, data)
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
	data.conditionReason = apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoveFailed
	data.progress = physicalContainerVolumeOperationRetryPending
	data.failureMessage = fmt.Sprintf("Failed to queue runtime volume removal: %v", enqueueErr)
	data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
	_ = r.volumeData.Update(volume.NamespacedName(), stateKey, data)
	return data.applyTo(volume) | additionalReconciliationNeeded
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
		inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volume.Spec.Volume.VolumeName)
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
		data.conditionReason = apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoveFailed
		data.progress = physicalContainerVolumeOperationRetryPending
		data.failureMessage = fmt.Sprintf("Failed to remove runtime volume: %v", removeErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
	} else {
		data.conditionReason = apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoved
		data.progress = physicalContainerVolumeOperationCompleted
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
	if removeErr == nil {
		return nil
	}

	_, confirmErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volumeID)
	if errors.Is(confirmErr, containers.ErrNotFound) {
		return nil
	}

	return fmt.Errorf("remove runtime volume: %w", errors.Join(removeErr, confirmErr))
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
	volumeConfig := volume.Spec.Volume
	creationLabels := physicalResourceCreationLabels(
		volumeConfig.Labels,
		volumeConfig.RetainRuntimeVolume,
		volume.UID,
		log,
	)
	labels := make(map[string]string, len(creationLabels))
	for _, label := range creationLabels {
		labels[label.Key] = label.Value
	}
	return labels
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
