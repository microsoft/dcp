// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/go-logr/logr"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	ct "github.com/microsoft/usvc-apiserver/internal/containers"
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

func (r *VolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.ContainerVolume{}).
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
		err := r.deleteVolume(ctx, vol.Spec.Name, log)
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

	result, err := saveChanges(r.Client, ctx, &vol, patch, change, nil, log)
	return result, err
}

func (r *VolumeReconciler) deleteVolume(ctx context.Context, volumeName string, log logr.Logger) error {
	const force = false

	removed, err := r.orchestrator.RemoveVolumes(ctx, []string{volumeName}, force)
	if err != nil {
		if err != ct.ErrNotFound {
			log.Error(err, "could not remove a container volume")
			return err
		} else {
			return nil // If the volume is not there, that's the desired state.
		}
	} else if len(removed) != 1 || removed[0] != volumeName {
		log.Error(fmt.Errorf("unexpected response received from container volume removal request. Number of volumes removed: %d", len(removed)), "")
		// .. but it did not fail, so assume the volume was removed.
	}

	return nil
}

func (r *VolumeReconciler) ensureVolume(ctx context.Context, volumeName string, log logr.Logger) objectChange {
	volumeName = strings.TrimSpace(volumeName)
	if volumeName == "" {
		log.Error(fmt.Errorf("specified volume name is empty"), "")

		// Hopefully someone will notice the error and update the Spec.
		// Once the Spec is changed, another reconciliation will kick in automatically.
		return noChange
	}

	_, err := r.orchestrator.InspectVolumes(ctx, []string{volumeName})
	if err == nil {
		return noChange // Volume exists, nothing to do
	} else if !errors.Is(err, ct.ErrNotFound) {
		log.Error(err, "could not determine whether volume exists")
		return additionalReconciliationNeeded
	}

	err = r.orchestrator.CreateVolume(ctx, volumeName)
	if err != nil {
		log.Error(err, "could not create a volume")
		return additionalReconciliationNeeded
	}

	log.Info("volume created")
	return noChange
}
