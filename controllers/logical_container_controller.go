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
	"github.com/microsoft/dcp/internal/version"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/slices"
)

const (
	logicalContainerImageRefField   = ".spec.container.imageRef"
	logicalContainerVolumeRefField  = ".spec.container.volumeMounts.volumeRef"
	logicalContainerNetworkRefField = ".spec.container.networks.networkRef"

	logicalContainerPreparationPendingReason apiv2.ConditionReason = "PhysicalResourcePreparationPending"
)

var (
	logicalContainerFinalizer    = fmt.Sprintf("%s/logicalcontainer-reconciler", apiv2.GroupVersion.Group)
	logicalContainerDataHandlers = map[logicalContainerState]logicalContainerDataHandlerFunc{
		logicalContainerStateNamespace: handleLogicalContainerNamespace,
		logicalContainerStateLifecycle: handleLogicalContainerLifecycle,
		logicalContainerStateLookup:    handleLogicalContainerLookup,
		logicalContainerStatePhysical:  handleLogicalContainerPhysical,
		logicalContainerStateRuntime:   handleLogicalContainerRuntime,
		logicalContainerStateDelete:    handleLogicalContainerDeleting,
		logicalContainerStateInvalid:   handleUnknownLogicalContainerState,
		0:                              handleUnknownLogicalContainerState,
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
	operationQueue     *resiliency.WorkQueue
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
		operationQueue:     resiliency.NewWorkQueue(lifetimeCtx, MaxConcurrentReconciles),
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

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.LogicalContainer{}).
		Owns(&apiv2.PhysicalContainer{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Owns(&apiv2.PhysicalContainerImage{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.PhysicalContainerImage{}, handler.EnqueueRequestsFromMapFunc(r.mapReferencedResourceToReconcileRequests(logicalContainerImageRefField)), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.PhysicalContainerVolume{}, handler.EnqueueRequestsFromMapFunc(r.mapReferencedResourceToReconcileRequests(logicalContainerVolumeRefField)), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.LogicalContainerNetwork{}, handler.EnqueueRequestsFromMapFunc(r.mapReferencedResourceToReconcileRequests(logicalContainerNetworkRefField)), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.LogicalContainerList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
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
			r.discardLogicalContainerData(req.NamespacedName, log)
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}
		log.Error(getErr, "Failed to Get() the LogicalContainer")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)
	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, container.GetResourceId())

	stateKey, previousData := r.containerData.BorrowByNamespacedName(req.NamespacedName)
	if previousData != nil && stateKey != container.UID {
		r.discardLogicalContainerData(req.NamespacedName, log)
	}
	r.containerData.RunDeferredOps(req.NamespacedName, &container)
	stateKey, data := r.containerData.BorrowByNamespacedName(container.NamespacedName())
	if data == nil {
		stateKey = container.UID
		data = &logicalContainerData{
			state: logicalContainerStateNamespace, progress: physicalResourceProgressNotReady,
		}
		r.containerData.Store(container.NamespacedName(), stateKey, data.Clone())
	}
	deleting := container.DeletionTimestamp != nil && !container.DeletionTimestamp.IsZero()
	if deleting && data.operation != nil && data.state != logicalContainerStateDelete {
		data.operation.cancel()
	}

	patch := ctrl_client.MergeFromWithOptions(container.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	var change objectChange
	delay := StandardDelay
	finalizerAdded := false
	if deleting && data.operation == nil {
		change = r.handleDeletionRequest(ctx, &container, data, log)
	} else if change = ensureFinalizer(&container, logicalContainerFinalizer, log); change != noChange {
		finalizerAdded = true
	} else {
		change = r.manageLogicalContainer(ctx, &container, data, log)
	}
	if !finalizerAdded && hasFinalizer(&container, logicalContainerFinalizer) {
		_ = r.containerData.Update(container.NamespacedName(), stateKey, data)
		dataChange, projectionDelay, valid := data.applyTo(&container)
		change |= dataChange
		delay = projectionDelay
		if !valid {
			log.Error(fmt.Errorf("invalid logical container state %v with progress %v", data.state, data.progress),
				"LogicalContainer reached invalid reconciliation state")
		}
	}
	if !hasFinalizer(&container, logicalContainerFinalizer) {
		defer logger.ReleaseResourceLog(container.GetResourceId())
	}
	return r.SaveChangesWithDelay(ctx, &container, patch, change, delay, nil, log)
}

func (r *LogicalContainerReconciler) manageLogicalContainer(
	ctx context.Context,
	container *apiv2.LogicalContainer,
	data *logicalContainerData,
	log logr.Logger,
) objectChange {
	handler := getStateInitializer(logicalContainerDataHandlers, data.state, log)
	return handler(ctx, r, container, data.state, data, log)
}

func handleLogicalContainerNamespace(
	ctx context.Context, reconciler *LogicalContainerReconciler, container *apiv2.LogicalContainer,
	_ logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	if container.Spec.EffectiveMode() == apiv2.LogicalContainerModeSession && container.Spec.Stop {
		data.state = logicalContainerStateRuntime
		data.progress = physicalResourceProgressExited
		data.reason, data.message = "ContainerStopped", "Stop was requested before a runtime container was created."
		return noChange
	}
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, reconciler.Client, container.Namespace)
	if !namespaceReady {
		data.reason = namespaceReason
		data.message = namespaceReadinessMessage(container.Namespace, namespaceReason)
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
			log.Error(namespaceErr, "Failed to get namespace", "Namespace", container.Namespace)
			data.progress = physicalResourceProgressRetryPending
			data.reason = apiv2.PhysicalResourceReasonNamespaceLookupFailed
			data.message = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
		}
		return noChange
	}

	data.reason, data.message = "", ""
	if container.Spec.EffectiveMode() == apiv2.LogicalContainerModeExisting {
		return reconciler.scheduleLogicalContainerOperation(container, data, logicalContainerStateLookup, reconciler.lookupLogicalContainer, log)
	}
	return reconciler.scheduleLogicalContainerOperation(container, data, logicalContainerStateLifecycle, reconciler.prepareLogicalContainerLifecycle, log)
}

