/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"fmt"
	"reflect"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
)

const logicalContainerExecRefField = "spec.containerRef"
const logicalContainerExecOwnerUIDField = ".metadata.logicalContainerExecOwnerUID"

var logicalContainerExecFinalizer = apiv2.GroupName + "/logicalcontainerexec-reconciler"

type logicalContainerExecDataHandlerFunc = stateInitializerFunc[
	apiv2.LogicalContainerExec, *apiv2.LogicalContainerExec,
	LogicalContainerExecReconciler, *LogicalContainerExecReconciler,
	logicalContainerExecState, logicalContainerExecData, *logicalContainerExecData,
]

var logicalContainerExecDataHandlers = map[logicalContainerExecState]logicalContainerExecDataHandlerFunc{
	logicalContainerExecStatePending:       handleLogicalContainerExecPending,
	logicalContainerExecStateCreating:      handleLogicalContainerExecCreating,
	logicalContainerExecStateRunning:       handleLogicalContainerExecRunning,
	logicalContainerExecStateExited:        handleLogicalContainerExecTerminal,
	logicalContainerExecStateRuntimeFailed: handleLogicalContainerExecTerminal,
	logicalContainerExecStateMissing:       handleLogicalContainerExecTerminal,
	logicalContainerExecStateFailed:        handleLogicalContainerExecTerminal,
	logicalContainerExecStateInvalid:       handleLogicalContainerExecTerminal,
	0:                                      handleInvalidLogicalContainerExecState,
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
	ownerIndexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, &apiv2.PhysicalContainerExec{}, logicalContainerExecOwnerUIDField,
		func(obj ctrl_client.Object) []string {
			owner := metav1.GetControllerOf(obj)
			if owner == nil {
				return nil
			}
			return []string{string(owner.UID)}
		})
	if ownerIndexErr != nil {
		return fmt.Errorf("index logical container execution owner: %w", ownerIndexErr)
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.LogicalContainerExec{}).
		Owns(&apiv2.PhysicalContainerExec{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.LogicalContainer{}, handler.EnqueueRequestsFromMapFunc(r.mapContainer)).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.LogicalContainerExecList{})),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Named(name).Complete(r)
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
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to get LogicalContainerExec")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)
	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, resource.GetResourceId())
	stateKey, data := r.executions.BorrowByNamespacedName(req.NamespacedName)
	if data != nil && stateKey != resource.UID {
		logger.ReleaseResourceLog((&apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{UID: stateKey}}).GetResourceId())
		r.executions.DeleteByNamespacedName(req.NamespacedName)
		data = nil
	}
	patch := ctrl_client.MergeFromWithOptions(resource.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	delay := StandardDelay
	if !resource.DeletionTimestamp.IsZero() {
		change, delay = r.handleLogicalContainerExecDeletion(ctx, &resource, data, log)
	} else {
		change = ensureFinalizer(&resource, logicalContainerExecFinalizer, log)
		if change == noChange {
			change, delay = r.manageLogicalContainerExec(ctx, &resource, data, log)
		}
	}
	if !hasFinalizer(&resource, logicalContainerExecFinalizer) {
		r.executions.DeleteByNamespacedName(req.NamespacedName)
		defer logger.ReleaseResourceLog(resource.GetResourceId())
	}
	return r.SaveChangesWithDelay(ctx, &resource, patch, change, delay, nil, log)
}

func (r *LogicalContainerExecReconciler) manageLogicalContainerExec(
	ctx context.Context, resource *apiv2.LogicalContainerExec, data *logicalContainerExecData, log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	if data == nil {
		ready, reason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, resource.Namespace)
		if !ready {
			change := noChange
			message := namespaceReadinessMessage(resource.Namespace, reason)
			if namespaceErr != nil {
				log.Error(namespaceErr, "Failed to get logical execution namespace")
				message = namespaceErr.Error()
				change |= additionalReconciliationNeeded
			}
			change |= setValue(&resource.Status.Phase, apiv2.ContainerExecPhasePending)
			change |= setCondition(&resource.Status.Conditions, apiv2.ConditionReady, resource.Generation, metav1.ConditionFalse, reason, message)
			return change, StandardDelay
		}
		data = &logicalContainerExecData{state: logicalContainerExecStatePending}
		r.executions.Store(resource.NamespacedName(), resource.UID, data.Clone())
	}
	stateHandler := getStateInitializer(logicalContainerExecDataHandlers, data.state, log)
	change := stateHandler(ctx, r, resource, data.state, data, log)
	_ = r.executions.Update(resource.NamespacedName(), resource.UID, data)
	projectionChange, delay, valid := data.applyTo(resource)
	if !valid {
		log.Error(fmt.Errorf("invalid logical container execution state %v", data.state), "LogicalContainerExec reached invalid reconciliation state")
	}
	return change | projectionChange, delay
}

