/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

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
	"github.com/microsoft/dcp/internal/dcpclient"
	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

var logicalProcessFinalizer = fmt.Sprintf("%s/logicalprocess-reconciler", apiv2.GroupVersion.Group)

type logicalProcessDataHandlerFunc = stateInitializerFunc[
	apiv2.LogicalProcess, *apiv2.LogicalProcess,
	LogicalProcessReconciler, *LogicalProcessReconciler,
	logicalProcessState, logicalProcessData, *logicalProcessData,
]

var logicalProcessDataHandlers = map[logicalProcessState]logicalProcessDataHandlerFunc{
	logicalProcessStateNamespace:        handleLogicalProcessNamespace,
	logicalProcessStatePrepare:          handleLogicalProcessPreparation,
	logicalProcessStatePhysical:         handleLogicalProcessPhysical,
	logicalProcessStatePhysicalRetiring: handleLogicalProcessPhysical,
	logicalProcessStateRuntime:          handleLogicalProcessRuntime,
	logicalProcessStateMissing:          handleLogicalProcessMissing,
	logicalProcessStatePrepared:         handleLogicalProcessPreparation,
	logicalProcessStatePrepareRetry:     handleLogicalProcessPreparation,
	logicalProcessStatePrepareFailed:    handleLogicalProcessPreparation,
	logicalProcessStatePhysicalRetry:    handleLogicalProcessPhysical,
	logicalProcessStatePhysicalFailed:   handleLogicalProcessPhysical,
	logicalProcessStateRunning:          handleLogicalProcessRuntime,
	logicalProcessStateExited:           handleLogicalProcessRuntime,
	logicalProcessStateRuntimeFailed:    handleLogicalProcessRuntime,
	logicalProcessStateUnknown:          handleLogicalProcessRuntime,
	logicalProcessStateRuntimeRetry:     handleLogicalProcessRuntime,
}

type LogicalProcessReconcilerConfig struct {
	StateStore         *statestore.Store
	ResourceLeaseOwner process.ProcessHandle
	WorkloadID         commonapi.WorkloadID
}

type LogicalProcessReconciler struct {
	*ReconcilerBase[apiv2.LogicalProcess, *apiv2.LogicalProcess]
	processExecutor process.Executor
	config          LogicalProcessReconcilerConfig
	processData     *ObjectStateMap[types.UID, logicalProcessData, *logicalProcessData, *apiv2.LogicalProcess]
	operationQueue  *resiliency.WorkQueue
}

func NewLogicalProcessReconciler(
	lifetimeCtx context.Context, client ctrl_client.Client, noCacheClient ctrl_client.Reader,
	log logr.Logger, executor process.Executor, config LogicalProcessReconcilerConfig,
) *LogicalProcessReconciler {
	return &LogicalProcessReconciler{
		ReconcilerBase:  NewReconcilerBase[apiv2.LogicalProcess](client, noCacheClient, log, lifetimeCtx),
		processExecutor: executor, config: config,
		processData:    NewObjectStateMap[types.UID, logicalProcessData, *logicalProcessData, *apiv2.LogicalProcess](),
		operationQueue: resiliency.NewWorkQueue(lifetimeCtx, MaxConcurrentReconciles),
	}
}

func (r *LogicalProcessReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.LogicalProcess{}).
		Owns(&apiv2.PhysicalProcess{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.LogicalProcessList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).Named(name).Complete(r)
}

