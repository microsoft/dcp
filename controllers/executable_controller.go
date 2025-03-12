// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/joho/godotenv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	ctrl_handler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/health"
	"github.com/microsoft/usvc-apiserver/internal/logs"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

type executableStateInitializerFunc = stateInitializerFunc[
	apiv1.Executable, *apiv1.Executable,
	ExecutableReconciler, *ExecutableReconciler,
	apiv1.ExecutableState,
	ExecutableRunInfo, *ExecutableRunInfo,
]

var executableStateInitializers = map[apiv1.ExecutableState]executableStateInitializerFunc{
	apiv1.ExecutableStateEmpty:         handleNewExecutable,
	apiv1.ExecutableStateStarting:      handleNewExecutable,
	apiv1.ExecutableStateRunning:       ensureExecutableRunningState,
	apiv1.ExecutableStateStopping:      ensureExecutableStoppingState,
	apiv1.ExecutableStateTerminated:    ensureExecutableFinalState,
	apiv1.ExecutableStateFailedToStart: ensureExecutableFinalState,
	apiv1.ExecutableStateFinished:      ensureExecutableFinalState,
	apiv1.ExecutableStateUnknown:       ensureExecutableFinalState,
}

type runStateMap = ObjectStateMap[RunID, ExecutableRunInfo, *ExecutableRunInfo]

// ExecutableReconciler reconciles a Executable object
type ExecutableReconciler struct {
	ctrl_client.Client

	Log                 logr.Logger
	reconciliationSeqNo uint32
	ExecutableRunners   map[apiv1.ExecutionType]ExecutableRunner

	// A map that stores information about running Executables,
	// searchable by Executable name (first key), or run ID (second key).
	runs *runStateMap

	// Health probe set used to execute health probes.
	hpSet *health.HealthProbeSet

	// Channel used to receive health probe results.
	healthProbeCh *concurrency.UnboundedChan[health.HealthProbeReport]

	// Channel used to trigger reconciliation function when underlying run status changes.
	notifyRunChanged *concurrency.UnboundedChan[ctrl_event.GenericEvent]

	// Debouncer used to schedule reconciliations. No extra data is needed for the debouncer.
	debouncer *reconcilerDebouncer[struct{}]

	// Reconciler lifetime context, used to cancel operations during reconciler shutdown
	lifetimeCtx context.Context

	// A WorkQueue for stopping Executables (which might take a while).
	stopQueue *resiliency.WorkQueue
}

var (
	executableFinalizer string = fmt.Sprintf("%s/executable-reconciler", apiv1.GroupVersion.Group)
	executableKind             = apiv1.GroupVersion.WithKind(reflect.TypeOf(apiv1.Executable{}).Name())
)

const (
	// The maximum number of Executable stop operations that can be executed in parallel.
	// Since it mostly involves sending signals and waiting for the process to exit,
	// we take the maximum level of concurrency allowed by WorkQueue.
	maxParallelExecutableStops = math.MaxUint8
)

func NewExecutableReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	log logr.Logger,
	executableRunners map[apiv1.ExecutionType]ExecutableRunner,
	healthProbeSet *health.HealthProbeSet,
) *ExecutableReconciler {
	r := ExecutableReconciler{
		Client:            client,
		ExecutableRunners: executableRunners,
		runs:              NewObjectStateMap[RunID, ExecutableRunInfo](),
		hpSet:             healthProbeSet,
		healthProbeCh:     concurrency.NewUnboundedChan[health.HealthProbeReport](lifetimeCtx),
		notifyRunChanged:  concurrency.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx),
		debouncer:         newReconcilerDebouncer[struct{}](),
		lifetimeCtx:       lifetimeCtx,
		stopQueue:         resiliency.NewWorkQueue(lifetimeCtx, maxParallelExecutableStops),
		Log:               log,
	}

	go r.handleHealthProbeResults(lifetimeCtx)
	_, subscriptionErr := r.hpSet.Subscribe(r.healthProbeCh.In, executableKind)
	if subscriptionErr != nil {
		// Should never happen
		log.Error(subscriptionErr, "could not subscribe to health probe results, the health of Executables will never be correctly reported")
	}

	return &r
}