func handleLogicalContainerExecPending(
	ctx context.Context, reconciler *LogicalContainerExecReconciler, resource *apiv2.LogicalContainerExec,
	_ logicalContainerExecState, data *logicalContainerExecData, log logr.Logger,
) objectChange {
	data.reconcileErr, data.reason, data.message = nil, "", ""
	if data.pendingPhysicalRef == "" {
		target := reconciler.resolveExecutionTarget(ctx, resource, data, log)
		if target == nil {
			return noChange
		}
		data.pendingPhysicalRef = target.Status.PhysicalContainerRef
	}
	return reconciler.ensureLogicalPhysicalContainerExec(ctx, resource, data, log)
}

func (r *LogicalContainerExecReconciler) resolveExecutionTarget(
	ctx context.Context, resource *apiv2.LogicalContainerExec, data *logicalContainerExecData, log logr.Logger,
) *apiv2.LogicalContainer {
	ready, reason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, resource.Namespace)
	if !ready {
		data.reason, data.message = reason, namespaceReadinessMessage(resource.Namespace, reason)
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get logical execution namespace")
			data.reconcileErr, data.message = namespaceErr, namespaceErr.Error()
		}
		return nil
	}
	var target apiv2.LogicalContainer
	getErr := r.Get(ctx, commonapi.AsNamespacedName(resource.Spec.ContainerRef, resource.Namespace), &target)
	if getErr != nil {
		data.reason, data.message = apiv2.ContainerExecReasonContainerNotFound, "Waiting for the target LogicalContainer."
		if !apierrors.IsNotFound(getErr) {
			log.Error(getErr, "Failed to get logical execution target")
			data.reconcileErr, data.reason, data.message = getErr, apiv2.ContainerExecReasonContainerLookupFailed, getErr.Error()
		}
		return nil
	}
	condition := apimeta.FindStatusCondition(target.Status.Conditions, string(apiv2.ConditionReady))
	if !target.DeletionTimestamp.IsZero() ||
		target.Status.Phase == apiv2.LogicalContainerPhaseExited || target.Status.Phase == apiv2.LogicalContainerPhaseFailed {
		data.fail(apiv2.ContainerExecReasonContainerUnavailable, "The target LogicalContainer has exited, failed, or is being deleted.")
		return nil
	}
	if target.Spec.Stop {
		data.fail(apiv2.ContainerExecReasonContainerUnavailable, "The target LogicalContainer is stopping.")
		return nil
	}
	if condition == nil || condition.Status != metav1.ConditionTrue ||
		target.Status.Phase != apiv2.LogicalContainerPhaseRunning || target.Status.PhysicalContainerRef == "" {
		data.reason, data.message = apiv2.ContainerExecReasonContainerNotReady, "Waiting for logical container readiness."
		return nil
	}
	if data.pendingPhysicalRef != "" && data.pendingPhysicalRef != target.Status.PhysicalContainerRef {
		data.fail(apiv2.ContainerExecReasonContainerUnavailable, "The selected logical container backing changed before physical realization.")
		return nil
	}
	return &target
}

