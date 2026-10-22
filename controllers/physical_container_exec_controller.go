/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/internal/logstream"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

const physicalContainerExecRefField = "spec.containerRef"

var physicalContainerExecFinalizer = apiv2.GroupName + "/physicalcontainerexec-reconciler"

type physicalContainerExecDataHandlerFunc = stateInitializerFunc[
	apiv2.PhysicalContainerExec, *apiv2.PhysicalContainerExec,
	PhysicalContainerExecReconciler, *PhysicalContainerExecReconciler,
	physicalContainerExecState, physicalContainerExecData, *physicalContainerExecData,
]

var physicalContainerExecDataHandlers = map[physicalContainerExecState]physicalContainerExecDataHandlerFunc{
	physicalContainerExecStateNamespace: handlePhysicalContainerExecNamespace,
	physicalContainerExecStateTarget:    handlePhysicalContainerExecTarget,
	physicalContainerExecStateLaunch:    handlePhysicalContainerExecLaunch,
	physicalContainerExecStateRuntime:   handlePhysicalContainerExecRuntime,
	physicalContainerExecStateStop:      handlePhysicalContainerExecStop,
	physicalContainerExecStateInvalid:   handlePhysicalContainerExecTerminal,
	0:                                   handleInvalidPhysicalContainerExecState,
}

// PhysicalContainerExecReconciler owns attached runtime execution and output capture.
type PhysicalContainerExecReconciler struct {
	*ReconcilerBase[apiv2.PhysicalContainerExec, *apiv2.PhysicalContainerExec]
	orchestrator   containers.ExecContainers
	executions     *ObjectStateMap[types.UID, physicalContainerExecData, *physicalContainerExecData, *apiv2.PhysicalContainerExec]
	operationQueue *resiliency.WorkQueue
}

func NewPhysicalContainerExecReconciler(
	lifetimeCtx context.Context, client ctrl_client.Client, reader ctrl_client.Reader, log logr.Logger,
	orchestrator containers.ExecContainers,
) *PhysicalContainerExecReconciler {
	return &PhysicalContainerExecReconciler{
		ReconcilerBase: NewReconcilerBase[apiv2.PhysicalContainerExec](client, reader, log, lifetimeCtx),
		orchestrator:   orchestrator,
		executions:     NewObjectStateMap[types.UID, physicalContainerExecData, *physicalContainerExecData, *apiv2.PhysicalContainerExec](),
		operationQueue: resiliency.NewWorkQueue(lifetimeCtx, MaxConcurrentReconciles),
	}
}

func (r *PhysicalContainerExecReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, &apiv2.PhysicalContainerExec{}, physicalContainerExecRefField, func(obj ctrl_client.Object) []string {
		execution := obj.(*apiv2.PhysicalContainerExec)
		return []string{commonapi.AsNamespacedName(execution.Spec.ContainerRef, execution.Namespace).Name}
	})
	if indexErr != nil {
		return fmt.Errorf("index physical container execution references: %w", indexErr)
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.PhysicalContainerExec{}).
		Watches(&apiv2.PhysicalContainer{}, handler.EnqueueRequestsFromMapFunc(r.mapContainer)).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.PhysicalContainerExecList{}))).
		WatchesRawSource(r.GetReconciliationEventSource()).Named(name).Complete(r)
}

func (r *PhysicalContainerExecReconciler) mapContainer(ctx context.Context, obj ctrl_client.Object) []ctrl.Request {
	var executions apiv2.PhysicalContainerExecList
	listErr := r.List(ctx, &executions, ctrl_client.InNamespace(obj.GetNamespace()), ctrl_client.MatchingFields{physicalContainerExecRefField: obj.GetName()})
	if listErr != nil {
		r.Log.Error(listErr, "Failed to list physical container executions", "Container", ctrl_client.ObjectKeyFromObject(obj))
		return nil
	}
	requests := make([]ctrl.Request, len(executions.Items))
	for i := range executions.Items {
		requests[i] = ctrl.Request{NamespacedName: executions.Items[i].NamespacedName()}
	}
	return requests
}