func (r *ExecutableReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	src := ctrl_source.Channel(r.notifyRunChanged.Out, &ctrl_handler.EnqueueRequestForObject{})
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Executable{}).
		Owns(&apiv1.Endpoint{}).
		WatchesRawSource(src).
		Named(name).
		Complete(r)
}

/*
The main reconciler function of the Executable controller.

Notes:
Updating the Executable path/working directory/arguments/environment will not effect an Executable run once it started.
Status will be updated based on the status of the corresponding run and the run will be terminated if
the Executable is deleted.
*/
func (r *ExecutableReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("Executable", req.NamespacedName).WithValues("Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1))

	r.debouncer.OnReconcile(req.NamespacedName)

	// Check to see if the request context has already expired
	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	// Retrieve the Executable object
	exe := apiv1.Executable{}
	if err := r.Get(ctx, req.NamespacedName, &exe); err != nil {
		if apierrors.IsNotFound(err) {
			// Ensure the cache of Executable to run ID is cleared
			r.runs.DeleteByNamespacedName(req.NamespacedName)
			log.V(1).Info("Executable not found, nothing to do...")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the Executable", "exe", exe)
			getFailedCounter.Add(ctx, 1)
			return ctrl.Result{}, err
		}
	} else {
		getSucceededCounter.Add(ctx, 1)
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(exe.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	r.runs.RunDeferredOps(req.NamespacedName)

	if exe.DeletionTimestamp != nil && !exe.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, &exe, log)
	} else if change = ensureFinalizer(&exe, executableFinalizer, log); change != noChange {
		// If we added a finalizer, we'll do the additional reconciliation next call
	} else {
		change = r.manageExecutable(ctx, &exe, log)
	}

	result, err := saveChanges(r, ctx, &exe, patch, change, nil, log)
	if exe.Done() {
		log.V(1).Info("Executable reached done state")
	}
	return result, err
}

func (r *ExecutableReconciler) handleDeletionRequest(ctx context.Context, exe *apiv1.Executable, log logr.Logger) objectChange {
	_, runInfo := r.runs.BorrowByNamespacedName(exe.NamespacedName())

	var change objectChange

	switch {
	case runInfo == nil:
		change = deleteFinalizer(exe, executableFinalizer, log)

	case runInfo.ExeState == apiv1.ExecutableStateStopping || runInfo.ExeState == apiv1.ExecutableStateStarting:
		// We just need to wait for the Executable to exit the transient state
		change = r.manageExecutable(ctx, exe, log)

	case runInfo.ExeState == apiv1.ExecutableStateRunning:
		// Transition to stopping state
		runInfo.ExeState = apiv1.ExecutableStateStopping
		stoppingInitializer := getStateInitializer(executableStateInitializers, apiv1.ExecutableStateStopping, log)
		change = stoppingInitializer(ctx, r, exe, apiv1.ExecutableStateStopping, runInfo, log)
		r.runs.Update(exe.NamespacedName(), runInfo.RunID, runInfo)

	default: // In initial or final state
		log.V(1).Info("Executable is being deleted...")
		r.releaseExecutableResources(ctx, exe, log)
		change = deleteFinalizer(exe, executableFinalizer, log)
		r.runs.DeleteByNamespacedName(exe.NamespacedName())
	}

	return change
}

func (r *ExecutableReconciler) manageExecutable(ctx context.Context, exe *apiv1.Executable, log logr.Logger) objectChange {
	targetExecutableState := exe.Status.State
	_, runInfo := r.runs.BorrowByNamespacedName(exe.NamespacedName())
	if runInfo != nil {
		// In-memory run info is not subject to issues related to caching
		// and failure to update Executable Status due to conflict, so it takes precedence.
		targetExecutableState = runInfo.ExeState
	}

	// Even if Executable.State == targetExecutableState, we still want to run the initializer
	// to ensure that Executable.Status is up to date and that the real-world resources
	// associated with Executable object are up to date.
	initializer := getStateInitializer(executableStateInitializers, targetExecutableState, log)
	change := initializer(ctx, r, exe, targetExecutableState, runInfo, log)

	if runInfo != nil {
		r.runs.Update(exe.NamespacedName(), runInfo.RunID, runInfo)
	}

	return change
}

func handleNewExecutable(
	ctx context.Context,
	r *ExecutableReconciler,
	exe *apiv1.Executable,
	_ apiv1.ExecutableState,
	runInfo *ExecutableRunInfo,
	log logr.Logger,
) objectChange {
	if exe.Spec.Stop && exe.Status.State == apiv1.ExecutableStateEmpty && runInfo == nil {
		log.Info("Executable.Stop property was set to true before Executable was started, marking Executable as 'failed to start'...")
		r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
		return statusChanged
	}

	if runInfo == nil {
		// This is brand new Executable and we need to get it started.
		return r.startExecutable(ctx, exe, log)
	} else {
		// Ensure the status matches the current state.
		return runInfo.ApplyTo(exe, log)
	}
}

func ensureExecutableRunningState(
	ctx context.Context,
	r *ExecutableReconciler,
	exe *apiv1.Executable,
	_ apiv1.ExecutableState,
	runInfo *ExecutableRunInfo,
	log logr.Logger,
) objectChange {
	change := r.setExecutableState(exe, apiv1.ExecutableStateRunning)

	if runInfo == nil {
		// Might happen if the Executable is being deleted and the run was already terminated
		// (we are seeing stale Executable data due to Kubernetes container-runtime caching).
		return change
	}

	if exe.Spec.Stop {
		runInfo.ExeState = apiv1.ExecutableStateStopping
		change |= r.setExecutableState(exe, apiv1.ExecutableStateStopping)
		return change
	}

	// Ensure the status matches the current state.
	change |= runInfo.ApplyTo(exe, log)

	ensureEndpointsForWorkload(ctx, r, exe, runInfo.ReservedPorts, log)

	if len(exe.Spec.HealthProbes) > 0 && (runInfo.healthProbesEnabled == nil || !*runInfo.healthProbesEnabled) {
		log.V(1).Info("enabling health probes for Executable...")

		// healthProbesEnabled is used to avoid enabling health probes multiple times for the same Executable
		// Enablement only fails if the lifetime context is cancelled, or under "should never happen" conditions,
		// so we set it unconditionally to avoid repeated attempts.
		runInfo.healthProbesEnabled = new(bool)
		*runInfo.healthProbesEnabled = true

		probeErr := r.hpSet.EnableProbes(apiv1.GetNamespacedNameWithKind(exe), exe.Spec.HealthProbes)
		if probeErr != nil {
			log.Error(probeErr, "could not enable health probes for Executable")
		}
	}

	return change
}

func ensureExecutableStoppingState(
	ctx context.Context,
	r *ExecutableReconciler,
	exe *apiv1.Executable,
	_ apiv1.ExecutableState,
	runInfo *ExecutableRunInfo,
	log logr.Logger,
) objectChange {
	change := r.setExecutableState(exe, apiv1.ExecutableStateStopping)

	if runInfo == nil {
		return change // See ensureExecutableRunningState() for explanation
	}

	if !runInfo.stopAttemptInitiated {
		log.V(1).Info("attempting to stop the Executable...", "RunID", runInfo.RunID)
		runInfo.stopAttemptInitiated = true
		runInfo.ExeState = apiv1.ExecutableStateStopping
		runInfoCopy := runInfo.Clone()
		err := r.stopQueue.Enqueue(r.stopExecutableFunc(exe, runInfoCopy, log))
		if err != nil {
			log.Error(err, "could not enqueue Executable stop operation")
			change |= ensureExecutableFinalState(ctx, r, exe, apiv1.ExecutableStateUnknown, runInfo, log)
			return change
		}
	}

	change |= runInfo.ApplyTo(exe, log)

	if len(exe.Spec.HealthProbes) > 0 && runInfo.healthProbesEnabled != nil && *runInfo.healthProbesEnabled {
		log.V(1).Info("disabling health probes for Executable...")
		*runInfo.healthProbesEnabled = false
		r.hpSet.DisableProbes(apiv1.GetNamespacedNameWithKind(exe))
	}

	removeEndpointsForWorkload(r, ctx, exe, log)

	return change
}

func ensureExecutableFinalState(
	ctx context.Context,
	r *ExecutableReconciler,
	exe *apiv1.Executable,
	desiredState apiv1.ExecutableState,
	runInfo *ExecutableRunInfo,
	log logr.Logger,
) objectChange {
	change := r.setExecutableState(exe, desiredState)

	if runInfo == nil {
		return change // See ensureExecutableRunningState() for explanation
	}

	// Ensure the status matches the current state.
	change |= runInfo.ApplyTo(exe, log)

	if len(exe.Spec.HealthProbes) > 0 && runInfo.healthProbesEnabled != nil && *runInfo.healthProbesEnabled {
		log.V(1).Info("disabling health probes for Executable...")
		*runInfo.healthProbesEnabled = false
		r.hpSet.DisableProbes(apiv1.GetNamespacedNameWithKind(exe))
	}

	removeEndpointsForWorkload(r, ctx, exe, log)

	return change
}

func (r *ExecutableReconciler) startExecutable(ctx context.Context, exe *apiv1.Executable, log logr.Logger) objectChange {
	runner, runnerNotFoundErr := r.getExecutableRunner(exe)
	if runnerNotFoundErr != nil {
		log.Error(runnerNotFoundErr, "the Executable cannot be run and will be marked as failed to start")
		r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
		return statusChanged
	}

	// Ports reserved for services that the Executable implements without specifying the desired port to use (via service-producer annotation).
	reservedServicePorts := make(map[types.NamespacedName]int32)

	err := r.computeEffectiveEnvironment(ctx, exe, reservedServicePorts, log)
	if isTransientTemplateError(err) {
		log.Info("could not compute effective environment for the Executable, retrying startup...", "Cause", err.Error())
		return additionalReconciliationNeeded
	} else if err != nil {
		log.Error(err, "could not compute effective environment for the Executable")
		r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
		return statusChanged
	}

	err = r.computeEffectiveInvocationArgs(ctx, exe, reservedServicePorts, log)
	if isTransientTemplateError(err) {
		log.Info("could not compute effective invocation arguments for the Executable, retrying startup...", "Cause", err.Error())
		return additionalReconciliationNeeded
	} else if err != nil {
		log.Error(err, "could not compute effective invocation arguments for the Executable")
		r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
		return statusChanged
	}

	if len(reservedServicePorts) > 0 {
		log.V(1).Info("reserving service ports...",
			"services", maps.Keys(reservedServicePorts),
			"ports", maps.Values(reservedServicePorts),
		)
	}

	log.V(1).Info("starting Executable...")
	run := NewRunInfo()
	run.ReservedPorts = reservedServicePorts
	r.runs.Store(exe.NamespacedName(), getStartingRunID(exe.NamespacedName()), run.Clone())

	err = runner.StartRun(ctx, exe, run, r, log)

	if err != nil {
		log.Error(err, "failed to start Executable")

		r.runs.DeleteByNamespacedName(exe.NamespacedName())

		if run.ExeState != apiv1.ExecutableStateFailedToStart {
			// The executor did not mark the Executable as failed to start, so we should retry.
			return additionalReconciliationNeeded
		} else {
			// The Executable failed to start and reached the final state.
			run.ApplyTo(exe, log)
			r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
			return statusChanged
		}
	}

	switch run.ExeState {
	case apiv1.ExecutableStateRunning, apiv1.ExecutableStateFailedToStart, apiv1.ExecutableStateFinished:
		// This was a synchronous startup, OnStartupCompleted() has been called already and queued the update of the runs map.
		// Endpoints will be created during next reconciliation loop, no need to call ensureEndpointsForWorkload() here.

		r.setExecutableState(exe, run.ExeState) // Make sure health status is updated.

	case apiv1.ExecutableStateStarting:
		_ = r.runs.Update(exe.NamespacedName(), getStartingRunID(exe.NamespacedName()), run)

	default:
		// Should never happen
		err = fmt.Errorf("unexpected Executable state after startup")
		log.Error(err, "ExecutableState", run.ExeState)
		log.V(1).Error(err, "StartingRunInfo", run.String())

		r.setExecutableState(exe, apiv1.ExecutableStateUnknown)
	}

	return statusChanged
}

func (r *ExecutableReconciler) OnMainProcessChanged(runID RunID, pid process.Pid_t) {
	r.processRunChangeNotification(runID, pid, apiv1.UnknownExitCode, nil, func(ri *ExecutableRunInfo) {
		ri.ExeState = apiv1.ExecutableStateRunning
	})
}

func (r *ExecutableReconciler) OnRunCompleted(runID RunID, exitCode *int32, err error) {
	r.processRunChangeNotification(runID, process.UnknownPID, exitCode, err, func(ri *ExecutableRunInfo) {
		ri.ExeState = apiv1.ExecutableStateFinished
		ri.FinishTimestamp = metav1.NowMicro()
	})
}

// Handle notification about main process change or run completion for an Executable run.
// This function runs outside of the reconciliation loop, so we just memorize the PID and process exit code
// (if available) in the run status map, and not attempt to modify any Executable object data.
func (r *ExecutableReconciler) processRunChangeNotification(
	runID RunID,
	pid process.Pid_t,
	exitCode *int32,
	err error,
	updateExeState func(*ExecutableRunInfo),
) {
	name, runInfo := r.runs.BorrowByStateKey(runID)

	// It's possible we receive a notification about a run we are not tracking, but that means we
	// no longer care about its status, so we can just ignore it.
	if runInfo == nil {
		return
	}

	updateExeState(runInfo)

	var effectiveExitCode *int32
	if err != nil {
		r.Log.V(1).Info("Executable run could not be tracked",
			"Executable", name.String(),
			"RunID", runID,
			"LastState", runInfo.ExeState,
			"Error", err.Error(),
		)
		effectiveExitCode = apiv1.UnknownExitCode
	} else {
		r.Log.V(1).Info("queue Executable run change",
			"Executable", name.String(),
			"RunID", runID,
			"LastState", runInfo.ExeState,
			"PID", pid,
			"ExitCode", exitCode,
			"ExecutableState", runInfo.ExeState,
		)
		effectiveExitCode = exitCode
	}

	// Memorize PID and (if applicable) exit code
	runInfo.ExitCode = effectiveExitCode
	if pid > 0 {
		pointers.SetValue(&runInfo.Pid, (*int64)(&pid))
	}

	r.runs.QueueDeferredOp(name, func(types.NamespacedName, RunID) {
		// The run may have been deleted by the time we get here, so we do not care if Update() returns false.
		_ = r.runs.Update(name, runID, runInfo)
	})

	r.debouncer.ReconciliationNeeded(r.lifetimeCtx, name, struct{}{}, r.scheduleExecutableReconciliation)
}

// Handle setting up process tracking once an Executable has transitioned from newly created or starting to a stable state such as running or finished.
func (r *ExecutableReconciler) OnStartupCompleted(
	exeName types.NamespacedName,
	startedRunInfo *ExecutableRunInfo,
	startWaitForRunCompletion func(),
) {
	r.Log.V(1).Info("Executable completed startup",
		"Executable", exeName.String(),
		"RunID", startedRunInfo.RunID,
		"NewState", startedRunInfo.ExeState,
		"NewExitCode", startedRunInfo.ExitCode,
	)

	// OnStartingCompleted might be invoked asynchronously. To avoid race conditions,
	//  we always queue updates to the runs map and run them as part of reconciliation function.
	r.runs.QueueDeferredOp(exeName, func(types.NamespacedName, RunID) {
		startingRunID, ri := r.runs.BorrowByNamespacedName(exeName)
		if ri == nil {
			// Should never happen
			r.Log.Error(fmt.Errorf("could not find starting run data after Executable start attempt"),
				"Executable", exeName.String(),
				"RunID", startedRunInfo.RunID,
				"NewState", startedRunInfo.ExeState,
				"NewExitCode", startedRunInfo.ExitCode,
			)
			return
		}

		ri.UpdateFrom(startedRunInfo)

		// Both keys in the runs map must be unique; if the startup failed and the run ID is not available,
		// keep the placeholder run ID derived from Executable name.
		effectiveRunID := startingRunID
		if startedRunInfo.RunID != UnknownRunID {
			effectiveRunID = startedRunInfo.RunID
		}
		_ = r.runs.UpdateChangingStateKey(exeName, startingRunID, effectiveRunID, ri)
		if startedRunInfo.ExeState == apiv1.ExecutableStateRunning {
			startWaitForRunCompletion()
		}
	})

	r.debouncer.ReconciliationNeeded(r.lifetimeCtx, exeName, struct{}{}, r.scheduleExecutableReconciliation)
}

// Stops the underlying Executable run, if any.
// The Executable data update related to stopped run is handled by the caller.
// The passed runInfo is a copy that the method can modify
func (r *ExecutableReconciler) stopExecutableFunc(exe *apiv1.Executable, runInfo *ExecutableRunInfo, log logr.Logger) func(context.Context) {
	return func(stopCtx context.Context) {
		runner, runnerNotFoundErr := r.getExecutableRunner(exe)
		if runnerNotFoundErr != nil {
			// Should never happen
			log.Error(runnerNotFoundErr, "the Executable cannot be stopped")
			return
		}

		stopErr := runner.StopRun(stopCtx, runInfo.RunID, log)

		// If the stop fails, we are not sure if the Executable is still running or not,
		// so we queue the transition to the unknown state. But if the stop succeeds,
		// OnRunCompleted() will be called and the transition to the final state will be handled there.

		if stopErr != nil {
			log.Error(stopErr, "could not stop the Executable", "RunID", runInfo.RunID)
			runInfo.ExeState = apiv1.ExecutableStateUnknown
			exeName := exe.NamespacedName()

			runID := runInfo.RunID
			r.runs.QueueDeferredOp(exeName, func(types.NamespacedName, RunID) {
				// The run may have been deleted by the time we get here, so we do not care if Update() returns false.
				_ = r.runs.Update(exeName, runID, runInfo)
			})

			r.debouncer.ReconciliationNeeded(r.lifetimeCtx, exe.NamespacedName(), struct{}{}, r.scheduleExecutableReconciliation)
		}
	}
}

func (r *ExecutableReconciler) getExecutableRunner(exe *apiv1.Executable) (ExecutableRunner, error) {
	executionType := exe.Spec.ExecutionType
	if executionType == "" {
		executionType = apiv1.ExecutionTypeProcess
	}

	runner, found := r.ExecutableRunners[executionType]
	if !found {
		return nil, fmt.Errorf("no runner found for execution type '%s'", executionType)
	}

	return runner, nil
}

func (r *ExecutableReconciler) releaseExecutableResources(ctx context.Context, exe *apiv1.Executable, log logr.Logger) {
	if len(exe.Spec.HealthProbes) > 0 {
		log.V(1).Info("disabling health probes for (deleted) Executable...")
		r.hpSet.DisableProbes(apiv1.GetNamespacedNameWithKind(exe))
	}

	removeEndpointsForWorkload(r, ctx, exe, log)
	r.deleteOutputFiles(ctx, exe, log)
}

func (r *ExecutableReconciler) deleteOutputFiles(ctx context.Context, exe *apiv1.Executable, log logr.Logger) {
	// Do not bother updating the Executable object--this method is called when the object is being deleted.

	if osutil.EnvVarSwitchEnabled(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS) {
		return
	}

	if exe.Status.StdOutFile != "" {
		if err := logs.RemoveWithRetry(ctx, exe.Status.StdOutFile); err != nil {
			log.Error(err, "could not remove process's standard output file", "path", exe.Status.StdOutFile)
		}
	}

	if exe.Status.StdErrFile != "" {
		if err := logs.RemoveWithRetry(ctx, exe.Status.StdErrFile); err != nil {
			log.Error(err, "could not remove process's standard error file", "path", exe.Status.StdErrFile)
		}
	}
}

func (r *ExecutableReconciler) scheduleExecutableReconciliation(rti reconcileTriggerInput[struct{}]) {
	event := ctrl_event.GenericEvent{
		Object: &apiv1.Executable{
			ObjectMeta: metav1.ObjectMeta{
				Name:      rti.target.Name,
				Namespace: rti.target.Namespace,
			},
		},
	}
	r.notifyRunChanged.In <- event
}

func (r *ExecutableReconciler) createEndpoint(
	ctx context.Context,
	owner ctrl_client.Object,
	serviceProducer ServiceProducer,
	log logr.Logger,
) (*apiv1.Endpoint, error) {
	endpointName, _, err := MakeUniqueName(owner.GetName())
	if err != nil {
		log.Error(err, "could not generate unique name for Endpoint object")
		return nil, err
	}

	if !networking.IsValidPort(int(serviceProducer.Port)) {
		return nil, fmt.Errorf("%s: missing information about the port to expose the service", serviceProducerIsInvalid)
	}

	address := serviceProducer.Address
	switch address {
	case "":
		address = networking.Localhost
	case networking.IPv4AllInterfaceAddress:
		// The client/proxy cannot really reach the Executable through this address (it is "use all available interfaces" address),
		// but we can choose whatever available address we want, IPv4 localhost in particular.
		address = networking.IPv4LocalhostDefaultAddress
	case networking.IPv6AllInterfaceAddress:
		// Similar to the IPv4 case, we choose the IPv6 localhost address.
		address = networking.IPv6LocalhostDefaultAddress
	}

	// Otherwise, create a new Endpoint object.
	endpoint := &apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      endpointName,
			Namespace: owner.GetNamespace(),
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: owner.GetNamespace(),
			ServiceName:      serviceProducer.ServiceName,
			Address:          address,
			Port:             serviceProducer.Port,
		},
	}

	return endpoint, nil
}

