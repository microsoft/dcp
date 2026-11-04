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
	"slices"
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
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

var (
	logicalProcessFinalizer         = fmt.Sprintf("%s/logicalprocess-reconciler", apiv2.GroupVersion.Group)
	errLogicalProcessBackingChanged = errors.New("logical process backing changed")
)

const logicalProcessOwnerUIDField = ".metadata.logicalProcessOwnerUID"

type logicalProcessDataHandlerFunc = stateInitializerFunc[
	apiv2.LogicalProcess, *apiv2.LogicalProcess,
	LogicalProcessReconciler, *LogicalProcessReconciler,
	logicalProcessState, logicalProcessData, *logicalProcessData,
]

var logicalProcessDataHandlers = map[logicalProcessState]logicalProcessDataHandlerFunc{
	logicalProcessStatePrepare:        handleLogicalProcessPreparation,
	logicalProcessStatePrepareFailed:  handleLogicalProcessPreparation,
	logicalProcessStatePhysical:       handleLogicalProcessPhysical,
	logicalProcessStatePhysicalFailed: handleLogicalProcessPhysical,
	logicalProcessStateRuntime:        handleLogicalProcessRuntime,
	logicalProcessStateMissing:        handleLogicalProcessMissing,
	logicalProcessStateRunning:        handleLogicalProcessRuntime,
	logicalProcessStateExited:         handleLogicalProcessRuntime,
	logicalProcessStateRuntimeFailed:  handleLogicalProcessRuntime,
	logicalProcessStateUnknown:        handleLogicalProcessRuntime,
	logicalProcessStateInvalid:        handleUnknownLogicalProcessState,
	0:                                 handleUnknownLogicalProcessState,
}

type LogicalProcessReconcilerConfig struct {
	StateStore         *statestore.Store
	ResourceLeaseOwner process.ProcessHandle
	WorkloadID         commonapi.WorkloadID
}

type LogicalProcessReconciler struct {
	*ReconcilerBase[apiv2.LogicalProcess, *apiv2.LogicalProcess]
	processExecutor    process.Executor
	config             LogicalProcessReconcilerConfig
	processData        *ObjectStateMap[types.UID, logicalProcessData, *logicalProcessData, *apiv2.LogicalProcess]
	outputCleanupQueue *resiliency.WorkQueue
}

func NewLogicalProcessReconciler(
	lifetimeCtx context.Context, client ctrl_client.Client, noCacheClient ctrl_client.Reader,
	log logr.Logger, executor process.Executor, outputCleanupQueue *resiliency.WorkQueue, config LogicalProcessReconcilerConfig,
) *LogicalProcessReconciler {
	return &LogicalProcessReconciler{
		ReconcilerBase:  NewReconcilerBase[apiv2.LogicalProcess](client, noCacheClient, log, lifetimeCtx),
		processExecutor: executor, config: config,
		outputCleanupQueue: outputCleanupQueue,
		processData:        NewObjectStateMap[types.UID, logicalProcessData, *logicalProcessData, *apiv2.LogicalProcess](),
	}
}

