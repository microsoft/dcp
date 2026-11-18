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
	"reflect"
	"slices"
	"strconv"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/version"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
)

const (
	projectContainerNetworkRefField = "spec.container.networks.networkRef"
	projectContainerOwnerUIDField   = ".metadata.projectContainerOwnerUID"
)

var projectContainerFinalizer = apiv2.GroupName + "/projectcontainer-reconciler"
var errMultipleProjectBackings = errors.New("multiple owned project backings")

type projectContainerDataHandlerFunc = stateInitializerFunc[
	apiv2.ProjectContainer, *apiv2.ProjectContainer, ProjectContainerReconciler, *ProjectContainerReconciler,
	projectState, projectContainerData, *projectContainerData,
]

var projectContainerDataHandlers = map[projectState]projectContainerDataHandlerFunc{
	projectStatePending:  handleProjectContainerPending,
	projectStateCreating: handleProjectContainerCreating,
	projectStateReady:    handleProjectContainerReady,
	projectStateActive:   handleProjectContainerActive,
	projectStatePaused:   handleProjectContainerReady,
	projectStateStopping: handleProjectContainerStopping,
	projectStateStopped:  handleProjectContainerStopped,
	projectStateFailed:   handleProjectContainerFailed,
	projectStateUnknown:  handleProjectContainerUnknown,
	0:                    handleInvalidProjectContainerState,
}

// ProjectContainerReconciler prepares a container context for AppHost-controlled debugging of user code.
type ProjectContainerReconciler struct {
	*ReconcilerBase[apiv2.ProjectContainer, *apiv2.ProjectContainer]
	projects             *ObjectStateMap[types.UID, projectContainerData, *projectContainerData, *apiv2.ProjectContainer]
	processExecutor      process.Executor
	containerRuntimeName string
}

func NewProjectContainerReconciler(
	ctx context.Context, client ctrl_client.Client, reader ctrl_client.Reader, log logr.Logger,
	executor process.Executor, containerRuntimeName string,
) *ProjectContainerReconciler {
	return &ProjectContainerReconciler{
		ReconcilerBase:       NewReconcilerBase[apiv2.ProjectContainer](client, reader, log, ctx),
		projects:             NewObjectStateMap[types.UID, projectContainerData, *projectContainerData, *apiv2.ProjectContainer](),
		processExecutor:      executor,
		containerRuntimeName: containerRuntimeName,
	}
}

func (r *ProjectContainerReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, &apiv2.ProjectContainer{}, projectContainerNetworkRefField,
		func(obj ctrl_client.Object) []string {
			resource := obj.(*apiv2.ProjectContainer)
			references := make([]string, 0, len(resource.Spec.Container.Networks))
			for _, network := range resource.Spec.Container.Networks {
				references = append(references, commonapi.AsNamespacedName(network.NetworkRef, resource.Namespace).Name)
			}
			return references
		})
	if indexErr != nil {
		return fmt.Errorf("index container project %s: %w", projectContainerNetworkRefField, indexErr)
	}
	for _, backing := range []ctrl_client.Object{&apiv2.PhysicalContainer{}, &apiv2.PhysicalContainerImage{}, &apiv2.ServiceEndpoint{}} {
		ownerIndexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, backing, projectContainerOwnerUIDField, func(obj ctrl_client.Object) []string {
			owner := metav1.GetControllerOf(obj)
			if owner == nil || owner.Kind != "ProjectContainer" {
				return nil
			}
			return []string{string(owner.UID)}
		})
		if ownerIndexErr != nil {
			return fmt.Errorf("index container project backing owner: %w", ownerIndexErr)
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.ProjectContainer{}).
		Owns(&apiv2.PhysicalContainer{}).Owns(&apiv2.PhysicalContainerImage{}).
		Owns(&apiv2.ServiceEndpoint{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.LogicalContainerNetwork{}, handler.EnqueueRequestsFromMapFunc(r.mapNetworkReference)).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.ProjectContainerList{}))).
		WatchesRawSource(r.GetReconciliationEventSource()).Named(name).Complete(r)
}

