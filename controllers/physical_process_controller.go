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

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dcpproc"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

const physicalProcessStopTimeout = 15 * time.Second

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
}

func NewPhysicalProcessReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	processExecutor process.Executor,
) *PhysicalProcessReconciler {
	return &PhysicalProcessReconciler{
		ReconcilerBase:  NewReconcilerBase[apiv2.PhysicalProcess](client, noCacheClient, log, lifetimeCtx),
		processExecutor: processExecutor,
		processData:     NewObjectStateMap[physicalProcessDataStateKey, physicalProcessData, *physicalProcessData, *apiv2.PhysicalProcess](),
		operationQueue:  resiliency.NewWorkQueue(lifetimeCtx, MaxConcurrentReconciles),
	}
}

func (r *PhysicalProcessReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.PhysicalProcess{}).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNamespace(&apiv2.PhysicalProcessList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
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
		nil,
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
		if !data.shouldInspectRuntimeProcess() {
			return change
		}
	}

	if data == nil {
		var establishChange objectChange
		stateKey, data, establishChange = r.establishPhysicalProcessTracking(physicalProcess, log)
		// A nil result means that tracking could not or should not be established and the returned
		// change fully describes the pending or terminal state for this reconciliation.
		if data == nil {
			return establishChange
		}
	}

	runningErr := r.processExecutor.CheckProcessRunning(data.handle)
	if process.IsProcessGoneErr(runningErr) {
		data.conditionReason = apiv2.PhysicalProcessReasonRuntimeProcessMissing
		data.progress = physicalProcessOperationCompleted
		data.finishedAt = time.Now()
		_ = r.processData.Update(physicalProcess.NamespacedName(), stateKey, data)
		return data.applyTo(physicalProcess)
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

	data.conditionReason = apiv2.PhysicalProcessReasonRuntimeProcessRunning
	data.progress = physicalProcessOperationCompleted
	_ = r.processData.Update(physicalProcess.NamespacedName(), stateKey, data)
	change := data.applyTo(physicalProcess)
	change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseRunning)
	change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionTrue, apiv2.PhysicalProcessReasonRuntimeProcessRunning, "Runtime process is running.")
	return change | additionalReconciliationNeeded
}

