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
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

const projectProcessOwnerUIDField = ".metadata.projectProcessOwnerUID"

var projectProcessFinalizer = apiv2.GroupName + "/projectprocess-reconciler"

type projectProcessDataHandlerFunc = stateInitializerFunc[
	apiv2.ProjectProcess, *apiv2.ProjectProcess, ProjectProcessReconciler, *ProjectProcessReconciler,
	projectState, projectProcessData, *projectProcessData,
]

var projectProcessDataHandlers = map[projectState]projectProcessDataHandlerFunc{
	projectStatePending:       handleProjectProcessPending,
	projectStateAwaitingStart: handleProjectProcessAwaitingStart,
	projectStateCreating:      handleProjectProcessPhysical,
	projectStateReady:         handleProjectProcessReady,
	projectStateActive:        handleProjectProcessReady,
	projectStateRunning:       handleProjectProcessPhysical,
	projectStateExited:        handleProjectProcessExited,
	projectStateUnknown:       handleProjectProcessPhysical,
	projectStateStopping:      handleProjectProcessStopping,
	projectStateStopped:       handleProjectProcessTerminal,
	projectStateFailed:        handleProjectProcessTerminal,
	0:                         handleInvalidProjectProcessState,
}

// ProjectProcessReconciler manages direct or adapter-controlled execution of user code.
type ProjectProcessReconciler struct {
	*ReconcilerBase[apiv2.ProjectProcess, *apiv2.ProjectProcess]
	projects        *ObjectStateMap[types.UID, projectProcessData, *projectProcessData, *apiv2.ProjectProcess]
	processExecutor process.Executor
}

func NewProjectProcessReconciler(
	ctx context.Context, client ctrl_client.Client, reader ctrl_client.Reader, log logr.Logger, executor process.Executor,
) *ProjectProcessReconciler {
	return &ProjectProcessReconciler{
		ReconcilerBase:  NewReconcilerBase[apiv2.ProjectProcess](client, reader, log, ctx),
		projects:        NewObjectStateMap[types.UID, projectProcessData, *projectProcessData, *apiv2.ProjectProcess](),
		processExecutor: executor,
	}
}

func (r *ProjectProcessReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	for _, backing := range []ctrl_client.Object{&apiv2.PhysicalProcess{}, &apiv2.ServiceEndpoint{}} {
		indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, backing, projectProcessOwnerUIDField, func(obj ctrl_client.Object) []string {
			owner := metav1.GetControllerOf(obj)
			if owner == nil || owner.Kind != "ProjectProcess" {
				return nil
			}
			return []string{string(owner.UID)}
		})
		if indexErr != nil {
			return fmt.Errorf("index process project backing owner: %w", indexErr)
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.ProjectProcess{}).
		Owns(&apiv2.PhysicalProcess{}).
		Owns(&apiv2.ServiceEndpoint{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.ProjectProcessList{}))).
		WatchesRawSource(r.GetReconciliationEventSource()).Named(name).Complete(r)
}

func (r *ProjectProcessReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)
	if ctx.Err() != nil {
		return ctrl.Result{}, nil
	}

	var resource apiv2.ProjectProcess
	getErr := reader.Get(ctx, req.NamespacedName, &resource)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			uid, _ := r.projects.BorrowByNamespacedName(req.NamespacedName)
			r.projects.DeleteByNamespacedName(req.NamespacedName)
			logger.ReleaseResourceLog((&apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{UID: uid}}).GetResourceId())
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to get ProjectProcess")
		return ctrl.Result{}, getErr
	}

	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, resource.GetResourceId())
	if !resource.DeletionTimestamp.IsZero() && !hasFinalizer(&resource, projectProcessFinalizer) {
		r.projects.DeleteByNamespacedName(req.NamespacedName)
		logger.ReleaseResourceLog(resource.GetResourceId())
		return ctrl.Result{}, nil
	}
	stateKey, data := r.projects.BorrowByNamespacedName(req.NamespacedName)
	if data != nil && stateKey != resource.UID {
		logger.ReleaseResourceLog((&apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{UID: stateKey}}).GetResourceId())
		r.projects.DeleteByNamespacedName(req.NamespacedName)
		data = nil
	}

	patch := ctrl_client.MergeFromWithOptions(resource.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	delay := StandardDelay

	if !resource.DeletionTimestamp.IsZero() {
		change, delay = r.handleDeletionRequest(ctx, &resource, data, log)
	} else {
		change = ensureFinalizer(&resource, projectProcessFinalizer, log)
		if change == noChange {
			change, delay = r.manageProject(ctx, &resource, data, log)
		}
	}

	return r.SaveChangesWithDelay(ctx, &resource, patch, change, delay, nil, log)
}