func (r *LogicalProcessReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)
	if ctx.Err() != nil {
		return ctrl.Result{}, nil
	}
	var resource apiv2.LogicalProcess
	getErr := reader.Get(ctx, req.NamespacedName, &resource)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			r.discardLogicalProcessData(req.NamespacedName, log)
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to Get() the LogicalProcess")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)
	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, resource.GetResourceId())
	stateKey, previous := r.processData.BorrowByNamespacedName(req.NamespacedName)
	if previous != nil && stateKey != resource.UID {
		r.discardLogicalProcessData(req.NamespacedName, log)
	}
	r.processData.RunDeferredOps(req.NamespacedName, &resource)
	_, data := r.processData.BorrowByNamespacedName(req.NamespacedName)
	if data == nil {
		data = &logicalProcessData{state: logicalProcessStateNamespace}
		r.processData.Store(req.NamespacedName, resource.UID, data.Clone())
	}
	patch := ctrl_client.MergeFromWithOptions(resource.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	finalizerAdded := false
	if !resource.DeletionTimestamp.IsZero() {
		if data.operation == nil {
			change = r.handleLogicalProcessDeletion(ctx, &resource, data, log)
		}
	} else if change = ensureFinalizer(&resource, logicalProcessFinalizer, log); change != noChange {
		finalizerAdded = true
	} else if data.operation == nil || data.physicalUID != "" {
		stateHandler, known := logicalProcessDataHandlers[data.state]
		if !known {
			stateErr := fmt.Errorf("unknown logical process state %v", data.state)
			log.Error(stateErr, "LogicalProcess reached invalid reconciliation state")
			data.state = logicalProcessStateRuntimeFailed
			data.reason, data.message = apiv2.PhysicalResourceReasonOperationStateInvalid, stateErr.Error()
		} else {
			change = stateHandler(ctx, r, &resource, data.state, data, log)
		}
	}
	delay := StandardDelay
	if !finalizerAdded && hasFinalizer(&resource, logicalProcessFinalizer) {
		_ = r.processData.Update(req.NamespacedName, resource.UID, data)
		if data.state != logicalProcessStateNamespace {
			projectionChange, projectionDelay, valid := data.applyTo(&resource)
			change |= projectionChange
			delay = projectionDelay
			if !valid {
				log.Error(fmt.Errorf("invalid logical process state %v", data.state),
					"LogicalProcess reached invalid reconciliation state")
			}
		}
	}
	if !hasFinalizer(&resource, logicalProcessFinalizer) {
		defer logger.ReleaseResourceLog(resource.GetResourceId())
	}
	return r.SaveChangesWithDelay(ctx, &resource, patch, change, delay, nil, log)
}

func handleLogicalProcessNamespace(
	ctx context.Context, reconciler *LogicalProcessReconciler, resource *apiv2.LogicalProcess,
	_ logicalProcessState, data *logicalProcessData, log logr.Logger,
) objectChange {
	ready, reason, namespaceErr := checkNamespaceReadiness(ctx, reconciler.Client, resource.Namespace)
	if !ready {
		change := noChange
		phase, message := apiv2.LogicalProcessPhasePending, namespaceReadinessMessage(resource.Namespace, reason)
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get namespace", "Namespace", resource.Namespace)
			phase, message = apiv2.LogicalProcessPhaseUnknown, fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
			change |= additionalReconciliationNeeded
		}
		change |= setValue(&resource.Status.Phase, phase)
		change |= setCondition(&resource.Status.Conditions, apiv2.ConditionReady, resource.Generation, metav1.ConditionFalse, reason, message)
		return change
	}
	return reconciler.scheduleLogicalProcessOperation(resource, data, logicalProcessStatePrepare, reconciler.prepareLogicalProcess, log)
}

func handleLogicalProcessPreparation(
	ctx context.Context, reconciler *LogicalProcessReconciler, resource *apiv2.LogicalProcess,
	_ logicalProcessState, data *logicalProcessData, log logr.Logger,
) objectChange {
	switch data.state {
	case logicalProcessStatePrepared:
		data.state = logicalProcessStatePhysical
		return handleLogicalProcessPhysical(ctx, reconciler, resource, data.state, data, log)
	case logicalProcessStatePrepareFailed:
		return noChange
	default:
		if time.Now().Before(data.retryAfter) {
			return additionalReconciliationNeeded
		}
		return reconciler.scheduleLogicalProcessOperation(resource, data, logicalProcessStatePrepare, reconciler.prepareLogicalProcess, log)
	}
}

