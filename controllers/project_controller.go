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
	templating "github.com/microsoft/dcp/internal/templating"
	"github.com/microsoft/dcp/internal/version"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
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
	projectStateStopped:  handleProjectStopped,
	projectStateFailed:   handleProjectFailed,
	projectStateUnknown:  handleProjectUnknown,
	projectStateDelete:   handleInvalidProjectState,
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
	for _, backing := range []ctrl_client.Object{&apiv2.PhysicalProcess{}, &apiv2.PhysicalContainer{}, &apiv2.PhysicalContainerImage{}, &apiv2.ServiceEndpoint{}} {
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
		arguments = resource.Spec.Process.Args
		if resource.Spec.Process.AmbientEnvironment.EffectiveBehavior() == apiv2.EnvironmentBehaviorInherit {
			for _, entry := range os.Environ() {
				name, value, found := strings.Cut(entry, "=")
				if found {
					environment = append(environment, commonapi.EnvVar{Name: name, Value: value})
				}
			}
		}
		environment = append(environment, resource.Spec.Process.Env...)
	} else {
		arguments, environment = resource.Spec.Container.Command, resource.Spec.Container.Env
	}
	ports, prepareErr := prepareLogicalPorts(ctx, r.Client, r.NoCacheClient, resource, requests, log)
	if prepareErr != nil {
		log.Error(prepareErr, "Failed to prepare Project ports")
		data.reason, data.message = apiv2.LogicalResourceReasonPortPreparationPending, prepareErr.Error()
		if errors.Is(prepareErr, errLogicalServiceDependencyFailed) {
			data.reason = apiv2.LogicalResourceReasonPortPreparationFailed
			return fmt.Errorf("%w: %w", errLogicalValuePreparationFailed, prepareErr)
		} else if !apierrors.IsNotFound(prepareErr) {
			data.retry(apiv2.LogicalResourceReasonPortPreparationPending, prepareErr)
		}
		return prepareErr
	}
	if resource.Spec.Container != nil && resource.Spec.DebugAdapter != nil {
		adapterPort := resource.Spec.DebugAdapter.GetContainerTCPPort()
		for index, selected := range ports {
			if adapterPort != 0 && selected.protocol == commonapi.PortProtocolTCP && resource.Spec.Container.Ports[index].ContainerPort == adapterPort {
				data.reason, data.message = apiv2.ProjectReasonAdapterPortConflict, "The adapter port conflicts with a declared TCP application port."
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
		data.reason, data.message = apiv2.LogicalResourceReasonTemplateEvaluationFailed, evaluationErr.Error()
		if !templating.IsTransientTemplateError(evaluationErr) {
			return fmt.Errorf("%w: %w", errLogicalValuePreparationFailed, evaluationErr)
		}
		return evaluationErr
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

func handleProjectPending(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
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
		if preparationErr := reconciler.prepareProjectValues(ctx, resource, data, log); preparationErr != nil {
			if errors.Is(preparationErr, errLogicalValuePreparationFailed) {
				data.state = projectStateFailed
				return noChange
			}
			return additionalReconciliationNeeded
		}
		data.state = projectStateReady
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
			data.state, data.reason, data.message = projectStateUnknown, apiv2.ProjectReasonPhysicalExecutionChanged, "The physical backing no longer exists."
			return additionalReconciliationNeeded
		}
		if preparationErr := reconciler.prepareProjectValues(ctx, resource, data, log); preparationErr != nil {
			if errors.Is(preparationErr, errLogicalValuePreparationFailed) {
				data.state = projectStateFailed
				return noChange
			}
			return additionalReconciliationNeeded
		}
		if imageErr := reconciler.ensureProjectPhysicalContainerImage(ctx, resource, data); imageErr != nil {
			log.Error(imageErr, "Failed to prepare Project image")
			if apierrors.IsAlreadyExists(imageErr) {
				data.state, data.reason, data.message = projectStateFailed, apiv2.ProjectReasonImagePreparationFailed, imageErr.Error()
				data.reconcileErr = nil
				return noChange
			}
			return additionalReconciliationNeeded
		}
		change |= reconciler.projectImageLogs(ctx, resource, data, log)
		if !reconciler.selectProjectNetworks(ctx, resource, data, log) {
			return change
		}
		created, createErr := reconciler.createProjectPhysicalContainer(ctx, resource, data)
		if createErr != nil {
			log.Error(createErr, "Failed to prepare Project physical backing")
			if apierrors.IsAlreadyExists(createErr) {
				data.state, data.reason, data.message = projectStateFailed, apiv2.ProjectReasonPhysicalPreparationFailed, createErr.Error()
				data.reconcileErr = nil
				return change
			}
			data.retry(apiv2.ProjectReasonPhysicalPreparationFailed, createErr)
			return change | additionalReconciliationNeeded
		}
		backing = created
	} else {
		if !data.networksSelected && backing.Spec.Container != nil {
			// A committed declaration pins dependency identity even when its create response was lost.
			data.networks = slices.Clone(backing.Spec.Container.Networks)
			data.networksSelected = true
		}
	}
	if validationErr := validateProjectPhysicalContainer(resource, backing, data); validationErr != nil {
		data.state, data.reason = projectStateUnknown, apiv2.ProjectReasonPhysicalExecutionChanged
		data.message = validationErr.Error()
		log.Error(validationErr, "Project physical backing changed")
		return change
	}
	data.physicalUID, data.physicalName = backing.UID, backing.Name
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
			data.state, data.reason, data.message = projectStateFailed, apiv2.ProjectReasonNetworkFailed, resolutionErr.Error()
		} else {
			data.retry(apiv2.ProjectReasonNetworkLookupFailed, resolutionErr)
		}
		return false
	}
	if pendingMessage != "" {
		data.reason, data.message = apiv2.ProjectReasonNetworkNotReady, pendingMessage
		return false
	}
	if data.networksSelected && !reflect.DeepEqual(data.networks, selected) {
		data.state, data.reason, data.message = projectStateFailed, apiv2.ProjectReasonNetworkChanged, "A selected logical network's physical backing changed before physical realization."
		return false
	}
	data.networks, data.networksSelected = selected, true
	return true
}

