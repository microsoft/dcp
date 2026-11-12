/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"

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
	templating "github.com/microsoft/dcp/internal/templating"
	"github.com/microsoft/dcp/internal/version"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

const (
	projectNetworkRefField = "spec.container.networks.networkRef"
	projectOwnerUIDField   = ".metadata.projectOwnerUID"
)

var projectFinalizer = apiv2.GroupName + "/project-reconciler"
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
	projectStatePaused:   handleProjectReady,
	projectStateStopping: handleProjectStopping,
	projectStateStopped:  handleProjectStopped,
	projectStateFailed:   handleProjectFailed,
	projectStateUnknown:  handleProjectUnknown,
	0:                    handleInvalidProjectState,
}

// ProjectReconciler prepares launch inputs and reusable context for AppHost-controlled debugging.
type ProjectReconciler struct {
	*ReconcilerBase[apiv2.Project, *apiv2.Project]
	projects             *ObjectStateMap[types.UID, projectData, *projectData, *apiv2.Project]
	processExecutor      process.Executor
	processCleanupQueue  *resiliency.WorkQueue
	containerRuntimeName string
}

func NewProjectReconciler(ctx context.Context, client ctrl_client.Client, reader ctrl_client.Reader, log logr.Logger, executor process.Executor, containerRuntimeName string) *ProjectReconciler {
	return &ProjectReconciler{
		ReconcilerBase:       NewReconcilerBase[apiv2.Project](client, reader, log, ctx),
		projects:             NewObjectStateMap[types.UID, projectData, *projectData, *apiv2.Project](),
		processExecutor:      executor,
		processCleanupQueue:  resiliency.NewWorkQueue(ctx, MaxConcurrentReconciles),
		containerRuntimeName: containerRuntimeName,
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
	for _, backing := range []ctrl_client.Object{&apiv2.PhysicalContainer{}, &apiv2.PhysicalContainerImage{}} {
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
		Owns(&apiv2.PhysicalContainer{}).Owns(&apiv2.PhysicalContainerImage{}).
		Watches(&apiv2.LogicalContainerNetwork{}, handler.EnqueueRequestsFromMapFunc(r.mapProjectReference(projectNetworkRefField))).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.ProjectList{}))).
		WatchesRawSource(r.GetReconciliationEventSource()).Named(name).Complete(r)
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
	if !resource.DeletionTimestamp.IsZero() && !hasFinalizer(&resource, projectFinalizer) {
		r.projects.DeleteByNamespacedName(req.NamespacedName)
		logger.ReleaseResourceLog(resource.GetResourceId())
		return ctrl.Result{}, nil
	}
	r.projects.RunDeferredOps(req.NamespacedName, &resource)
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
				message = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
				change |= additionalReconciliationNeeded
			}
			change |= setValue(&resource.Status.Phase, phase)
			change |= setCondition(&resource.Status.Conditions, apiv2.ConditionReady, resource.Generation, metav1.ConditionFalse, apiv2.ProjectReasonPreparing, message)
			return change, StandardDelay
		}
		data = &projectData{state: projectStatePending}
		r.projects.Store(resource.NamespacedName(), resource.UID, data.Clone())
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

func projectPortRequests(resource *apiv2.Project) []logicalPortRequest {
	var requests []logicalPortRequest
	if resource.Spec.Process != nil {
		for _, port := range resource.Spec.Process.Ports {
			requests = append(requests, logicalPortRequest{binding: port.LogicalServiceBinding, address: port.Address, port: port.Port})
		}
	} else {
		for _, port := range resource.Spec.Container.Ports {
			requests = append(requests, logicalPortRequest{binding: port.LogicalServiceBinding, address: port.Address, port: port.ContainerPort})
		}
	}
	return requests
}

