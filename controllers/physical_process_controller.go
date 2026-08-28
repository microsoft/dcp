/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

var (
	physicalProcessFinalizer = fmt.Sprintf("%s/physicalprocess-reconciler", apiv2.GroupVersion.Group)

	physicalProcessDataInitializers = map[apiv2.ConditionReason]func(
		context.Context,
		*PhysicalProcessReconciler,
		*apiv2.PhysicalProcess,
		physicalProcessDataStateKey,
		*physicalProcessData,
		logr.Logger,
	) objectChange{
		apiv2.PhysicalProcessReasonLaunching:             handlePhysicalProcessOperationInProgress,
		apiv2.PhysicalProcessReasonLaunchFailed:          handlePhysicalProcessLaunchFailed,
		apiv2.PhysicalProcessReasonRuntimeProcessRunning: handlePhysicalProcessStableState,
		apiv2.PhysicalProcessReasonRuntimeProcessExited:  handlePhysicalProcessStableState,
		apiv2.PhysicalProcessReasonRuntimeProcessMissing: handlePhysicalProcessStableState,
		apiv2.PhysicalProcessReasonStopping:              handlePhysicalProcessOperationInProgress,
		apiv2.PhysicalProcessReasonStopFailed:            handlePhysicalProcessStableState,
	}
)

type PhysicalProcessReconciler struct {
	*ReconcilerBase[apiv2.PhysicalProcess, *apiv2.PhysicalProcess]

	processExecutor process.Executor
	processData     *ObjectStateMap[physicalProcessDataStateKey, physicalProcessData, *physicalProcessData, *apiv2.PhysicalProcess]
	operationQueue  *resiliency.WorkQueue

	retainedLaunchLock        sync.Mutex
	pendingRetainedLaunches   map[physicalProcessOwner]process.ProcessHandle
	shuttingDown              bool
	retainedLaunchCleanupDone chan struct{}
}

type physicalProcessOwner struct {
	name        types.NamespacedName
	resourceUID types.UID
}

func NewPhysicalProcessReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	processExecutor process.Executor,
) *PhysicalProcessReconciler {
	reconciler := &PhysicalProcessReconciler{
		ReconcilerBase:            NewReconcilerBase[apiv2.PhysicalProcess](client, noCacheClient, log, lifetimeCtx),
		processExecutor:           processExecutor,
		processData:               NewObjectStateMap[physicalProcessDataStateKey, physicalProcessData, *physicalProcessData, *apiv2.PhysicalProcess](),
		operationQueue:            resiliency.NewWorkQueue(lifetimeCtx, MaxConcurrentReconciles),
		pendingRetainedLaunches:   map[physicalProcessOwner]process.ProcessHandle{},
		retainedLaunchCleanupDone: make(chan struct{}),
	}
	_ = context.AfterFunc(lifetimeCtx, func() {
		reconciler.stopPendingRetainedLaunches()
		close(reconciler.retainedLaunchCleanupDone)
	})
	return reconciler
}

func (r *PhysicalProcessReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.PhysicalProcess{}).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNamespace), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *PhysicalProcessReconciler) requestReconcileForNamespace(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	namespace := obj.(*apiv2.Namespace)
	var processList apiv2.PhysicalProcessList
	listErr := r.List(ctx, &processList, ctrl_client.InNamespace(namespace.Name))
	if listErr != nil {
		r.Log.Error(listErr, "Failed to list PhysicalProcesses for namespace", "Namespace", namespace.Name)
		return nil
	}

	requests := make([]reconcile.Request, len(processList.Items))
	for i := range processList.Items {
		requests[i] = reconcile.Request{NamespacedName: processList.Items[i].NamespacedName()}
	}
	return requests
}