// Environment variables starting with these prefixes will never be applied to Executables.
var suppressVarPrefixes = []string{
	"DEBUG_SESSION",
	"DCP_",
}

// Computes the effective set of environment variables for the Executable run and stores it in Status.EffectiveEnv.
func (r *ExecutableReconciler) computeEffectiveEnvironment(
	ctx context.Context,
	exe *apiv1.Executable,
	reservedServicePorts map[types.NamespacedName]int32,
	log logr.Logger,
) error {
	// Start with ambient environment.
	var envMap maps.StringKeyMap[string]
	if osutil.IsWindows() {
		envMap = maps.NewStringKeyMap[string](maps.StringMapModeCaseInsensitive)
	} else {
		envMap = maps.NewStringKeyMap[string](maps.StringMapModeCaseSensitive)
	}

	if exe.Spec.AmbientEnvironment.Behavior == "" || exe.Spec.AmbientEnvironment.Behavior == apiv1.EnvironmentBehaviorInherit {
		envMap.Apply(maps.SliceToMap(os.Environ(), func(envStr string) (string, string) {
			parts := strings.SplitN(envStr, "=", 2)
			return parts[0], parts[1]
		}))
	} else if exe.Spec.AmbientEnvironment.Behavior == apiv1.EnvironmentBehaviorDoNotInherit {
		// Noop
	} else {
		return fmt.Errorf("unknown environment behavior: %s", exe.Spec.AmbientEnvironment.Behavior)
	}

	// Add environment variables from .env files.
	if len(exe.Spec.EnvFiles) > 0 {
		if additionalEnv, err := godotenv.Read(exe.Spec.EnvFiles...); err != nil {
			log.Error(err, "Environment settings from .env file(s) were not applied.", "EnvFiles", exe.Spec.EnvFiles)
		} else {
			envMap.Apply(additionalEnv)
		}
	}

	// Add environment variables from the Spec.
	for _, envVar := range exe.Spec.Env {
		envMap.Override(envVar.Name, envVar.Value)
	}

	// Apply variable substitutions.

	tmpl, err := newSpecValueTemplate(ctx, r, exe, reservedServicePorts, log)
	if err != nil {
		return err
	}

	for key, value := range envMap.Data() {
		substitutionCtx := fmt.Sprintf("environment variable %s", key)
		effectiveValue, templateErr := executeTemplate(tmpl, exe, value, substitutionCtx, log)
		if templateErr != nil {
			return templateErr
		}
		envMap.Set(key, effectiveValue)
	}

	for _, prefix := range suppressVarPrefixes {
		envMap.DeletePrefix(prefix)
	}

	exe.Status.EffectiveEnv = maps.MapToSlice[apiv1.EnvVar](envMap.Data(), func(key string, value string) apiv1.EnvVar {
		return apiv1.EnvVar{Name: key, Value: value}
	})

	return nil
}

