// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ExecutableReplicaSetReconciler reconciles an ExecutableReplicaSet object
type ExecutableReplicaSetReconciler struct {
	ctrl_client.Client
	Log logr.Logger

	// Debouncer used to schedule reconciliations.
	debouncer *reconcilerDebouncer[any]
}

const (
	exeOwnerKey = ".metadata.controller" // client index key for child Executables
)

var (
	executableReplicaSetFinalizer string = fmt.Sprintf("%s/executable-replica-set-reconciler", apiv1.GroupVersion.Group)
)

func NewExecutableReplicaSetReconciler(client ctrl_client.Client, log logr.Logger) *ExecutableReplicaSetReconciler {
	r := ExecutableReplicaSetReconciler{
		Client:    client,
		debouncer: newReconcilerDebouncer[any](reconciliationDebounceDelay),
	}

	r.Log = log.WithValues("Controller", executableReplicaSetFinalizer)

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
		r.Log.Error(err, "failed to create index for ExecutableReplicaSet")
		return err
	}

	// Register for recoonciliation on changes to ExecutalbeReplicaSet objects as well
	// as owned Executable objects (metadata.ownerReferences pointing to an ExecutableReplicaSet)
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.ExecutableReplicaSet{}).
		Owns(&apiv1.Executable{}).
		Complete(r)
}

// Create a new Executable replica for the given ExecutableReplicaSet
func (r *ExecutableReplicaSetReconciler) createExecutable(replicaSet *apiv1.ExecutableReplicaSet, log logr.Logger) (*apiv1.Executable, error) {
	// Replica names are postfixed with a unique string to avoid collisions.
	uniqueName, err := MakeUniqueName(replicaSet.Name)
	if err != nil {
		return nil, err
	}

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

	for k, v := range replicaSet.Spec.Template.Labels {
		exe.Labels[k] = v
	}

	// Set the ExecutableReplica set as the owner of the Executable so that changes to the Executable will trigger
	// our reconciler loop.
	if err := ctrl.SetControllerReference(replicaSet, exe, r.Scheme()); err != nil {
		log.Error(err, "failed to create executable for ExecutableReplicaSet", "exe", exe)
		return nil, err
	}

	return exe, nil
}

// Ensure the ExecutableReplicaSet status matches the current state of the replicas and attempt to make the future state of the replicas match the desired state.
// If a child Executable is deleted, it will trigger our reconciler and potentially result in a scale up action to restore the desired number of replicas.
func (r *ExecutableReplicaSetReconciler) updateReplicas(ctx context.Context, replicaSet *apiv1.ExecutableReplicaSet, replicas apiv1.ExecutableList, log logr.Logger) objectChange {
	change := noChange

	numReplicas := int32(len(replicas.Items))
	if replicaSet.Status.ObservedReplicas != numReplicas {
		replicaSet.Status.ObservedReplicas = numReplicas
		change = statusChanged
	}

	runningReplicas := make([]*apiv1.Executable, 0)
	failedReplicas := make([]*apiv1.Executable, 0)
	finishedReplicas := make([]*apiv1.Executable, 0)
	for i, exe := range replicas.Items {
		switch exe.Status.State {
		case apiv1.ExecutableStateRunning:
			runningReplicas = append(runningReplicas, &replicas.Items[i])
		case apiv1.ExecutableStateFailedToStart:
			failedReplicas = append(failedReplicas, &replicas.Items[i])
		default:
			finishedReplicas = append(finishedReplicas, &replicas.Items[i])
		}
	}

	numRunningReplicas := int32(len(runningReplicas))
	if replicaSet.Status.RunningReplicas != numRunningReplicas {
		replicaSet.Status.RunningReplicas = numRunningReplicas
		change |= statusChanged
	}

	numFailedReplicas := int32(len(failedReplicas))
	if replicaSet.Status.FailedReplicas != numFailedReplicas {
		replicaSet.Status.FailedReplicas = numFailedReplicas
		change |= statusChanged
	}

	numFinishedReplicas := int32(len(finishedReplicas))
	if replicaSet.Status.FinishedReplicas != numFinishedReplicas {
		replicaSet.Status.FinishedReplicas = numFinishedReplicas
		change |= statusChanged
	}

	currentScaleTime := metav1.Now()

	if numReplicas > replicaSet.Spec.Replicas {
		// Scale down the replica set if there are too many
		for _, exe := range replicas.Items[0 : numReplicas-replicaSet.Spec.Replicas] {
			if err := r.Delete(ctx, &exe, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground)); ctrl_client.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete Executable", "exe", exe)
				change |= additionalReconciliationNeeded
			} else {
				replicaSet.Status.LastScaleTime = currentScaleTime
				log.Info("deleted Executable", "exe", exe)
				change |= statusChanged
			}
		}
	}

	if numReplicas < replicaSet.Spec.Replicas {
		// Scale up the replica set if there aren't enough
		for i := 0; i < int(replicaSet.Spec.Replicas-numReplicas); i++ {
			if exe, err := r.createExecutable(replicaSet, log); err != nil {
				log.Error(err, "unable to create Executable")
				change |= additionalReconciliationNeeded
			} else if err := r.Create(ctx, exe); err != nil {
				log.Error(err, "unable to create Executable", "exe", exe)
				change |= additionalReconciliationNeeded
			} else {
				replicaSet.Status.LastScaleTime = currentScaleTime
				log.Info("created Executable", "exe", exe)
				change |= statusChanged
			}
		}
	}

	return change
}