func (r *PhysicalProcessReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)
	if ctx.Err() != nil {
		return ctrl.Result{}, nil
	}

	physicalProcess := apiv2.PhysicalProcess{}
	getErr := reader.Get(ctx, req.NamespacedName, &physicalProcess)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			r.processData.DeleteByNamespacedName(req.NamespacedName)
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to Get() the PhysicalProcess")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)

	r.processData.RunDeferredOps(req.NamespacedName, &physicalProcess)
	patch := ctrl_client.MergeFromWithOptions(physicalProcess.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	var change objectChange
	if physicalProcess.DeletionTimestamp != nil && !physicalProcess.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(&physicalProcess, log)
	} else if change = ensureFinalizer(&physicalProcess, physicalProcessFinalizer, log); change == noChange {
		change = r.managePhysicalProcess(ctx, &physicalProcess, log)
	}

	return r.SaveChangesWithDelay(
		ctx,
		&physicalProcess,
		patch,
		change,
		physicalProcessReconcileDelay(&physicalProcess),
		r.onPhysicalProcessStatusDurable(&physicalProcess),
		log,
	)
}

func physicalProcessReconcileDelay(physicalProcess *apiv2.PhysicalProcess) AdditionalReconciliationDelay {
	readyCondition := apimeta.FindStatusCondition(physicalProcess.Status.Conditions, string(apiv2.ConditionReady))
	if readyCondition != nil {
		switch apiv2.ConditionReason(readyCondition.Reason) {
		case apiv2.PhysicalProcessReasonLaunchFailed,
			apiv2.PhysicalProcessReasonRuntimeProcessInspectFailed,
			apiv2.PhysicalProcessReasonRuntimeProcessAlreadyTracked,
			apiv2.PhysicalProcessReasonStopFailed,
			apiv2.PhysicalResourceReasonNamespaceLookupFailed,
			apiv2.PhysicalResourceReasonOperationStateInvalid:
			return LongDelay
		}
	}
	if physicalProcess.Status.Phase == apiv2.PhysicalProcessPhaseRunning {
		return MonitoringDelay
	}
	return StandardDelay
}

func (r *PhysicalProcessReconciler) managePhysicalProcess(ctx context.Context, physicalProcess *apiv2.PhysicalProcess, log logr.Logger) objectChange {
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, r.Client, physicalProcess.Namespace)
	if !namespaceReady {
		namespacePhase := apiv2.PhysicalProcessPhasePending
		namespaceMessage := namespaceReadinessMessage(physicalProcess.Namespace, namespaceReason)
		change := noChange
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get namespace", "Namespace", physicalProcess.Namespace)
			namespacePhase = apiv2.PhysicalProcessPhaseUnknown
			namespaceMessage = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
			change |= additionalReconciliationNeeded
		}
		change |= setValue(&physicalProcess.Status.Phase, namespacePhase)
		change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, namespaceReason, namespaceMessage)
		return change
	}

	stateKey, data := r.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
	if data != nil {
		change := data.applyTo(physicalProcess)
		initializer, found := physicalProcessDataInitializers[data.conditionReason]
		if !found {
			r.processData.DeleteByNamespacedName(physicalProcess.NamespacedName())
			message := fmt.Sprintf("Physical process operation reached unknown condition reason %q.", data.conditionReason)
			log.Error(fmt.Errorf("unknown physical process condition reason %q", data.conditionReason), "Physical process operation reached unknown condition reason")
			change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseUnknown)
			change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, apiv2.PhysicalResourceReasonOperationStateInvalid, message)
			return change | additionalReconciliationNeeded
		}
		change |= initializer(ctx, r, physicalProcess, stateKey, data, log)
		if data.operationInProgress() ||
			data.conditionReason == apiv2.PhysicalProcessReasonLaunchFailed ||
			data.conditionReason == apiv2.PhysicalProcessReasonRuntimeProcessExited ||
			data.conditionReason == apiv2.PhysicalProcessReasonRuntimeProcessMissing {
			return change
		}
	}

	if data == nil {
		var establishChange objectChange
		stateKey, data, establishChange = r.establishPhysicalProcessData(physicalProcess, log)
		if data == nil {
			return establishChange
		}
	}

	runningErr := r.checkPhysicalProcessRunning(data)
	if process.IsProcessGoneErr(runningErr) {
		updatedData := data.Clone()
		updatedData.conditionReason = apiv2.PhysicalProcessReasonRuntimeProcessMissing
		updatedData.progress = physicalProcessOperationCompleted
		updatedData.finishedAt = time.Now()
		_ = r.processData.Update(physicalProcess.NamespacedName(), stateKey, updatedData)
		return updatedData.applyTo(physicalProcess)
	}
	if runningErr != nil {
		log.Error(runningErr, "Failed to inspect runtime process", "PID", data.handle.Pid)
		change := setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseUnknown)
		change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, apiv2.PhysicalProcessReasonRuntimeProcessInspectFailed, fmt.Sprintf("Failed to inspect runtime process: %v", runningErr))
		return change | additionalReconciliationNeeded
	}

	if physicalProcess.Spec.Stop {
		return r.schedulePhysicalProcessStop(physicalProcess, stateKey, data, log)
	}

	updatedData := data.Clone()
	updatedData.conditionReason = apiv2.PhysicalProcessReasonRuntimeProcessRunning
	updatedData.progress = physicalProcessOperationCompleted
	_ = r.processData.Update(physicalProcess.NamespacedName(), stateKey, updatedData)
	change := updatedData.applyTo(physicalProcess)
	change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseRunning)
	change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionTrue, apiv2.PhysicalProcessReasonRuntimeProcessRunning, "Runtime process is running.")
	return change | additionalReconciliationNeeded
}

