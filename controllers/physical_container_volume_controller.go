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
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/resiliency"
)

var (
	physicalContainerVolumeFinalizer string = fmt.Sprintf("%s/physicalcontainervolume-reconciler", apiv2.GroupVersion.Group)

	physicalContainerVolumeDataInitializers = map[apiv2.ConditionReason]physicalContainerVolumeDataInitializerFunc{
		apiv2.PhysicalContainerVolumeReasonCreating:             handlePhysicalContainerVolumeCreating,
		apiv2.PhysicalContainerVolumeReasonCreated:              handlePhysicalContainerVolumeCreated,
		apiv2.PhysicalContainerVolumeReasonCreateFailed:         handlePhysicalContainerVolumeCreateFailed,
		apiv2.PhysicalContainerVolumeReasonReconciliationFailed: handlePhysicalContainerVolumeRecoverableCreateFailed,
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

	var change objectChange
	var onSuccessfulSave func()
	patch := ctrl_client.MergeFromWithOptions(volume.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if volume.DeletionTimestamp != nil && !volume.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, &volume, log)
	} else if change = ensureFinalizer(&volume, physicalContainerVolumeFinalizer, log); change != noChange {
		// Make additional changes during the next reconciliation.
	} else {
		change, onSuccessfulSave = r.managePhysicalContainerVolume(ctx, &volume, log)
	}

	return r.SaveChangesWithDelay(ctx, &volume, patch, change, physicalContainerVolumeReconcileDelay(&volume), onSuccessfulSave, log)
}

func physicalContainerVolumeReconcileDelay(volume *apiv2.PhysicalContainerVolume) AdditionalReconciliationDelay {
	if volume.DeletionTimestamp != nil && !volume.DeletionTimestamp.IsZero() {
		return LongDelay
	}

	switch volume.Status.Phase {
	case apiv2.PhysicalContainerVolumePhaseReady, apiv2.PhysicalContainerVolumePhaseMissing:
		return MonitoringDelay
	case apiv2.PhysicalContainerVolumePhaseFailed:
		if physicalContainerVolumeFailedTerminally(volume) {
			return StandardDelay
		}
		return LongDelay
	default:
		return StandardDelay
	}
}

func (r *PhysicalContainerVolumeReconciler) physicalContainerVolumeDataSaveCallback(
	stateKey physicalContainerVolumeDataStateKey,
	data *physicalContainerVolumeData,
	change objectChange,
) func() {
	if data == nil {
		return nil
	}

	switch data.conditionReason {
	case apiv2.PhysicalContainerVolumeReasonCreated:
		if data.volumeID == "" {
			return nil
		}
	case apiv2.PhysicalContainerVolumeReasonCreateFailed:
	default:
		return nil
	}

	expectedReason := data.conditionReason
	expectedVolumeID := data.volumeID
	expectedFailureMessage := data.failureMessage
	expectedRetryAfter := data.retryAfter
	return func() {
		r.volumeData.DeleteByStateKeyIf(stateKey, func(current *physicalContainerVolumeData) bool {
			return current.conditionReason == expectedReason &&
				current.volumeID == expectedVolumeID &&
				current.failureMessage == expectedFailureMessage &&
				current.retryAfter.Equal(expectedRetryAfter)
		})
	}
}