func (r *ProjectContainerReconciler) mapNetworkReference(ctx context.Context, obj ctrl_client.Object) []ctrl.Request {
	var resources apiv2.ProjectContainerList
	listErr := r.List(ctx, &resources, ctrl_client.InNamespace(obj.GetNamespace()),
		ctrl_client.MatchingFields{projectContainerNetworkRefField: obj.GetName()})
	if listErr != nil {
		r.Log.Error(listErr, "Failed to list referencing ProjectContainers", "Resource", ctrl_client.ObjectKeyFromObject(obj))
		return nil
	}
	requests := make([]ctrl.Request, len(resources.Items))
	for i := range resources.Items {
		requests[i] = ctrl.Request{NamespacedName: resources.Items[i].NamespacedName()}
	}
	return requests
}

func (r *ProjectContainerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)
	if ctx.Err() != nil {
		return ctrl.Result{}, nil
	}

	var resource apiv2.ProjectContainer
	getErr := reader.Get(ctx, req.NamespacedName, &resource)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			uid, _ := r.projects.BorrowByNamespacedName(req.NamespacedName)
			r.projects.DeleteByNamespacedName(req.NamespacedName)
			logger.ReleaseResourceLog((&apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{UID: uid}}).GetResourceId())
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to get ProjectContainer")
		return ctrl.Result{}, getErr
	}

	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, resource.GetResourceId())
	if !resource.DeletionTimestamp.IsZero() && !hasFinalizer(&resource, projectContainerFinalizer) {
		r.projects.DeleteByNamespacedName(req.NamespacedName)
		logger.ReleaseResourceLog(resource.GetResourceId())
		return ctrl.Result{}, nil
	}
	stateKey, data := r.projects.BorrowByNamespacedName(req.NamespacedName)
	if data != nil && stateKey != resource.UID {
		logger.ReleaseResourceLog((&apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{UID: stateKey}}).GetResourceId())
		r.projects.DeleteByNamespacedName(req.NamespacedName)
		data = nil
	}

	patch := ctrl_client.MergeFromWithOptions(resource.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	delay := StandardDelay

	if !resource.DeletionTimestamp.IsZero() {
		change, delay = r.handleDeletionRequest(ctx, &resource, data, log)
	} else {
		change = ensureFinalizer(&resource, projectContainerFinalizer, log)
		if change == noChange {
			change, delay = r.manageProject(ctx, &resource, data, log)
		}
	}

	return r.SaveChangesWithDelay(ctx, &resource, patch, change, delay, nil, log)
}

func (r *ProjectContainerReconciler) manageProject(
	ctx context.Context, resource *apiv2.ProjectContainer, data *projectContainerData, log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	if data == nil {
		if change, ready := checkProjectNamespace(ctx, r.Client, resource, log); !ready {
			return change, StandardDelay
		}
		data = &projectContainerData{projectData: projectData{state: projectStatePending}}
		r.projects.Store(resource.NamespacedName(), resource.UID, data.Clone())
	}

	stateHandler := getStateInitializer(projectContainerDataHandlers, data.state, log)
	change := stateHandler(ctx, r, resource, data.state, data, log)
	_ = r.projects.Update(resource.NamespacedName(), resource.UID, data)
	projectionChange, delay, valid := data.applyTo(resource)
	if !valid {
		log.Error(fmt.Errorf("invalid ProjectContainer state %v", data.state), "ProjectContainer reached invalid reconciliation state")
	}
	return change | projectionChange, delay
}

func projectContainerPortRequests(resource *apiv2.ProjectContainer) []logicalPortRequest {
	var requests []logicalPortRequest
	for _, port := range resource.Spec.Container.Ports {
		requests = append(requests, logicalPortRequest{binding: port.LogicalServiceBinding, address: port.Address, port: port.ContainerPort})
	}
	return requests
}

func projectContainerAdapterPort(resource *apiv2.ProjectContainer) int32 {
	if resource.Spec.DebugAdapter == nil {
		return 0
	}
	return resource.Spec.DebugAdapter.GetTCPPort()
}