func (r *PhysicalProcessReconciler) establishPhysicalProcessData(
	physicalProcess *apiv2.PhysicalProcess,
	log logr.Logger,
) (physicalProcessDataStateKey, *physicalProcessData, objectChange) {
	if physicalProcess.Spec.Process != nil && physicalProcess.Status.PID == nil {
		return physicalProcessDataKey(physicalProcess), nil, r.schedulePhysicalProcessLaunch(physicalProcess, physicalProcessDataKey(physicalProcess), nil, log)
	}

	pidValue := physicalProcess.Status.PID
	if pidValue == nil {
		pidValue = physicalProcess.Spec.PID
	}
	if pidValue == nil {
		return "", nil, additionalReconciliationNeeded
	}

	pid, pidErr := process.Int64_ToPidT(*pidValue)
	if pidErr != nil {
		change := setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseFailed)
		change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, apiv2.PhysicalResourceReasonOperationStateInvalid, fmt.Sprintf("Invalid process ID: %v", pidErr))
		return "", nil, change
	}

	identityTime := physicalProcess.Status.IdentityTimestamp.Time
	if identityTime.IsZero() {
		identityTime = process.ProcessIdentityTime(pid)
	}
	if identityTime.IsZero() {
		probedProcess, probeErr := process.FindProcess(process.NewHandle(pid, time.Time{}))
		if process.IsProcessGoneErr(probeErr) {
			change := setPhysicalProcessPID(&physicalProcess.Status.PID, *pidValue)
			change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseExited)
			change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, apiv2.PhysicalProcessReasonRuntimeProcessMissing, "Runtime process was not found.")
			return "", nil, change
		}
		if probeErr != nil {
			change := setPhysicalProcessPID(&physicalProcess.Status.PID, *pidValue)
			change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseUnknown)
			change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, apiv2.PhysicalProcessReasonRuntimeProcessInspectFailed, fmt.Sprintf("Failed to inspect runtime process: %v", probeErr))
			return "", nil, change | additionalReconciliationNeeded
		}
		releaseErr := probedProcess.Release()
		if releaseErr != nil {
			change := setPhysicalProcessPID(&physicalProcess.Status.PID, *pidValue)
			change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseUnknown)
			change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, apiv2.PhysicalProcessReasonRuntimeProcessInspectFailed, fmt.Sprintf("Failed to release runtime process handle: %v", releaseErr))
			return "", nil, change | additionalReconciliationNeeded
		}
		change := setPhysicalProcessPID(&physicalProcess.Status.PID, *pidValue)
		change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseUnknown)
		change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, apiv2.PhysicalProcessReasonRuntimeProcessInspectFailed, "Failed to determine the runtime process identity timestamp.")
		return "", nil, change | additionalReconciliationNeeded
	}
	handle := process.NewHandle(pid, identityTime)
	data := &physicalProcessData{
		resourceUID:     physicalProcess.UID,
		conditionReason: apiv2.PhysicalProcessReasonRuntimeProcessRunning,
		progress:        physicalProcessOperationCompleted,
		handle:          handle,
		created:         physicalProcess.Spec.Process != nil,
	}
	stateKey := physicalProcessHandleDataKey(handle)
	owner, stored := r.processData.StoreIfStateKeyUnclaimed(physicalProcess.NamespacedName(), stateKey, data)
	if !stored {
		change := setPhysicalProcessPID(&physicalProcess.Status.PID, *pidValue)
		change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhasePending)
		change |= setCondition(
			&physicalProcess.Status.Conditions,
			apiv2.ConditionReady,
			physicalProcess.Generation,
			metav1.ConditionFalse,
			apiv2.PhysicalProcessReasonRuntimeProcessAlreadyTracked,
			fmt.Sprintf("Runtime process is already tracked by PhysicalProcess %q.", owner.String()),
		)
		return "", nil, change | additionalReconciliationNeeded
	}
	return stateKey, data, data.applyTo(physicalProcess)
}

