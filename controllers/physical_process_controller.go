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

	physicalProcessDataHandlers = map[physicalProcessState]physicalProcessDataHandlerFunc{
		physicalProcessStateNamespace: handlePhysicalProcessNamespace,
		physicalProcessStateResolve:   handlePhysicalProcessResolve,
		physicalProcessStateLaunch:    handlePhysicalProcessLaunchState,
		physicalProcessStateRuntime:   handlePhysicalProcessRuntime,
		physicalProcessStateStop:      handlePhysicalProcessStopState,
		physicalProcessStateInvalid:   handlePhysicalProcessTerminal,
		0:                             handleUnknownPhysicalProcessState,
	}
)

type physicalProcessDataHandlerFunc = stateInitializerFunc[
	apiv2.PhysicalProcess, *apiv2.PhysicalProcess,
	PhysicalProcessReconciler, *PhysicalProcessReconciler,
	physicalProcessState,
	physicalProcessData, *physicalProcessData,
]

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
	reconciliationDelay := StandardDelay
	if physicalProcess.DeletionTimestamp != nil && !physicalProcess.DeletionTimestamp.IsZero() {
		change, reconciliationDelay = r.managePhysicalProcess(ctx, &physicalProcess, log)
	} else if change = ensureFinalizer(&physicalProcess, physicalProcessFinalizer, log); change == noChange {
		change, reconciliationDelay = r.managePhysicalProcess(ctx, &physicalProcess, log)
	}

	return r.SaveChangesWithDelay(
		ctx,
		&physicalProcess,
		patch,
		change,
		reconciliationDelay,
		nil,
		log,
	)
}

func (r *PhysicalProcessReconciler) managePhysicalProcess(
	ctx context.Context,
	physicalProcess *apiv2.PhysicalProcess,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	_, data := r.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
	if data == nil {
		data = &physicalProcessData{
			resourceUID: physicalProcess.UID,
			state:       physicalProcessStateNamespace,
			progress:    physicalResourceProgressNotReady,
		}
		initialStateKey := physicalProcessDataKey(physicalProcess)
		// Store() retains the supplied pointer, so keep an unaliased copy for this reconciliation.
		r.processData.Store(physicalProcess.NamespacedName(), initialStateKey, data.Clone())
	}

	handler := getStateInitializer(physicalProcessDataHandlers, data.state, log)
	change := handler(ctx, r, physicalProcess, data.state, data, log)

	_, currentData := r.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
	if currentData == nil {
		return change, StandardDelay
	}
	dataChange, delay, valid := currentData.applyTo(physicalProcess)
	change |= dataChange
	if !valid {
		log.Error(
			fmt.Errorf("invalid physical process state %v with progress %v", currentData.state, currentData.progress),
			"Physical process reached invalid reconciliation state",
		)
	}
	return change, delay
}

func handlePhysicalProcessNamespace(
	ctx context.Context,
	reconciler *PhysicalProcessReconciler,
	physicalProcess *apiv2.PhysicalProcess,
	_ physicalProcessState,
	data *physicalProcessData,
	log logr.Logger,
) objectChange {
	if physicalProcess.DeletionTimestamp != nil && !physicalProcess.DeletionTimestamp.IsZero() {
		change, _ := reconciler.handleDeletionRequest(physicalProcess, log)
		return change
	}
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, reconciler.Client, physicalProcess.Namespace)
	if !namespaceReady {
		data.state = physicalProcessStateNamespace
		data.failureMessage = namespaceReadinessMessage(physicalProcess.Namespace, namespaceReason)
		switch namespaceReason {
		case apiv2.PhysicalResourceReasonNamespaceNotFound:
			data.progress = physicalResourceProgressNotFound
		case apiv2.PhysicalResourceReasonNamespaceTerminating:
			data.progress = physicalResourceProgressTerminating
		case apiv2.PhysicalResourceReasonNamespaceNotActive:
			data.progress = physicalResourceProgressNotActive
		default:
			data.progress = physicalResourceProgressNotReady
		}
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get namespace", "Namespace", physicalProcess.Namespace)
			data.progress = physicalResourceProgressRetryPending
			data.failureMessage = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
		}
		_ = reconciler.processData.UpdateByNamespacedName(physicalProcess.NamespacedName(), data)
		return noChange
	}

	data.state = physicalProcessStateResolve
	data.progress = physicalResourceProgressInProgress
	data.failureMessage = ""
	if !reconciler.processData.UpdateByNamespacedName(physicalProcess.NamespacedName(), data) {
		return additionalReconciliationNeeded
	}
	return handlePhysicalProcessResolve(ctx, reconciler, physicalProcess, data.state, data, log)
}

