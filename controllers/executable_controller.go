// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"text/template"

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
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

// ExecutableReconciler reconciles a Executable object
type ExecutableReconciler struct {
	ctrl_client.Client

	Log                 logr.Logger
	reconciliationSeqNo uint32
	ExecutableRunners   map[apiv1.ExecutionType]ExecutableRunner

	// A map that stores information about running Executables,
	// searchable by Executable name (first key), or run ID (second key).
	runs *maps.SynchronizedDualKeyMap[types.NamespacedName, RunID, apiv1.ExecutableStatus]

	// A map that stores information about reserved service ports for Executables,
	// searchabe by Executable name.
	reservedServicePorts syncmap.Map[types.NamespacedName, map[types.NamespacedName]int32]

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
		Client:               client,
		ExecutableRunners:    executableRunners,
		runs:                 maps.NewSynchronizedDualKeyMap[types.NamespacedName, RunID, apiv1.ExecutableStatus](),
		reservedServicePorts: syncmap.Map[types.NamespacedName, map[types.NamespacedName]int32]{},
		notifyRunChanged:     chanx.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx, 1),
		debouncer:            newReconcilerDebouncer[RunID](reconciliationDebounceDelay),
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
		if apierrors.IsNotFound(err) {
			// Ensure the cache of Executable to run ID is cleared
			r.runs.DeleteByFirstKey(req.NamespacedName)
			log.V(1).Info("Executable not found, nothing to do...")
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the Executable", "exe", exe)
			return ctrl.Result{}, err
		}
	}

	log = log.WithValues("State", exe.Status.State).WithValues("ExecutionID", exe.Status.ExecutionID)

	var change objectChange
	var onSuccessfulSave func() = nil
	patch := ctrl_client.MergeFromWithOptions(exe.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if exe.DeletionTimestamp != nil && !exe.DeletionTimestamp.IsZero() && !exe.Starting() {
		// Remove the finalizer if deletion has been requested and the Executable has completed initial startup
		log.Info("Executable is being deleted...")
		r.stopExecutable(ctx, &exe, log)
		change = deleteFinalizer(&exe, executableFinalizer)
		r.deleteOutputFiles(&exe, log)
		removeEndpointsForWorkload(r, ctx, &exe, log)
	} else {
		change = ensureFinalizer(&exe, executableFinalizer)

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

	result, err := saveChanges(r, ctx, &exe, patch, change, log)
	if exe.Done() {
		log.V(1).Info("Executable reached done state")
	}
	if err == nil && onSuccessfulSave != nil {
		onSuccessfulSave()
	}
	return result, err
}

// Handle notification about changed or completed Executable run. This function runs outside of the reconcilation loop,
// so we just memorize the PID and process exit code (if available) in the run status map,
// and not attempt to modify any Kubernetes data.
func (r *ExecutableReconciler) OnRunChanged(runID RunID, pid process.Pid_t, exitCode *int32, err error) {
	name, exeStatus, found := r.runs.FindBySecondKey(runID)

	// It's possible we receive a notification about a run we are not tracking, but that means we
	// no longer care about its status, so we can just ignore it.
	if !found {
		return
	}

	// The status object contains pointers and we are going to be modifying values pointed by them,
	// so we need to make a copy to avoid data races with other controller methods.
	ps := exeStatus.DeepCopy()

	var effectiveExitCode *int32
	if err != nil {
		r.Log.V(1).Info("Executable run could not be tracked", "Executable", name.String(), "RunID", runID, "LastState", exeStatus.State, "Error", err.Error())
		effectiveExitCode = apiv1.UnknownExitCode
	} else {
		r.Log.V(1).Info("queue Executable run changed", "Executable", name.String(), "RunID", runID, "LastState", exeStatus.State, "NewPID", pid, "NewExitCode", exitCode)
		effectiveExitCode = exitCode
	}

	// Memorize PID and (if applicable) exit code
	if ps.PID == apiv1.UnknownPID {
		ps.PID = new(int64)
	}
	*ps.PID = int64(pid)
	ps.ExitCode = effectiveExitCode

	if ps.ExitCode != apiv1.UnknownExitCode {
		ps.State = apiv1.ExecutableStateFinished
	} else if ps.PID != apiv1.UnknownPID {
		ps.State = apiv1.ExecutableStateRunning
	}

	updated := r.runs.Update(name, runID, *ps)
	if !updated {
		// The Executable is being deleted, so we do not care about this run anymore.
		return
	}

	// Schedule reconciliation for corresponding executable
	scheduleErr := r.debouncer.ReconciliationNeeded(name, runID, r.scheduleExecutableReconciliation)
	if scheduleErr != nil {
		r.Log.Error(scheduleErr, "could not schedule reconciliation for Executable object", "Executable", name.String(), "RunID", runID, "LastState", exeStatus.State)
	}
}

// Handle setting up process tracking once an Executable has transitioned from newly created or starting to a stabe state such as running or finished.
func (r *ExecutableReconciler) OnStarted(name types.NamespacedName, runID RunID, exeStatus apiv1.ExecutableStatus, startWaitForRunCompletion func()) {
	if runID != UnknownRunID {
		r.Log.V(1).Info("Executable completed startup", "Executable", name.String(), "RunID", runID, "NewState", exeStatus.State, "NewExitCode", exeStatus.ExitCode)
		r.runs.Store(name, runID, exeStatus)

		startWaitForRunCompletion()
	} else {
		r.Log.V(1).Info("Executable started with invalid RunID", "Executable", name.String(), "NewState", exeStatus.State, "NewExitCode", exeStatus.ExitCode)
	}
}

// Performs actual Executable startup, as necessary, and updates status with the appropriate data.
// If startup is successful, starts tracking the run.
// Returns:
// - an indication whether Executable object was changed,
// - information about any ports allocated for serving services (if not pre-defined by clients via service-producer annotation)
// - an indication whether the Executable was actually started (and thus Endpoints need to be allocated)
func (r *ExecutableReconciler) ensureExecutableRunning(ctx context.Context, exe *apiv1.Executable, log logr.Logger) objectChange {
	if !exe.Status.StartupTimestamp.IsZero() {
		log.V(1).Info("Executable already started...")
		return noChange
	}

	if exe.Done() {
		log.V(1).Info("Executable reached done state, nothing to do...")
		return noChange
	}

	if _, rps, found := r.runs.FindByFirstKey(exe.NamespacedName()); found {
		// We are already tracking a run for this Executable, ensure the status matches the current state.
		log.V(1).Info("Executable already started...")

		rps.CopyTo(exe)
		return statusChanged
	}

	if exe.Status.State == apiv1.ExecutableStateStarting {
		log.V(1).Info("Executable already starting, wait for OnStarted notification...")
		return noChange
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

	reservedServicePorts, found := r.reservedServicePorts.Load(exe.NamespacedName())
	if !found {
		newReservedServicePorts, err := r.computeEffectiveEnvironment(ctx, exe, log)
		if errors.Is(err, errServiceNotAssignedPort) {
			log.Info("could not compute effective environment for the Executable because one of the services it uses does not have a port assigned yet")
			return additionalReconciliationNeeded
		} else if err != nil {
			log.Error(err, "could not compute effective environment for the Executable")
			exe.Status.State = apiv1.ExecutableStateFailedToStart
			exe.Status.FinishTimestamp = metav1.Now()
			return statusChanged
		}

		reservedServicePorts = newReservedServicePorts
	}

	log.V(1).Info("starting Executable...")

	if err := runner.StartRun(ctx, exe, r, log); err != nil {
		log.Error(err, "failed to start Executable")
		if exe.Status.State != apiv1.ExecutableStateFailedToStart {
			// The executor did not mark the Executable as failed to start, so we should retry.
			return additionalReconciliationNeeded
		} else {
			// The Executable failed to start and reached the final state.
			return statusChanged
		}
	}

	if exe.Status.State == apiv1.ExecutableStateRunning {
		log.V(1).Info("creating Endpoints...", "services", maps.Keys(reservedServicePorts), "ports", maps.Values(reservedServicePorts))
		ensureEndpointsForWorkload(ctx, r, exe, reservedServicePorts, r.Log)
		r.reservedServicePorts.Delete(exe.NamespacedName())
	} else if exe.Status.State == apiv1.ExecutableStateStarting {
		log.V(1).Info("registering Endpoints for later creation", "services", maps.Keys(reservedServicePorts), "ports", maps.Values(reservedServicePorts))
		r.reservedServicePorts.Store(exe.NamespacedName(), reservedServicePorts)
	}

	return statusChanged
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
	r.reservedServicePorts.Delete(exe.NamespacedName())

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
// Returns whether the Executable object was changed, and a function that should be called
// when changes to the Executable object are successfully saved.
func (r *ExecutableReconciler) updateRunState(ctx context.Context, exe *apiv1.Executable, log logr.Logger) (objectChange, func()) {
	var change objectChange = noChange
	var onSuccessfulSave func() = nil

	if runID, ps, found := r.runs.FindByFirstKey(exe.NamespacedName()); found {
		if ps.State != exe.Status.State || !arePointerValuesEqual(ps.PID, exe.Status.PID) {
			log.Info("Executable run changed", "RunID", runID, "PID", exe.Status.PID, "NewPID", ps.PID, "State", exe.Status.State, "NewState", ps.State, "ExitCode", exe.Status.ExitCode, "NewExitCode", ps.ExitCode)
			exe.UpdateRunningStatus(ps.PID, ps.ExitCode, ps.State)
			change = statusChanged

			if ps.State == apiv1.ExecutableStateRunning {
				if reservedServicePorts, found := r.reservedServicePorts.Load(exe.NamespacedName()); found {
					log.V(1).Info("creating Endpoints...", "services", maps.Keys(reservedServicePorts), "ports", maps.Values(reservedServicePorts))
					ensureEndpointsForWorkload(ctx, r, exe, reservedServicePorts, log)
					r.reservedServicePorts.Delete(exe.NamespacedName())
				} else {
					log.V(1).Info("no Endpoints to create")
				}
			}

			if ps.State != apiv1.ExecutableStateStarting && ps.State != apiv1.ExecutableStateRunning {
				onSuccessfulSave = func() {
					// The executable is no longer running, so we can delete associated data,
					// but only if the Executable object is successfully saved.
					// Otherwise we will need this data when we re-try to update run state upon next reconciliation.
					r.runs.DeleteByFirstKey(exe.NamespacedName())
					r.reservedServicePorts.Delete(exe.NamespacedName())
				}
			}
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

var errServiceNotAssignedPort = fmt.Errorf("service does not have a port assigned yet")

// Computes the effective set of environment variables for the Executable run and stores it in Status.EffectiveEnv.
// The returned map contains newly reserved ports for services that the Executable implements without specifying
// the desired port to use (via service-producer annotation).
func (r *ExecutableReconciler) computeEffectiveEnvironment(ctx context.Context, exe *apiv1.Executable, log logr.Logger) (map[types.NamespacedName]int32, error) {
	// Start with ambient environment.
	envMap := maps.SliceToMap(os.Environ(), func(envStr string) (string, string) {
		parts := strings.SplitN(envStr, "=", 2)
		return parts[0], parts[1]
	})

	// Add environment variables from .env files.
	if len(exe.Spec.EnvFiles) > 0 {
		if additionalEnv, err := godotenv.Read(exe.Spec.EnvFiles...); err != nil {
			log.Error(err, "Environment settings from .env file(s) were not applied.", "EnvFiles", exe.Spec.EnvFiles)
		} else {
			envMap = maps.Apply(envMap, additionalEnv)
		}
	}

	// Add environment variables from the Spec.
	for _, envVar := range exe.Spec.Env {
		envMap[envVar.Name] = envVar.Value
	}

	// Apply variable substitutions.
	servicesProduced, err := getServiceProducersForObject(exe, log)
	if err != nil {
		return nil, err
	}
	reservedServicePorts := make(map[types.NamespacedName]int32)

	tmpl := template.New("envar").Funcs(template.FuncMap{
		"portForServing": func(serviceNamespacedName string) (int32, error) {
			var serviceName = asNamespacedName(serviceNamespacedName, exe.GetObjectMeta().Namespace)

			for _, sp := range servicesProduced {
				if serviceName != sp.ServiceNamespacedName() {
					continue
				}
				if sp.Port != 0 {
					// The service producer annotation specifies the desired port, so we do not have to reserve anything,
					// and we do not need to report it via reservedServicePorts.
					return sp.Port, nil
				}

				// Need to take a peek at the Service to find out what protocol it is using.
				var svc apiv1.Service
				if err := r.Get(ctx, serviceName, &svc); err != nil {
					// CONSIDER in future we could be smarter and delay the startup of the Executable until
					// the service appears in the system, leaving the Executable in "pending" state.
					// This would necessitate watching over Services (specifically, Service creation).
					return 0, fmt.Errorf("service '%s' referenced by an environment variable does not exist", serviceName)
				}

				port, err := networking.GetFreePort(svc.Spec.Protocol, sp.Address)
				if err != nil {
					return 0, fmt.Errorf("could not allocate a port for service '%s' with desired address '%s': %w", serviceName, sp.Address, err)
				}
				reservedServicePorts[serviceName] = port
				return port, nil
			}

			return 0, fmt.Errorf("service '%s' referenced by an environment variable is not produced by the Executable", serviceName)
		},
		"portFor": func(serviceNamespacedName string) (int32, error) {
			var serviceName = asNamespacedName(serviceNamespacedName, exe.GetObjectMeta().Namespace)

			// Need to take a peek at the Service to find out what port it is assigned.
			var svc apiv1.Service
			if err := r.Get(ctx, serviceName, &svc); err != nil {
				// CONSIDER in future we could be smarter and delay the startup of the Executable until
				// the service appears in the system, leaving the Executable in "pending" state.
				// This would necessitate watching over Services (specifically, Service creation).
				return 0, fmt.Errorf("service '%s' referenced by an environment variable does not exist", serviceName)
			} else if svc.Status.EffectivePort == 0 {
				return 0, errServiceNotAssignedPort
			}

			return svc.Status.EffectivePort, nil
		},
	})

	for key, value := range envMap {
		variableTmpl, err := tmpl.Parse(value)
		if err != nil {
			// This does not necessarily indicate a problem--the value might be a completely intentional string
			// that happens to be un-parseable as a text template.
			log.Info("substitution is not possible for environment variable", "VariableName", key, "VariableValue", value)
			continue // We are going to use the value as-is.
		}

		var sb strings.Builder
		err = variableTmpl.Execute(&sb, exe)
		if errors.Is(err, errServiceNotAssignedPort) {
			return nil, err
		} else if err != nil {
			// We could not apply the template, so we are going to use the variable value as-is.
			// This will likely cause the value of the environment variable to be something that is not useful,
			// but it will be easier for the user to diagnose the problem if we start the Executable anyway.
			// TODO: the error from applying the template should be reported to the user via an event
			// (compare with https://github.com/microsoft/usvc/issues/20)
			log.Error(err, "could not perform substitution for environment variable", "VariableName", key, "VariableValue", value)
		} else {
			envMap[key] = sb.String()
		}
	}

	exe.Status.EffectiveEnv = maps.MapToSlice[string, string, apiv1.EnvVar](envMap, func(key string, value string) apiv1.EnvVar {
		return apiv1.EnvVar{Name: key, Value: value}
	})

	return reservedServicePorts, nil
}

func arePointerValuesEqual[T comparable](ptr1, ptr2 *T) bool {
	switch {
	case ptr1 == nil && ptr2 == nil:
		return true
	case ptr1 == nil || ptr2 == nil:
		return false
	default:
		return *ptr1 == *ptr2
	}
}