func handleLogicalContainerLifecycle(
	_ context.Context, reconciler *LogicalContainerReconciler, container *apiv2.LogicalContainer,
	_ logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalResourceProgressInProgress:
		return noChange
	case physicalResourceProgressFailed:
		if data.lease != nil {
			return reconciler.scheduleLogicalContainerLeaseSettlement(container, data, nil, log)
		}
		return noChange
	case physicalResourceProgressCompleted:
		if container.Spec.EffectiveMode() != apiv2.LogicalContainerModeSession {
			return reconciler.scheduleLogicalContainerOperation(container, data, logicalContainerStateLookup, reconciler.lookupLogicalContainer, log)
		}
		data.state, data.progress = logicalContainerStatePhysical, physicalResourceProgressNotReady
		data.reason, data.message = "", ""
		return additionalReconciliationNeeded
	default:
		if time.Now().Before(data.retryAfter) {
			return additionalReconciliationNeeded
		}
		return reconciler.scheduleLogicalContainerOperation(container, data, logicalContainerStateLifecycle, reconciler.prepareLogicalContainerLifecycle, log)
	}
}

func handleLogicalContainerLookup(
	_ context.Context, reconciler *LogicalContainerReconciler, container *apiv2.LogicalContainer,
	_ logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalResourceProgressInProgress:
		return noChange
	case physicalResourceProgressCompleted:
		data.state, data.progress = logicalContainerStatePhysical, physicalResourceProgressNotReady
		data.reason, data.message = "", ""
		return additionalReconciliationNeeded
	default:
		if time.Now().Before(data.retryAfter) {
			return additionalReconciliationNeeded
		}
		return reconciler.scheduleLogicalContainerOperation(container, data, logicalContainerStateLookup, reconciler.lookupLogicalContainer, log)
	}
}