func (r *ProjectContainerReconciler) prepareProjectValues(
	ctx context.Context, resource *apiv2.ProjectContainer, data *projectContainerData, log logr.Logger,
) error {
	if data.inputsPrepared || resource.Spec.Stop {
		return nil
	}
	requests := projectContainerPortRequests(resource)
	checkAdapterPort := func(ports []logicalPortData) error {
		adapterPort := projectContainerAdapterPort(resource)
		for index, selected := range ports {
			if adapterPort != 0 && selected.protocol == commonapi.PortProtocolTCP && resource.Spec.Container.Ports[index].ContainerPort == adapterPort {
				data.reason, data.message = apiv2.ProjectReasonPreparationFailed, "The adapter port conflicts with a declared TCP application port."
				conflictErr := errors.New(data.message)
				log.Error(conflictErr, "Failed to prepare ProjectContainer adapter port")
				return fmt.Errorf("%w: %w", errLogicalValuePreparationFailed, conflictErr)
			}
		}
		return nil
	}
	ports, resolvedArgs, resolvedEnv, prepareErr := prepareProjectValues(ctx, r.Client, r.NoCacheClient, resource, &data.projectData,
		requests, resource.Spec.Container.Command, resource.Spec.Container.Env, false, checkAdapterPort, log)
	if prepareErr != nil {
		return prepareErr
	}
	data.effectiveArgs, data.effectiveEnv = resolvedArgs, resolvedEnv
	data.ports, data.inputsPrepared = logicalPortSelections(ports, requests), true
	return nil
}

func handleProjectContainerPending(
	ctx context.Context, reconciler *ProjectContainerReconciler, resource *apiv2.ProjectContainer,
	_ projectState, data *projectContainerData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	data.reason, data.message = "", ""
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
		change |= applyProjectContainerImageLogs(resource, image)
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

func (r *ProjectContainerReconciler) ensureProjectPhysicalContainerImage(ctx context.Context, resource *apiv2.ProjectContainer, data *projectContainerData) (*apiv2.PhysicalContainerImage, error) {
	if resource.Spec.Container.Image == nil {
		return nil, nil
	}
	image, getErr := r.findProjectImage(ctx, resource, data)
	if getErr != nil {
		return nil, fmt.Errorf("find project image: %w", getErr)
	}
	name := types.NamespacedName{Namespace: resource.Namespace, Name: projectContainerPhysicalName(resource)}
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

func (r *ProjectContainerReconciler) createProjectPhysicalContainer(
	ctx context.Context, resource *apiv2.ProjectContainer, data *projectContainerData, spec apiv2.PhysicalContainerSpec,
) (*apiv2.PhysicalContainer, error) {
	backing := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: projectContainerPhysicalName(resource), Namespace: resource.Namespace},
		Spec:       spec,
	}
	return createLogicalBacking(ctx, r.Client, r.NoCacheClient, resource, backing, physicalContainerFinalizer,
		&data.physicalUID, &data.unobservedChildren, nil)
}

func handleProjectContainerCreating(
	ctx context.Context, reconciler *ProjectContainerReconciler, resource *apiv2.ProjectContainer,
	_ projectState, data *projectContainerData, log logr.Logger,
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
	return change | applyProjectContainerImageLogs(resource, image)
}

func handleProjectContainerReady(
	ctx context.Context, reconciler *ProjectContainerReconciler, resource *apiv2.ProjectContainer,
	_ projectState, data *projectContainerData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	physical, change, contextReady := reconciler.observePreparedProject(ctx, resource, data, log)
	if !contextReady {
		return change | additionalReconciliationNeeded
	}
	if projectDebugSessionsActive(&resource.Status.ProjectCommonStatus) {
		data.state = projectStateActive
		return change | additionalReconciliationNeeded
	}
	data.state, data.reason, data.message = projectStateReady, "", ""
	return change | reconciler.syncProjectEndpoints(ctx, resource, data, physical, log)
}

