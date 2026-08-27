/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/randdata"
)

// Data about ContainerVolume objects that we keep in memory
// (a remedy for K8s client libraries caching).
type containerVolumeData struct {
	// The most recent state of the ContainerVolume object
	state apiv1.ContainerVolumeState
}

func (cvd *containerVolumeData) Clone() *containerVolumeData {
	return &containerVolumeData{
		state: cvd.state,
	}
}

func (cvd *containerVolumeData) UpdateFrom(other *containerVolumeData) bool {
	if other == nil {
		return false
	}
	updated := false

	if cvd.state != other.state {
		cvd.state = other.state
		updated = true
	}

	return updated
}

type volumeStateInitializerFunc = stateInitializerFunc[
	apiv1.ContainerVolume, *apiv1.ContainerVolume,
	VolumeReconciler, *VolumeReconciler,
	apiv1.ContainerVolumeState,
	containerVolumeData, *containerVolumeData,
]

var (
	volumeFinalizer string = fmt.Sprintf("%s/volume-reconciler", apiv1.GroupVersion.Group)

	volumeStateInitializers = map[apiv1.ContainerVolumeState]volumeStateInitializerFunc{
		apiv1.ContainerVolumeStateEmpty:            handleNewContainerVolume,
		apiv1.ContainerVolumeStatePending:          handleNewContainerVolume,
		apiv1.ContainerVolumeStateRuntimeUnhealthy: handleNewContainerVolume,
		apiv1.ContainerVolumeStateReady:            handleReadyContainerVolume,
	}
)

const volumeOwnershipTokenLength = 32

type volumeName string
type volumeDataMap = ObjectStateMap[volumeName, containerVolumeData, *containerVolumeData, *apiv1.ContainerVolume]

type VolumeReconcilerConfig struct {
	StateStore         *statestore.Store
	ResourceLeaseOwner process.ProcessHandle
	WorkloadID         commonapi.WorkloadID
}

type VolumeReconciler struct {
	*ReconcilerBase[apiv1.ContainerVolume, *apiv1.ContainerVolume]
	orchestrator containers.VolumeOrchestrator
	volumeData   *volumeDataMap
	config       VolumeReconcilerConfig
}

func NewVolumeReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	orchestrator containers.VolumeOrchestrator,
	config VolumeReconcilerConfig,
) *VolumeReconciler {
	base := NewReconcilerBase[apiv1.ContainerVolume](client, noCacheClient, log, lifetimeCtx)

	r := VolumeReconciler{
		ReconcilerBase: base,
		orchestrator:   orchestrator,
		volumeData:     NewObjectStateMap[volumeName, containerVolumeData, *containerVolumeData, *apiv1.ContainerVolume](),
		config:         config,
	}
	return &r
}

func (r *VolumeReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv1.ContainerVolume{}).
		Named(name). // zero value is OK and will result in a default provided by controller-runtime
		Complete(r)
}

func (r *VolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	vol := apiv1.ContainerVolume{}
	err := reader.Get(ctx, req.NamespacedName, &vol)

	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.V(1).Info("The ContainerVolume object was deleted")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "Failed to Get() the ContainerVolume object")
			getFailedCounter.Add(ctx, 1)
			return ctrl.Result{}, err
		}
	} else {
		getSucceededCounter.Add(ctx, 1)
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(vol.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if vol.DeletionTimestamp != nil && !vol.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, &vol, log)
	} else if change = ensureFinalizer(&vol, volumeFinalizer, log); change != noChange {
		// Make additional changes during next reconciliation
	} else {
		change = r.manageVolume(ctx, &vol, log)
	}

	reconciliationDelay := StandardDelay
	if vol.Status.State == apiv1.ContainerVolumeStateRuntimeUnhealthy {
		reconciliationDelay = LongDelay
	}

	return r.SaveChangesWithDelay(ctx, &vol, patch, change, reconciliationDelay, nil, log)
}

func (r *VolumeReconciler) handleDeletionRequest(ctx context.Context, vol *apiv1.ContainerVolume, log logr.Logger) objectChange {
	_, volData := r.volumeData.BorrowByNamespacedName(vol.NamespacedName())
	if volData == nil || volData.state != apiv1.ContainerVolumeStateReady || pointers.TrueValue(vol.Spec.Persistent) {
		// No actual volume to delete, or it is persistent and needs to be preserved.
		// We can just silently continue with finalizer removal and deletion of the object.
		r.volumeData.DeleteByNamespacedName(vol.NamespacedName())
		return deleteFinalizer(vol, volumeFinalizer, log)
	}

	err := removeVolume(ctx, r.orchestrator, vol.Spec.Name)
	if err != nil && !errors.Is(err, containers.ErrNotFound) {
		log.Error(err, "Could not remove a container volume")
		return additionalReconciliationNeeded
	}

	log.V(1).Info("Volume removed")
	change := deleteFinalizer(vol, volumeFinalizer, log)
	r.volumeData.DeleteByNamespacedName(vol.NamespacedName())
	return change
}

