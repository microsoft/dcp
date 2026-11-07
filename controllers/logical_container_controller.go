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
	std_slices "slices"
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
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcpclient"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/internal/templating"
	"github.com/microsoft/dcp/internal/version"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
)

const (
	logicalContainerImageRefField   = ".spec.container.imageRef"
	logicalContainerVolumeRefField  = ".spec.container.volumeMounts.volumeRef"
	logicalContainerNetworkRefField = ".spec.container.networks.networkRef"
	logicalContainerOwnerUIDField   = ".metadata.logicalContainerOwnerUID"
)

var (
	errMultipleLogicalContainerBackings = errors.New("multiple owned physical resources")
	errLogicalNetworkFailed             = errors.New("logical network failed")
	logicalContainerFinalizer           = fmt.Sprintf("%s/logicalcontainer-reconciler", apiv2.GroupVersion.Group)
	logicalContainerDataHandlers        = map[logicalContainerState]logicalContainerDataHandlerFunc{
		logicalContainerStatePending:           handleLogicalContainerPending,
		logicalContainerStatePreparationFailed: handleLogicalContainerTerminal,
		logicalContainerStateCreating:          handleLogicalContainerCreating,
		logicalContainerStateRunning:           handleLogicalContainerRunning,
		logicalContainerStatePaused:            handleLogicalContainerRunning,
		logicalContainerStateExited:            handleLogicalContainerTerminal,
		logicalContainerStateFailed:            handleLogicalContainerTerminal,
		logicalContainerStateUnknown:           handleLogicalContainerUnknown,
		logicalContainerStateStopping:          handleLogicalContainerStopping,
		logicalContainerStateStopped:           handleLogicalContainerTerminal,
		logicalContainerStateStopFailed:        handleLogicalContainerTerminal,
		0:                                      handleLogicalContainerUnknown,
	}
)

type logicalContainerDataHandlerFunc = stateInitializerFunc[
	apiv2.LogicalContainer, *apiv2.LogicalContainer,
	LogicalContainerReconciler, *LogicalContainerReconciler,
	logicalContainerState,
	logicalContainerData, *logicalContainerData,
]

type LogicalContainerReconcilerConfig struct {
	StateStore         *statestore.Store
	ResourceLeaseOwner process.ProcessHandle
	WorkloadID         commonapi.WorkloadID
}

type LogicalContainerReconciler struct {
	*ReconcilerBase[apiv2.LogicalContainer, *apiv2.LogicalContainer]

	containerInspector containers.InspectContainers
	runtimeName        string
	config             LogicalContainerReconcilerConfig
	containerData      *ObjectStateMap[types.UID, logicalContainerData, *logicalContainerData, *apiv2.LogicalContainer]
}

func NewLogicalContainerReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	orchestrator containers.ContainerOrchestrator,
	config LogicalContainerReconcilerConfig,
) *LogicalContainerReconciler {
	return &LogicalContainerReconciler{
		ReconcilerBase:     NewReconcilerBase[apiv2.LogicalContainer](client, noCacheClient, log, lifetimeCtx),
		containerInspector: orchestrator,
		runtimeName:        orchestrator.Name(),
		config:             config,
		containerData:      NewObjectStateMap[types.UID, logicalContainerData, *logicalContainerData, *apiv2.LogicalContainer](),
	}
}