func handlePhysicalProcessOperationInProgress(
	_ context.Context,
	_ *PhysicalProcessReconciler,
	_ *apiv2.PhysicalProcess,
	_ physicalProcessDataStateKey,
	_ *physicalProcessData,
	_ logr.Logger,
) objectChange {
	return noChange
}

func handlePhysicalProcessStableState(
	_ context.Context,
	_ *PhysicalProcessReconciler,
	_ *apiv2.PhysicalProcess,
	_ physicalProcessDataStateKey,
	_ *physicalProcessData,
	_ logr.Logger,
) objectChange {
	return noChange
}

func handlePhysicalProcessLaunchFailed(
	_ context.Context,
	reconciler *PhysicalProcessReconciler,
	physicalProcess *apiv2.PhysicalProcess,
	stateKey physicalProcessDataStateKey,
	data *physicalProcessData,
	log logr.Logger,
) objectChange {
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	if data.handle.Pid > 0 {
		return reconciler.scheduleInvalidPhysicalProcessCleanup(physicalProcess, stateKey, data, log)
	}
	return reconciler.schedulePhysicalProcessLaunch(physicalProcess, stateKey, data, log)
}

func (r *PhysicalProcessReconciler) schedulePhysicalProcessLaunch(
	physicalProcess *apiv2.PhysicalProcess,
	stateKey physicalProcessDataStateKey,
	currentData *physicalProcessData,
	log logr.Logger,
) objectChange {
	data := &physicalProcessData{
		resourceUID:     physicalProcess.UID,
		conditionReason: apiv2.PhysicalProcessReasonLaunching,
		progress:        physicalProcessOperationInProgress,
		created:         true,
	}
	if currentData == nil {
		r.processData.Store(physicalProcess.NamespacedName(), stateKey, data)
	} else if !r.processData.Update(physicalProcess.NamespacedName(), stateKey, data) {
		return additionalReconciliationNeeded
	}

	processSnapshot := physicalProcess.DeepCopy()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.launchPhysicalProcess(operationCtx, processSnapshot, stateKey, data.Clone(), log)
	})
	if enqueueErr != nil {
		failedData := data.Clone()
		failedData.conditionReason = apiv2.PhysicalProcessReasonLaunchFailed
		failedData.progress = physicalProcessOperationRetryPending
		failedData.failureMessage = fmt.Sprintf("Failed to queue physical process launch: %v", enqueueErr)
		failedData.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		_ = r.processData.Update(physicalProcess.NamespacedName(), stateKey, failedData)
		return failedData.applyTo(physicalProcess) | additionalReconciliationNeeded
	}
	return data.applyTo(physicalProcess)
}