func (r *ProjectProcessReconciler) manageProject(
	ctx context.Context, resource *apiv2.ProjectProcess, data *projectProcessData, log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	if data == nil {
		if change, ready := checkProjectNamespace(ctx, r.Client, resource, log); !ready {
			return change, StandardDelay
		}
		data = &projectProcessData{projectData: projectData{state: projectStatePending}}
		r.projects.Store(resource.NamespacedName(), resource.UID, data.Clone())
	}

	stateHandler := getStateInitializer(projectProcessDataHandlers, data.state, log)
	change := stateHandler(ctx, r, resource, data.state, data, log)
	_ = r.projects.Update(resource.NamespacedName(), resource.UID, data)
	projectionChange, delay, valid := data.applyTo(resource)
	if !valid {
		log.Error(fmt.Errorf("invalid ProjectProcess state %v", data.state), "ProjectProcess reached invalid reconciliation state")
	}
	return change | projectionChange, delay
}

func projectProcessPortRequests(resource *apiv2.ProjectProcess) []logicalPortRequest {
	var requests []logicalPortRequest
	for _, port := range resource.Spec.Process.Ports {
		requests = append(requests, logicalPortRequest{binding: port.LogicalServiceBinding, address: port.Address, port: port.Port})
	}
	return requests
}

func (r *ProjectProcessReconciler) prepareProjectValues(
	ctx context.Context, resource *apiv2.ProjectProcess, data *projectProcessData, log logr.Logger,
) error {
	if data.inputsPrepared || resource.Spec.Stop {
		return nil
	}
	requests := projectProcessPortRequests(resource)
	ports, resolvedArgs, resolvedEnv, prepareErr := prepareProjectValues(ctx, r.Client, r.NoCacheClient, resource, &data.projectData,
		requests, resource.Spec.Process.Args, resource.Spec.Process.Env, true, nil, log)
	if prepareErr != nil {
		return prepareErr
	}
	if resource.Spec.Process.AmbientEnvironment.EffectiveBehavior() == apiv2.EnvironmentBehaviorInherit {
		resolvedEnv = mergeProjectAmbientEnvironment(resolvedEnv)
	}
	resolvedEnv = slices.DeleteFunc(resolvedEnv, func(variable commonapi.EnvVar) bool {
		name := variable.Name
		if osutil.IsWindows() {
			name = strings.ToUpper(name)
		}
		for _, prefix := range suppressVarPrefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	})
	data.effectiveArgs, data.effectiveEnv = resolvedArgs, resolvedEnv
	data.ports, data.inputsPrepared = logicalPortSelections(ports, requests), true
	return nil
}

func (r *ProjectProcessReconciler) syncProjectEndpoints(
	ctx context.Context, resource *apiv2.ProjectProcess, data *projectProcessData, log logr.Logger,
) objectChange {
	return syncProjectEndpoints(ctx, r.Client, r.NoCacheClient, resource, &data.projectData, data, projectProcessPortRequests(resource),
		projectProcessOwnerUIDField, nil, log)
}

func projectProcessPhysicalName(resource *apiv2.ProjectProcess) string {
	return "project-" + string(resource.UID)
}

func projectProcessPhysicalSpec(resource *apiv2.ProjectProcess, data *projectProcessData) apiv2.PhysicalProcessSpec {
	config := resource.Spec.Process.LogicalProcessConfig
	return apiv2.PhysicalProcessSpec{Process: &apiv2.PhysicalProcessConfig{
		ExecutablePath: config.ExecutablePath, WorkingDirectory: config.WorkingDirectory,
		Args: append([]string(nil), data.effectiveArgs...), Env: append([]commonapi.EnvVar(nil), data.effectiveEnv...),
		AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
	}}
}

