// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
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
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"

	"github.com/microsoft/usvc-apiserver/internal/health"
	"github.com/microsoft/usvc-apiserver/internal/networking"
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
	runs *maps.SynchronizedDualKeyMap[types.NamespacedName, RunID, *ExecutableRunInfo]

	// Health probe set used to execute health probes.
	hpSet *health.HealthProbeSet

	// Channel used to receive health probe results.
	healthProbeCh *chanx.UnboundedChan[health.HealthProbeReport]

	// Channel used to trigger reconciliation function when underlying run status changes.
	notifyRunChanged *chanx.UnboundedChan[ctrl_event.GenericEvent]

	// Debouncer used to schedule reconciliations. Extra data carried is the finished run ID.
	debouncer *reconcilerDebouncer[RunID]

	// Reconciler lifetime context, used to cancel operations during reconciler shutdown
	lifetimeCtx context.Context
}

var (
	executableFinalizer string = fmt.Sprintf("%s/executable-reconciler", apiv1.GroupVersion.Group)
	executableKind             = apiv1.GroupVersion.WithKind(reflect.TypeOf(apiv1.Executable{}).Name())
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
		runs:              maps.NewSynchronizedDualKeyMap[types.NamespacedName, RunID, *ExecutableRunInfo](),
		hpSet:             healthProbeSet,
		healthProbeCh:     chanx.NewUnboundedChan[health.HealthProbeReport](lifetimeCtx, 1),
		notifyRunChanged:  chanx.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx, 1),
		debouncer:         newReconcilerDebouncer[RunID](),
		lifetimeCtx:       lifetimeCtx,
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

func (r *ExecutableReconciler) SetupWithManager(mgr ctrl.Manager) error {
	src := ctrl_source.Channel(r.notifyRunChanged.Out, &ctrl_handler.EnqueueRequestForObject{})
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Executable{}).
		Owns(&apiv1.Endpoint{}).
		WatchesRawSource(src).
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

	_, runInfo, found := r.runs.FindByFirstKey(exe.NamespacedName())
	if !found {
		runInfo = NewRunInfo(&exe)
		r.runs.Store(exe.NamespacedName(), getStartingRunID(exe.NamespacedName()), runInfo)
	}

	// Lock the run info record so that we can safely process the current state without conflict from async updates
	runInfo.Lock()
	defer runInfo.Unlock()

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(exe.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if exe.DeletionTimestamp != nil && !exe.DeletionTimestamp.IsZero() && !exe.Starting() {
		// Remove the finalizer if deletion has been requested and the Executable has completed initial startup
		log.V(1).Info("Executable is being deleted...")
		r.releaseExecutableResources(ctx, &exe, runInfo, log)
		change = deleteFinalizer(&exe, executableFinalizer, log)
	} else if change = ensureFinalizer(&exe, executableFinalizer, log); change != noChange {
		// If we added a finalizer, we'll do the additional reconciliation next call
	} else {
		change = r.manageExecutable(ctx, &exe, runInfo, log)
	}

	result, err := saveChanges(r, ctx, &exe, patch, change, nil, log)
	if exe.Done() {
		log.V(1).Info("Executable reached done state")
	}
	return result, err
}

func (r *ExecutableReconciler) manageExecutable(ctx context.Context, exe *apiv1.Executable, runInfo *ExecutableRunInfo, log logr.Logger) objectChange {
	// Apply any cached changes to the Executable object
	change := runInfo.ApplyTo(exe, log)

	queueReconciliation := false
	switch exe.Status.State {
	case apiv1.ExecutableStateEmpty, apiv1.ExecutableStateStarting:
		queueReconciliation = r.handleNewExecutable(ctx, exe, runInfo, log)
	case apiv1.ExecutableStateRunning:
		queueReconciliation = r.ensureExecutableRunningState(ctx, exe, runInfo, log)
	case apiv1.ExecutableStateTerminated, apiv1.ExecutableStateFailedToStart, apiv1.ExecutableStateFinished, apiv1.ExecutableStateUnknown:
		queueReconciliation = r.ensureExecutableFinalState(ctx, exe, runInfo, log)
	}

	if queueReconciliation {
		change |= additionalReconciliationNeeded
	}

	// Apply any new changes to the Executable object
	change |= runInfo.ApplyTo(exe, log)

	return change
}

