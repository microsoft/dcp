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
		physicalContainerVolumeStateNamespace: handlePhysicalContainerVolumeNamespace,
		physicalContainerVolumeStateResolve:   handlePhysicalContainerVolumeResolve,
		physicalContainerVolumeStateCreate:    handlePhysicalContainerVolumeCreateState,
		physicalContainerVolumeStateReplace:   handlePhysicalContainerVolumeCreateState,
		physicalContainerVolumeStateRuntime:   handlePhysicalContainerVolumeRuntime,
		physicalContainerVolumeStateRemove:    handlePhysicalContainerVolumeRemovalState,
		0:                                     handleUnknownPhysicalContainerVolumeDataReason,
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
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNamespace(&apiv2.PhysicalContainerVolumeList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
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
	reconciliationDelay := StandardDelay
	patch := ctrl_client.MergeFromWithOptions(volume.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if volume.DeletionTimestamp != nil && !volume.DeletionTimestamp.IsZero() {
		change, reconciliationDelay = r.managePhysicalContainerVolume(ctx, &volume, log)
	} else if change = ensureFinalizer(&volume, physicalContainerVolumeFinalizer, log); change != noChange {
		// Make additional changes during the next reconciliation.
	} else {
		change, reconciliationDelay = r.managePhysicalContainerVolume(ctx, &volume, log)
	}

	return r.SaveChangesWithDelay(ctx, &volume, patch, change, reconciliationDelay, nil, log)
}

func (r *PhysicalContainerVolumeReconciler) managePhysicalContainerVolume(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	_, data := r.volumeData.BorrowByNamespacedName(volume.NamespacedName())
	if data == nil {
		data = &physicalContainerVolumeData{
			state:    physicalContainerVolumeStateNamespace,
			progress: physicalResourceProgressNotReady,
		}
		initialStateKey := physicalContainerVolumeDataKey(volume)
		r.volumeData.Store(volume.NamespacedName(), initialStateKey, data)
		_, data = r.volumeData.BorrowByNamespacedName(volume.NamespacedName())
	}

	handler := getStateInitializer(physicalContainerVolumeDataInitializers, data.state, log)
	change := handler(ctx, r, volume, data.state, data, log)

	_, currentData := r.volumeData.BorrowByNamespacedName(volume.NamespacedName())
	if currentData == nil {
		return change, StandardDelay
	}
	change |= currentData.applyTo(volume)
	delay := physicalContainerVolumeProjections.reconciliationDelay(currentData.state, currentData.progress)
	return change, delay
}

func handlePhysicalContainerVolumeNamespace(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	_ physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if volume.DeletionTimestamp != nil && !volume.DeletionTimestamp.IsZero() {
		return reconciler.beginPhysicalContainerVolumeRemoval(volume, data, log)
	}
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, reconciler.Client, volume.Namespace)
	if !namespaceReady {
		data.state = physicalContainerVolumeStateNamespace
		data.failureMessage = namespaceReadinessMessage(volume.Namespace, namespaceReason)
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
			log.Error(namespaceErr, "Failed to get namespace", "Namespace", volume.Namespace)
			data.progress = physicalResourceProgressRetryPending
			data.failureMessage = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
		}
		stateKey, _ := reconciler.volumeData.BorrowByNamespacedName(volume.NamespacedName())
		_ = reconciler.volumeData.Update(volume.NamespacedName(), stateKey, data)
		return noChange
	}

	data.state = physicalContainerVolumeStateResolve
	data.progress = physicalResourceProgressInProgress
	data.failureMessage = ""
	stateKey, _ := reconciler.volumeData.BorrowByNamespacedName(volume.NamespacedName())
	if !reconciler.volumeData.Update(volume.NamespacedName(), stateKey, data) {
		return additionalReconciliationNeeded
	}
	return handlePhysicalContainerVolumeResolve(ctx, reconciler, volume, data.state, data, log)
}

func handlePhysicalContainerVolumeResolve(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	_ physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if volume.DeletionTimestamp != nil && !volume.DeletionTimestamp.IsZero() {
		return reconciler.beginPhysicalContainerVolumeRemoval(volume, data, log)
	}

	volumeID := volume.Spec.VolumeID
	if volumeID == "" {
		volumeID = data.volumeID
	}
	if volumeID == "" {
		return reconciler.schedulePhysicalContainerVolumeCreate(volume, log)
	}
	return reconciler.applyRuntimeVolumeStatus(ctx, volume, data, volumeID, log)
}

func handlePhysicalContainerVolumeRuntime(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	_ physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if volume.DeletionTimestamp != nil && !volume.DeletionTimestamp.IsZero() {
		return reconciler.beginPhysicalContainerVolumeRemoval(volume, data, log)
	}
	return reconciler.applyRuntimeVolumeStatus(ctx, volume, data, data.volumeID, log)
}