func (r *LogicalContainerReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	indexes := []struct {
		field   string
		extract ctrl_client.IndexerFunc
	}{
		{logicalContainerImageRefField, logicalContainerImageReferences},
		{logicalContainerVolumeRefField, logicalContainerVolumeReferences},
		{logicalContainerNetworkRefField, logicalContainerNetworkReferences},
	}
	for _, index := range indexes {
		indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, &apiv2.LogicalContainer{}, index.field, index.extract)
		if indexErr != nil {
			r.Log.Error(indexErr, "Failed to create reference index for LogicalContainer", "IndexField", index.field)
			return indexErr
		}
	}
	for _, child := range []ctrl_client.Object{&apiv2.PhysicalContainer{}, &apiv2.PhysicalContainerImage{}, &apiv2.ServiceEndpoint{}} {
		ownerIndexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, child, logicalContainerOwnerUIDField, func(obj ctrl_client.Object) []string {
			owner := metav1.GetControllerOf(obj)
			if owner == nil {
				return nil
			}
			return []string{string(owner.UID)}
		})
		if ownerIndexErr != nil {
			r.Log.Error(ownerIndexErr, "Failed to create logical container owner index")
			return ownerIndexErr
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.LogicalContainer{}).
		Owns(&apiv2.PhysicalContainer{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Owns(&apiv2.PhysicalContainerImage{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Owns(&apiv2.ServiceEndpoint{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.PhysicalContainerImage{}, handler.EnqueueRequestsFromMapFunc(r.mapReferencedResourceToReconcileRequests(logicalContainerImageRefField)), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.PhysicalContainerVolume{}, handler.EnqueueRequestsFromMapFunc(r.mapReferencedResourceToReconcileRequests(logicalContainerVolumeRefField)), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.LogicalContainerNetwork{}, handler.EnqueueRequestsFromMapFunc(r.mapReferencedResourceToReconcileRequests(logicalContainerNetworkRefField)), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.LogicalContainerList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Named(name).
		Complete(r)
}

func logicalContainerImageReferences(rawObj ctrl_client.Object) []string {
	container := rawObj.(*apiv2.LogicalContainer)
	if container.Spec.Container == nil || container.Spec.Container.ImageRef == "" {
		return nil
	}
	imageName := commonapi.AsNamespacedName(container.Spec.Container.ImageRef, container.Namespace)
	return []string{imageName.Name}
}

func logicalContainerVolumeReferences(rawObj ctrl_client.Object) []string {
	container := rawObj.(*apiv2.LogicalContainer)
	if container.Spec.Container == nil {
		return nil
	}
	volumeNames := make([]string, 0, len(container.Spec.Container.VolumeMounts))
	for _, mount := range container.Spec.Container.VolumeMounts {
		if mount.Type == apiv2.NamedVolumeMount && mount.VolumeRef != "" {
			volumeName := commonapi.AsNamespacedName(mount.VolumeRef, container.Namespace)
			volumeNames = append(volumeNames, volumeName.Name)
		}
	}
	return slices.Unique(volumeNames)
}

func logicalContainerNetworkReferences(rawObj ctrl_client.Object) []string {
	container := rawObj.(*apiv2.LogicalContainer)
	if container.Spec.Container == nil {
		return nil
	}
	networkNames := make([]string, 0, len(container.Spec.Container.Networks))
	for _, network := range container.Spec.Container.Networks {
		if network.NetworkRef != "" {
			networkName := commonapi.AsNamespacedName(network.NetworkRef, container.Namespace)
			networkNames = append(networkNames, networkName.Name)
		}
	}
	return slices.Unique(networkNames)
}

func (r *LogicalContainerReconciler) mapReferencedResourceToReconcileRequests(indexField string) handler.MapFunc {
	return func(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
		resourceName := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
		var containerList apiv2.LogicalContainerList
		listErr := r.List(ctx, &containerList, ctrl_client.InNamespace(resourceName.Namespace), ctrl_client.MatchingFields{indexField: resourceName.Name})
		if listErr != nil {
			r.Log.Error(listErr, "Failed to list LogicalContainers referencing resource", "Resource", resourceName, "IndexField", indexField)
			return nil
		}

		requests := make([]reconcile.Request, len(containerList.Items))
		for i := range containerList.Items {
			requests[i] = reconcile.Request{NamespacedName: containerList.Items[i].NamespacedName()}
		}
		r.Log.V(1).Info("Referenced resource updated, requesting LogicalContainer reconciliation", "Resource", resourceName, "IndexField", indexField, "Containers", len(requests))
		return requests
	}
}

func (r *LogicalContainerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)
	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	var container apiv2.LogicalContainer
	getErr := reader.Get(ctx, req.NamespacedName, &container)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			log.V(1).Info("LogicalContainer not found, nothing to do...")
			discardErr := r.discardLogicalContainerData(ctx, req.NamespacedName)
			if discardErr != nil {
				log.Error(discardErr, "Failed to discard logical container state")
				return ctrl.Result{}, discardErr
			}
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to Get() the LogicalContainer")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)
	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, container.GetResourceId())

	_, data := r.containerData.BorrowByNamespacedName(container.NamespacedName())

	patch := ctrl_client.MergeFromWithOptions(container.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	delay := StandardDelay
	if !container.DeletionTimestamp.IsZero() {
		change, delay = r.handleDeletionRequest(ctx, &container, data, log)
	} else {
		change = ensureFinalizer(&container, logicalContainerFinalizer, log)
		if change == noChange {
			change, delay = r.manageLogicalContainer(ctx, &container, data, log)
		}
	}
	return r.SaveChangesWithDelay(ctx, &container, patch, change, delay, nil, log)
}

func (r *LogicalContainerReconciler) manageLogicalContainer(
	ctx context.Context,
	container *apiv2.LogicalContainer,
	data *logicalContainerData,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	if data == nil {
		namespaceReady, namespaceReason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, container.Namespace)
		if !namespaceReady {
			namespaceChange := noChange
			namespacePhase := apiv2.LogicalContainerPhasePending
			namespaceMessage := namespaceReadinessMessage(container.Namespace, namespaceReason)
			if namespaceErr != nil {
				log.Error(namespaceErr, "Failed to get namespace", "Namespace", container.Namespace)
				namespacePhase = apiv2.LogicalContainerPhaseUnknown
				namespaceMessage = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
				namespaceChange |= additionalReconciliationNeeded
			}
			namespaceChange |= setValue(&container.Status.Phase, namespacePhase)
			namespaceChange |= setCondition(&container.Status.Conditions, apiv2.ConditionReady,
				container.Generation, metav1.ConditionFalse, namespaceReason, namespaceMessage)
			return namespaceChange, StandardDelay
		}
		data = &logicalContainerData{
			state: logicalContainerStatePending,
		}
		r.containerData.Store(container.NamespacedName(), container.UID, data.Clone())
	}
	handler := getStateInitializer(logicalContainerDataHandlers, data.state, log)
	change := handler(ctx, r, container, data.state, data, log)
	_ = r.containerData.Update(container.NamespacedName(), container.UID, data)
	dataChange, delay, valid := data.applyTo(container)
	if !valid {
		log.Error(fmt.Errorf("invalid logical container state %v", data.state),
			"LogicalContainer reached invalid reconciliation state")
	}
	return change | dataChange, delay
}

func (r *LogicalContainerReconciler) prepareLogicalContainerValues(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData, log logr.Logger,
) error {
	if container.Spec.Stop {
		return nil
	}
	requests := make([]logicalPortRequest, len(container.Spec.Ports))
	for portIndex, port := range container.Spec.Ports {
		requests[portIndex] = logicalPortRequest{binding: port.LogicalServiceBinding, address: port.Address, port: port.ContainerPort}
	}
	ports, prepareErr := prepareLogicalPorts(ctx, r.Client, r.NoCacheClient, container, requests, log)
	if prepareErr != nil {
		log.Error(prepareErr, "Failed to prepare logical container ports")
		data.reason, data.message = apiv2.LogicalResourceReasonPortPreparationPending, prepareErr.Error()
		if errors.Is(prepareErr, errLogicalServiceDependencyFailed) {
			data.reason = apiv2.LogicalResourceReasonPortPreparationFailed
			return fmt.Errorf("%w: %w", errLogicalValuePreparationFailed, prepareErr)
		} else if !apierrors.IsNotFound(prepareErr) {
			data.reconcileErr = prepareErr
		}
		return prepareErr
	}
	config := container.Spec.Container
	if config != nil {
		arguments, environment, evaluationErr := templating.EvaluateV2Values(
			ctx, r.NoCacheClient, container, servingPorts(ports), config.Command, config.Env, false)
		if evaluationErr != nil {
			log.Error(evaluationErr, "Failed to evaluate logical container values")
			data.reason, data.message = apiv2.LogicalResourceReasonTemplateEvaluationFailed, evaluationErr.Error()
			if !templating.IsTransientTemplateError(evaluationErr) {
				return fmt.Errorf("%w: %w", errLogicalValuePreparationFailed, evaluationErr)
			}
			return evaluationErr
		}
		data.effectiveCommand, data.effectiveEnv = arguments, environment
	}
	data.ports = logicalPortSelections(ports, requests)
	return nil
}

func handleLogicalContainerPending(
	ctx context.Context, reconciler *LogicalContainerReconciler, container *apiv2.LogicalContainer,
	_ logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	data.reconcileErr = nil
	data.reason, data.message = "", ""
	data.retryAfter = time.Time{}

	physical, lookupErr := reconciler.findPhysicalContainer(ctx, container, data, reconciler.Client, container.UID)
	if lookupErr != nil {
		log.Error(lookupErr, "Failed to list physical containers backing the current logical container")
		return additionalReconciliationNeeded
	}
	if physical != nil {
		if physical.Spec.Container != nil {
			data.effectiveCommand = std_slices.Clone(physical.Spec.Container.Command)
			data.effectiveEnv = std_slices.Clone(physical.Spec.Container.Env)
			for _, label := range physical.Spec.Container.Labels {
				if label.Key == lifecycleKeyLabel {
					data.lifecycleKey = label.Value
				}
			}
		}
	} else if data.physicalContainerUID != "" {
		data.message = "Waiting to observe the owned PhysicalContainer."
		return additionalReconciliationNeeded
	}

	var spec apiv2.PhysicalContainerSpec
	lifecycleKey := data.lifecycleKey
	if lifecycleKey == "" {
		lifecycleKey = container.Spec.LifecycleKey
	}
	if physical != nil {
		spec = physical.Spec
	} else {
		if preparationErr := reconciler.prepareLogicalContainerValues(ctx, container, data, log); preparationErr != nil {
			if errors.Is(preparationErr, errLogicalValuePreparationFailed) {
				data.state = logicalContainerStatePreparationFailed
				return noChange
			}
			return additionalReconciliationNeeded
		}
		mode := container.Spec.EffectiveMode()
		if mode == apiv2.LogicalContainerModePersistent && data.lease == nil {
			if reconciler.config.StateStore == nil {
				log.Error(errors.New("state store is not configured"), "Cannot prepare persistent logical container")
				data.state, data.reason = logicalContainerStatePreparationFailed, apiv2.LogicalResourceReasonStateStoreNotConfigured
				data.message = "State store is not configured."
				return noChange
			}
			lease, leaseErr := reconciler.config.StateStore.AcquireResourceLease(ctx, container,
				reconciler.config.ResourceLeaseOwner, resourceLeaseRevalidationInterval)
			if leaseErr != nil {
				data.reason, data.reconcileErr = apiv2.LogicalResourceReasonLeaseAcquisitionFailed, leaseErr
				data.message = leaseErr.Error()
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				if errors.Is(leaseErr, statestore.ErrResourceLeaseHeld) {
					logResourceLeaseHeld(log, leaseErr, container.GetLeaseKey(), "Persistent container is being updated by another DCP instance, retrying")
					data.reason = apiv2.LogicalResourceReasonLeaseHeld
				} else {
					log.Error(leaseErr, "Failed to acquire persistent logical container lease")
				}
				return additionalReconciliationNeeded
			}
			data.lease = lease
		}

		var inspected *containers.InspectedContainer
		if mode.ShouldReuseExisting() {
			inspection, inspectErr := inspectContainerIfExists(ctx, reconciler.containerInspector, container.Spec.ContainerName)
			if inspectErr == nil && (inspection == nil || inspection.Id == "") {
				inspectErr = errors.New("container inspection returned no runtime identity")
			}
			if inspectErr != nil && !errors.Is(inspectErr, containers.ErrNotFound) {
				log.Error(inspectErr, "Failed to inspect logical container by name")
				data.reason, data.reconcileErr = apiv2.LogicalContainerReasonRuntimeLookupFailed, inspectErr
				data.message = inspectErr.Error()
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				return additionalReconciliationNeeded
			}
			if inspectErr == nil {
				inspected = inspection
			}
		}

		if inspected == nil && container.Spec.Stop {
			data.state = logicalContainerStateStopped
			data.reason, data.message = apiv2.LogicalContainerReasonStopped, "Stop was requested before a runtime container was created."
			if data.lease != nil {
				return reconciler.settleLogicalContainerLease(ctx, container, data, nil, log)
			}
			return noChange
		}
		if inspected == nil && !mode.ShouldCreateIfMissing() {
			data.reason = apiv2.LogicalContainerReasonExistingContainerNotFound
			data.message = fmt.Sprintf("Runtime container %q was not found.", container.Spec.ContainerName)
			data.retryAfter = time.Now().Add(delayDurations[MonitoringDelay].Duration)
			return additionalReconciliationNeeded
		}

		containerID := ""
		existingKey := ""
		managed := false
		if inspected != nil {
			containerID, existingKey = inspected.Id, inspected.Labels[lifecycleKeyLabel]
			_, managed = inspected.Labels[dcpBuildLabel]
		}
		var physicalPorts []apiv2.ContainerPort
		if !container.Spec.Stop {
			for portIndex, port := range container.Spec.Ports {
				physicalPorts = append(physicalPorts, port.ToContainerPort(data.ports[portIndex].protocol))
			}
		}
		replaceExisting := false
		lifecycleSpec := container.Spec.DeepCopy()
		if lifecycleSpec.Container != nil {
			lifecycleSpec.Container.Command = std_slices.Clone(data.effectiveCommand)
			lifecycleSpec.Container.Env = std_slices.Clone(data.effectiveEnv)
		}
		if mode.ShouldCreateIfMissing() && !container.Spec.Stop && (inspected == nil || managed) {
			desiredKey, _, keyErr := lifecycleSpec.GetLifecycleKey(physicalPorts)
			if keyErr != nil {
				log.Error(keyErr, "Failed to calculate logical container lifecycle key")
				data.state, data.reason = logicalContainerStatePreparationFailed, apiv2.LogicalResourceReasonLifecycleKeyCalculationFailed
				data.message = keyErr.Error()
				return noChange
			}
			replaceExisting = inspected != nil && (desiredKey != existingKey || inspected.Status != containers.ContainerStatusRunning)
			lifecycleKey = desiredKey
			if replaceExisting {
				containerID = ""
			}
		}

		spec = apiv2.PhysicalContainerSpec{ContainerID: containerID, Stop: container.Spec.Stop}
		if containerID == "" {
			containerName := container.Spec.ContainerName
			if containerName == "" {
				generatedName, _, nameErr := MakeUniqueName(container.Name)
				if nameErr != nil {
					log.Error(nameErr, "Failed to generate logical container runtime name")
					data.reason, data.reconcileErr = apiv2.LogicalResourceReasonLifecyclePreparationFailed, nameErr
					data.message = nameErr.Error()
					return additionalReconciliationNeeded
				}
				containerName = generatedName
			}
			desired := physicalContainerConfigFromLogicalSpec(lifecycleSpec, physicalPorts)
			desired.ContainerName, desired.ReplaceExisting = containerName, replaceExisting
			desired.ImageRef = commonapi.AsNamespacedName(desired.ImageRef, container.Namespace).Name
			desired.Labels = append(desired.Labels,
				commonapi.Label{Key: dcpBuildLabel, Value: version.ProductVersion},
				commonapi.Label{Key: groupVersionLabel, Value: apiv2.GroupVersion.String()},
				commonapi.Label{Key: nameLabel, Value: container.Name},
				commonapi.Label{Key: lifecycleKeyLabel, Value: lifecycleKey},
			)
			spec.Container = desired
		}
	}
	data.lifecycleKey = lifecycleKey

	change := noChange
	if spec.Container != nil && container.Spec.Container != nil && container.Spec.Container.Image != nil {
		image, imageErr := reconciler.findPhysicalContainerImage(ctx, container, data, reconciler.Client, container.UID)
		if imageErr == nil && image == nil && physical == nil && data.physicalImageUID == "" {
			image, imageErr = reconciler.createPhysicalContainerImage(ctx, container, data, "logical-"+string(container.UID))
		}
		if imageErr != nil {
			log.Error(imageErr, "Failed to prepare logical container image")
			// AlreadyExists here means an invalid existing physical image; valid ones are returned without an error.
			if apierrors.IsAlreadyExists(imageErr) {
				data.state, data.reason, data.message = logicalContainerStatePreparationFailed, apiv2.LogicalContainerReasonPhysicalResourcePreparationFailed, imageErr.Error()
				data.reconcileErr = nil
				return noChange
			}
			if data.state != logicalContainerStateUnknown && data.reconcileErr == nil {
				data.reason, data.message, data.reconcileErr = apiv2.LogicalContainerReasonPhysicalResourcePreparationFailed, imageErr.Error(), imageErr
			}
			return additionalReconciliationNeeded
		}
		if image == nil {
			data.state = logicalContainerStatePending
			data.message = "Waiting to observe the owned PhysicalContainerImage."
			return additionalReconciliationNeeded
		}
		change |= applyLogicalContainerStartupImageStatus(container, image)
		if physical == nil {
			spec.Container.ImageRef = image.Name
		}
	}
	if physical == nil {
		if spec.Container != nil {
			networksResolved, physicalNetworks, networkMessage, resolveErr := reconciler.resolveLogicalContainerNetworks(ctx, container)
			if resolveErr != nil {
				log.Error(resolveErr, "Failed to resolve logical container networks")
				data.message = resolveErr.Error()
				if errors.Is(resolveErr, errLogicalNetworkFailed) {
					data.state, data.reason = logicalContainerStatePreparationFailed, apiv2.LogicalContainerReasonLogicalNetworkFailed
					if data.lease != nil {
						return change | reconciler.settleLogicalContainerLease(ctx, container, data, nil, log)
					}
					return change
				}
				data.reason, data.reconcileErr = apiv2.LogicalContainerReasonPhysicalResourcePreparationFailed, resolveErr
				return change | additionalReconciliationNeeded
			}
			if !networksResolved {
				data.message = networkMessage
				return change
			}
			spec.Container.Networks = physicalNetworks
		}
		if data.lease != nil {
			verifyErr := reconciler.config.StateStore.VerifyResourceLeaseHeld(ctx, container, reconciler.config.ResourceLeaseOwner)
			if verifyErr != nil {
				log.Error(verifyErr, "Failed to verify persistent container startup lease")
				data.state, data.reason = logicalContainerStatePreparationFailed, apiv2.LogicalResourceReasonLeaseLost
				data.message = verifyErr.Error()
				return change
			}
		}
		created, createErr := reconciler.createPhysicalContainer(ctx, container, data, spec)
		if createErr != nil {
			log.Error(createErr, "Failed to prepare PhysicalContainer")
			// AlreadyExists here means an invalid existing physical container; valid ones are returned without an error.
			if apierrors.IsAlreadyExists(createErr) {
				data.state, data.reason, data.message = logicalContainerStatePreparationFailed, apiv2.LogicalContainerReasonPhysicalResourcePreparationFailed, createErr.Error()
				data.reconcileErr = nil
				return change
			}
			if data.state != logicalContainerStateUnknown {
				data.reconcileErr = createErr
				data.message = createErr.Error()
			}
			if created != nil {
				if created.UID != "" {
					data.physicalContainerUID = created.UID
				}
			}
			return change | additionalReconciliationNeeded
		}
		physical = created
		data.physicalContainerUID = physical.UID
	}
	if physical.Spec.Container != nil && container.Spec.Container != nil &&
		container.Spec.Container.Image != nil && data.physicalImageUID == "" {
		data.state = logicalContainerStatePending
		data.message = "Waiting to observe the owned PhysicalContainerImage."
		return change | additionalReconciliationNeeded
	}
	if physical.Spec.Container == nil && data.lease != nil {
		change |= reconciler.settleLogicalContainerLease(ctx, container, data, nil, log)
		if data.lease != nil {
			return change
		}
	}
	data.state = logicalContainerStateCreating
	if container.Spec.Stop {
		data.state = logicalContainerStateStopping
	}
	return change
}

func physicalContainerConfigFromLogicalSpec(spec *apiv2.LogicalContainerSpec, ports []apiv2.ContainerPort) *apiv2.PhysicalContainerConfig {
	source := spec.Container
	config := &apiv2.PhysicalContainerConfig{
		RetainRuntimeContainer: spec.EffectiveMode() == apiv2.LogicalContainerModePersistent,
		MonitorPID:             spec.MonitorPID, MonitorTimestamp: spec.MonitorTimestamp,
		ImageRef: source.ImageRef, ContainerName: spec.ContainerName,
		Entrypoint: source.Entrypoint, Command: source.Command, Env: source.Env,
		Ports: ports, VolumeMounts: source.VolumeMounts, CreateFiles: source.CreateFiles, Labels: source.Labels,
	}
	return config.DeepCopy()
}

func (r *LogicalContainerReconciler) resolveLogicalContainerNetworks(
	ctx context.Context, container *apiv2.LogicalContainer,
) (bool, []apiv2.ContainerNetworkConnectionConfig, string, error) {
	var resolved []apiv2.ContainerNetworkConnectionConfig
	var pendingMessage string
	var resolutionErr error
	for _, requested := range container.Spec.Container.Networks {
		network := &apiv2.LogicalContainerNetwork{}
		networkName := commonapi.AsNamespacedName(requested.NetworkRef, container.Namespace)
		getErr := r.Get(ctx, networkName, network)
		if apierrors.IsNotFound(getErr) {
			if pendingMessage == "" {
				pendingMessage = fmt.Sprintf("LogicalContainerNetwork %q does not exist.", networkName)
			}
			continue
		}
		if getErr != nil {
			resolutionErr = errors.Join(resolutionErr, fmt.Errorf("get LogicalContainerNetwork %q: %w", networkName, getErr))
			continue
		}
		if network.Status.Phase == apiv2.LogicalContainerNetworkPhaseFailed {
			message := fmt.Sprintf("LogicalContainerNetwork %q failed.", networkName)
			if failure := apimeta.FindStatusCondition(network.Status.Conditions, string(apiv2.ConditionReady)); failure != nil {
				message = fmt.Sprintf("LogicalContainerNetwork %q reports %s: %s", networkName, failure.Reason, failure.Message)
			}
			resolutionErr = errors.Join(resolutionErr, fmt.Errorf("%w: %s", errLogicalNetworkFailed, message))
			continue
		}
		if !network.DeletionTimestamp.IsZero() {
			if pendingMessage == "" {
				pendingMessage = fmt.Sprintf("LogicalContainerNetwork %q is terminating.", networkName)
			}
			continue
		}
		if network.Status.PhysicalNetworkRef == "" {
			if pendingMessage == "" {
				pendingMessage = fmt.Sprintf("LogicalContainerNetwork %q has not published its backing physical network reference.", networkName)
			}
			continue
		}
		resolved = append(resolved, apiv2.ContainerNetworkConnectionConfig{
			NetworkRef: network.Status.PhysicalNetworkRef, Aliases: std_slices.Clone(requested.Aliases),
		})
	}
	if resolutionErr != nil {
		return false, nil, "", resolutionErr
	}
	if pendingMessage != "" {
		return false, nil, pendingMessage, nil
	}
	return true, resolved, "", nil
}

// createPhysicalContainerImage declares an owned image and validates AlreadyExists collisions.
func (r *LogicalContainerReconciler) createPhysicalContainerImage(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData, physicalName string,
) (*apiv2.PhysicalContainerImage, error) {
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{Name: physicalName, Namespace: container.Namespace},
		Spec:       apiv2.PhysicalContainerImageSpec{Image: container.Spec.Container.Image.DeepCopy()},
	}
	InitializeLogicalBackingFinalizers(image, physicalContainerImageFinalizer)
	ownerErr := ctrl.SetControllerReference(container, image, r.Scheme())
	if ownerErr != nil {
		return nil, fmt.Errorf("set physical image owner: %w", ownerErr)
	}
	data.unobservedChildren.recordCreateAttempt(image)
	createErr := r.Create(ctx, image)
	if image.UID != "" {
		data.physicalImageUID = image.UID
	}
	if createErr != nil {
		if !apierrors.IsAlreadyExists(createErr) {
			return image, createErr
		}
		existing := &apiv2.PhysicalContainerImage{}
		if lookupErr := r.NoCacheClient.Get(ctx, image.NamespacedName(), existing); lookupErr != nil {
			return image, fmt.Errorf("get existing physical image: %w", lookupErr)
		}
		if identityErr := validatePhysicalResourceIdentity(container, existing, data.physicalImageUID); identityErr != nil {
			return nil, fmt.Errorf("%v: %w", identityErr, createErr)
		}
		if !reflect.DeepEqual(existing.Spec, image.Spec) {
			return nil, fmt.Errorf("physical image %q has a different spec: %w", existing.NamespacedName(), createErr)
		}
		data.physicalImageUID = existing.UID
		return existing, nil
	}
	return image, nil
}

// createPhysicalContainer declares the physical resource and validates AlreadyExists collisions.
func (r *LogicalContainerReconciler) createPhysicalContainer(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData, spec apiv2.PhysicalContainerSpec,
) (*apiv2.PhysicalContainer, error) {
	physical := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(container.UID), Namespace: container.Namespace},
		Spec:       spec,
	}
	InitializeLogicalBackingFinalizers(physical, physicalContainerFinalizer)
	ownerErr := ctrl.SetControllerReference(container, physical, r.Scheme())
	if ownerErr != nil {
		return nil, fmt.Errorf("set physical container owner: %w", ownerErr)
	}
	data.unobservedChildren.recordCreateAttempt(physical)
	createErr := r.Create(ctx, physical)
	if physical.UID != "" {
		data.physicalContainerUID = physical.UID
	}
	if createErr != nil {
		if !apierrors.IsAlreadyExists(createErr) {
			return physical, createErr
		}
		existing := &apiv2.PhysicalContainer{}
		if lookupErr := r.NoCacheClient.Get(ctx, physical.NamespacedName(), existing); lookupErr != nil {
			return physical, fmt.Errorf("get existing physical container: %w", lookupErr)
		}
		if identityErr := validatePhysicalResourceIdentity(container, existing, data.physicalContainerUID); identityErr != nil {
			return nil, fmt.Errorf("%v: %w", identityErr, createErr)
		}
		if existing.Spec.Container != nil {
			data.effectiveCommand = std_slices.Clone(existing.Spec.Container.Command)
			data.effectiveEnv = std_slices.Clone(existing.Spec.Container.Env)
			for _, label := range existing.Spec.Container.Labels {
				if label.Key == lifecycleKeyLabel {
					data.lifecycleKey = label.Value
				}
			}
			if len(existing.Spec.Container.Ports) == len(container.Spec.Ports) {
				data.ports = nil
				for _, port := range existing.Spec.Container.Ports {
					protocol := port.Protocol
					if protocol == "" {
						protocol = commonapi.PortProtocolTCP
					}
					data.ports = append(data.ports, logicalPortSelection{protocol: protocol})
				}
			}
		}
		data.physicalContainerUID = existing.UID
		return existing, nil
	}
	return physical, nil
}