func (r *PhysicalProcessReconciler) launchPhysicalProcess(
	operationCtx context.Context,
	physicalProcess *apiv2.PhysicalProcess,
	stateKey physicalProcessDataStateKey,
	data *physicalProcessData,
	log logr.Logger,
) {
	processConfig := physicalProcess.Spec.Process
	cmd := exec.Command(processConfig.ExecutablePath, processConfig.Args...)
	cmd.Dir = processConfig.WorkingDirectory
	cmd.Env = physicalProcessEnvironment(processConfig)
	if osutil.IsWindows() {
		process.ForkFromParent(cmd)
	}

	processCtx := r.LifetimeCtx
	var creationFlags process.ProcessCreationFlag = process.CreationFlagEnsureKillOnDispose

	var handle process.ProcessHandle
	var reportExit atomic.Bool
	reportExit.Store(true)
	exitHandler := process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, exitErr error) {
		if reportExit.Load() {
			r.processExited(physicalProcess.NamespacedName(), physicalProcess.UID, handle, pid, exitCode, exitErr)
		}
	})
	var startedHandle process.ProcessHandle
	var startWaitForExit func()
	var startErr error
	if processConfig.RetainRuntimeProcess {
		r.retainedLaunchLock.Lock()
		if r.shuttingDown || operationCtx.Err() != nil {
			r.retainedLaunchLock.Unlock()
			return
		}
		processCtx = context.WithoutCancel(r.LifetimeCtx)
		creationFlags = process.CreationFlagsNone
		startedHandle, startWaitForExit, startErr = r.processExecutor.StartProcess(processCtx, cmd, exitHandler, creationFlags, nil)
		if startErr == nil && startedHandle.Pid > 0 && !startedHandle.IdentityTime.IsZero() {
			owner := physicalProcessOwner{name: physicalProcess.NamespacedName(), resourceUID: physicalProcess.UID}
			r.pendingRetainedLaunches[owner] = startedHandle
		}
		r.retainedLaunchLock.Unlock()
	} else {
		startedHandle, startWaitForExit, startErr = r.processExecutor.StartProcess(processCtx, cmd, exitHandler, creationFlags, nil)
	}
	handle = startedHandle
	if startErr != nil {
		data.conditionReason = apiv2.PhysicalProcessReasonLaunchFailed
		data.progress = physicalProcessOperationRetryPending
		data.failureMessage = fmt.Sprintf("Failed to launch physical process: %v", startErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		r.queuePhysicalProcessDataResult(physicalProcess, stateKey, data)
		return
	}
	if handle.Pid <= 0 || handle.IdentityTime.IsZero() {
		reportExit.Store(false)
		data.handle = handle
		data.conditionReason = apiv2.PhysicalProcessReasonLaunchFailed
		data.progress = physicalProcessOperationRetryPending
		data.failureMessage = "Physical process launch returned an invalid process identity."
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		if handle.Pid > 0 {
			cleanupErr := r.processExecutor.StopProcess(handle)
			if cleanupErr == nil || process.IsProcessGoneErr(cleanupErr) {
				data.handle = process.ProcessHandle{}
			} else {
				data.failureMessage = fmt.Sprintf("Physical process launch returned an invalid process identity and cleanup failed: %v", cleanupErr)
			}
		}
		r.queuePhysicalProcessDataResult(physicalProcess, stateKey, data)
		if startWaitForExit != nil {
			startWaitForExit()
		}
		return
	}

	data.handle = handle
	data.conditionReason = apiv2.PhysicalProcessReasonRuntimeProcessRunning
	data.progress = physicalProcessOperationCompleted
	data.failureMessage = ""
	data.retryAfter = time.Time{}
	r.queuePhysicalProcessDataResult(physicalProcess, stateKey, data)
	if startWaitForExit != nil {
		startWaitForExit()
	}
	log.V(1).Info("Physical process launched", "PID", handle.Pid, "ExecutablePath", processConfig.ExecutablePath)
}