func (r *LogicalProcessReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, &apiv2.PhysicalProcess{}, logicalProcessOwnerUIDField,
		func(obj ctrl_client.Object) []string {
			owner := metav1.GetControllerOf(obj)
			if owner == nil {
				return nil
			}
			return []string{string(owner.UID)}
		})
	if indexErr != nil {
		return fmt.Errorf("index logical process owner: %w", indexErr)
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.LogicalProcess{}).
		Owns(&apiv2.PhysicalProcess{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Owns(&apiv2.ServiceEndpoint{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.LogicalProcessList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Named(name).Complete(r)
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
			discardErr := r.discardLogicalProcessData(ctx, req.NamespacedName)
			if discardErr != nil {
				log.Error(discardErr, "Failed to discard logical process state")
				return ctrl.Result{}, discardErr
			}
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
		discardErr := r.discardLogicalProcessData(ctx, req.NamespacedName)
		if discardErr != nil {
			log.Error(discardErr, "Failed to discard previous logical process state")
			return ctrl.Result{}, discardErr
		}
	}
	_, data := r.processData.BorrowByNamespacedName(req.NamespacedName)
	patch := ctrl_client.MergeFromWithOptions(resource.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	delay := StandardDelay
	if !resource.DeletionTimestamp.IsZero() {
		change, delay = r.handleLogicalProcessDeletion(ctx, &resource, data, log)
	} else {
		change = ensureFinalizer(&resource, logicalProcessFinalizer, log)
		if change == noChange {
			change, delay = r.manageLogicalProcess(ctx, &resource, data, log)
		}
	}
	if !hasFinalizer(&resource, logicalProcessFinalizer) {
		defer logger.ReleaseResourceLog(resource.GetResourceId())
	}
	return r.SaveChangesWithDelay(ctx, &resource, patch, change, delay, nil, log)
}

func (r *LogicalProcessReconciler) manageLogicalProcess(
	ctx context.Context, resource *apiv2.LogicalProcess, data *logicalProcessData, log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	if data == nil {
		ready, reason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, resource.Namespace)
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
			return change, StandardDelay
		}
		data = &logicalProcessData{state: logicalProcessStatePrepare}
		r.processData.Store(resource.NamespacedName(), resource.UID, data.Clone())
	}
	stateHandler := getStateInitializer(logicalProcessDataHandlers, data.state, log)
	change := stateHandler(ctx, r, resource, data.state, data, log)
	var endpointCleanupErr error
	if data.state == logicalProcessStateMissing || data.state == logicalProcessStateInvalid {
		endpointCleanupErr = syncLogicalEndpoints(ctx, r.Client, r.NoCacheClient, resource, data.ports, nil)
		if endpointCleanupErr != nil {
			log.Error(endpointCleanupErr, "Failed to withdraw terminal logical process endpoints")
			change |= additionalReconciliationNeeded
		}
	}
	_ = r.processData.Update(resource.NamespacedName(), resource.UID, data)
	projectionChange, delay, valid := data.applyTo(resource)
	if endpointCleanupErr != nil {
		delay = LongDelay
	}
	if !valid {
		log.Error(fmt.Errorf("invalid logical process state %v", data.state), "LogicalProcess reached invalid reconciliation state")
	}
	return change | projectionChange, delay
}

func handleLogicalProcessPreparation(
	ctx context.Context, reconciler *LogicalProcessReconciler, resource *apiv2.LogicalProcess,
	_ logicalProcessState, data *logicalProcessData, log logr.Logger,
) objectChange {
	if data.state == logicalProcessStatePrepareFailed {
		return noChange
	}
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	data.reconcileErr, data.reason, data.message, data.retryAfter = nil, "", "", time.Time{}
	if !reconciler.prepareLogicalProcessValues(ctx, resource, data, log) {
		if data.state == logicalProcessStatePrepareFailed {
			return noChange
		}
		return additionalReconciliationNeeded
	}
	reconciler.prepareLogicalProcess(ctx, resource, data, log)
	if data.state != logicalProcessStatePhysical {
		return noChange
	}
	return handleLogicalProcessPhysical(ctx, reconciler, resource, data.state, data, log)
}

func handleLogicalProcessPhysical(
	ctx context.Context, reconciler *LogicalProcessReconciler, resource *apiv2.LogicalProcess,
	_ logicalProcessState, data *logicalProcessData, log logr.Logger,
) objectChange {
	if data.state == logicalProcessStatePhysicalFailed {
		if data.lease != nil {
			return reconciler.persistLogicalProcess(ctx, resource, data, nil, log)
		}
		return noChange
	}
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	data.reconcileErr, data.reason, data.message, data.retryAfter = nil, "", "", time.Time{}
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
			data.reconcileErr, data.message = previousErr, previousErr.Error()
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
		enqueueProcessOutputCleanup(reconciler.outputCleanupQueue,
			reconciler.Log.WithValues("LogicalProcess", resource.NamespacedName(), "ResourceUID", resource.UID),
			record.StdOutFile, record.StdErrFile)
		retireErr := reconciler.config.StateStore.DeletePersistentProcess(ctx, resource.GetLeaseKey())
		if retireErr != nil {
			log.Error(retireErr, "Failed to retire replaced process record")
			data.reconcileErr, data.message = retireErr, retireErr.Error()
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			return noChange
		}
		data.pendingRecord, data.replaceExisting = nil, false
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
		desired := data.runConfig
		if desired == nil && resource.Spec.Stop {
			desired = resource.Spec.Process
		}
		physicalSpec.Process = &apiv2.PhysicalProcessConfig{
			ExecutablePath: desired.ExecutablePath, Args: desired.Args, WorkingDirectory: desired.WorkingDirectory,
			AmbientEnvironment: desired.AmbientEnvironment, Env: desired.Env,
			RetainRuntimeProcess: resource.Spec.EffectiveMode() == apiv2.LogicalProcessModePersistent,
			MonitorPID:           resource.Spec.MonitorPID, MonitorTimestamp: resource.Spec.MonitorTimestamp,
		}
	}
	physical, physicalErr := reconciler.ensureLogicalPhysicalProcess(ctx, resource, data.physicalUID, "", physicalSpec)
	if physicalErr != nil {
		log.Error(physicalErr, "Failed to prepare backing physical process")
		data.reconcileErr, data.message = physicalErr, physicalErr.Error()
		return noChange
	}
	data.physicalUID, data.pendingRecord = physical.UID, nil
	data.state = logicalProcessStateRuntime
	return noChange
}