func (r *LogicalContainerReconciler) findPhysicalContainer(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData,
	reader ctrl_client.Reader, ownerUID types.UID,
) (*apiv2.PhysicalContainer, error) {
	var children apiv2.PhysicalContainerList
	listErr := reader.List(ctx, &children, ctrl_client.InNamespace(container.Namespace),
		ctrl_client.MatchingFields{logicalContainerOwnerUIDField: string(ownerUID)})
	if listErr != nil {
		lookupErr := fmt.Errorf("list owned physical containers: %w", listErr)
		data.reason, data.message, data.reconcileErr = apiv2.LogicalContainerReasonPhysicalContainerLookupFailed, lookupErr.Error(), lookupErr
		return nil, lookupErr
	}
	var owned *apiv2.PhysicalContainer
	for i := range children.Items {
		child := &children.Items[i]
		if !metav1.IsControlledBy(child, container) {
			continue
		}
		if owned != nil {
			multipleErr := fmt.Errorf("%w: more than one PhysicalContainer belongs to %q",
				errMultipleLogicalContainerBackings, container.NamespacedName())
			data.state, data.reason, data.message, data.reconcileErr = logicalContainerStateUnknown, apiv2.LogicalContainerReasonMultiplePhysicalContainers, multipleErr.Error(), nil
			return nil, multipleErr
		}
		owned = child
	}
	if owned != nil {
		data.unobservedChildren.observeCached(owned)
		if (data.physicalContainerUID != "" && owned.UID != data.physicalContainerUID) || !hasFinalizer(owned, LogicalBackingFinalizer) {
			identityErr := fmt.Errorf("physical container %q UID or logical hold changed", owned.NamespacedName())
			data.state, data.reason, data.message, data.reconcileErr = logicalContainerStateUnknown, apiv2.LogicalContainerReasonPhysicalContainerChanged, identityErr.Error(), nil
			return nil, identityErr
		}
		data.physicalContainerUID = owned.UID
	}
	return owned, nil
}