func (reconciler *LogicalContainerExecReconciler) ensureLogicalPhysicalContainerExec(
	ctx context.Context, resource *apiv2.LogicalContainerExec, data *logicalContainerExecData, log logr.Logger,
) objectChange {
	physical := &apiv2.PhysicalContainerExec{
		ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(resource.UID), Namespace: resource.Namespace},
		Spec:       apiv2.PhysicalContainerExecSpec{ContainerRef: data.pendingPhysicalRef, Execution: *resource.Spec.Execution.DeepCopy()},
	}
	desiredSpec := physical.Spec
	observed, getErr := reconciler.findPhysicalContainerExec(ctx, resource, data)
	if apierrors.IsNotFound(getErr) {
		if reconciler.resolveExecutionTarget(ctx, resource, data, log) == nil {
			return noChange
		}
		physical.Spec = desiredSpec
		ownerErr := ctrl.SetControllerReference(resource, physical, reconciler.Scheme())
		if ownerErr != nil {
			log.Error(ownerErr, "Failed to set physical execution owner")
			data.fail(apiv2.LogicalContainerExecReasonPhysicalExecutionPreparationFailed, ownerErr.Error())
			return noChange
		}
		InitializeLogicalBackingFinalizers(physical, physicalContainerExecFinalizer)
		data.unobservedChildren.recordCreateAttempt(physical)
		createErr := reconciler.Create(ctx, physical)
		if physical.UID != "" {
			data.physicalUID = physical.UID
		}
		if createErr != nil {
			if !apierrors.IsAlreadyExists(createErr) {
				data.reconcileErr = fmt.Errorf("create physical execution: %w", createErr)
				data.message = data.reconcileErr.Error()
				log.Error(data.reconcileErr, "Failed to prepare physical container execution")
				return noChange
			}
			confirmErr := reconciler.NoCacheClient.Get(ctx, physical.NamespacedName(), physical)
			if confirmErr != nil {
				data.reconcileErr = fmt.Errorf("get existing physical execution: %w", confirmErr)
				data.message = data.reconcileErr.Error()
				log.Error(data.reconcileErr, "Failed to prepare physical container execution")
				return noChange
			}
		}
	} else if getErr != nil {
		log.Error(getErr, "Failed to resolve physical execution creation outcome")
		data.reconcileErr, data.message = getErr, getErr.Error()
		return noChange
	} else {
		physical = observed
	}
	if identityErr := validatePhysicalResourceIdentity(resource, physical, data.physicalUID); identityErr != nil {
		data.fail(apiv2.LogicalContainerExecReasonPhysicalExecutionChanged, identityErr.Error())
		return noChange
	}
	if !reflect.DeepEqual(physical.Spec, desiredSpec) {
		data.fail(apiv2.LogicalContainerExecReasonPhysicalExecutionChanged, "The backing physical execution has a different configuration.")
		return noChange
	}
	data.physicalUID, data.pendingPhysicalRef = physical.UID, ""
	data.state, data.reason, data.message = logicalContainerExecStateCreating, "", ""
	data.failureTime = metav1.MicroTime{}
	return additionalReconciliationNeeded
}

func handleLogicalContainerExecCreating(
	ctx context.Context, reconciler *LogicalContainerExecReconciler, resource *apiv2.LogicalContainerExec,
	_ logicalContainerExecState, data *logicalContainerExecData, log logr.Logger,
) objectChange {
	data.reconcileErr = nil
	physical, getErr := reconciler.findPhysicalContainerExec(ctx, resource, data)
	if getErr != nil {
		log.Error(getErr, "Failed to observe physical execution")
		return additionalReconciliationNeeded
	}
	change, observedState := applyLogicalContainerExecBackingStatus(resource, physical)
	if observedState == logicalContainerExecStateInvalid {
		return change | handleInvalidLogicalContainerExecState(ctx, reconciler, resource, observedState, data, log)
	}
	data.state, data.reason, data.message = observedState, apiv2.LogicalContainerExecReasonPhysicalExecutionNotReady, "Waiting for physical execution state."
	if condition := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady)); condition != nil {
		data.reason, data.message = apiv2.ConditionReason(condition.Reason), condition.Message
	}
	if data.state != logicalContainerExecStateCreating || !physical.DeletionTimestamp.IsZero() {
		change |= additionalReconciliationNeeded
	}
	return change
}