func handleLogicalProcessPhysical(
	ctx context.Context, reconciler *LogicalProcessReconciler, resource *apiv2.LogicalProcess,
	_ logicalProcessState, data *logicalProcessData, log logr.Logger,
) objectChange {
	if data.state == logicalProcessStatePhysicalFailed {
		return noChange
	}
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	data.state, data.reason, data.message = logicalProcessStatePhysical, "", ""
	if data.lease != nil {
		verifyErr := reconciler.config.StateStore.VerifyResourceLeaseHeld(ctx, resource, reconciler.config.ResourceLeaseOwner)
		if verifyErr != nil {
			log.Error(verifyErr, "Failed to verify persistent process startup lease")
			data.state, data.reason, data.message = logicalProcessStatePhysicalFailed, "ResourceLeaseLost", verifyErr.Error()
			return noChange
		}
	}
	if data.replaceExisting {
		record := data.pendingRecord
		pid := int64(record.PID)
		previous, previousErr := reconciler.ensureLogicalPhysicalProcess(ctx, resource, data.replacementUID, "-previous",
			apiv2.PhysicalProcessSpec{PID: &pid, IdentityTimestamp: metav1.NewMicroTime(record.IdentityTime), Stop: true,
				StdoutFile: record.StdOutFile, StderrFile: record.StdErrFile})
		if previousErr != nil {
			log.Error(previousErr, "Failed to prepare persistent process replacement")
			data.state, data.message = logicalProcessStatePhysicalRetry, previousErr.Error()
			return noChange
		}
		data.replacementUID = previous.UID
		if !logicalPhysicalProcessObserved(previous) || previous.Status.Phase != apiv2.PhysicalProcessPhaseExited {
			data.message = "Waiting for the physical controller to stop the previous persistent process."
			if previous.Status.Phase == apiv2.PhysicalProcessPhaseFailed {
				data.state, data.message = logicalProcessStatePhysicalFailed, "The physical controller could not stop the previous persistent process."
			}
			return noChange
		}
		return reconciler.scheduleLogicalProcessOperation(resource, data, logicalProcessStatePhysicalRetiring,
			func(retireCtx context.Context, snapshot *apiv2.LogicalProcess, result *logicalProcessData, operationLog logr.Logger) {
				retireErr := logs.RemoveProcessOutput(retireCtx, result.pendingRecord.StdOutFile, result.pendingRecord.StdErrFile)
				if retireErr == nil {
					retireErr = reconciler.config.StateStore.DeletePersistentProcess(retireCtx, snapshot.GetLeaseKey())
				}
				if retireErr != nil {
					operationLog.Error(retireErr, "Failed to retire replaced process output and record")
					result.state, result.message = logicalProcessStatePhysicalRetry, retireErr.Error()
					result.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
					return
				}
				result.pendingRecord, result.replaceExisting = nil, false
				result.state = logicalProcessStatePhysical
			}, log)
	}
	physicalSpec := apiv2.PhysicalProcessSpec{Stop: resource.Spec.Stop}
	switch {
	case resource.Spec.EffectiveMode() == apiv2.LogicalProcessModeExisting:
		physicalSpec.PID, physicalSpec.IdentityTimestamp = resource.Spec.PID, resource.Spec.IdentityTimestamp
	case data.pendingRecord != nil:
		pid := int64(data.pendingRecord.PID)
		physicalSpec.PID, physicalSpec.IdentityTimestamp = &pid, metav1.NewMicroTime(data.pendingRecord.IdentityTime)
		physicalSpec.StdoutFile, physicalSpec.StderrFile = data.pendingRecord.StdOutFile, data.pendingRecord.StdErrFile
	default:
		desired := resource.Spec.Process
		physicalSpec.Process = &apiv2.PhysicalProcessConfig{
			ExecutablePath: desired.ExecutablePath, Args: desired.Args, WorkingDirectory: desired.WorkingDirectory,
			InheritEnvironment: desired.InheritEnvironment, Env: desired.Env,
			RetainRuntimeProcess: resource.Spec.EffectiveMode() == apiv2.LogicalProcessModePersistent,
			MonitorPID:           resource.Spec.MonitorPID, MonitorTimestamp: resource.Spec.MonitorTimestamp,
		}
	}
	physical, physicalErr := reconciler.ensureLogicalPhysicalProcess(ctx, resource, data.physicalUID, "", physicalSpec)
	if physicalErr != nil {
		log.Error(physicalErr, "Failed to prepare backing physical process")
		data.state, data.message = logicalProcessStatePhysicalRetry, physicalErr.Error()
		return noChange
	}
	data.physicalUID, data.pendingRecord = physical.UID, nil
	data.state = logicalProcessStateRuntime
	return handleLogicalProcessRuntime(ctx, reconciler, resource, data.state, data, log)
}