func (r *ProjectReconciler) ensureProjectPhysicalContainerImage(ctx context.Context, resource *apiv2.Project, data *projectData) error {
	if resource.Spec.Container.Image == nil {
		return nil
	}
	image, getErr := r.findProjectImage(ctx, resource, data)
	if getErr != nil {
		return fmt.Errorf("find project image: %w", getErr)
	}
	name := types.NamespacedName{Namespace: resource.Namespace, Name: projectPhysicalName(resource)}
	if image == nil {
		image = &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
			Spec: apiv2.PhysicalContainerImageSpec{Image: resource.Spec.Container.Image.DeepCopy()}}
		ownerErr := ctrl.SetControllerReference(resource, image, r.Scheme())
		if ownerErr != nil {
			data.retry(apiv2.ProjectReasonImagePreparationFailed, ownerErr)
			return fmt.Errorf("set project image owner: %w", ownerErr)
		}
		InitializeLogicalBackingFinalizers(image, physicalContainerImageFinalizer)
		createErr := r.Create(ctx, image)
		if image.UID != "" {
			data.imageUID, data.imageName = image.UID, image.Name
		}
		if createErr != nil {
			if !apierrors.IsAlreadyExists(createErr) {
				data.retry(apiv2.ProjectReasonImagePreparationFailed, createErr)
				return createErr
			}
			existing := &apiv2.PhysicalContainerImage{}
			if lookupErr := r.NoCacheClient.Get(ctx, image.NamespacedName(), existing); lookupErr != nil {
				data.retry(apiv2.ProjectReasonImagePreparationFailed, lookupErr)
				return fmt.Errorf("get existing project physical image: %w", lookupErr)
			}
			if validationErr := validateProjectPhysicalContainerImage(resource, existing, data.imageUID); validationErr != nil {
				return fmt.Errorf("%v: %w", validationErr, createErr)
			}
			image = existing
		}
	}
	return data.acceptImage(resource, image)
}

