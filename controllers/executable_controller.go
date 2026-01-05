// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/go-logr/logr"
	"github.com/joho/godotenv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/health"
	"github.com/microsoft/usvc-apiserver/internal/logs"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/templating"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
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

type runStateMap = ObjectStateMap[RunID, ExecutableRunInfo, *ExecutableRunInfo, *apiv1.Executable]

// ExecutableReconciler reconciles a Executable object
type ExecutableReconciler struct {
	*ReconcilerBase[apiv1.Executable, *apiv1.Executable]

	ExecutableRunners map[apiv1.ExecutionType]ExecutableRunner

	// A map that stores information about running Executables,
	// searchable by Executable name (first key), or run ID (second key).
	runs *runStateMap

	// Health probe set used to execute health probes.
	hpSet *health.HealthProbeSet

	// Channel used to receive health probe results.
	healthProbeCh *concurrency.UnboundedChan[health.HealthProbeReport]

	// A WorkQueue for operations related to stopping Executables (which might take a while).
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
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	executableRunners map[apiv1.ExecutionType]ExecutableRunner,
	healthProbeSet *health.HealthProbeSet,
) *ExecutableReconciler {
	base := NewReconcilerBase[apiv1.Executable](client, noCacheClient, log, lifetimeCtx)

	r := ExecutableReconciler{
		ReconcilerBase:    base,
		ExecutableRunners: executableRunners,
		runs:              NewObjectStateMap[RunID, ExecutableRunInfo, *ExecutableRunInfo, *apiv1.Executable](),
		hpSet:             healthProbeSet,
		healthProbeCh:     concurrency.NewUnboundedChan[health.HealthProbeReport](lifetimeCtx),
		stopQueue:         resiliency.NewWorkQueue(lifetimeCtx, maxParallelExecutableStops),
	}

	go r.handleHealthProbeResults()
	_, subscriptionErr := r.hpSet.Subscribe(r.healthProbeCh.In, executableKind)
	if subscriptionErr != nil {
		// Should never happen
		log.Error(subscriptionErr, "Could not subscribe to health probe results, the health of Executables will never be correctly reported")
	}

	return &r
}

func (r *ExecutableReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv1.Executable{}).
		Owns(&apiv1.Endpoint{}).
		WatchesRawSource(r.GetReconciliationEventSource()).
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
	reader, log := r.StartReconciliation(req)

	// Check to see if the request context has already expired
	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	// Retrieve the Executable object
	exe := apiv1.Executable{}
	if err := reader.Get(ctx, req.NamespacedName, &exe); err != nil {
		if apierrors.IsNotFound(err) {
			// Ensure the cache of Executable to run ID is cleared
			r.runs.DeleteByNamespacedName(req.NamespacedName)
			log.V(1).Info("Executable not found, nothing to do...")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "Failed to Get() the Executable", "Exe", exe)
			getFailedCounter.Add(ctx, 1)
			return ctrl.Result{}, err
		}
	} else {
		getSucceededCounter.Add(ctx, 1)
	}

	// Route logs to the resource sink
	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, exe.GetResourceId())

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(exe.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	r.runs.RunDeferredOps(req.NamespacedName, &exe)

	if exe.DeletionTimestamp != nil && !exe.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, &exe, log)
	} else if change = ensureFinalizer(&exe, executableFinalizer, log); change != noChange {
		// If we added a finalizer, we'll do the additional reconciliation next call
	} else {
		change = r.manageExecutable(ctx, &exe, log)
	}

	result, err := r.SaveChanges(ctx, &exe, patch, change, nil, log)
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
		log.V(1).Info("Executable is being deleted (deleting finalizer only)...")
		change = deleteFinalizer(exe, executableFinalizer, log)

	case runInfo.ExeState == apiv1.ExecutableStateStopping || runInfo.ExeState == apiv1.ExecutableStateStarting:
		log.V(1).Info("Executable is being deleted, waiting for it to exit transient state...",
			"CurrentState", runInfo.ExeState)
		change = r.manageExecutable(ctx, exe, log)

	case runInfo.ExeState == apiv1.ExecutableStateRunning:
		log.V(1).Info("Executable is being deleted, but it needs to be stopped first...")
		runInfo.ExeState = apiv1.ExecutableStateStopping
		stoppingInitializer := getStateInitializer(executableStateInitializers, apiv1.ExecutableStateStopping, log)
		change = stoppingInitializer(ctx, r, exe, apiv1.ExecutableStateStopping, runInfo, log)
		r.runs.Update(exe.NamespacedName(), runInfo.RunID, runInfo)

	default:
		log.V(1).Info("Executable is being deleted (in initial or final state; releasing resources and deleting finalizer)...",
			"CurrentState", runInfo.ExeState)
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

	return r.startExecutable(ctx, exe, runInfo, log)
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
	r.enableEndpointsAndHealthProbes(ctx, exe, runInfo, log)
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
		log.V(1).Info("Attempting to stop the Executable...",
			"RunID", runInfo.RunID)
		runInfo.stopAttemptInitiated = true
		runInfo.ExeState = apiv1.ExecutableStateStopping
		runInfoCopy := runInfo.Clone()
		err := r.stopQueue.Enqueue(r.stopExecutableFunc(exe, runInfoCopy, log))
		if err != nil {
			log.Error(err, "Could not enqueue Executable stop operation")
			change |= ensureExecutableFinalState(ctx, r, exe, apiv1.ExecutableStateUnknown, runInfo, log)
			return change
		}
	}

	change |= runInfo.ApplyTo(exe, log)
	r.disableEndpointsAndHealthProbes(ctx, exe, runInfo, log)
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

	change |= runInfo.ApplyTo(exe, log) // Ensure the status matches the current state.
	r.disableEndpointsAndHealthProbes(ctx, exe, runInfo, log)
	return change
}