func handleLogicalContainerPhysical(
	ctx context.Context, reconciler *LogicalContainerReconciler, container *apiv2.LogicalContainer,
	_ logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	if data.operation != nil {
		return noChange
	}
	if data.progress == physicalResourceProgressFailed {
		if data.lease != nil {
			return reconciler.scheduleLogicalContainerLeaseSettlement(container, data, nil, log)
		}
		return noChange
	}
	// Stop can change after lookup; preserve the found identity until physical realization begins.
	if container.Spec.Stop && data.pendingContainerID != "" {
		data.lifecycleKey = data.existingLifecycleKey
		data.replaceExisting = false
	}
	if data.pendingContainerID == "" && container.Spec.Stop {
		data.state, data.progress = logicalContainerStateRuntime, physicalResourceProgressExited
		data.reason, data.message = "ContainerStopped", "Stop was requested before a runtime container was created."
		if data.lease != nil {
			return reconciler.scheduleLogicalContainerLeaseSettlement(container, data, nil, log)
		}
		return noChange
	}
	data.progress, data.reason, data.message = physicalResourceProgressNotReady, "", ""
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, reconciler.Client, container.Namespace)
	if !namespaceReady {
		data.reason = namespaceReason
		data.message = namespaceReadinessMessage(container.Namespace, namespaceReason)
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get namespace before preparing physical resources")
			data.progress = physicalResourceProgressRetryPending
			data.message = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
		}
		return noChange
	}
	selectedContainerID := data.pendingContainerID
	if data.replaceExisting {
		selectedContainerID = ""
	}
	if data.lease != nil {
		verifyErr := reconciler.config.StateStore.VerifyResourceLeaseHeld(ctx, container, reconciler.config.ResourceLeaseOwner)
		if verifyErr != nil {
			log.Error(verifyErr, "Failed to verify persistent container startup lease")
			data.progress, data.reason = physicalResourceProgressFailed, "ResourceLeaseLost"
			data.message = fmt.Sprintf("Persistent container startup lease was lost: %v", verifyErr)
			return noChange
		}
	}

	physicalName := "logical-" + string(container.UID)
	change := noChange
	physicalSpec := apiv2.PhysicalContainerSpec{ContainerID: selectedContainerID, Stop: container.Spec.Stop}
	if selectedContainerID == "" {
		imageRef, imageErr := reconciler.ensureLogicalContainerImage(ctx, container, physicalName)
		if imageErr != nil {
			log.Error(imageErr, "Failed to prepare logical container image")
			data.progress = physicalResourceProgressRetryPending
			data.message = imageErr.Error()
			return noChange
		}
		image, imageChange := reconciler.observeLogicalContainerImage(ctx, container, imageRef, log)
		change |= imageChange
		networksReady, physicalNetworks, networkMessage, resolveErr := reconciler.resolveLogicalContainerNetworks(ctx, container)
		if !networksReady {
			if image != nil && image.Status.Phase == apiv2.PhysicalContainerImagePhaseFailed {
				imageReady := apimeta.FindStatusCondition(image.Status.Conditions, string(apiv2.ConditionReady))
				if imageReady != nil && imageReady.ObservedGeneration == image.Generation {
					data.progress, data.reason = physicalResourceProgressFailed, apiv2.PhysicalContainerReasonImageFailed
					data.message = fmt.Sprintf("PhysicalContainerImage %q failed: %s: %s", imageRef, imageReady.Reason, imageReady.Message)
					if data.lease != nil {
						return change | reconciler.scheduleLogicalContainerLeaseSettlement(container, data, nil, log)
					}
					return change
				}
			}
			if resolveErr != nil {
				log.Error(resolveErr, "Failed to resolve logical container networks")
				data.progress = physicalResourceProgressRetryPending
				data.message = resolveErr.Error()
				return change
			}
			data.message = networkMessage
			return change
		}
		desired := container.Spec.Container.DeepCopy()
		labels := setRuntimeLabel(desired.Labels, dcpBuildLabel, version.ProductVersion)
		labels = setRuntimeLabel(labels, groupVersionLabel, apiv2.GroupVersion.String())
		labels = setRuntimeLabel(labels, nameLabel, container.Name)
		labels = setRuntimeLabel(labels, lifecycleKeyLabel, data.lifecycleKey)
		containerName := container.Spec.ContainerName
		if containerName == "" {
			containerName = data.generatedContainerName
		}
		physicalSpec.Container = &apiv2.PhysicalContainerConfig{
			RetainRuntimeContainer: container.Spec.EffectiveMode() == apiv2.LogicalContainerModePersistent,
			MonitorPID:             container.Spec.MonitorPID, MonitorTimestamp: container.Spec.MonitorTimestamp,
			ImageRef: imageRef, ContainerName: containerName, ReplaceExisting: data.replaceExisting,
			Entrypoint: desired.Entrypoint, Command: desired.Command, Env: desired.Env,
			Ports: desired.Ports, VolumeMounts: desired.VolumeMounts, CreateFiles: desired.CreateFiles, Labels: labels,
			Networks: physicalNetworks,
		}
	}
	physicalContainer := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: physicalName, Namespace: container.Namespace},
		Spec:       physicalSpec,
	}
	ownerErr := ctrl.SetControllerReference(container, physicalContainer, reconciler.Scheme())
	if ownerErr != nil {
		log.Error(ownerErr, "Failed to set physical container owner")
		data.progress = physicalResourceProgressFailed
		data.message = fmt.Sprintf("Failed to set physical container owner: %v", ownerErr)
		return change
	}
	createErr := reconciler.Create(ctx, physicalContainer)
	if apierrors.IsAlreadyExists(createErr) {
		getErr := reconciler.NoCacheClient.Get(ctx, physicalContainer.NamespacedName(), physicalContainer)
		if getErr != nil {
			createErr = getErr
		} else {
			actualSpec := physicalContainer.Spec
			actualSpec.Stop, physicalSpec.Stop = false, false
			if !metav1.IsControlledBy(physicalContainer, container) || !reflect.DeepEqual(actualSpec, physicalSpec) {
				createErr = fmt.Errorf("physical container %q has a different owner or spec", physicalContainer.NamespacedName())
				data.progress = physicalResourceProgressFailed
			} else {
				createErr = nil
			}
		}
	}
	if createErr != nil {
		log.Error(createErr, "Failed to prepare backing physical container")
		if data.progress != physicalResourceProgressFailed {
			data.progress = physicalResourceProgressRetryPending
		}
		data.message = fmt.Sprintf("Failed to prepare backing physical container: %v", createErr)
		return change
	}
	data.physicalContainerUID = physicalContainer.UID
	data.pendingContainerID = ""
	data.generatedContainerName = ""
	data.existingLifecycleKey = ""
	data.replaceExisting = false
	data.state, data.progress = logicalContainerStateRuntime, physicalResourceProgressNotReady
	return change | handleLogicalContainerRuntime(ctx, reconciler, container, data.state, data, log)
}