func (r *ExecutableReconciler) computeEffectiveInvocationArgs(
	ctx context.Context,
	exe *apiv1.Executable,
	reservedServicePorts map[types.NamespacedName]int32,
	log logr.Logger,
) error {
	tmpl, err := newSpecValueTemplate(ctx, r, exe, reservedServicePorts, log)
	if err != nil {
		return err
	}

	effectiveArgs := make([]string, len(exe.Spec.Args))
	for i, arg := range exe.Spec.Args {
		substitutionCtx := fmt.Sprintf("argument %s", arg)
		effectiveArg, templateErr := executeTemplate(tmpl, exe, arg, substitutionCtx, log)
		if templateErr != nil {
			return templateErr
		}
		effectiveArgs[i] = effectiveArg
	}

	exe.Status.EffectiveArgs = effectiveArgs
	return nil
}

func (r *ExecutableReconciler) handleHealthProbeResults(lifetimeCtx context.Context) {
	for {
		select {
		case <-lifetimeCtx.Done():
			return

		case report, isOpen := <-r.healthProbeCh.Out:
			if !isOpen {
				return
			}

			if report.Owner.Kind != executableKind {
				r.Log.Error(fmt.Errorf("Executable reconciler received health probe report for some other type of object"), "", "Kind", report.Owner.Kind)
				continue
			}

			exeName := report.Owner.NamespacedName
			runID, runInfo := r.runs.BorrowByNamespacedName(exeName)
			if runInfo == nil {
				// Not tracking this Executable anymore, most likely Executable was deleted and we got a stale report.
				// We disable probes when the Executable reaches final state AND just before removing the finalizer,
				// so it is very unlikely any Executable health probes will go orphaned long-term.
				// It might look like it would be a good idea to call DisableProbes() here "just in case"
				// (DisableProbes() is idempotent), but it is not, for two reasons:
				// 1. handleHealthProbeResults() runs asynchronously compared to the main reconciliation loop,
				//    and calling DisableProbes() here might put us in a race condition with an Executable
				//    that is deleted and quickly re-created, a common .NET Aspire scenario.
				// 2. We cannot use deferred ops to avoid the race because we do not have a valid runInfo at this point.

				continue
			}

			runInfo.healthProbeResults[report.Probe.Name] = report.Result

			r.runs.QueueDeferredOp(exeName, func(types.NamespacedName, RunID) {
				// The run may have been deleted by the time we get here, so we do not care if Update() returns false.
				_ = r.runs.Update(exeName, runID, runInfo)
			})

			r.debouncer.ReconciliationNeeded(r.lifetimeCtx, exeName, struct{}{}, r.scheduleExecutableReconciliation)
		}
	}
}