func (r *ProjectReconciler) prepareProjectValues(
	ctx context.Context, resource *apiv2.Project, data *projectData, log logr.Logger,
) error {
	if data.inputsPrepared || resource.Spec.Stop {
		return nil
	}
	requests := projectPortRequests(resource)
	var arguments []string
	var environment []commonapi.EnvVar
	if resource.Spec.Process != nil {
		arguments, environment = resource.Spec.Process.Args, resource.Spec.Process.Env
	} else {
		arguments, environment = resource.Spec.Container.Command, resource.Spec.Container.Env
	}
	ports, prepareErr := prepareLogicalPorts(ctx, r.Client, r.NoCacheClient, resource, requests, log)
	if prepareErr != nil {
		log.Error(prepareErr, "Failed to prepare Project ports")
		data.reason, data.message = apiv2.ProjectReasonPreparing, prepareErr.Error()
		if errors.Is(prepareErr, errLogicalServiceDependencyFailed) {
			data.reason = apiv2.ProjectReasonPreparationFailed
			return fmt.Errorf("%w: %w", errLogicalValuePreparationFailed, prepareErr)
		}
		return prepareErr
	}
	if resource.Spec.Container != nil && resource.Spec.DebugAdapter != nil {
		adapterPort := resource.Spec.DebugAdapter.GetContainerTCPPort()
		for index, selected := range ports {
			if adapterPort != 0 && selected.protocol == commonapi.PortProtocolTCP && resource.Spec.Container.Ports[index].ContainerPort == adapterPort {
				data.reason, data.message = apiv2.ProjectReasonPreparationFailed, "The adapter port conflicts with a declared TCP application port."
				conflictErr := errors.New(data.message)
				log.Error(conflictErr, "Failed to prepare Project adapter port")
				return fmt.Errorf("%w: %w", errLogicalValuePreparationFailed, conflictErr)
			}
		}
	}
	resolvedArgs, resolvedEnv, evaluationErr := templating.EvaluateV2Values(ctx, r.NoCacheClient, resource,
		servingPorts(ports), arguments, environment, resource.Spec.Process != nil && osutil.IsWindows())
	if evaluationErr != nil {
		log.Error(evaluationErr, "Failed to evaluate Project launch values")
		data.reason, data.message = apiv2.ProjectReasonPreparationFailed, evaluationErr.Error()
		if !templating.IsTransientTemplateError(evaluationErr) {
			return fmt.Errorf("%w: %w", errLogicalValuePreparationFailed, evaluationErr)
		}
		data.reason = apiv2.ProjectReasonPreparing
		return evaluationErr
	}
	if resource.Spec.Process != nil && resource.Spec.Process.AmbientEnvironment.EffectiveBehavior() == apiv2.EnvironmentBehaviorInherit {
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

// mergeProjectAmbientEnvironment overlays evaluated declarations onto literal host environment values.
func mergeProjectAmbientEnvironment(declared []commonapi.EnvVar) []commonapi.EnvVar {
	mode := maps.StringMapModeCaseSensitive
	if osutil.IsWindows() {
		mode = maps.StringMapModeCaseInsensitive
	}
	merged := maps.NewStringKeyMap[string](mode)
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if found {
			merged.Override(name, value)
		}
	}
	for _, variable := range declared {
		merged.Override(variable.Name, variable.Value)
	}
	return maps.MapToSlice[commonapi.EnvVar](merged.Data(), func(name, value string) commonapi.EnvVar {
		return commonapi.EnvVar{Name: name, Value: value}
	})
}

func handleProjectPending(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	data.reason, data.message = "", ""
	if resource.Spec.Process != nil {
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
	backing, getErr := reconciler.findProjectContainer(ctx, resource, data)
	if getErr != nil {
		log.Error(getErr, "Failed to resolve Project physical creation outcome")
		return additionalReconciliationNeeded
	}
	change := noChange
	if backing == nil {
		if data.physicalUID != "" {
			data.message = "Waiting to observe the owned PhysicalContainer."
			return additionalReconciliationNeeded
		}
		if preparationErr := reconciler.prepareProjectValues(ctx, resource, data, log); preparationErr != nil {
			if errors.Is(preparationErr, errLogicalValuePreparationFailed) {
				data.state = projectStateFailed
				return noChange
			}
			return additionalReconciliationNeeded
		}
		image, imageErr := reconciler.ensureProjectPhysicalContainerImage(ctx, resource, data)
		if imageErr != nil {
			log.Error(imageErr, "Failed to prepare Project image")
			if apierrors.IsAlreadyExists(imageErr) {
				data.state, data.reason, data.message = projectStateFailed, apiv2.ProjectReasonPreparationFailed, imageErr.Error()
				return noChange
			}
			return additionalReconciliationNeeded
		}
		change |= applyProjectImageLogs(resource, image)
		if resource.Spec.Container.Image != nil && image == nil {
			data.message = "Waiting to observe the owned PhysicalContainerImage."
			return change | additionalReconciliationNeeded
		}
		networksReady, networks, networkMessage, networkErr := resolveLogicalContainerNetworks(ctx, reconciler.Client, resource.Namespace, resource.Spec.Container.Networks)
		if networkErr != nil {
			log.Error(networkErr, "Failed to resolve Project networks")
			data.reason, data.message = apiv2.ProjectReasonPreparing, networkErr.Error()
			if errors.Is(networkErr, errLogicalNetworkFailed) {
				data.state, data.reason = projectStateFailed, apiv2.ProjectReasonPreparationFailed
				return change
			}
			return change | additionalReconciliationNeeded
		}
		if !networksReady {
			data.reason, data.message = apiv2.ProjectReasonPreparing, networkMessage
			return change
		}
		spec := projectContainerSpec(resource, data)
		spec.Container.Networks = networks
		created, createErr := reconciler.createProjectPhysicalContainer(ctx, resource, data, spec)
		if createErr != nil {
			log.Error(createErr, "Failed to prepare Project physical backing")
			if apierrors.IsAlreadyExists(createErr) {
				data.state, data.reason, data.message = projectStateFailed, apiv2.ProjectReasonPreparationFailed, createErr.Error()
				return change
			}
			return change | additionalReconciliationNeeded
		}
		backing = created
	}
	data.physicalUID, data.physicalName = backing.UID, backing.Name
	data.state = projectStateCreating
	return change | additionalReconciliationNeeded
}

func (r *ProjectReconciler) ensureProjectPhysicalContainerImage(ctx context.Context, resource *apiv2.Project, data *projectData) (*apiv2.PhysicalContainerImage, error) {
	if resource.Spec.Container.Image == nil {
		return nil, nil
	}
	image, getErr := r.findProjectImage(ctx, resource, data)
	if getErr != nil {
		return nil, fmt.Errorf("find project image: %w", getErr)
	}
	name := types.NamespacedName{Namespace: resource.Namespace, Name: projectPhysicalName(resource)}
	if image == nil {
		if data.imageUID != "" {
			return nil, nil
		}
		image = &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
			Spec: apiv2.PhysicalContainerImageSpec{Image: resource.Spec.Container.Image.DeepCopy()}}
		declared, createErr := createLogicalBacking(ctx, r.Client, r.NoCacheClient, resource, image, physicalContainerImageFinalizer,
			&data.imageUID, &data.unobservedChildren, func(existing *apiv2.PhysicalContainerImage) error {
				if !reflect.DeepEqual(existing.Spec, image.Spec) {
					return fmt.Errorf("physical image %q has a different spec", existing.NamespacedName())
				}
				return nil
			})
		if createErr != nil {
			return nil, createErr
		}
		image = declared
	}
	data.imageUID, data.imageName = image.UID, image.Name
	return image, nil
}

func (r *ProjectReconciler) createProjectPhysicalContainer(
	ctx context.Context, resource *apiv2.Project, data *projectData, spec apiv2.PhysicalContainerSpec,
) (*apiv2.PhysicalContainer, error) {
	backing := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: projectPhysicalName(resource), Namespace: resource.Namespace},
		Spec:       spec,
	}
	return createLogicalBacking(ctx, r.Client, r.NoCacheClient, resource, backing, physicalContainerFinalizer,
		&data.physicalUID, &data.unobservedChildren, nil)
}

