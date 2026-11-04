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
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/version"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
)

const (
	projectNetworkRefField = "spec.container.networks.networkRef"
	projectOwnerUIDField   = ".metadata.projectOwnerUID"
)

var projectFinalizer = apiv2.GroupName + "/project-reconciler"
var errProjectImageChanged = errors.New("project image identity or configuration changed")
var errMultipleProjectBackings = errors.New("multiple owned project backings")

type projectDataHandlerFunc = stateInitializerFunc[
	apiv2.Project, *apiv2.Project, ProjectReconciler, *ProjectReconciler,
	projectState, projectData, *projectData,
]

var projectDataHandlers = map[projectState]projectDataHandlerFunc{
	projectStatePending:  handleProjectPending,
	projectStateCreating: handleProjectCreating,
	projectStateReady:    handleProjectReady,
	projectStateActive:   handleProjectActive,
	projectStateStopping: handleProjectStopping,
	projectStateStopped:  handleProjectTerminal,
	projectStateFailed:   handleProjectTerminal,
	projectStateMissing:  handleProjectTerminal,
	projectStateDelete:   handleInvalidProjectState,
	projectStateInvalid:  handleInvalidProjectState,
	0:                    handleInvalidProjectState,
}

// ProjectReconciler prepares launch inputs and reusable context for AppHost-controlled debugging.
type ProjectReconciler struct {
	*ReconcilerBase[apiv2.Project, *apiv2.Project]
	projects *ObjectStateMap[types.UID, projectData, *projectData, *apiv2.Project]
}

func NewProjectReconciler(ctx context.Context, client ctrl_client.Client, reader ctrl_client.Reader, log logr.Logger) *ProjectReconciler {
	return &ProjectReconciler{
		ReconcilerBase: NewReconcilerBase[apiv2.Project](client, reader, log, ctx),
		projects:       NewObjectStateMap[types.UID, projectData, *projectData, *apiv2.Project](),
	}
}

func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	for fieldName, index := range map[string]ctrl_client.IndexerFunc{
		projectNetworkRefField: func(obj ctrl_client.Object) []string {
			resource := obj.(*apiv2.Project)
			if resource.Spec.Container == nil {
				return nil
			}
			references := make([]string, 0, len(resource.Spec.Container.Networks))
			for _, network := range resource.Spec.Container.Networks {
				references = append(references, commonapi.AsNamespacedName(network.NetworkRef, resource.Namespace).Name)
			}
			return references
		},
	} {
		indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, &apiv2.Project{}, fieldName, index)
		if indexErr != nil {
			return fmt.Errorf("index project %s: %w", fieldName, indexErr)
		}
	}
	for _, backing := range []ctrl_client.Object{&apiv2.PhysicalProcess{}, &apiv2.PhysicalContainer{}, &apiv2.PhysicalContainerImage{}} {
		indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, backing, projectOwnerUIDField, func(obj ctrl_client.Object) []string {
			owner := metav1.GetControllerOf(obj)
			if owner == nil {
				return nil
			}
			return []string{string(owner.UID)}
		})
		if indexErr != nil {
			return fmt.Errorf("index project backing owner: %w", indexErr)
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.Project{}).
		Owns(&apiv2.PhysicalProcess{}).Owns(&apiv2.PhysicalContainer{}).Owns(&apiv2.PhysicalContainerImage{}).
		Owns(&apiv2.ServiceEndpoint{}).
		Watches(&apiv2.LogicalContainerNetwork{}, handler.EnqueueRequestsFromMapFunc(r.mapProjectReference(projectNetworkRefField))).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.ProjectList{}))).
		Named(name).Complete(r)
}

func (r *ProjectReconciler) mapProjectReference(indexField string) handler.MapFunc {
	return func(ctx context.Context, obj ctrl_client.Object) []ctrl.Request {
		var resources apiv2.ProjectList
		listErr := r.List(ctx, &resources, ctrl_client.InNamespace(obj.GetNamespace()), ctrl_client.MatchingFields{indexField: obj.GetName()})
		if listErr != nil {
			r.Log.Error(listErr, "Failed to list referencing Projects", "Resource", ctrl_client.ObjectKeyFromObject(obj))
			return nil
		}
		requests := make([]ctrl.Request, len(resources.Items))
		for i := range resources.Items {
			requests[i] = ctrl.Request{NamespacedName: resources.Items[i].NamespacedName()}
		}
		return requests
	}
}