func (r *PhysicalContainerExecReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)
	if ctx.Err() != nil {
		return ctrl.Result{}, nil
	}
	var resource apiv2.PhysicalContainerExec
	getErr := reader.Get(ctx, req.NamespacedName, &resource)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			r.discardExecution(req.NamespacedName, log)
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to get PhysicalContainerExec")
		return ctrl.Result{}, getErr
	}
	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, resource.GetResourceId())
	if owner := metav1.GetControllerOf(&resource); owner != nil &&
		owner.APIVersion == apiv2.GroupVersion.String() && owner.Kind == "LogicalContainerExec" {
		logical := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{UID: owner.UID}}
		forwarded, forwardErr := logger.WithAdditionalResourceStream(log, logical.GetResourceId())
		if forwardErr != nil {
			log.Error(forwardErr, "Failed to attach logical execution diagnostics")
		} else {
			log = forwarded
		}
	}
	stateKey, previous := r.executions.BorrowByNamespacedName(req.NamespacedName)
	if previous != nil && stateKey != resource.UID {
		r.discardExecution(req.NamespacedName, log)
	}
	r.executions.RunDeferredOps(req.NamespacedName, &resource)
	_, data := r.executions.BorrowByNamespacedName(req.NamespacedName)
	if data == nil {
		data = &physicalContainerExecData{state: physicalContainerExecStateNamespace, progress: physicalResourceProgressNotReady}
		r.executions.Store(req.NamespacedName, resource.UID, data.Clone())
	}
	patch := ctrl_client.MergeFromWithOptions(resource.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	delay := StandardDelay
	deleting := !resource.DeletionTimestamp.IsZero()
	if deleting && data.session != nil {
		data.session.cancel()
	}
	finalizerAdded := false
	if deleting && !data.operationInProgress() {
		change = r.handleExecutionDeletion(&resource, data, log)
	} else if change = ensureFinalizer(&resource, physicalContainerExecFinalizer, log); change != noChange {
		finalizerAdded = true
	} else {
		stateHandler := getStateInitializer(physicalContainerExecDataHandlers, data.state, log)
		change = stateHandler(ctx, r, &resource, data.state, data, log)
	}
	if !finalizerAdded && hasFinalizer(&resource, physicalContainerExecFinalizer) {
		_ = r.executions.Update(req.NamespacedName, resource.UID, data)
		projectionChange, projectionDelay, valid := data.applyTo(&resource)
		change |= projectionChange
		delay = projectionDelay
		if !valid {
			log.Error(fmt.Errorf("invalid physical container execution state %v with progress %v", data.state, data.progress),
				"PhysicalContainerExec reached invalid reconciliation state")
		}
	}
	if !hasFinalizer(&resource, physicalContainerExecFinalizer) {
		defer logger.ReleaseResourceLog(resource.GetResourceId())
	}
	return r.SaveChangesWithDelay(ctx, &resource, patch, change, delay, nil, log)
}

func handlePhysicalContainerExecNamespace(
	ctx context.Context, reconciler *PhysicalContainerExecReconciler, resource *apiv2.PhysicalContainerExec,
	_ physicalContainerExecState, data *physicalContainerExecData, log logr.Logger,
) objectChange {
	if data.progress == physicalResourceProgressFailed {
		return noChange
	}
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	ready, reason, namespaceErr := checkNamespaceReady(ctx, reconciler.Client, resource.Namespace)
	if !ready {
		data.state, data.progress = physicalContainerExecStateNamespace, physicalResourceProgressNotReady
		data.reason, data.message = reason, namespaceReadinessMessage(resource.Namespace, reason)
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get execution namespace")
			data.progress = physicalResourceProgressRetryPending
			data.message = namespaceErr.Error()
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		}
		return noChange
	}
	data.state, data.progress = physicalContainerExecStateTarget, physicalResourceProgressNotReady
	return handlePhysicalContainerExecTarget(ctx, reconciler, resource, data.state, data, log)
}