func (r *LogicalContainerReconciler) findPhysicalContainerImage(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData,
	reader ctrl_client.Reader, ownerUID types.UID,
) (*apiv2.PhysicalContainerImage, error) {
	if container.Spec.Container == nil || container.Spec.Container.Image == nil {
		return nil, nil
	}
	var children apiv2.PhysicalContainerImageList
	listErr := reader.List(ctx, &children, ctrl_client.InNamespace(container.Namespace),
		ctrl_client.MatchingFields{logicalContainerOwnerUIDField: string(ownerUID)})
	if listErr != nil {
		lookupErr := fmt.Errorf("list owned physical images: %w", listErr)
		data.reason, data.message, data.reconcileErr = apiv2.LogicalContainerReasonPhysicalImageLookupFailed, lookupErr.Error(), lookupErr
		return nil, lookupErr
	}
	var owned *apiv2.PhysicalContainerImage
	for i := range children.Items {
		child := &children.Items[i]
		if !metav1.IsControlledBy(child, container) {
			continue
		}
		if owned != nil {
			multipleErr := fmt.Errorf("%w: more than one PhysicalContainerImage belongs to %q",
				errMultipleLogicalContainerBackings, container.NamespacedName())
			data.state, data.reason, data.message, data.reconcileErr = logicalContainerStateUnknown, apiv2.LogicalContainerReasonMultiplePhysicalImages, multipleErr.Error(), nil
			return nil, multipleErr
		}
		owned = child
	}
	if owned != nil {
		data.unobservedChildren.observeCached(owned)
		if (data.physicalImageUID != "" && owned.UID != data.physicalImageUID) || !hasFinalizer(owned, LogicalBackingFinalizer) {
			identityErr := fmt.Errorf("physical image %q UID or logical hold changed", owned.NamespacedName())
			data.state, data.reason, data.message, data.reconcileErr = logicalContainerStateUnknown, apiv2.LogicalContainerReasonPhysicalImageChanged, identityErr.Error(), nil
			return nil, identityErr
		}
		data.physicalImageUID = owned.UID
	}
	return owned, nil
}