func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)
	if ctx.Err() != nil {
		return ctrl.Result{}, nil
	}

	var resource apiv2.Project
	getErr := reader.Get(ctx, req.NamespacedName, &resource)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			uid, _ := r.projects.BorrowByNamespacedName(req.NamespacedName)
			r.projects.DeleteByNamespacedName(req.NamespacedName)
			logger.ReleaseResourceLog((&apiv2.Project{ObjectMeta: metav1.ObjectMeta{UID: uid}}).GetResourceId())
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to get Project")
		return ctrl.Result{}, getErr
	}

	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, resource.GetResourceId())
	stateKey, data := r.projects.BorrowByNamespacedName(req.NamespacedName)
	if data != nil && stateKey != resource.UID {
		logger.ReleaseResourceLog((&apiv2.Project{ObjectMeta: metav1.ObjectMeta{UID: stateKey}}).GetResourceId())
		r.projects.DeleteByNamespacedName(req.NamespacedName)
		data = nil
	}

	patch := ctrl_client.MergeFromWithOptions(resource.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	delay := StandardDelay

	if !resource.DeletionTimestamp.IsZero() {
		change, delay = r.handleDeletionRequest(ctx, &resource, data, log)
	} else {
		change = ensureFinalizer(&resource, projectFinalizer, log)
		if change == noChange {
			change, delay = r.manageProject(ctx, &resource, data, log)
		}
	}

	if !hasFinalizer(&resource, projectFinalizer) {
		r.projects.DeleteByNamespacedName(req.NamespacedName)
		defer logger.ReleaseResourceLog(resource.GetResourceId())
	}

	return r.SaveChangesWithDelay(ctx, &resource, patch, change, delay, nil, log)
}

func (r *ProjectReconciler) manageProject(
	ctx context.Context, resource *apiv2.Project, data *projectData, log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	if data == nil {
		ready, reason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, resource.Namespace)
		if !ready {
			change := noChange
			phase, message := apiv2.ProjectPhasePending, namespaceReadinessMessage(resource.Namespace, reason)
			if namespaceErr != nil {
				log.Error(namespaceErr, "Failed to get Project namespace")
				phase, message = apiv2.ProjectPhaseUnknown, namespaceErr.Error()
				change |= additionalReconciliationNeeded
			}
			change |= setValue(&resource.Status.Phase, phase)
			change |= setCondition(&resource.Status.Conditions, apiv2.ConditionReady, resource.Generation, metav1.ConditionFalse, reason, message)
			if resource.Status.Debug != nil {
				change |= setValue(&resource.Status.Debug.PrerequisitesReady, false)
			}
			return change, StandardDelay
		}
		data = &projectData{state: projectStatePending}
		r.projects.Store(resource.NamespacedName(), resource.UID, data.Clone())
	}

	if resource.Spec.Stop && data.state != projectStateStopped {
		data.state = projectStateStopping
		if data.stoppingAt.IsZero() {
			data.stoppingAt = time.Now()
		}
	}
	stateHandler := getStateInitializer(projectDataHandlers, data.state, log)
	change := stateHandler(ctx, r, resource, data.state, data, log)
	_ = r.projects.Update(resource.NamespacedName(), resource.UID, data)
	projectionChange, delay, valid := data.applyTo(resource)
	if !valid {
		log.Error(fmt.Errorf("invalid Project state %v", data.state), "Project reached invalid reconciliation state")
	}
	return change | projectionChange, delay
}