// Inspects the runtime volume and records the resulting reconciliation state.
func (r *PhysicalContainerVolumeReconciler) applyRuntimeVolumeStatus(
	ctx context.Context,
	volume *apiv2.PhysicalContainerVolume,
	data *physicalContainerVolumeData,
	volumeID string,
	log logr.Logger,
) objectChange {
	inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, r.orchestrator, volumeID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		data.state = physicalContainerVolumeStateRuntime
		data.progress = physicalResourceProgressMissing
		data.volumeID = volumeID
		data.failureMessage = ""
		stateKey, _ := r.volumeData.BorrowByNamespacedName(volume.NamespacedName())
		_ = r.volumeData.Update(volume.NamespacedName(), stateKey, data)
		return noChange
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect runtime volume", "VolumeID", volumeID)
		data.state = physicalContainerVolumeStateRuntime
		data.progress = physicalResourceProgressRetryPending
		data.volumeID = volumeID
		data.failureMessage = fmt.Sprintf("Failed to inspect runtime volume: %v", inspectErr)
		stateKey, _ := r.volumeData.BorrowByNamespacedName(volume.NamespacedName())
		_ = r.volumeData.Update(volume.NamespacedName(), stateKey, data)
		return noChange
	}

	data.state = physicalContainerVolumeStateRuntime
	data.progress = physicalResourceProgressCompleted
	data.volumeID = inspectedVolume.Name
	data.failureMessage = ""
	stateKey, _ := r.volumeData.BorrowByNamespacedName(volume.NamespacedName())
	_ = r.volumeData.Update(volume.NamespacedName(), stateKey, data)
	return applyReadyPhysicalContainerVolumeStatus(volume, inspectedVolume)
}

func (r *PhysicalContainerVolumeReconciler) schedulePhysicalContainerVolumeCreate(volume *apiv2.PhysicalContainerVolume, log logr.Logger) objectChange {
	volumeConfig := volume.Spec.Volume
	stateKey := physicalContainerVolumeDataKey(volume)
	data := &physicalContainerVolumeData{state: physicalContainerVolumeStateCreate}
	data.progress = physicalContainerVolumeOperationInProgress
	r.volumeData.Store(volume.NamespacedName(), stateKey, data)
	volumeSnapshot := volume.DeepCopy()
	dataSnapshot := data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.createPhysicalContainerVolume(operationCtx, volumeSnapshot, stateKey, dataSnapshot, log)
	})
	if enqueueErr != nil {
		log.Error(enqueueErr, "Failed to queue PhysicalContainerVolume create", "VolumeName", volumeConfig.VolumeName)
		data.progress = physicalResourceProgressFailed
		data.failureMessage = fmt.Sprintf("Failed to queue runtime volume create: %v", enqueueErr)
		_ = r.volumeData.Update(volume.NamespacedName(), stateKey, data)
		return data.applyTo(volume)
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
			data.state = physicalContainerVolumeStateReplace
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
		data.state = physicalContainerVolumeStateCreate
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
		data.state = physicalContainerVolumeStateCreate
		data.progress = physicalContainerVolumeOperationCompleted
		data.volumeID = inspectedVolume.Name
		data.failureMessage = ""
		data.retryAfter = time.Time{}
		return
	}
	if inspectErr == nil {
		if volumeConfig.ReplaceExisting {
			data.state = physicalContainerVolumeStateCreate
			data.progress = physicalContainerVolumeOperationRetryPending
			if data.failureMessage == "" {
				data.failureMessage = fmt.Sprintf("Runtime volume name %q was claimed during replacement.", volumeConfig.VolumeName)
			}
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		} else {
			data.state = physicalContainerVolumeStateCreate
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
	data.state = physicalContainerVolumeStateCreate
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

func handlePhysicalContainerVolumeCreateState(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if volume.DeletionTimestamp != nil && !volume.DeletionTimestamp.IsZero() {
		return handlePhysicalContainerVolumeCreateStateDuringDeletion(ctx, reconciler, volume, state, data, log)
	}
	switch data.progress {
	case physicalContainerVolumeOperationInProgress:
		return handlePhysicalContainerVolumeCreating(ctx, reconciler, volume, state, data, log)
	case physicalContainerVolumeOperationCompleted:
		return handlePhysicalContainerVolumeCreated(ctx, reconciler, volume, state, data, log)
	case physicalContainerVolumeOperationRetryPending,
		physicalContainerVolumeOperationFailed:
		return handlePhysicalContainerVolumeCreateFailure(ctx, reconciler, volume, state, data, log)
	default:
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
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
	if data.progress != physicalContainerVolumeOperationInProgress {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
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
	if data.progress != physicalContainerVolumeOperationCompleted {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, physicalContainerVolumeStateCreate, data, log)
	}

	log.V(1).Info("Runtime volume created; saving volume status", "VolumeID", data.volumeID)
	return reconciler.applyRuntimeVolumeStatus(ctx, volume, data, data.volumeID, log)
}

func handlePhysicalContainerVolumeCreateFailure(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalContainerVolumeOperationRetryPending:
		return handlePhysicalContainerVolumeRecoverableCreateFailed(ctx, reconciler, volume, state, data, log)
	case physicalContainerVolumeOperationFailed:
		return handlePhysicalContainerVolumeCreateFailed(ctx, reconciler, volume, state, data, log)
	default:
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}
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
	if data.progress != physicalContainerVolumeOperationRetryPending {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}

	volumeConfig := volume.Spec.Volume
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}

	inspectedVolume, inspectErr := inspectPhysicalContainerVolume(ctx, reconciler.orchestrator, volumeConfig.VolumeName)
	if inspectErr == nil {
		belongsToResource := physicalContainerVolumeBelongsToResource(inspectedVolume, volume)
		if !belongsToResource && !volumeConfig.ReplaceExisting {
			data.state = physicalContainerVolumeStateCreate
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

		data.state = physicalContainerVolumeStateRuntime
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
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if volume.DeletionTimestamp != nil && !volume.DeletionTimestamp.IsZero() {
		return reconciler.beginPhysicalContainerVolumeRemoval(volume, data, log)
	}
	reconciler.volumeData.DeleteByNamespacedName(volume.NamespacedName())
	message := fmt.Sprintf("Runtime volume operation reached invalid state %v with progress %v.", state, data.progress)
	log.Error(fmt.Errorf("invalid physical volume state %v with progress %v", state, data.progress), "Runtime volume operation reached invalid state")
	change := setValue(&volume.Status.Phase, apiv2.PhysicalContainerVolumePhaseUnknown)
	change |= setCondition(&volume.Status.Conditions, apiv2.ConditionReady, volume.Generation, metav1.ConditionFalse, apiv2.PhysicalResourceReasonOperationStateInvalid, message)
	return change | additionalReconciliationNeeded
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

	volumeID := ""
	if data != nil {
		volumeID = data.volumeID
	}
	resolveOwnedVolumeByName := volumeID == "" &&
		data != nil
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
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationInProgress {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
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
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationCompleted {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}

	return reconciler.beginPhysicalContainerVolumeRemoval(volume, data, log)
}

func handlePhysicalContainerVolumeFailedCreateDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationFailed {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}

	reconciler.volumeData.DeleteByNamespacedName(volume.NamespacedName())
	return deleteFinalizer(volume, physicalContainerVolumeFinalizer, log)
}

func handlePhysicalContainerVolumeCreateStateDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalContainerVolumeOperationInProgress:
		return handlePhysicalContainerVolumeCreateInProgressDuringDeletion(ctx, reconciler, volume, state, data, log)
	case physicalContainerVolumeOperationCompleted:
		return handlePhysicalContainerVolumeCreatedDuringDeletion(ctx, reconciler, volume, state, data, log)
	case physicalContainerVolumeOperationRetryPending:
		return handlePhysicalContainerVolumeRecoverableCreateFailureDuringDeletion(ctx, reconciler, volume, state, data, log)
	case physicalContainerVolumeOperationFailed:
		return handlePhysicalContainerVolumeFailedCreateDuringDeletion(ctx, reconciler, volume, state, data, log)
	default:
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}
}

func handlePhysicalContainerVolumeRecoverableCreateFailureDuringDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationRetryPending {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}

	return reconciler.beginPhysicalContainerVolumeRemoval(volume, data, log)
}