func (r *LogicalContainerReconciler) resolveLogicalContainerNetworks(
	ctx context.Context, container *apiv2.LogicalContainer,
) (bool, []apiv2.ContainerNetworkConnectionConfig, string, error) {
	var resolved []apiv2.ContainerNetworkConnectionConfig
	for _, requested := range container.Spec.Container.Networks {
		network := &apiv2.LogicalContainerNetwork{}
		networkName := commonapi.AsNamespacedName(requested.NetworkRef, container.Namespace)
		getErr := r.Get(ctx, networkName, network)
		if apierrors.IsNotFound(getErr) {
			return false, nil, fmt.Sprintf("LogicalContainerNetwork %q does not exist.", networkName), nil
		}
		if getErr != nil {
			return false, nil, "", fmt.Errorf("get LogicalContainerNetwork %q: %w", networkName, getErr)
		}
		if !network.DeletionTimestamp.IsZero() {
			return false, nil, fmt.Sprintf("LogicalContainerNetwork %q is terminating.", networkName), nil
		}
		ready := apimeta.FindStatusCondition(network.Status.Conditions, string(apiv2.ConditionReady))
		if network.Status.Phase != apiv2.LogicalContainerNetworkPhaseReady ||
			ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != network.Generation {
			message := fmt.Sprintf("LogicalContainerNetwork %q is not ready.", networkName)
			if ready != nil {
				message = fmt.Sprintf("LogicalContainerNetwork %q reports %s: %s", networkName, ready.Reason, ready.Message)
			}
			return false, nil, message, nil
		}
		if network.Status.PhysicalNetworkRef == "" {
			return false, nil, fmt.Sprintf("LogicalContainerNetwork %q has not published its backing physical network reference.", networkName), nil
		}
		resolved = append(resolved, apiv2.ContainerNetworkConnectionConfig{
			NetworkRef: network.Status.PhysicalNetworkRef, Aliases: std_slices.Clone(requested.Aliases),
		})
	}
	return true, resolved, "", nil
}

func (r *LogicalContainerReconciler) ensureLogicalContainerImage(
	ctx context.Context, container *apiv2.LogicalContainer, physicalName string,
) (string, error) {
	if container.Spec.Container.ImageRef != "" {
		return commonapi.AsNamespacedName(container.Spec.Container.ImageRef, container.Namespace).Name, nil
	}
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{Name: physicalName, Namespace: container.Namespace},
		Spec:       apiv2.PhysicalContainerImageSpec{Image: container.Spec.Container.Image.DeepCopy()},
	}
	ownerErr := ctrl.SetControllerReference(container, image, r.Scheme())
	if ownerErr != nil {
		return "", fmt.Errorf("set physical image owner: %w", ownerErr)
	}
	expectedSpec := image.Spec.DeepCopy()
	createErr := r.Create(ctx, image)
	if apierrors.IsAlreadyExists(createErr) {
		getErr := r.NoCacheClient.Get(ctx, image.NamespacedName(), image)
		if getErr != nil {
			return "", fmt.Errorf("get backing physical image: %w", getErr)
		}
		if !metav1.IsControlledBy(image, container) || !reflect.DeepEqual(image.Spec, *expectedSpec) {
			return "", fmt.Errorf("physical image %q has a different owner or spec", image.NamespacedName())
		}
	} else if createErr != nil {
		return "", fmt.Errorf("create backing physical image: %w", createErr)
	}
	return image.Name, nil
}