func (r *ProjectProcessReconciler) findProjectProcess(
	ctx context.Context, resource *apiv2.ProjectProcess, data *projectProcessData,
) (*apiv2.PhysicalProcess, error) {
	name := types.NamespacedName{Namespace: resource.Namespace, Name: projectProcessPhysicalName(resource)}
	var children apiv2.PhysicalProcessList
	listErr := r.List(ctx, &children, ctrl_client.InNamespace(resource.Namespace),
		ctrl_client.MatchingFields{projectProcessOwnerUIDField: string(resource.UID)})
	if listErr != nil {
		return nil, fmt.Errorf("list Project process backings: %w", listErr)
	}
	var physical *apiv2.PhysicalProcess
	for index := range children.Items {
		child := &children.Items[index]
		if !metav1.IsControlledBy(child, resource) {
			continue
		}
		if physical != nil || child.Name != name.Name {
			identityErr := fmt.Errorf("ProjectProcess has unexpected or multiple PhysicalProcess backings")
			data.failMissing(identityErr.Error())
			return nil, identityErr
		}
		physical = child
		data.unobservedChildren.observeCached(child)
	}
	if physical == nil {
		physical = &apiv2.PhysicalProcess{}
		getErr := r.NoCacheClient.Get(ctx, name, physical)
		if apierrors.IsNotFound(getErr) {
			if data.physicalUID != "" {
				data.failMissing("The owned PhysicalProcess no longer exists.")
				data.runtimeLogs = &apiv2.LogSource{Name: data.physicalName, UID: data.physicalUID, Complete: true}
				return nil, getErr
			}
			return nil, nil
		}
		if getErr != nil {
			return nil, fmt.Errorf("get Project process backing: %w", getErr)
		}
	}
	// A held backing that is being deleted is still ours; its controller reports the teardown through its lifecycle.
	if !metav1.IsControlledBy(physical, resource) || (data.physicalUID != "" && physical.UID != data.physicalUID) ||
		!hasFinalizer(physical, LogicalBackingFinalizer) {
		identityErr := fmt.Errorf("physical process %q has a different owner, UID, or logical hold", physical.NamespacedName())
		data.failMissing(identityErr.Error())
		return nil, identityErr
	}
	actual, expected := physical.Spec.DeepCopy(), projectProcessPhysicalSpec(resource, data)
	actual.Stop, expected.Stop = false, false
	if !data.inputsPrepared && actual.Process != nil {
		expected.Process.Args = slices.Clone(actual.Process.Args)
		expected.Process.Env = slices.Clone(actual.Process.Env)
	}
	if !reflect.DeepEqual(*actual, expected) {
		specErr := fmt.Errorf("physical process %q has a different spec", physical.NamespacedName())
		data.failMissing(specErr.Error())
		return nil, specErr
	}
	data.physicalUID, data.physicalName = physical.UID, physical.Name
	return physical, nil
}

func (r *ProjectProcessReconciler) createProjectProcess(
	ctx context.Context, resource *apiv2.ProjectProcess, data *projectProcessData,
) (*apiv2.PhysicalProcess, error) {
	physical := &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: projectProcessPhysicalName(resource), Namespace: resource.Namespace},
		Spec:       projectProcessPhysicalSpec(resource, data),
	}
	return createLogicalBacking(ctx, r.Client, r.NoCacheClient, resource, physical, physicalProcessFinalizer,
		&data.physicalUID, &data.unobservedChildren, func(existing *apiv2.PhysicalProcess) error {
			actual, expected := existing.Spec.DeepCopy(), physical.Spec.DeepCopy()
			actual.Stop, expected.Stop = false, false
			if !reflect.DeepEqual(actual, expected) {
				return fmt.Errorf("physical process %q has a different spec", existing.NamespacedName())
			}
			return nil
		})
}

func (data *projectProcessData) observePhysical(physical *apiv2.PhysicalProcess) {
	data.physicalStatus = physical.Status.DeepCopy()
	data.runtimeLogs = physical.GetLogSource()
	data.finishedAt = physical.Status.FinishedAt
	data.exitCode = cloneInt32Pointer(physical.Status.ExitCode)
}

