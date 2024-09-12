// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type ExecutableReplicaSetState string

const (
	ExecutableReplicaSetStateActive   ExecutableReplicaSetState = "active"
	ExecutableReplicaSetStateInactive ExecutableReplicaSetState = "inactive"
)

// Data we keep in memory about ExecutableReplicaSet objects.
type executableReplicaSetData struct {
	lastScaled     time.Time
	actualReplicas int32
}

// ExecutableReplicaSetReconciler reconciles an ExecutableReplicaSet object
type ExecutableReplicaSetReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	runningReplicaSets  syncmap.Map[types.NamespacedName, executableReplicaSetData]
	replicaCounters     syncmap.Map[types.NamespacedName, *atomic.Int32]

	// Debouncer used to schedule reconciliations.
	debouncer *reconcilerDebouncer[any]
}

const (
	ExecutableReplicaStateAnnotation   = "executable-replica-set.usvc-dev.developer.microsoft.com/replica-state"
	ExecutableDisplayNameAnnotation    = "executable-replica-set.usvc-dev.developer.microsoft.com/display-name"
	ExecutableReplicaIdAnnotation      = "executable-replica-set.usvc-dev.developer.microsoft.com/replica-id"
	ExecutableReplicaSetNameAnnotation = "executable-replica-set.usvc-dev.developer.microsoft.com/replica-set-name"

	// Used by .NET Aspire.
	// CONSIDER having means to create this annotation based on information in Executable spec template.
	OtelServiceinstaneIdAnnotation = "otel-service-instance-id"
)

const (
	exeOwnerKey    = ".metadata.controllerOwner" // client index key for child Executables
	scaleRateLimit = 2 * time.Second
)

var (
	executableReplicaSetFinalizer string = fmt.Sprintf("%s/executable-replica-set-reconciler", apiv1.GroupVersion.Group)
)

func NewExecutableReplicaSetReconciler(client ctrl_client.Client, log logr.Logger) *ExecutableReplicaSetReconciler {
	r := ExecutableReplicaSetReconciler{
		Client:             client,
		debouncer:          newReconcilerDebouncer[any](reconciliationDebounceDelay),
		runningReplicaSets: syncmap.Map[types.NamespacedName, executableReplicaSetData]{},
		replicaCounters:    syncmap.Map[types.NamespacedName, *atomic.Int32]{},
		Log:                log,
	}

	return &r
}

func (r *ExecutableReplicaSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Setup a client side index to allow quickly finding all Executables owned by an ExecutableReplicaSet.
	// Behind the scenes this is using listers and informers to keep an index on an internal cache owned by
	// the Manager up to date.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.Executable{}, exeOwnerKey, func(rawObj ctrl_client.Object) []string {
		exe := rawObj.(*apiv1.Executable)
		owner := metav1.GetControllerOf(exe)

		if owner == nil {
			return nil
		}

		// Ignore any Executables that aren't owned by an ExecutableReplicaSet
		if owner.APIVersion != apiv1.GroupVersion.String() || owner.Kind != "ExecutableReplicaSet" {
			return nil
		}

		return []string{owner.Name}
	}); err != nil {
		r.Log.Error(err, "failed to create index for ExecutableReplicaSet", "indexField", exeOwnerKey)
		return err
	}

	// Register for reconciliation on changes to ExecutableReplicaSet objects as well
	// as owned Executable objects (metadata.ownerReferences pointing to an ExecutableReplicaSet)
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.ExecutableReplicaSet{}).
		Owns(&apiv1.Executable{}).
		WithOptions(controller.Options{CacheSyncTimeout: 30 * time.Second}).
		Complete(r)
}