func handleLogicalContainerRunning(
	ctx context.Context, reconciler *LogicalContainerReconciler, container *apiv2.LogicalContainer,
	state logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	data.reconcileErr = nil
	if container.Spec.Stop {
		data.state, data.reason, data.message = logicalContainerStateStopping, apiv2.LogicalContainerReasonStopping, ""
		return additionalReconciliationNeeded
	}
	change := noChange
	physical, getErr := reconciler.findPhysicalContainer(ctx, container, data, reconciler.Client, container.UID)
	if getErr != nil {
		log.Error(getErr, "Failed to get running container backing")
		return additionalReconciliationNeeded
	}
	if physical == nil {
		data.state, data.message = logicalContainerStateUnknown, "The backing PhysicalContainer is no longer available."
		return additionalReconciliationNeeded
	}
	change |= applyLogicalContainerBackingStatus(container, physical)
	startupImage, imageErr := reconciler.findPhysicalContainerImage(ctx, container, data, reconciler.Client, container.UID)
	if imageErr != nil {
		log.Error(imageErr, "Failed to get running container startup image")
		return change | additionalReconciliationNeeded
	} else if startupImage != nil {
		change |= applyLogicalContainerStartupImageStatus(container, startupImage)
	}
	change |= reconciler.settleLogicalContainerObservedLease(ctx, container, data, physical, log)
	observedState := logicalContainerBackingState(physical)
	if observedState != state {
		if observedState == logicalContainerStateRunning && len(container.Spec.Ports) > 0 {
			observedState = logicalContainerStateCreating
		}
		data.state, data.reason, data.message = observedState, "", ""
		if ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady)); ready != nil {
			data.message = fmt.Sprintf("PhysicalContainer reports %s: %s", ready.Reason, ready.Message)
		}
		return change | additionalReconciliationNeeded
	}
	data.reason, data.message = "", ""
	publish := state == logicalContainerStateRunning && physical.DeletionTimestamp.IsZero()
	if _, endpointErr := syncLogicalEndpoints(ctx, reconciler.Client, reconciler.NoCacheClient, container, data, logicalContainerOwnerUIDField, &data.unobservedChildren, publish, physical.Status.PortMappings...); endpointErr != nil {
		log.Error(endpointErr, "Failed to synchronize logical container endpoints")
		change |= additionalReconciliationNeeded
	}
	return change
}