// establishPhysicalProcessTracking claims and initializes tracking state for a runtime process.
// A nil data result means that the returned change schedules work or applies the complete pending
// or terminal status for this reconciliation.
func (r *PhysicalProcessReconciler) establishPhysicalProcessTracking(
	physicalProcess *apiv2.PhysicalProcess,
	log logr.Logger,
) (physicalProcessDataStateKey, *physicalProcessData, objectChange) {
	if physicalProcess.Spec.Process != nil && physicalProcess.Status.PID == nil {
		if physicalProcess.Spec.Stop {
			return "", nil, applyPhysicalProcessLaunchSkippedStatus(physicalProcess)
		}
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
		readyCondition := apimeta.FindStatusCondition(physicalProcess.Status.Conditions, string(apiv2.ConditionReady))
		if physicalProcess.Status.PID != nil &&
			readyCondition != nil &&
			apiv2.ConditionReason(readyCondition.Reason) == apiv2.PhysicalProcessReasonRuntimeProcessMissing {
			change := setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseExited)
			change |= setCondition(
				&physicalProcess.Status.Conditions,
				apiv2.ConditionReady,
				physicalProcess.Generation,
				metav1.ConditionFalse,
				apiv2.PhysicalProcessReasonRuntimeProcessMissing,
				"Runtime process was not found.",
			)
			return "", nil, change
		}

		probedHandle, probeErr := r.processExecutor.FindProcessHandle(pid)
		if process.IsProcessGoneErr(probeErr) {
			change := setPhysicalProcessPID(&physicalProcess.Status.PID, *pidValue)
			change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseExited)
			change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, apiv2.PhysicalProcessReasonRuntimeProcessMissing, "Runtime process was not found.")
			return "", nil, change
		}
		if probeErr != nil {
			log.Error(probeErr, "Failed to inspect runtime process", "PID", pid)
			change := setPhysicalProcessPID(&physicalProcess.Status.PID, *pidValue)
			change |= setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseUnknown)
			change |= setCondition(&physicalProcess.Status.Conditions, apiv2.ConditionReady, physicalProcess.Generation, metav1.ConditionFalse, apiv2.PhysicalProcessReasonRuntimeProcessInspectFailed, fmt.Sprintf("Failed to inspect runtime process: %v", probeErr))
			return "", nil, change | additionalReconciliationNeeded
		}
		identityTime = probedHandle.IdentityTime
	}
	if identityTime.IsZero() {
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
	}
	stateKey := physicalProcessHandleDataKey(handle)
	owner, stored := r.processData.StoreIfStateKeyUnclaimed(physicalProcess.NamespacedName(), stateKey, data)
	if !stored {
		change := setPhysicalProcessPID(&physicalProcess.Status.PID, *pidValue)
		change |= setTimestamp(&physicalProcess.Status.IdentityTimestamp, metav1.NewMicroTime(handle.IdentityTime))
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
	return stateKey, data.Clone(), data.applyTo(physicalProcess)
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
	if data.handle.Pid > 0 {
		if !physicalProcess.Spec.Stop && time.Now().Before(data.retryAfter) {
			return additionalReconciliationNeeded
		}
		return reconciler.scheduleInvalidPhysicalProcessCleanup(physicalProcess, stateKey, data, log)
	}
	if physicalProcess.Spec.Stop {
		reconciler.processData.DeleteByNamespacedName(physicalProcess.NamespacedName())
		return applyPhysicalProcessLaunchSkippedStatus(physicalProcess)
	}
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
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

	// The executor may invoke the exit handler before StartProcess() returns, for example when the
	// process context is already done, so the launched process identity is published atomically.
	// Exits reported before the identity is published, or for a launch that is being abandoned,
	// belong to no tracked process and are dropped.
	var launchedHandle atomic.Pointer[process.ProcessHandle]
	exitHandler := process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, exitErr error) {
		expectedHandle := launchedHandle.Load()
		if expectedHandle == nil {
			return
		}
		r.processExited(physicalProcess.NamespacedName(), physicalProcess.UID, *expectedHandle, pid, exitCode, exitErr)
	})
	var handle process.ProcessHandle
	var startWaitForExit func()
	var startErr error
	if processConfig.RetainRuntimeProcess {
		if operationCtx.Err() != nil {
			return
		}
		processCtx = context.WithoutCancel(r.LifetimeCtx)
		creationFlags = process.CreationFlagsNone
		handle, startWaitForExit, startErr = r.processExecutor.StartProcess(processCtx, cmd, exitHandler, creationFlags, nil)
	} else {
		handle, startWaitForExit, startErr = r.processExecutor.StartProcess(processCtx, cmd, exitHandler, creationFlags, nil)
	}
	if startErr != nil {
		data.conditionReason = apiv2.PhysicalProcessReasonLaunchFailed
		data.progress = physicalProcessOperationRetryPending
		data.failureMessage = fmt.Sprintf("Failed to launch physical process: %v", startErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		r.queuePhysicalProcessDataResult(physicalProcess, stateKey, data)
		return
	}
	if handle.Pid <= 0 || handle.IdentityTime.IsZero() {
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

	publishedHandle := handle
	launchedHandle.Store(&publishedHandle)
	data.handle = handle
	data.conditionReason = apiv2.PhysicalProcessReasonRuntimeProcessRunning
	data.progress = physicalProcessOperationCompleted
	data.failureMessage = ""
	data.retryAfter = time.Time{}
	if !processConfig.RetainRuntimeProcess {
		dcpproc.RunProcessWatcher(r.processExecutor, handle, log)
	}
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
		currentData.conditionReason = apiv2.PhysicalProcessReasonRuntimeProcessExited
		currentData.progress = physicalProcessOperationCompleted
		currentData.finishedAt = time.Now()
		currentData.failureMessage = ""
		if exitErr == nil && exitCode != process.UnknownExitCode {
			currentData.exitCode = &exitCode
		}
		_ = r.processData.Update(currentName, currentStateKey, currentData)
	})
	if queued {
		r.ScheduleReconciliation(name)
	}
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
	ctx context.Context,
	physicalProcess *apiv2.PhysicalProcess,
	stateKey physicalProcessDataStateKey,
	data *physicalProcessData,
	log logr.Logger,
) {
	var stopErr error
	if osutil.IsWindows() {
		stopCtx, stopCtxCancel := context.WithTimeout(ctx, physicalProcessStopTimeout)
		stopErr = dcpproc.StopProcessTree(stopCtx, r.processExecutor, data.handle, log)
		stopCtxCancel()
	} else {
		stopErr = r.processExecutor.StopProcess(data.handle)
	}
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
	if data != nil && data.operationInProgress() {
		return additionalReconciliationNeeded
	}

	// A resource that never took ownership of a running process has nothing to stop, so deletion
	// only needs to drop the finalizer.
	retain := physicalProcess.Spec.Process == nil || physicalProcess.Spec.Process.RetainRuntimeProcess
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

func applyPhysicalProcessLaunchSkippedStatus(physicalProcess *apiv2.PhysicalProcess) objectChange {
	change := setValue(&physicalProcess.Status.Phase, apiv2.PhysicalProcessPhaseExited)
	change |= setCondition(
		&physicalProcess.Status.Conditions,
		apiv2.ConditionReady,
		physicalProcess.Generation,
		metav1.ConditionFalse,
		apiv2.PhysicalProcessReasonStopRequested,
		"Physical process was not launched because stop was requested.",
	)
	return change
}