// Performs an attempt to start an Executable run.
// Main runner is tried first (Spec.ExecutionType), and if that fails and fallback runners are allowed (Spec.FalbackExecutionTypes),
// they are in the order specified, one at a time.
// This function is called from the reconciliation loop.
func (r *ExecutableReconciler) startExecutable(
	ctx context.Context,
	exe *apiv1.Executable,
	ri *ExecutableRunInfo,
	log logr.Logger,
) objectChange {
	if ri == nil {
		log.V(1).Info("Starting Executable...")
		ri = NewRunInfo(exe)
		ri.ExeState = apiv1.ExecutableStateStarting
		ri.RunID = getStartingRunID(exe.NamespacedName())
		r.runs.Store(exe.NamespacedName(), ri.RunID, ri.Clone())
	}

	if ri.startupStage == StartupStageInitial {
		if exe.Spec.PemCertificates != nil {
			prepareCertErr := r.prepareCertificateFiles(exe, log)
			if prepareCertErr != nil {
				// Error already logged in prepareCertificateFiles()
				r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
				return statusChanged
			}
		}
		ri.startupStage = StartupStageCertificateDataReady
		r.runs.Update(exe.NamespacedName(), ri.RunID, ri.Clone())
	}

	if ri.startupStage == StartupStageCertificateDataReady {
		oc := r.computeExecutableEnvironment(ctx, exe, log, ri)
		if oc != noChange {
			// Environment is not ready, or an error occurred; report it and let the next reconciliation try again.
			return oc
		}
	}

	if ri.startupStage >= StartupStageDefaultRunner && len(ri.startResults) == 0 { // Should never happen
		log.Error(errors.New("inconsistent Executable startup state"), "No startup results found despite startup stage indicating that a startup attempt was made")
		r.setExecutableState(exe, apiv1.ExecutableStateUnknown)
		return statusChanged
	}

	var startResult *ExecutableStartResult = nil
	if len(ri.startResults) > 0 {
		startResult = ri.startResults[len(ri.startResults)-1]
	}

	if ri.startupStage >= StartupStageDefaultRunner && startResult != nil && startResult.ExeState == apiv1.ExecutableStateStarting {
		// Run attempt in progress, just make sure the Executable status is up to date.
		// When the attempt finishes, OnStartupCompleted() will schedule another reconciliation.
		return ri.ApplyTo(exe, log)
	}

	// Helper function to update Executable status when the start attempt was successful.
	handleSuccessfulStart := func(res *ExecutableStartResult) {
		startingRunID := ri.RunID // Might be the temporary starting run ID. We need that to update the runs map.
		res.applyTo(ri)
		ri.ApplyTo(exe, log)
		r.setExecutableState(exe, ri.ExeState)
		r.runs.UpdateChangingStateKey(exe.NamespacedName(), startingRunID, res.RunID, ri.Clone())

		if res.ExeState == apiv1.ExecutableStateRunning && res.StartWaitForRunCompletion != nil {
			res.StartWaitForRunCompletion()
		}
	}

	var unknownStartupFailureErr error = errors.New("the reason for the Executable startup failure is unknown")

	// We got here either because this is the first attempt to start the Executable,
	// or because the previous, asynchronous attempt has completed (successfully or not).
	// So first we need to check the result of the previous startup attempt (if any).
	if startResult.IsSuccessfullyCompleted() {
		handleSuccessfulStart(startResult)
		return statusChanged
	} else if startResult != nil {
		runErr := startResult.StartupError
		if runErr != nil {
			runErr = unknownStartupFailureErr
		}
		log.Error(runErr, "An attempt to start the Executable failed")
		r.cleanUpFailedStartResources(ctx, exe, log)
	}

	// Try to start the Executable using available runner(s)
	for {
		if allRunnersAttempted(ri.startupStage, exe) {
			log.Error(errors.New("all available Executable runners have been tried and failed"), "The Executable failed to start")
			ri.ApplyTo(exe, log)
			exe.Status.ExecutionID = "" // Clear the starting execution ID
			r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
			if startResult != nil {
				exe.Status.StartupTimestamp = startResult.CompletionTimestamp
			} else {
				exe.Status.StartupTimestamp = metav1.NowMicro()
			}
			exe.Status.FinishTimestamp = exe.Status.StartupTimestamp
			r.runs.DeleteByNamespacedName(exe.NamespacedName())
			return statusChanged
		}

		ri.startupStage++
		ri.startResults = append(ri.startResults, NewExecutableStartResult())
		r.runs.Update(exe.NamespacedName(), ri.RunID, ri.Clone())

		runner, runnerNotFoundErr := r.getExecutableRunner(exe, ri.startupStage)
		if runnerNotFoundErr != nil {
			log.Error(runnerNotFoundErr, "The Executable runner is not available", "StartupStage", ri.startupStage)
			continue
		}

		startResult = runner.StartRun(ctx, exe, r, log)
		if startResult == nil {
			// Should never happen
			log.Error(errors.New("the Executable runner returned nil result from StartRun() method"), "An attempt to start the Executable failed")
			r.cleanUpFailedStartResources(ctx, exe, log)
			continue
		}
		if startResult.ExeState == apiv1.ExecutableStateFailedToStart {
			runErr := startResult.StartupError
			if runErr == nil {
				runErr = unknownStartupFailureErr
			}
			log.Error(runErr, "An attempt to start the Executable failed")
			r.cleanUpFailedStartResources(ctx, exe, log)
			continue
		}

		if startResult.IsSuccessfullyCompleted() {
			handleSuccessfulStart(startResult)
			return statusChanged
		}

		// Asynchronous start in progress, wait for OnStartupCompleted() to be called
		return additionalReconciliationNeeded
	}
}