func handleLogicalContainerCreating(
	ctx context.Context, reconciler *LogicalContainerReconciler, container *apiv2.LogicalContainer,
	_ logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	if container.Spec.Stop {
		data.state, data.reason, data.message = logicalContainerStateStopping, apiv2.LogicalContainerReasonStopping, ""
		return additionalReconciliationNeeded
	}
	data.reconcileErr = nil
	change := noChange
	physical, getErr := reconciler.findPhysicalContainer(ctx, container, data, reconciler.Client, container.UID)
	if getErr != nil {
		log.Error(getErr, "Failed to get creating container backing")
		return additionalReconciliationNeeded
	}
	if physical == nil {
		data.message = "Waiting to observe the owned PhysicalContainer."
		return additionalReconciliationNeeded
	}
	change |= applyLogicalContainerBackingStatus(container, physical)
	startupImage, imageErr := reconciler.findPhysicalContainerImage(ctx, container, data, reconciler.Client, container.UID)
	if imageErr != nil {
		log.Error(imageErr, "Failed to get creating container startup image")
		return change | additionalReconciliationNeeded
	} else if startupImage != nil {
		change |= applyLogicalContainerStartupImageStatus(container, startupImage)
	}
	observedState := logicalContainerBackingState(physical)
	data.reason, data.message = "", ""
	ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	if observedState != logicalContainerStateRunning && ready != nil {
		data.message = fmt.Sprintf("PhysicalContainer reports %s: %s", ready.Reason, ready.Message)
	}
	change |= reconciler.settleLogicalContainerObservedLease(ctx, container, data, physical, log)
	if observedState == logicalContainerStateRunning && len(container.Spec.Ports) > 0 {
		addressesReady, endpointErr := syncLogicalEndpoints(ctx, reconciler.Client, reconciler.NoCacheClient, container, data, logicalContainerOwnerUIDField, &data.unobservedChildren, true, physical.Status.PortMappings...)
		if endpointErr != nil {
			log.Error(endpointErr, "Failed to publish starting container endpoints")
			data.reason, data.message = apiv2.LogicalResourceReasonPortPreparationPending, "Waiting to publish service endpoints."
			return change | additionalReconciliationNeeded
		}
		if !addressesReady {
			data.reason, data.message = apiv2.LogicalResourceReasonPortPreparationPending, "Waiting for container port mappings."
			return change | additionalReconciliationNeeded
		}
	}
	data.state = observedState
	if data.state != logicalContainerStateCreating {
		change |= additionalReconciliationNeeded
	}
	return change
}

func handleLogicalContainerUnknown(
	ctx context.Context, reconciler *LogicalContainerReconciler, container *apiv2.LogicalContainer,
	state logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	change := noChange
	if _, endpointErr := syncLogicalEndpoints(ctx, reconciler.Client, reconciler.NoCacheClient, container, data, logicalContainerOwnerUIDField, &data.unobservedChildren, false); endpointErr != nil {
		log.Error(endpointErr, "Failed to withdraw unknown logical container endpoints")
		change |= additionalReconciliationNeeded
	}
	if state != logicalContainerStateUnknown {
		log.Error(fmt.Errorf("unknown logical container state %v", state), "LogicalContainer has an invalid reconciliation state")
		data.state, data.reason = logicalContainerStateUnknown, apiv2.PhysicalResourceReasonOperationStateInvalid
		data.message = "Logical container reached an invalid reconciliation state."
	}
	data.reconcileErr = nil
	physical, getErr := reconciler.findPhysicalContainer(ctx, container, data, reconciler.Client, container.UID)
	if getErr != nil {
		log.Error(getErr, "Failed to get unknown container backing")
		if data.lease != nil && data.reconcileErr == nil {
			change |= reconciler.settleLogicalContainerLease(ctx, container, data, nil, log)
		}
		return change
	}
	if physical == nil {
		data.message = "The backing PhysicalContainer is no longer available."
		if data.lease != nil {
			change |= reconciler.settleLogicalContainerLease(ctx, container, data, nil, log)
		}
		return change
	}
	change |= applyLogicalContainerBackingStatus(container, physical)
	startupImage, imageErr := reconciler.findPhysicalContainerImage(ctx, container, data, reconciler.Client, container.UID)
	if imageErr != nil {
		log.Error(imageErr, "Failed to get unknown container startup image")
		if data.lease != nil && data.reconcileErr == nil {
			change |= reconciler.settleLogicalContainerLease(ctx, container, data, nil, log)
		}
		return change
	}
	if startupImage == nil && data.physicalImageUID != "" {
		data.reason, data.message = apiv2.LogicalContainerReasonPhysicalImageChanged, "The owned PhysicalContainerImage is no longer available."
		if data.lease != nil {
			change |= reconciler.settleLogicalContainerLease(ctx, container, data, nil, log)
		}
		return change
	}
	if startupImage != nil {
		change |= applyLogicalContainerStartupImageStatus(container, startupImage)
	}
	change |= reconciler.settleLogicalContainerObservedLease(ctx, container, data, physical, log)
	observedState := logicalContainerBackingState(physical)
	if observedState != logicalContainerStateUnknown {
		if observedState == logicalContainerStateRunning && len(container.Spec.Ports) > 0 {
			observedState = logicalContainerStateCreating
		}
		data.state, data.reason, data.message = observedState, "", ""
		return change | additionalReconciliationNeeded
	}
	if container.Spec.Stop {
		data.state, data.reason, data.message = logicalContainerStateStopping, apiv2.LogicalContainerReasonStopping, ""
		return change | additionalReconciliationNeeded
	}
	return change
}

func handleLogicalContainerStopping(
	ctx context.Context, reconciler *LogicalContainerReconciler, container *apiv2.LogicalContainer,
	_ logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	change := noChange
	if _, endpointErr := syncLogicalEndpoints(ctx, reconciler.Client, reconciler.NoCacheClient, container, data, logicalContainerOwnerUIDField, &data.unobservedChildren, false); endpointErr != nil {
		log.Error(endpointErr, "Failed to withdraw stopping logical container endpoints")
		change |= additionalReconciliationNeeded
	}
	data.reconcileErr = nil
	physical, getErr := reconciler.findPhysicalContainer(ctx, container, data, reconciler.Client, container.UID)
	if getErr != nil {
		log.Error(getErr, "Failed to get stopping container backing")
		return change | additionalReconciliationNeeded
	}
	if physical == nil {
		if data.physicalContainerUID == "" {
			data.state, data.reason, data.message = logicalContainerStateStopped, apiv2.LogicalContainerReasonStopped, "Stop was requested and no runtime container exists."
			return change | additionalReconciliationNeeded
		}
		data.state, data.message = logicalContainerStateUnknown, "The backing PhysicalContainer is no longer available."
		return change | additionalReconciliationNeeded
	}
	change |= applyLogicalContainerBackingStatus(container, physical)
	change |= reconciler.settleLogicalContainerObservedLease(ctx, container, data, physical, log)
	switch physical.Status.Phase {
	case apiv2.PhysicalContainerPhaseExited:
		data.state, data.reason, data.message = logicalContainerStateStopped, apiv2.LogicalContainerReasonStopped, ""
		return change | additionalReconciliationNeeded
	case apiv2.PhysicalContainerPhaseFailed:
		data.state, data.reason = logicalContainerStateStopFailed, apiv2.LogicalContainerReasonPhysicalContainerFailed
		if ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady)); ready != nil {
			data.message = ready.Message
		}
		return change | additionalReconciliationNeeded
	}
	data.reason, data.message = apiv2.LogicalContainerReasonStopping, "Waiting for the physical controller to stop the runtime container."
	if !physical.Spec.Stop {
		patch := ctrl_client.MergeFromWithOptions(physical.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
		update := physical.DeepCopy()
		update.Spec.Stop = true
		if stopErr := reconciler.Patch(ctx, update, patch); stopErr != nil {
			log.Error(stopErr, "Failed to forward logical container stop request")
			data.reason, data.reconcileErr = apiv2.LogicalContainerReasonStopRequestFailed, stopErr
			data.message = fmt.Sprintf("Failed to forward stop request: %v", stopErr)
		}
	}
	return change | additionalReconciliationNeeded
}