func (r *PhysicalProcessReconciler) queuePhysicalProcessDataResult(
	physicalProcess *apiv2.PhysicalProcess,
	stateKey physicalProcessDataStateKey,
	result *physicalProcessData,
) {
	queued := r.processData.QueueDeferredOpForStateKey(physicalProcess.NamespacedName(), stateKey, func(name types.NamespacedName, currentStateKey physicalProcessDataStateKey, _ *apiv2.PhysicalProcess) {
		resultToStore := result
		_, currentData := r.processData.BorrowByNamespacedName(name)
		if currentData != nil &&
			result.conditionReason == apiv2.PhysicalProcessReasonRuntimeProcessExited &&
			result.exitCode == nil &&
			currentData.exitCode != nil {
			resultToStore = result.Clone()
			resultToStore.exitCode = cloneInt32Pointer(currentData.exitCode)
		}
		newStateKey := currentStateKey
		if resultToStore.handle.Pid > 0 {
			newStateKey = physicalProcessHandleDataKey(resultToStore.handle)
		}
		if newStateKey != currentStateKey {
			_ = r.processData.UpdateChangingStateKey(name, currentStateKey, newStateKey, resultToStore)
		} else {
			_ = r.processData.Update(name, currentStateKey, resultToStore)
		}
	})
	if queued {
		r.ScheduleReconciliation(physicalProcess.NamespacedName())
	}
}

func (r *PhysicalProcessReconciler) processExited(
	name types.NamespacedName,
	resourceUID types.UID,
	expectedHandle process.ProcessHandle,
	pid process.Pid_t,
	exitCode int32,
	exitErr error,
) {
	queued := r.processData.QueueDeferredOp(name, func(currentName types.NamespacedName, stateKey physicalProcessDataStateKey, _ *apiv2.PhysicalProcess) {
		currentStateKey, currentData := r.processData.BorrowByNamespacedName(currentName)
		if currentData == nil || currentData.resourceUID != resourceUID ||
			currentData.handle != expectedHandle || expectedHandle.Pid != pid {
			return
		}
		updatedData := currentData.Clone()
		updatedData.conditionReason = apiv2.PhysicalProcessReasonRuntimeProcessExited
		updatedData.progress = physicalProcessOperationCompleted
		updatedData.finishedAt = time.Now()
		updatedData.failureMessage = ""
		if exitErr == nil && exitCode != process.UnknownExitCode {
			updatedData.exitCode = &exitCode
		}
		_ = r.processData.Update(currentName, currentStateKey, updatedData)
	})
	if queued {
		r.ScheduleReconciliation(name)
	}
}

func (r *PhysicalProcessReconciler) checkPhysicalProcessRunning(data *physicalProcessData) error {
	if data.created {
		return r.processExecutor.CheckProcessRunning(data.handle)
	}
	osProcess, findErr := process.FindProcess(data.handle)
	if findErr != nil {
		return findErr
	}
	return osProcess.Release()
}

func (r *PhysicalProcessReconciler) schedulePhysicalProcessStop(
	physicalProcess *apiv2.PhysicalProcess,
	stateKey physicalProcessDataStateKey,
	data *physicalProcessData,
	log logr.Logger,
) objectChange {
	if data.conditionReason == apiv2.PhysicalProcessReasonStopFailed && time.Now().Before(data.retryAfter) {
		return data.applyTo(physicalProcess) | additionalReconciliationNeeded
	}

	stoppingData := data.Clone()
	stoppingData.conditionReason = apiv2.PhysicalProcessReasonStopping
	stoppingData.progress = physicalProcessOperationInProgress
	stoppingData.failureMessage = ""
	if !r.processData.Update(physicalProcess.NamespacedName(), stateKey, stoppingData) {
		return additionalReconciliationNeeded
	}

	processSnapshot := physicalProcess.DeepCopy()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.stopPhysicalProcess(operationCtx, processSnapshot, stateKey, stoppingData.Clone(), log)
	})
	if enqueueErr != nil {
		stoppingData.conditionReason = apiv2.PhysicalProcessReasonStopFailed
		stoppingData.progress = physicalProcessOperationRetryPending
		stoppingData.failureMessage = fmt.Sprintf("Failed to queue physical process termination: %v", enqueueErr)
		stoppingData.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		_ = r.processData.Update(physicalProcess.NamespacedName(), stateKey, stoppingData)
		return stoppingData.applyTo(physicalProcess) | additionalReconciliationNeeded
	}
	return stoppingData.applyTo(physicalProcess)
}