func (r *ExecutableReconciler) OnMainProcessChanged(runID RunID, pid process.Pid_t) {
	r.processRunChangeNotification("Run process changed", runID, pid, apiv1.UnknownExitCode, nil, func(ri *ExecutableRunInfo) {
		ri.ExeState = apiv1.ExecutableStateRunning
	})
}

func (r *ExecutableReconciler) OnRunCompleted(runID RunID, exitCode *int32, err error) {
	r.processRunChangeNotification("Run completed", runID, process.UnknownPID, exitCode, err, func(ri *ExecutableRunInfo) {
		ri.ExeState = apiv1.ExecutableStateFinished
		ri.FinishTimestamp = metav1.NowMicro()
	})
}

// Handle notification about main process change or run completion for an Executable run.
// This function runs outside of the reconciliation loop, so we just memorize the PID and process exit code
// (if available) in the run status map, and not attempt to modify any Executable object data.
func (r *ExecutableReconciler) processRunChangeNotification(
	runChangeDescription string,
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

	log := r.Log.WithValues(
		logger.RESOURCE_LOG_STREAM_ID, runInfo.GetResourceId(),
		"Executable", name.String(),
		"RunID", runID,
		"LastState", runInfo.ExeState,
		"RunChange", runChangeDescription,
	)

	var effectiveExitCode *int32
	if err != nil {
		log.Info("Executable run could not be tracked",
			"Error", err.Error(),
		)
		effectiveExitCode = apiv1.UnknownExitCode
	} else {
		log.V(1).Info("Queue Executable run change",
			"PID", pid,
			"ExitCode", exitCode,
			"ExecutableState", runInfo.ExeState,
		)
		effectiveExitCode = exitCode
	}

	// Memorize PID and (if applicable) exit code
	runInfo.ExitCode = effectiveExitCode
	if pid > 0 {
		pointers.SetValue(&runInfo.Pid, int64(pid))
	}

	runMap := r.runs
	r.runs.QueueDeferredOp(name, func(types.NamespacedName, RunID, *apiv1.Executable) {
		// The run may have been deleted by the time we get here, so we do not care if Update() returns false.
		_ = runMap.Update(name, runID, runInfo)
	})

	r.ScheduleReconciliation(name)
}