func handleLogicalContainerRuntime(
	ctx context.Context, reconciler *LogicalContainerReconciler, container *apiv2.LogicalContainer,
	_ logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	if data.physicalContainerUID == "" && container.Spec.Stop {
		data.progress = physicalResourceProgressExited
		data.reason, data.message = "ContainerStopped", "Stop was requested and no runtime container exists."
		if data.lease != nil {
			return reconciler.scheduleLogicalContainerLeaseSettlement(container, data, nil, log)
		}
		return noChange
	}
	physicalName := types.NamespacedName{Namespace: container.Namespace, Name: "logical-" + string(container.UID)}
	physicalContainer := &apiv2.PhysicalContainer{}
	getErr := reconciler.Get(ctx, physicalName, physicalContainer)
	if apierrors.IsNotFound(getErr) {
		getErr = reconciler.NoCacheClient.Get(ctx, physicalName, physicalContainer)
	}
	if apierrors.IsNotFound(getErr) {
		data.progress, data.reason = physicalResourceProgressMissing, ""
		data.message = "The backing PhysicalContainer no longer exists."
		change := applyLogicalContainerPhysicalDetails(container, &apiv2.PhysicalContainerStatus{})
		runtimeLogs := &apiv2.LogSource{
			Name: physicalName.Name, UID: data.physicalContainerUID, Complete: true,
		}
		if !reflect.DeepEqual(container.Status.RuntimeLogs, runtimeLogs) {
			container.Status.RuntimeLogs = runtimeLogs
			change |= statusChanged
		}
		if data.lease != nil {
			return change | reconciler.scheduleLogicalContainerLeaseSettlement(container, data, nil, log)
		}
		return change
	}
	if getErr != nil {
		log.Error(getErr, "Failed to get backing physical container")
		data.progress, data.reason = physicalResourceProgressRetryPending, ""
		data.message = fmt.Sprintf("Failed to get backing PhysicalContainer: %v", getErr)
		return noChange
	}
	if physicalContainer.UID != data.physicalContainerUID || !metav1.IsControlledBy(physicalContainer, container) {
		log.Error(fmt.Errorf("physical container %q identity or owner changed", physicalContainer.NamespacedName()),
			"LogicalContainer backing physical resource changed")
		data.progress, data.reason = physicalResourceProgressFailed, "PhysicalContainerChanged"
		data.message = "The backing PhysicalContainer identity or owner changed."
		if data.lease != nil {
			return reconciler.scheduleLogicalContainerLeaseSettlement(container, data, nil, log)
		}
		return noChange
	}
	change := applyLogicalContainerPhysicalDetails(container, &physicalContainer.Status)
	runtimeLogs := physicalContainer.GetLogSource()
	if !reflect.DeepEqual(container.Status.RuntimeLogs, runtimeLogs) {
		container.Status.RuntimeLogs = runtimeLogs
		change |= statusChanged
	}
	imageRefForLogs := ""
	if physicalContainer.Spec.Container != nil {
		imageRefForLogs = physicalContainer.Spec.Container.ImageRef
	}
	_, imageChange := reconciler.observeLogicalContainerImage(ctx, container, imageRefForLogs, log)
	change |= imageChange
	if container.Spec.Container != nil && container.Spec.Container.Image != nil {
		imageRef := ""
		if physicalContainer.Spec.Container != nil {
			imageRef = physicalContainer.Spec.Container.ImageRef
		}
		change |= setValue(&container.Status.ImageRef, imageRef)
	}
	if !physicalContainer.DeletionTimestamp.IsZero() {
		data.progress, data.reason = physicalResourceProgressUnknown, "PhysicalContainerTerminating"
		data.message = "The backing PhysicalContainer is being deleted."
		return change
	}
	if container.Spec.Stop && !physicalContainer.Spec.Stop {
		physicalPatch := ctrl_client.MergeFromWithOptions(physicalContainer.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
		physicalContainer.Spec.Stop = true
		stopErr := reconciler.Patch(ctx, physicalContainer, physicalPatch)
		if stopErr != nil {
			log.Error(stopErr, "Failed to forward logical container stop request")
			data.progress, data.reason = physicalResourceProgressRetryPending, "StopRequestFailed"
			data.message = fmt.Sprintf("Failed to forward stop request: %v", stopErr)
			return change
		}
		data.progress, data.reason = physicalResourceProgressNotReady, "ContainerStopping"
		data.message = "Waiting for the physical controller to stop the runtime container."
		return change | additionalReconciliationNeeded
	}

	data.reason, data.message = "", ""
	ready := apimeta.FindStatusCondition(physicalContainer.Status.Conditions, string(apiv2.ConditionReady))
	if ready == nil || ready.ObservedGeneration != physicalContainer.Generation {
		data.progress = physicalResourceProgressNotReady
		data.message = "Waiting for the physical controller to observe the desired container configuration."
		return change
	}
	switch physicalContainer.Status.Phase {
	case apiv2.PhysicalContainerPhaseRunning:
		data.progress = physicalResourceProgressRunning
		if ready.Status != metav1.ConditionTrue || container.Spec.Stop {
			data.progress = physicalResourceProgressNotReady
		}
	case apiv2.PhysicalContainerPhasePaused:
		data.progress = physicalResourceProgressPaused
	case apiv2.PhysicalContainerPhaseExited:
		data.progress = physicalResourceProgressExited
		if container.Spec.Stop {
			data.reason = "ContainerStopped"
		}
	case apiv2.PhysicalContainerPhaseFailed:
		data.progress = physicalResourceProgressFailed
	case apiv2.PhysicalContainerPhaseUnknown:
		data.progress = physicalResourceProgressUnknown
	default:
		data.progress = physicalResourceProgressNotReady
	}
	if data.progress != physicalResourceProgressRunning {
		data.message = fmt.Sprintf("PhysicalContainer reports %s: %s", ready.Reason, ready.Message)
	}
	settled := data.progress == physicalResourceProgressRunning ||
		data.progress == physicalResourceProgressPaused ||
		data.progress == physicalResourceProgressExited ||
		data.progress == physicalResourceProgressFailed
	if data.lease != nil && settled {
		var record *statestore.PersistentContainerRecord
		if data.progress != physicalResourceProgressFailed && physicalContainer.Status.ContainerID != "" {
			record = &statestore.PersistentContainerRecord{
				ResourceKey: container.GetLeaseKey(), ContainerID: physicalContainer.Status.ContainerID,
				ContainerName: physicalContainer.Status.ContainerName, RuntimeName: reconciler.runtimeName,
			}
		}
		return change | reconciler.scheduleLogicalContainerLeaseSettlement(container, data, record, log)
	}
	return change
}

func applyLogicalContainerPhysicalDetails(
	container *apiv2.LogicalContainer, physicalStatus *apiv2.PhysicalContainerStatus,
) objectChange {
	change := setValue(&container.Status.ContainerID, physicalStatus.ContainerID)
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
	return change
}

func (r *LogicalContainerReconciler) observeLogicalContainerImage(
	ctx context.Context, container *apiv2.LogicalContainer, imageRef string, log logr.Logger,
) (*apiv2.PhysicalContainerImage, objectChange) {
	var image *apiv2.PhysicalContainerImage
	var imageLogs *apiv2.LogSource
	if imageRef != "" {
		image = &apiv2.PhysicalContainerImage{}
		imageName := commonapi.AsNamespacedName(imageRef, container.Namespace)
		getErr := r.Get(ctx, imageName, image)
		if apierrors.IsNotFound(getErr) {
			getErr = r.NoCacheClient.Get(ctx, imageName, image)
		}
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			log.Error(getErr, "Failed to observe image startup logs", "Image", imageName)
			return nil, additionalReconciliationNeeded
		}
		if getErr == nil {
			imageLogs = image.GetLogSource()
		} else {
			image = nil
		}
	}
	if !reflect.DeepEqual(container.Status.StartupLogs, imageLogs) {
		container.Status.StartupLogs = imageLogs
		return image, statusChanged
	}
	return image, noChange
}