func handlePhysicalContainerExecTarget(
	ctx context.Context, reconciler *PhysicalContainerExecReconciler, resource *apiv2.PhysicalContainerExec,
	_ physicalContainerExecState, data *physicalContainerExecData, log logr.Logger,
) objectChange {
	if data.progress == physicalResourceProgressFailed {
		return noChange
	}
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	data.progress, data.reason, data.message = physicalResourceProgressNotReady, "", ""
	var container apiv2.PhysicalContainer
	getErr := reconciler.Get(ctx, commonapi.AsNamespacedName(resource.Spec.ContainerRef, resource.Namespace), &container)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) && data.targetUID != "" {
			data.fail("ContainerUnavailable", "The selected PhysicalContainer no longer exists.")
			return noChange
		}
		data.progress = physicalResourceProgressNotFound
		data.reason, data.message = "ContainerNotFound", "Waiting for the target PhysicalContainer."
		if !apierrors.IsNotFound(getErr) {
			log.Error(getErr, "Failed to get execution target")
			data.reason, data.message = "ContainerLookupFailed", getErr.Error()
			data.progress = physicalResourceProgressRetryPending
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		}
		return noChange
	}
	if !data.matchesTarget(&container) {
		data.fail("ContainerUnavailable", "The selected PhysicalContainer identity changed.")
		return noChange
	}
	if !container.DeletionTimestamp.IsZero() {
		data.fail("ContainerUnavailable", "The target PhysicalContainer is being deleted.")
		return noChange
	}
	condition := apimeta.FindStatusCondition(container.Status.Conditions, string(apiv2.ConditionReady))
	if condition != nil && condition.ObservedGeneration == container.Generation {
		if container.Status.Phase == apiv2.PhysicalContainerPhaseExited || container.Status.Phase == apiv2.PhysicalContainerPhaseFailed {
			data.fail("ContainerUnavailable", "The target PhysicalContainer has exited or failed.")
			return noChange
		}
		if condition.Status == metav1.ConditionTrue && container.Status.Phase == apiv2.PhysicalContainerPhaseRunning && container.Status.ContainerID != "" {
			return reconciler.scheduleExecution(resource, &container, data, log)
		}
	}
	data.reason, data.message = "ContainerNotReady", "Waiting for the target PhysicalContainer to be running."
	return noChange
}

func handlePhysicalContainerExecLaunch(
	ctx context.Context, reconciler *PhysicalContainerExecReconciler, resource *apiv2.PhysicalContainerExec,
	state physicalContainerExecState, data *physicalContainerExecData, log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalResourceProgressInProgress, physicalResourceProgressFailed:
		return noChange
	case physicalResourceProgressRetryPending:
		data.state, data.progress = physicalContainerExecStateTarget, physicalResourceProgressNotReady
		return handlePhysicalContainerExecTarget(ctx, reconciler, resource, data.state, data, log)
	default:
		return handleInvalidPhysicalContainerExecState(ctx, reconciler, resource, state, data, log)
	}
}

func handlePhysicalContainerExecRuntime(
	ctx context.Context, reconciler *PhysicalContainerExecReconciler, resource *apiv2.PhysicalContainerExec,
	state physicalContainerExecState, data *physicalContainerExecData, log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalResourceProgressExited, physicalResourceProgressFailed:
		return noChange
	case physicalResourceProgressRunning:
		if data.session != nil {
			return noChange
		}
	}
	return handleInvalidPhysicalContainerExecState(ctx, reconciler, resource, state, data, log)
}

func handlePhysicalContainerExecStop(
	ctx context.Context, reconciler *PhysicalContainerExecReconciler, resource *apiv2.PhysicalContainerExec,
	state physicalContainerExecState, data *physicalContainerExecData, log logr.Logger,
) objectChange {
	if data.progress == physicalResourceProgressInProgress && data.session != nil {
		return noChange
	}
	return handleInvalidPhysicalContainerExecState(ctx, reconciler, resource, state, data, log)
}

func handlePhysicalContainerExecTerminal(
	_ context.Context, _ *PhysicalContainerExecReconciler, _ *apiv2.PhysicalContainerExec,
	_ physicalContainerExecState, _ *physicalContainerExecData, _ logr.Logger,
) objectChange {
	return noChange
}

func handleInvalidPhysicalContainerExecState(
	_ context.Context, _ *PhysicalContainerExecReconciler, _ *apiv2.PhysicalContainerExec,
	_ physicalContainerExecState, data *physicalContainerExecData, log logr.Logger,
) objectChange {
	stateErr := fmt.Errorf("invalid physical container execution state %v with progress %v", data.state, data.progress)
	log.Error(stateErr, "PhysicalContainerExec reached invalid reconciliation state")
	data.state = physicalContainerExecStateInvalid
	data.fail(apiv2.PhysicalResourceReasonOperationStateInvalid, stateErr.Error())
	return noChange
}