func handleProjectPending(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	data.reconcileErr, data.reason, data.message, data.retryAfter = nil, "", "", time.Time{}
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReadiness(ctx, reconciler.Client, resource.Namespace)
	if !namespaceReady {
		data.reason, data.message = namespaceReason, namespaceReadinessMessage(resource.Namespace, namespaceReason)
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get Project namespace during preparation")
			data.retry(namespaceReason, namespaceErr)
		}
		return noChange
	}
	if resource.Spec.Process != nil {
		if !reconciler.prepareProjectValues(ctx, resource, data, log) {
			if data.state == projectStateFailed {
				return noChange
			}
			return additionalReconciliationNeeded
		}
		data.state = projectStateReady
		return additionalReconciliationNeeded
	}
	var backing ctrl_client.Object = &apiv2.PhysicalContainer{}
	var backingList ctrl_client.ObjectList = &apiv2.PhysicalContainerList{}
	name := types.NamespacedName{Namespace: resource.Namespace, Name: projectPhysicalName(resource)}
	observed, getErr := reconciler.findProjectBacking(ctx, resource, backingList)
	if getErr != nil {
		log.Error(getErr, "Failed to resolve Project physical creation outcome")
		data.retry("PhysicalPreparationFailed", getErr)
		if errors.Is(getErr, errMultipleProjectBackings) {
			data.state, data.reconcileErr = projectStateFailed, nil
		}
		return noChange
	}
	change := noChange
	if observed == nil {
		if data.physicalUID != "" {
			data.state, data.reason, data.message = projectStateMissing, "PhysicalExecutionChanged", "The physical backing no longer exists."
			return noChange
		}
		if resource.Spec.Stop {
			data.state = projectStateStopped
			data.reason, data.message = "ProjectStopped", "Stop was requested before physical execution was prepared."
			return noChange
		}
		if !reconciler.prepareProjectValues(ctx, resource, data, log) {
			if data.state == projectStateFailed {
				return noChange
			}
			return additionalReconciliationNeeded
		}
		if resource.Spec.Container != nil {
			if imageErr := reconciler.ensureProjectImage(ctx, resource, data); imageErr != nil {
				log.Error(imageErr, "Failed to prepare Project image")
				data.retry("ImagePreparationFailed", imageErr)
				if errors.Is(imageErr, errProjectImageChanged) || errors.Is(imageErr, errMultipleProjectBackings) {
					data.state, data.reconcileErr = projectStateFailed, nil
				}
				return change
			}
			change |= reconciler.projectImageLogs(ctx, resource, data, log)
			if !reconciler.selectProjectNetworks(ctx, resource, data, log) {
				return change
			}
		}
		backing.SetName(name.Name)
		backing.SetNamespace(name.Namespace)
		backing.(*apiv2.PhysicalContainer).Spec = projectContainerSpec(resource, data)
		ownerErr := ctrl.SetControllerReference(resource, backing, reconciler.Scheme())
		if ownerErr != nil {
			log.Error(ownerErr, "Failed to set Project physical owner")
			data.state, data.reason, data.message = projectStateFailed, "PhysicalPreparationFailed", ownerErr.Error()
			return change
		}
		InitializeLogicalBackingFinalizers(backing, physicalContainerFinalizer)
		createErr := reconciler.Create(ctx, backing)
		if createErr != nil {
			confirmed, confirmErr := reconciler.listProjectBackings(ctx, resource, reconciler.NoCacheClient, backingList)
			if confirmErr != nil || len(confirmed) != 1 {
				if len(confirmed) > 1 {
					confirmErr = errMultipleProjectBackings
				}
				failure := fmt.Errorf("create project backing and confirm owned resources: %w", errors.Join(createErr, confirmErr))
				log.Error(failure, "Failed to create Project physical backing")
				data.retry("PhysicalPreparationFailed", failure)
				if errors.Is(confirmErr, errMultipleProjectBackings) {
					data.state, data.reconcileErr = projectStateFailed, nil
				}
				return change
			}
			backing = confirmed[0]
		}
	} else {
		backing = observed
		if physical, container := backing.(*apiv2.PhysicalContainer); container && !data.networksSelected && physical.Spec.Container != nil {
			// A committed declaration pins dependency identity even when its create response was lost.
			data.networks = slices.Clone(physical.Spec.Container.Networks)
			data.networksSelected = true
		}
	}
	matches := metav1.IsControlledBy(backing, resource) && backing.GetDeletionTimestamp().IsZero() &&
		hasFinalizer(backing, LogicalBackingFinalizer) && (data.physicalUID == "" || data.physicalUID == backing.GetUID())
	switch physical := backing.(type) {
	case *apiv2.PhysicalContainer:
		actual, expected := physical.Spec, projectContainerSpec(resource, data)
		actual.Stop, expected.Stop = false, false
		matches = matches && reflect.DeepEqual(actual, expected)
	}
	if !matches {
		data.state, data.reason = projectStateFailed, "PhysicalExecutionChanged"
		data.message = "The physical backing has a different owner or configuration, or is being deleted."
		log.Error(errors.New("physical backing owner or configuration changed, or is being deleted"), "Project physical backing changed")
		return change
	}
	data.physicalUID, data.physicalName = backing.GetUID(), backing.GetName()
	data.networks, data.networksSelected = nil, false
	data.state, data.retryAfter = projectStateCreating, time.Time{}
	return change | additionalReconciliationNeeded
}