func (r *LogicalProcessReconciler) ensureLogicalPhysicalProcess(
	ctx context.Context, resource *apiv2.LogicalProcess, expectedUID types.UID, suffix string, spec apiv2.PhysicalProcessSpec,
) (*apiv2.PhysicalProcess, error) {
	physical := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(resource.UID) + suffix, Namespace: resource.Namespace},
		Spec:       spec,
	}
	getErr := r.NoCacheClient.Get(ctx, physical.NamespacedName(), physical)
	if apierrors.IsNotFound(getErr) {
		ready, reason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, resource.Namespace)
		if namespaceErr != nil {
			return nil, fmt.Errorf("could not check namespace readiness: %w", namespaceErr)
		}
		if !ready {
			return nil, errors.New(namespaceReadinessMessage(resource.Namespace, reason))
		}
		ownerErr := ctrl.SetControllerReference(resource, physical, r.Scheme())
		if ownerErr != nil {
			return nil, fmt.Errorf("could not set physical process owner: %w", ownerErr)
		}
		createErr := r.Create(ctx, physical)
		if apierrors.IsAlreadyExists(createErr) {
			getErr = r.NoCacheClient.Get(ctx, physical.NamespacedName(), physical)
			if getErr != nil {
				return nil, getErr
			}
		} else if createErr != nil {
			return nil, createErr
		}
	} else if getErr != nil {
		return nil, getErr
	}
	actualSpec, expectedSpec := physical.Spec, spec
	actualSpec.Stop, expectedSpec.Stop = false, false
	actualSpec.IdentityTimestamp, expectedSpec.IdentityTimestamp = metav1.MicroTime{}, metav1.MicroTime{}
	actualSpec.Process, expectedSpec.Process = physical.Spec.Process.DeepCopy(), spec.Process.DeepCopy()
	if actualSpec.Process != nil && expectedSpec.Process != nil {
		actualSpec.Process.MonitorTimestamp, expectedSpec.Process.MonitorTimestamp = metav1.MicroTime{}, metav1.MicroTime{}
	}
	monitorMatches := physical.Spec.Process == nil || spec.Process == nil ||
		osutil.MicroEqual(physical.Spec.Process.MonitorTimestamp, spec.Process.MonitorTimestamp)
	if !metav1.IsControlledBy(physical, resource) || (expectedUID != "" && physical.UID != expectedUID) ||
		!reflect.DeepEqual(actualSpec, expectedSpec) || !monitorMatches ||
		!osutil.MicroEqual(physical.Spec.IdentityTimestamp, spec.IdentityTimestamp) {
		return nil, fmt.Errorf("physical process %q has a different identity, owner, or spec", physical.NamespacedName())
	}
	if !physical.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("physical process %q is being deleted", physical.NamespacedName())
	}
	return physical, nil
}

func logicalPhysicalProcessObserved(physical *apiv2.PhysicalProcess) bool {
	ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	return ready != nil && ready.ObservedGeneration == physical.Generation
}

func logicalPhysicalProcessStartupSettled(physical *apiv2.PhysicalProcess) bool {
	if !physical.DeletionTimestamp.IsZero() || !logicalPhysicalProcessObserved(physical) {
		return false
	}
	ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	switch physical.Status.Phase {
	case apiv2.PhysicalProcessPhaseRunning:
		return ready.Status == metav1.ConditionTrue
	case apiv2.PhysicalProcessPhaseExited, apiv2.PhysicalProcessPhaseFailed:
		return true
	default:
		// A terminating namespace cannot become active and allow a pending launch.
		return physical.Status.Phase == apiv2.PhysicalProcessPhasePending &&
			ready.Reason == string(apiv2.PhysicalResourceReasonNamespaceTerminating)
	}
}