func (r *PhysicalContainerExecReconciler) handleExecutionDeletion(
	resource *apiv2.PhysicalContainerExec, data *physicalContainerExecData, log logr.Logger,
) objectChange {
	if data.session != nil {
		data.session.cancel()
		data.state, data.progress = physicalContainerExecStateStop, physicalResourceProgressInProgress
		data.reason, data.message = "Stopping", "Waiting for attached execution cancellation to complete."
		return noChange
	}
	r.discardExecution(resource.NamespacedName(), log)
	return deleteFinalizer(resource, physicalContainerExecFinalizer, log)
}

func (data *physicalContainerExecData) fail(reason apiv2.ConditionReason, message string) {
	data.progress, data.reason, data.message = physicalResourceProgressFailed, reason, message
	data.finishedAt = metav1.NowMicro()
}

func (r *PhysicalContainerExecReconciler) scheduleExecution(
	resource *apiv2.PhysicalContainerExec, container *apiv2.PhysicalContainer, data *physicalContainerExecData, log logr.Logger,
) objectChange {
	execCtx, cancel := context.WithCancel(r.LifetimeCtx)
	if data.targetUID == "" {
		data.targetUID, data.containerID = container.UID, container.Status.ContainerID
	}
	data.session = &physicalContainerExecSession{cancel: cancel}
	data.state, data.progress = physicalContainerExecStateLaunch, physicalResourceProgressInProgress
	data.reason, data.message = "Launching", "Attached command launch is in progress."
	_ = r.executions.Update(resource.NamespacedName(), resource.UID, data)
	snapshot, result := resource.DeepCopy(), data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(context.Context) {
		r.launchExecution(execCtx, snapshot, result, log)
	})
	if enqueueErr != nil {
		cancel()
		data.session = nil
		data.progress = physicalResourceProgressRetryPending
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		data.reason, data.message = "LaunchQueueFailed", enqueueErr.Error()
		log.Error(enqueueErr, "Failed to queue attached execution launch")
		return noChange
	}
	return noChange
}

func (r *PhysicalContainerExecReconciler) launchExecution(
	ctx context.Context, resource *apiv2.PhysicalContainerExec, data *physicalContainerExecData, log logr.Logger,
) {
	fail := func(reason apiv2.ConditionReason, failure error) {
		log.Error(failure, "Failed to launch container execution")
		data.fail(reason, failure.Error())
		r.finishExecution(resource, data, log)
	}
	ready, namespaceReason, namespaceErr := checkNamespaceReady(ctx, r.NoCacheClient, resource.Namespace)
	if namespaceErr != nil {
		r.retryExecution(resource, data, physicalContainerExecStateNamespace, namespaceReason, namespaceErr, log)
		return
	}
	if !ready {
		r.retryExecution(resource, data, physicalContainerExecStateNamespace, namespaceReason, fmt.Errorf("%s", namespaceReadinessMessage(resource.Namespace, namespaceReason)), log)
		return
	}
	var current apiv2.PhysicalContainer
	targetErr := r.NoCacheClient.Get(ctx, commonapi.AsNamespacedName(resource.Spec.ContainerRef, resource.Namespace), &current)
	condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
	if targetErr != nil && !apierrors.IsNotFound(targetErr) {
		r.retryExecution(resource, data, physicalContainerExecStateTarget, "ContainerLookupFailed", targetErr, log)
		return
	}
	if targetErr != nil || !data.matchesTarget(&current) || !current.DeletionTimestamp.IsZero() ||
		current.Status.Phase == apiv2.PhysicalContainerPhaseExited || current.Status.Phase == apiv2.PhysicalContainerPhaseFailed {
		fail("ContainerUnavailable", fmt.Errorf("target container changed or became unavailable before execution launch: %v", targetErr))
		return
	}
	if current.Status.ContainerID == "" || current.Status.Phase != apiv2.PhysicalContainerPhaseRunning ||
		condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != current.Generation {
		r.retryExecution(resource, data, physicalContainerExecStateTarget, "ContainerNotReady", fmt.Errorf("target container is not ready for execution"), log)
		return
	}
	data.retryAfter = time.Time{}
	stdout, stdoutErr := logs.NewCaptureFile(resource, logstream.LogStreamSourceStdout)
	if stdoutErr != nil {
		fail("CaptureFailed", stdoutErr)
		return
	}
	data.stdout = stdout
	stderr, stderrErr := logs.NewCaptureFile(resource, logstream.LogStreamSourceStderr)
	if stderrErr != nil {
		fail("CaptureFailed", stderrErr)
		return
	}
	data.stderr = stderr
	config := resource.Spec.Execution
	exitCodes, launchErr := r.orchestrator.ExecContainer(ctx, containers.ExecContainerOptions{
		Container: data.containerID, Command: config.Command, Args: config.Args,
		WorkingDirectory: config.WorkingDirectory, Env: config.Env, EnvFiles: config.EnvFiles,
		StreamCommandOptions: containers.StreamCommandOptions{StdOutStream: stdout.Writer, StdErrStream: stderr.Writer},
	})
	if launchErr != nil {
		fail("LaunchFailed", launchErr)
		return
	}
	if exitCodes == nil {
		fail("LaunchFailed", fmt.Errorf("runtime returned no execution completion channel"))
		return
	}
	data.state, data.progress = physicalContainerExecStateRuntime, physicalResourceProgressRunning
	data.reason, data.message = "ExecutionRunning", "Attached runtime execution is running."
	data.startedAt = metav1.NowMicro()
	r.queueExecutionResult(resource, data.Clone())
	go func() {
		exitCode, valid := <-exitCodes
		if valid && exitCode != process.UnknownExitCode {
			data.exitCode = &exitCode
		}
		if !valid {
			completionErr := fmt.Errorf("runtime execution completion channel closed without a result")
			log.Error(completionErr, "Container execution completion was not reported")
			data.fail("CompletionUnknown", completionErr.Error())
		} else {
			data.progress, data.reason, data.message = physicalResourceProgressExited, "ExecutionExited", "Attached runtime execution has completed."
			data.finishedAt = metav1.NowMicro()
		}
		r.finishExecution(resource, data, log)
	}()
}

