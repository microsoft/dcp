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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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

	// Debouncer used to schedule reconciliations.
	debouncer *reconcilerDebouncer[any]
}

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

	observedReplicas := int32(len(replicas.Items))
	rsData, found := r.runningReplicaSets.Load(replicaSet.NamespacedName())
	if !found {
		rsData = executableReplicaSetData{
			lastScaled:     time.Now(),
			actualReplicas: observedReplicas,
		}
		r.runningReplicaSets.Store(replicaSet.NamespacedName(), rsData)
	}

	if replicaSet.Status.ObservedReplicas != observedReplicas {
		replicaSet.Status.ObservedReplicas = observedReplicas
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
		case apiv1.ExecutableStateFinished, apiv1.ExecutableStateTerminated:
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

	if observedReplicas > replicaSet.Spec.Replicas {
		// Scale down the replica set if there are too many
		for _, exe := range replicas.Items[0 : observedReplicas-replicaSet.Spec.Replicas] {
			if err := r.Delete(ctx, &exe, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground)); ctrl_client.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete Executable", "exe", exe)
				change |= additionalReconciliationNeeded
			} else {
				replicaSet.Status.LastScaleTime = currentScaleTime
				rsData.actualReplicas--
				rsData.lastScaled = currentScaleTime.Time
				r.runningReplicaSets.Store(replicaSet.NamespacedName(), rsData)
				log.Info("deleted Executable", "exe", exe)
				change |= statusChanged
			}
		}
	}

	if observedReplicas < replicaSet.Spec.Replicas {
		// Scale up the replica set if there aren't enough
		for i := 0; i < int(replicaSet.Spec.Replicas-observedReplicas); i++ {
			if exe, err := r.createExecutable(replicaSet, log); err != nil {
				log.Error(err, "unable to create Executable")
				change |= additionalReconciliationNeeded
			} else if err := r.Create(ctx, exe); err != nil {
				log.Error(err, "unable to create Executable", "exe", exe)
				change |= additionalReconciliationNeeded
			} else {
				replicaSet.Status.LastScaleTime = currentScaleTime
				rsData.actualReplicas++
				rsData.lastScaled = currentScaleTime.Time
				r.runningReplicaSets.Store(replicaSet.NamespacedName(), rsData)
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
	log := r.Log.WithValues("ExecutableReplicaSet", req.NamespacedName).WithValues("Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1))

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

	rsData, found := r.runningReplicaSets.Load(replicaSet.NamespacedName())
	var onSuccessfulSave func() = nil

	if replicaSet.DeletionTimestamp != nil && !replicaSet.DeletionTimestamp.IsZero() && found && rsData.actualReplicas == 0 {
		// Deletion has been requested and the running replicas have been drained.
		log.Info("ExecutableReplicaSet is being deleted...")
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
			var childExecutables apiv1.ExecutableList
			err := r.List(ctx, &childExecutables, ctrl_client.InNamespace(req.Namespace), ctrl_client.MatchingFields{exeOwnerKey: req.Name})
			if err != nil {
				log.Error(err, "failed to list child Executable objects")
				return ctrl.Result{}, err
			}

			if childExecutables.ItemCount() != uint32(replicaSet.Spec.Replicas) && found && rsData.lastScaled.Add(scaleRateLimit).After(time.Now()) {
				log.Info("replica count changed, but scaling is rate limited", "lastScaled", rsData.lastScaled)
				return ctrl.Result{RequeueAfter: scaleRateLimit}, nil
			}

			change = r.updateReplicas(ctx, &replicaSet, childExecutables, log)
		}
	}

	result, err := saveChanges(r, ctx, &replicaSet, patch, change, log)
	if err == nil && onSuccessfulSave != nil {
		onSuccessfulSave()
	}
	return result, err
}