func (r *PhysicalContainerVolumeReconciler) managePhysicalContainerVolume(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	log logr.Logger,
) (objectChange, func()) {
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, r.Client, volume.Namespace)
	if !namespaceReady {
		namespacePhase := apiv2.PhysicalContainerVolumePhasePending
		namespaceMessage := namespaceReadinessMessage(volume.Namespace, namespaceReason)
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get namespace", "Namespace", volume.Namespace)
			namespacePhase = apiv2.PhysicalContainerVolumePhaseFailed
			namespaceMessage = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
		}
		change := setValue(&volume.Status.Phase, namespacePhase)
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, namespaceReason, namespaceMessage)
		change |= additionalReconciliationNeeded
		return change, nil
	}

	change := noChange
	stateKey, data := r.volumeData.BorrowByNamespacedName(volume.NamespacedName())
	if data != nil {
		change |= data.applyTo(volume)
		initializer := getStateInitializer(physicalContainerVolumeDataInitializers, data.conditionReason, log)
		change |= initializer(ctx, r, volume, data.conditionReason, data, log)
		if data.conditionReason == apiv2.PhysicalContainerVolumeReasonCreated ||
			data.conditionReason == apiv2.PhysicalContainerVolumeReasonCreateFailed {
			return change, r.physicalContainerVolumeDataSaveCallback(stateKey, data, change)
		}
		return change, nil
	}

	if physicalContainerVolumeFailedTerminally(volume) {
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

func physicalContainerVolumeFailedTerminally(volume *apiv2.PhysicalContainerVolume) bool {
	if volume.Status.Phase != apiv2.PhysicalContainerVolumePhaseFailed {
		return false
	}

	readyCondition := apimeta.FindStatusCondition(volume.Status.Conditions, string(apiv2.ConditionReady))
	return readyCondition != nil && readyCondition.Reason == string(apiv2.PhysicalContainerVolumeReasonCreateFailed)
}

func (r *PhysicalContainerVolumeReconciler) applyRuntimeVolumeStatus(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	volumeID string,
	log logr.Logger,
) objectChange {
	inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volumeID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		change := setValue(&volume.Status.VolumeID, volumeID)
		change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhaseMissing)
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonRuntimeVolumeMissing, "Runtime volume was not found.")
		return change | additionalReconciliationNeeded
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect runtime volume", "VolumeID", volumeID)
		change := setValue(&volume.Status.VolumeID, volumeID)
		change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhaseFailed)
		change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonReconciliationFailed, fmt.Sprintf("Failed to inspect runtime volume: %v", inspectErr))
		return change | additionalReconciliationNeeded
	}

	return applyReadyPhysicalContainerVolumeStatus(volume, inspectedVolume)
}

func (r *PhysicalContainerVolumeReconciler) schedulePhysicalContainerVolumeCreate(volume *apiv2.PhysicalContainerVolume, log logr.Logger) objectChange {
	volumeConfig := volume.Spec.Volume
	stateKey := physicalContainerVolumeDataKey(volume)
	data := &physicalContainerVolumeData{conditionReason: apiv2.PhysicalContainerVolumeReasonCreating}
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
			data.conditionReason = apiv2.PhysicalContainerVolumeReasonReconciliationFailed
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
		Labels: physicalContainerVolumeCreationLabels(volume),
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
		data.volumeID = inspectedVolume.Name
		data.failureMessage = ""
		data.retryAfter = time.Time{}
		log.V(1).Info("Adopted runtime volume created by an earlier attempt", "VolumeID", inspectedVolume.Name)
		return false, nil
	}

	if !r.removeRuntimeVolume(ctx, inspectedVolume.Name, log) {
		return false, fmt.Errorf("remove runtime volume %q", inspectedVolume.Name)
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
		data.volumeID = inspectedVolume.Name
		data.failureMessage = ""
		data.retryAfter = time.Time{}
		return
	}
	if inspectErr == nil {
		if volumeConfig.ReplaceExisting {
			data.conditionReason = apiv2.PhysicalContainerVolumeReasonReconciliationFailed
			if data.failureMessage == "" {
				data.failureMessage = fmt.Sprintf("Runtime volume name %q was claimed during replacement.", volumeConfig.VolumeName)
			}
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		} else {
			data.conditionReason = apiv2.PhysicalContainerVolumeReasonCreateFailed
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
	data.conditionReason = apiv2.PhysicalContainerVolumeReasonReconciliationFailed
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
	_ context.Context,
	_ *PhysicalContainerVolumeReconciler,
	_ *apiv2.PhysicalContainerVolume,
	_ apiv2.ConditionReason,
	_ *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
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
	log.V(1).Info("Runtime volume created; saving volume status", "VolumeID", data.volumeID)
	return reconciler.applyRuntimeVolumeStatus(ctx, volume, data.volumeID, log)
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
	return noChange
}

func handlePhysicalContainerVolumeRecoverableCreateFailed(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	_ apiv2.ConditionReason,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	volumeConfig := volume.Spec.Volume
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}

	inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, reconciler.orchestrator, volumeConfig.VolumeName)
	if inspectErr == nil {
		belongsToResource := physicalContainerVolumeBelongsToResource(inspectedVolume, volume)
		if !belongsToResource && !volumeConfig.ReplaceExisting {
			data.conditionReason = apiv2.PhysicalContainerVolumeReasonCreateFailed
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
	change := setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhaseFailed)
	change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerVolumeReasonReconciliationFailed, message)
	return change | additionalReconciliationNeeded
}

