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
	physicalContainerExecStateTarget:         handlePhysicalContainerExecTarget,
	physicalContainerExecStateTargetNotFound: handlePhysicalContainerExecTarget,
	physicalContainerExecStateTargetRetry:    handlePhysicalContainerExecTarget,
	physicalContainerExecStateLaunch:         handlePhysicalContainerExecLaunch,
	physicalContainerExecStateLaunchRetry:    handlePhysicalContainerExecLaunch,
	physicalContainerExecStateRunning:        handlePhysicalContainerExecRuntime,
	physicalContainerExecStateExited:         handlePhysicalContainerExecTerminal,
	physicalContainerExecStateFailed:         handlePhysicalContainerExecTerminal,
	physicalContainerExecStateStop:           handlePhysicalContainerExecStop,
	physicalContainerExecStateInvalid:        handlePhysicalContainerExecTerminal,
	0:                                        handleInvalidPhysicalContainerExecState,
}

// PhysicalContainerExecReconciler owns attached runtime execution and output capture.
type PhysicalContainerExecReconciler struct {
	*ReconcilerBase[apiv2.PhysicalContainerExec, *apiv2.PhysicalContainerExec]
	orchestrator       containers.ExecContainers
	executions         *ObjectStateMap[types.UID, physicalContainerExecData, *physicalContainerExecData, *apiv2.PhysicalContainerExec]
	operationQueue     *resiliency.WorkQueue
	outputCleanupQueue *resiliency.WorkQueue
}