func (r *PhysicalProcessReconciler) stopPhysicalProcess(
	_ context.Context,
	physicalProcess *apiv2.PhysicalProcess,
	stateKey physicalProcessDataStateKey,
	data *physicalProcessData,
	log logr.Logger,
) {
	stopErr := r.processExecutor.StopProcess(data.handle)
	if stopErr != nil && !process.IsProcessGoneErr(stopErr) {
		data.conditionReason = apiv2.PhysicalProcessReasonStopFailed
		data.progress = physicalProcessOperationRetryPending
		data.failureMessage = fmt.Sprintf("Failed to stop physical process: %v", stopErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		r.queuePhysicalProcessDataResult(physicalProcess, stateKey, data)
		return
	}

	data.conditionReason = apiv2.PhysicalProcessReasonRuntimeProcessExited
	data.progress = physicalProcessOperationCompleted
	data.finishedAt = time.Now()
	data.failureMessage = ""
	data.retryAfter = time.Time{}
	r.queuePhysicalProcessDataResult(physicalProcess, stateKey, data)
	log.V(1).Info("Physical process stopped", "PID", data.handle.Pid)
}

func (r *PhysicalProcessReconciler) scheduleInvalidPhysicalProcessCleanup(
	physicalProcess *apiv2.PhysicalProcess,
	stateKey physicalProcessDataStateKey,
	data *physicalProcessData,
	log logr.Logger,
) objectChange {
	cleanupData := data.Clone()
	cleanupData.progress = physicalProcessOperationInProgress
	if !r.processData.Update(physicalProcess.NamespacedName(), stateKey, cleanupData) {
		return additionalReconciliationNeeded
	}

	processSnapshot := physicalProcess.DeepCopy()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.cleanupInvalidPhysicalProcess(operationCtx, processSnapshot, stateKey, cleanupData.Clone(), log)
	})
	if enqueueErr != nil {
		cleanupData.progress = physicalProcessOperationRetryPending
		cleanupData.failureMessage = fmt.Sprintf("Failed to queue invalid physical process cleanup: %v", enqueueErr)
		cleanupData.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		_ = r.processData.Update(physicalProcess.NamespacedName(), stateKey, cleanupData)
		return cleanupData.applyTo(physicalProcess) | additionalReconciliationNeeded
	}
	return cleanupData.applyTo(physicalProcess)
}