// Create a new Executable replica for the given ExecutableReplicaSet
func (r *ExecutableReplicaSetReconciler) createExecutable(replicaSet *apiv1.ExecutableReplicaSet, log logr.Logger) (*apiv1.Executable, error) {
	// Replica names are postfixed with a unique string to avoid collisions.
	uniqueName, postfix, err := MakeUniqueName(replicaSet.Name)
	if err != nil {
		return nil, err
	}

	// Replicas have a display name annotation that is the replica set name concatenated with a monotonically increasing counter.
	counter, _ := r.replicaCounters.LoadOrStoreNew(replicaSet.NamespacedName(), func() *atomic.Int32 { return &atomic.Int32{} })
	displayName := fmt.Sprintf("%s-%d", replicaSet.Name, counter.Add(1))

	// We don't honor all metadata fields from the template for now, only Labels and Annotations
	exe := &apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      make(map[string]string),
			Annotations: make(map[string]string),
			Name:        uniqueName,
			Namespace:   replicaSet.Namespace,
		},
		Spec: *replicaSet.Spec.Template.Spec.DeepCopy(),
	}

	for k, v := range replicaSet.Spec.Template.Annotations {
		exe.Annotations[k] = v
	}
	exe.Annotations[ExecutableReplicaStateAnnotation] = string(ExecutableReplicaSetStateActive)
	exe.Annotations[ExecutableDisplayNameAnnotation] = displayName
	exe.Annotations[ExecutableReplicaIdAnnotation] = postfix
	exe.Annotations[OtelServiceinstaneIdAnnotation] = postfix
	exe.Annotations[ExecutableReplicaSetNameAnnotation] = replicaSet.Name

	for k, v := range replicaSet.Spec.Template.Labels {
		exe.Labels[k] = v
	}

	// Set the ExecutableReplica set as the owner of the Executable so that changes to the Executable will trigger
	// our reconciler loop.
	if err = ctrl.SetControllerReference(replicaSet, exe, r.Scheme()); err != nil {
		log.Error(err, "failed to create executable for ExecutableReplicaSet", "exe", exe)
		return nil, err
	}

	return exe, nil
}

// Ensure the ExecutableReplicaSet status matches the current state of the replicas.
func (r *ExecutableReplicaSetReconciler) updateReplicaStatus(replicaSet *apiv1.ExecutableReplicaSet, replicas []*apiv1.Executable) objectChange {
	change := noChange

	observedReplicas := int32(len(replicas))
	if _, found := r.runningReplicaSets.Load(replicaSet.NamespacedName()); !found {
		rsData := executableReplicaSetData{
			lastScaled:     time.Now(),
			actualReplicas: observedReplicas,
		}
		r.runningReplicaSets.Store(replicaSet.NamespacedName(), rsData)
	}

	if replicaSet.Status.ObservedReplicas != observedReplicas {
		replicaSet.Status.ObservedReplicas = observedReplicas
		change = statusChanged
	}

	var numRunningReplicas, numFailedReplicas, numFinishedReplicas, numHealthyReplicas int32

	for _, exe := range replicas {
		switch exe.Status.State {
		case apiv1.ExecutableStateRunning:
			numRunningReplicas++
		case apiv1.ExecutableStateFailedToStart:
			numFailedReplicas++
		case apiv1.ExecutableStateFinished, apiv1.ExecutableStateTerminated:
			numFinishedReplicas++
		}
		if exe.Status.HealthStatus == apiv1.HealthStatusHealthy {
			numHealthyReplicas++
		}
	}

	if replicaSet.Status.RunningReplicas != numRunningReplicas {
		replicaSet.Status.RunningReplicas = numRunningReplicas
		change |= statusChanged
	}

	if replicaSet.Status.FailedReplicas != numFailedReplicas {
		replicaSet.Status.FailedReplicas = numFailedReplicas
		change |= statusChanged
	}

	if replicaSet.Status.FinishedReplicas != numFinishedReplicas {
		replicaSet.Status.FinishedReplicas = numFinishedReplicas
		change |= statusChanged
	}

	var newHealthStatus apiv1.HealthStatus
	if numHealthyReplicas >= replicaSet.Spec.Replicas {
		newHealthStatus = apiv1.HealthStatusHealthy
	} else if numHealthyReplicas > 0 {
		newHealthStatus = apiv1.HealthStatusCaution
	} else {
		newHealthStatus = apiv1.HealthStatusUnhealthy
	}
	if replicaSet.Status.HealthStatus != newHealthStatus {
		replicaSet.Status.HealthStatus = newHealthStatus
		change |= statusChanged
	}

	return change
}