func handleLogicalProcessRuntime(
	ctx context.Context, reconciler *LogicalProcessReconciler, resource *apiv2.LogicalProcess,
	_ logicalProcessState, data *logicalProcessData, log logr.Logger,
) objectChange {
	physical, getErr := reconciler.getLogicalPhysicalProcess(ctx, resource, data.physicalUID, "")
	if getErr != nil {
		log.Error(getErr, "Failed to get backing physical process")
		data.state, data.reason, data.message = logicalProcessStateRuntimeRetry, "", getErr.Error()
		if apierrors.IsNotFound(getErr) {
			data.state = logicalProcessStateMissing
			data.reason, data.message = "PhysicalProcessMissing", "The backing PhysicalProcess no longer exists; the logical process cannot recover."
			return handleLogicalProcessMissing(ctx, reconciler, resource, data.state, data, log)
		}
		return noChange
	}
	if resource.Spec.Stop && !physical.Spec.Stop {
		patch := ctrl_client.MergeFromWithOptions(physical.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
		physical.Spec.Stop = resource.Spec.Stop
		stopErr := reconciler.Patch(ctx, physical, patch)
		if stopErr != nil {
			log.Error(stopErr, "Failed to request physical process stop")
			data.state, data.message = logicalProcessStateRuntimeRetry, stopErr.Error()
			return noChange
		}
	}
	change := applyLogicalProcessPhysicalDetails(resource, physical)
	data.state, data.reason, data.message = logicalProcessStateRuntime, "", ""
	if !physical.DeletionTimestamp.IsZero() {
		data.state, data.message = logicalProcessStateUnknown, "The backing PhysicalProcess is being deleted."
		return change
	}
	if !logicalPhysicalProcessObserved(physical) {
		return change
	}
	condition := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	data.message = condition.Message
	switch physical.Status.Phase {
	case apiv2.PhysicalProcessPhaseRunning:
		ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
		if ready.Status == metav1.ConditionTrue {
			data.state = logicalProcessStateRunning
		}
	case apiv2.PhysicalProcessPhaseExited:
		data.state = logicalProcessStateExited
	case apiv2.PhysicalProcessPhaseFailed:
		data.state = logicalProcessStateRuntimeFailed
	case apiv2.PhysicalProcessPhaseUnknown:
		data.state = logicalProcessStateUnknown
	}
	if data.lease != nil && logicalPhysicalProcessStartupSettled(physical) {
		return change | reconciler.scheduleLogicalProcessOperation(resource, data, logicalProcessStatePersistence, reconciler.persistLogicalProcess, log)
	}
	return change
}

func handleLogicalProcessMissing(
	_ context.Context, reconciler *LogicalProcessReconciler, resource *apiv2.LogicalProcess,
	_ logicalProcessState, data *logicalProcessData, log logr.Logger,
) objectChange {
	change := data.applyMissingLogSource(resource)
	if data.lease != nil {
		return change | reconciler.scheduleLogicalProcessOperation(resource, data, logicalProcessStatePersistence, reconciler.persistLogicalProcess, log)
	}
	return change
}

func applyLogicalProcessPhysicalDetails(resource *apiv2.LogicalProcess, physical *apiv2.PhysicalProcess) objectChange {
	change := noChange
	status := &physical.Status
	source := physical.GetLogSource()
	if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
		resource.Status.RuntimeLogs = source
		change |= statusChanged
	}
	if !reflect.DeepEqual(resource.Status.PID, status.PID) {
		resource.Status.PID = status.DeepCopy().PID
		change |= statusChanged
	}
	if !reflect.DeepEqual(resource.Status.ExitCode, status.ExitCode) {
		resource.Status.ExitCode = cloneInt32Pointer(status.ExitCode)
		change |= statusChanged
	}
	change |= setTimestamp(&resource.Status.IdentityTimestamp, status.IdentityTimestamp)
	change |= setTimestamp(&resource.Status.FinishedAt, status.FinishedAt)
	return change
}

