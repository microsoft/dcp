/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"fmt"
	"reflect"
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
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
)

const logicalContainerExecRefField = "spec.containerRef"

var logicalContainerExecFinalizer = apiv2.GroupName + "/logicalcontainerexec-reconciler"

type logicalContainerExecDataHandlerFunc = stateInitializerFunc[
	apiv2.LogicalContainerExec, *apiv2.LogicalContainerExec,
	LogicalContainerExecReconciler, *LogicalContainerExecReconciler,
	logicalContainerExecState, logicalContainerExecData, *logicalContainerExecData,
]

var logicalContainerExecDataHandlers = map[logicalContainerExecState]logicalContainerExecDataHandlerFunc{
	logicalContainerExecStateNamespace: handleLogicalContainerExecNamespace,
	logicalContainerExecStateTarget:    handleLogicalContainerExecPrerequisites,
	logicalContainerExecStatePhysical:  handleLogicalContainerExecPhysical,
	logicalContainerExecStateRuntime:   handleLogicalContainerExecRuntime,
	logicalContainerExecStateDelete:    handleLogicalContainerExecDeletion,
	logicalContainerExecStateInvalid:   handleLogicalContainerExecTerminal,
	0:                                  handleInvalidLogicalContainerExecState,
}

// LogicalContainerExecReconciler translates logical target readiness into one physical execution.
type LogicalContainerExecReconciler struct {
	*ReconcilerBase[apiv2.LogicalContainerExec, *apiv2.LogicalContainerExec]
	executions *ObjectStateMap[types.UID, logicalContainerExecData, *logicalContainerExecData, *apiv2.LogicalContainerExec]
}

func NewLogicalContainerExecReconciler(
	lifetimeCtx context.Context, client ctrl_client.Client, reader ctrl_client.Reader, log logr.Logger,
) *LogicalContainerExecReconciler {
	return &LogicalContainerExecReconciler{
		ReconcilerBase: NewReconcilerBase[apiv2.LogicalContainerExec](client, reader, log, lifetimeCtx),
		executions:     NewObjectStateMap[types.UID, logicalContainerExecData, *logicalContainerExecData, *apiv2.LogicalContainerExec](),
	}
}

func (r *LogicalContainerExecReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, &apiv2.LogicalContainerExec{}, logicalContainerExecRefField, func(obj ctrl_client.Object) []string {
		execution := obj.(*apiv2.LogicalContainerExec)
		return []string{commonapi.AsNamespacedName(execution.Spec.ContainerRef, execution.Namespace).Name}
	})
	if indexErr != nil {
		return fmt.Errorf("index logical container execution references: %w", indexErr)
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.LogicalContainerExec{}).Owns(&apiv2.PhysicalContainerExec{}).
		Watches(&apiv2.LogicalContainer{}, handler.EnqueueRequestsFromMapFunc(r.mapContainer)).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.LogicalContainerExecList{}))).
		WatchesRawSource(r.GetReconciliationEventSource()).Named(name).Complete(r)
}

func (r *LogicalContainerExecReconciler) mapContainer(ctx context.Context, obj ctrl_client.Object) []ctrl.Request {
	var executions apiv2.LogicalContainerExecList
	listErr := r.List(ctx, &executions, ctrl_client.InNamespace(obj.GetNamespace()), ctrl_client.MatchingFields{logicalContainerExecRefField: obj.GetName()})
	if listErr != nil {
		r.Log.Error(listErr, "Failed to list logical container executions", "Container", ctrl_client.ObjectKeyFromObject(obj))
		return nil
	}
	requests := make([]ctrl.Request, len(executions.Items))
	for i := range executions.Items {
		requests[i] = ctrl.Request{NamespacedName: executions.Items[i].NamespacedName()}
	}
	return requests
}