func handleProjectContainerActive(
	ctx context.Context, reconciler *ProjectContainerReconciler, resource *apiv2.ProjectContainer,
	_ projectState, data *projectContainerData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	physical, change, contextReady := reconciler.observePreparedProject(ctx, resource, data, log)
	if !contextReady {
		return change | additionalReconciliationNeeded
	}
	if !projectDebugSessionsActive(&resource.Status.ProjectCommonStatus) {
		data.state = projectStateReady
		return change | additionalReconciliationNeeded
	}
	data.message = "Project has active debug sessions."
	data.state, data.reason = projectStateActive, ""
	return change | reconciler.syncProjectEndpoints(ctx, resource, data, physical, log)
}

// observePreparedProject returns the observed container context when the prepared container is usable.
func (r *ProjectContainerReconciler) observePreparedProject(
	ctx context.Context, resource *apiv2.ProjectContainer, data *projectContainerData, log logr.Logger,
) (*apiv2.PhysicalContainer, objectChange, bool) {
	physical, change, getErr := r.observeProjectContainer(ctx, resource, data)
	if getErr != nil {
		log.Error(getErr, "Failed to observe prepared Project container")
		return nil, change, false
	}
	data.updateLifecycleFromContainer(resource, physical)
	if data.state != projectStateReady && data.state != projectStateActive {
		return nil, change, false
	}
	image, imageErr := r.findProjectImage(ctx, resource, data)
	if imageErr != nil {
		log.Error(imageErr, "Failed to observe prepared Project startup image")
		return nil, change | additionalReconciliationNeeded, false
	}
	if image == nil && data.imageUID != "" {
		data.failMissing("The owned PhysicalContainerImage is no longer available.")
		return nil, change, false
	}
	return physical, change | applyProjectContainerImageLogs(resource, image), true
}

// syncProjectEndpoints publishes or withdraws service endpoints; physical supplies container port mappings when available.
func (r *ProjectContainerReconciler) syncProjectEndpoints(
	ctx context.Context, resource *apiv2.ProjectContainer, data *projectContainerData, physical *apiv2.PhysicalContainer, log logr.Logger,
) objectChange {
	var mappings []apiv2.PhysicalContainerPortMapping
	if physical != nil {
		mappings = physical.Status.PortMappings
	}
	return syncProjectEndpoints(ctx, r.Client, r.NoCacheClient, resource, &data.projectData, data, projectContainerPortRequests(resource),
		projectContainerOwnerUIDField, mappings, log)
}

func handleProjectContainerFailed(
	ctx context.Context, reconciler *ProjectContainerReconciler, resource *apiv2.ProjectContainer,
	_ projectState, data *projectContainerData, log logr.Logger,
) objectChange {
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	change := reconciler.syncProjectEndpoints(ctx, resource, data, nil, log)
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
		change |= applyProjectContainerImageLogs(resource, image)
	}
	return change
}

func handleProjectContainerStopped(
	ctx context.Context, reconciler *ProjectContainerReconciler, resource *apiv2.ProjectContainer,
	_ projectState, data *projectContainerData, log logr.Logger,
) objectChange {
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	return reconciler.syncProjectEndpoints(ctx, resource, data, nil, log)
}

func handleProjectContainerUnknown(
	ctx context.Context, reconciler *ProjectContainerReconciler, resource *apiv2.ProjectContainer,
	_ projectState, data *projectContainerData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	physical, change, ready := reconciler.observePreparedProject(ctx, resource, data, log)
	if !ready {
		return change | additionalReconciliationNeeded
	}
	data.state, data.reason, data.message = projectStateReady, "", ""
	if projectDebugSessionsActive(&resource.Status.ProjectCommonStatus) {
		data.state = projectStateActive
	}
	return change | reconciler.syncProjectEndpoints(ctx, resource, data, physical, log) | additionalReconciliationNeeded
}

func (r *ProjectContainerReconciler) listProjectBackings(
	ctx context.Context, resource *apiv2.ProjectContainer, reader ctrl_client.Reader, list ctrl_client.ObjectList, options ...ctrl_client.ListOption,
) ([]ctrl_client.Object, error) {
	listOptions := append([]ctrl_client.ListOption{ctrl_client.InNamespace(resource.Namespace)}, options...)
	if listErr := reader.List(ctx, list, listOptions...); listErr != nil {
		return nil, fmt.Errorf("list project backings: %w", listErr)
	}
	return projectBackingItems(resource, list)
}