func (r *ProjectReconciler) selectProjectNetworks(ctx context.Context, resource *apiv2.Project, data *projectData, log logr.Logger) bool {
	var selected []apiv2.ContainerNetworkConnectionConfig
	var pendingMessage string
	var resolutionErr error
	failed := false
	for _, requested := range resource.Spec.Container.Networks {
		var network apiv2.LogicalContainerNetwork
		getErr := r.Get(ctx, commonapi.AsNamespacedName(requested.NetworkRef, resource.Namespace), &network)
		if getErr != nil {
			if !apierrors.IsNotFound(getErr) {
				resolutionErr = errors.Join(resolutionErr, fmt.Errorf("get logical network %q: %w", requested.NetworkRef, getErr))
			} else if pendingMessage == "" {
				pendingMessage = fmt.Sprintf("Waiting for LogicalContainerNetwork %q.", requested.NetworkRef)
			}
			continue
		}
		if network.Status.Phase == apiv2.LogicalContainerNetworkPhaseFailed {
			message := fmt.Sprintf("LogicalContainerNetwork %q failed.", network.NamespacedName())
			if condition := apimeta.FindStatusCondition(network.Status.Conditions, string(apiv2.ConditionReady)); condition != nil {
				message = fmt.Sprintf("LogicalContainerNetwork %q reports %s: %s", network.NamespacedName(), condition.Reason, condition.Message)
			}
			resolutionErr = errors.Join(resolutionErr, errors.New(message))
			failed = true
			continue
		}
		if !network.DeletionTimestamp.IsZero() || network.Status.PhysicalNetworkRef == "" {
			if pendingMessage == "" {
				pendingMessage = fmt.Sprintf("Waiting for LogicalContainerNetwork %q to publish its backing reference.", network.NamespacedName())
			}
			continue
		}
		selected = append(selected, apiv2.ContainerNetworkConnectionConfig{NetworkRef: network.Status.PhysicalNetworkRef, Aliases: slices.Clone(requested.Aliases)})
	}
	if resolutionErr != nil {
		log.Error(resolutionErr, "Failed to resolve Project networks")
		if failed {
			data.state, data.reason, data.message = projectStateFailed, "NetworkFailed", resolutionErr.Error()
		} else {
			data.retry("NetworkLookupFailed", resolutionErr)
		}
		return false
	}
	if pendingMessage != "" {
		data.reason, data.message = "NetworkNotReady", pendingMessage
		return false
	}
	if data.networksSelected && !reflect.DeepEqual(data.networks, selected) {
		data.state, data.reason, data.message = projectStateFailed, "NetworkChanged", "A selected logical network's physical backing changed before physical realization."
		return false
	}
	data.networks, data.networksSelected = selected, true
	return true
}

func (r *ProjectReconciler) ensureProjectImage(ctx context.Context, resource *apiv2.Project, data *projectData) error {
	if resource.Spec.Container.Image == nil {
		return nil
	}
	observed, getErr := r.findProjectBacking(ctx, resource, &apiv2.PhysicalContainerImageList{})
	if getErr != nil {
		return fmt.Errorf("find project image: %w", getErr)
	}
	image := &apiv2.PhysicalContainerImage{}
	name := types.NamespacedName{Namespace: resource.Namespace, Name: projectPhysicalName(resource)}
	if observed == nil && data.imageUID == "" {
		image.ObjectMeta = metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace}
		image.Spec.Image = resource.Spec.Container.Image.DeepCopy()
		ownerErr := ctrl.SetControllerReference(resource, image, r.Scheme())
		if ownerErr != nil {
			return fmt.Errorf("set project image owner: %w", ownerErr)
		}
		InitializeLogicalBackingFinalizers(image, physicalContainerImageFinalizer)
		createErr := r.Create(ctx, image)
		if createErr != nil {
			confirmed, confirmErr := r.listProjectBackings(ctx, resource, r.NoCacheClient, &apiv2.PhysicalContainerImageList{})
			if len(confirmed) > 1 {
				confirmErr = errMultipleProjectBackings
			}
			if confirmErr != nil || len(confirmed) != 1 {
				return fmt.Errorf("create project image and confirm owned resources: %w", errors.Join(createErr, confirmErr))
			}
			image = confirmed[0].(*apiv2.PhysicalContainerImage)
		}
	} else if observed == nil {
		return fmt.Errorf("%w: backing image %q is missing", errProjectImageChanged, name)
	} else {
		image = observed.(*apiv2.PhysicalContainerImage)
	}
	if !metav1.IsControlledBy(image, resource) || !reflect.DeepEqual(image.Spec, apiv2.PhysicalContainerImageSpec{Image: resource.Spec.Container.Image}) ||
		(data.imageUID != "" && image.UID != data.imageUID) || !image.DeletionTimestamp.IsZero() ||
		!hasFinalizer(image, LogicalBackingFinalizer) {
		return fmt.Errorf("%w: image %q has a different owner or configuration, or is being deleted", errProjectImageChanged, name)
	}
	data.imageUID, data.imageName = image.UID, image.Name
	return nil
}

func handleProjectCreating(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	change := reconciler.observeProjectContainerContext(ctx, resource, data, log)
	if data.state == projectStateReady {
		change |= additionalReconciliationNeeded
	}
	return change
}