func (r *LogicalProcessReconciler) getLogicalPhysicalProcess(
	ctx context.Context, resource *apiv2.LogicalProcess, expectedUID types.UID, suffix string,
) (*apiv2.PhysicalProcess, error) {
	physical := &apiv2.PhysicalProcess{}
	name := types.NamespacedName{Namespace: resource.Namespace, Name: "logical-" + string(resource.UID) + suffix}
	getErr := r.NoCacheClient.Get(ctx, name, physical)
	if getErr != nil {
		return nil, getErr
	}
	if !metav1.IsControlledBy(physical, resource) || (expectedUID != "" && physical.UID != expectedUID) {
		return nil, fmt.Errorf("physical process %q has a different identity or owner", name)
	}
	return physical, nil
}

func (r *LogicalProcessReconciler) handleLogicalProcessDeletion(
	ctx context.Context, resource *apiv2.LogicalProcess, data *logicalProcessData, log logr.Logger,
) objectChange {
	if data.lease != nil {
		if data.state == logicalProcessStateMissing {
			return handleLogicalProcessMissing(ctx, r, resource, data.state, data, log)
		}
		if data.physicalUID != "" {
			data.state = logicalProcessStateRuntime
			return handleLogicalProcessRuntime(ctx, r, resource, data.state, data, log)
		}
		return r.scheduleLogicalProcessOperation(resource, data, logicalProcessStatePersistence, r.persistLogicalProcess, log)
	}
	data.state, data.reason, data.message = logicalProcessStateDelete, "", ""
	for _, backing := range []struct {
		suffix string
		uid    types.UID
	}{{"", data.physicalUID}, {"-previous", data.replacementUID}} {
		physical, getErr := r.getLogicalPhysicalProcess(ctx, resource, backing.uid, backing.suffix)
		if apierrors.IsNotFound(getErr) {
			continue
		}
		if getErr != nil {
			log.Error(getErr, "Failed to get physical process for deletion")
			data.state, data.message = logicalProcessStateDeleteRetry, getErr.Error()
			return noChange
		}
		if physical.DeletionTimestamp.IsZero() {
			uid := physical.UID
			deleteErr := r.Delete(ctx, physical, ctrl_client.Preconditions{UID: &uid})
			if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				log.Error(deleteErr, "Failed to delete backing physical process")
				data.state, data.message = logicalProcessStateDeleteRetry, deleteErr.Error()
				return noChange
			}
		}
		return additionalReconciliationNeeded
	}
	r.processData.DeleteByNamespacedName(resource.NamespacedName())
	return deleteFinalizer(resource, logicalProcessFinalizer, log)
}

func (r *LogicalProcessReconciler) scheduleLogicalProcessOperation(
	resource *apiv2.LogicalProcess, current *logicalProcessData, state logicalProcessState,
	work func(context.Context, *apiv2.LogicalProcess, *logicalProcessData, logr.Logger), log logr.Logger,
) objectChange {
	if current.operation != nil {
		return noChange
	}
	if state == logicalProcessStatePersistence && current.leaseFailureMessage != "" && time.Now().Before(current.retryAfter) {
		return additionalReconciliationNeeded
	}
	operationCtx, cancel := context.WithCancel(r.LifetimeCtx)
	data := current.Clone()
	if state == logicalProcessStatePersistence {
		data.leaseFailureMessage = ""
	} else {
		data.state, data.reason, data.message = state, "", ""
	}
	data.retryAfter = time.Time{}
	data.operation = &logicalProcessOperation{cancel: cancel}
	if !r.processData.Update(resource.NamespacedName(), resource.UID, data) {
		cancel()
		return additionalReconciliationNeeded
	}
	current.UpdateFrom(data)
	snapshot, result := resource.DeepCopy(), data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(context.Context) {
		defer cancel()
		work(operationCtx, snapshot, result, log)
		operation := result.operation
		result.operation = nil
		queued := r.processData.QueueDeferredOpForStateKey(snapshot.NamespacedName(), snapshot.UID,
			func(name types.NamespacedName, stateKey types.UID, activeResource *apiv2.LogicalProcess) {
				_, active := r.processData.BorrowByNamespacedName(name)
				if activeResource.UID == snapshot.UID && active != nil && active.operation == operation {
					if state == logicalProcessStatePersistence {
						active.operation, active.lease = nil, result.lease
						active.leaseFailureMessage, active.retryAfter = result.leaseFailureMessage, result.retryAfter
						_ = r.processData.Update(name, stateKey, active)
					} else {
						_ = r.processData.Update(name, stateKey, result)
					}
				}
			})
		if queued {
			r.ScheduleReconciliation(snapshot.NamespacedName())
		} else if result.lease != nil {
			releaseErr := result.lease.Release(r.LifetimeCtx)
			if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
				log.Error(releaseErr, "Failed to release stale logical process lease")
			}
		}
	})
	if enqueueErr != nil {
		cancel()
		log.Error(enqueueErr, "Failed to queue logical process operation")
		current.operation = nil
		if state == logicalProcessStatePersistence {
			current.leaseFailureMessage = enqueueErr.Error()
		} else if state == logicalProcessStatePhysicalRetiring {
			current.state, current.message = logicalProcessStatePhysicalRetry, enqueueErr.Error()
		} else {
			current.state, current.message = logicalProcessStatePrepareRetry, enqueueErr.Error()
		}
		current.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
	}
	return noChange
}