func handleUnknownLogicalContainerState(
	_ context.Context, _ *LogicalContainerReconciler, _ *apiv2.LogicalContainer,
	state logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	log.Error(fmt.Errorf("unknown logical container state %v with progress %v", state, data.progress),
		"LogicalContainer has an invalid reconciliation state")
	data.state, data.progress = logicalContainerStateInvalid, physicalResourceProgressFailed
	data.reason = apiv2.PhysicalResourceReasonOperationStateInvalid
	data.message = "Logical container reached an invalid reconciliation state."
	return noChange
}

func handleLogicalContainerDeleting(
	ctx context.Context, reconciler *LogicalContainerReconciler, container *apiv2.LogicalContainer,
	state logicalContainerState, data *logicalContainerData, log logr.Logger,
) objectChange {
	if data.operation == nil {
		return handleUnknownLogicalContainerState(ctx, reconciler, container, state, data, log)
	}
	return additionalReconciliationNeeded
}

func (r *LogicalContainerReconciler) handleDeletionRequest(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData, log logr.Logger,
) objectChange {
	data.state, data.progress = logicalContainerStateDelete, physicalResourceProgressNotReady
	data.reason, data.message = "", ""
	physicalName := "logical-" + string(container.UID)
	physicalContainer := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: physicalName, Namespace: container.Namespace}}
	deleted, deleteErr := r.deleteLogicalContainerPhysicalResource(ctx, container, physicalContainer)
	if deleteErr == nil && deleted && container.Spec.Container != nil && container.Spec.Container.Image != nil {
		image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: physicalName, Namespace: container.Namespace}}
		deleted, deleteErr = r.deleteLogicalContainerPhysicalResource(ctx, container, image)
	}
	if deleteErr != nil {
		log.Error(deleteErr, "Failed to delete logical container physical resources")
		data.progress = physicalResourceProgressRetryPending
		data.message = deleteErr.Error()
		return noChange
	}
	if !deleted {
		return additionalReconciliationNeeded
	}
	if data.lease != nil {
		return r.scheduleLogicalContainerLeaseSettlement(container, data, nil, log)
	}
	r.containerData.DeleteByNamespacedName(container.NamespacedName())
	return deleteFinalizer(container, logicalContainerFinalizer, log)
}

func (r *LogicalContainerReconciler) deleteLogicalContainerPhysicalResource(
	ctx context.Context, container *apiv2.LogicalContainer, resource ctrl_client.Object,
) (bool, error) {
	getErr := r.NoCacheClient.Get(ctx, ctrl_client.ObjectKeyFromObject(resource), resource)
	if apierrors.IsNotFound(getErr) {
		return true, nil
	}
	if getErr != nil {
		return false, fmt.Errorf("get backing physical resource: %w", getErr)
	}
	if !metav1.IsControlledBy(resource, container) {
		return false, fmt.Errorf("backing physical resource %q has a different owner", ctrl_client.ObjectKeyFromObject(resource))
	}
	if resource.GetDeletionTimestamp().IsZero() {
		uid := resource.GetUID()
		deleteErr := r.Delete(ctx, resource, ctrl_client.Preconditions{UID: &uid})
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return false, fmt.Errorf("delete backing physical resource: %w", deleteErr)
		}
	}
	return false, nil
}

func (r *LogicalContainerReconciler) discardLogicalContainerData(name types.NamespacedName, log logr.Logger) {
	resourceUID, data := r.containerData.BorrowByNamespacedName(name)
	if data == nil {
		return
	}
	discarded := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{UID: resourceUID}}
	defer logger.ReleaseResourceLog(discarded.GetResourceId())
	if data.operation != nil {
		data.operation.cancel()
	}
	r.containerData.DeleteByNamespacedName(name)
	if data.lease != nil {
		enqueueErr := r.operationQueue.Enqueue(func(cleanupCtx context.Context) {
			releaseErr := data.lease.Release(cleanupCtx)
			if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
				log.Error(releaseErr, "Failed to release discarded logical container lease")
			}
		})
		if enqueueErr != nil {
			log.Error(enqueueErr, "Failed to queue discarded logical container lease release")
		}
	}
}