// Handle setting up process tracking once an Executable has transitioned from newly created or starting to a stable state such as running or finished.
func (r *ExecutableReconciler) OnStartupCompleted(
	exeName types.NamespacedName,
	startResult *ExecutableStartResult,
) {
	// OnStartingCompleted might be invoked asynchronously. To avoid race conditions,
	// we always queue updates to the runs map and run them as part of reconciliation function.

	if startResult == nil {
		// Should never happen
		r.Log.Error(
			errors.New("nil ExecutableStartResult received in OnStartupCompleted"),
			"Executable start result could not be processed",
			"Executable", exeName,
		)
		return
	}

	runMap := r.runs
	r.runs.QueueDeferredOp(exeName, func(_ types.NamespacedName, _ RunID, exe *apiv1.Executable) {
		log := r.Log.WithValues(
			logger.RESOURCE_LOG_STREAM_ID, exe.GetResourceId(),
			"Executable", exe.NamespacedName(),
			"Outcome", startResult.ExeState,
		)
		_, ri := r.runs.BorrowByNamespacedName(exeName)
		if ri == nil {
			// Should never happen
			log.Error(
				errors.New("could not find starting run data after Executable start attempt"),
				"No valid Executable run info could be found",
			)
			return
		}

		if ri.ExeState != apiv1.ExecutableStateStarting {
			// Startup attempt completed synchronously, this will be handled by the state initializer function
			// that is part of the reconciliation loop (startExecutable). Nothing to do here.
			return
		}

		log.V(1).Info("Executable completed startup attempt", "RunID", startResult.RunID)

		// Just store the result and let the reconciliation loop handle the rest.
		previousRunID := ri.RunID

		if len(ri.startResults) == 0 {
			// Should never happen
			log.Error(
				errors.New("inconsistent Executable startup state"),
				"No startup results found despite startup stage indicating that a startup attempt was made",
			)
			ri.ExeState = apiv1.ExecutableStateUnknown
		} else {
			current := ri.startResults[len(ri.startResults)-1]
			_ = current.UpdateFrom(startResult)
		}

		_ = runMap.UpdateChangingStateKey(exeName, previousRunID, ri.RunID, ri)

	})

	r.ScheduleReconciliation(exeName)
}

// Handles run message notifications from the Executable runner.
func (r *ExecutableReconciler) OnRunMessage(runID RunID, level RunMessageLevel, message string) {
	name, runInfo := r.runs.BorrowByStateKey(runID)
	if runInfo == nil {
		// Not tracking this run anymore (or it is not our run at all).
		return
	}

	log := r.Log.WithValues(
		logger.RESOURCE_LOG_STREAM_ID, runInfo.GetResourceId(),
		"Executable", name.String(),
		"RunID", runID,
	)

	switch level {

	case RunMessageLevelInfo:
		log.Info(message)

	case RunMessageLevelDebug:
		log.V(1).Info(message)

	case RunMessageLevelError:
		log.Error(fmt.Errorf("Executable run encountered an error"), message)

	default:
		// Should never happen
		log.V(1).Info("Executable runner emitted a message with unrecognized level", "Level", level, "Message", message)
	}
}

// Stops the underlying Executable run, if any.
// The Executable data update related to stopped run is handled by the caller.
// The passed runInfo is a copy that the method can modify
func (r *ExecutableReconciler) stopExecutableFunc(exe *apiv1.Executable, runInfo *ExecutableRunInfo, log logr.Logger) func(context.Context) {
	return func(stopCtx context.Context) {
		runner, runnerNotFoundErr := r.getExecutableRunner(exe, runInfo.startupStage)
		if runnerNotFoundErr != nil {
			// Should never happen
			log.Error(runnerNotFoundErr, "The Executable cannot be stopped")
			return
		}

		stopErr := runner.StopRun(stopCtx, runInfo.RunID, log)

		// If the stop fails, we are not sure if the Executable is still running or not,
		// so we queue the transition to the unknown state. But if the stop succeeds,
		// OnRunCompleted() will be called and the transition to the final state will be handled there.

		if stopErr != nil {
			log.Error(stopErr, "Could not stop the Executable",
				"RunID", runInfo.RunID)
			runInfo.ExeState = apiv1.ExecutableStateUnknown
			exeName := exe.NamespacedName()

			runID := runInfo.RunID
			runMap := r.runs
			r.runs.QueueDeferredOp(exeName, func(types.NamespacedName, RunID, *apiv1.Executable) {
				// The run may have been deleted by the time we get here, so we do not care if Update() returns false.
				_ = runMap.Update(exeName, runID, runInfo)
			})

			r.ScheduleReconciliation(exeName)
		}
	}
}