func (data *projectProcessData) updateLifecycleFromProcess(physical *apiv2.PhysicalProcess) {
	message := logicalBackingStatusMessage("PhysicalProcess", physical.Status.Conditions)
	switch logicalProcessBackingState(physical) {
	case logicalProcessStateRunning:
		data.prepared = true
		data.state, data.reason, data.message = projectStateRunning, "", ""
	case logicalProcessStateExited:
		data.prepared = true
		data.state, data.reason, data.message = projectStateExited, apiv2.ProjectReasonCompleted, message
	case logicalProcessStateMissing, logicalProcessStateRuntimeFailed:
		data.failMissing(message)
	case logicalProcessStateCreating, logicalProcessStateStopping, logicalProcessStateUnknown:
		data.state, data.reason, data.message = projectStateCreating, apiv2.ProjectReasonPreparing, message
		if data.prepared {
			data.state, data.reason = projectStateUnknown, apiv2.ProjectReasonLookupFailed
		}
	default:
		data.state, data.reason, data.message = projectStateFailed, apiv2.PhysicalResourceReasonOperationStateInvalid,
			fmt.Sprintf("Unrecognized physical process phase %q.", physical.Status.Phase)
	}
}

func (r *ProjectProcessReconciler) cleanupProjectProcesses(
	ctx context.Context, resource *apiv2.ProjectProcess, data *projectProcessData, log logr.Logger,
) (bool, error) {
	var children apiv2.PhysicalProcessList
	listErr := r.List(ctx, &children, ctrl_client.InNamespace(resource.Namespace),
		ctrl_client.MatchingFields{projectProcessOwnerUIDField: string(resource.UID)})
	if listErr != nil {
		log.Error(listErr, "Failed to list Project process backings")
		return false, listErr
	}
	data.unobservedChildren.recordCreateAttempt(&apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: projectProcessPhysicalName(resource)}})
	resolveErr := resolveUnobservedLogicalChildren(ctx, r.NoCacheClient, resource, data.unobservedChildren, &children.Items)
	if resolveErr != nil {
		log.Error(resolveErr, "Failed to resolve Project process backings")
		return false, resolveErr
	}
	var cleanupErr error
	found := false
	for index := range children.Items {
		physical := &children.Items[index]
		if !metav1.IsControlledBy(physical, resource) {
			continue
		}
		found = true
		if physical.Name == projectProcessPhysicalName(resource) && (data.physicalUID == "" || data.physicalUID == physical.UID) {
			data.physicalUID, data.physicalName = physical.UID, physical.Name
			data.observePhysical(physical)
		}
		if physical.DeletionTimestamp.IsZero() {
			uid := physical.UID
			deleteErr := r.Delete(ctx, physical, ctrl_client.Preconditions{UID: &uid})
			if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				log.Error(deleteErr, "Failed to delete Project process backing")
				cleanupErr = errors.Join(cleanupErr, deleteErr)
			}
			continue
		}
		if hasFinalizer(physical, physicalProcessFinalizer) {
			continue
		}
		releaseErr := ReleaseLogicalBackingFinalizer(ctx, r.Client, physical)
		if releaseErr != nil && !apierrors.IsNotFound(releaseErr) {
			log.Error(releaseErr, "Failed to release Project process backing")
			cleanupErr = errors.Join(cleanupErr, releaseErr)
		}
	}
	return !found, cleanupErr
}

