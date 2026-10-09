/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/syncmap"
)

const containerVolumeResetFinalizer = "usvc-dev.developer.microsoft.com/container-volume-reset-reconciler"

// ContainerVolumeResetTerminalRetention bounds retention of published terminal operation results.
const ContainerVolumeResetTerminalRetention = time.Hour

// ContainerVolumeResetReconcilerConfig supplies the operation controller's clock.
type ContainerVolumeResetReconcilerConfig struct {
	// Now supplies the current time. If omitted, the system clock is used.
	Now func() time.Time
}

type containerVolumeResetExecution struct {
	lock        sync.Mutex
	status      apiv1.ContainerVolumeResetStatus
	ctx         context.Context
	cancel      context.CancelFunc
	executing   bool
	targetUID   types.UID
	retainUntil time.Time
}

// ContainerVolumeResetReconciler owns reset operation status and cancellation.
// Runtime changes are serialized by the target Container's reconciliation queue.
type ContainerVolumeResetReconciler struct {
	*ReconcilerBase[apiv1.ContainerVolumeReset, *apiv1.ContainerVolumeReset]
	containers    *ContainerReconciler
	admission     sync.Mutex
	activeTargets map[types.UID]types.UID
	executions    syncmap.Map[types.UID, *containerVolumeResetExecution]
	now           func() time.Time
}

func NewContainerVolumeResetReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	reader ctrl_client.Reader,
	log logr.Logger,
	containerController *ContainerReconciler,
	config ContainerVolumeResetReconcilerConfig,
) *ContainerVolumeResetReconciler {
	now := config.Now
	if now == nil {
		now = time.Now
	}
	r := &ContainerVolumeResetReconciler{
		ReconcilerBase: NewReconcilerBase[apiv1.ContainerVolumeReset](client, reader, log, lifetimeCtx),
		containers:     containerController,
		activeTargets:  make(map[types.UID]types.UID),
		now:            now,
	}
	containerController.volumeResetController = r
	return r
}

func (r *ContainerVolumeResetReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv1.ContainerVolumeReset{}).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *ContainerVolumeResetReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(request)
	reset := &apiv1.ContainerVolumeReset{}
	if getErr := reader.Get(ctx, request.NamespacedName, reset); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Could not read container volume reset operation")
		return ctrl.Result{}, getErr
	}
	patch := ctrl_client.MergeFromWithOptions(reset.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	target := types.NamespacedName{Name: reset.Spec.ContainerName, Namespace: reset.Namespace}
	execution, found := r.executions.Load(reset.UID)
	var change objectChange
	if !reset.DeletionTimestamp.IsZero() {
		if found {
			execution.cancel()
			execution.lock.Lock()
			executing := execution.executing
			execution.lock.Unlock()
			if executing {
				return r.SaveChanges(ctx, reset, patch, additionalReconciliationNeeded, nil, log)
			}
			r.executions.Delete(reset.UID)
			r.releaseTarget(reset.UID, execution.targetUID)
		}
		change = deleteFinalizer(reset, containerVolumeResetFinalizer, log)
		r.containers.ScheduleReconciliation(target)
		return r.SaveChanges(ctx, reset, patch, change, nil, log)
	}
	if change = ensureFinalizer(reset, containerVolumeResetFinalizer, log); change != noChange {
		return r.SaveChanges(ctx, reset, patch, change|additionalReconciliationNeeded, nil, log)
	}
	if !found {
		r.admission.Lock()
		operationCtx, cancel := context.WithCancel(r.LifetimeCtx)
		candidate := &containerVolumeResetExecution{
			status: apiv1.ContainerVolumeResetStatus{State: "Pending"},
			ctx:    operationCtx, cancel: cancel,
			targetUID: reset.Spec.ContainerUID,
		}
		if owner, active := r.activeTargets[reset.Spec.ContainerUID]; active && owner != reset.UID {
			candidate.status.State = "Failed"
			candidate.status.Message = fmt.Sprintf("container already has an active reset operation %q", owner)
			cancel()
		}
		execution, found = r.executions.LoadOrStore(reset.UID, candidate)
		if found {
			cancel()
		} else if candidate.status.State == "Pending" {
			r.activeTargets[reset.Spec.ContainerUID] = reset.UID
		}
		r.admission.Unlock()
	}
	execution.lock.Lock()
	status := execution.status.DeepCopy()
	if !execution.executing && (status.State == "Pending" || status.State == "Running") {
		container := &apiv1.Container{}
		targetErr := r.NoCacheClient.Get(ctx, target, container)
		if targetErr != nil && !apierrors.IsNotFound(targetErr) {
			execution.lock.Unlock()
			log.Error(targetErr, "Could not verify reset target")
			return ctrl.Result{}, targetErr
		}
		if apierrors.IsNotFound(targetErr) || container.UID != reset.Spec.ContainerUID || !container.DeletionTimestamp.IsZero() {
			status.State = "Failed"
			status.Message = "reset target Container was deleted or replaced"
			execution.status = *status
			execution.cancel()
		} else if container.Spec.EffectiveMode() != apiv1.ContainerModeSession && container.Spec.EffectiveMode() != apiv1.ContainerModePersistent {
			status.State = "Failed"
			status.Message = "only session and persistent containers can reset volumes"
			execution.status = *status
			execution.cancel()
		}
	}
	terminal := status.State == "Succeeded" || status.State == "Failed"
	if terminal && status.FinishTimestamp.IsZero() {
		status.FinishTimestamp = metav1.NewMicroTime(r.now().UTC().Truncate(time.Microsecond))
		execution.status = *status
	}
	retainUntil := execution.retainUntil
	execution.lock.Unlock()
	if terminal {
		r.releaseTarget(reset.UID, reset.Spec.ContainerUID)
		if !retainUntil.IsZero() && !r.now().Before(retainUntil) {
			deleteErr := r.Delete(ctx, reset, ctrl_client.Preconditions{UID: &reset.UID})
			if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				log.Error(deleteErr, "Could not delete expired container volume reset operation")
				return ctrl.Result{}, deleteErr
			}
			log.Info("Deleted expired container volume reset operation")
			return ctrl.Result{RequeueAfter: delayDuration(StandardDelay)}, nil
		}
	}
	if !apiequality.Semantic.DeepEqual(reset.Status, *status) {
		reset.Status = *status
		change |= statusChanged
	}
	r.containers.ScheduleReconciliation(target)
	if status.State == "Pending" || status.State == "Running" {
		change |= additionalReconciliationNeeded
	}
	result, saveErr := r.SaveChanges(ctx, reset, patch, change, func() {
		if terminal {
			execution.lock.Lock()
			if execution.retainUntil.IsZero() {
				execution.retainUntil = r.now().Add(ContainerVolumeResetTerminalRetention)
			}
			execution.lock.Unlock()
		}
	}, log)
	if saveErr != nil {
		return result, fmt.Errorf("save reset operation status: %w", saveErr)
	}
	if terminal {
		execution.lock.Lock()
		retainUntil = execution.retainUntil
		execution.lock.Unlock()
		if !retainUntil.IsZero() {
			remaining := max(retainUntil.Sub(r.now()), time.Nanosecond)
			if result.RequeueAfter == 0 || remaining < result.RequeueAfter {
				result.RequeueAfter = remaining
			}
		}
	}
	return result, nil
}

func (r *ContainerVolumeResetReconciler) releaseTarget(operationUID, targetUID types.UID) {
	r.admission.Lock()
	defer r.admission.Unlock()
	if r.activeTargets[targetUID] == operationUID {
		delete(r.activeTargets, targetUID)
	}
}