func (r *LogicalProcessReconciler) prepareLogicalProcess(
	ctx context.Context, resource *apiv2.LogicalProcess, data *logicalProcessData, log logr.Logger,
) {
	if resource.Spec.EffectiveMode() == apiv2.LogicalProcessModeExisting {
		data.lifecycleKey, data.state = resource.Spec.LifecycleKey, logicalProcessStatePrepared
		return
	}
	key, _, keyErr := resource.Spec.GetLifecycleKey()
	if keyErr != nil {
		log.Error(keyErr, "Failed to calculate logical process lifecycle key")
		data.state, data.message = logicalProcessStatePrepareFailed, keyErr.Error()
		return
	}
	data.lifecycleKey = key
	if resource.Spec.EffectiveMode() != apiv2.LogicalProcessModePersistent {
		data.state = logicalProcessStatePrepared
		return
	}
	preparationErr := func() error {
		if r.config.StateStore == nil {
			return errors.New("state store is not configured")
		}
		if data.lease == nil {
			lease, leaseErr := r.config.StateStore.AcquireResourceLease(ctx, resource, r.config.ResourceLeaseOwner, resourceLeaseRevalidationInterval)
			if leaseErr != nil {
				return leaseErr
			}
			data.lease = lease
		}
		record, recordErr := r.config.StateStore.GetPersistentProcess(ctx, resource.GetLeaseKey())
		if errors.Is(recordErr, statestore.ErrPersistentProcessNotFound) {
			data.pendingRecord, data.replaceExisting = nil, false
			return nil
		}
		if recordErr != nil {
			return recordErr
		}
		runningErr := r.processExecutor.CheckProcessRunning(record.ProcessHandle())
		if process.IsProcessGoneErr(runningErr) {
			removeErr := logs.RemoveProcessOutput(ctx, record.StdOutFile, record.StdErrFile)
			if removeErr != nil {
				return removeErr
			}
			return r.config.StateStore.DeletePersistentProcess(ctx, resource.GetLeaseKey())
		}
		if runningErr != nil {
			return runningErr
		}
		data.pendingRecord, data.replaceExisting = record, record.LifecycleKey != key
		return nil
	}()
	if preparationErr != nil {
		log.Error(preparationErr, "Failed to prepare persistent logical process")
		data.state, data.message = logicalProcessStatePrepareRetry, preparationErr.Error()
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return
	}
	data.state = logicalProcessStatePrepared
}