func (r *ExecutableReconciler) getExecutableRunner(exe *apiv1.Executable, startupStage ExecutableStartuptStage) (ExecutableRunner, error) {
	var executionType apiv1.ExecutionType

	if startupStage <= StartupStageDefaultRunner {
		// Note, the runner might be necessary for stopping the run even if we haven't reached the default runner stage yet.
		executionType = exe.Spec.ExecutionType
		if executionType == "" {
			executionType = apiv1.ExecutionTypeProcess
		}
	} else {
		fallbackIndex := int(startupStage - 1)
		if len(exe.Spec.FallbackExecutionTypes) <= fallbackIndex {
			return nil, errors.New("startup progressed beyond available fallback execution types")
		}
		executionType = exe.Spec.FallbackExecutionTypes[fallbackIndex]
	}

	runner, found := r.ExecutableRunners[executionType]
	if !found {
		return nil, fmt.Errorf("no runner found for execution type '%s'", executionType)
	}

	return runner, nil
}

func allRunnersAttempted(currentStage ExecutableStartuptStage, exe *apiv1.Executable) bool {
	return int(currentStage) == len(exe.Spec.FallbackExecutionTypes)
}

func (r *ExecutableReconciler) releaseExecutableResources(ctx context.Context, exe *apiv1.Executable, log logr.Logger) {
	r.disableEndpointsAndHealthProbes(ctx, exe, nil, log)
	r.deleteOutputFiles(exe, log)
	r.deleteCertificateFiles(exe, log)
	logger.ReleaseResourceLog(exe.GetResourceId())
}

func (r *ExecutableReconciler) cleanUpFailedStartResources(ctx context.Context, exe *apiv1.Executable, log logr.Logger) {
	r.disableEndpointsAndHealthProbes(ctx, exe, nil, log)
	r.deleteOutputFiles(exe, log)
}

func (r *ExecutableReconciler) enableEndpointsAndHealthProbes(
	ctx context.Context,
	exe *apiv1.Executable,
	runInfo *ExecutableRunInfo,
	log logr.Logger,
) {
	ensureEndpointsForWorkload(ctx, r, exe, runInfo.ReservedPorts, struct{}{}, log)

	if len(exe.Spec.HealthProbes) > 0 && pointers.NotTrue(runInfo.healthProbesEnabled) {
		log.V(1).Info("Enabling health probes for Executable...")

		// healthProbesEnabled is used to avoid enabling health probes multiple times for the same Executable
		// Enablement only fails if the lifetime context is cancelled, or under "should never happen" conditions,
		// so we set it unconditionally to avoid repeated attempts.
		pointers.Make(&runInfo.healthProbesEnabled, true)

		probeErr := r.hpSet.EnableProbes(exe, exe.Spec.HealthProbes)
		if probeErr != nil {
			log.Error(probeErr, "Could not enable health probes for Executable")
		}
	}
}

func (r *ExecutableReconciler) disableEndpointsAndHealthProbes(
	ctx context.Context,
	exe *apiv1.Executable,
	runInfo *ExecutableRunInfo,
	log logr.Logger,
) {
	if len(exe.Spec.HealthProbes) > 0 && (runInfo == nil || pointers.TrueValue(runInfo.healthProbesEnabled)) {
		log.V(1).Info("Disabling health probes for Executable...")
		r.hpSet.DisableProbes(exe)
		if runInfo != nil {
			pointers.Make(&runInfo.healthProbesEnabled, false)
		}
	}

	removeEndpointsForWorkload(ctx, r, exe, log)
}

func (r *ExecutableReconciler) deleteOutputFiles(exe *apiv1.Executable, log logr.Logger) {
	// Do not bother updating the Executable object--this method is called when the object is being deleted.

	if osutil.EnvVarSwitchEnabled(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS) {
		return
	}

	if exe.Status.StdOutFile != "" {
		path := exe.Status.StdOutFile
		_ = r.stopQueue.Enqueue(func(opCtx context.Context) { // Only errors if lifetimeCtx is done
			if err := logs.RemoveWithRetry(opCtx, path); err != nil {
				log.Error(err, "Could not remove process's standard output file",
					"Path", path)
			}
		})
	}

	if exe.Status.StdErrFile != "" {
		path := exe.Status.StdErrFile
		_ = r.stopQueue.Enqueue(func(opCtx context.Context) {
			if err := logs.RemoveWithRetry(opCtx, path); err != nil {
				log.Error(err, "Could not remove process's standard error file",
					"Path", path)
			}
		})
	}
}