// Reconcile implements reconcile.Reconciler.
// The reconciler loop for ExecutableReplicaSet objects updates the status to reflect the current number
// of running replicas as well as attempting to ensure the replica count reaches the desired state.
// Changes to Executables "owned" by a given ExecutableReplicaSet will also trigger our reconciler loop,
// allowing us to respond to changes to both the ExecutableReplicaSet as well as its child Executables.
func (r *ExecutableReplicaSetReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := r.Log.WithValues("ExecutableReplicaSet", req.NamespacedName)

	r.debouncer.OnReconcile(req.NamespacedName)

	select {
	case _, isOpen := <-ctx.Done():
		if !isOpen {
			log.V(1).Info("Request context expired, nothing to do...")
			return ctrl.Result{}, nil
		}
	default: // not done, proceed
	}

	replicaSet := apiv1.ExecutableReplicaSet{}
	if err := r.Get(ctx, req.NamespacedName, &replicaSet); err != nil {
		if errors.IsNotFound(err) {
			log.V(1).Info("ExecutableReplicaSet not found, nothing to do...")
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the ExecutableReplicaSet")
			return ctrl.Result{}, err
		}
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(replicaSet.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if replicaSet.DeletionTimestamp != nil && !replicaSet.DeletionTimestamp.IsZero() {
		// Deletion has ben requested, so ensure that we start scaling down to zero replicas.
		if replicaSet.Spec.Replicas > 0 {
			log.Info("Deletion requested for ExecutableReplicaSet, scaling replicas to 0")
			replicaSet.Spec.Replicas = 0
		}
	}

	if replicaSet.DeletionTimestamp != nil && !replicaSet.DeletionTimestamp.IsZero() && replicaSet.Status.ObservedReplicas < 1 {
		// Deletion has been requested and the running replicas have been drained.
		log.Info("ExecutableReplicaSet is being deleted...")
		change = deleteFinalizer(&replicaSet, executableReplicaSetFinalizer)
		// Removing the finalizer will unblock the deletion of the ExecutableReplicaSet object.
		// Status update will fail, because the object will no longer be there, so suppress it.
		change &= ^statusChanged
	} else {
		// We haven't been deleted or still have existing replicas, update our running replicas.
		change = ensureFinalizer(&replicaSet, executableReplicaSetFinalizer)
		// If we added a finalizer, we'll do the additional reconciliation next call
		if change == noChange {
			var childExecutables apiv1.ExecutableList
			err := r.List(ctx, &childExecutables, ctrl_client.InNamespace(req.Namespace), ctrl_client.MatchingFields{exeOwnerKey: req.Name})
			if err != nil {
				log.Error(err, "failed to list child Executable objects")
				return ctrl.Result{}, err
			}

			change |= r.updateReplicas(ctx, &replicaSet, childExecutables, log)
		}
	}

	if change == noChange {
		log.V(1).Info("no changes detected for Executable, continue monitoring...")
		return ctrl.Result{}, nil
	}

	var update *apiv1.ExecutableReplicaSet

	if (change & statusChanged) != 0 {
		update = replicaSet.DeepCopy()
		if err := r.Status().Patch(ctx, update, patch); err != nil {
			log.Error(err, "Executable status update failed")
			return ctrl.Result{}, err
		}
		log.V(1).Info("Executable status update succeeded")
	}

	if (change & (metadataChanged | specChanged)) != 0 {
		update = replicaSet.DeepCopy()
		if err := r.Patch(ctx, update, patch); err != nil {
			log.Error(err, "Executable update failed")
			return ctrl.Result{}, err
		}
		log.V(1).Info("Executable update succeeded")
	}

	if (change & additionalReconciliationNeeded) != 0 {
		log.V(1).Info("scheduling additional reconciliation for ExecutableReplicaSet...")
		return ctrl.Result{RequeueAfter: additionalReconciliationDelay}, nil
	}

	return ctrl.Result{}, nil
}