func handleProjectProcessPending(
	ctx context.Context, reconciler *ProjectProcessReconciler, resource *apiv2.ProjectProcess,
	_ projectState, data *projectProcessData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	data.reason, data.message = "", ""
	if resource.Spec.EffectiveLaunchMode() == apiv2.ProjectLaunchModeDirect {
		physical, lookupErr := reconciler.findProjectProcess(ctx, resource, data)
		if lookupErr != nil {
			log.Error(lookupErr, "Failed to discover Project process backing")
			return additionalReconciliationNeeded
		}
		if physical != nil {
			if !data.inputsPrepared {
				if len(projectProcessPortRequests(resource)) != 0 {
					data.state, data.reason, data.message = projectStateUnknown, apiv2.ProjectReasonLookupFailed,
						"Cannot recover the prepared service port selections for the existing process."
					return additionalReconciliationNeeded
				}
				if physical.Spec.Process == nil {
					data.failMissing("The owned PhysicalProcess does not contain a process configuration.")
					return noChange
				}
				data.effectiveArgs = slices.Clone(physical.Spec.Process.Args)
				data.effectiveEnv = slices.Clone(physical.Spec.Process.Env)
				data.inputsPrepared = true
			}
			data.state = projectStateCreating
			return additionalReconciliationNeeded
		}
	}
	if preparationErr := reconciler.prepareProjectValues(ctx, resource, data, log); preparationErr != nil {
		if errors.Is(preparationErr, errLogicalValuePreparationFailed) {
			data.state = projectStateFailed
			return noChange
		}
		return additionalReconciliationNeeded
	}
	if !resource.Spec.ShouldStart() {
		data.state = projectStateAwaitingStart
		return additionalReconciliationNeeded
	}
	if resource.Spec.EffectiveLaunchMode() == apiv2.ProjectLaunchModeDirect {
		physical, createErr := reconciler.createProjectProcess(ctx, resource, data)
		if createErr != nil {
			log.Error(createErr, "Failed to create Project process backing")
			if apierrors.IsAlreadyExists(createErr) {
				data.state, data.reason, data.message = projectStateFailed, apiv2.ProjectReasonPreparationFailed, createErr.Error()
				return noChange
			}
			return additionalReconciliationNeeded
		}
		data.physicalUID, data.physicalName = physical.UID, physical.Name
		data.state = projectStateCreating
		return additionalReconciliationNeeded
	}
	data.state, data.prepared = projectStateReady, true
	return additionalReconciliationNeeded
}

func handleProjectProcessAwaitingStart(
	_ context.Context, _ *ProjectProcessReconciler, resource *apiv2.ProjectProcess,
	_ projectState, data *projectProcessData, _ logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	if resource.Spec.ShouldStart() {
		data.state = projectStatePending
		return additionalReconciliationNeeded
	}
	return noChange
}

func handleProjectProcessPhysical(
	ctx context.Context, reconciler *ProjectProcessReconciler, resource *apiv2.ProjectProcess,
	state projectState, data *projectProcessData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	if !data.inputsPrepared {
		if state == projectStateUnknown {
			return reconciler.syncProjectEndpoints(ctx, resource, data, log)
		}
		data.state = projectStatePending
		return additionalReconciliationNeeded
	}
	physical, lookupErr := reconciler.findProjectProcess(ctx, resource, data)
	if lookupErr != nil || physical == nil {
		if lookupErr != nil {
			log.Error(lookupErr, "Failed to observe Project process backing")
		}
		if data.state != projectStateFailed {
			data.state, data.reason, data.message = projectStateUnknown, apiv2.ProjectReasonLookupFailed, "Waiting to observe the owned PhysicalProcess."
		}
		return reconciler.syncProjectEndpoints(ctx, resource, data, log) | additionalReconciliationNeeded
	}
	data.observePhysical(physical)
	data.updateLifecycleFromProcess(physical)
	return reconciler.syncProjectEndpoints(ctx, resource, data, log)
}

// handleProjectProcessReady tracks debug session activity; a prepared process context has no backing to observe.
func handleProjectProcessReady(
	ctx context.Context, reconciler *ProjectProcessReconciler, resource *apiv2.ProjectProcess,
	_ projectState, data *projectProcessData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	if mainSession := projectProcessMainSession(&resource.Status.ProjectCommonStatus); mainSession != nil &&
		(mainSession.Phase == apiv2.ProjectDebugPhaseExited || mainSession.Phase == apiv2.ProjectDebugPhaseFailed) {
		data.state, data.reason, data.message = projectStateExited, apiv2.ProjectReasonCompleted, "The main debug session has ended."
		data.finishedAt = mainSession.FinishedAt
		return reconciler.syncProjectEndpoints(ctx, resource, data, log)
	}
	data.state, data.reason, data.message = projectStateReady, "", ""
	if projectDebugSessionsActive(&resource.Status.ProjectCommonStatus) {
		data.state, data.message = projectStateActive, "Project has active debug sessions."
	}
	return reconciler.syncProjectEndpoints(ctx, resource, data, log)
}