func projectBackingItems(resource *apiv2.ProjectContainer, list ctrl_client.ObjectList) ([]ctrl_client.Object, error) {
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

func (r *ProjectContainerReconciler) findProjectBacking(
	ctx context.Context, resource *apiv2.ProjectContainer, list ctrl_client.ObjectList,
) (ctrl_client.Object, error) {
	children, listErr := r.listProjectBackings(ctx, resource, r.Client, list,
		ctrl_client.MatchingFields{projectContainerOwnerUIDField: string(resource.UID)})
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

func (r *ProjectContainerReconciler) observeProjectContainer(
	ctx context.Context, resource *apiv2.ProjectContainer, data *projectContainerData,
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
		port := resource.Spec.DebugAdapter.GetTCPPort()
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

func (r *ProjectContainerReconciler) findProjectContainer(ctx context.Context, resource *apiv2.ProjectContainer, data *projectContainerData) (*apiv2.PhysicalContainer, error) {
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

func (r *ProjectContainerReconciler) findProjectImage(ctx context.Context, resource *apiv2.ProjectContainer, data *projectContainerData) (*apiv2.PhysicalContainerImage, error) {
	if resource.Spec.Container.Image == nil {
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

func applyProjectContainerImageLogs(resource *apiv2.ProjectContainer, image *apiv2.PhysicalContainerImage) objectChange {
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

func (r *ProjectContainerReconciler) handleDeletionRequest(
	ctx context.Context, resource *apiv2.ProjectContainer, data *projectContainerData, log logr.Logger,
) (change objectChange, delay AdditionalReconciliationDelay) {
	delay = StandardDelay
	if data == nil {
		data = &projectContainerData{projectData: projectData{state: projectStatePending}}
		data.unobservedChildren.recordCreateAttempt(&apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: projectContainerPhysicalName(resource)}})
		if resource.Spec.Container.Image != nil {
			data.unobservedChildren.recordCreateAttempt(&apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: projectContainerPhysicalName(resource)}})
		}
		for _, request := range projectContainerPortRequests(resource) {
			data.unobservedChildren.recordCreateAttempt(&apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: logicalPortResourceName(resource, request.binding.Name)}})
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
	// Endpoints are withdrawn before waiting for debug sessions so deletion stops routing immediately.
	endpointsDeleted, endpointDeleteErr := cleanupLogicalEndpoints(ctx, r.Client, r.NoCacheClient, resource, projectContainerOwnerUIDField, data.unobservedChildren)
	if endpointDeleteErr != nil {
		log.Error(endpointDeleteErr, "Failed to delete Project endpoints")
	}
	if projectDebugSessionsActive(&resource.Status.ProjectCommonStatus) {
		return additionalReconciliationNeeded, delay
	}

	observeProjectProcesses(r.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	cleaned, cleanupErr := r.cleanupProjectBackings(ctx, resource, data, log)
	if cleanupErr != nil {
		delay = LongDelay
		return additionalReconciliationNeeded, delay
	}
	if !cleaned || !endpointsDeleted || endpointDeleteErr != nil {
		return additionalReconciliationNeeded, delay
	}
	return deleteFinalizer(resource, projectContainerFinalizer, log), delay
}

func (r *ProjectContainerReconciler) cleanupProjectBackings(
	ctx context.Context, resource *apiv2.ProjectContainer, data *projectContainerData, log logr.Logger,
) (bool, error) {
	for _, group := range []struct {
		list      ctrl_client.ObjectList
		finalizer string
	}{
		{&apiv2.PhysicalContainerList{}, physicalContainerFinalizer},
		{&apiv2.PhysicalContainerImageList{}, physicalContainerImageFinalizer},
	} {
		children, listErr := r.listProjectBackings(ctx, resource, r.Client, group.list,
			ctrl_client.MatchingFields{projectContainerOwnerUIDField: string(resource.UID)})
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

func handleProjectContainerStopping(
	ctx context.Context, reconciler *ProjectContainerReconciler, resource *apiv2.ProjectContainer,
	_ projectState, data *projectContainerData, log logr.Logger,
) objectChange {
	data.reason, data.message = "", ""
	if endpointChange := reconciler.syncProjectEndpoints(ctx, resource, data, nil, log); endpointChange != noChange {
		return endpointChange
	}
	if projectDebugSessionsActive(&resource.Status.ProjectCommonStatus) {
		data.reason, data.message = apiv2.ProjectReasonStopping, "Waiting for debug sessions and adapter cleanup to finish."
		return additionalReconciliationNeeded
	}
	observeProjectProcesses(reconciler.processExecutor, &resource.Status.ProjectCommonStatus, &data.projectData, log)
	cleaned, cleanupErr := reconciler.cleanupProjectBackings(ctx, resource, data, log)
	if cleanupErr != nil {
		return additionalReconciliationNeeded
	}
	if !cleaned {
		return additionalReconciliationNeeded
	}
	data.state = projectStateStopped
	return noChange
}

func projectContainerPhysicalName(resource *apiv2.ProjectContainer) string {
	return "project-" + string(resource.UID)
}

func projectContainerImageRef(resource *apiv2.ProjectContainer) string {
	if resource.Spec.Container.ImageRef != "" {
		return commonapi.AsNamespacedName(resource.Spec.Container.ImageRef, resource.Namespace).Name
	}
	return projectContainerPhysicalName(resource)
}

func projectContainerSpec(resource *apiv2.ProjectContainer, data *projectContainerData) apiv2.PhysicalContainerSpec {
	desired := resource.Spec.Container.LogicalContainerConfig.DeepCopy()
	if data.inputsPrepared {
		desired.Command, desired.Env = slices.Clone(data.effectiveArgs), slices.Clone(data.effectiveEnv)
	}
	var physicalPorts []apiv2.ContainerPort
	for portIndex, port := range resource.Spec.Container.Ports {
		physicalPorts = append(physicalPorts, port.ToContainerPort(data.ports[portIndex].protocol))
	}
	if resource.Spec.DebugAdapter != nil {
		if port := resource.Spec.DebugAdapter.GetTCPPort(); port != 0 {
			physicalPorts = append(physicalPorts, apiv2.ContainerPort{
				ContainerPort: port, HostIP: "127.0.0.1", Protocol: commonapi.PortProtocolTCP,
			})
		}
	}
	imageRef := projectContainerImageRef(resource)
	if desired.Image != nil && data.imageName != "" {
		imageRef = data.imageName
	}
	labels := append(desired.Labels,
		commonapi.Label{Key: dcpBuildLabel, Value: version.ProductVersion},
		commonapi.Label{Key: groupVersionLabel, Value: apiv2.GroupVersion.String()},
		commonapi.Label{Key: nameLabel, Value: resource.Name},
	)
	var copyFiles []apiv2.ContainerCopyFileContext
	if resource.Spec.DebugAdapter != nil {
		copyFiles = append(copyFiles, apiv2.ContainerCopyFileContext{
			Directory:       resource.Spec.DebugAdapter.Directory,
			Destination:     resource.Spec.DebugAdapter.GetContainerDirectory(),
			ExecutableFiles: slices.Clone(resource.Spec.DebugAdapter.ExecutableFiles),
		})
	}
	return apiv2.PhysicalContainerSpec{Stop: resource.Spec.Stop, Container: &apiv2.PhysicalContainerConfig{
		ContainerName: projectContainerPhysicalName(resource), ImageRef: imageRef,
		Entrypoint: "tail", Command: []string{"-f", "/dev/null"}, RunArgs: desired.RunArgs, Env: desired.Env,
		Ports: physicalPorts, VolumeMounts: desired.VolumeMounts,
		CreateFiles: desired.CreateFiles, CopyFiles: copyFiles, Labels: labels,
	}}
}

func handleInvalidProjectContainerState(
	_ context.Context, _ *ProjectContainerReconciler, _ *apiv2.ProjectContainer, _ projectState, data *projectContainerData, log logr.Logger,
) objectChange {
	return handleInvalidProjectState(&data.projectData, log)
}