func handleLogicalContainerTerminal(
	ctx context.Context, reconciler *LogicalContainerReconciler, container *apiv2.LogicalContainer,
	_ logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	change := noChange
	if _, endpointErr := syncLogicalEndpoints(ctx, reconciler.Client, reconciler.NoCacheClient, container, data, logicalContainerOwnerUIDField, &data.unobservedChildren, false); endpointErr != nil {
		log.Error(endpointErr, "Failed to withdraw terminal logical container endpoints")
		change |= additionalReconciliationNeeded
	}
	if data.lease != nil {
		data.leaseFailureMessage = ""
		change |= reconciler.settleLogicalContainerLease(ctx, container, data, nil, log)
	}
	data.reconcileErr = nil
	if data.physicalContainerUID != "" {
		lookupData := *data
		physical, getErr := reconciler.findPhysicalContainer(ctx, container, &lookupData, reconciler.Client, container.UID)
		if getErr != nil {
			log.Error(getErr, "Failed to get terminal container backing")
		} else {
			change |= applyLogicalContainerBackingStatus(container, physical)
		}
	}
	imageLookupData := *data
	startupImage, imageErr := reconciler.findPhysicalContainerImage(ctx, container, &imageLookupData, reconciler.Client, container.UID)
	if imageErr != nil {
		log.Error(imageErr, "Failed to get terminal container startup image")
	} else if startupImage != nil {
		change |= applyLogicalContainerStartupImageStatus(container, startupImage)
	}
	return change
}

func (reconciler *LogicalContainerReconciler) settleLogicalContainerObservedLease(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData,
	physicalContainer *apiv2.PhysicalContainer, log logr.Logger,
) objectChange {
	if data.lease == nil {
		return noChange
	}
	ready := apimeta.FindStatusCondition(physicalContainer.Status.Conditions, string(apiv2.ConditionReady))
	if physicalContainer.Status.ContainerID != "" &&
		(ready == nil || ready.Reason != string(apiv2.PhysicalContainerReasonRuntimeContainerAlreadyTracked)) {
		var record *statestore.PersistentContainerRecord
		if physicalContainer.Spec.Container != nil {
			record = &statestore.PersistentContainerRecord{
				ResourceKey: container.GetLeaseKey(), ContainerID: physicalContainer.Status.ContainerID,
				ContainerName: physicalContainer.Status.ContainerName, RuntimeName: reconciler.runtimeName,
			}
		}
		return reconciler.settleLogicalContainerLease(ctx, container, data, record, log)
	}
	switch physicalContainer.Status.Phase {
	case apiv2.PhysicalContainerPhaseExited, apiv2.PhysicalContainerPhaseFailed:
		return reconciler.settleLogicalContainerLease(ctx, container, data, nil, log)
	}
	return noChange
}

func applyLogicalContainerBackingStatus(
	container *apiv2.LogicalContainer, physical *apiv2.PhysicalContainer,
) objectChange {
	change := noChange
	if physical != nil {
		change |= setValue(&container.Status.PhysicalContainerRef, physical.Name)
		physicalStatus := &physical.Status
		change |= setValue(&container.Status.ContainerID, physicalStatus.ContainerID)
		change |= setValue(&container.Status.ContainerName, physicalStatus.ContainerName)
		change |= setValue(&container.Status.Image, physicalStatus.Image)
		change |= setValue(&container.Status.CreatedAt, physicalStatus.CreatedAt)
		change |= setValue(&container.Status.StartedAt, physicalStatus.StartedAt)
		change |= setValue(&container.Status.FinishedAt, physicalStatus.FinishedAt)
		if !reflect.DeepEqual(container.Status.ExitCode, physicalStatus.ExitCode) {
			container.Status.ExitCode = nil
			if physicalStatus.ExitCode != nil {
				exitCode := *physicalStatus.ExitCode
				container.Status.ExitCode = &exitCode
			}
			change |= statusChanged
		}
		if !reflect.DeepEqual(container.Status.PortMappings, physicalStatus.PortMappings) {
			container.Status.PortMappings = std_slices.Clone(physicalStatus.PortMappings)
			change |= statusChanged
		}
		var runtimeLogs *apiv2.LogSource
		if physical.UID != "" {
			runtimeLogs = physical.GetLogSource()
		}
		if !reflect.DeepEqual(container.Status.RuntimeLogs, runtimeLogs) {
			container.Status.RuntimeLogs = runtimeLogs
			change |= statusChanged
		}
		if container.Spec.Container != nil && container.Spec.Container.Image != nil {
			imageRef := ""
			if physical.Spec.Container != nil {
				imageRef = physical.Spec.Container.ImageRef
			}
			change |= setValue(&container.Status.ImageRef, imageRef)
		}
	}
	return change
}

func applyLogicalContainerStartupImageStatus(
	container *apiv2.LogicalContainer, startupImage *apiv2.PhysicalContainerImage,
) objectChange {
	if startupImage == nil {
		return noChange
	}
	startupLogs := startupImage.GetLogSource()
	if !reflect.DeepEqual(container.Status.StartupLogs, startupLogs) {
		container.Status.StartupLogs = startupLogs
		return statusChanged
	}
	return noChange
}