func handleProjectCreating(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	physical, change, getErr := reconciler.observeProjectContainer(ctx, resource, data)
	if getErr != nil {
		log.Error(getErr, "Failed to observe Project container preparation")
		return change | additionalReconciliationNeeded
	}
	data.updateLifecycleFromContainer(resource, physical)
	if data.state != projectStateCreating {
		return change | additionalReconciliationNeeded
	}
	image, imageErr := reconciler.findProjectImage(ctx, resource, data)
	if imageErr != nil {
		log.Error(imageErr, "Failed to observe Project startup image")
		return change | additionalReconciliationNeeded
	}
	return change | applyProjectImageLogs(resource, image)
}

func handleProjectReady(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	reconciler.observeProjectProcesses(resource, data, log)
	change, contextReady := reconciler.observePreparedProject(ctx, resource, data, log)
	if !contextReady {
		return change | additionalReconciliationNeeded
	}
	if projectDebugSessionsActive(resource) {
		data.state = projectStateActive
		return change | additionalReconciliationNeeded
	}
	data.state, data.reason, data.message = projectStateReady, "", ""
	return change
}

func handleProjectActive(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	reconciler.observeProjectProcesses(resource, data, log)
	change, contextReady := reconciler.observePreparedProject(ctx, resource, data, log)
	if !contextReady {
		return change | additionalReconciliationNeeded
	}
	if !projectDebugSessionsActive(resource) {
		data.state = projectStateReady
		return change | additionalReconciliationNeeded
	}
	data.message = "Project has active debug sessions."
	data.state, data.reason = projectStateActive, ""
	return change
}