func (r *LogicalProcessReconciler) ensureLogicalPhysicalProcess(
	ctx context.Context, resource *apiv2.LogicalProcess, expectedUID types.UID, suffix string, spec apiv2.PhysicalProcessSpec,
) (*apiv2.PhysicalProcess, error) {
	physical := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(resource.UID) + suffix, Namespace: resource.Namespace},
		Spec:       spec,
	}
	observed, getErr := r.getLogicalPhysicalProcess(ctx, resource, expectedUID, suffix)
	if apierrors.IsNotFound(getErr) {
		if expectedUID != "" {
			return nil, fmt.Errorf("%w: physical process %q no longer exists", errLogicalProcessBackingChanged, physical.NamespacedName())
		}
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
		InitializeLogicalBackingFinalizers(physical, physicalProcessFinalizer)
		createErr := r.Create(ctx, physical)
		if createErr != nil {
			getErr = r.NoCacheClient.Get(ctx, physical.NamespacedName(), physical)
			if getErr != nil {
				return nil, fmt.Errorf("create physical process and verify backing: %w", errors.Join(createErr, getErr))
			}
		}
	} else if getErr != nil {
		return nil, getErr
	} else {
		physical = observed
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
	if !hasFinalizer(physical, LogicalBackingFinalizer) {
		return nil, fmt.Errorf("physical process %q no longer has its logical hold", physical.NamespacedName())
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
	if !physical.DeletionTimestamp.IsZero() {
		return !hasFinalizer(physical, physicalProcessFinalizer)
	}
	if !logicalPhysicalProcessObserved(physical) {
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
	data.reconcileErr = nil
	physical, getErr := reconciler.getLogicalPhysicalProcess(ctx, resource, data.physicalUID, "")
	if getErr != nil {
		log.Error(getErr, "Failed to get backing physical process")
		data.reconcileErr, data.reason, data.message = getErr, "", getErr.Error()
		if errors.Is(getErr, errLogicalProcessBackingChanged) {
			data.state, data.reason, data.reconcileErr = logicalProcessStateInvalid, "PhysicalProcessChanged", nil
			return handleUnknownLogicalProcessState(ctx, reconciler, resource, data.state, data, log)
		}
		if apierrors.IsNotFound(getErr) {
			data.reconcileErr = nil
			data.state = logicalProcessStateMissing
			data.reason, data.message = "PhysicalProcessMissing", "The backing PhysicalProcess no longer exists; the logical process cannot recover."
			return handleLogicalProcessMissing(ctx, reconciler, resource, data.state, data, log)
		}
		return noChange
	}
	if !hasFinalizer(physical, LogicalBackingFinalizer) {
		data.state, data.reason, data.message = logicalProcessStateInvalid, "PhysicalProcessChanged",
			"The backing PhysicalProcess logical hold changed."
		log.Error(errors.New(data.message), "Logical process backing changed")
		return handleUnknownLogicalProcessState(ctx, reconciler, resource, data.state, data, log)
	}
	if resource.Spec.Stop && !physical.Spec.Stop {
		patch := ctrl_client.MergeFromWithOptions(physical.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
		physical.Spec.Stop = resource.Spec.Stop
		stopErr := reconciler.Patch(ctx, physical, patch)
		if stopErr != nil {
			log.Error(stopErr, "Failed to request physical process stop")
			data.reconcileErr, data.message = stopErr, stopErr.Error()
			return noChange
		}
	}
	change := applyLogicalProcessPhysicalDetails(resource, physical)
	data.state, data.reason, data.message = logicalProcessStateRuntime, "", ""
	if !physical.DeletionTimestamp.IsZero() && hasFinalizer(physical, physicalProcessFinalizer) {
		data.state, data.message = logicalProcessStateUnknown, "The backing PhysicalProcess is being deleted."
		return change | additionalReconciliationNeeded
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
	var addresses [][]apiv2.ServiceAddress
	if data.state == logicalProcessStateRunning && !resource.Spec.Stop {
		addresses = processEndpointAddresses(data.ports)
	}
	if endpointErr := syncLogicalEndpoints(ctx, reconciler.Client, reconciler.NoCacheClient, resource, data.ports, addresses); endpointErr != nil {
		log.Error(endpointErr, "Failed to synchronize logical process endpoints")
		data.reconcileErr, data.message = endpointErr, endpointErr.Error()
		change |= additionalReconciliationNeeded
	}
	if data.lease != nil && logicalPhysicalProcessStartupSettled(physical) {
		return change | reconciler.persistLogicalProcess(ctx, resource, data, physical, log)
	}
	return change
}

func handleLogicalProcessMissing(
	ctx context.Context, reconciler *LogicalProcessReconciler, resource *apiv2.LogicalProcess,
	_ logicalProcessState, data *logicalProcessData, log logr.Logger,
) objectChange {
	change := data.applyMissingLogSource(resource)
	if data.lease != nil {
		return change | reconciler.persistLogicalProcess(ctx, resource, data, nil, log)
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
	var children apiv2.PhysicalProcessList
	listErr := r.List(ctx, &children, ctrl_client.InNamespace(resource.Namespace),
		ctrl_client.MatchingFields{logicalProcessOwnerUIDField: string(resource.UID)})
	if listErr != nil {
		return nil, listErr
	}
	found := false
	for i := range children.Items {
		child := &children.Items[i]
		if child.Name == name.Name && metav1.IsControlledBy(child, resource) {
			physical, found = child, true
			break
		}
	}
	if !found {
		getErr := r.NoCacheClient.Get(ctx, name, physical)
		if getErr != nil {
			return nil, getErr
		}
	}
	if !metav1.IsControlledBy(physical, resource) || (expectedUID != "" && physical.UID != expectedUID) {
		return nil, fmt.Errorf("%w: physical process %q has a different identity or owner", errLogicalProcessBackingChanged, name)
	}
	return physical, nil
}

func (r *LogicalProcessReconciler) handleLogicalProcessDeletion(
	ctx context.Context, resource *apiv2.LogicalProcess, data *logicalProcessData, log logr.Logger,
) (change objectChange, delay AdditionalReconciliationDelay) {
	delay = StandardDelay
	if data == nil {
		data = &logicalProcessData{state: logicalProcessStateDelete}
		r.processData.Store(resource.NamespacedName(), resource.UID, data.Clone())
	}
	defer func() {
		if data != nil {
			_ = r.processData.Update(resource.NamespacedName(), resource.UID, data)
			projectionChange, projectionDelay, valid := data.applyTo(resource)
			change |= projectionChange
			delay = projectionDelay
			if !valid {
				log.Error(fmt.Errorf("invalid logical process state %v", data.state), "LogicalProcess reached invalid reconciliation state")
			}
		}
	}()
	data.state, data.reason, data.message, data.reconcileErr = logicalProcessStateDelete, "", "", nil
	bindings := make([]apiv2.LogicalServiceBinding, len(resource.Spec.Ports))
	for portIndex, port := range resource.Spec.Ports {
		bindings[portIndex] = port.LogicalServiceBinding
	}
	// Endpoint failures must not prevent requesting backing deletion.
	portsDeleted, portDeleteErr := cleanupLogicalEndpoints(ctx, r.Client, r.NoCacheClient, resource, bindings)
	if portDeleteErr != nil {
		log.Error(portDeleteErr, "Failed to delete logical process endpoints")
		data.reconcileErr, data.message = portDeleteErr, portDeleteErr.Error()
	}
	var children apiv2.PhysicalProcessList
	listErr := r.List(ctx, &children, ctrl_client.InNamespace(resource.Namespace),
		ctrl_client.MatchingFields{logicalProcessOwnerUIDField: string(resource.UID)})
	if listErr != nil {
		log.Error(listErr, "Failed to list physical processes before deletion")
		data.reconcileErr, data.message = listErr, listErr.Error()
		return additionalReconciliationNeeded, delay
	}
	currentName := "logical-" + string(resource.UID)
	hasCurrent := slices.ContainsFunc(children.Items, func(child apiv2.PhysicalProcess) bool {
		return child.Name == currentName
	})
	hasPrevious := slices.ContainsFunc(children.Items, func(child apiv2.PhysicalProcess) bool {
		return child.Name == currentName+"-previous"
	})
	if !hasCurrent || ((data.replaceExisting || data.replacementUID != "") && !hasPrevious) {
		verifyErr := r.NoCacheClient.List(ctx, &children, ctrl_client.InNamespace(resource.Namespace))
		if verifyErr != nil {
			log.Error(verifyErr, "Failed to verify physical process absence before deletion")
			data.reconcileErr, data.message = verifyErr, verifyErr.Error()
			return additionalReconciliationNeeded, delay
		}
		children.Items = slices.DeleteFunc(children.Items, func(child apiv2.PhysicalProcess) bool {
			return !metav1.IsControlledBy(&child, resource)
		})
	}
	if len(children.Items) != 0 {
		waiting := false
		var current *apiv2.PhysicalProcess
		for i := range children.Items {
			physical := &children.Items[i]
			if physical.Name == currentName {
				if data.physicalUID == "" {
					data.physicalUID = physical.UID
				}
				if physical.UID == data.physicalUID {
					current = physical
					change |= applyLogicalProcessPhysicalDetails(resource, physical)
				}
			}
			if physical.DeletionTimestamp.IsZero() {
				uid := physical.UID
				deleteErr := r.Delete(ctx, physical, ctrl_client.Preconditions{UID: &uid})
				if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
					log.Error(deleteErr, "Failed to delete backing physical process")
					data.reconcileErr, data.message = deleteErr, deleteErr.Error()
					waiting = true
				}
			}
			if physical.DeletionTimestamp.IsZero() || hasFinalizer(physical, physicalProcessFinalizer) {
				waiting = true
			}
		}
		if waiting {
			return change | additionalReconciliationNeeded, delay
		}
		if data.lease != nil {
			return change | r.persistLogicalProcess(ctx, resource, data, current, log), delay
		}
		for i := range children.Items {
			physical := &children.Items[i]
			releaseErr := ReleaseLogicalBackingFinalizer(ctx, r.Client, physical)
			if releaseErr != nil && !apierrors.IsNotFound(releaseErr) {
				log.Error(releaseErr, "Failed to release logical process backing")
				data.reconcileErr, data.message = releaseErr, releaseErr.Error()
			}
		}
		return change | additionalReconciliationNeeded, delay
	}
	if data.lease != nil {
		return r.persistLogicalProcess(ctx, resource, data, nil, log), delay
	}
	if portDeleteErr != nil || !portsDeleted {
		return additionalReconciliationNeeded, delay
	}
	discardErr := r.discardLogicalProcessData(ctx, resource.NamespacedName())
	if discardErr != nil {
		log.Error(discardErr, "Failed to discard logical process state")
		data.reconcileErr, data.message = discardErr, discardErr.Error()
		return additionalReconciliationNeeded, delay
	}
	data = nil
	return deleteFinalizer(resource, logicalProcessFinalizer, log), delay
}

func handleUnknownLogicalProcessState(
	ctx context.Context, reconciler *LogicalProcessReconciler, resource *apiv2.LogicalProcess,
	state logicalProcessState, data *logicalProcessData, log logr.Logger,
) objectChange {
	if state == logicalProcessStateInvalid {
		change := data.applyMissingLogSource(resource)
		if data.lease != nil {
			change |= reconciler.persistLogicalProcess(ctx, resource, data, nil, log)
		}
		return change
	}
	log.Error(fmt.Errorf("unknown logical process state %v", state), "LogicalProcess has an invalid reconciliation state")
	data.state, data.reason, data.message = logicalProcessStateInvalid, apiv2.PhysicalResourceReasonOperationStateInvalid,
		"Logical process reached an invalid reconciliation state."
	data.reconcileErr = nil
	return noChange
}

func (r *LogicalProcessReconciler) prepareLogicalProcess(
	ctx context.Context, resource *apiv2.LogicalProcess, data *logicalProcessData, log logr.Logger,
) {
	if resource.Spec.EffectiveMode() == apiv2.LogicalProcessModeExisting {
		data.lifecycleKey, data.state = resource.Spec.LifecycleKey, logicalProcessStatePhysical
		return
	}
	lifecycleSpec := resource.Spec.DeepCopy()
	if !resource.Spec.Stop {
		lifecycleSpec.Process = data.runConfig.DeepCopy()
	}
	key, _, keyErr := lifecycleSpec.GetLifecycleKey()
	if keyErr != nil {
		log.Error(keyErr, "Failed to calculate logical process lifecycle key")
		data.state, data.message = logicalProcessStatePrepareFailed, keyErr.Error()
		return
	}
	data.lifecycleKey = key
	if resource.Spec.EffectiveMode() != apiv2.LogicalProcessModePersistent {
		data.state = logicalProcessStatePhysical
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
			enqueueProcessOutputCleanup(r.outputCleanupQueue,
				r.Log.WithValues("LogicalProcess", resource.NamespacedName(), "ResourceUID", resource.UID),
				record.StdOutFile, record.StdErrFile)
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
		data.reconcileErr, data.message = preparationErr, preparationErr.Error()
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return
	}
	data.state = logicalProcessStatePhysical
}

func (r *LogicalProcessReconciler) persistLogicalProcess(
	ctx context.Context, resource *apiv2.LogicalProcess, data *logicalProcessData,
	physical *apiv2.PhysicalProcess, log logr.Logger,
) objectChange {
	if data.leaseFailureMessage != "" && time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	data.leaseFailureMessage = ""
	if physical != nil {
		ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
		if physical.Status.Phase == apiv2.PhysicalProcessPhaseFailed ||
			(ready != nil && (ready.Reason == string(apiv2.PhysicalProcessReasonRuntimeProcessMissing) ||
				ready.Reason == string(apiv2.PhysicalProcessReasonRuntimeProcessAlreadyTracked))) {
			physical = nil
		}
	}
	persistErr := func() error {
		if physical != nil {
			if !logicalPhysicalProcessStartupSettled(physical) {
				return errors.New("waiting for retained physical process startup to settle")
			}
			verifyErr := r.config.StateStore.VerifyResourceLeaseHeld(ctx, resource, r.config.ResourceLeaseOwner)
			if verifyErr != nil {
				return verifyErr
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
						enqueueProcessOutputCleanup(r.outputCleanupQueue,
							r.Log.WithValues("LogicalProcess", resource.NamespacedName(), "ResourceUID", resource.UID),
							record.StdOutFile, record.StdErrFile)
					}
					record = &statestore.PersistentProcessRecord{RunID: string(physical.UID)}
				}
				record.ResourceKey, record.LifecycleKey = resource.GetLeaseKey(), data.lifecycleKey
				record.PID, record.IdentityTime = pid, physical.Status.IdentityTimestamp.Time
				if workloadID != "" {
					record.WorkloadID = workloadID
				}
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
		}
		releaseErr := data.lease.Release(ctx)
		if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
			return releaseErr
		}
		data.lease = nil
		return nil
	}()
	if persistErr != nil {
		log.Error(persistErr, "Failed to settle persistent logical process ownership")
		data.leaseFailureMessage = persistErr.Error()
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return additionalReconciliationNeeded
	}
	data.retryAfter = time.Time{}
	return additionalReconciliationNeeded
}

func (r *LogicalProcessReconciler) discardLogicalProcessData(ctx context.Context, name types.NamespacedName) error {
	uid, data := r.processData.BorrowByNamespacedName(name)
	if data == nil {
		return nil
	}
	if data.lease != nil {
		releaseErr := data.lease.Release(ctx)
		if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
			return fmt.Errorf("release discarded logical process lease: %w", releaseErr)
		}
	}
	missing := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{UID: uid}}
	defer logger.ReleaseResourceLog(missing.GetResourceId())
	r.processData.DeleteByNamespacedName(name)
	return nil
}