func (r *LogicalContainerReconciler) scheduleLogicalContainerOperation(
	container *apiv2.LogicalContainer,
	currentData *logicalContainerData,
	state logicalContainerState,
	work func(context.Context, *apiv2.LogicalContainer, *logicalContainerData, logr.Logger),
	log logr.Logger,
) objectChange {
	operationCtx, cancelOperation := context.WithCancel(r.LifetimeCtx)
	data := currentData.Clone()
	if state == logicalContainerStatePersistence {
		data.leaseFailureMessage = ""
	} else {
		data.state, data.progress = state, physicalResourceProgressInProgress
		data.reason, data.message = "", ""
	}
	data.retryAfter = time.Time{}
	data.operation = &logicalContainerOperation{cancel: cancelOperation}
	if !r.containerData.Update(container.NamespacedName(), container.UID, data) {
		cancelOperation()
		return additionalReconciliationNeeded
	}
	currentData.UpdateFrom(data)
	snapshot := container.DeepCopy()
	result := data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(context.Context) {
		defer cancelOperation()
		if operationCtx.Err() != nil {
			if state == logicalContainerStatePersistence {
				result.leaseFailureMessage = operationCtx.Err().Error()
			} else {
				result.progress = physicalResourceProgressRetryPending
				result.message = operationCtx.Err().Error()
			}
		} else {
			work(operationCtx, snapshot, result, log)
		}
		operation := result.operation
		result.operation = nil
		queued := r.containerData.QueueDeferredOpForStateKey(snapshot.NamespacedName(), snapshot.UID,
			func(name types.NamespacedName, stateKey types.UID, current *apiv2.LogicalContainer) {
				_, active := r.containerData.BorrowByNamespacedName(name)
				if current.UID == snapshot.UID && active != nil && active.operation == operation {
					if state == logicalContainerStatePersistence {
						active.operation = nil
						active.lease = result.lease
						active.leaseFailureMessage = result.leaseFailureMessage
						active.retryAfter = result.retryAfter
						_ = r.containerData.Update(name, stateKey, active)
					} else {
						_ = r.containerData.Update(name, stateKey, result)
					}
				}
			})
		if queued {
			r.ScheduleReconciliation(snapshot.NamespacedName())
		} else if result.lease != nil {
			releaseErr := result.lease.Release(r.LifetimeCtx)
			if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
				log.Error(releaseErr, "Failed to release stale logical container operation lease")
			}
		}
	})
	if enqueueErr != nil {
		cancelOperation()
		log.Error(enqueueErr, "Failed to queue logical container operation", "State", state)
		data.operation = nil
		if state == logicalContainerStatePersistence {
			data.leaseFailureMessage = fmt.Sprintf("Failed to queue persistent container lease settlement: %v", enqueueErr)
		} else {
			data.progress = physicalResourceProgressRetryPending
			data.message = fmt.Sprintf("Failed to queue logical container operation: %v", enqueueErr)
		}
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		currentData.UpdateFrom(data)
	}
	return noChange
}

func (r *LogicalContainerReconciler) prepareLogicalContainerLifecycle(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData, log logr.Logger,
) {
	if container.Spec.ContainerName == "" && data.generatedContainerName == "" {
		generatedName, _, nameErr := MakeUniqueName(container.Name)
		if nameErr != nil {
			log.Error(nameErr, "Failed to generate logical container runtime name")
			data.progress = physicalResourceProgressRetryPending
			data.message = fmt.Sprintf("Failed to generate container name: %v", nameErr)
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			return
		}
		data.generatedContainerName = generatedName
	}
	if !container.Spec.Stop {
		lifecycleKey, _, keyErr := container.Spec.GetLifecycleKey()
		if keyErr != nil {
			log.Error(keyErr, "Failed to calculate logical container lifecycle key")
			data.progress = physicalResourceProgressFailed
			data.reason = "LifecycleKeyCalculationFailed"
			data.message = fmt.Sprintf("Failed to calculate lifecycle key: %v", keyErr)
			return
		}
		data.lifecycleKey = lifecycleKey
	}
	if container.Spec.EffectiveMode() == apiv2.LogicalContainerModePersistent && data.lease == nil {
		if r.config.StateStore == nil {
			missingStoreErr := errors.New("state store is not configured")
			log.Error(missingStoreErr, "Cannot prepare persistent logical container")
			data.progress, data.reason = physicalResourceProgressFailed, "StateStoreNotConfigured"
			data.message = missingStoreErr.Error()
			return
		}
		lease, leaseErr := r.config.StateStore.AcquireResourceLease(ctx, container, r.config.ResourceLeaseOwner, resourceLeaseRevalidationInterval)
		if leaseErr != nil {
			data.progress = physicalResourceProgressRetryPending
			data.reason = "ResourceLeaseAcquisitionFailed"
			data.message = fmt.Sprintf("Failed to acquire persistent container startup lease: %v", leaseErr)
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			if errors.Is(leaseErr, statestore.ErrResourceLeaseHeld) {
				logResourceLeaseHeld(log, leaseErr, container.GetLeaseKey(), "Persistent container is being updated by another DCP instance, retrying")
				data.reason = "ResourceLeaseHeld"
			} else {
				log.Error(leaseErr, "Failed to acquire persistent logical container lease")
			}
			return
		}
		data.lease = lease
	}
	data.progress = physicalResourceProgressCompleted
}

