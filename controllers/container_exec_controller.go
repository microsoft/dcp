// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/logs"
	"github.com/microsoft/usvc-apiserver/internal/templating"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

type runningContainerExecStatus struct {
	state         apiv1.ExecutableState
	effectiveEnv  []apiv1.EnvVar
	effectiveArgs []string

	// Paths to captured standard output and standard error files
	stdOutFile string
	stdErrFile string

	exitCode         *int32
	startupTimestamp metav1.MicroTime
	finishTimestamp  metav1.MicroTime

	// Function to cancel the execution
	cancel func()

	mutex *sync.Mutex
}

type ContainerExecReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	orchestrator        containers.ContainerOrchestrator

	// Currently running container exec commands
	executions syncmap.Map[types.UID, *runningContainerExecStatus]

	// Channel used to trigger reconciliation when underlying execution completes
	notifyExecChanged *concurrency.UnboundedChan[ctrl_event.GenericEvent]
	// Debouncer used to schedule reconciliation. Extra data is the running ContainerExec ID whose state changed.
	debouncer *reconcilerDebouncer[string]

	// Reconciler lifetime context, used to cancel container exec during reconciler shutdown
	lifetimeCtx context.Context

	// A WorkQueue related to stopping container executions, which need to be run asynchronously.
	stopQueue *resiliency.WorkQueue
}

const (
	maxParallelStopOps = math.MaxUint8
)

var (
	containerExecFinalizer string = fmt.Sprintf("%s/container-exec-reconciler", apiv1.GroupVersion.Group)
)

func NewContainerExecReconciler(lifetimeCtx context.Context, client ctrl_client.Client, log logr.Logger, orchestrator containers.ContainerOrchestrator) *ContainerExecReconciler {
	r := ContainerExecReconciler{
		Client:            client,
		Log:               log,
		orchestrator:      orchestrator,
		executions:        syncmap.Map[types.UID, *runningContainerExecStatus]{},
		notifyExecChanged: concurrency.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx),
		debouncer:         newReconcilerDebouncer[string](),
		lifetimeCtx:       lifetimeCtx,
		stopQueue:         resiliency.NewWorkQueue(lifetimeCtx, maxParallelStopOps),
	}
	return &r
}

func (r *ContainerExecReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	src := ctrl_source.Channel(r.notifyExecChanged.Out, &handler.EnqueueRequestForObject{})
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv1.ContainerExec{}).
		WatchesRawSource(src).
		Named(name).
		Complete(r)
}