func (reconciler *ProjectReconciler) observeProjectContainerContext(
	ctx context.Context, resource *apiv2.Project, data *projectData, log logr.Logger,
) objectChange {
	data.reconcileErr, data.reason, data.message = nil, "", ""
	observation, getErr := reconciler.observeProjectContainer(ctx, resource, data.physicalUID)
	if apierrors.IsNotFound(getErr) || errors.Is(getErr, errProjectBackingChanged) || errors.Is(getErr, errMultipleProjectBackings) {
		data.state, data.reason, data.message = projectStateMissing, "ContainerContextMissing", "The prepared container is missing or its identity or owner changed."
		return reconciler.withdrawProjectEndpoints(ctx, resource, data, log)
	}
	if getErr != nil {
		log.Error(getErr, "Failed to observe Project container preparation")
		data.retry("ContainerLookupFailed", getErr)
		return noChange
	}
	backing := observation.backing
	change := observation.change
	data.state, data.reason, data.message, data.retryAfter = projectStateCreating, "", "", time.Time{}
	if !reflect.DeepEqual(resource.Status.RuntimeLogs, observation.logs) {
		resource.Status.RuntimeLogs = observation.logs
		change |= statusChanged
	}
	condition := apimeta.FindStatusCondition(observation.conditions, string(apiv2.ConditionReady))
	if !backing.GetDeletionTimestamp().IsZero() {
		data.state, data.reason, data.message = projectStateFailed, "ContainerContextTerminating", "The prepared container is being deleted."
	} else if condition != nil && condition.ObservedGeneration == backing.GetGeneration() {
		data.message = fmt.Sprintf("Container preparation reports %s: %s", condition.Reason, condition.Message)
		switch observation.phase {
		case apiv2.PhysicalResourcePhaseRunning:
			if condition.Status == metav1.ConditionTrue {
				data.state = projectStateReady
			}
		case apiv2.PhysicalResourcePhaseExited, apiv2.PhysicalResourcePhaseFailed:
			data.state = projectStateFailed
		case apiv2.PhysicalResourcePhaseUnknown, apiv2.PhysicalResourcePhasePaused, apiv2.PhysicalResourcePhasePending, "":
		default:
			return change | handleInvalidProjectState(ctx, reconciler, resource, data.state, data, log)
		}
	}
	change |= reconciler.projectImageLogs(ctx, resource, data, log)
	return change | reconciler.withdrawProjectEndpoints(ctx, resource, data, log)
}

func handleProjectReady(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	data.reconcileErr, data.reason, data.message = nil, "", ""
	ready, reason, namespaceErr := checkNamespaceReadiness(ctx, reconciler.Client, resource.Namespace)
	if !ready {
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to observe Project namespace")
			data.retry(reason, namespaceErr)
		} else {
			data.state, data.reason, data.message = projectStatePending, reason, namespaceReadinessMessage(resource.Namespace, reason)
		}
		return additionalReconciliationNeeded
	}
	change := noChange
	if resource.Spec.Container != nil {
		change = reconciler.observeProjectContainerContext(ctx, resource, data, log)
		if data.state != projectStateReady {
			return change
		}
	} else {
		data.state = projectStateReady
	}
	if resource.Status.Debug != nil {
		for _, session := range resource.Status.Debug.Sessions {
			if session.Phase == apiv2.ProjectDebugPhasePending || session.Phase == apiv2.ProjectDebugPhaseRunning {
				data.state = projectStateActive
				break
			}
		}
	}
	return change
}

func handleProjectActive(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	state projectState, data *projectData, log logr.Logger,
) objectChange {
	change := handleProjectReady(ctx, reconciler, resource, state, data, log)
	if data.state == projectStateActive {
		data.message = "Project has active debug sessions."
	}
	return change
}

func handleProjectTerminal(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	if data.physicalUID != "" && data.state == projectStateFailed {
		reason, message := data.reason, data.message
		change := reconciler.observeProjectContainerContext(ctx, resource, data, log)
		if data.state != projectStateMissing {
			data.state, data.reason, data.message = projectStateFailed, reason, message
		}
		return change
	}
	if resource.Spec.Container != nil && data.imageUID != "" && data.state != projectStateStopped {
		return reconciler.projectImageLogs(ctx, resource, data, log)
	}
	return noChange
}

var errProjectBackingChanged = errors.New("project physical backing identity or owner changed")

// projectRuntimeObservation is a per-reconcile view of the selected physical execution.
type projectRuntimeObservation struct {
	backing    ctrl_client.Object
	phase      apiv2.PhysicalResourcePhase
	conditions []metav1.Condition
	logs       *apiv2.LogSource
	change     objectChange
}