func (r *ProjectReconciler) createProjectPhysicalContainer(ctx context.Context, resource *apiv2.Project, data *projectData) (*apiv2.PhysicalContainer, error) {
	backing := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: projectPhysicalName(resource), Namespace: resource.Namespace},
		Spec:       projectContainerSpec(resource, data),
	}
	if ownerErr := ctrl.SetControllerReference(resource, backing, r.Scheme()); ownerErr != nil {
		return nil, fmt.Errorf("set project container owner: %w", ownerErr)
	}
	InitializeLogicalBackingFinalizers(backing, physicalContainerFinalizer)
	createErr := r.Create(ctx, backing)
	if backing.UID != "" {
		data.physicalUID, data.physicalName = backing.UID, backing.Name
	}
	if createErr != nil {
		if !apierrors.IsAlreadyExists(createErr) {
			return nil, createErr
		}
		existing := &apiv2.PhysicalContainer{}
		if lookupErr := r.NoCacheClient.Get(ctx, backing.NamespacedName(), existing); lookupErr != nil {
			return nil, fmt.Errorf("get existing project physical container: %w", lookupErr)
		}
		if validationErr := validateProjectPhysicalContainer(resource, existing, data); validationErr != nil {
			return nil, fmt.Errorf("%v: %w", validationErr, createErr)
		}
		backing = existing
	}
	return backing, nil
}

func validateProjectPhysicalContainer(resource *apiv2.Project, physical *apiv2.PhysicalContainer, data *projectData) error {
	if identityErr := validatePhysicalResourceIdentity(resource, physical, data.physicalUID); identityErr != nil {
		return identityErr
	}
	actual, expected := physical.Spec, projectContainerSpec(resource, data)
	actual.Stop, expected.Stop = false, false
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("physical container %q has a different spec", physical.NamespacedName())
	}
	return nil
}

func validateProjectPhysicalContainerImage(resource *apiv2.Project, image *apiv2.PhysicalContainerImage, expectedUID types.UID) error {
	if identityErr := validatePhysicalResourceIdentity(resource, image, expectedUID); identityErr != nil {
		return identityErr
	}
	if !reflect.DeepEqual(image.Spec, apiv2.PhysicalContainerImageSpec{Image: resource.Spec.Container.Image}) {
		return fmt.Errorf("physical image %q has a different spec", image.NamespacedName())
	}
	return nil
}

func handleProjectCreating(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	data.reconcileErr, data.reason, data.message = nil, "", ""
	physical, change, getErr := reconciler.observeProjectContainer(ctx, resource, data)
	if getErr != nil {
		log.Error(getErr, "Failed to observe Project container preparation")
		return change | additionalReconciliationNeeded
	}
	data.state, data.reason, data.message = projectContainerBackingState(resource, physical, data.adapterAddress)
	if data.state != projectStateCreating {
		return change | additionalReconciliationNeeded
	}
	change |= reconciler.projectImageLogs(ctx, resource, data, log)
	return change | reconciler.withdrawProjectEndpoints(ctx, resource, data, log)
}

func handleProjectReady(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	data.reconcileErr, data.reason, data.message = nil, "", ""
	reconciler.observeProjectProcesses(resource, data, log)
	change, contextReady := reconciler.observePreparedProject(ctx, resource, data, log)
	if !contextReady {
		return change | additionalReconciliationNeeded
	}
	if projectDebugSessionsActive(resource) {
		data.state = projectStateActive
		return change | additionalReconciliationNeeded
	}
	return change | reconciler.withdrawProjectEndpoints(ctx, resource, data, log)
}

func handleProjectActive(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	if resource.Spec.Stop && data.requestStop() {
		return additionalReconciliationNeeded
	}
	data.reconcileErr, data.reason, data.message = nil, "", ""
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
	return change | reconciler.withdrawProjectEndpoints(ctx, resource, data, log)
}