func NewPhysicalContainerExecReconciler(
	lifetimeCtx context.Context, client ctrl_client.Client, reader ctrl_client.Reader, log logr.Logger,
	orchestrator containers.ExecContainers,
	outputCleanupQueue *resiliency.WorkQueue,
) *PhysicalContainerExecReconciler {
	return &PhysicalContainerExecReconciler{
		ReconcilerBase:     NewReconcilerBase[apiv2.PhysicalContainerExec](client, reader, log, lifetimeCtx),
		orchestrator:       orchestrator,
		executions:         NewObjectStateMap[types.UID, physicalContainerExecData, *physicalContainerExecData, *apiv2.PhysicalContainerExec](),
		operationQueue:     resiliency.NewWorkQueue(lifetimeCtx, MaxConcurrentReconciles),
		outputCleanupQueue: outputCleanupQueue,
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
	patch := ctrl_client.MergeFromWithOptions(resource.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	delay := StandardDelay
	deleting := !resource.DeletionTimestamp.IsZero()
	if deleting && data != nil && data.session != nil {
		data.session.cancel()
	}
	if deleting && !data.operationInProgress() {
		change, delay = r.handleExecutionDeletion(&resource, data, log)
	} else {
		change = ensureFinalizer(&resource, physicalContainerExecFinalizer, log)
		if change == noChange {
			change, delay = r.managePhysicalContainerExec(ctx, &resource, data, log)
		}
	}
	if !hasFinalizer(&resource, physicalContainerExecFinalizer) && !hasFinalizer(&resource, LogicalBackingFinalizer) {
		defer logger.ReleaseResourceLog(resource.GetResourceId())
	}
	return r.SaveChangesWithDelay(ctx, &resource, patch, change, delay, nil, log)
}

func (r *PhysicalContainerExecReconciler) managePhysicalContainerExec(
	ctx context.Context, resource *apiv2.PhysicalContainerExec, data *physicalContainerExecData, log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	if data == nil {
		ready, reason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, resource.Namespace)
		if !ready {
			change := noChange
			message := namespaceReadinessMessage(resource.Namespace, reason)
			if namespaceErr != nil {
				log.Error(namespaceErr, "Failed to get execution namespace")
				message = namespaceErr.Error()
				change |= additionalReconciliationNeeded
			}
			change |= setValue(&resource.Status.Phase, apiv2.ContainerExecPhasePending)
			change |= setCondition(&resource.Status.Conditions, apiv2.ConditionReady, resource.Generation, metav1.ConditionFalse, reason, message)
			return change, StandardDelay
		}
		data = &physicalContainerExecData{state: physicalContainerExecStateTarget}
		r.executions.Store(resource.NamespacedName(), resource.UID, data.Clone())
	}
	stateHandler := getStateInitializer(physicalContainerExecDataHandlers, data.state, log)
	change := stateHandler(ctx, r, resource, data.state, data, log)
	_ = r.executions.Update(resource.NamespacedName(), resource.UID, data)
	projectionChange, delay, valid := data.applyTo(resource)
	if !valid {
		log.Error(fmt.Errorf("invalid physical container execution state %v", data.state), "PhysicalContainerExec reached invalid reconciliation state")
	}
	return change | projectionChange, delay
}

func handlePhysicalContainerExecTarget(
	ctx context.Context, reconciler *PhysicalContainerExecReconciler, resource *apiv2.PhysicalContainerExec,
	_ physicalContainerExecState, data *physicalContainerExecData, log logr.Logger,
) objectChange {
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	data.state, data.reason, data.message = physicalContainerExecStateTarget, "", ""
	var container apiv2.PhysicalContainer
	getErr := reconciler.Get(ctx, commonapi.AsNamespacedName(resource.Spec.ContainerRef, resource.Namespace), &container)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) && data.targetUID != "" {
			data.fail("ContainerUnavailable", "The selected PhysicalContainer no longer exists.")
			return noChange
		}
		data.state = physicalContainerExecStateTargetNotFound
		data.reason, data.message = "ContainerNotFound", "Waiting for the target PhysicalContainer."
		if !apierrors.IsNotFound(getErr) {
			log.Error(getErr, "Failed to get execution target")
			data.reason, data.message = "ContainerLookupFailed", getErr.Error()
			data.state = physicalContainerExecStateTargetRetry
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
	switch data.state {
	case physicalContainerExecStateLaunch:
		return noChange
	case physicalContainerExecStateLaunchRetry:
		data.state = physicalContainerExecStateTarget
		return handlePhysicalContainerExecTarget(ctx, reconciler, resource, data.state, data, log)
	default:
		return handleInvalidPhysicalContainerExecState(ctx, reconciler, resource, state, data, log)
	}
}

func handlePhysicalContainerExecRuntime(
	ctx context.Context, reconciler *PhysicalContainerExecReconciler, resource *apiv2.PhysicalContainerExec,
	state physicalContainerExecState, data *physicalContainerExecData, log logr.Logger,
) objectChange {
	switch data.state {
	case physicalContainerExecStateRunning:
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
	if data.state == physicalContainerExecStateStop && data.session != nil {
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
	stateErr := fmt.Errorf("invalid physical container execution state %v", data.state)
	log.Error(stateErr, "PhysicalContainerExec reached invalid reconciliation state")
	data.fail(apiv2.PhysicalResourceReasonOperationStateInvalid, stateErr.Error())
	data.state = physicalContainerExecStateInvalid
	return noChange
}

func (r *PhysicalContainerExecReconciler) handleExecutionDeletion(
	resource *apiv2.PhysicalContainerExec, data *physicalContainerExecData, log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	if data == nil {
		if !hasFinalizer(resource, physicalContainerExecFinalizer) {
			return noChange, StandardDelay
		}
		data = &physicalContainerExecData{
			state: physicalContainerExecStateExited, reason: "ExecutionCancelled",
			message: "Execution was cancelled before launch.", finishedAt: metav1.NowMicro(),
		}
		r.executions.Store(resource.NamespacedName(), resource.UID, data.Clone())
	}
	if data.session != nil {
		data.session.cancel()
		data.state = physicalContainerExecStateStop
		data.workInProgress = true
		data.reason, data.message = "Stopping", "Waiting for attached execution cancellation to complete."
		_ = r.executions.Update(resource.NamespacedName(), resource.UID, data)
		change, delay, _ := data.applyTo(resource)
		return change, delay
	}
	if data.state != physicalContainerExecStateExited && data.state != physicalContainerExecStateFailed &&
		data.state != physicalContainerExecStateInvalid {
		data.state, data.reason, data.message = physicalContainerExecStateExited, "ExecutionCancelled", "Execution was cancelled before launch."
		data.finishedAt = metav1.NowMicro()
	}
	_ = r.executions.Update(resource.NamespacedName(), resource.UID, data)
	change, _, valid := data.applyTo(resource)
	if !valid || change&statusChanged != 0 {
		return change | additionalReconciliationNeeded, StandardDelay
	}
	closeErr := data.closeOutputLogs(logs.CaptureFileCloseDefault)
	if closeErr != nil {
		log.Error(closeErr, "Failed to close execution output capture")
	}
	if !hasFinalizer(resource, LogicalBackingFinalizer) {
		enqueueCaptureFileCleanup(r.outputCleanupQueue, log, data.stdout, data.stderr)
	}
	return deleteFinalizer(resource, physicalContainerExecFinalizer, log), StandardDelay
}

func (data *physicalContainerExecData) fail(reason apiv2.ConditionReason, message string) {
	data.state, data.reason, data.message = physicalContainerExecStateFailed, reason, message
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
	data.state = physicalContainerExecStateLaunch
	data.workInProgress = true
	data.reason, data.message = "Launching", "Attached command launch is in progress."
	_ = r.executions.Update(resource.NamespacedName(), resource.UID, data)
	snapshot, result := resource.DeepCopy(), data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(context.Context) {
		r.launchExecution(execCtx, snapshot, result, log)
	})
	if enqueueErr != nil {
		cancel()
		data.session = nil
		data.workInProgress = false
		data.state = physicalContainerExecStateLaunchRetry
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
	ready, namespaceReason, namespaceErr := checkNamespaceReadiness(ctx, r.NoCacheClient, resource.Namespace)
	if !ready {
		if namespaceErr == nil {
			namespaceErr = fmt.Errorf("%s", namespaceReadinessMessage(resource.Namespace, namespaceReason))
		}
		r.retryExecution(resource, data, physicalContainerExecStateTargetRetry, namespaceReason, namespaceErr, log)
		return
	}
	var current apiv2.PhysicalContainer
	targetErr := r.NoCacheClient.Get(ctx, commonapi.AsNamespacedName(resource.Spec.ContainerRef, resource.Namespace), &current)
	condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
	if targetErr != nil && !apierrors.IsNotFound(targetErr) {
		r.retryExecution(resource, data, physicalContainerExecStateTargetRetry, "ContainerLookupFailed", targetErr, log)
		return
	}
	if targetErr != nil || !data.matchesTarget(&current) || !current.DeletionTimestamp.IsZero() ||
		current.Status.Phase == apiv2.PhysicalContainerPhaseExited || current.Status.Phase == apiv2.PhysicalContainerPhaseFailed {
		fail("ContainerUnavailable", fmt.Errorf("target container changed or became unavailable before execution launch: %v", targetErr))
		return
	}
	if current.Status.ContainerID == "" || current.Status.Phase != apiv2.PhysicalContainerPhaseRunning ||
		condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != current.Generation {
		r.retryExecution(resource, data, physicalContainerExecStateTargetRetry, "ContainerNotReady", fmt.Errorf("target container is not ready for execution"), log)
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
	data.state = physicalContainerExecStateRunning
	data.workInProgress = false
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
			data.state, data.reason, data.message = physicalContainerExecStateExited, "ExecutionExited", "Attached runtime execution has completed."
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
	data.workInProgress = false
	data.state = state
	data.reason, data.message = reason, failure.Error()
	data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
	r.queueExecutionResult(resource, data)
}

func (r *PhysicalContainerExecReconciler) finishExecution(
	resource *apiv2.PhysicalContainerExec, data *physicalContainerExecData, log logr.Logger,
) {
	data.session.cancel()
	data.session = nil
	data.workInProgress = false
	if data.stdout != nil {
		data.stdout.CloseAndLog(logs.CaptureFileCloseDefault, log)
	}
	if data.stderr != nil {
		data.stderr.CloseAndLog(logs.CaptureFileCloseDefault, log)
	}
	if !r.queueExecutionResult(resource, data) {
		enqueueCaptureFileCleanup(r.outputCleanupQueue, log, data.stdout, data.stderr)
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
			enqueueCaptureFileCleanup(r.outputCleanupQueue, log, data.stdout, data.stderr)
		}
		logger.ReleaseResourceLog((&apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{UID: uid}}).GetResourceId())
	}
	r.executions.DeleteByNamespacedName(name)
}