func (r *LogicalContainerExecReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)
	if ctx.Err() != nil {
		return ctrl.Result{}, nil
	}
	var resource apiv2.LogicalContainerExec
	getErr := reader.Get(ctx, req.NamespacedName, &resource)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			uid, _ := r.executions.BorrowByNamespacedName(req.NamespacedName)
			r.executions.DeleteByNamespacedName(req.NamespacedName)
			logger.ReleaseResourceLog((&apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{UID: uid}}).GetResourceId())
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to get LogicalContainerExec")
		return ctrl.Result{}, getErr
	}
	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, resource.GetResourceId())
	stateKey, data := r.executions.BorrowByNamespacedName(req.NamespacedName)
	if data == nil || stateKey != resource.UID {
		if data != nil {
			logger.ReleaseResourceLog((&apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{UID: stateKey}}).GetResourceId())
		}
		data = &logicalContainerExecData{state: logicalContainerExecStateNamespace, progress: physicalResourceProgressNotReady}
		r.executions.Store(req.NamespacedName, resource.UID, data.Clone())
	}
	patch := ctrl_client.MergeFromWithOptions(resource.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	delay := StandardDelay
	finalizerAdded := false
	if !resource.DeletionTimestamp.IsZero() {
		change = handleLogicalContainerExecDeletion(ctx, r, &resource, data.state, data, log)
	} else if change = ensureFinalizer(&resource, logicalContainerExecFinalizer, log); change != noChange {
		finalizerAdded = true
	} else {
		stateHandler := getStateInitializer(logicalContainerExecDataHandlers, data.state, log)
		change = stateHandler(ctx, r, &resource, data.state, data, log)
	}
	if !finalizerAdded && hasFinalizer(&resource, logicalContainerExecFinalizer) {
		_ = r.executions.Update(req.NamespacedName, resource.UID, data)
		projectionChange, projectionDelay, valid := data.applyTo(&resource)
		change |= projectionChange
		delay = projectionDelay
		if !valid {
			log.Error(fmt.Errorf("invalid logical container execution state %v with progress %v", data.state, data.progress),
				"LogicalContainerExec reached invalid reconciliation state")
		}
	}
	if !hasFinalizer(&resource, logicalContainerExecFinalizer) {
		r.executions.DeleteByNamespacedName(req.NamespacedName)
		defer logger.ReleaseResourceLog(resource.GetResourceId())
	}
	return r.SaveChangesWithDelay(ctx, &resource, patch, change, delay, nil, log)
}

func handleLogicalContainerExecNamespace(
	ctx context.Context, reconciler *LogicalContainerExecReconciler, resource *apiv2.LogicalContainerExec,
	_ logicalContainerExecState, data *logicalContainerExecData, log logr.Logger,
) objectChange {
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	ready, reason, namespaceErr := checkNamespaceReady(ctx, reconciler.Client, resource.Namespace)
	if !ready {
		data.progress, data.reason = physicalResourceProgressNotReady, reason
		data.message = namespaceReadinessMessage(resource.Namespace, reason)
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get logical execution namespace")
			data.progress, data.message = physicalResourceProgressRetryPending, namespaceErr.Error()
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		}
		return noChange
	}
	data.state, data.progress = logicalContainerExecStateTarget, physicalResourceProgressNotReady
	return handleLogicalContainerExecPrerequisites(ctx, reconciler, resource, data.state, data, log)
}

func handleLogicalContainerExecPrerequisites(
	ctx context.Context, reconciler *LogicalContainerExecReconciler, resource *apiv2.LogicalContainerExec,
	_ logicalContainerExecState, data *logicalContainerExecData, log logr.Logger,
) objectChange {
	if data.progress == physicalResourceProgressFailed {
		return noChange
	}
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	target := reconciler.resolveExecutionTarget(ctx, resource, data, log)
	if target == nil {
		return noChange
	}
	data.pendingPhysicalRef = target.Status.PhysicalContainerRef
	data.state, data.progress = logicalContainerExecStatePhysical, physicalResourceProgressNotReady
	return handleLogicalContainerExecPhysical(ctx, reconciler, resource, data.state, data, log)
}

func (r *LogicalContainerExecReconciler) resolveExecutionTarget(
	ctx context.Context, resource *apiv2.LogicalContainerExec, data *logicalContainerExecData, log logr.Logger,
) *apiv2.LogicalContainer {
	data.progress, data.reason, data.message = physicalResourceProgressNotReady, "", ""
	ready, reason, namespaceErr := checkNamespaceReady(ctx, r.Client, resource.Namespace)
	if !ready {
		data.reason, data.message = reason, namespaceReadinessMessage(resource.Namespace, reason)
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get logical execution namespace")
			data.progress, data.message = physicalResourceProgressRetryPending, namespaceErr.Error()
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		}
		return nil
	}
	var target apiv2.LogicalContainer
	getErr := r.Get(ctx, commonapi.AsNamespacedName(resource.Spec.ContainerRef, resource.Namespace), &target)
	if getErr != nil {
		data.progress, data.reason, data.message = physicalResourceProgressNotFound, "ContainerNotFound", "Waiting for the target LogicalContainer."
		if !apierrors.IsNotFound(getErr) {
			log.Error(getErr, "Failed to get logical execution target")
			data.progress, data.reason, data.message = physicalResourceProgressRetryPending, "ContainerLookupFailed", getErr.Error()
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		}
		return nil
	}
	condition := apimeta.FindStatusCondition(target.Status.Conditions, string(apiv2.ConditionReady))
	if !target.DeletionTimestamp.IsZero() ||
		(condition != nil && condition.ObservedGeneration == target.Generation &&
			(target.Status.Phase == apiv2.LogicalContainerPhaseExited || target.Status.Phase == apiv2.LogicalContainerPhaseFailed)) {
		data.fail("ContainerUnavailable", "The target LogicalContainer has exited, failed, or is being deleted.")
		return nil
	}
	if condition == nil || condition.ObservedGeneration != target.Generation || condition.Status != metav1.ConditionTrue ||
		target.Status.Phase != apiv2.LogicalContainerPhaseRunning || target.Status.PhysicalContainerRef == "" {
		data.reason, data.message = "ContainerNotReady", "Waiting for current-generation logical container readiness."
		return nil
	}
	if data.pendingPhysicalRef != "" && data.pendingPhysicalRef != target.Status.PhysicalContainerRef {
		data.fail("ContainerUnavailable", "The selected logical container backing changed before physical realization.")
		return nil
	}
	return &target
}