func (r *PhysicalProcessReconciler) cleanupInvalidPhysicalProcess(
	_ context.Context,
	physicalProcess *apiv2.PhysicalProcess,
	stateKey physicalProcessDataStateKey,
	data *physicalProcessData,
	log logr.Logger,
) {
	cleanupErr := r.processExecutor.StopProcess(data.handle)
	if cleanupErr != nil && !process.IsProcessGoneErr(cleanupErr) {
		data.progress = physicalProcessOperationRetryPending
		data.failureMessage = fmt.Sprintf("Failed to clean up process with an invalid identity: %v", cleanupErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		r.queuePhysicalProcessDataResult(physicalProcess, stateKey, data)
		return
	}

	log.V(1).Info("Cleaned up process with invalid identity", "PID", data.handle.Pid)
	data.handle = process.ProcessHandle{}
	data.progress = physicalProcessOperationRetryPending
	data.failureMessage = "Physical process launch returned an invalid process identity."
	data.retryAfter = time.Time{}
	r.queuePhysicalProcessDataResult(physicalProcess, stateKey, data)
}

func (r *PhysicalProcessReconciler) handleDeletionRequest(physicalProcess *apiv2.PhysicalProcess, log logr.Logger) objectChange {
	stateKey, data := r.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
	if data == nil && physicalProcess.Status.PID != nil {
		var establishChange objectChange
		stateKey, data, establishChange = r.establishPhysicalProcessData(physicalProcess, log)
		if data == nil {
			return establishChange | additionalReconciliationNeeded
		}
	}
	if data != nil && data.operationInProgress() {
		return additionalReconciliationNeeded
	}

	retain := physicalProcess.Spec.Process == nil ||
		(physicalProcess.Spec.Process != nil && physicalProcess.Spec.Process.RetainRuntimeProcess)
	if retain && data != nil && r.retainedLaunchPending(physicalProcess, data.handle) {
		retain = false
	}
	if data == nil || data.handle.Pid <= 0 || retain ||
		data.conditionReason == apiv2.PhysicalProcessReasonRuntimeProcessExited ||
		data.conditionReason == apiv2.PhysicalProcessReasonRuntimeProcessMissing {
		r.processData.DeleteByNamespacedName(physicalProcess.NamespacedName())
		return deleteFinalizer(physicalProcess, physicalProcessFinalizer, log)
	}

	return r.schedulePhysicalProcessStop(physicalProcess, stateKey, data, log)
}

func handlePIDString(handle process.ProcessHandle) string {
	return strconv.FormatInt(int64(handle.Pid), 10)
}

func physicalProcessEnvironment(processConfig *apiv2.PhysicalProcessConfig) []string {
	environment := make([]string, 0, len(processConfig.Env))
	if processConfig.InheritEnvironment {
		environment = append(environment, os.Environ()...)
	}
	for _, envVar := range processConfig.Env {
		environment = append(environment, envVar.Name+"="+envVar.Value)
	}
	return environment
}

func (r *PhysicalProcessReconciler) onPhysicalProcessStatusDurable(physicalProcess *apiv2.PhysicalProcess) func() {
	if physicalProcess.Spec.Process == nil ||
		!physicalProcess.Spec.Process.RetainRuntimeProcess ||
		physicalProcess.Status.PID == nil {
		return nil
	}

	_, data := r.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
	if data == nil || data.handle.Pid != process.Pid_t(*physicalProcess.Status.PID) {
		return nil
	}
	owner := physicalProcessOwner{name: physicalProcess.NamespacedName(), resourceUID: physicalProcess.UID}
	handle := data.handle
	return func() {
		r.retainedLaunchLock.Lock()
		defer r.retainedLaunchLock.Unlock()
		if pendingHandle, found := r.pendingRetainedLaunches[owner]; found && pendingHandle == handle {
			delete(r.pendingRetainedLaunches, owner)
		}
	}
}

func (r *PhysicalProcessReconciler) stopPendingRetainedLaunches() {
	r.retainedLaunchLock.Lock()
	r.shuttingDown = true
	pendingLaunches := r.pendingRetainedLaunches
	r.pendingRetainedLaunches = map[physicalProcessOwner]process.ProcessHandle{}
	r.retainedLaunchLock.Unlock()

	for owner, handle := range pendingLaunches {
		stopErr := r.processExecutor.StopProcess(handle)
		if stopErr != nil && !process.IsProcessGoneErr(stopErr) {
			r.Log.Error(stopErr, "Failed to stop retained process before its identity became durable", "PhysicalProcess", owner.name, "PID", handle.Pid)
		}
	}
}

func (r *PhysicalProcessReconciler) retainedLaunchPending(
	physicalProcess *apiv2.PhysicalProcess,
	handle process.ProcessHandle,
) bool {
	owner := physicalProcessOwner{name: physicalProcess.NamespacedName(), resourceUID: physicalProcess.UID}
	r.retainedLaunchLock.Lock()
	defer r.retainedLaunchLock.Unlock()
	pendingHandle, found := r.pendingRetainedLaunches[owner]
	return found && pendingHandle == handle
}

// WaitForRetainedLaunchCleanup waits until retained processes without durable identities are stopped.
func (r *PhysicalProcessReconciler) WaitForRetainedLaunchCleanup() {
	<-r.retainedLaunchCleanupDone
}