func (r *ProjectReconciler) observePreparedProject(ctx context.Context, resource *apiv2.Project, data *projectData, log logr.Logger) (objectChange, bool) {
	change := noChange
	if resource.Spec.Container != nil {
		physical, observationChange, getErr := r.observeProjectContainer(ctx, resource, data)
		change |= observationChange
		if getErr != nil {
			log.Error(getErr, "Failed to observe prepared Project container")
			return change, false
		}
		data.updateLifecycleFromContainer(resource, physical)
		if data.state != projectStateReady && data.state != projectStateActive {
			return change, false
		}
		image, imageErr := r.findProjectImage(ctx, resource, data)
		if imageErr != nil {
			log.Error(imageErr, "Failed to observe prepared Project startup image")
			return change | additionalReconciliationNeeded, false
		}
		if image == nil && data.imageUID != "" {
			data.failMissing("The owned PhysicalContainerImage is no longer available.")
			return change, false
		}
		change |= applyProjectImageLogs(resource, image)
	}
	return change, true
}

func projectDebugSessionsActive(resource *apiv2.Project) bool {
	if resource.Status.Debug != nil {
		for _, session := range resource.Status.Debug.Sessions {
			if session.Phase == apiv2.ProjectDebugPhasePending || session.Phase == apiv2.ProjectDebugPhaseRunning {
				return true
			}
		}
	}
	return false
}

func handleProjectFailed(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	reconciler.observeProjectProcesses(resource, data, log)
	change := noChange
	if data.physicalUID != "" {
		lookupData := *data
		_, observationChange, getErr := reconciler.observeProjectContainer(ctx, resource, &lookupData)
		change |= observationChange
		if getErr != nil {
			log.Error(getErr, "Failed to observe failed Project container")
		}
	}
	imageLookupData := *data
	image, imageErr := reconciler.findProjectImage(ctx, resource, &imageLookupData)
	if imageErr != nil {
		log.Error(imageErr, "Failed to observe failed Project startup image")
	} else {
		change |= applyProjectImageLogs(resource, image)
	}
	return change
}