func (r *LogicalContainerReconciler) lookupLogicalContainer(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData, log logr.Logger,
) {
	inspected, inspectErr := inspectContainerIfExists(ctx, r.containerInspector, container.Spec.ContainerName)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		data.pendingContainerID = ""
		data.existingLifecycleKey = ""
		data.replaceExisting = false
		if container.Spec.EffectiveMode() == apiv2.LogicalContainerModeExisting && !container.Spec.Stop {
			data.progress = physicalResourceProgressNotFound
			data.message = fmt.Sprintf("Runtime container %q was not found.", container.Spec.ContainerName)
			data.retryAfter = time.Now().Add(delayDurations[MonitoringDelay].Duration)
		} else {
			data.progress = physicalResourceProgressCompleted
		}
		return
	}
	if inspectErr != nil || inspected == nil || inspected.Id == "" {
		if inspectErr == nil {
			inspectErr = errors.New("container inspection returned no runtime identity")
		}
		log.Error(inspectErr, "Failed to inspect logical container by name")
		data.progress = physicalResourceProgressRetryPending
		data.message = fmt.Sprintf("Failed to look up runtime container: %v", inspectErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return
	}

	_, managed := inspected.Labels[dcpBuildLabel]
	actualKey := inspected.Labels[lifecycleKeyLabel]
	data.pendingContainerID, data.existingLifecycleKey = inspected.Id, actualKey
	reusable := container.Spec.EffectiveMode() == apiv2.LogicalContainerModeExisting ||
		container.Spec.Stop || !managed ||
		(actualKey == data.lifecycleKey && inspected.Status == containers.ContainerStatusRunning)
	if reusable {
		data.lifecycleKey = actualKey
		data.replaceExisting = false
		log.V(1).Info("Reusing existing runtime container", "ContainerID", GetShortId(inspected.Id))
	} else {
		data.replaceExisting = true
		log.Info("Delegating persistent container replacement to the physical controller",
			"ContainerID", GetShortId(inspected.Id), "LifecycleKeyChanged", actualKey != data.lifecycleKey,
			"RuntimeStatus", inspected.Status)
	}
	data.progress = physicalResourceProgressCompleted
}

func (r *LogicalContainerReconciler) scheduleLogicalContainerLeaseSettlement(
	container *apiv2.LogicalContainer, data *logicalContainerData, record *statestore.PersistentContainerRecord, log logr.Logger,
) objectChange {
	if data.operation != nil {
		return noChange
	}
	if data.leaseFailureMessage != "" && time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}
	return r.scheduleLogicalContainerOperation(container, data, logicalContainerStatePersistence,
		func(ctx context.Context, snapshot *apiv2.LogicalContainer, result *logicalContainerData, operationLog logr.Logger) {
			r.persistLogicalContainer(ctx, snapshot, result, record, operationLog)
		}, log)
}

func (r *LogicalContainerReconciler) persistLogicalContainer(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData,
	record *statestore.PersistentContainerRecord, log logr.Logger,
) {
	if data.lease == nil {
		return
	}
	if record != nil {
		workloadID, workloadErr := dcpclient.ResolveNamespaceWorkloadID(ctx, r.NoCacheClient, container.Namespace, r.config.WorkloadID)
		if workloadErr != nil {
			log.Error(workloadErr, "Failed to resolve persistent logical container workload ID")
			data.leaseFailureMessage = fmt.Sprintf("Failed to resolve persistent container workload ID: %v", workloadErr)
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			return
		}
		if workloadID != "" {
			record.WorkloadID = workloadID
			verifyErr := r.config.StateStore.VerifyResourceLeaseHeld(ctx, container, r.config.ResourceLeaseOwner)
			if verifyErr != nil {
				log.Error(verifyErr, "Failed to verify persistent logical container lease before recording ownership")
				data.leaseFailureMessage = fmt.Sprintf("Failed to verify persistent container lease: %v", verifyErr)
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				return
			}
			recordErr := r.config.StateStore.UpsertPersistentContainer(ctx, *record)
			if recordErr != nil {
				log.Error(recordErr, "Failed to record persistent logical container")
				data.leaseFailureMessage = fmt.Sprintf("Failed to record persistent container: %v", recordErr)
				data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				return
			}
		}
	}
	releaseErr := data.lease.Release(ctx)
	if releaseErr != nil && !errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
		log.Error(releaseErr, "Failed to release persistent logical container startup lease")
		data.leaseFailureMessage = fmt.Sprintf("Failed to release persistent container lease: %v", releaseErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return
	}
	data.lease = nil
}