func handleLogicalContainerExecRunning(
	ctx context.Context, reconciler *LogicalContainerExecReconciler, resource *apiv2.LogicalContainerExec,
	_ logicalContainerExecState, data *logicalContainerExecData, log logr.Logger,
) objectChange {
	data.reconcileErr = nil
	physical, getErr := reconciler.findPhysicalContainerExec(ctx, resource, data)
	if getErr != nil {
		log.Error(getErr, "Failed to get running execution backing")
		return additionalReconciliationNeeded
	}
	change, observedState := applyLogicalContainerExecBackingStatus(resource, physical)
	if observedState == logicalContainerExecStateInvalid {
		return change | handleInvalidLogicalContainerExecState(ctx, reconciler, resource, observedState, data, log)
	}
	if observedState != logicalContainerExecStateRunning {
		data.state, data.reason, data.message = observedState, "", ""
		if condition := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady)); condition != nil {
			data.reason, data.message = apiv2.ConditionReason(condition.Reason), condition.Message
		}
		change |= additionalReconciliationNeeded
	}
	return change
}

func (r *LogicalContainerExecReconciler) findPhysicalContainerExec(
	ctx context.Context, resource *apiv2.LogicalContainerExec, data *logicalContainerExecData,
) (*apiv2.PhysicalContainerExec, error) {
	var children apiv2.PhysicalContainerExecList
	listErr := r.List(ctx, &children, ctrl_client.InNamespace(resource.Namespace),
		ctrl_client.MatchingFields{logicalContainerExecOwnerUIDField: string(resource.UID)})
	if listErr != nil {
		data.reconcileErr, data.reason, data.message = listErr, apiv2.LogicalContainerExecReasonPhysicalExecutionLookupFailed, listErr.Error()
		return nil, listErr
	}
	name := "logical-" + string(resource.UID)
	for i := range children.Items {
		child := &children.Items[i]
		if child.Name == name && metav1.IsControlledBy(child, resource) {
			data.unobservedChildren.observeCached(child)
			if (data.physicalUID != "" && child.UID != data.physicalUID) || !hasFinalizer(child, LogicalBackingFinalizer) {
				identityErr := fmt.Errorf("physical execution %q UID or logical hold changed", child.NamespacedName())
				data.state, data.reason, data.message, data.reconcileErr = logicalContainerExecStateMissing, apiv2.LogicalContainerExecReasonPhysicalExecutionChanged, identityErr.Error(), nil
				return nil, identityErr
			}
			return child, nil
		}
	}
	physical := &apiv2.PhysicalContainerExec{}
	getErr := r.NoCacheClient.Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: name}, physical)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) && data.physicalUID != "" {
			data.state, data.reason, data.message, data.reconcileErr = logicalContainerExecStateMissing, apiv2.LogicalContainerExecReasonPhysicalExecutionChanged,
				"The backing physical execution no longer exists.", nil
		} else if !apierrors.IsNotFound(getErr) {
			data.reconcileErr, data.reason, data.message = getErr, apiv2.LogicalContainerExecReasonPhysicalExecutionLookupFailed, getErr.Error()
		}
		return nil, getErr
	}
	if !metav1.IsControlledBy(physical, resource) || (data.physicalUID != "" && physical.UID != data.physicalUID) ||
		!hasFinalizer(physical, LogicalBackingFinalizer) {
		identityErr := fmt.Errorf("physical execution %q has a different identity, owner, or logical hold", physical.NamespacedName())
		data.state, data.reason, data.message, data.reconcileErr = logicalContainerExecStateMissing, apiv2.LogicalContainerExecReasonPhysicalExecutionChanged, identityErr.Error(), nil
		return nil, identityErr
	}
	return physical, nil
}