func (r *ProjectReconciler) getProjectPhysicalExecution(
	ctx context.Context, resource *apiv2.Project, uid types.UID, backing ctrl_client.Object,
) error {
	var list ctrl_client.ObjectList = &apiv2.PhysicalProcessList{}
	if _, container := backing.(*apiv2.PhysicalContainer); container {
		list = &apiv2.PhysicalContainerList{}
	}
	observed, getErr := r.findProjectBacking(ctx, resource, list)
	if getErr != nil {
		return getErr
	}
	if observed == nil || observed.GetUID() != uid || !hasFinalizer(observed, LogicalBackingFinalizer) {
		return errProjectBackingChanged
	}
	switch physical := backing.(type) {
	case *apiv2.PhysicalProcess:
		*physical = *observed.(*apiv2.PhysicalProcess)
	case *apiv2.PhysicalContainer:
		*physical = *observed.(*apiv2.PhysicalContainer)
	}
	return nil
}

func (r *ProjectReconciler) listProjectBackings(
	ctx context.Context, resource *apiv2.Project, reader ctrl_client.Reader, list ctrl_client.ObjectList, options ...ctrl_client.ListOption,
) ([]ctrl_client.Object, error) {
	listOptions := append([]ctrl_client.ListOption{ctrl_client.InNamespace(resource.Namespace)}, options...)
	if listErr := reader.List(ctx, list, listOptions...); listErr != nil {
		return nil, fmt.Errorf("list project backings: %w", listErr)
	}
	items, extractErr := apimeta.ExtractList(list)
	if extractErr != nil {
		return nil, fmt.Errorf("extract project backings: %w", extractErr)
	}
	var children []ctrl_client.Object
	for _, item := range items {
		child, valid := item.(ctrl_client.Object)
		if !valid {
			return nil, fmt.Errorf("invalid project backing type %T", item)
		}
		if metav1.IsControlledBy(child, resource) {
			children = append(children, child)
		}
	}
	return children, nil
}

func (r *ProjectReconciler) findProjectBacking(
	ctx context.Context, resource *apiv2.Project, list ctrl_client.ObjectList,
) (ctrl_client.Object, error) {
	children, listErr := r.listProjectBackings(ctx, resource, r.Client, list,
		ctrl_client.MatchingFields{projectOwnerUIDField: string(resource.UID)})
	if listErr == nil && len(children) == 0 {
		children, listErr = r.listProjectBackings(ctx, resource, r.NoCacheClient, list)
	}
	if listErr != nil {
		return nil, listErr
	}
	if len(children) > 1 {
		return nil, fmt.Errorf("%w: %T", errMultipleProjectBackings, list)
	}
	if len(children) == 0 {
		return nil, nil
	}
	return children[0], nil
}

func (r *ProjectReconciler) observeProjectContainer(
	ctx context.Context, resource *apiv2.Project, uid types.UID,
) (*projectRuntimeObservation, error) {
	physical := &apiv2.PhysicalContainer{}
	if getErr := r.getProjectPhysicalExecution(ctx, resource, uid, physical); getErr != nil {
		return nil, getErr
	}
	change := setValue(&resource.Status.ContainerID, physical.Status.ContainerID)
	change |= setValue(&resource.Status.ContainerName, physical.Status.ContainerName)
	change |= setValue(&resource.Status.Image, physical.Status.Image)
	change |= setTimestamp(&resource.Status.CreatedAt, physical.Status.CreatedAt)
	change |= setTimestamp(&resource.Status.StartedAt, physical.Status.StartedAt)
	if !reflect.DeepEqual(resource.Status.PortMappings, physical.Status.PortMappings) {
		resource.Status.PortMappings = slices.Clone(physical.Status.PortMappings)
		change |= statusChanged
	}
	return &projectRuntimeObservation{
		backing: physical, phase: apiv2.PhysicalResourcePhase(physical.Status.Phase),
		conditions: physical.Status.Conditions, logs: physical.GetLogSource(), change: change,
	}, nil
}

func (r *ProjectReconciler) projectImageLogs(ctx context.Context, resource *apiv2.Project, data *projectData, log logr.Logger) objectChange {
	if resource.Spec.Container == nil || resource.Spec.Container.Image == nil {
		if resource.Status.StartupLogs != nil {
			resource.Status.StartupLogs = nil
			return statusChanged
		}
		return noChange
	}
	observed, getErr := r.findProjectBacking(ctx, resource, &apiv2.PhysicalContainerImageList{})
	if getErr != nil {
		log.Error(getErr, "Failed to observe Project startup logs")
		return additionalReconciliationNeeded
	}
	var source *apiv2.LogSource
	if observed != nil && (data.imageUID == "" || data.imageUID == observed.GetUID()) {
		image := observed.(*apiv2.PhysicalContainerImage)
		data.imageUID, data.imageName = image.UID, image.Name
		source = image.GetLogSource()
	} else if data.imageUID != "" {
		source = &apiv2.LogSource{Name: data.imageName, UID: data.imageUID, Complete: true}
	}
	if !reflect.DeepEqual(resource.Status.StartupLogs, source) {
		resource.Status.StartupLogs = source
		return statusChanged
	}
	return noChange
}

