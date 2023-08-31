// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"github.com/smallnest/chanx"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	ctrl_handler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
)

// ExecutableReconciler reconciles a Executable object
type ExecutableReconciler struct {
	ctrl_client.Client

	Log               logr.Logger
	ExecutableRunners map[apiv1.ExecutionType]ExecutableRunner

	// A map that stores information about running Executables,
	// searchable by Executable name (first key), or run ID (second key).
	runs *maps.SynchronizedDualKeyMap[types.NamespacedName, RunID, apiv1.ExecutableStatus]

	// Channel used to trigger reconciliation function when underlying run status changes.
	notifyRunChanged *chanx.UnboundedChan[ctrl_event.GenericEvent]

	// Debouncer used to schedule reconciliations. Extra data carried is the finished run ID.
	debouncer *reconcilerDebouncer[RunID]
}

var (
	executableFinalizer string = fmt.Sprintf("%s/executable-reconciler", apiv1.GroupVersion.Group)
)

func NewExecutableReconciler(lifetimeCtx context.Context, client ctrl_client.Client, log logr.Logger, executableRunners map[apiv1.ExecutionType]ExecutableRunner) *ExecutableReconciler {
	r := ExecutableReconciler{
		Client:            client,
		ExecutableRunners: executableRunners,
		runs:              maps.NewSynchronizedDualKeyMap[types.NamespacedName, RunID, apiv1.ExecutableStatus](),
		notifyRunChanged:  chanx.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx, 1),
		debouncer:         newReconcilerDebouncer[RunID](reconciliationDebounceDelay),
	}

	r.Log = log.WithValues("Controller", executableFinalizer)

	return &r
}

func (r *ExecutableReconciler) SetupWithManager(mgr ctrl.Manager) error {
	src := ctrl_source.Channel{
		Source: r.notifyRunChanged.Out,
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Executable{}).
		WatchesRawSource(&src, &ctrl_handler.EnqueueRequestForObject{}).
		Complete(r)
}