func handleProjectStopped(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	if !reconciler.settleProjectProcesses(resource, data, log) {
		return additionalReconciliationNeeded
	}
	return noChange
}

func handleProjectUnknown(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	reconciler.observeProjectProcesses(resource, data, log)
	change, ready := reconciler.observePreparedProject(ctx, resource, data, log)
	if !ready {
		return change | additionalReconciliationNeeded
	}
	data.state, data.reason, data.message = projectStateReady, "", ""
	if projectDebugSessionsActive(resource) {
		data.state = projectStateActive
	}
	return change | additionalReconciliationNeeded
}

func (r *ProjectReconciler) listProjectBackings(
	ctx context.Context, resource *apiv2.Project, reader ctrl_client.Reader, list ctrl_client.ObjectList, options ...ctrl_client.ListOption,
) ([]ctrl_client.Object, error) {
	listOptions := append([]ctrl_client.ListOption{ctrl_client.InNamespace(resource.Namespace)}, options...)
	if listErr := reader.List(ctx, list, listOptions...); listErr != nil {
		return nil, fmt.Errorf("list project backings: %w", listErr)
	}
	return projectBackingItems(resource, list)
}

func projectBackingItems(resource *apiv2.Project, list ctrl_client.ObjectList) ([]ctrl_client.Object, error) {
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
	ctx context.Context, resource *apiv2.Project, data *projectData,
) (*apiv2.PhysicalContainer, objectChange, error) {
	physical, getErr := r.findProjectContainer(ctx, resource, data)
	if getErr != nil {
		return nil, noChange, getErr
	}
	if physical == nil {
		if data.state == projectStateCreating {
			data.message = "Waiting to observe the owned PhysicalContainer."
			return nil, noChange, fmt.Errorf("owned PhysicalContainer has not reached the cache")
		}
		missingErr := fmt.Errorf("prepared Project container is missing")
		data.failMissing(missingErr.Error())
		source := &apiv2.LogSource{Name: data.physicalName, UID: data.physicalUID, Complete: true}
		if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
			resource.Status.RuntimeLogs = source
			return nil, statusChanged, missingErr
		}
		return nil, noChange, missingErr
	}
	change := setValue(&resource.Status.ContainerID, physical.Status.ContainerID)
	change |= setValue(&resource.Status.ContainerRuntime, r.containerRuntimeName)
	change |= setValue(&resource.Status.ContainerName, physical.Status.ContainerName)
	change |= setValue(&resource.Status.Image, physical.Status.Image)
	if !slices.Equal(resource.Status.EffectiveEnv, physical.Status.EffectiveEnv) {
		resource.Status.EffectiveEnv = slices.Clone(physical.Status.EffectiveEnv)
		change |= statusChanged
	}
	change |= setTimestamp(&resource.Status.CreatedAt, physical.Status.CreatedAt)
	change |= setTimestamp(&resource.Status.StartedAt, physical.Status.StartedAt)
	if !reflect.DeepEqual(resource.Status.PortMappings, physical.Status.PortMappings) {
		resource.Status.PortMappings = slices.Clone(physical.Status.PortMappings)
		change |= statusChanged
	}
	data.adapterAddress = ""
	if resource.Spec.DebugAdapter != nil {
		port := resource.Spec.DebugAdapter.GetContainerTCPPort()
		for _, mapping := range physical.Status.PortMappings {
			if port != 0 && mapping.ContainerPort == port && mapping.Protocol == commonapi.PortProtocolTCP &&
				mapping.HostIP == "127.0.0.1" && mapping.HostPort > 0 {
				data.adapterAddress = net.JoinHostPort(mapping.HostIP, strconv.Itoa(int(mapping.HostPort)))
				break
			}
		}
	}
	source := physical.GetLogSource()
	if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
		resource.Status.RuntimeLogs = source
		change |= statusChanged
	}
	return physical, change, nil
}