func getStartingRunID(exeName types.NamespacedName) RunID {
	return RunID("__starting-" + exeName.String())
}

func (r *ExecutableReconciler) setExecutableState(exe *apiv1.Executable, state apiv1.ExecutableState) objectChange {
	change := noChange

	if exe.Status.State != state {
		exe.Status.State = state
		change = statusChanged

		finalState := state == apiv1.ExecutableStateFinished || state == apiv1.ExecutableStateTerminated || state == apiv1.ExecutableStateFailedToStart
		if finalState && exe.Status.FinishTimestamp.IsZero() {
			exe.Status.FinishTimestamp = metav1.NowMicro()
		}
	}

	change |= updateExecutableHealthStatus(exe, state, r.Log)

	return change
}

func updateExecutableHealthStatus(exe *apiv1.Executable, state apiv1.ExecutableState, log logr.Logger) objectChange {
	var newHealthStatus apiv1.HealthStatus

	switch state {
	case apiv1.ExecutableStateEmpty, apiv1.ExecutableStateStarting, apiv1.ExecutableStateStopping, apiv1.ExecutableStateTerminated, apiv1.ExecutableStateUnknown:
		newHealthStatus = apiv1.HealthStatusCaution

	case apiv1.ExecutableStateRunning:
		if len(exe.Spec.HealthProbes) == 0 {
			newHealthStatus = apiv1.HealthStatusHealthy
		} else {
			newHealthStatus = health.HealthStatusFromProbeResults(exe.Status.HealthProbeResults)
		}

	case apiv1.ExecutableStateFailedToStart:
		newHealthStatus = apiv1.HealthStatusUnhealthy

	case apiv1.ExecutableStateFinished:
		if exe.Status.ExitCode == apiv1.UnknownExitCode || *exe.Status.ExitCode == 0 {
			newHealthStatus = apiv1.HealthStatusCaution
		} else {
			newHealthStatus = apiv1.HealthStatusUnhealthy
		}

	default:
		// This should never happen and would indicate we failed to account for some Executable state.
		// Report the status as unhealthy, but do not panic. This should be pretty visible for clients and cause a bug report.
		newHealthStatus = apiv1.HealthStatusUnhealthy
	}

	if exe.Status.HealthStatus != newHealthStatus {
		log.V(1).Info("Health status changed", "Executable", exe.NamespacedName().String(), "NewHealthStatus", newHealthStatus)
		exe.Status.HealthStatus = newHealthStatus
		return statusChanged
	} else {
		return noChange
	}
}

var _ RunChangeHandler = (*ExecutableReconciler)(nil)