func handleLogicalContainerExecPhysical(
	ctx context.Context, reconciler *LogicalContainerExecReconciler, resource *apiv2.LogicalContainerExec,
	state logicalContainerExecState, data *logicalContainerExecData, log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalResourceProgressFailed:
		return noChange
	case physicalResourceProgressNotFound, physicalResourceProgressNotReady, physicalResourceProgressRetryPending:
	default:
		return handleInvalidLogicalContainerExecState(ctx, reconciler, resource, state, data, log)
	}
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	physical := &apiv2.PhysicalContainerExec{
		ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(resource.UID), Namespace: resource.Namespace},
		Spec:       apiv2.PhysicalContainerExecSpec{ContainerRef: data.pendingPhysicalRef, Execution: *resource.Spec.Execution.DeepCopy()},
	}
	desiredSpec := physical.Spec
	getErr := reconciler.NoCacheClient.Get(ctx, physical.NamespacedName(), physical)
	if apierrors.IsNotFound(getErr) {
		if reconciler.resolveExecutionTarget(ctx, resource, data, log) == nil {
			return noChange
		}
		physical.Spec = desiredSpec
		ownerErr := ctrl.SetControllerReference(resource, physical, reconciler.Scheme())
		if ownerErr != nil {
			log.Error(ownerErr, "Failed to set physical execution owner")
			data.fail("PhysicalExecutionPreparationFailed", ownerErr.Error())
			return noChange
		}
		createErr := reconciler.Create(ctx, physical)
		if createErr != nil {
			log.Error(createErr, "Failed to prepare physical container execution")
			data.progress, data.reason, data.message = physicalResourceProgressRetryPending, "PhysicalExecutionPreparationFailed", createErr.Error()
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			return noChange
		}
	} else if getErr != nil {
		log.Error(getErr, "Failed to resolve physical execution creation outcome")
		data.progress, data.reason, data.message = physicalResourceProgressRetryPending, "PhysicalExecutionPreparationFailed", getErr.Error()
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return noChange
	}
	if !metav1.IsControlledBy(physical, resource) || !reflect.DeepEqual(physical.Spec, desiredSpec) {
		data.fail("PhysicalExecutionChanged", "The backing physical execution has a different owner or configuration.")
		return noChange
	}
	data.physicalUID, data.pendingPhysicalRef = physical.UID, ""
	data.state, data.progress, data.reason, data.message = logicalContainerExecStateRuntime, physicalResourceProgressNotReady, "", ""
	data.failureTime = metav1.MicroTime{}
	data.retryAfter = time.Time{}
	return handleLogicalContainerExecRuntime(ctx, reconciler, resource, data.state, data, log)
}