func (r *ProjectReconciler) observePreparedProject(ctx context.Context, resource *apiv2.Project, data *projectData, log logr.Logger) (objectChange, bool) {
	ready, reason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, resource.Namespace)
	if namespaceErr != nil {
		log.Error(namespaceErr, "Failed to observe Project namespace")
		data.retry(reason, namespaceErr)
		return noChange, false
	}
	if !ready {
		data.state, data.reason, data.message = projectStatePending, reason, namespaceReadinessMessage(resource.Namespace, reason)
		return noChange, false
	}
	change := noChange
	if resource.Spec.Container != nil {
		physical, observationChange, getErr := r.observeProjectContainer(ctx, resource, data)
		change |= observationChange
		if getErr != nil {
			log.Error(getErr, "Failed to observe prepared Project container")
			return change, false
		}
		observedState, observedReason, observedMessage := projectContainerBackingState(resource, physical, data.adapterAddress)
		if observedState != projectStateReady {
			data.state, data.reason, data.message = observedState, observedReason, observedMessage
			return change, false
		}
		change |= r.projectImageLogs(ctx, resource, data, log)
		if data.state == projectStateUnknown {
			return change, false
		}
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
	data.reconcileErr = nil
	reconciler.observeProjectProcesses(resource, data, log)
	change := reconciler.withdrawProjectEndpoints(ctx, resource, data, log)
	if data.physicalUID != "" {
		lookupData := *data
		_, observationChange, getErr := reconciler.observeProjectContainer(ctx, resource, &lookupData)
		change |= observationChange
		if getErr != nil {
			log.Error(getErr, "Failed to observe failed Project container")
		}
	}
	return change | (reconciler.projectImageLogs(ctx, resource, data, log) &^ additionalReconciliationNeeded)
}

func handleProjectStopped(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	data.reconcileErr = nil
	reconciler.observeProjectProcesses(resource, data, log)
	for _, target := range data.processes {
		if !target.cleanupAttempted {
			data.state = projectStateStopping
			return additionalReconciliationNeeded
		}
	}
	return reconciler.withdrawProjectEndpoints(ctx, resource, data, log)
}

func handleProjectUnknown(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	data.reconcileErr = nil
	reconciler.observeProjectProcesses(resource, data, log)
	change := reconciler.withdrawProjectEndpoints(ctx, resource, data, log)
	if data.physicalUID != "" {
		lookupData := *data
		_, observationChange, getErr := reconciler.observeProjectContainer(ctx, resource, &lookupData)
		change |= observationChange
		if getErr != nil {
			log.Error(getErr, "Failed to observe unknown Project container")
		}
	}
	return change |
		(reconciler.projectImageLogs(ctx, resource, data, log) &^ additionalReconciliationNeeded)
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
	ctx context.Context, resource *apiv2.Project, data *projectData,
) (*apiv2.PhysicalContainer, objectChange, error) {
	physical, getErr := r.findProjectContainer(ctx, resource, data)
	if getErr != nil {
		return nil, noChange, getErr
	}
	if physical == nil {
		missingErr := fmt.Errorf("prepared Project container is missing")
		data.state, data.reason, data.message, data.reconcileErr = projectStateUnknown, apiv2.ProjectReasonContainerContextMissing, missingErr.Error(), nil
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
			data.state, data.reason, data.message, data.reconcileErr = projectStateUnknown, apiv2.ProjectReasonPhysicalExecutionChanged, getErr.Error(), nil
		} else {
			data.retry(apiv2.ProjectReasonContainerLookupFailed, getErr)
		}
		return nil, getErr
	}
	if observed == nil {
		return nil, nil
	}
	physical := observed.(*apiv2.PhysicalContainer)
	if (data.physicalUID != "" && physical.UID != data.physicalUID) || !hasFinalizer(physical, LogicalBackingFinalizer) {
		identityErr := fmt.Errorf("physical container %q UID or logical hold changed", physical.NamespacedName())
		data.state, data.reason, data.message, data.reconcileErr = projectStateUnknown, apiv2.ProjectReasonPhysicalExecutionChanged, identityErr.Error(), nil
		return nil, identityErr
	}
	data.physicalUID, data.physicalName = physical.UID, physical.Name
	return physical, nil
}

func (data *projectData) acceptImage(resource *apiv2.Project, image *apiv2.PhysicalContainerImage) error {
	if validationErr := validateProjectPhysicalContainerImage(resource, image, data.imageUID); validationErr != nil {
		identityErr := fmt.Errorf("%w: %w", errProjectImageChanged, validationErr)
		data.state, data.reason, data.message, data.reconcileErr = projectStateUnknown, apiv2.ProjectReasonImagePreparationFailed, identityErr.Error(), nil
		return identityErr
	}
	data.imageUID, data.imageName = image.UID, image.Name
	return nil
}