func handlePhysicalProcessResolve(
	_ context.Context,
	reconciler *PhysicalProcessReconciler,
	physicalProcess *apiv2.PhysicalProcess,
	_ physicalProcessState,
	data *physicalProcessData,
	log logr.Logger,
) objectChange {
	if physicalProcess.DeletionTimestamp != nil && !physicalProcess.DeletionTimestamp.IsZero() {
		change, _ := reconciler.handleDeletionRequest(physicalProcess, log)
		return change
	}
	if data.progress == physicalResourceProgressFailed {
		return noChange
	}
	return reconciler.establishPhysicalProcessTracking(physicalProcess, data, log)
}

func handlePhysicalProcessLaunchState(
	ctx context.Context,
	reconciler *PhysicalProcessReconciler,
	physicalProcess *apiv2.PhysicalProcess,
	_ physicalProcessState,
	data *physicalProcessData,
	log logr.Logger,
) objectChange {
	if physicalProcess.DeletionTimestamp != nil && !physicalProcess.DeletionTimestamp.IsZero() {
		change, _ := reconciler.handleDeletionRequest(physicalProcess, log)
		return change
	}
	if data.progress == physicalResourceProgressInProgress {
		return noChange
	}
	if data.progress == physicalResourceProgressSkipped {
		return noChange
	}
	if data.progress != physicalResourceProgressRetryPending {
		return handleUnknownPhysicalProcessState(ctx, reconciler, physicalProcess, data.state, data, log)
	}
	stateKey, _ := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
	change, _ := reconciler.handlePhysicalProcessLaunchFailed(physicalProcess, stateKey, data, log)
	return change
}