func (r *ProjectReconciler) findProjectContainer(ctx context.Context, resource *apiv2.Project, data *projectData) (*apiv2.PhysicalContainer, error) {
	observed, getErr := r.findProjectBacking(ctx, resource, &apiv2.PhysicalContainerList{})
	if getErr != nil {
		if errors.Is(getErr, errMultipleProjectBackings) {
			data.failMissing(getErr.Error())
		}
		return nil, getErr
	}
	if observed == nil {
		return nil, nil
	}
	physical := observed.(*apiv2.PhysicalContainer)
	data.unobservedChildren.observeCached(physical)
	if (data.physicalUID != "" && physical.UID != data.physicalUID) || !hasFinalizer(physical, LogicalBackingFinalizer) {
		identityErr := fmt.Errorf("physical container %q UID or logical hold changed", physical.NamespacedName())
		data.failMissing(identityErr.Error())
		return nil, identityErr
	}
	data.physicalUID, data.physicalName = physical.UID, physical.Name
	return physical, nil
}

func (r *ProjectReconciler) findProjectImage(ctx context.Context, resource *apiv2.Project, data *projectData) (*apiv2.PhysicalContainerImage, error) {
	if resource.Spec.Container == nil || resource.Spec.Container.Image == nil {
		return nil, nil
	}
	observed, getErr := r.findProjectBacking(ctx, resource, &apiv2.PhysicalContainerImageList{})
	if getErr != nil {
		if errors.Is(getErr, errMultipleProjectBackings) {
			data.failMissing(getErr.Error())
		}
		return nil, getErr
	}
	if observed == nil {
		return nil, nil
	}
	image := observed.(*apiv2.PhysicalContainerImage)
	data.unobservedChildren.observeCached(image)
	if (data.imageUID != "" && image.UID != data.imageUID) || !hasFinalizer(image, LogicalBackingFinalizer) {
		identityErr := fmt.Errorf("physical image %q UID or logical hold changed", image.NamespacedName())
		data.failMissing(identityErr.Error())
		return nil, identityErr
	}
	data.imageUID, data.imageName = image.UID, image.Name
	if !image.DeletionTimestamp.IsZero() && data.state != projectStateFailed && data.state != projectStateStopped {
		deletionErr := fmt.Errorf("the owned PhysicalContainerImage %q is being deleted", image.NamespacedName())
		data.failMissing(deletionErr.Error())
		return nil, deletionErr
	}
	return image, nil
}

func applyProjectImageLogs(resource *apiv2.Project, image *apiv2.PhysicalContainerImage) objectChange {
	if image == nil {
		return noChange
	}
	source := image.GetLogSource()
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
		data = &projectData{state: projectStatePending}
		if resource.Spec.Container != nil {
			data.unobservedChildren.recordCreateAttempt(&apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: projectPhysicalName(resource)}})
			if resource.Spec.Container.Image != nil {
				data.unobservedChildren.recordCreateAttempt(&apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: projectPhysicalName(resource)}})
			}
		}
		r.projects.Store(resource.NamespacedName(), resource.UID, data.Clone())
	}
	defer func() {
		if data != nil {
			_ = r.projects.Update(resource.NamespacedName(), resource.UID, data)
			projectionChange, _, valid := data.applyTo(resource)
			change |= projectionChange
			if !valid {
				log.Error(fmt.Errorf("invalid Project state %v", data.state), "Project reached invalid reconciliation state")
			}
		}
	}()
	if projectDebugSessionsActive(resource) {
		return additionalReconciliationNeeded, delay
	}

	if !r.settleProjectProcesses(resource, data, log) {
		return additionalReconciliationNeeded, delay
	}
	cleaned, cleanupErr := r.cleanupProjectBackings(ctx, resource, data, log)
	if cleanupErr != nil {
		delay = LongDelay
		return additionalReconciliationNeeded, delay
	}
	if !cleaned {
		return additionalReconciliationNeeded, delay
	}
	logUnconfirmedProjectTargets(resource, log)
	return deleteFinalizer(resource, projectFinalizer, log), delay
}