func (r *ContainerExecReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues(
		"ContainerExec", req.NamespacedName.String(),
		"Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1),
	)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	exec := apiv1.ContainerExec{}
	err := r.Get(ctx, req.NamespacedName, &exec)

	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.V(1).Info("the ContainerExec object was deleted")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the ContainerExec object")
			getFailedCounter.Add(ctx, 1)
			return ctrl.Result{}, err
		}
	} else {
		getSucceededCounter.Add(ctx, 1)
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(exec.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if exec.DeletionTimestamp != nil && !exec.DeletionTimestamp.IsZero() {
		log.V(1).Info("ContainerExec object is being deleted")
		r.releaseContainerExecResources(&exec, log)
		change = deleteFinalizer(&exec, containerExecFinalizer, log)
	} else {
		change = ensureFinalizer(&exec, containerExecFinalizer, log)
		if change == noChange {
			change = r.ensureExec(ctx, &exec, log)
		}
	}

	result, saveErr := saveChanges(r.Client, ctx, &exec, patch, change, nil, log)
	return result, saveErr
}

func (r *ContainerExecReconciler) ensureExec(ctx context.Context, exec *apiv1.ContainerExec, log logr.Logger) objectChange {
	execStatus, found := r.executions.LoadOrStore(exec.UID, &runningContainerExecStatus{
		state: apiv1.ExecutableStateStarting,
		mutex: &sync.Mutex{},
	})

	execStatus.mutex.Lock()
	defer execStatus.mutex.Unlock()

	if found && execStatus.state != apiv1.ExecutableStateStarting {
		// We're already running this exec job, so just update the latest status
		return updateContainerExecStatus(exec, execStatus)
	}

	container := apiv1.Container{}
	getContainerErr := r.Get(ctx, commonapi.AsNamespacedName(exec.Spec.ContainerName, exec.Namespace), &container)
	if getContainerErr != nil {
		// If we failed to find the target container, retry later
		r.Log.V(1).Info("failed to find target Container for ContainerExec, retrying later", "Container", exec.Spec.ContainerName)
		return additionalReconciliationNeeded
	}

	if !container.Status.FinishTimestamp.IsZero() {
		// The container finished running, we won't be able to run this job
		r.Log.Info("container has already finished, can't start a new exec job", "Container", container.Name)
		execStatus.finishTimestamp = metav1.NowMicro()
		execStatus.state = apiv1.ExecutableStateFailedToStart
		return updateContainerExecStatus(exec, execStatus)
	}

	if container.Status.State != apiv1.ContainerStateRunning {
		// The container isn't ready yet, schedule a retry
		r.Log.V(1).Info("container is not ready to run an exec job, retrying later", "Container", container.Name)
		return additionalReconciliationNeeded
	}

	effectiveEnv, envErr := r.computeEffectiveEnvironment(ctx, exec, &container, log)
	if envErr != nil {
		if templating.IsTransientTemplateError(envErr) {
			log.V(1).Info("could not compute effective environment for the ContainerExec job, retrying startup...", "Cause", envErr.Error())
			return additionalReconciliationNeeded
		}

		log.Error(envErr, "could not compute effective environment for the ContainerExec job")
		execStatus.finishTimestamp = metav1.NowMicro()
		execStatus.state = apiv1.ExecutableStateFailedToStart
		return updateContainerExecStatus(exec, execStatus)
	}

	effectiveArgs, argsErr := r.computeEffectiveInvocationArgs(ctx, exec, &container, log)
	if argsErr != nil {
		if templating.IsTransientTemplateError(argsErr) {
			log.V(1).Info("could not compute effective invocation arguments for the ContainerExec job, retrying startup...", "Cause", argsErr.Error())
			return additionalReconciliationNeeded
		}

		log.Error(argsErr, "could not compute effective invocation arguments for the ContainerExec job")
		execStatus.finishTimestamp = metav1.NowMicro()
		execStatus.state = apiv1.ExecutableStateFailedToStart

		return updateContainerExecStatus(exec, execStatus)
	}

	stdOutFile, err := usvc_io.OpenTempFile(fmt.Sprintf("%s_out_%s", exec.Name, exec.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		log.Error(err, "failed to create temporary file for capturing process standard output data")
	} else {
		execStatus.stdOutFile = stdOutFile.Name()
	}

	stdErrFile, err := usvc_io.OpenTempFile(fmt.Sprintf("%s_err_%s", exec.Name, exec.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		log.Error(err, "failed to create temporary file for capturing process standard error data")
	} else {
		execStatus.stdErrFile = stdErrFile.Name()
	}

	execStatus.effectiveArgs = effectiveArgs
	execStatus.effectiveEnv = effectiveEnv

	options := containers.ExecContainerOptions{
		Container:        container.Status.ContainerID,
		WorkingDirectory: exec.Spec.WorkingDirectory,
		Env:              effectiveEnv,
		EnvFiles:         exec.Spec.EnvFiles,
		Command:          exec.Spec.Command,
		Args:             effectiveArgs,
		StreamCommandOptions: containers.StreamCommandOptions{
			// Always append timestamp to logs; we'll strip them out if the streaming request doesn't ask for them
			StdOutStream: usvc_io.NewParagraphWriter(usvc_io.NewTimestampWriter(stdOutFile), osutil.LineSep()),
			StdErrStream: usvc_io.NewParagraphWriter(usvc_io.NewTimestampWriter(stdErrFile), osutil.LineSep()),
		},
	}

	startupTime := metav1.NowMicro()

	execContext, execCancel := context.WithCancel(r.lifetimeCtx)

	execStatus.cancel = execCancel
	execChan, execErr := r.orchestrator.ExecContainer(execContext, options)
	if execErr != nil {
		// We failed to start execution, so mark the status failed
		log.Error(execErr, "failed to run ContainerExec job in container")
		execStatus.state = apiv1.ExecutableStateFailedToStart
		execStatus.finishTimestamp = metav1.NowMicro()

		return updateContainerExecStatus(exec, execStatus)
	}

	execStatus.state = apiv1.ExecutableStateRunning
	execStatus.startupTimestamp = startupTime

	// Start a goroutine to monitor the execution
	go func() {
		select {
		case <-r.lifetimeCtx.Done():
			// We're exiting, so there's nothing to do
			return
		case exitCode := <-execChan:
			if r.lifetimeCtx.Err() != nil {
				// We're exiting, so there's nothing to do
				return
			}

			finishTimestamp := metav1.NowMicro()

			execStatus.mutex.Lock()
			defer execStatus.mutex.Unlock()

			execStatus.exitCode = &exitCode
			execStatus.state = apiv1.ExecutableStateFinished
			execStatus.finishTimestamp = finishTimestamp

			r.Log.V(1).Info("detected exec command completion, scheduling reconciliation for ContainerExec object", "ContainerExec", exec.Name)
			r.debouncer.ReconciliationNeeded(r.lifetimeCtx, exec.NamespacedName(), string(exec.UID), r.scheduleReconciliation)
		}
	}()

	return updateContainerExecStatus(exec, execStatus)
}

func (r *ContainerExecReconciler) scheduleReconciliation(rti reconcileTriggerInput[string]) {
	event := ctrl_event.GenericEvent{
		Object: &apiv1.Container{
			ObjectMeta: metav1.ObjectMeta{
				Name:      rti.target.Name,
				Namespace: rti.target.Namespace,
			},
		},
	}
	r.notifyExecChanged.In <- event
}

func (r *ContainerExecReconciler) computeEffectiveEnvironment(
	ctx context.Context,
	exec *apiv1.ContainerExec,
	ctr *apiv1.Container,
	log logr.Logger,
) ([]apiv1.EnvVar, error) {
	// Note: there is no value substitution by DCP for .env files, these are handled by container orchestrator directly.
	effectiveEnv := []apiv1.EnvVar{}

	tmpl, err := templating.NewSpecValueTemplate(ctx, r, ctr, nil, log)
	if err != nil {
		return effectiveEnv, err
	}

	for _, envVar := range exec.Spec.Env {
		substitutionCtx := fmt.Sprintf("environment variable %s", envVar.Name)
		effectiveValue, templateErr := templating.ExecuteTemplate(tmpl, ctr, envVar.Value, substitutionCtx, log)
		if templateErr != nil {
			return effectiveEnv, templateErr
		}

		effectiveEnv = append(effectiveEnv, apiv1.EnvVar{Name: envVar.Name, Value: effectiveValue})
	}

	return effectiveEnv, nil
}

func (r *ContainerExecReconciler) computeEffectiveInvocationArgs(
	ctx context.Context,
	exec *apiv1.ContainerExec,
	ctr *apiv1.Container,
	log logr.Logger,
) ([]string, error) {
	effectiveArgs := []string{}
	tmpl, err := templating.NewSpecValueTemplate(ctx, r, ctr, nil, log)
	if err != nil {
		return effectiveArgs, err
	}

	for i, arg := range exec.Spec.Args {
		substitutionCtx := fmt.Sprintf("argument %d", i)
		effectiveValue, templateErr := templating.ExecuteTemplate(tmpl, ctr, arg, substitutionCtx, log)
		if templateErr != nil {
			return effectiveArgs, templateErr
		}

		effectiveArgs = append(effectiveArgs, effectiveValue)
	}

	return effectiveArgs, nil
}

func (r *ContainerExecReconciler) releaseContainerExecResources(exec *apiv1.ContainerExec, log logr.Logger) {
	r.stopContainerExec(exec, log)

	// We are about to terminate the run. Since the run is not allowed to complete normally,
	// we are not interested in its exit code (it will indicate a failure,
	// but it is a failure induced by the Executable user), so we stop tracking the run now.
	r.executions.Delete(exec.UID)
	r.deleteOutputFiles(exec, log)
}

func (r *ContainerExecReconciler) stopContainerExec(exec *apiv1.ContainerExec, log logr.Logger) {
	execStatus, found := r.executions.Load(exec.UID)
	if !found {
		log.V(1).Info("run data is not available, nothing to stop")
		return
	}

	execStatus.mutex.Lock()
	defer execStatus.mutex.Unlock()

	// Cancel the execution
	execStatus.cancel()
}

func (r *ContainerExecReconciler) deleteOutputFiles(exec *apiv1.ContainerExec, log logr.Logger) {
	// Do not bother updating the ContainerExec object--this method is called when the object is being deleted.

	if osutil.EnvVarSwitchEnabled(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS) {
		return
	}

	if exec.Status.StdOutFile != "" {
		path := exec.Status.StdOutFile
		_ = r.stopQueue.Enqueue(func(opCtx context.Context) { // Only errors if lifetimeCtx is done
			if err := logs.RemoveWithRetry(opCtx, path); err != nil {
				log.Error(err, "could not remove process's standard output file", "path", path)
			}
		})
	}

	if exec.Status.StdErrFile != "" {
		path := exec.Status.StdErrFile
		_ = r.stopQueue.Enqueue(func(opCtx context.Context) {
			if err := logs.RemoveWithRetry(opCtx, path); err != nil {
				log.Error(err, "could not remove process's standard error file", "path", path)
			}
		})
	}
}

func updateContainerExecStatus(exec *apiv1.ContainerExec, execStatus *runningContainerExecStatus) objectChange {
	change := noChange

	if exec.Status.State != execStatus.state {
		exec.Status.State = execStatus.state
		change = statusChanged
	}

	if exec.Status.ExitCode == nil && execStatus.exitCode != nil {
		exec.Status.ExitCode = execStatus.exitCode
		change = statusChanged
	}

	if exec.Status.StartupTimestamp.IsZero() && !execStatus.startupTimestamp.IsZero() {
		exec.Status.StartupTimestamp = execStatus.startupTimestamp
		change = statusChanged
	}

	if exec.Status.FinishTimestamp.IsZero() && !execStatus.finishTimestamp.IsZero() {
		exec.Status.FinishTimestamp = execStatus.finishTimestamp
		change = statusChanged
	}

	if !slices.Equal(exec.Status.EffectiveArgs, execStatus.effectiveArgs) {
		exec.Status.EffectiveArgs = execStatus.effectiveArgs
		change = statusChanged
	}

	if !slices.Equal(exec.Status.EffectiveEnv, execStatus.effectiveEnv) {
		exec.Status.EffectiveEnv = execStatus.effectiveEnv
		change = statusChanged
	}

	if exec.Status.StdOutFile != execStatus.stdOutFile {
		exec.Status.StdOutFile = execStatus.stdOutFile
		change = statusChanged
	}

	if exec.Status.StdErrFile != execStatus.stdErrFile {
		exec.Status.StdErrFile = execStatus.stdErrFile
		change = statusChanged
	}

	return change
}
