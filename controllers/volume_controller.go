// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/go-logr/logr"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	ct "github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
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

type volumeName string
type volumeDataMap = ObjectStateMap[volumeName, containerVolumeData, *containerVolumeData]

type VolumeReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	orchestrator        ct.VolumeOrchestrator
	volumeData          *volumeDataMap
	debouncer           *reconcilerDebouncer[volumeName]
}

func NewVolumeReconciler(client ctrl_client.Client, log logr.Logger, orchestrator ct.VolumeOrchestrator) *VolumeReconciler {
	r := VolumeReconciler{
		Client:       client,
		Log:          log,
		orchestrator: orchestrator,
		volumeData:   NewObjectStateMap[volumeName, containerVolumeData, *containerVolumeData](),
		debouncer:    newReconcilerDebouncer[volumeName](),
	}
	return &r
}

func (r *VolumeReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.ContainerVolume{}).
		Named(name). // zero value is OK and will result in a default provided by controller-runtime
		Complete(r)
}

func (r *VolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("VolumeName", req.NamespacedName).WithValues("Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1))

	r.debouncer.OnReconcile(req.NamespacedName)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	vol := apiv1.ContainerVolume{}
	err := r.Get(ctx, req.NamespacedName, &vol)

	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.V(1).Info("the ContainerVolume object was deleted")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the ContainerVolume object")
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

	reconciliationDelay, finalChange := computeAdditionalReconciliationDelay(change, vol.Status.State == apiv1.ContainerVolumeStateRuntimeUnhealthy)

	result, saveErr := saveChangesWithCustomReconciliationDelay(r.Client, ctx, &vol, patch, finalChange, reconciliationDelay, nil, log)
	return result, saveErr
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
	if err != nil && !errors.Is(err, ct.ErrNotFound) {
		log.Error(err, "could not remove a container volume")
		return additionalReconciliationNeeded
	}

	log.V(1).Info("volume removed")
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

	initializer := getStateInitializer(volumeStateInitializers, targetState, log)
	change := initializer(ctx, r, vol, targetState, volData, log)

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
		log.V(1).Info("container runtime is not healthy, retrying reconciliation later...")
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

	_, inspectErr := inspectContainerVolumeIfExists(ctx, r.orchestrator, vol.Spec.Name)
	if inspectErr == nil {
		log.V(1).Info("container volume already exists")
		volData.state = apiv1.ContainerVolumeStateReady
		r.volumeData.Update(vol.NamespacedName(), volumeName(vol.Spec.Name), volData)
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStateReady)
	} else if !errors.Is(inspectErr, ct.ErrNotFound) {
		log.Error(inspectErr, "could not determine whether container volume exists")
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
	}

	// Need to create the volume
	_, createErr := createVolume(ctx, r.orchestrator, vol.Spec.Name)
	if createErr != nil {
		log.Error(createErr, "could not create a container volume")
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
	}

	log.V(1).Info("container volume created")
	volData.state = apiv1.ContainerVolumeStateReady
	r.volumeData.Update(vol.NamespacedName(), volumeName(vol.Spec.Name), volData)
	return setContainerVolumeState(vol, apiv1.ContainerVolumeStateReady)
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