func (r *LogicalProcessReconciler) persistLogicalProcess(
	ctx context.Context, resource *apiv2.LogicalProcess, data *logicalProcessData, log logr.Logger,
) {
	persistErr := func() error {
		verifyErr := r.config.StateStore.VerifyResourceLeaseHeld(ctx, resource, r.config.ResourceLeaseOwner)
		if verifyErr != nil {
			return verifyErr
		}
		if data.state == logicalProcessStateMissing {
			// The backing resource was lost; release startup coordination without reconstructing runtime ownership.
			releaseErr := data.lease.Release(ctx)
			if releaseErr != nil {
				return releaseErr
			}
			data.lease = nil
			return nil
		}
		physical, getErr := r.getLogicalPhysicalProcess(ctx, resource, data.physicalUID, "")
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			return getErr
		}
		if getErr == nil {
			if !logicalPhysicalProcessStartupSettled(physical) {
				return errors.New("waiting for retained physical process startup to settle")
			}
			if physical.Status.PID != nil && !physical.Status.IdentityTimestamp.IsZero() {
				workloadID, resolveErr := dcpclient.ResolveNamespaceWorkloadID(ctx, r.NoCacheClient, resource.Namespace, r.config.WorkloadID)
				if resolveErr != nil {
					return resolveErr
				}
				pid, pidErr := process.Int64_ToPidT(*physical.Status.PID)
				if pidErr != nil {
					return pidErr
				}
				record, readErr := r.config.StateStore.GetPersistentProcess(ctx, resource.GetLeaseKey())
				if readErr != nil && !errors.Is(readErr, statestore.ErrPersistentProcessNotFound) {
					return readErr
				}
				if record == nil || record.PID != pid ||
					!osutil.Within(record.IdentityTime, physical.Status.IdentityTimestamp.Time, process.ProcessIdentityTimeMaximumDifference) {
					if record != nil {
						runningErr := r.processExecutor.CheckProcessRunning(record.ProcessHandle())
						if runningErr == nil {
							return errors.New("cannot overwrite the record of a running persistent process")
						}
						if !process.IsProcessGoneErr(runningErr) {
							return runningErr
						}
						removeErr := logs.RemoveProcessOutput(ctx, record.StdOutFile, record.StdErrFile)
						if removeErr != nil {
							return removeErr
						}
					}
					record = &statestore.PersistentProcessRecord{RunID: string(physical.UID)}
				}
				record.ResourceKey, record.LifecycleKey = resource.GetLeaseKey(), data.lifecycleKey
				record.PID, record.IdentityTime = pid, physical.Status.IdentityTimestamp.Time
				record.WorkloadID = workloadID
				record.StdOutFile, record.StdErrFile = physical.Status.StdoutFile, physical.Status.StderrFile
				storeErr := r.config.StateStore.UpsertPersistentProcess(ctx, *record)
				if storeErr != nil {
					return storeErr
				}
			} else if physical.Spec.Process != nil {
				deleteErr := r.config.StateStore.DeletePersistentProcess(ctx, resource.GetLeaseKey())
				if deleteErr != nil {
					return deleteErr
				}
			}
		} else if data.replacementUID != "" {
			previous, previousErr := r.getLogicalPhysicalProcess(ctx, resource, data.replacementUID, "-previous")
			if previousErr != nil && !apierrors.IsNotFound(previousErr) {
				return previousErr
			}
			if previousErr == nil && (!logicalPhysicalProcessObserved(previous) || previous.Status.Phase != apiv2.PhysicalProcessPhaseExited) {
				return errors.New("waiting for the previous physical process stop to settle")
			}
		}
		releaseErr := data.lease.Release(ctx)
		if releaseErr != nil {
			return releaseErr
		}
		data.lease = nil
		return nil
	}()
	if persistErr != nil {
		log.Error(persistErr, "Failed to settle persistent logical process ownership")
		data.leaseFailureMessage = persistErr.Error()
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return
	}
}

func (r *LogicalProcessReconciler) discardLogicalProcessData(name types.NamespacedName, log logr.Logger) {
	uid, data := r.processData.BorrowByNamespacedName(name)
	if data == nil {
		return
	}
	if data.operation != nil {
		data.operation.cancel()
	}
	missing := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{UID: uid}}
	defer logger.ReleaseResourceLog(missing.GetResourceId())
	r.processData.DeleteByNamespacedName(name)
	if data.lease != nil {
		enqueueErr := r.operationQueue.Enqueue(func(ctx context.Context) {
			releaseErr := data.lease.Release(ctx)
			if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
				log.Error(releaseErr, "Failed to release discarded logical process lease")
			}
		})
		if enqueueErr != nil {
			log.Error(enqueueErr, "Failed to queue discarded logical process lease release")
		}
	}
}
