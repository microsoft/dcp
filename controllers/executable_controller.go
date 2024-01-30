// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	stdlib_maps "maps"
	"os"
	"strings"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/joho/godotenv"
	"github.com/smallnest/chanx"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	ctrl_handler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

// ExecutableReconciler reconciles a Executable object
type ExecutableReconciler struct {
	ctrl_client.Client

	Log                 logr.Logger
	reconciliationSeqNo uint32
	ExecutableRunners   map[apiv1.ExecutionType]ExecutableRunner

	// A map that stores information about running Executables,
	// searchable by Executable name (first key), or run ID (second key).
	runs *maps.SynchronizedDualKeyMap[types.NamespacedName, RunID, *runInfo]

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
		runs:              maps.NewSynchronizedDualKeyMap[types.NamespacedName, RunID, *runInfo](),
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
		Owns(&apiv1.Endpoint{}).
		WatchesRawSource(&src, &ctrl_handler.EnqueueRequestForObject{}).
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
			r.runs.DeleteByFirstKey(req.NamespacedName)
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
	var onSuccessfulSave func() = nil
	patch := ctrl_client.MergeFromWithOptions(exe.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	r.runs.RunDeferredOps(req.NamespacedName)

	if exe.DeletionTimestamp != nil && !exe.DeletionTimestamp.IsZero() && !exe.Starting() {
		// Remove the finalizer if deletion has been requested and the Executable has completed initial startup
		log.Info("Executable is being deleted...")
		r.releaseExecutableResources(ctx, &exe, log)
		change = deleteFinalizer(&exe, executableFinalizer, log)

	} else {
		change = ensureFinalizer(&exe, executableFinalizer, log)

		// If we added a finalizer, we'll do the additional reconciliation next call
		if change == noChange {
			change, onSuccessfulSave = r.updateRunState(ctx, &exe, log)

			if change == noChange {
				change = r.ensureExecutableRunning(ctx, &exe, log)
			}

			if change != noChange && exe.Status.State != apiv1.ExecutableStateStarting && exe.Status.State != apiv1.ExecutableStateRunning {
				removeEndpointsForWorkload(r, ctx, &exe, log)
			}
		}
	}

	result, err := saveChanges(r, ctx, &exe, patch, change, onSuccessfulSave, log)
	if exe.Done() {
		log.V(1).Info("Executable reached done state")
	}
	return result, err
}

// Handle notification about changed or completed Executable run. This function runs outside of the reconcilation loop,
// so we just memorize the PID and process exit code (if available) in the run status map,
// and not attempt to modify any Kubernetes data.
func (r *ExecutableReconciler) OnRunChanged(runID RunID, pid process.Pid_t, exitCode *int32, err error) {
	name, currentRunInfo, found := r.runs.FindBySecondKey(runID)

	// It's possible we receive a notification about a run we are not tracking, but that means we
	// no longer care about its status, so we can just ignore it.
	if !found {
		return
	}

	// The status object contains pointers and we are going to be modifying values pointed by them,
	// so we need to make a copy to avoid data races with other controller methods.
	newRunInfo := currentRunInfo.DeepCopy()

	var effectiveExitCode *int32
	if err != nil {
		r.Log.V(1).Info("Executable run could not be tracked", "Executable", name.String(), "RunID", runID, "LastState", currentRunInfo.exeState, "Error", err.Error())
		effectiveExitCode = apiv1.UnknownExitCode
	} else {
		r.Log.V(1).Info("queue Executable run change", "Executable", name.String(), "RunID", runID, "LastState", currentRunInfo.exeState, "NewPID", pid, "NewExitCode", exitCode)
		effectiveExitCode = exitCode
	}

	// Memorize PID and (if applicable) exit code
	newRunInfo.exitCode = effectiveExitCode
	if pid > 0 {
		if newRunInfo.pid == apiv1.UnknownPID {
			newRunInfo.pid = new(int64)
		}
		*newRunInfo.pid = int64(pid)
	}

	if newRunInfo.exitCode != apiv1.UnknownExitCode {
		newRunInfo.exeState = apiv1.ExecutableStateFinished
	} else if newRunInfo.pid != apiv1.UnknownPID {
		newRunInfo.exeState = apiv1.ExecutableStateRunning
	}

	r.runs.QueueDeferredOp(name, func(runs *maps.DualKeyMap[types.NamespacedName, RunID, *runInfo]) {
		// The run may have been deleted by the time we get here, so we do not care if Update() returns false.
		_ = runs.Update(name, runID, newRunInfo)
	})

	// Schedule reconciliation for corresponding executable
	scheduleErr := r.debouncer.ReconciliationNeeded(name, runID, r.scheduleExecutableReconciliation)
	if scheduleErr != nil {
		r.Log.Error(scheduleErr, "could not schedule reconciliation for Executable object", "Executable", name.String(), "RunID", runID, "ExecutableState", currentRunInfo.exeState)
	}
}

// Handle setting up process tracking once an Executable has transitioned from newly created or starting to a stabe state such as running or finished.
func (r *ExecutableReconciler) OnStartingCompleted(name types.NamespacedName, runID RunID, exeStatus apiv1.ExecutableStatus, startWaitForRunCompletion func()) {
	startupSucceeded := runID != UnknownRunID

	if startupSucceeded {
		r.Log.V(1).Info("Executable completed startup", "Executable", name.String(), "RunID", runID, "NewState", exeStatus.State, "NewExitCode", exeStatus.ExitCode)
	} else {
		// If we couldn't successfully reach a running state, update the starting cache so that it can be
		// reported during the next reconciliation loop
		r.Log.V(1).Info("Executable failed to reach valid running state", "Executable", name.String())
		exeStatus.State = apiv1.ExecutableStateFailedToStart
		exeStatus.FinishTimestamp = metav1.Now()
	}

	// OnStartingCompleted might be invoked asynchronously. To avoid race conditions,
	//  we always queue updates to the runs map and run them as part of reconciliation function.
	r.runs.QueueDeferredOp(name, func(runs *maps.DualKeyMap[types.NamespacedName, RunID, *runInfo]) {
		startingRunID, ri, found := runs.FindByFirstKey(name)
		if !found {
			// Should never happen
			r.Log.Error(fmt.Errorf("could not find starting run data after Executable start attempt"), "Executable", name.String(), "RunID", runID, "NewState", exeStatus.State, "NewExitCode", exeStatus.ExitCode)
		} else {
			ri.UpdateFrom(exeStatus)
			_ = runs.UpdateChangingSecondKey(name, startingRunID, runID, ri)
			if startupSucceeded {
				startWaitForRunCompletion()
			}
		}
	})

	// Schedule reconciliation for corresponding Executable
	scheduleErr := r.debouncer.ReconciliationNeeded(name, runID, r.scheduleExecutableReconciliation)
	if scheduleErr != nil {
		r.Log.Error(scheduleErr, "could not schedule reconciliation for Executable object", "Executable", name.String(), "RunID", runID, "LastState", exeStatus.State)
	}
}

// Performs actual Executable startup, as necessary, and updates status with the appropriate data.
// If startup is successful, starts tracking the run.
// Returns an indication whether Executable object was changed.
func (r *ExecutableReconciler) ensureExecutableRunning(ctx context.Context, exe *apiv1.Executable, log logr.Logger) objectChange {
	if !exe.Status.StartupTimestamp.IsZero() {
		log.V(1).Info("Executable already started...")
		return noChange
	}

	if exe.Done() {
		log.V(1).Info("Executable reached done state, nothing to do...")
		return noChange
	}

	if exe.Spec.Stop {
		if exe.Status.FinishTimestamp.IsZero() {
			log.V(1).Info("Executable launched with finished as desired state; marking it as finished...")
			exe.Status.State = apiv1.ExecutableStateFailedToStart
			exe.Status.FinishTimestamp = metav1.Now()
		}
	}

	if _, runInfo, found := r.runs.FindByFirstKey(exe.NamespacedName()); found {
		if runInfo.exeState == apiv1.ExecutableStateStarting {
			// We are in the process of starting the run, so we should wait for it to complete.
			log.V(1).Info("tracking Executable start...")
			return noChange
		}

		// Ensure the status matches the current state.
		return runInfo.ApplyTo(exe, log)
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

	// Ports reserved for services that the Executable implements without specifying the desired port to use (via service-producer annotation).
	reservedServicePorts := make(map[types.NamespacedName]int32)

	err := r.computeEffectiveEnvironment(ctx, exe, reservedServicePorts, log)
	if isTransientTemplateError(err) {
		log.Info("could not compute effective environment for the Executable, retrying startup...", "Cause", err.Error())
		return additionalReconciliationNeeded
	} else if err != nil {
		log.Error(err, "could not compute effective environment for the Executable")
		exe.Status.State = apiv1.ExecutableStateFailedToStart
		exe.Status.FinishTimestamp = metav1.Now()
		return statusChanged
	}

	err = r.computeEffectiveInvocationArgs(ctx, exe, reservedServicePorts, log)
	if isTransientTemplateError(err) {
		log.Info("could not compute effective invocation arguments for the Executable, retrying startup...", "Cause", err.Error())
		return additionalReconciliationNeeded
	} else if err != nil {
		log.Error(err, "could not compute effective invocation arguments for the Executable")
		exe.Status.State = apiv1.ExecutableStateFailedToStart
		exe.Status.FinishTimestamp = metav1.Now()
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
	run.reservedPorts = reservedServicePorts
	r.runs.Store(exe.NamespacedName(), getStartingRunID(exe.NamespacedName()), run)

	err = runner.StartRun(ctx, exe, r, log)
	if err != nil {
		log.Error(err, "failed to start Executable")

		r.runs.DeleteByFirstKey(exe.NamespacedName())

		if exe.Status.State != apiv1.ExecutableStateFailedToStart {
			// The executor did not mark the Executable as failed to start, so we should retry.
			return additionalReconciliationNeeded
		} else {
			// The Executable failed to start and reached the final state.
			return statusChanged
		}
	}

	if exe.Status.State == apiv1.ExecutableStateRunning {
		// This was a synchronous startup, OnStartupCompleted() has been called already and queued the update of the runs map.

		ensureEndpointsForWorkload(ctx, r, exe, run.reservedPorts, r.Log)
	}

	if exe.Status.State == apiv1.ExecutableStateStarting {
		run.UpdateFrom(exe.Status)
		r.runs.Store(exe.NamespacedName(), getStartingRunID(exe.NamespacedName()), run)
	}

	return statusChanged
}

// Stops the underlying Executable run, if any.
// The Execuatable data update related to stopped run is handled by the caller.
func (r *ExecutableReconciler) stopExecutable(ctx context.Context, exe *apiv1.Executable, log logr.Logger) {
	var runID RunID = RunID(exe.Status.ExecutionID)
	if runID == "" || exe.Done() {
		log.V(1).Info("Executable is not running, nothing to stop...")
		return
	}

	_, _, found := r.runs.FindBySecondKey(runID)
	if !found {
		// Either we never attempted to start the Executable, or we already attempted to stop the process,
		// and the current reconciliation loop is just catching up to some other changes.
		// Either way there is nothing to do.
		log.V(1).Info("run data is not available, nothing to stop...")
		return
	}

	runner, found := r.ExecutableRunners[exe.Spec.ExecutionType]
	if !found {
		// Should never happen
		err := fmt.Errorf("no runner found for execution type '%s'", exe.Spec.ExecutionType)
		log.Error(err, "the Executable cannot be stopped")
		return
	}

	err := runner.StopRun(ctx, runID, log)
	if err != nil {
		log.Error(err, "could not stop the Executable", "RunID", runID)
	} else {
		log.V(1).Info("Executable stopped", "RunID", runID)
	}
}

func (r *ExecutableReconciler) releaseExecutableResources(ctx context.Context, exe *apiv1.Executable, log logr.Logger) {
	var runID RunID = RunID(exe.Status.ExecutionID)
	if runID == "" || exe.Done() {
		return // Nothing to do--the Executable is not running
	}

	r.stopExecutable(ctx, exe, log)

	// We are about to terminate the run. Since the run is not allowed to complete normally,
	// we are not interested in its exit code (it will indicate a failure,
	// but it is a failure induced by the Executable user), so we stop tracking the run now.
	r.runs.DeleteBySecondKey(runID)
	removeEndpointsForWorkload(r, ctx, exe, log)
	r.deleteOutputFiles(exe, log)
}

// Called by the main reconciler function, this function will update the Executable run state
// based on run state change notifications we have received.
// Returns whether the Executable object was changed, and a function that should be called
// when changes to the Executable object are successfully saved.
func (r *ExecutableReconciler) updateRunState(ctx context.Context, exe *apiv1.Executable, log logr.Logger) (objectChange, func()) {
	var onSuccessfulSave func() = nil

	runID, runInfo, found := r.runs.FindByFirstKey(exe.NamespacedName())
	if !found {
		// We haven't started the run yet, or Executable is being deleted and the run was terminated.
		// Either way, nothing to do.
		return noChange, onSuccessfulSave
	}

	if exe.Spec.Stop {
		// If we think the executable is still running, we should attempt to stop it.
		if runInfo.exeState == apiv1.ExecutableStateRunning {
			log.V(1).Info("attempting to stop the Executable...", "RunID", runID)
			r.stopExecutable(ctx, exe, log)
			// Don't set the Executable state to Finished yet because we might need to process the run completion notification.
		}
	}

	change := runInfo.ApplyTo(exe, log)
	if change == noChange {
		// The run state has not changed.
		return noChange, onSuccessfulSave
	}

	reachedFinalState := runInfo.exeState == apiv1.ExecutableStateFinished ||
		runInfo.exeState == apiv1.ExecutableStateTerminated ||
		runInfo.exeState == apiv1.ExecutableStateFailedToStart
	if reachedFinalState {
		exe.Status.FinishTimestamp = metav1.Now()
		change |= statusChanged
	}

	if runInfo.exeState == apiv1.ExecutableStateRunning {
		ensureEndpointsForWorkload(ctx, r, exe, runInfo.reservedPorts, log)
	}

	if runInfo.exeState != apiv1.ExecutableStateStarting && runInfo.exeState != apiv1.ExecutableStateRunning {
		onSuccessfulSave = func() {
			// The executable is no longer running, so we can delete associated data,
			// but only if the Executable object is successfully saved.
			// Otherwise we will need this data when we re-try to update run state upon next reconciliation.
			r.runs.DeleteByFirstKey(exe.NamespacedName())
		}
	}

	return change, onSuccessfulSave
}

func (r *ExecutableReconciler) deleteOutputFiles(exe *apiv1.Executable, log logr.Logger) {
	// Do not bother updating the Executable object--this method is called when the object is being deleted.

	if exe.Status.StdOutFile != "" {
		if err := os.Remove(exe.Status.StdOutFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Error(err, "could not remove process's standard output file", "path", exe.Status.StdOutFile)
		}
	}

	if exe.Status.StdErrFile != "" {
		if err := os.Remove(exe.Status.StdErrFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Error(err, "could not remove process's standard error file", "path", exe.Status.StdErrFile)
		}
	}
}

func (r *ExecutableReconciler) scheduleExecutableReconciliation(rti reconcileTriggerInput[RunID]) error {
	event := ctrl_event.GenericEvent{
		Object: &apiv1.Executable{
			ObjectMeta: metav1.ObjectMeta{
				Name:      rti.target.Name,
				Namespace: rti.target.Namespace,
			},
		},
	}
	r.notifyRunChanged.In <- event
	return nil
}

func (r *ExecutableReconciler) createEndpoint(
	ctx context.Context,
	owner ctrl_client.Object,
	serviceProducer ServiceProducer,
	log logr.Logger,
) (*apiv1.Endpoint, error) {
	endpointName, err := MakeUniqueName(owner.GetName())
	if err != nil {
		log.Error(err, "could not generate unique name for Endpoint object")
		return nil, err
	}

	if serviceProducer.Port == 0 {
		return nil, fmt.Errorf("%s: missing information about the port to expose the service", serviceProducerIsInvalid)
	}

	address := serviceProducer.Address
	if address == "" {
		address = "localhost"
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

	envMap.Apply(maps.SliceToMap(os.Environ(), func(envStr string) (string, string) {
		parts := strings.SplitN(envStr, "=", 2)
		return parts[0], parts[1]
	}))

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

	exe.Status.EffectiveEnv = maps.MapToSlice[string, string, apiv1.EnvVar](envMap.Data(), func(key string, value string) apiv1.EnvVar {
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

// Stores information about Executable run
type runInfo struct {
	// State of the run (starting, running, finished, etc.)
	exeState apiv1.ExecutableState

	// Process ID of the process that runs the Executable
	pid *int64

	// Execution ID for the Executable (see ExecutableStatus for details)
	executionID string

	// Exit code of the Executable process
	exitCode *int32

	// Timestamp when the run was started
	startupTimestamp metav1.Time

	// Timestamp when the run was finished
	finishTimestamp metav1.Time

	// Paths to captured standard output and standard error files
	stdOutFile string
	stdErrFile string

	// The map of ports reserved for services that the Executable implements
	reservedPorts map[types.NamespacedName]int32
}

func NewRunInfo() *runInfo {
	return &runInfo{
		exeState:         "",
		pid:              apiv1.UnknownPID,
		exitCode:         apiv1.UnknownExitCode,
		startupTimestamp: metav1.Time{},
		finishTimestamp:  metav1.Time{},
	}
}

func (ri *runInfo) UpdateFrom(status apiv1.ExecutableStatus) {
	ri.exeState = status.State
	if status.PID != apiv1.UnknownPID {
		ri.pid = new(int64)
		*ri.pid = *status.PID
	}
	if status.ExecutionID != "" {
		ri.executionID = status.ExecutionID
	}
	if status.ExitCode != apiv1.UnknownExitCode {
		ri.exitCode = new(int32)
		*ri.exitCode = *status.ExitCode
	}
	if !status.StartupTimestamp.IsZero() {
		ri.startupTimestamp = status.StartupTimestamp
	}
	if !status.FinishTimestamp.IsZero() {
		ri.finishTimestamp = status.FinishTimestamp
	}
	if status.StdOutFile != "" {
		ri.stdOutFile = status.StdOutFile
	}
	if status.StdErrFile != "" {
		ri.stdErrFile = status.StdErrFile
	}
}

func (ri *runInfo) DeepCopy() *runInfo {
	retval := runInfo{
		exeState: ri.exeState,
	}
	if ri.pid != apiv1.UnknownPID {
		retval.pid = new(int64)
		*retval.pid = *ri.pid
	}
	retval.executionID = ri.executionID
	if ri.exitCode != apiv1.UnknownExitCode {
		retval.exitCode = new(int32)
		*retval.exitCode = *ri.exitCode
	}
	if len(ri.reservedPorts) > 0 {
		retval.reservedPorts = stdlib_maps.Clone(ri.reservedPorts)
	}
	retval.startupTimestamp = ri.startupTimestamp
	retval.finishTimestamp = ri.finishTimestamp
	retval.stdOutFile = ri.stdOutFile
	retval.stdErrFile = ri.stdErrFile
	return &retval
}

func (ri *runInfo) ApplyTo(exe *apiv1.Executable, log logr.Logger) objectChange {
	status := exe.Status
	originalStatusRI := NewRunInfo()
	originalStatusRI.UpdateFrom(status)
	changed := noChange

	if ri.exeState != "" && status.State != ri.exeState {
		status.State = ri.exeState
		changed = statusChanged
	}

	if ri.pid != apiv1.UnknownPID {
		if status.PID == apiv1.UnknownPID {
			status.PID = new(int64)
		}
		if *status.PID != *ri.pid {
			*status.PID = *ri.pid
			changed = statusChanged
		}
	}

	if ri.executionID != "" && status.ExecutionID != ri.executionID {
		status.ExecutionID = ri.executionID
		changed = statusChanged
	}

	if ri.exitCode != apiv1.UnknownExitCode {
		if status.ExitCode == apiv1.UnknownExitCode {
			status.ExitCode = new(int32)
		}
		if *status.ExitCode != *ri.exitCode {
			*status.ExitCode = *ri.exitCode
			changed = statusChanged
		}
	}

	// We only overwrite timestamps if the Executable status has them as zero values
	// to avoid round-tripping errors.
	if !ri.startupTimestamp.IsZero() && status.StartupTimestamp.IsZero() {
		status.StartupTimestamp = ri.startupTimestamp
		changed = statusChanged
	}

	if !ri.finishTimestamp.IsZero() && status.FinishTimestamp.IsZero() {
		status.FinishTimestamp = ri.finishTimestamp
		changed = statusChanged
	}

	if ri.stdOutFile != "" && status.StdOutFile != ri.stdOutFile {
		status.StdOutFile = ri.stdOutFile
		changed = statusChanged
	}

	if ri.stdErrFile != "" && status.StdErrFile != ri.stdErrFile {
		status.StdErrFile = ri.stdErrFile
		changed = statusChanged
	}

	if changed != noChange {
		exe.Status = status

		log.Info("Executable run changed", "PropertiesChanged", DiffString(originalStatusRI, ri))
	}

	return changed
}

func (ri *runInfo) String() string {
	return fmt.Sprintf(
		"{exeState=%s, pid=%s, executionID=%s, exitCode=%s, startupTimestamp=%s, finishTimestamp=%s, stdOutFile=%s, stdErrFile=%s}",
		ri.exeState,
		logger.IntPtrValToString(ri.pid),
		logger.FriendlyString(ri.executionID),
		logger.IntPtrValToString(ri.exitCode),
		logger.FriendlyTimestamp(ri.startupTimestamp),
		logger.FriendlyTimestamp(ri.finishTimestamp),
		logger.FriendlyString(ri.stdOutFile),
		logger.FriendlyString(ri.stdErrFile),
	)
}

func DiffString(r1, r2 *runInfo) string {
	sb := strings.Builder{}
	sb.WriteString("{")

	if r1.exeState != r2.exeState {
		sb.WriteString(fmt.Sprintf("exeState=%s->%s, ", r1.exeState, r2.exeState))
	}

	if logger.IsPtrValDifferent(r1.pid, r2.pid) {
		sb.WriteString(fmt.Sprintf("pid=%s->%s, ", logger.IntPtrValToString(r1.pid), logger.IntPtrValToString(r2.pid)))
	}

	if r1.executionID != r2.executionID {
		sb.WriteString(fmt.Sprintf("executionID=%s->%s, ", logger.FriendlyString(r1.executionID), logger.FriendlyString(r2.executionID)))
	}

	if logger.IsPtrValDifferent(r1.exitCode, r2.exitCode) {
		sb.WriteString(fmt.Sprintf("exitCode=%s->%s, ", logger.IntPtrValToString(r1.exitCode), logger.IntPtrValToString(r2.exitCode)))
	}

	if r1.startupTimestamp != r2.startupTimestamp {
		sb.WriteString(fmt.Sprintf("startupTimestamp=%s->%s, ", logger.FriendlyTimestamp(r1.startupTimestamp), logger.FriendlyTimestamp(r2.startupTimestamp)))
	}

	if r1.finishTimestamp != r2.finishTimestamp {
		sb.WriteString(fmt.Sprintf("finishTimestamp=%s->%s, ", logger.FriendlyTimestamp(r1.finishTimestamp), logger.FriendlyTimestamp(r2.finishTimestamp)))
	}

	if r1.stdOutFile != r2.stdOutFile {
		sb.WriteString(fmt.Sprintf("stdOutFile=%s->%s, ", logger.FriendlyString(r1.stdOutFile), logger.FriendlyString(r2.stdOutFile)))
	}

	if r1.stdErrFile != r2.stdErrFile {
		sb.WriteString(fmt.Sprintf("stdErrFile=%s->%s, ", logger.FriendlyString(r1.stdErrFile), logger.FriendlyString(r2.stdErrFile)))
	}

	sb.WriteString("}")
	return sb.String()
}

var _ fmt.Stringer = (*runInfo)(nil)

func getStartingRunID(exeName types.NamespacedName) RunID {
	return RunID("__starting-" + exeName.String())
}