func (r *PhysicalContainerVolumeReconciler) handleDeletionRequest(ctx context.Context, volume *apiv2.PhysicalContainerVolume, log logr.Logger) objectChange {
	_, data := r.volumeData.BorrowByNamespacedName(volume.NamespacedName())
	if data != nil && data.operationInProgress() {
		log.V(1).Info("PhysicalContainerVolume is being deleted while creation is in progress")
		return additionalReconciliationNeeded
	}

	volumeID := volume.Status.VolumeID
	if volumeID == "" && data != nil {
		volumeID = data.volumeID
	}
	if volumeID == "" {
		volumeID = volume.Spec.VolumeID
	}
	if volume.Spec.Volume != nil && !volume.Spec.Volume.RetainRuntimeVolume &&
		volumeID == "" && data != nil &&
		data.conditionReason == apiv2.PhysicalContainerVolumeReasonReconciliationFailed {
		inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volume.Spec.Volume.VolumeName)
		if inspectErr == nil && physicalContainerVolumeBelongsToResource(inspectedVolume, volume) {
			volumeID = inspectedVolume.Name
		} else if inspectErr != nil && !errors.Is(inspectErr, containers.ErrNotFound) {
			log.Error(inspectErr, "Failed to verify whether runtime volume creation succeeded during deletion", "VolumeName", volume.Spec.Volume.VolumeName)
			return additionalReconciliationNeeded
		}
	}

	if volume.Spec.Volume != nil && !volume.Spec.Volume.RetainRuntimeVolume && volumeID != "" && !r.removeRuntimeVolume(ctx, volumeID, log) {
		return additionalReconciliationNeeded
	}

	r.volumeData.DeleteByNamespacedName(volume.NamespacedName())
	return deleteFinalizer(volume, physicalContainerVolumeFinalizer, log)
}

func (r *PhysicalContainerVolumeReconciler) removeRuntimeVolume(ctx context.Context, volumeID string, log logr.Logger) bool {
	_, inspectErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volumeID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		return true
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect runtime volume before removal", "VolumeID", volumeID)
		return false
	}

	_, removeErr := r.orchestrator.RemoveVolumes(ctx, containers.RemoveVolumesOptions{
		Volumes: []string{volumeID},
		Force:   false,
	})
	if removeErr == nil {
		return true
	}

	_, confirmErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volumeID)
	if errors.Is(confirmErr, containers.ErrNotFound) {
		return true
	}
	if errors.Is(removeErr, containers.ErrObjectInUse) {
		log.V(1).Info("Runtime volume is still in use; waiting before retrying removal", "VolumeID", volumeID)
		return false
	}

	log.Error(errors.Join(removeErr, confirmErr), "Failed to remove runtime volume", "VolumeID", volumeID)
	return false
}

func inspectPhysicalContainerVolume(
	ctx context.Context,
	orchestrator containers.VolumeOrchestrator,
	volume string,
) (*containers.InspectedVolume, error) {
	inspectedVolumes, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
		Volumes: []string{volume},
	})
	if len(inspectedVolumes) > 0 {
		return &inspectedVolumes[0], nil
	}
	if inspectErr != nil {
		return nil, inspectErr
	}

	return nil, containers.ErrNotFound
}

func physicalContainerVolumeCreationLabels(volume *apiv2.PhysicalContainerVolume) map[string]string {
	labels := map[string]string{}
	for _, label := range volume.Spec.Volume.Labels {
		labels[label.Key] = label.Value
	}
	if volume.UID != "" {
		labels[uidLabel] = string(volume.UID)
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
	change |= setValue(&volume.Status.VolumeName, inspectedVolume.Name)
	change |= setValue(&volume.Status.Driver, inspectedVolume.Driver)
	change |= setValue(&volume.Status.MountPoint, inspectedVolume.MountPoint)
	change |= setValue(&volume.Status.Scope, inspectedVolume.Scope)
	change |= setTimestamp(&volume.Status.CreatedAt, metav1.NewMicroTime(inspectedVolume.CreatedAt))
	change |= setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhaseReady)
	change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionTrue, apiv2.PhysicalContainerVolumeReasonVolumeReady, "Runtime volume is available.")
	return change | additionalReconciliationNeeded
}