// projectProcessMainSession returns the earliest-created debug session that connected its adapter transport.
// Its end ends an adapter-mode run, like the primary adapter of a container Project; sessions that fail setup do not start a run.
func projectProcessMainSession(status *apiv2.ProjectCommonStatus) *apiv2.ProjectDebugSessionStatus {
	if status.Debug == nil {
		return nil
	}
	for sessionIndex := range status.Debug.Sessions {
		session := &status.Debug.Sessions[sessionIndex]
		if session.Progress != nil && session.Progress.StreamsConnected {
			return session
		}
	}
	return nil
}

// handleProjectProcessExited observes the exited backing in direct mode; an adapter-mode run has no backing after it ends.
func handleProjectProcessExited(
	ctx context.Context, reconciler *ProjectProcessReconciler, resource *apiv2.ProjectProcess,
	state projectState, data *projectProcessData, log logr.Logger,
) objectChange {
	if resource.Spec.EffectiveLaunchMode() == apiv2.ProjectLaunchModeDirect {
		return handleProjectProcessPhysical(ctx, reconciler, resource, state, data, log)
	}
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	return reconciler.syncProjectEndpoints(ctx, resource, data, log)
}

func handleProjectProcessStopping(
	ctx context.Context, reconciler *ProjectProcessReconciler, resource *apiv2.ProjectProcess,
	_ projectState, data *projectProcessData, log logr.Logger,
) objectChange {
	data.reason, data.message = "", ""
	change := reconciler.syncProjectEndpoints(ctx, resource, data, log)
	if projectDebugSessionsActive(&resource.Status.ProjectCommonStatus) {
		data.reason, data.message = apiv2.ProjectReasonStopping, "Waiting for debug sessions and adapter cleanup to finish."
		return change | additionalReconciliationNeeded
	}
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	// Like LogicalProcess, stop is forwarded to the retained backing; only Project deletion removes it.
	physical, lookupErr := reconciler.findProjectProcess(ctx, resource, data)
	if lookupErr != nil {
		log.Error(lookupErr, "Failed to observe stopping Project process")
		return change | additionalReconciliationNeeded
	}
	if physical == nil {
		data.state, data.message = projectStateStopped, "Stop was requested before a process was created."
		return change | additionalReconciliationNeeded
	}
	data.observePhysical(physical)
	message := logicalBackingStatusMessage("PhysicalProcess", physical.Status.Conditions)
	switch logicalProcessBackingState(physical) {
	case logicalProcessStateExited:
		data.state, data.message = projectStateStopped, message
		return change | additionalReconciliationNeeded
	case logicalProcessStateMissing, logicalProcessStateRuntimeFailed:
		data.failMissing(message)
		return change | additionalReconciliationNeeded
	}
	return change | reconciler.requestBackingStop(ctx, physical, data, log)
}