func handlePhysicalContainerVolumeRemovalState(
	ctx context.Context,
	reconciler *PhysicalContainerVolumeReconciler,
	volume *apiv2.PhysicalContainerVolume,
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalContainerVolumeOperationInProgress:
		return handlePhysicalContainerVolumeRemovalInProgress(ctx, reconciler, volume, state, data, log)
	case physicalContainerVolumeOperationRetryPending:
		return handlePhysicalContainerVolumeRemovalFailed(ctx, reconciler, volume, state, data, log)
	case physicalContainerVolumeOperationCompleted:
		return handlePhysicalContainerVolumeRemovalCompleted(ctx, reconciler, volume, state, data, log)
	case physicalResourceProgressAbandoned:
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
	if data.progress != physicalContainerVolumeOperationInProgress {
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
	if data.progress != physicalContainerVolumeOperationRetryPending {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
	}

	if reconciler.namespaceDeletionVolumeRemovalTimeoutExpired(ctx, volume, log) {
		data.state = physicalContainerVolumeStateRemove
		data.progress = physicalResourceProgressAbandoned
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
	state physicalContainerVolumeState,
	data *physicalContainerVolumeData,
	log logr.Logger,
) objectChange {
	if data.progress != physicalContainerVolumeOperationCompleted {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
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
	if data.progress != physicalResourceProgressAbandoned {
		return handleUnknownPhysicalContainerVolumeDataReason(ctx, reconciler, volume, state, data, log)
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
	volumeID string,
	resolveOwnedVolumeByName bool,
	log logr.Logger,
) objectChange {
	stateKey := physicalContainerVolumeDataKey(volume)
	data := &physicalContainerVolumeData{
		state:         physicalContainerVolumeStateRemove,
		progress:      physicalContainerVolumeOperationInProgress,
		volumeID:      volumeID,
		resolveByName: resolveOwnedVolumeByName,
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
		data.state = physicalContainerVolumeStateRemove
		data.progress = physicalContainerVolumeOperationRetryPending
		data.failureMessage = fmt.Sprintf("Failed to remove runtime volume: %v", removeErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
	} else {
		data.state = physicalContainerVolumeStateRemove
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