func (r *ProjectReconciler) handleDeletionRequest(
	ctx context.Context, resource *apiv2.Project, data *projectData, log logr.Logger,
) (change objectChange, delay AdditionalReconciliationDelay) {
	delay = StandardDelay
	if data == nil {
		data = &projectData{state: projectStateDelete}
		r.projects.Store(resource.NamespacedName(), resource.UID, data.Clone())
	}
	defer func() {
		if data != nil {
			_ = r.projects.Update(resource.NamespacedName(), resource.UID, data)
			projectionChange, projectionDelay, valid := data.applyTo(resource)
			change |= projectionChange
			delay = projectionDelay
			if !valid {
				log.Error(fmt.Errorf("invalid Project state %v", data.state), "Project reached invalid reconciliation state")
			}
		}
	}()
	data.state, data.reason, data.message, data.reconcileErr = projectStateDelete, "", "", nil

	if resource.Status.Debug != nil {
		active := false
		for _, session := range resource.Status.Debug.Sessions {
			active = active || session.Phase == apiv2.ProjectDebugPhasePending || session.Phase == apiv2.ProjectDebugPhaseRunning
		}
		for _, execution := range resource.Status.Debug.Executions {
			active = active || execution.Phase == apiv2.ProjectDebugPhasePending || execution.Phase == apiv2.ProjectDebugPhaseRunning ||
				(execution.PID != nil && execution.FinishedAt.IsZero())
		}
		if active && time.Now().Before(resource.DeletionTimestamp.Add(apiv2.ProjectDebugShutdownGrace)) {
			data.reason, data.message = "DebugShutdownPending", "Waiting for debug sessions to finish graceful shutdown."
			return additionalReconciliationNeeded, delay
		}
		if active {
			data.reason, data.message = "DebugShutdownGraceExpired", "Debug shutdown grace elapsed; continuing physical backing cleanup."
			log.Info("Debug shutdown grace elapsed; continuing Project backing cleanup")
		}
	}

	cleaned, cleanupErr := r.cleanupProjectBackings(ctx, resource, log)
	if cleanupErr != nil {
		data.retry("PhysicalDeletionFailed", cleanupErr)
		return additionalReconciliationNeeded, delay
	}
	if !cleaned {
		return additionalReconciliationNeeded, delay
	}
	r.projects.DeleteByNamespacedName(resource.NamespacedName())
	data = nil
	return deleteFinalizer(resource, projectFinalizer, log), delay
}

func (r *ProjectReconciler) cleanupProjectBackings(
	ctx context.Context, resource *apiv2.Project, log logr.Logger,
) (bool, error) {
	for _, group := range []struct {
		list      ctrl_client.ObjectList
		finalizer string
	}{
		{&apiv2.PhysicalProcessList{}, physicalProcessFinalizer},
		{&apiv2.PhysicalContainerList{}, physicalContainerFinalizer},
		{&apiv2.PhysicalContainerImageList{}, physicalContainerImageFinalizer},
	} {
		children, listErr := r.listProjectBackings(ctx, resource, r.Client, group.list,
			ctrl_client.MatchingFields{projectOwnerUIDField: string(resource.UID)})
		if listErr == nil && len(children) == 0 {
			children, listErr = r.listProjectBackings(ctx, resource, r.NoCacheClient, group.list)
		}
		if listErr != nil {
			log.Error(listErr, "Failed to verify Project backings before deletion")
			return false, listErr
		}
		if len(children) == 0 {
			continue
		}
		var cleanupErr error
		for _, backing := range children {
			if backing.GetDeletionTimestamp().IsZero() {
				uid := backing.GetUID()
				deleteErr := r.Delete(ctx, backing, ctrl_client.Preconditions{UID: &uid})
				if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
					log.Error(deleteErr, "Failed to delete Project backing")
					cleanupErr = errors.Join(cleanupErr, deleteErr)
				}
				continue
			}
			if hasFinalizer(backing, group.finalizer) {
				continue
			}
			releaseErr := ReleaseLogicalBackingFinalizer(ctx, r.Client, backing)
			if releaseErr != nil && !apierrors.IsNotFound(releaseErr) {
				log.Error(releaseErr, "Failed to release Project backing")
				cleanupErr = errors.Join(cleanupErr, releaseErr)
			}
		}
		return false, cleanupErr
	}
	portsDeleted, portDeleteErr := cleanupLogicalEndpoints(ctx, r.Client, r.NoCacheClient, resource, projectPortBindings(resource))
	if portDeleteErr != nil {
		log.Error(portDeleteErr, "Failed to clean Project service endpoints")
		return false, portDeleteErr
	}
	if !portsDeleted {
		return false, nil
	}
	return true, nil
}