func (r *PhysicalContainerExecReconciler) retryExecution(
	resource *apiv2.PhysicalContainerExec, data *physicalContainerExecData, state physicalContainerExecState,
	reason apiv2.ConditionReason, failure error, log logr.Logger,
) {
	log.Error(failure, "Could not establish execution prerequisites; launch will be retried")
	data.session.cancel()
	data.session = nil
	data.state, data.progress = state, physicalResourceProgressRetryPending
	data.reason, data.message = reason, failure.Error()
	data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
	r.queueExecutionResult(resource, data)
}

func (r *PhysicalContainerExecReconciler) finishExecution(
	resource *apiv2.PhysicalContainerExec, data *physicalContainerExecData, log logr.Logger,
) {
	data.session.cancel()
	data.session = nil
	if data.stdout != nil {
		data.stdout.CloseAndLog(logs.CaptureFileCloseDefault, log)
	}
	if data.stderr != nil {
		data.stderr.CloseAndLog(logs.CaptureFileCloseDefault, log)
	}
	if !r.queueExecutionResult(resource, data) {
		if data.stdout != nil {
			data.stdout.CloseAndLog(logs.CaptureFileDisposeOnClose, log)
		}
		if data.stderr != nil {
			data.stderr.CloseAndLog(logs.CaptureFileDisposeOnClose, log)
		}
	}
}

func (r *PhysicalContainerExecReconciler) queueExecutionResult(resource *apiv2.PhysicalContainerExec, result *physicalContainerExecData) bool {
	queued := r.executions.QueueDeferredOpForStateKey(resource.NamespacedName(), resource.UID,
		func(name types.NamespacedName, uid types.UID, _ *apiv2.PhysicalContainerExec) {
			_ = r.executions.Update(name, uid, result)
		})
	if queued {
		r.ScheduleReconciliation(resource.NamespacedName())
	} else if result.session != nil {
		result.session.cancel()
	}
	return queued
}

func (r *PhysicalContainerExecReconciler) discardExecution(name types.NamespacedName, log logr.Logger) {
	uid, data := r.executions.BorrowByNamespacedName(name)
	if data != nil {
		if data.session != nil {
			data.session.cancel()
		} else {
			if data.stdout != nil {
				data.stdout.CloseAndLog(logs.CaptureFileDisposeOnClose, log)
			}
			if data.stderr != nil {
				data.stderr.CloseAndLog(logs.CaptureFileDisposeOnClose, log)
			}
		}
		logger.ReleaseResourceLog((&apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{UID: uid}}).GetResourceId())
	}
	r.executions.DeleteByNamespacedName(name)
}
