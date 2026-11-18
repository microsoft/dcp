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
	projectStatePending:  handleProjectProcessPending,
	projectStateReady:    handleProjectProcessReady,
	projectStateActive:   handleProjectProcessReady,
	projectStateStopping: handleProjectProcessStopping,
	projectStateStopped:  handleProjectProcessTerminal,
	projectStateFailed:   handleProjectProcessTerminal,
	0:                    handleInvalidProjectProcessState,
}

// ProjectProcessReconciler prepares process launch inputs for AppHost-controlled debugging of user code.
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
	indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, &apiv2.ServiceEndpoint{}, projectProcessOwnerUIDField, func(obj ctrl_client.Object) []string {
		owner := metav1.GetControllerOf(obj)
		if owner == nil || owner.Kind != "ProjectProcess" {
			return nil
		}
		return []string{string(owner.UID)}
	})
	if indexErr != nil {
		return fmt.Errorf("index process project endpoint owner: %w", indexErr)
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.ProjectProcess{}).
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

func handleProjectProcessPending(
	ctx context.Context, reconciler *ProjectProcessReconciler, resource *apiv2.ProjectProcess,
	_ projectState, data *projectProcessData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	data.reason, data.message = "", ""
	if preparationErr := reconciler.prepareProjectValues(ctx, resource, data, log); preparationErr != nil {
		if errors.Is(preparationErr, errLogicalValuePreparationFailed) {
			data.state = projectStateFailed
			return noChange
		}
		return additionalReconciliationNeeded
	}
	data.state, data.prepared = projectStateReady, true
	return additionalReconciliationNeeded
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
	data.state, data.reason, data.message = projectStateReady, "", ""
	if projectDebugSessionsActive(&resource.Status.ProjectCommonStatus) {
		data.state, data.message = projectStateActive, "Project has active debug sessions."
	}
	return reconciler.syncProjectEndpoints(ctx, resource, data, log)
}

func handleProjectProcessStopping(
	ctx context.Context, reconciler *ProjectProcessReconciler, resource *apiv2.ProjectProcess,
	_ projectState, data *projectProcessData, log logr.Logger,
) objectChange {
	data.reason, data.message = "", ""
	if endpointChange := reconciler.syncProjectEndpoints(ctx, resource, data, log); endpointChange != noChange {
		return endpointChange
	}
	if projectDebugSessionsActive(&resource.Status.ProjectCommonStatus) {
		data.reason, data.message = apiv2.ProjectReasonStopping, "Waiting for debug sessions and adapter cleanup to finish."
		return additionalReconciliationNeeded
	}
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	data.state = projectStateStopped
	return noChange
}

func handleProjectProcessTerminal(
	ctx context.Context, reconciler *ProjectProcessReconciler, resource *apiv2.ProjectProcess,
	_ projectState, data *projectProcessData, log logr.Logger,
) objectChange {
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
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
	if !endpointsDeleted || endpointDeleteErr != nil {
		return additionalReconciliationNeeded, delay
	}
	return deleteFinalizer(resource, projectProcessFinalizer, log), delay
}

type projectProcessData struct {
	projectData
}

func (data *projectProcessData) Clone() *projectProcessData {
	return &projectProcessData{projectData: data.cloneProjectData()}
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
	return data.applyCommonTo(resource, projectProcessPortRequests(resource), true)
}