// Returns true if additional reconciliation is required
func (r *ExecutableReconciler) handleNewExecutable(
	ctx context.Context,
	exe *apiv1.Executable,
	runInfo *ExecutableRunInfo,
	log logr.Logger,
) bool {
	if exe.Spec.Stop && runInfo.State == apiv1.ExecutableStateEmpty {
		log.Info("Executable.Stop property was set to true before Executable was started, marking Executable as 'failed to start'...")

		runInfo.SetState(apiv1.ExecutableStateFailedToStart)
		return false
	}

	if runInfo.State == apiv1.ExecutableStateEmpty {
		// This is brand new Executable and we need to get it started.
		return r.startExecutable(ctx, exe, runInfo, log)
	}

	return false
}

// Returns true if additional reconciliation is required
func (r *ExecutableReconciler) ensureExecutableRunningState(
	ctx context.Context,
	exe *apiv1.Executable,
	runInfo *ExecutableRunInfo,
	log logr.Logger,
) bool {
	if exe.Spec.Stop {
		log.V(1).Info("attempting to stop the Executable...")
		r.stopExecutable(ctx, exe, runInfo, log)
		// Don't set the Executable state to Finished yet; we do this on run change notification.
	}

	ensureEndpointsForWorkload(ctx, r, exe, runInfo.reservedPorts, log)

	if len(exe.Spec.HealthProbes) > 0 && !runInfo.healthProbesEnabled {
		log.V(1).Info("enabling health probes for Executable...")

		// healthProbesEnabled is used to avoid enabling health probes multiple times for the same Executable
		// Enablement only fails if the lifetime context is cancelled, or under "should never happen" conditions,
		// so we set it unconditionally to avoid repeated attempts.
		runInfo.healthProbesEnabled = true

		probeErr := r.hpSet.EnableProbes(apiv1.GetNamespacedNameWithKind(exe), exe.Spec.HealthProbes)
		if probeErr != nil {
			log.Error(probeErr, "could not enable health probes for Executable")
		}
	}

	return false
}

// Returns true if additional reconciliation is required
func (r *ExecutableReconciler) ensureExecutableFinalState(
	ctx context.Context,
	exe *apiv1.Executable,
	runInfo *ExecutableRunInfo,
	log logr.Logger,
) bool {
	removeEndpointsForWorkload(r, ctx, exe, log)

	if len(exe.Spec.HealthProbes) > 0 && runInfo.healthProbesEnabled {
		log.V(1).Info("disabling health probes for Executable...")
		runInfo.healthProbesEnabled = false
		r.hpSet.DisableProbes(apiv1.GetNamespacedNameWithKind(exe))
	}

	return false
}

// Returns true if additional reconciliation is required
func (r *ExecutableReconciler) startExecutable(ctx context.Context, exe *apiv1.Executable, runInfo *ExecutableRunInfo, log logr.Logger) bool {
	runner, runnerNotFoundErr := r.getExecutableRunner(exe)
	if runnerNotFoundErr != nil {
		log.Error(runnerNotFoundErr, "the Executable cannot be run and will be marked as failed to start")
		runInfo.SetState(apiv1.ExecutableStateFailedToStart)
		return false
	}

	// Ports reserved for services that the Executable implements without specifying the desired port to use (via service-producer annotation).
	reservedServicePorts := make(map[types.NamespacedName]int32)

	err := r.computeEffectiveEnvironment(ctx, exe, runInfo, reservedServicePorts, log)
	if isTransientTemplateError(err) {
		log.Info("could not compute effective environment for the Executable, retrying startup...", "Cause", err.Error())
		return true
	} else if err != nil {
		log.Error(err, "could not compute effective environment for the Executable")
		runInfo.SetState(apiv1.ExecutableStateFailedToStart)
		return false
	}

	err = r.computeEffectiveInvocationArgs(ctx, exe, runInfo, reservedServicePorts, log)
	if isTransientTemplateError(err) {
		log.Info("could not compute effective invocation arguments for the Executable, retrying startup...", "Cause", err.Error())
		return true
	} else if err != nil {
		log.Error(err, "could not compute effective invocation arguments for the Executable")
		runInfo.SetState(apiv1.ExecutableStateFailedToStart)
		return false
	}

	if len(reservedServicePorts) > 0 {
		log.V(1).Info("reserving service ports...",
			"services", maps.Keys(reservedServicePorts),
			"ports", maps.Values(reservedServicePorts),
		)
	}

	log.V(1).Info("starting Executable...")
	runInfo.reservedPorts = reservedServicePorts

	err = runner.StartRun(ctx, exe, runInfo, r, log)
	if err != nil {
		log.Error(err, "failed to start Executable")

		if runInfo.State != apiv1.ExecutableStateFailedToStart {
			// The executor did not mark the Executable as failed to start, so we should retry.
			return true
		} else {
			// The Executable failed to start and reached the final state.
			return false
		}
	}

	return false
}