// Attempt to make the number of replicas match the desired state. Will perform scale up or down as appropriate.
func (r *ExecutableReplicaSetReconciler) scaleReplicas(ctx context.Context, replicaSet *apiv1.ExecutableReplicaSet, replicas []*apiv1.Executable, log logr.Logger) objectChange {
	change := noChange
	currentScaleTime := metav1.NowMicro()

	rsData, found := r.runningReplicaSets.Load(replicaSet.NamespacedName())
	if !found {
		log.Error(fmt.Errorf("unable to find running replica set"), "unable to scale replicas")
		return noChange
	}

	observedReplicas := int32(len(replicas))

	if observedReplicas > replicaSet.Spec.Replicas {
		log.V(1).Info("scaling down replicas")
		// Scale down the replica set if there are too many
		for _, exe := range replicas[0 : observedReplicas-replicaSet.Spec.Replicas] {
			if replicaSet.Spec.StopOnScaleDown {
				// User requested to soft deleted scaled down replicas
				exePatch := exe.DeepCopy()
				annotations := exePatch.GetAnnotations()
				annotations[ExecutableReplicaStateAnnotation] = string(ExecutableReplicaSetStateInactive)
				exePatch.SetAnnotations(annotations)
				exePatch.Spec.Stop = true
				if err := r.Patch(ctx, exePatch, ctrl_client.MergeFromWithOptions(exe, ctrl_client.MergeFromWithOptimisticLock{})); err != nil {
					if errors.IsNotFound(err) {
						log.V(1).Info("executable not found, nothing to update", "exe", exe.NamespacedName())
					} else if errors.IsConflict(err) {
						// Expected optimistic concurrency check error, log it at debug level and move on
						log.V(1).Info("conflict while soft deleting Executable", "exe", exe.NamespacedName())
					} else {
						log.Error(err, "unable to soft delete Executable", "exe", exe.NamespacedName())
					}

					change |= additionalReconciliationNeeded
					continue
				} else {
					log.V(1).Info("soft deleted Executable", "exe", exe.NamespacedName())
				}
			} else {
				// Default delete on scale down behavior
				if err := r.Delete(ctx, exe, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground)); ctrl_client.IgnoreNotFound(err) != nil {
					log.Error(err, "unable to delete Executable", "exe", exe.NamespacedName())
					change |= additionalReconciliationNeeded
					continue
				} else {
					log.V(1).Info("deleted Executable", "exe", exe.NamespacedName())
				}
			}

			// If we successfully scaled down
			replicaSet.Status.LastScaleTime = currentScaleTime
			rsData.actualReplicas--
			rsData.lastScaled = currentScaleTime.Time
			r.runningReplicaSets.Store(replicaSet.NamespacedName(), rsData)

			change |= statusChanged
		}
	}

	if observedReplicas < replicaSet.Spec.Replicas {
		log.V(1).Info("scaling up replicas")
		// Scale up the replica set if there aren't enough
		for i := 0; i < int(replicaSet.Spec.Replicas-observedReplicas); i++ {
			if exe, exeSetupErr := r.createExecutable(replicaSet, log); exeSetupErr != nil {
				log.Error(exeSetupErr, "unable to create Executable")
				change |= additionalReconciliationNeeded
			} else if exeCreationErr := r.Create(ctx, exe); exeCreationErr != nil {
				log.Error(exeCreationErr, "unable to create Executable", "exe", exe.NamespacedName())
				change |= additionalReconciliationNeeded
			} else {
				replicaSet.Status.LastScaleTime = currentScaleTime
				rsData.actualReplicas++
				rsData.lastScaled = currentScaleTime.Time
				r.runningReplicaSets.Store(replicaSet.NamespacedName(), rsData)
				log.V(1).Info("created Executable", "exe", exe.NamespacedName())
				change |= statusChanged
			}
		}
	}

	return change
}

func (r *ExecutableReplicaSetReconciler) deleteReplicas(ctx context.Context, replicaSet *apiv1.ExecutableReplicaSet, log logr.Logger) {
	// Delete any inactive child Executable objects
	// List the active child Executable replicas
	var childExecutables apiv1.ExecutableList
	if err := r.List(
		ctx,
		&childExecutables,
		ctrl_client.InNamespace(replicaSet.Namespace),
		ctrl_client.MatchingFields{
			exeOwnerKey: replicaSet.Name,
		},
	); err != nil {
		log.Error(err, "failed to list inactive child Executable objects, continuing with deletion")
	} else {
		log.V(1).Info("deleting ExecutableReplicaSet children", "Count", childExecutables.ItemCount())
		for _, exe := range childExecutables.Items {
			if err = r.Delete(ctx, &exe, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "failed to delete inactive child Executable object", "exe", exe.NamespacedName())
			}
		}
	}
}