func (r *VolumeReconciler) manageVolume(ctx context.Context, vol *apiv1.ContainerVolume, log logr.Logger) objectChange {
	targetState := vol.Status.State
	_, volData := r.volumeData.BorrowByNamespacedName(vol.NamespacedName())
	if volData != nil {
		targetState = volData.state
	}

	runInitializer := func(ctx context.Context) objectChange {
		initializer := getStateInitializer(volumeStateInitializers, targetState, log)
		return initializer(ctx, r, vol, targetState, volData, log)
	}

	change := noChange
	if pointers.TrueValue(vol.Spec.Persistent) && r.config.WorkloadID != "" {
		if r.config.StateStore == nil {
			stateStoreErr := fmt.Errorf("state store is not configured")
			log.Error(stateStoreErr, "Could not acquire persistent volume lease")
			// ContainerVolume has no terminal failure state, so retry rather than creating an untracked volume.
			return additionalReconciliationNeeded
		}

		leaseErr := r.config.StateStore.WithResourceLease(
			ctx,
			vol,
			r.config.ResourceLeaseOwner,
			resourceLeaseRevalidationInterval,
			func(leaseCtx context.Context, lease *statestore.ResourceLease) error {
				log.V(1).Info("Acquired resource lease", "ResourceKey", lease.ResourceKey)
				change = runInitializer(leaseCtx)
				return nil
			},
		)
		if errors.Is(leaseErr, statestore.ErrResourceLeaseHeld) {
			logResourceLeaseHeld(log, leaseErr, vol.GetLeaseKey(), "Persistent volume is being updated by another DCP instance, retrying")
			return additionalReconciliationNeeded
		}
		if leaseErr != nil {
			log.Error(leaseErr, "Could not manage persistent volume under resource lease")
			// ContainerVolume has no terminal failure state, so retry until bookkeeping is available again.
			change |= additionalReconciliationNeeded
		}
	} else {
		change = runInitializer(ctx)
	}

	if volData != nil {
		r.volumeData.Update(vol.NamespacedName(), volumeName(vol.Spec.Name), volData)
	}

	return change
}

func handleNewContainerVolume(
	ctx context.Context,
	r *VolumeReconciler,
	vol *apiv1.ContainerVolume,
	_ apiv1.ContainerVolumeState,
	volData *containerVolumeData,
	log logr.Logger,
) objectChange {
	runtimeStatus := r.orchestrator.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed)
	if !runtimeStatus.IsHealthy() {
		log.V(1).Info("Container runtime is not healthy, retrying reconciliation later...")
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStateRuntimeUnhealthy) | additionalReconciliationNeeded
	}

	if volData == nil {
		volData = &containerVolumeData{
			state: apiv1.ContainerVolumeStatePending,
		}
		r.volumeData.Store(vol.NamespacedName(), volumeName(vol.Spec.Name), volData)
	}

	if volData.state == apiv1.ContainerVolumeStateReady {
		// We have already created the volume. There is nothing to do, we are just seeing stale ContainerVolume object
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStateReady)
	}

	inspectedVolume, inspectErr := inspectContainerVolumeIfExists(ctx, r.orchestrator, vol.Spec.Name)
	if inspectErr == nil {
		if reconcileRecordErr := r.reconcileExistingPersistentVolumeRecord(ctx, vol, inspectedVolume); reconcileRecordErr != nil {
			log.Error(reconcileRecordErr, "Could not reconcile existing ContainerVolume workload record", "ResourceKey", vol.GetLeaseKey())
			return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
		}
		log.V(1).Info("Container volume already exists")
		volData.state = apiv1.ContainerVolumeStateReady
		r.volumeData.Update(vol.NamespacedName(), volumeName(vol.Spec.Name), volData)
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStateReady)
	} else if !errors.Is(inspectErr, containers.ErrNotFound) {
		log.Error(inspectErr, "Could not determine whether container volume exists")
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
	}

	// Need to create the volume
	ownershipToken, prepareRecordErr := r.preparePersistentVolumeRecord(ctx, vol)
	if prepareRecordErr != nil {
		log.Error(prepareRecordErr, "Could not persist pending ContainerVolume workload record", "ResourceKey", vol.GetLeaseKey())
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
	}

	createOptions := containers.CreateVolumeOptions{Name: vol.Spec.Name}
	if ownershipToken != "" {
		createOptions.Labels = map[string]string{
			containers.VolumeOwnershipTokenLabel: ownershipToken,
		}
	}
	var createErr error
	inspectedVolume, createErr = createVolume(ctx, r.orchestrator, createOptions)
	if errors.Is(createErr, containers.ErrAlreadyExists) {
		var postCreateInspectErr error
		inspectedVolume, postCreateInspectErr = inspectContainerVolume(ctx, r.orchestrator, vol.Spec.Name)
		if postCreateInspectErr == nil {
			createErr = nil
		} else {
			createErr = errors.Join(createErr, postCreateInspectErr)
		}
	}
	if createErr != nil {
		log.Error(createErr, "Could not create a container volume")
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
	}
	if ownershipToken != "" && !persistentVolumeOwnershipMatches(inspectedVolume, ownershipToken) {
		discardRecordErr := r.discardPendingPersistentVolumeRecord(ctx, vol, ownershipToken)
		if discardRecordErr != nil {
			log.Error(discardRecordErr, "Could not discard stale ContainerVolume workload record", "ResourceKey", vol.GetLeaseKey())
			return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
		}
		log.V(1).Info("Container volume was created concurrently and will be adopted")
	} else {
		log.V(1).Info("Container volume created")
	}

	volData.state = apiv1.ContainerVolumeStateReady
	r.volumeData.Update(vol.NamespacedName(), volumeName(vol.Spec.Name), volData)
	return setContainerVolumeState(vol, apiv1.ContainerVolumeStateReady)
}