/*
The main reconciler function of the Executable controller.

TODO: describe the work at a high level.

Notes:
Updating the Executable path/working directory/arguments/environment will not effect an Executable run once it started.
Status will be updated based on the status of the corresponding run and the run will be terminated if
the Executable is deleted.
*/
func (r *ExecutableReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("Executable", req.NamespacedName)

	r.debouncer.OnReconcile(req.NamespacedName)

	// Check to see if the request context has already expired
	select {
	case _, isOpen := <-ctx.Done():
		if !isOpen {
			log.V(1).Info("Request context expired, nothing to do...")
			return ctrl.Result{}, nil
		}
	default: // not done, proceed
	}

	// Retrieve the Executable object
	exe := apiv1.Executable{}
	if err := r.Get(ctx, req.NamespacedName, &exe); err != nil {
		if errors.IsNotFound(err) {
			// Ensure the cache of Executable to run ID is cleared
			r.runs.DeleteByFirstKey(req.NamespacedName)
			log.V(1).Info("Executable not found, nothing to do...")
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the Executable", "exe", exe)
			return ctrl.Result{}, err
		}
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(exe.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if exe.DeletionTimestamp != nil && !exe.DeletionTimestamp.IsZero() {
		log.Info("Executable is being deleted...")
		r.stopExecutable(ctx, &exe, log)
		change = deleteFinalizer(&exe, executableFinalizer)
		r.deleteOutputFiles(&exe, log)
	} else {
		change = ensureFinalizer(&exe, executableFinalizer)
		// If we added a finalizer, we'll do the additional reconciliation next call
		if change == noChange {
			change |= r.updateRunState(&exe, log)
			change |= r.runExecutable(ctx, &exe, log)
		}
	}

	var update *apiv1.Executable

	// Apply one update per reconciliation function invocation,
	// to avoid observing "partially updated" objects during subsequent reconciliations.
	switch {
	case change == noChange:
		log.V(1).Info("no changes detected for Executable, continue monitoring...")
		return ctrl.Result{}, nil
	case (change & statusChanged) != 0:
		update = exe.DeepCopy()
		if err := r.Status().Patch(ctx, update, patch); err != nil {
			log.Error(err, "Executable status update failed")
			return ctrl.Result{}, err
		}
		log.V(1).Info("Executable status update succeeded")
	case (change & (metadataChanged | specChanged)) != 0:
		update = exe.DeepCopy()
		if err := r.Patch(ctx, update, patch); err != nil {
			log.Error(err, "Executable update failed")
			return ctrl.Result{}, err
		}
		log.V(1).Info("Executable update succeeded")
	}

	if exe.Done() {
		log.V(1).Info("Executable reached done state")
	}

	if (change & additionalReconciliationNeeded) != 0 {
		log.V(1).Info("scheduling additional reconciliation for Executable...")
		return ctrl.Result{RequeueAfter: additionalReconciliationDelay}, nil
	} else {
		return ctrl.Result{}, nil
	}
}

// Handle notification about completed Executable run. This function runs outside of the reconcilation loop,
// so we just memorize the process exit code (if available) in the run status map,
// and not attempt to modify any Kubernetes data.
func (r *ExecutableReconciler) OnRunCompleted(runID RunID, exitCode int32, err error) {
	name, ps, found := r.runs.FindBySecondKey(runID)

	// It's possible we receive a notification about a run we are not tracking, but that means we
	// no longer care about its status, so we can just ignore it.
	if !found {
		return
	}

	var effectiveExitCode int32
	if err != nil {
		r.Log.V(1).Info("Executable run could not be tracked", "RunID", runID, "Error", err.Error())
		effectiveExitCode = apiv1.UnknownExitCode
	} else {
		r.Log.V(1).Info("Executable run ended", "RunID", runID, "exitCode", exitCode)
		effectiveExitCode = exitCode
	}

	// Memorize exit code
	ps.ExitCode = effectiveExitCode
	ps.State = apiv1.ExecutableStateFinished

	r.runs.Store(name, runID, ps)

	// Schedule reconciliation for corresponding executable
	scheduleErr := r.debouncer.ReconciliationNeeded(name, runID, func(rti reconcileTriggerInput[RunID]) error {
		return r.scheduleExecutableReconciliation(rti.target, rti.input)
	})
	if scheduleErr != nil {
		r.Log.Error(scheduleErr, "could not schedule reconciliation for Executable object")
	}
}

// Performs actual Executable startup and updates status with the appropriate data.
// If startup is successful, starts tracking the run.
func (r *ExecutableReconciler) runExecutable(ctx context.Context, exe *apiv1.Executable, log logr.Logger) objectChange {
	var err error

	if !exe.Status.StartupTimestamp.IsZero() {
		log.V(1).Info("Executable already started...", "ExecutionID", exe.Status.ExecutionID)
		return noChange
	}

	if exe.Done() {
		log.V(1).Info("Executable reached done state, nothing to do...")
		return noChange
	}

	if _, rps, found := r.runs.FindByFirstKey(exe.NamespacedName()); found {
		// We are already tracking a run for this Executable, ensure the status matches the current state.
		log.V(1).Info("Executable already started...", "ExecutionID", exe.Status.ExecutionID)
		rps.CopyTo(exe)
		return statusChanged
	}

	executionType := exe.Spec.ExecutionType
	if executionType == "" {
		executionType = apiv1.ExecutionTypeProcess
	}
	runner, found := r.ExecutableRunners[executionType]
	if !found {
		log.Error(fmt.Errorf("no runner found for execution type '%s'", executionType), "the Executable cannot be run and will be marked as finished")
		exe.Status.State = apiv1.ExecutableStateFailedToStart
		exe.Status.FinishTimestamp = metav1.Now()
		return statusChanged
	}

	runID, startWaitForRunCompletion, err := runner.StartRun(ctx, exe, r, log)

	if err == nil {
		r.runs.Store(exe.NamespacedName(), runID, exe.Status)

		startWaitForRunCompletion()
		return statusChanged
	} else {
		if exe.Status.State != apiv1.ExecutableStateFailedToStart {
			// The executor did not mark the Executable as failed to start, so we should retry.
			return additionalReconciliationNeeded
		} else {
			// The Executable failed to start and reached the final state.
			return statusChanged
		}
	}
}

// Stops the underlying Executable run, if any.
// The Execuatable data update related to stopped run is handled by the caller.
func (r *ExecutableReconciler) stopExecutable(ctx context.Context, exe *apiv1.Executable, log logr.Logger) {
	var runID RunID = RunID(exe.Status.ExecutionID)
	if runID == "" || exe.Done() {
		return // Nothing to do--the Executable is not running
	}

	// We are about to terminate the run. Since the run is not allowed to complete normally,
	// we are not interested in its exit code (it will indicate a failure,
	// but it is a failure induced by the Executable user), so we stop tracking the run now.
	r.runs.DeleteBySecondKey(runID)

	runner, found := r.ExecutableRunners[exe.Spec.ExecutionType]
	if !found {
		log.Error(fmt.Errorf("no runner found for execution type '%s'", exe.Spec.ExecutionType), "the Executable cannot be stopped")
		return
	}

	err := runner.StopRun(ctx, runID, log)
	if err != nil {
		log.V(1).Info("could not stop the Executable", "RunID", runID, "Error", err.Error())
	} else {
		log.V(1).Info("Executable stopped", "RunID", runID)
	}
}

// Called by the main reconciler function, this function will update the Executable run state
// based on run state change notifications we have received.
func (r *ExecutableReconciler) updateRunState(exe *apiv1.Executable, log logr.Logger) objectChange {
	var change objectChange = noChange

	if exe.Status.State == apiv1.ExecutableStateRunning {
		runID := RunID(exe.Status.ExecutionID)
		if _, ps, found := r.runs.FindBySecondKey(runID); found && ps.State != apiv1.ExecutableStateRunning {
			log.Info("Executable run finished", "RunID", runID, "ExitCode", ps.ExitCode)
			exe.UpdateRunningStatus(ps.ExitCode, ps.State)
			r.runs.DeleteBySecondKey(runID)
			change = statusChanged
		}
	}

	return change
}

func (r *ExecutableReconciler) deleteOutputFiles(exe *apiv1.Executable, log logr.Logger) {
	// Do not bother updating the Executable object--this method is called when the object is being deleted.

	if exe.Status.StdOutFile != "" {
		if err := os.Remove(exe.Status.StdOutFile); err != nil {
			log.Error(err, "could not remove process's standard output file", "path", exe.Status.StdOutFile)
		}
	}

	if exe.Status.StdErrFile != "" {
		if err := os.Remove(exe.Status.StdErrFile); err != nil {
			log.Error(err, "could not remove process's standard error file", "path", exe.Status.StdErrFile)
		}
	}
}

func (r *ExecutableReconciler) scheduleExecutableReconciliation(target types.NamespacedName, finishedRunID RunID) error {
	event := ctrl_event.GenericEvent{
		Object: &apiv1.Executable{
			ObjectMeta: metav1.ObjectMeta{
				Name:      target.Name,
				Namespace: target.Namespace,
			},
		},
	}

	select {
	case r.notifyRunChanged.In <- event:
		return nil // Reconciliation scheduled successfully

	default:
		// We could not schedule the reconciliation. This should never really happen, given that we are usin an unbounded channel.
		// If this happens though, returning from OnProcessExited() handler is most important.
		err := fmt.Errorf("could not schedule reconciliation for Executable whose run has finished")
		r.Log.Error(err, "the state of the Executable may not reflect the real world", "Executable", target.Name, "FinishedRunID", finishedRunID)
		return err
	}
}