func (r *ProjectProcessReconciler) requestBackingStop(
	ctx context.Context, physical *apiv2.PhysicalProcess, data *projectProcessData, log logr.Logger,
) objectChange {
	if !physical.Spec.Stop {
		patch := ctrl_client.MergeFromWithOptions(physical.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
		update := physical.DeepCopy()
		update.Spec.Stop = true
		if stopErr := r.Patch(ctx, update, patch); stopErr != nil {
			log.Error(stopErr, "Failed to forward Project stop request")
			data.message = fmt.Sprintf("Failed to forward Project stop request: %v", stopErr)
		}
	}
	return additionalReconciliationNeeded
}

func handleProjectProcessTerminal(
	ctx context.Context, reconciler *ProjectProcessReconciler, resource *apiv2.ProjectProcess,
	_ projectState, data *projectProcessData, log logr.Logger,
) objectChange {
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	if data.physicalUID != "" {
		lookupData := data.Clone()
		if physical, lookupErr := reconciler.findProjectProcess(ctx, resource, lookupData); lookupErr == nil && physical != nil {
			data.observePhysical(physical)
		} else if lookupErr != nil {
			log.Error(lookupErr, "Failed to observe terminal Project process backing")
		}
	}
	return reconciler.syncProjectEndpoints(ctx, resource, data, log)
}

func handleInvalidProjectProcessState(
	_ context.Context, _ *ProjectProcessReconciler, _ *apiv2.ProjectProcess, _ projectState, data *projectProcessData, log logr.Logger,
) objectChange {
	return handleInvalidProjectState(&data.projectData, log)
}

func (r *ProjectProcessReconciler) handleDeletionRequest(
	ctx context.Context, resource *apiv2.ProjectProcess, data *projectProcessData, log logr.Logger,
) (change objectChange, delay AdditionalReconciliationDelay) {
	delay = StandardDelay
	if data == nil {
		data = &projectProcessData{projectData: projectData{state: projectStatePending}}
		data.unobservedChildren.recordCreateAttempt(&apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: projectProcessPhysicalName(resource)}})
		for _, request := range projectProcessPortRequests(resource) {
			data.unobservedChildren.recordCreateAttempt(&apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: logicalPortResourceName(resource, request.binding.Name)}})
		}
		r.projects.Store(resource.NamespacedName(), resource.UID, data.Clone())
	}
	defer func() {
		_ = r.projects.Update(resource.NamespacedName(), resource.UID, data)
		projectionChange, _, valid := data.applyTo(resource)
		change |= projectionChange
		if !valid {
			log.Error(fmt.Errorf("invalid ProjectProcess state %v", data.state), "ProjectProcess reached invalid reconciliation state")
		}
	}()
	// Endpoints are withdrawn before waiting for debug sessions so deletion stops routing immediately.
	endpointsDeleted, endpointDeleteErr := cleanupLogicalEndpoints(ctx, r.Client, r.NoCacheClient, resource, projectProcessOwnerUIDField, data.unobservedChildren)
	if endpointDeleteErr != nil {
		log.Error(endpointDeleteErr, "Failed to delete ProjectProcess endpoints")
	}
	if projectDebugSessionsActive(&resource.Status.ProjectCommonStatus) {
		return additionalReconciliationNeeded, delay
	}
	observeProjectProcesses(r.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	cleaned, cleanupErr := r.cleanupProjectProcesses(ctx, resource, data, log)
	if !cleaned || cleanupErr != nil || !endpointsDeleted || endpointDeleteErr != nil {
		return additionalReconciliationNeeded, delay
	}
	return deleteFinalizer(resource, projectProcessFinalizer, log), delay
}

type projectProcessData struct {
	projectData
	physicalUID    types.UID
	physicalName   string
	physicalStatus *apiv2.PhysicalProcessStatus
	runtimeLogs    *apiv2.LogSource
}

func (data *projectProcessData) Clone() *projectProcessData {
	cloned := *data
	cloned.projectData = data.cloneProjectData()
	cloned.physicalStatus = data.physicalStatus.DeepCopy()
	cloned.runtimeLogs = data.runtimeLogs.DeepCopy()
	return &cloned
}

func (data *projectProcessData) UpdateFrom(other *projectProcessData) bool {
	if other == nil || reflect.DeepEqual(data, other) {
		return false
	}
	*data = *other.Clone()
	return true
}

func (data *projectProcessData) portData(resource *apiv2.ProjectProcess) []logicalPortData {
	return data.portDataFor(resource, projectProcessPortRequests(resource))
}

func (data *projectProcessData) applyTo(resource *apiv2.ProjectProcess) (objectChange, AdditionalReconciliationDelay, bool) {
	physicalRef := ""
	if data.physicalUID != "" {
		physicalRef = data.physicalName
	}
	change := setValue(&resource.Status.PhysicalProcessRef, physicalRef)
	if data.physicalStatus != nil {
		if !reflect.DeepEqual(resource.Status.PID, data.physicalStatus.PID) {
			resource.Status.PID = data.physicalStatus.DeepCopy().PID
			change |= statusChanged
		}
		change |= setTimestamp(&resource.Status.IdentityTimestamp, data.physicalStatus.IdentityTimestamp)
	}
	source := data.runtimeLogs
	if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
		resource.Status.RuntimeLogs = source.DeepCopy()
		change |= statusChanged
	}
	commonChange, delay, valid := data.applyCommonTo(resource, projectProcessPortRequests(resource), true)
	return change | commonChange, delay, valid
}