func (r *LogicalContainerReconciler) handleDeletionRequest(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData, log logr.Logger,
) (change objectChange, delay AdditionalReconciliationDelay) {
	delay = StandardDelay
	if data == nil {
		data = &logicalContainerData{state: logicalContainerStateDelete}
		physicalName := "logical-" + string(container.UID)
		data.unobservedChildren.recordCreateAttempt(&apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: physicalName}})
		if container.Spec.Container != nil && container.Spec.Container.Image != nil {
			data.unobservedChildren.recordCreateAttempt(&apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: physicalName}})
		}
		for _, port := range container.Spec.Ports {
			data.unobservedChildren.recordCreateAttempt(&apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: logicalPortResourceName(container, port.Name)}})
		}
		r.containerData.Store(container.NamespacedName(), container.UID, data.Clone())
	}

	defer func() {
		if data != nil {
			_ = r.containerData.Update(container.NamespacedName(), container.UID, data)
			dataChange, projectionDelay, valid := data.applyTo(container)
			change |= dataChange
			delay = projectionDelay
			if !valid {
				log.Error(fmt.Errorf("invalid logical container state %v", data.state),
					"LogicalContainer reached invalid reconciliation state")
			}
		}
	}()

	data.state = logicalContainerStateDelete
	data.reason, data.message = "", ""
	data.reconcileErr = nil

	// Endpoint failures must not prevent requesting backing deletion.
	portsDeleted, portDeleteErr := cleanupLogicalEndpoints(ctx, r.Client, r.NoCacheClient, container, logicalContainerOwnerUIDField, data.unobservedChildren)
	if portDeleteErr != nil {
		log.Error(portDeleteErr, "Failed to delete logical container endpoints")
	}

	options := []ctrl_client.ListOption{
		ctrl_client.InNamespace(container.Namespace),
		ctrl_client.MatchingFields{logicalContainerOwnerUIDField: string(container.UID)},
	}
	var physicals apiv2.PhysicalContainerList
	containerListErr := r.List(ctx, &physicals, options...)
	if containerListErr != nil {
		log.Error(containerListErr, "Failed to list physical containers before deletion")
		data.reconcileErr, data.message = containerListErr, containerListErr.Error()
		return additionalReconciliationNeeded, delay
	}
	if confirmationErr := resolveUnobservedLogicalChildren(ctx, r.NoCacheClient, container, data.unobservedChildren, &physicals.Items); confirmationErr != nil {
		log.Error(confirmationErr, "Failed to resolve unobserved physical containers before deletion")
		data.reconcileErr, data.message = confirmationErr, confirmationErr.Error()
		return additionalReconciliationNeeded, delay
	}
	if len(physicals.Items) != 0 {
		if len(physicals.Items) == 1 && data.physicalContainerUID == "" {
			data.physicalContainerUID = physicals.Items[0].UID
		}
		waiting := false
		for i := range physicals.Items {
			physical := &physicals.Items[i]
			deleteErr := r.deleteLogicalContainerPhysicalResource(ctx, physical)
			if deleteErr != nil {
				log.Error(deleteErr, "Failed to request physical container deletion", "PhysicalContainer", physical.NamespacedName())
				data.reconcileErr, data.message = deleteErr, deleteErr.Error()
				waiting = true
			}
			if physical.DeletionTimestamp.IsZero() || hasFinalizer(physical, physicalContainerFinalizer) {
				waiting = true
			}
		}
		if waiting {
			return additionalReconciliationNeeded, delay
		}
		if data.lease != nil {
			var record *statestore.PersistentContainerRecord
			for i := range physicals.Items {
				physical := &physicals.Items[i]
				ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
				if physical.UID == data.physicalContainerUID && physical.Spec.Container != nil &&
					physical.Status.ContainerID != "" && ready != nil &&
					physical.Status.Phase != apiv2.PhysicalContainerPhaseFailed &&
					ready.Reason != string(apiv2.PhysicalContainerReasonRuntimeContainerMissing) &&
					ready.Reason != string(apiv2.PhysicalContainerReasonRuntimeContainerAlreadyTracked) {
					record = &statestore.PersistentContainerRecord{
						ResourceKey: container.GetLeaseKey(), ContainerID: physical.Status.ContainerID,
						ContainerName: physical.Status.ContainerName, RuntimeName: r.runtimeName,
					}
				}
			}
			return r.settleLogicalContainerLease(ctx, container, data, record, log), delay
		}
		for i := range physicals.Items {
			physical := &physicals.Items[i]
			releaseErr := ReleaseLogicalBackingFinalizer(ctx, r.Client, physical)
			if releaseErr != nil && !apierrors.IsNotFound(releaseErr) {
				log.Error(releaseErr, "Failed to release logical container backing", "PhysicalContainer", physical.NamespacedName())
				data.reconcileErr, data.message = releaseErr, releaseErr.Error()
			}
		}
		return additionalReconciliationNeeded, delay
	}

	var images apiv2.PhysicalContainerImageList
	imageListErr := r.List(ctx, &images, options...)
	if imageListErr != nil {
		log.Error(imageListErr, "Failed to list physical images before deletion")
		data.reconcileErr, data.message = imageListErr, imageListErr.Error()
		return additionalReconciliationNeeded, delay
	}
	if confirmationErr := resolveUnobservedLogicalChildren(ctx, r.NoCacheClient, container, data.unobservedChildren, &images.Items); confirmationErr != nil {
		log.Error(confirmationErr, "Failed to resolve unobserved physical images before deletion")
		data.reconcileErr, data.message = confirmationErr, confirmationErr.Error()
		return additionalReconciliationNeeded, delay
	}
	if len(images.Items) != 0 {
		if len(images.Items) == 1 && data.physicalImageUID == "" {
			data.physicalImageUID = images.Items[0].UID
		}
		waiting := false
		for i := range images.Items {
			image := &images.Items[i]
			deleteErr := r.deleteLogicalContainerPhysicalResource(ctx, image)
			if deleteErr != nil {
				log.Error(deleteErr, "Failed to request physical image deletion", "PhysicalContainerImage", image.NamespacedName())
				data.reconcileErr, data.message = deleteErr, deleteErr.Error()
				waiting = true
			}
			if image.DeletionTimestamp.IsZero() || hasFinalizer(image, physicalContainerImageFinalizer) {
				waiting = true
			}
		}
		if waiting {
			return additionalReconciliationNeeded, delay
		}
		for i := range images.Items {
			image := &images.Items[i]
			releaseErr := ReleaseLogicalBackingFinalizer(ctx, r.Client, image)
			if releaseErr != nil && !apierrors.IsNotFound(releaseErr) {
				log.Error(releaseErr, "Failed to release logical image backing", "PhysicalContainerImage", image.NamespacedName())
				data.reconcileErr, data.message = releaseErr, releaseErr.Error()
			}
		}
		return additionalReconciliationNeeded, delay
	}

	if data.lease != nil {
		return r.settleLogicalContainerLease(ctx, container, data, nil, log), delay
	}

	if portDeleteErr != nil || !portsDeleted {
		return additionalReconciliationNeeded, delay
	}

	discardErr := r.discardLogicalContainerData(ctx, container.NamespacedName())
	if discardErr != nil {
		log.Error(discardErr, "Failed to discard logical container state")
		data.reconcileErr = discardErr
		data.message = discardErr.Error()
		return additionalReconciliationNeeded, delay
	}
	data = nil
	return deleteFinalizer(container, logicalContainerFinalizer, log), delay
}

func (r *LogicalContainerReconciler) deleteLogicalContainerPhysicalResource(
	ctx context.Context, resource ctrl_client.Object,
) error {
	if resource.GetDeletionTimestamp().IsZero() {
		uid := resource.GetUID()
		deleteErr := r.Delete(ctx, resource, ctrl_client.Preconditions{UID: &uid})
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete backing physical resource: %w", deleteErr)
		}
	}
	return nil
}

func (r *LogicalContainerReconciler) discardLogicalContainerData(ctx context.Context, name types.NamespacedName) error {
	resourceUID, data := r.containerData.BorrowByNamespacedName(name)
	if data == nil {
		return nil
	}
	if data.lease != nil {
		releaseErr := data.lease.Release(ctx)
		if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
			return fmt.Errorf("release discarded logical container lease: %w", releaseErr)
		}
	}
	discarded := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{UID: resourceUID}}
	defer logger.ReleaseResourceLog(discarded.GetResourceId())
	r.containerData.DeleteByNamespacedName(name)
	return nil
}

func (r *LogicalContainerReconciler) settleLogicalContainerLease(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData,
	record *statestore.PersistentContainerRecord, log logr.Logger,
) objectChange {
	if data.lease == nil {
		return noChange
	}
	if data.leaseFailureMessage != "" && time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}

	data.leaseFailureMessage = ""
	if record != nil {
		workloadID, workloadErr := dcpclient.ResolveNamespaceWorkloadID(ctx, r.NoCacheClient, container.Namespace, r.config.WorkloadID)
		if workloadErr != nil {
			log.Error(workloadErr, "Failed to resolve persistent logical container workload ID")
			data.leaseFailureMessage = fmt.Sprintf("Failed to resolve persistent container workload ID: %v", workloadErr)
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			return additionalReconciliationNeeded
		}
		if workloadID != "" {
			record.WorkloadID = workloadID
			verifyErr := r.config.StateStore.VerifyResourceLeaseHeld(ctx, container, r.config.ResourceLeaseOwner)
			if verifyErr != nil {
				log.Error(verifyErr, "Failed to verify persistent logical container lease before recording ownership")
				data.leaseFailureMessage = fmt.Sprintf("Failed to verify persistent container lease: %v", verifyErr)
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				return additionalReconciliationNeeded
			}
			recordErr := r.config.StateStore.UpsertPersistentContainer(ctx, *record)
			if recordErr != nil {
				log.Error(recordErr, "Failed to record persistent logical container")
				data.leaseFailureMessage = fmt.Sprintf("Failed to record persistent container: %v", recordErr)
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				return additionalReconciliationNeeded
			}
		}
	}
	releaseErr := data.lease.Release(ctx)
	if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
		log.Error(releaseErr, "Failed to release persistent logical container startup lease")
		data.leaseFailureMessage = fmt.Sprintf("Failed to release persistent container lease: %v", releaseErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return additionalReconciliationNeeded
	}
	data.lease = nil
	data.retryAfter = time.Time{}
	return noChange
}