func (r *ProjectReconciler) cleanupProjectBackings(
	ctx context.Context, resource *apiv2.Project, data *projectData, log logr.Logger,
) (bool, error) {
	for _, group := range []struct {
		list      ctrl_client.ObjectList
		finalizer string
	}{
		{&apiv2.PhysicalContainerList{}, physicalContainerFinalizer},
		{&apiv2.PhysicalContainerImageList{}, physicalContainerImageFinalizer},
	} {
		children, listErr := r.listProjectBackings(ctx, resource, r.Client, group.list,
			ctrl_client.MatchingFields{projectOwnerUIDField: string(resource.UID)})
		if listErr == nil {
			switch list := group.list.(type) {
			case *apiv2.PhysicalContainerList:
				listErr = resolveUnobservedLogicalChildren(ctx, r.NoCacheClient, resource, data.unobservedChildren, &list.Items)
			case *apiv2.PhysicalContainerImageList:
				listErr = resolveUnobservedLogicalChildren(ctx, r.NoCacheClient, resource, data.unobservedChildren, &list.Items)
			}
			if listErr == nil {
				children, listErr = projectBackingItems(resource, group.list)
			}
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
	return true, nil
}

func handleProjectStopping(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	data.reason, data.message = "", ""
	if projectDebugSessionsActive(resource) {
		data.reason, data.message = apiv2.ProjectReasonStopping, "Waiting for debug sessions and adapter cleanup to finish."
		return additionalReconciliationNeeded
	}
	if !reconciler.settleProjectProcesses(resource, data, log) {
		return additionalReconciliationNeeded
	}
	cleaned, cleanupErr := reconciler.cleanupProjectBackings(ctx, resource, data, log)
	if cleanupErr != nil {
		return additionalReconciliationNeeded
	}
	if !cleaned {
		return additionalReconciliationNeeded
	}
	data.state = projectStateStopped
	logUnconfirmedProjectTargets(resource, log)
	return noChange
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
	desired := resource.Spec.Container.LogicalContainerConfig.DeepCopy()
	if data.inputsPrepared {
		desired.Command, desired.Env = slices.Clone(data.effectiveArgs), slices.Clone(data.effectiveEnv)
	}
	var physicalPorts []apiv2.ContainerPort
	for portIndex, port := range resource.Spec.Container.Ports {
		physicalPorts = append(physicalPorts, port.ToContainerPort(data.ports[portIndex].protocol))
	}
	if resource.Spec.DebugAdapter != nil {
		if port := resource.Spec.DebugAdapter.GetContainerTCPPort(); port != 0 {
			physicalPorts = append(physicalPorts, apiv2.ContainerPort{
				ContainerPort: port, HostIP: "127.0.0.1", Protocol: commonapi.PortProtocolTCP,
			})
		}
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
		Entrypoint: "tail", Command: []string{"-f", "/dev/null"}, RunArgs: desired.RunArgs, Env: desired.Env,
		Ports: physicalPorts, VolumeMounts: desired.VolumeMounts,
		CreateFiles: desired.CreateFiles, Labels: labels,
	}}
}

func handleInvalidProjectState(
	_ context.Context, _ *ProjectReconciler, _ *apiv2.Project, _ projectState, data *projectData, log logr.Logger,
) objectChange {
	stateErr := fmt.Errorf("invalid project state %v", data.state)
	log.Error(stateErr, "Project reached invalid reconciliation state")
	data.state, data.reason, data.message = projectStateFailed, apiv2.PhysicalResourceReasonOperationStateInvalid, stateErr.Error()
	return noChange
}