func (r *LogicalContainerExecReconciler) handleLogicalContainerExecDeletion(
	ctx context.Context, resource *apiv2.LogicalContainerExec, data *logicalContainerExecData, log logr.Logger,
) (change objectChange, delay AdditionalReconciliationDelay) {
	delay = StandardDelay
	if data == nil {
		data = &logicalContainerExecData{state: logicalContainerExecStateDelete}
		data.unobservedChildren.recordCreateAttempt(&apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(resource.UID)}})
		r.executions.Store(resource.NamespacedName(), resource.UID, data.Clone())
	}
	defer func() {
		if data != nil {
			_ = r.executions.Update(resource.NamespacedName(), resource.UID, data)
			projectionChange, projectionDelay, valid := data.applyTo(resource)
			change |= projectionChange
			delay = projectionDelay
			if !valid {
				log.Error(fmt.Errorf("invalid logical container execution state %v", data.state), "LogicalContainerExec reached invalid reconciliation state")
			}
		}
	}()
	data.state, data.reason, data.message, data.reconcileErr = logicalContainerExecStateDelete, "", "", nil
	var children apiv2.PhysicalContainerExecList
	listErr := r.List(ctx, &children, ctrl_client.InNamespace(resource.Namespace),
		ctrl_client.MatchingFields{logicalContainerExecOwnerUIDField: string(resource.UID)})
	if listErr == nil {
		listErr = resolveUnobservedLogicalChildren(ctx, r.NoCacheClient, resource, data.unobservedChildren, &children.Items)
	}
	if listErr != nil {
		log.Error(listErr, "Failed to list physical executions before deletion")
		data.reconcileErr, data.message = listErr, listErr.Error()
		return additionalReconciliationNeeded, delay
	}
	waiting := false
	for i := range children.Items {
		physical := &children.Items[i]
		if !metav1.IsControlledBy(physical, resource) {
			continue
		}
		if data.physicalUID == "" && physical.Name == "logical-"+string(resource.UID) {
			data.physicalUID = physical.UID
		}
		waiting = true
		if physical.DeletionTimestamp.IsZero() {
			uid := physical.UID
			deleteErr := r.Delete(ctx, physical, ctrl_client.Preconditions{UID: &uid})
			if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				log.Error(deleteErr, "Failed to delete physical execution")
				data.reconcileErr, data.message = deleteErr, deleteErr.Error()
			}
			continue
		}
		if hasFinalizer(physical, physicalContainerExecFinalizer) {
			continue
		}
		releaseErr := ReleaseLogicalBackingFinalizer(ctx, r.Client, physical)
		if releaseErr != nil && !apierrors.IsNotFound(releaseErr) {
			log.Error(releaseErr, "Failed to release logical execution backing")
			data.reconcileErr, data.message = releaseErr, releaseErr.Error()
		}
	}
	if waiting {
		return additionalReconciliationNeeded, delay
	}
	r.executions.DeleteByNamespacedName(resource.NamespacedName())
	data = nil
	return deleteFinalizer(resource, logicalContainerExecFinalizer, log), delay
}

func (data *logicalContainerExecData) fail(reason apiv2.ConditionReason, message string) {
	if data.state == logicalContainerExecStateFailed && data.reason == reason && data.message == message {
		return
	}
	data.state, data.reason, data.message = logicalContainerExecStateFailed, reason, message
	data.reconcileErr = nil
	data.failureTime = metav1.NowMicro()
}

func handleLogicalContainerExecTerminal(
	ctx context.Context, reconciler *LogicalContainerExecReconciler, resource *apiv2.LogicalContainerExec,
	_ logicalContainerExecState, data *logicalContainerExecData, log logr.Logger,
) objectChange {
	if data.physicalUID == "" || data.state == logicalContainerExecStateMissing {
		return noChange
	}
	data.reconcileErr = nil
	lookupData := *data
	physical, getErr := reconciler.findPhysicalContainerExec(ctx, resource, &lookupData)
	if getErr != nil {
		log.Error(getErr, "Failed to refresh terminal execution backing")
		if lookupData.state == logicalContainerExecStateMissing {
			data.state, data.reason, data.message = lookupData.state, lookupData.reason, lookupData.message
		}
		return noChange
	}
	change, _ := applyLogicalContainerExecBackingStatus(resource, physical)
	return change
}

func handleInvalidLogicalContainerExecState(
	_ context.Context, _ *LogicalContainerExecReconciler, _ *apiv2.LogicalContainerExec,
	_ logicalContainerExecState, data *logicalContainerExecData, log logr.Logger,
) objectChange {
	stateErr := fmt.Errorf("invalid logical container execution state %v", data.state)
	log.Error(stateErr, "LogicalContainerExec reached invalid reconciliation state")
	data.fail(apiv2.PhysicalResourceReasonOperationStateInvalid, stateErr.Error())
	data.state = logicalContainerExecStateInvalid
	return noChange
}
