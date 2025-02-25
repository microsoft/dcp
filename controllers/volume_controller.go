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
	ct "github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
)

type VolumeReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	orchestrator        ct.VolumeOrchestrator
}

var (
	volumeFinalizer string = fmt.Sprintf("%s/volume-reconciler", apiv1.GroupVersion.Group)
)

func NewVolumeReconciler(client ctrl_client.Client, log logr.Logger, orchestrator ct.VolumeOrchestrator) *VolumeReconciler {
	r := VolumeReconciler{
		Client:       client,
		Log:          log,
		orchestrator: orchestrator,
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
		log.Info("ContainerVolume object is being deleted")
		err = r.deleteVolume(ctx, &vol, log)
		if err != nil {
			// deleteVolume() logged the error already
			change = additionalReconciliationNeeded
		} else {
			change = deleteFinalizer(&vol, volumeFinalizer, log)
		}
	} else {
		change = ensureFinalizer(&vol, volumeFinalizer, log)
		change |= r.ensureVolume(ctx, vol.Spec.Name, log)
	}

	result, saveErr := saveChanges(r.Client, ctx, &vol, patch, change, nil, log)
	return result, saveErr
}

func (r *VolumeReconciler) deleteVolume(ctx context.Context, vol *apiv1.ContainerVolume, log logr.Logger) error {
	if pointers.TrueValue(vol.Spec.Persistent) {
		return nil // Do not delete persistent volumes
	}
	err := removeVolume(ctx, r.orchestrator, vol.Spec.Name)
	if err != nil && !errors.Is(err, ct.ErrNotFound) {
		log.Error(err, "could not remove a container volume")
		return err
	}

	log.V(1).Info("volume removed")
	return nil
}

func (r *VolumeReconciler) ensureVolume(ctx context.Context, volumeName string, log logr.Logger) objectChange {
	_, inspectErr := inspectContainerVolumeIfExists(ctx, r.orchestrator, volumeName)
	if inspectErr == nil {
		return noChange // Volume exists, nothing to do
	} else if !errors.Is(inspectErr, ct.ErrNotFound) {
		log.Error(inspectErr, "could not determine whether volume exists")
		return additionalReconciliationNeeded
	}

	_, createErr := createVolume(ctx, r.orchestrator, volumeName)
	if createErr != nil {
		log.Error(createErr, "could not create a volume")
		return additionalReconciliationNeeded
	}

	log.Info("volume created")
	return noChange
}