func (r *ProjectReconciler) findProjectImage(ctx context.Context, resource *apiv2.Project, data *projectData) (*apiv2.PhysicalContainerImage, error) {
	observed, getErr := r.findProjectBacking(ctx, resource, &apiv2.PhysicalContainerImageList{})
	if getErr != nil {
		if errors.Is(getErr, errMultipleProjectBackings) {
			data.state, data.reason, data.message, data.reconcileErr = projectStateUnknown, apiv2.ProjectReasonImagePreparationFailed, getErr.Error(), nil
		} else {
			data.retry(apiv2.ProjectReasonImagePreparationFailed, getErr)
		}
		return nil, getErr
	}
	if observed == nil {
		if data.imageUID != "" {
			missingErr := fmt.Errorf("%w: backing image %q is missing", errProjectImageChanged, data.imageName)
			data.state, data.reason, data.message, data.reconcileErr = projectStateUnknown, apiv2.ProjectReasonImagePreparationFailed, missingErr.Error(), nil
			return nil, missingErr
		}
		return nil, nil
	}
	image := observed.(*apiv2.PhysicalContainerImage)
	if identityErr := data.acceptImage(resource, image); identityErr != nil {
		return nil, identityErr
	}
	return image, nil
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
	if data.state != projectStateDelete {
		for handle, target := range data.processes {
			if target.observation.Cleanup == apiv2.ProjectDebugProcessCleanupUnconfirmed {
				target.cleanupAttempted = false
				data.processes[handle] = target
			}
		}
	}
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
			data.reason, data.message = apiv2.ProjectReasonDebugShutdownPending, "Waiting for debug sessions to finish graceful shutdown."
			return additionalReconciliationNeeded, delay
		}
		if active {
			data.reason, data.message = apiv2.ProjectReasonDebugShutdownGraceExpired, "Debug shutdown grace elapsed; continuing physical backing cleanup."
			log.Info("Debug shutdown grace elapsed; continuing Project backing cleanup")
		}
	}

	if !r.settleProjectProcesses(resource, data, log) {
		return additionalReconciliationNeeded, delay
	}
	cleaned, cleanupErr := r.cleanupProjectBackings(ctx, resource, data, log)
	if cleanupErr != nil {
		data.retry(apiv2.ProjectReasonPhysicalDeletionFailed, cleanupErr)
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
	portsDeleted, portDeleteErr := cleanupLogicalEndpoints(ctx, r.Client, r.NoCacheClient, resource, projectOwnerUIDField, data.unobservedChildren)
	if portDeleteErr != nil {
		log.Error(portDeleteErr, "Failed to clean Project service endpoints")
	}
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
	return portsDeleted && portDeleteErr == nil, nil
}

func handleProjectStopping(
	ctx context.Context, reconciler *ProjectReconciler, resource *apiv2.Project,
	_ projectState, data *projectData, log logr.Logger,
) objectChange {
	data.reconcileErr, data.reason, data.message = nil, "", ""
	if projectDebugWorkActive(resource) && time.Now().Before(data.stoppingAt.Add(apiv2.ProjectDebugShutdownGrace)) {
		data.reason, data.message = apiv2.ProjectReasonDebugShutdownPending, "Waiting for debug sessions to finish graceful shutdown."
		return additionalReconciliationNeeded
	}
	if !reconciler.settleProjectProcesses(resource, data, log) {
		return additionalReconciliationNeeded
	}
	cleaned, cleanupErr := reconciler.cleanupProjectBackings(ctx, resource, data, log)
	if cleanupErr != nil {
		data.retry(apiv2.ProjectReasonPhysicalDeletionFailed, cleanupErr)
		return additionalReconciliationNeeded
	}
	if !cleaned {
		return additionalReconciliationNeeded
	}
	data.state = projectStateStopped
	logUnconfirmedProjectTargets(resource, log)
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
		Entrypoint: "tail", Command: []string{"-f", "/dev/null"}, Env: desired.Env,
		Ports: physicalPorts, VolumeMounts: desired.VolumeMounts, Networks: data.networks,
		CreateFiles: desired.CreateFiles, Labels: labels,
	}}
}

func handleInvalidProjectState(
	_ context.Context, _ *ProjectReconciler, _ *apiv2.Project, _ projectState, data *projectData, log logr.Logger,
) objectChange {
	stateErr := fmt.Errorf("invalid project state %v", data.state)
	log.Error(stateErr, "Project reached invalid reconciliation state")
	data.state, data.reason, data.message = projectStateUnknown, apiv2.PhysicalResourceReasonOperationStateInvalid, stateErr.Error()
	data.reconcileErr = nil
	return noChange
}