func (r *ExecutableReconciler) deleteCertificateFiles(exe *apiv1.Executable, log logr.Logger) {
	// Do not bother updating the Executable object--this method is called when the object is being deleted.

	if osutil.EnvVarSwitchEnabled(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS) {
		return
	}

	if exe.Spec.PemCertificates != nil {
		certsPath := filepath.Join(usvc_io.DcpTempDir(), exe.Name)
		removeErr := os.RemoveAll(certsPath)
		if removeErr != nil {
			log.Error(removeErr, "Could not remove temporary folder for PEM certificates", "Path", certsPath)
		}
	}
}

func (r *ExecutableReconciler) createEndpoints(
	ctx context.Context,
	owner ctrl_client.Object,
	serviceProducer commonapi.ServiceProducer,
	existingEndpoints []*apiv1.Endpoint,
	_ struct{}, // Endpoint creation context data, not used for Executables
	log logr.Logger,
) ([]*apiv1.Endpoint, error) {
	endpointName, _, err := MakeUniqueName(owner.GetName())
	if err != nil {
		log.Error(err, "Could not generate unique name for Endpoint object")
		return nil, err
	}

	if !networking.IsValidPort(int(serviceProducer.Port)) {
		return nil, fmt.Errorf("information about the port to expose the service is missing; service-producer annotation is invalid")
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

	endpointExists := slices.Any(existingEndpoints, func(ep *apiv1.Endpoint) bool {
		return ep.Spec.ServiceName == serviceProducer.ServiceName &&
			ep.Spec.ServiceNamespace == owner.GetNamespace() &&
			ep.Spec.Address == address &&
			ep.Spec.Port == serviceProducer.Port
	})
	if endpointExists {
		return nil, nil
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

	return []*apiv1.Endpoint{endpoint}, nil
}

func (r *ExecutableReconciler) validateExistingEndpoints(
	ctx context.Context,
	owner ctrl_client.Object,
	serviceProducer commonapi.ServiceProducer,
	existing []*apiv1.Endpoint,
	_ struct{},
	log logr.Logger,
) ([]*apiv1.Endpoint, []*apiv1.Endpoint, error) {
	// Currently we do not support any scenario when Executable replicas change the ports they listen on
	// at run time, so once an Endpoint is created, it will always be "valid".
	return existing, nil, nil
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

	switch exe.Spec.AmbientEnvironment.Behavior {
	case "", apiv1.EnvironmentBehaviorInherit:
		envMap.Apply(maps.SliceToMap(os.Environ(), func(envStr string) (string, string) {
			parts := strings.SplitN(envStr, "=", 2)
			return parts[0], parts[1]
		}))
	case apiv1.EnvironmentBehaviorDoNotInherit:
		// Noop
	default:
		return fmt.Errorf("unknown environment behavior: %s", exe.Spec.AmbientEnvironment.Behavior)
	}

	// Add environment variables from .env files.
	if len(exe.Spec.EnvFiles) > 0 {
		if additionalEnv, err := godotenv.Read(exe.Spec.EnvFiles...); err != nil {
			log.Error(err, "Environment settings from .env file(s) were not applied.",
				"EnvFiles", exe.Spec.EnvFiles)
		} else {
			envMap.Apply(additionalEnv)
		}
	}

	// Add environment variables from the Spec.
	for _, envVar := range exe.Spec.Env {
		envMap.Override(envVar.Name, envVar.Value)
	}

	// Apply variable substitutions.

	tmpl, err := templating.NewSpecValueTemplate(ctx, r, exe, reservedServicePorts, log)
	if err != nil {
		return err
	}

	for key, value := range envMap.Data() {
		substitutionCtx := fmt.Sprintf("environment variable %s", key)
		effectiveValue, templateErr := templating.ExecuteTemplate(tmpl, exe, value, substitutionCtx, log)
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

// Computes effective invocation arguments for the Executable run and stores them in Status.EffectiveArgs.
func (r *ExecutableReconciler) computeEffectiveInvocationArgs(
	ctx context.Context,
	exe *apiv1.Executable,
	reservedServicePorts map[types.NamespacedName]int32,
	log logr.Logger,
) error {
	tmpl, err := templating.NewSpecValueTemplate(ctx, r, exe, reservedServicePorts, log)
	if err != nil {
		return err
	}

	effectiveArgs := make([]string, len(exe.Spec.Args))
	for i, arg := range exe.Spec.Args {
		substitutionCtx := fmt.Sprintf("argument %s", arg)
		effectiveArg, templateErr := templating.ExecuteTemplate(tmpl, exe, arg, substitutionCtx, log)
		if templateErr != nil {
			return templateErr
		}
		effectiveArgs[i] = effectiveArg
	}

	exe.Status.EffectiveArgs = effectiveArgs
	return nil
}

// Computes effective environment and invocation arguments for the Executable.
// Returned objectChange indicates whether the method was successful or not.
// noChange means environment was computed successfully and we can proceed with startup.
// Any other value means there was an error and the caller should return allow another reconciliation loop iteration.
// This method is called from the reconciliation loop.
func (r *ExecutableReconciler) computeExecutableEnvironment(ctx context.Context, exe *apiv1.Executable, log logr.Logger, ri *ExecutableRunInfo) objectChange {
	// Ports reserved for services that the Executable implements without specifying the desired port to use (via service-producer annotation).
	reservedServicePorts := make(map[types.NamespacedName]int32)

	markExecutableFailed := func() {
		ri.ExeState = apiv1.ExecutableStateFailedToStart
		r.runs.Update(exe.NamespacedName(), ri.RunID, ri.Clone())
		r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}

	err := r.computeEffectiveEnvironment(ctx, exe, reservedServicePorts, log)
	if templating.IsTransientTemplateError(err) {
		if exe.DeletionTimestamp != nil && !exe.DeletionTimestamp.IsZero() {
			log.Info("Executable is being deleted while waiting to compute effective environment, aborting startup",
				"Cause", err.Error())
			markExecutableFailed()
			return statusChanged
		} else {
			log.Info("Could not compute effective environment for the Executable, retrying startup...",
				"Cause", err.Error())
			return r.setExecutableState(exe, apiv1.ExecutableStateStarting) | additionalReconciliationNeeded
		}
	} else if err != nil {
		log.Error(err, "Could not compute effective environment for the Executable")
		markExecutableFailed()
		return statusChanged
	}

	err = r.computeEffectiveInvocationArgs(ctx, exe, reservedServicePorts, log)
	if templating.IsTransientTemplateError(err) {
		if exe.DeletionTimestamp != nil && !exe.DeletionTimestamp.IsZero() {
			log.Info("Executable is being deleted while waiting to compute effective invocation arguments, aborting startup",
				"Cause", err.Error())
			markExecutableFailed()
			return statusChanged
		} else {
			log.Info("Could not compute effective invocation arguments for the Executable, retrying startup...",
				"Cause", err.Error())
			return r.setExecutableState(exe, apiv1.ExecutableStateStarting) | additionalReconciliationNeeded
		}
	} else if err != nil {
		log.Error(err, "Could not compute effective invocation arguments for the Executable")
		markExecutableFailed()
		return statusChanged
	}

	if len(reservedServicePorts) > 0 {
		log.V(1).Info("Reserving service ports...",
			"Services", maps.Keys(reservedServicePorts),
			"Ports", maps.Values(reservedServicePorts),
		)
	}
	ri.ReservedPorts = reservedServicePorts
	ri.startupStage = StartupStageDataInitialized
	r.runs.Update(exe.NamespacedName(), ri.RunID, ri.Clone())
	return noChange
}

func (r *ExecutableReconciler) prepareCertificateFiles(
	exe *apiv1.Executable,
	log logr.Logger,
) error {
	certsTempFolder, certsTempFolderErr := usvc_io.CreateTempFolder(filepath.Join(exe.Name, "certs"), osutil.PermissionOnlyOwnerReadWriteTraverse)
	if certsTempFolderErr != nil {
		if !errors.Is(certsTempFolderErr, os.ErrExist) {
			if exe.Spec.PemCertificates.ContinueOnError {
				log.Info("Could not create temporary folder for processing PEM certificates, continuing...", "Error", certsTempFolderErr.Error())
			} else {
				log.Error(certsTempFolderErr, "Could not create temporary folder for processing PEM certificates")
				return certsTempFolderErr
			}
		}

		return nil
	}

	bundle := []string{}
	fingerprints := []string{}
	for _, cert := range exe.Spec.PemCertificates.Certificates {
		fingerprint, fingerprintErr := cert.OpenSSLFingerprint()
		if fingerprintErr != nil {
			if exe.Spec.PemCertificates.ContinueOnError {
				log.Info("Could not compute certificate fingerprint, skipping certificate", "Thumbprint", cert.Thumbprint, "Error", fingerprintErr.Error())
				continue
			}
			log.Error(fingerprintErr, "Could not compute certificate fingerprint", "Thumbprint", cert.Thumbprint)
			return fingerprintErr
		}

		certFilepath := filepath.Join(certsTempFolder, fmt.Sprintf("%s.pem", cert.Thumbprint))
		certWriteErr := usvc_io.WriteFile(certFilepath, []byte(cert.Contents), osutil.PermissionOnlyOwnerReadWrite)
		if certWriteErr != nil {
			if exe.Spec.PemCertificates.ContinueOnError {
				log.Info("Could not write certificate to temporary folder, skipping certificate...", "Thumbprint", cert.Thumbprint, "Error", certWriteErr.Error())
				continue
			}

			log.Error(certWriteErr, "Could not write certificate to temporary folder", "Thumbprint", cert.Thumbprint)
			return certWriteErr
		}

		collisions := 0
		for _, existingFingerprint := range fingerprints {
			if existingFingerprint == fingerprint {
				collisions++
			}
		}

		if runtime.GOOS == "windows" {
			certWriteErr = usvc_io.WriteFile(filepath.Join(certsTempFolder, fmt.Sprintf("%s.%d", fingerprint, collisions)), []byte(cert.Contents), osutil.PermissionOnlyOwnerReadWrite)
		} else {
			// Create an OpenSSL style symlink for the certificate
			certWriteErr = os.Symlink(certFilepath, filepath.Join(certsTempFolder, fmt.Sprintf("%s.%d", fingerprint, collisions)))
		}

		if certWriteErr != nil {
			if exe.Spec.PemCertificates.ContinueOnError {
				log.Info("Could not create fingerprint symlink for certificate, skipping certificate...", "Thumbprint", cert.Thumbprint, "Error", certWriteErr.Error())
				continue
			}

			log.Error(certWriteErr, "Could not create fingerprint symlink for certificate", "Thumbprint", cert.Thumbprint)
			return certWriteErr
		} else {
			fingerprints = append(fingerprints, fingerprint)
		}

		bundle = append(bundle, cert.Contents)
	}

	bundleWriteErr := usvc_io.WriteFile(filepath.Join(usvc_io.DcpTempDir(), exe.Name, "cert.pem"), []byte(strings.Join(bundle, "\n")), osutil.PermissionOnlyOwnerReadWrite)
	if bundleWriteErr != nil {
		if exe.Spec.PemCertificates.ContinueOnError {
			log.Info("Could not write certificate bundle to temporary folder, continuing...", "Error", bundleWriteErr.Error())
		} else {
			log.Error(bundleWriteErr, "Could not write certificate bundle to temporary folder")
			return bundleWriteErr
		}
	}

	return nil
}

func (r *ExecutableReconciler) handleHealthProbeResults() {
	for {
		select {
		case <-r.LifetimeCtx.Done():
			return

		case report, isOpen := <-r.healthProbeCh.Out:
			if !isOpen {
				return
			}

			if report.Owner.Kind != executableKind {
				r.Log.Error(
					fmt.Errorf("executable reconciler received health probe report for some other type of object"),
					"Invalid health probe report",
					"Kind", report.Owner.Kind)
				continue
			}

			exeName := report.Owner.NamespacedName
			runID, runInfo := r.runs.BorrowByNamespacedName(exeName)
			if runInfo == nil {
				// Not tracking this Executable anymore, most likely Executable was deleted and we got a stale report.
				// We disable probes when the Executable reaches final state AND just before removing the finalizer,
				// so it is very unlikely any Executable health probes will go orphaned long-term.
				// It might look like it would be a good idea to call DisableProbes(exeName) here "just in case"
				// (DisableProbes() is idempotent), but it is not, for two reasons:
				// 1. handleHealthProbeResults() runs asynchronously compared to the main reconciliation loop,
				//    and calling DisableProbes() here might put us in a race condition with an Executable
				//    that is deleted and quickly re-created, a common .NET Aspire scenario.
				// 2. We cannot use deferred ops to avoid the race because we do not have a valid runInfo at this point.

				continue
			}

			runInfo.healthProbeResults[report.Probe.Name] = report.Result

			runMap := r.runs
			r.runs.QueueDeferredOp(exeName, func(types.NamespacedName, RunID, *apiv1.Executable) {
				// The run may have been deleted by the time we get here, so we do not care if Update() returns false.
				_ = runMap.Update(exeName, runID, runInfo)
			})

			r.ScheduleReconciliation(exeName)
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

		if state.IsTerminal() && state != apiv1.ExecutableStateUnknown && exe.Status.FinishTimestamp.IsZero() {
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
		if len(exe.Spec.HealthProbes) == 0 && len(exe.Status.HealthProbeResults) == 0 {
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

	if exe.Status.HealthStatus == newHealthStatus {
		return noChange
	}

	log.V(1).Info("Executable health status changed",
		"NewHealthStatus", newHealthStatus,
		"OldHealthStatus", exe.Status.HealthStatus,
	)
	exe.Status.HealthStatus = newHealthStatus
	return statusChanged
}

var _ RunChangeHandler = (*ExecutableReconciler)(nil)