func handlePhysicalProcessRuntime(
	ctx context.Context,
	reconciler *PhysicalProcessReconciler,
	physicalProcess *apiv2.PhysicalProcess,
	_ physicalProcessState,
	data *physicalProcessData,
	log logr.Logger,
) objectChange {
	if physicalProcess.DeletionTimestamp != nil && !physicalProcess.DeletionTimestamp.IsZero() {
		change, _ := reconciler.handleDeletionRequest(physicalProcess, log)
		return change
	}
	if data.progress == physicalResourceProgressExited ||
		data.progress == physicalResourceProgressMissing {
		return noChange
	}
	if data.progress == physicalResourceProgressRetryPending {
		if time.Now().Before(data.retryAfter) {
			return additionalReconciliationNeeded
		}
	}

	runningErr := reconciler.processExecutor.CheckProcessRunning(data.handle)
	if process.IsProcessGoneErr(runningErr) {
		data.state = physicalProcessStateRuntime
		data.progress = physicalResourceProgressMissing
		data.finishedAt = time.Now()
		_ = reconciler.processData.UpdateByNamespacedName(physicalProcess.NamespacedName(), data)
		return noChange
	}
	if runningErr != nil {
		log.Error(runningErr, "Failed to inspect runtime process", "PID", data.handle.Pid)
		data.state = physicalProcessStateRuntime
		data.progress = physicalResourceProgressRetryPending
		data.failureMessage = fmt.Sprintf("Failed to inspect runtime process: %v", runningErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		_ = reconciler.processData.UpdateByNamespacedName(physicalProcess.NamespacedName(), data)
		return noChange
	}

	if physicalProcess.Spec.Stop {
		stateKey, _ := reconciler.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
		change, _ := reconciler.schedulePhysicalProcessStop(physicalProcess, stateKey, data, log)
		return change
	}

	data.state = physicalProcessStateRuntime
	data.progress = physicalResourceProgressRunning
	data.failureMessage = ""
	_ = reconciler.processData.UpdateByNamespacedName(physicalProcess.NamespacedName(), data)
	return noChange
}

func handlePhysicalProcessStopState(
	ctx context.Context,
	reconciler *PhysicalProcessReconciler,
	physicalProcess *apiv2.PhysicalProcess,
	state physicalProcessState,
	data *physicalProcessData,
	log logr.Logger,
) objectChange {
	if physicalProcess.DeletionTimestamp != nil && !physicalProcess.DeletionTimestamp.IsZero() {
		change, _ := reconciler.handleDeletionRequest(physicalProcess, log)
		return change
	}
	if data.progress == physicalResourceProgressInProgress {
		return noChange
	}
	if data.progress != physicalResourceProgressRetryPending {
		return handleUnknownPhysicalProcessState(ctx, reconciler, physicalProcess, state, data, log)
	}
	return handlePhysicalProcessRuntime(ctx, reconciler, physicalProcess, state, data, log)
}

func handlePhysicalProcessTerminal(
	_ context.Context,
	reconciler *PhysicalProcessReconciler,
	physicalProcess *apiv2.PhysicalProcess,
	_ physicalProcessState,
	_ *physicalProcessData,
	log logr.Logger,
) objectChange {
	if physicalProcess.DeletionTimestamp == nil || physicalProcess.DeletionTimestamp.IsZero() {
		return noChange
	}
	change, _ := reconciler.handleDeletionRequest(physicalProcess, log)
	return change
}

func handleUnknownPhysicalProcessState(
	_ context.Context,
	reconciler *PhysicalProcessReconciler,
	physicalProcess *apiv2.PhysicalProcess,
	_ physicalProcessState,
	data *physicalProcessData,
	log logr.Logger,
) objectChange {
	if physicalProcess.DeletionTimestamp != nil && !physicalProcess.DeletionTimestamp.IsZero() {
		change, _ := reconciler.handleDeletionRequest(physicalProcess, log)
		return change
	}
	invalidState := data.state
	invalidProgress := data.progress
	data.state = physicalProcessStateInvalid
	data.progress = physicalResourceProgressFailed
	data.failureMessage = fmt.Sprintf("Physical process reached invalid reconciliation state %v with progress %v.", invalidState, invalidProgress)
	_ = reconciler.processData.UpdateByNamespacedName(physicalProcess.NamespacedName(), data)
	return additionalReconciliationNeeded
}

// establishPhysicalProcessTracking claims and initializes tracking state for a runtime process.
// A nil data result means that the returned change schedules work or applies the complete pending
// or terminal status for this reconciliation.
func (r *PhysicalProcessReconciler) establishPhysicalProcessTracking(
	physicalProcess *apiv2.PhysicalProcess,
	data *physicalProcessData,
	log logr.Logger,
) objectChange {
	if data.handle.Pid > 0 && !data.handle.IdentityTime.IsZero() {
		return r.claimPhysicalProcessTracking(physicalProcess, data, data.handle)
	}
	if physicalProcess.Spec.PID == nil {
		if physicalProcess.Spec.Stop {
			data.state = physicalProcessStateLaunch
			data.progress = physicalResourceProgressSkipped
			data.failureMessage = ""
			_ = r.processData.UpdateByNamespacedName(physicalProcess.NamespacedName(), data)
			return noChange
		}
		change, _ := r.schedulePhysicalProcessLaunch(physicalProcess, physicalProcessDataKey(physicalProcess), nil, log)
		return change
	}

	pid, pidErr := process.Int64_ToPidT(*physicalProcess.Spec.PID)
	if pidErr != nil {
		data.state = physicalProcessStateResolve
		data.progress = physicalResourceProgressFailed
		data.failureMessage = fmt.Sprintf("Invalid process ID: %v", pidErr)
		_ = r.processData.UpdateByNamespacedName(physicalProcess.NamespacedName(), data)
		return noChange
	}
	probedHandle, probeErr := r.processExecutor.FindProcessHandle(pid)
	if process.IsProcessGoneErr(probeErr) {
		*data = physicalProcessData{
			resourceUID: physicalProcess.UID,
			state:       physicalProcessStateRuntime,
			progress:    physicalResourceProgressMissing,
			handle:      process.NewHandle(pid, time.Time{}),
			finishedAt:  time.Now(),
		}
		// The runtime identity was never claimed, so the state stays keyed by the resource UID.
		r.processData.Store(physicalProcess.NamespacedName(), physicalProcessDataKey(physicalProcess), data.Clone())
		return noChange
	}
	if probeErr != nil {
		log.Error(probeErr, "Failed to inspect runtime process", "PID", pid)
		data.state = physicalProcessStateResolve
		data.progress = physicalResourceProgressRetryPending
		data.failureMessage = fmt.Sprintf("Failed to inspect runtime process: %v", probeErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		_ = r.processData.UpdateByNamespacedName(physicalProcess.NamespacedName(), data)
		return noChange
	}
	if probedHandle.IdentityTime.IsZero() {
		data.state = physicalProcessStateResolve
		data.progress = physicalResourceProgressRetryPending
		data.failureMessage = "Failed to determine the runtime process identity timestamp."
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		_ = r.processData.UpdateByNamespacedName(physicalProcess.NamespacedName(), data)
		return noChange
	}
	return r.claimPhysicalProcessTracking(physicalProcess, data, probedHandle)
}

// Claims the runtime process identity for this resource, recording a retry when another
// PhysicalProcess already owns it.
func (r *PhysicalProcessReconciler) claimPhysicalProcessTracking(
	physicalProcess *apiv2.PhysicalProcess,
	data *physicalProcessData,
	handle process.ProcessHandle,
) objectChange {
	claimedData := &physicalProcessData{
		resourceUID: physicalProcess.UID,
		state:       physicalProcessStateRuntime,
		progress:    physicalResourceProgressRunning,
		handle:      handle,
	}
	stateKey := physicalProcessHandleDataKey(handle)
	owner, stored := r.processData.StoreIfStateKeyUnclaimed(physicalProcess.NamespacedName(), stateKey, claimedData)
	if !stored {
		data.state = physicalProcessStateResolve
		data.progress = physicalResourceProgressRetryPending
		data.handle = handle
		data.failureMessage = fmt.Sprintf("Runtime process is already tracked by PhysicalProcess %q.", owner.String())
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		_ = r.processData.UpdateByNamespacedName(physicalProcess.NamespacedName(), data)
		return noChange
	}
	*data = *claimedData.Clone()
	return noChange
}

func (r *PhysicalProcessReconciler) handlePhysicalProcessLaunchFailed(
	physicalProcess *apiv2.PhysicalProcess,
	stateKey physicalProcessDataStateKey,
	data *physicalProcessData,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	if data.handle.Pid > 0 {
		if !physicalProcess.Spec.Stop && time.Now().Before(data.retryAfter) {
			return additionalReconciliationNeeded, LongDelay
		}
		return r.scheduleInvalidPhysicalProcessCleanup(physicalProcess, stateKey, data, log)
	}
	if physicalProcess.Spec.Stop {
		data.state = physicalProcessStateLaunch
		data.progress = physicalResourceProgressSkipped
		data.handle = process.ProcessHandle{}
		data.failureMessage = ""
		data.retryAfter = time.Time{}
		_ = r.processData.Update(physicalProcess.NamespacedName(), stateKey, data)
		change, delay, _ := data.applyTo(physicalProcess)
		return change, delay
	}
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded, LongDelay
	}
	return r.schedulePhysicalProcessLaunch(physicalProcess, stateKey, data, log)
}

func (r *PhysicalProcessReconciler) schedulePhysicalProcessLaunch(
	physicalProcess *apiv2.PhysicalProcess,
	stateKey physicalProcessDataStateKey,
	currentData *physicalProcessData,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	data := &physicalProcessData{
		resourceUID: physicalProcess.UID,
		state:       physicalProcessStateLaunch,
		progress:    physicalResourceProgressInProgress,
	}
	if currentData == nil {
		r.processData.Store(physicalProcess.NamespacedName(), stateKey, data)
	} else if !r.processData.Update(physicalProcess.NamespacedName(), stateKey, data) {
		return additionalReconciliationNeeded, StandardDelay
	}

	processSnapshot := physicalProcess.DeepCopy()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.launchPhysicalProcess(operationCtx, processSnapshot, stateKey, data.Clone(), log)
	})
	if enqueueErr != nil {
		failedData := data.Clone()
		failedData.progress = physicalResourceProgressRetryPending
		failedData.failureMessage = fmt.Sprintf("Failed to queue physical process launch: %v", enqueueErr)
		failedData.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		_ = r.processData.Update(physicalProcess.NamespacedName(), stateKey, failedData)
		change, delay, _ := failedData.applyTo(physicalProcess)
		return change, delay
	}
	change, delay, _ := data.applyTo(physicalProcess)
	return change, delay
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
		data.progress = physicalResourceProgressRetryPending
		data.failureMessage = fmt.Sprintf("Failed to launch physical process: %v", startErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		r.queuePhysicalProcessDataResult(physicalProcess, stateKey, data)
		return
	}
	if handle.Pid <= 0 || handle.IdentityTime.IsZero() {
		data.handle = handle
		data.progress = physicalResourceProgressRetryPending
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
	data.state = physicalProcessStateRuntime
	data.progress = physicalResourceProgressRunning
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
			result.state == physicalProcessStateRuntime &&
			result.progress == physicalResourceProgressExited &&
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
		currentData.state = physicalProcessStateRuntime
		currentData.progress = physicalResourceProgressExited
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
) (objectChange, AdditionalReconciliationDelay) {
	if data.state == physicalProcessStateStop &&
		data.progress == physicalResourceProgressRetryPending &&
		time.Now().Before(data.retryAfter) {
		change, delay, _ := data.applyTo(physicalProcess)
		return change, delay
	}

	stoppingData := data.Clone()
	stoppingData.state = physicalProcessStateStop
	stoppingData.progress = physicalResourceProgressInProgress
	stoppingData.failureMessage = ""
	if !r.processData.Update(physicalProcess.NamespacedName(), stateKey, stoppingData) {
		return additionalReconciliationNeeded, StandardDelay
	}

	processSnapshot := physicalProcess.DeepCopy()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.stopPhysicalProcess(operationCtx, processSnapshot, stateKey, stoppingData.Clone(), log)
	})
	if enqueueErr != nil {
		stoppingData.progress = physicalResourceProgressRetryPending
		stoppingData.failureMessage = fmt.Sprintf("Failed to queue physical process termination: %v", enqueueErr)
		stoppingData.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		_ = r.processData.Update(physicalProcess.NamespacedName(), stateKey, stoppingData)
		change, delay, _ := stoppingData.applyTo(physicalProcess)
		return change, delay
	}
	change, delay, _ := stoppingData.applyTo(physicalProcess)
	return change, delay
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
		data.state = physicalProcessStateStop
		data.progress = physicalResourceProgressRetryPending
		data.failureMessage = fmt.Sprintf("Failed to stop physical process: %v", stopErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		r.queuePhysicalProcessDataResult(physicalProcess, stateKey, data)
		return
	}

	data.state = physicalProcessStateRuntime
	data.progress = physicalResourceProgressExited
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
) (objectChange, AdditionalReconciliationDelay) {
	cleanupData := data.Clone()
	cleanupData.progress = physicalResourceProgressInProgress
	if !r.processData.Update(physicalProcess.NamespacedName(), stateKey, cleanupData) {
		return additionalReconciliationNeeded, StandardDelay
	}

	processSnapshot := physicalProcess.DeepCopy()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.cleanupInvalidPhysicalProcess(operationCtx, processSnapshot, stateKey, cleanupData.Clone(), log)
	})
	if enqueueErr != nil {
		cleanupData.progress = physicalResourceProgressRetryPending
		cleanupData.failureMessage = fmt.Sprintf("Failed to queue invalid physical process cleanup: %v", enqueueErr)
		cleanupData.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		_ = r.processData.Update(physicalProcess.NamespacedName(), stateKey, cleanupData)
		change, delay, _ := cleanupData.applyTo(physicalProcess)
		return change, delay
	}
	change, delay, _ := cleanupData.applyTo(physicalProcess)
	return change, delay
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
		data.progress = physicalResourceProgressRetryPending
		data.failureMessage = fmt.Sprintf("Failed to clean up process with an invalid identity: %v", cleanupErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		r.queuePhysicalProcessDataResult(physicalProcess, stateKey, data)
		return
	}

	log.V(1).Info("Cleaned up process with invalid identity", "PID", data.handle.Pid)
	data.handle = process.ProcessHandle{}
	data.progress = physicalResourceProgressRetryPending
	data.failureMessage = "Physical process launch returned an invalid process identity."
	data.retryAfter = time.Time{}
	r.queuePhysicalProcessDataResult(physicalProcess, stateKey, data)
}

func (r *PhysicalProcessReconciler) handleDeletionRequest(
	physicalProcess *apiv2.PhysicalProcess,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	stateKey, data := r.processData.BorrowByNamespacedName(physicalProcess.NamespacedName())
	if data != nil && data.operationInProgress() {
		return additionalReconciliationNeeded, StandardDelay
	}

	// A resource that never took ownership of a running process has nothing to stop, so deletion
	// only needs to drop the finalizer.
	retain := physicalProcess.Spec.Process == nil || physicalProcess.Spec.Process.RetainRuntimeProcess
	if data == nil || data.handle.Pid <= 0 || retain ||
		data.progress == physicalResourceProgressExited ||
		data.progress == physicalResourceProgressMissing {
		r.processData.DeleteByNamespacedName(physicalProcess.NamespacedName())
		return deleteFinalizer(physicalProcess, physicalProcessFinalizer, log), StandardDelay
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