// Reconcile implements reconcile.Reconciler.
// The reconciler loop for ExecutableReplicaSet objects updates the status to reflect the current number
// of running replicas as well as attempting to ensure the replica count reaches the desired state.
// Changes to Executables "owned" by a given ExecutableReplicaSet will also trigger our reconciler loop,
// allowing us to respond to changes to both the ExecutableReplicaSet as well as its child Executables.
func (r *ExecutableReplicaSetReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	reconciliationDelay := additionalReconciliationDelay

	log := r.Log.WithValues("ExecutableReplicaSet", req.NamespacedName).WithValues("Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1))

	r.debouncer.OnReconcile(req.NamespacedName)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	replicaSet := apiv1.ExecutableReplicaSet{}
	if err := r.Get(ctx, req.NamespacedName, &replicaSet); err != nil {
		if errors.IsNotFound(err) {
			log.V(1).Info("ExecutableReplicaSet not found, nothing to do...")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the ExecutableReplicaSet")
			getFailedCounter.Add(ctx, 1)
			return ctrl.Result{}, err
		}
	} else {
		getSucceededCounter.Add(ctx, 1)
	}

	log = log.WithValues("DesiredReplicas", replicaSet.Spec.Replicas)

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(replicaSet.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if replicaSet.DeletionTimestamp != nil && !replicaSet.DeletionTimestamp.IsZero() {
		// Deletion has ben requested, so ensure that we start scaling down to zero replicas.
		if replicaSet.Spec.Replicas > 0 {
			log.V(1).Info("Deletion requested for ExecutableReplicaSet, scaling replicas to 0")
			replicaSet.Spec.Replicas = 0
		}
	}

	rsData, found := r.runningReplicaSets.Load(replicaSet.NamespacedName())
	var onSuccessfulSave func() = nil

	if replicaSet.DeletionTimestamp != nil && !replicaSet.DeletionTimestamp.IsZero() && found && rsData.actualReplicas == 0 {
		// Delete any remaining child replicas to ensure successful deletion (cleanup of soft deleted replicas)
		r.deleteReplicas(ctx, &replicaSet, log)

		// Deletion has been requested and the running replicas have been drained.
		log.V(1).Info("ExecutableReplicaSet is being deleted...")
		change = deleteFinalizer(&replicaSet, executableReplicaSetFinalizer, log)
		// Removing the finalizer will unblock the deletion of the ExecutableReplicaSet object.
		// Status update will fail, because the object will no longer be there, so suppress it.
		change &= ^statusChanged
		onSuccessfulSave = func() { r.runningReplicaSets.Delete(replicaSet.NamespacedName()) }
	} else {
		// We haven't been deleted or still have existing replicas, update our running replicas.
		change = ensureFinalizer(&replicaSet, executableReplicaSetFinalizer, log)
		// If we added a finalizer, we'll do the additional reconciliation next call
		if change == noChange {
			// List the active child Executable replicas
			var childExecutables apiv1.ExecutableList
			if err := r.List(
				ctx,
				&childExecutables,
				ctrl_client.InNamespace(req.Namespace),
				ctrl_client.MatchingFields{
					exeOwnerKey: req.Name,
				},
			); err != nil {
				log.Error(err, "failed to list child Executable objects")
				return ctrl.Result{}, err
			}

			totalReplicas := int32(len(childExecutables.Items))
			activeReplicas := []*apiv1.Executable{}
			for i, exe := range childExecutables.Items {
				state, annotationFound := exe.Annotations[ExecutableReplicaStateAnnotation]
				if annotationFound && state == string(ExecutableReplicaSetStateActive) {
					activeReplicas = append(activeReplicas, &childExecutables.Items[i])
				}
			}

			log = log.WithValues("TotalReplicas", totalReplicas).WithValues("ActiveReplicas", len(activeReplicas))

			change = r.updateReplicaStatus(&replicaSet, activeReplicas)

			log.V(1).Info("loaded child Executables", "RunningReplicas", replicaSet.Status.RunningReplicas, "FinishedReplicas", replicaSet.Status.FinishedReplicas, "FailedReplicas", replicaSet.Status.FailedReplicas)

			if replicaSet.Spec.Replicas != 0 && len(activeReplicas) != int(replicaSet.Spec.Replicas) && found && rsData.lastScaled.Add(scaleRateLimit).After(time.Now()) {
				log.Info("replica count changed, but scaling is rate limited", "LastScaled", rsData.lastScaled)
				reconciliationDelay = scaleRateLimit
				change |= additionalReconciliationNeeded
			} else {
				change |= r.scaleReplicas(ctx, &replicaSet, activeReplicas, log)
			}
		}
	}

	result, err := saveChangesWithCustomReconciliationDelay(
		r,
		ctx,
		&replicaSet,
		patch,
		change,
		reconciliationDelay,
		nil,
		log,
	)

	if err == nil && onSuccessfulSave != nil {
		onSuccessfulSave()
	}
	return result, err
}