func handleLogicalContainerExecRuntime(
	ctx context.Context, reconciler *LogicalContainerExecReconciler, resource *apiv2.LogicalContainerExec,
	_ logicalContainerExecState, data *logicalContainerExecData, log logr.Logger,
) objectChange {
	if data.progress == physicalResourceProgressMissing {
		return noChange
	}
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	var physical apiv2.PhysicalContainerExec
	getErr := reconciler.NoCacheClient.Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: "logical-" + string(resource.UID)}, &physical)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		log.Error(getErr, "Failed to observe physical execution")
		data.progress, data.reason, data.message = physicalResourceProgressRetryPending, "PhysicalExecutionLookupFailed", getErr.Error()
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return noChange
	}
	if getErr != nil || physical.UID != data.physicalUID || !metav1.IsControlledBy(&physical, resource) {
		data.progress, data.reason = physicalResourceProgressMissing, "PhysicalExecutionChanged"
		data.message = "The backing physical execution is missing or its identity changed."
		return noChange
	}
	data.retryAfter = time.Time{}
	change := setValue(&resource.Status.ContainerID, physical.Status.ContainerID)
	change |= setTimestamp(&resource.Status.StartedAt, physical.Status.StartedAt)
	change |= setTimestamp(&resource.Status.FinishedAt, physical.Status.FinishedAt)
	change |= setPhysicalProcessExitCode(&resource.Status.ExitCode, physical.Status.ExitCode)
	source := physical.GetLogSource()
	if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
		resource.Status.RuntimeLogs = source
		change |= statusChanged
	}
	condition := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	data.progress, data.reason, data.message = physicalResourceProgressNotReady, "PhysicalExecutionNotReady", "Waiting for current physical execution state."
	if condition == nil || condition.ObservedGeneration != physical.Generation || !physical.DeletionTimestamp.IsZero() {
		return change
	}
	data.reason, data.message = apiv2.ConditionReason(condition.Reason), condition.Message
	switch physical.Status.Phase {
	case apiv2.ContainerExecPhaseRunning:
		if condition.Status == metav1.ConditionTrue {
			data.progress = physicalResourceProgressRunning
		}
	case apiv2.ContainerExecPhaseExited:
		data.progress = physicalResourceProgressExited
	case apiv2.ContainerExecPhaseFailed:
		data.progress = physicalResourceProgressFailed
	case apiv2.ContainerExecPhasePending, "":
	default:
		return change | handleInvalidLogicalContainerExecState(ctx, reconciler, resource, data.state, data, log)
	}
	return change
}

func handleLogicalContainerExecDeletion(
	ctx context.Context, reconciler *LogicalContainerExecReconciler, resource *apiv2.LogicalContainerExec,
	_ logicalContainerExecState, data *logicalContainerExecData, log logr.Logger,
) objectChange {
	data.state, data.progress, data.reason, data.message = logicalContainerExecStateDelete, physicalResourceProgressNotReady, "", ""
	var physical apiv2.PhysicalContainerExec
	getErr := reconciler.NoCacheClient.Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: "logical-" + string(resource.UID)}, &physical)
	if apierrors.IsNotFound(getErr) {
		return deleteFinalizer(resource, logicalContainerExecFinalizer, log)
	}
	if getErr != nil {
		log.Error(getErr, "Failed to get physical execution for deletion")
		data.progress, data.message = physicalResourceProgressRetryPending, getErr.Error()
		return noChange
	}
	if !metav1.IsControlledBy(&physical, resource) || (data.physicalUID != "" && physical.UID != data.physicalUID) {
		log.Info("Leaving independently owned physical execution untouched", "Execution", physical.NamespacedName())
		return deleteFinalizer(resource, logicalContainerExecFinalizer, log)
	}
	if physical.DeletionTimestamp.IsZero() {
		uid := physical.UID
		deleteErr := reconciler.Delete(ctx, &physical, ctrl_client.Preconditions{UID: &uid})
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			log.Error(deleteErr, "Failed to delete physical execution")
			data.progress, data.message = physicalResourceProgressRetryPending, deleteErr.Error()
			return noChange
		}
	}
	return noChange
}

func (data *logicalContainerExecData) fail(reason apiv2.ConditionReason, message string) {
	if data.progress == physicalResourceProgressFailed && data.reason == reason && data.message == message {
		return
	}
	data.progress, data.reason, data.message = physicalResourceProgressFailed, reason, message
	data.failureTime = metav1.NowMicro()
}

func handleLogicalContainerExecTerminal(
	_ context.Context, _ *LogicalContainerExecReconciler, _ *apiv2.LogicalContainerExec,
	_ logicalContainerExecState, _ *logicalContainerExecData, _ logr.Logger,
) objectChange {
	return noChange
}

func handleInvalidLogicalContainerExecState(
	_ context.Context, _ *LogicalContainerExecReconciler, _ *apiv2.LogicalContainerExec,
	_ logicalContainerExecState, data *logicalContainerExecData, log logr.Logger,
) objectChange {
	stateErr := fmt.Errorf("invalid logical container execution state %v with progress %v", data.state, data.progress)
	log.Error(stateErr, "LogicalContainerExec reached invalid reconciliation state")
	data.state = logicalContainerExecStateInvalid
	data.fail(apiv2.PhysicalResourceReasonOperationStateInvalid, stateErr.Error())
	return noChange
}