func (r *ExecutableReconciler) OnRunChanged(runID RunID, pid process.Pid_t, exitCode *int32, err error) {
	r.processRunChangeNotification(runID, pid, exitCode, err, func(ri *ExecutableRunInfo) {
		ri.SetState(apiv1.ExecutableStateRunning)
	})
}

func (r *ExecutableReconciler) OnRunCompleted(runID RunID, exitCode *int32, err error) {
	r.processRunChangeNotification(runID, process.UnknownPID, exitCode, err, func(ri *ExecutableRunInfo) {
		ri.SetState(apiv1.ExecutableStateFinished)
	})
}

// Handle notification about changed or completed Executable run. This function runs outside of the reconcilation loop,
// so we just memorize the PID and process exit code (if available) in the run status map,
// and not attempt to modify any Kubernetes data.
func (r *ExecutableReconciler) processRunChangeNotification(
	runID RunID,
	pid process.Pid_t,
	exitCode *int32,
	err error,
	updateExeState func(*ExecutableRunInfo),
) {
	name, runInfo, found := r.runs.FindBySecondKey(runID)

	// It's possible we receive a notification about a run we are not tracking, but that means we
	// no longer care about its status, so we can just ignore it.
	if !found {
		return
	}

	// Lock the run info to avoid conflicts when updating
	runInfo.mutex.Lock()
	defer runInfo.mutex.Unlock()

	// The status object contains pointers and we are going to be modifying values pointed by them,
	// so we need to make a copy to avoid data races with other controller methods.
	previousRunInfo := runInfo.DeepCopy()

	updateExeState(runInfo)

	var effectiveExitCode *int32
	if err != nil {
		r.Log.V(1).Info("Executable run could not be tracked", "Executable", name.String(), "RunID", runID, "LastState", previousRunInfo.State, "Error", err.Error())
		effectiveExitCode = apiv1.UnknownExitCode
	} else {
		r.Log.V(1).Info("queue Executable run change", "Executable", name.String(), "RunID", runID, "LastState", previousRunInfo.State, "PID", pid, "ExitCode", exitCode, "ExecutableState", runInfo.State)
		effectiveExitCode = exitCode
	}

	// Update the PID and (if applicable) exit code

	runInfo.ExitCode = effectiveExitCode
	if pid > 0 {
		if runInfo.PID == apiv1.UnknownPID {
			runInfo.PID = new(int64)
		}
		*runInfo.PID = int64(pid)
	}

	r.debouncer.ReconciliationNeeded(r.lifetimeCtx, name, runID, r.scheduleExecutableReconciliation)
}

// Handle setting up process tracking once an Executable has transitioned from newly created or starting to a stabe state such as running or finished.
func (r *ExecutableReconciler) OnStartingCompleted(name types.NamespacedName, runID RunID, runInfo *ExecutableRunInfo, startWaitForRunCompletion func()) {
	startupSucceeded := runID != UnknownRunID

	if startupSucceeded {
		r.Log.V(1).Info("Executable completed startup", "Executable", name.String(), "RunID", runID, "NewState", runInfo.State, "NewExitCode", runInfo.ExitCode)
	} else {
		// If we couldn't successfully reach a running state, update the starting cache so that it can be
		// reported during the next reconciliation loop
		r.Log.V(1).Info("Executable failed to reach valid running state", "Executable", name.String())
		runInfo.SetState(apiv1.ExecutableStateFailedToStart)
	}

	startingRunID, _, found := r.runs.FindByFirstKey(name)
	if !found {
		// Should never happen
		r.Log.Error(fmt.Errorf("could not find starting run data after Executable start attempt"), "Executable", name.String(), "RunID", runID, "NewState", runInfo.State, "NewExitCode", runInfo.ExitCode)
	} else {
		_ = r.runs.UpdateChangingSecondKey(name, startingRunID, runID, runInfo)
		if startupSucceeded {
			startWaitForRunCompletion()
		}
	}

	r.debouncer.ReconciliationNeeded(r.lifetimeCtx, name, runID, r.scheduleExecutableReconciliation)
}