func handleProjectStopping(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	data.reconcileErr, data.reason, data.message = nil, "", ""
	if projectDebugWorkActive(resource) && time.Now().Before(data.stoppingAt.Add(apiv2.ProjectDebugShutdownGrace)) {
		data.reason, data.message = "DebugShutdownPending", "Waiting for debug sessions to finish graceful shutdown."
		return additionalReconciliationNeeded
	}
	cleaned, cleanupErr := reconciler.cleanupProjectBackings(ctx, resource, log)
	if cleanupErr != nil {
		data.retry("PhysicalDeletionFailed", cleanupErr)
		return additionalReconciliationNeeded
	}
	if !cleaned {
		return additionalReconciliationNeeded
	}
	data.state = projectStateStopped
	return noChange
}

func projectDebugWorkActive(resource *apiv2.Project) bool {
	if resource.Status.Debug == nil {
		return false
	}
	for _, session := range resource.Status.Debug.Sessions {
		if session.Phase == apiv2.ProjectDebugPhasePending || session.Phase == apiv2.ProjectDebugPhaseRunning {
			return true
		}
	}
	for _, execution := range resource.Status.Debug.Executions {
		if execution.Phase == apiv2.ProjectDebugPhasePending || execution.Phase == apiv2.ProjectDebugPhaseRunning ||
			(execution.PID != nil && execution.FinishedAt.IsZero()) {
			return true
		}
	}
	return false
}

func projectPhysicalName(resource *apiv2.Project) string {
	return "project-" + string(resource.UID)
}

func projectImageRef(resource *apiv2.Project) string {
	if resource.Spec.Container == nil {
		return ""
	}
	if resource.Spec.Container.ImageRef != "" {
		return commonapi.AsNamespacedName(resource.Spec.Container.ImageRef, resource.Namespace).Name
	}
	return projectPhysicalName(resource)
}

func projectContainerSpec(resource *apiv2.Project, data *projectData) apiv2.PhysicalContainerSpec {
	desired := data.containerConfig.DeepCopy()
	if desired == nil {
		desired = resource.Spec.Container.LogicalContainerConfig.DeepCopy()
	}
	var physicalPorts []apiv2.ContainerPort
	for portIndex, port := range resource.Spec.Container.Ports {
		physicalPorts = append(physicalPorts, port.ToContainerPort(data.ports[portIndex].protocol))
	}
	imageRef := projectImageRef(resource)
	if desired.Image != nil && data.imageName != "" {
		imageRef = data.imageName
	}
	labels := append(desired.Labels,
		commonapi.Label{Key: dcpBuildLabel, Value: version.ProductVersion},
		commonapi.Label{Key: groupVersionLabel, Value: apiv2.GroupVersion.String()},
		commonapi.Label{Key: nameLabel, Value: resource.Name},
	)
	return apiv2.PhysicalContainerSpec{Stop: resource.Spec.Stop, Container: &apiv2.PhysicalContainerConfig{
		ContainerName: projectPhysicalName(resource), ImageRef: imageRef,
		Entrypoint: "tail", Command: []string{"-f", "/dev/null"}, Env: desired.Env,
		Ports: physicalPorts, VolumeMounts: desired.VolumeMounts, Networks: data.networks,
		CreateFiles: desired.CreateFiles, Labels: labels,
	}}
}

func (data *projectData) retry(reason apiv2.ConditionReason, operationErr error) {
	data.reconcileErr, data.reason, data.message = operationErr, reason, operationErr.Error()
	data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
}

func handleInvalidProjectState(
	_ context.Context, _ *ProjectReconciler, _ *apiv2.Project, _ projectState, data *projectData, log logr.Logger,
) objectChange {
	stateErr := fmt.Errorf("invalid project state %v", data.state)
	log.Error(stateErr, "Project reached invalid reconciliation state")
	data.state, data.reason, data.message = projectStateInvalid, apiv2.PhysicalResourceReasonOperationStateInvalid, stateErr.Error()
	data.reconcileErr = nil
	return noChange
}