func (r *VolumeReconciler) preparePersistentVolumeRecord(
	ctx context.Context,
	vol *apiv1.ContainerVolume,
) (string, error) {
	if r.config.WorkloadID == "" || !pointers.TrueValue(vol.Spec.Persistent) {
		return "", nil
	}
	if r.config.StateStore == nil {
		return "", fmt.Errorf("state store is not configured")
	}

	existingRecord, getRecordErr := r.config.StateStore.GetPersistentVolume(ctx, vol.GetLeaseKey())
	if getRecordErr == nil &&
		existingRecord.VolumeName == vol.Spec.Name &&
		existingRecord.RuntimeName == r.orchestrator.Name() &&
		existingRecord.WorkloadID == r.config.WorkloadID &&
		existingRecord.OwnershipToken != "" {
		return existingRecord.OwnershipToken, nil
	}
	if getRecordErr != nil && !errors.Is(getRecordErr, statestore.ErrPersistentVolumeNotFound) {
		return "", getRecordErr
	}

	tokenBytes, tokenErr := randdata.MakeRandomString(volumeOwnershipTokenLength)
	if tokenErr != nil {
		return "", fmt.Errorf("could not generate persistent volume ownership token: %w", tokenErr)
	}
	ownershipToken := string(tokenBytes)

	record := statestore.PersistentVolumeRecord{
		ResourceKey:    vol.GetLeaseKey(),
		VolumeName:     vol.Spec.Name,
		RuntimeName:    r.orchestrator.Name(),
		WorkloadID:     r.config.WorkloadID,
		OwnershipToken: ownershipToken,
	}
	if persistErr := r.config.StateStore.UpsertPersistentVolume(ctx, record); persistErr != nil {
		return "", persistErr
	}
	return ownershipToken, nil
}

func (r *VolumeReconciler) discardPendingPersistentVolumeRecord(
	ctx context.Context,
	vol *apiv1.ContainerVolume,
	ownershipToken string,
) error {
	if ownershipToken == "" {
		return nil
	}
	deleted, deleteErr := r.config.StateStore.DeletePersistentVolumeIfOwnershipTokenMatches(ctx, vol.GetLeaseKey(), ownershipToken)
	if deleteErr != nil {
		return deleteErr
	}
	if !deleted {
		return fmt.Errorf("persistent volume ownership record changed before it could be discarded")
	}
	return nil
}

func (r *VolumeReconciler) reconcileExistingPersistentVolumeRecord(
	ctx context.Context,
	vol *apiv1.ContainerVolume,
	inspectedVolume *containers.InspectedVolume,
) error {
	if r.config.WorkloadID == "" || !pointers.TrueValue(vol.Spec.Persistent) || r.config.StateStore == nil {
		return nil
	}

	record, getRecordErr := r.config.StateStore.GetPersistentVolume(ctx, vol.GetLeaseKey())
	if errors.Is(getRecordErr, statestore.ErrPersistentVolumeNotFound) {
		return nil
	}
	if getRecordErr != nil {
		return getRecordErr
	}
	if record.WorkloadID != r.config.WorkloadID || persistentVolumeOwnershipMatches(inspectedVolume, record.OwnershipToken) {
		return nil
	}
	return r.config.StateStore.DeletePersistentVolume(ctx, record.ResourceKey)
}

func persistentVolumeOwnershipMatches(inspectedVolume *containers.InspectedVolume, ownershipToken string) bool {
	if inspectedVolume == nil || ownershipToken == "" {
		return false
	}
	return inspectedVolume.Labels[containers.VolumeOwnershipTokenLabel] == ownershipToken
}

func handleReadyContainerVolume(
	ctx context.Context,
	r *VolumeReconciler,
	vol *apiv1.ContainerVolume,
	targetState apiv1.ContainerVolumeState,
	volData *containerVolumeData,
	log logr.Logger,
) objectChange {
	// Just make sure the ContainerVolume.Status is updated.
	change := setContainerVolumeState(vol, apiv1.ContainerVolumeStateReady)
	return change
}

func setContainerVolumeState(vol *apiv1.ContainerVolume, state apiv1.ContainerVolumeState) objectChange {
	change := noChange

	if vol.Status.State != state {
		vol.Status.State = state
		change = statusChanged
	}

	if state == apiv1.ContainerVolumeStateRuntimeUnhealthy {
		change |= additionalReconciliationNeeded
	}

	return change
}