// Stops the underlying Executable run, if any.
// The Execuatable data update related to stopped run is handled by the caller.
func (r *ExecutableReconciler) stopExecutable(ctx context.Context, exe *apiv1.Executable, runInfo *ExecutableRunInfo, log logr.Logger) {
	var runID RunID = RunID(runInfo.ExecutionID)
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

	runner, runnerNotFoundErr := r.getExecutableRunner(exe)
	if runnerNotFoundErr != nil {
		// Should never happen
		log.Error(runnerNotFoundErr, "the Executable cannot be stopped")
		return
	}

	err := runner.StopRun(ctx, runID, log)
	if err != nil {
		log.Error(err, "could not stop the Executable", "RunID", runID)
	} else {
		log.V(1).Info("Executable stopped", "RunID", runID)
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

func (r *ExecutableReconciler) releaseExecutableResources(ctx context.Context, exe *apiv1.Executable, runInfo *ExecutableRunInfo, log logr.Logger) {
	if len(exe.Spec.HealthProbes) > 0 {
		log.V(1).Info("disabling health probes for (deleted) Executable...")
		r.hpSet.DisableProbes(apiv1.GetNamespacedNameWithKind(exe))
	}

	var runID RunID = RunID(exe.Status.ExecutionID)
	if runID != "" && !exe.Done() {
		r.stopExecutable(ctx, exe, runInfo, log)
		removeEndpointsForWorkload(r, ctx, exe, log)
		r.deleteOutputFiles(exe, log)
	}

	// We are about to terminate the run. Since the run is not allowed to complete normally,
	// we are not interested in its exit code (it will indicate a failure,
	// but it is a failure induced by the Executable user), so we stop tracking the run now.
	r.runs.DeleteByFirstKey(exe.NamespacedName())
}

func (r *ExecutableReconciler) deleteOutputFiles(exe *apiv1.Executable, log logr.Logger) {
	// Do not bother updating the Executable object--this method is called when the object is being deleted.

	if osutil.EnvVarSwitchEnabled(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS) {
		return
	}

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

func (r *ExecutableReconciler) scheduleExecutableReconciliation(rti reconcileTriggerInput[RunID]) {
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
	runInfo *ExecutableRunInfo,
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

	runInfo.EffectiveEnv = maps.MapToSlice[apiv1.EnvVar](envMap.Data(), func(key string, value string) apiv1.EnvVar {
		return apiv1.EnvVar{Name: key, Value: value}
	})

	return nil
}

func (r *ExecutableReconciler) computeEffectiveInvocationArgs(
	ctx context.Context,
	exe *apiv1.Executable,
	runInfo *ExecutableRunInfo,
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

	runInfo.EffectiveArgs = effectiveArgs
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
			runID, runInfo, found := r.runs.FindByFirstKey(exeName)
			if !found {
				// Not tracking this Executable anymore, most likely Executable was deleted and we got a stale report.
				// We disable probes when the Executable reaches final state AND just before removing the finalizer,
				// so it is very unlikely any Executable health probes will go orphaned long-term.
				// It might look like it would be a good idea to call DisableProbes() here "just in case"
				// (DisableProbes() is idempotent), but it is not, for two reasons:
				// 1. handleHealthProbeResults() runs asynchronosuly compared to the main reconciliation loop,
				//    and calling DisableProbes() here might put us in a race condition with an Executable
				//    that is deleted and quickly re-created, a common .NET Aspire scenario.
				// 2. We cannot use deferred ops to avoid the race because we do not have a valid runInfo at this point.

				continue
			}

			func() {
				runInfo.mutex.Lock()
				defer runInfo.mutex.Unlock()

				newResult := true
				for i, hpr := range runInfo.HealthProbeResults {
					if hpr.ProbeName == report.Probe.Name {
						runInfo.HealthProbeResults[i] = report.Result
						newResult = false
						break
					}
				}

				if newResult {
					runInfo.HealthProbeResults = append(runInfo.HealthProbeResults, report.Result)
				}
			}()

			r.debouncer.ReconciliationNeeded(r.lifetimeCtx, exeName, runID, r.scheduleExecutableReconciliation)
		}
	}
}

func getStartingRunID(exeName types.NamespacedName) RunID {
	return RunID("__starting-" + exeName.String())
}
