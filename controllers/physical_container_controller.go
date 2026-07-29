/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	std_slices "slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/slices"
)

const physicalContainerImageRefField = ".spec.imageRef"

var (
	physicalContainerFinalizer string = fmt.Sprintf("%s/physicalcontainer-reconciler", apiv2.GroupVersion.Group)

	physicalContainerDataInitializers = map[string]physicalContainerDataInitializerFunc{
		apiv2.PhysicalContainerReasonCreating:       handlePhysicalContainerCreating,
		apiv2.PhysicalContainerReasonCreated:        handlePhysicalContainerCreated,
		apiv2.PhysicalContainerReasonCopyingFiles:   handlePhysicalContainerOperationInProgress,
		apiv2.PhysicalContainerReasonFilesCreated:   handlePhysicalContainerFilesCreated,
		apiv2.PhysicalContainerReasonStarting:       handlePhysicalContainerOperationInProgress,
		apiv2.PhysicalContainerReasonStarted:        handlePhysicalContainerStarted,
		apiv2.PhysicalContainerReasonCreateFailed:   handlePhysicalContainerOperationFailed,
		apiv2.PhysicalContainerReasonFileCopyFailed: handlePhysicalContainerOperationFailed,
		apiv2.PhysicalContainerReasonStartFailed:    handlePhysicalContainerOperationFailed,
		"":                                          handleUnknownPhysicalContainerDataReason,
	}
)

type physicalContainerDataInitializerFunc = stateInitializerFunc[
	apiv2.PhysicalContainer, *apiv2.PhysicalContainer,
	PhysicalContainerReconciler, *PhysicalContainerReconciler,
	string,
	physicalContainerData, *physicalContainerData,
]

type PhysicalContainerReconciler struct {
	*ReconcilerBase[apiv2.PhysicalContainer, *apiv2.PhysicalContainer]
	*ContainerWatcher[apiv2.PhysicalContainer]

	orchestrator   containers.ContainerOrchestrator
	containerData  *ObjectStateMap[physicalContainerDataStateKey, physicalContainerData, *physicalContainerData, *apiv2.PhysicalContainer]
	operationQueue *resiliency.WorkQueue
}

func NewPhysicalContainerReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	orchestrator containers.ContainerOrchestrator,
) *PhysicalContainerReconciler {
	lock := &sync.Mutex{}
	reconciler := PhysicalContainerReconciler{
		ReconcilerBase:   NewReconcilerBase[apiv2.PhysicalContainer](client, noCacheClient, log, lifetimeCtx),
		ContainerWatcher: NewContainerWatcher[apiv2.PhysicalContainer](orchestrator, lock, lifetimeCtx),
		orchestrator:     orchestrator,
		containerData:    NewObjectStateMap[physicalContainerDataStateKey, physicalContainerData, *physicalContainerData, *apiv2.PhysicalContainer](),
		operationQueue:   resiliency.NewWorkQueue(lifetimeCtx, MaxConcurrentReconciles),
	}
	reconciler.ContainerWatcher.ProcessContainerEvent = reconciler.processContainerEvent
	return &reconciler
}

func (r *PhysicalContainerReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv2.PhysicalContainer{}, physicalContainerImageRefField, func(rawObj ctrl_client.Object) []string {
		container := rawObj.(*apiv2.PhysicalContainer)
		if container.Spec.ImageRef == "" {
			return nil
		}

		return []string{container.Spec.ImageRef}
	}); err != nil {
		r.Log.Error(err, "Failed to create imageRef index for PhysicalContainer", "IndexField", physicalContainerImageRefField)
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.PhysicalContainer{}).
		Watches(&apiv2.PhysicalContainerImage{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForImage), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *PhysicalContainerReconciler) requestReconcileForImage(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	image := obj.(*apiv2.PhysicalContainerImage)
	var containerList apiv2.PhysicalContainerList
	listErr := r.List(ctx, &containerList, ctrl_client.InNamespace(image.Namespace), ctrl_client.MatchingFields{physicalContainerImageRefField: image.Name})
	if listErr != nil {
		r.Log.Error(listErr, "Failed to list PhysicalContainers referencing PhysicalContainerImage", "Image", image.NamespacedName())
		return nil
	}

	requests := make([]reconcile.Request, 0, len(containerList.Items))
	for i := range containerList.Items {
		container := containerList.Items[i]
		requests = append(requests, reconcile.Request{NamespacedName: container.NamespacedName()})
	}

	r.Log.V(1).Info("PhysicalContainerImage updated, requesting PhysicalContainer reconciliation", "Image", image.NamespacedName(), "Containers", len(requests))
	return requests
}

func (r *PhysicalContainerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	container := apiv2.PhysicalContainer{}
	getErr := reader.Get(ctx, req.NamespacedName, &container)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			log.V(1).Info("PhysicalContainer not found, nothing to do...")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}

		log.Error(getErr, "Failed to Get() the PhysicalContainer")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)

	r.containerData.RunDeferredOps(req.NamespacedName, &container)

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(container.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if container.DeletionTimestamp != nil && !container.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, &container, log)
	} else if change = ensureFinalizer(&container, physicalContainerFinalizer, log); change != noChange {
		// Make additional changes during the next reconciliation.
	} else {
		change = r.managePhysicalContainer(ctx, &container, log)
	}

	// A running container is in a steady state and relies on runtime events for status changes.
	// Reconcile it on a slow cadence purely to recover from events that were missed or never delivered.
	additionalReconcileDelay := StandardDelay
	if container.Status.Phase == apiv2.PhysicalContainerPhaseRunning {
		additionalReconcileDelay = MonitoringDelay
	}

	return r.SaveChangesWithDelay(ctx, &container, patch, change, additionalReconcileDelay, nil, log)
}

func (r *PhysicalContainerReconciler) managePhysicalContainer(ctx context.Context, container *apiv2.PhysicalContainer, log logr.Logger) objectChange {
	if namespaceReady, change := ensureNamespace(ctx, r.Client, container.Namespace, func(message string) objectChange {
		change := setValue(&container.Status.Phase, apiv2.PhysicalContainerPhasePending)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonPending, message)
		return change
	}, func(message string) objectChange {
		change := setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonReconciliationFailed, message)
		return change
	}, log); !namespaceReady {
		return change
	}

	change := noChange
	_, data := r.containerData.BorrowByNamespacedName(container.NamespacedName())
	if data != nil {
		change |= data.applyTo(container)
		initializer := getStateInitializer(physicalContainerDataInitializers, data.conditionReason, log)
		change |= initializer(ctx, r, container, data.conditionReason, data, log)
		if data.conditionReason != apiv2.PhysicalContainerReasonStarted {
			return change
		}
	}

	containerID := container.Spec.ContainerID
	if containerID == "" {
		if data != nil && data.containerID != "" {
			containerID = data.containerID
		} else {
			containerID = container.Status.ContainerID
		}
	}

	if containerID == "" {
		imageReady, imageChange := r.resolvePhysicalContainerImage(ctx, container, log)
		if !imageReady {
			return imageChange
		}
		if imageChange != noChange {
			return imageChange | r.schedulePhysicalContainerCreate(container, log)
		}
		return r.schedulePhysicalContainerCreate(container, log)
	}
	if data == nil {
		storeStartedPhysicalContainerData(r.containerData, container, containerID)
	}
	r.ensurePhysicalContainerWatch(container, log)

	inspectedContainer, inspectErr := r.inspectPhysicalContainer(ctx, containerID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		change |= setValue(&container.Status.ContainerID, containerID)
		change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseMissing)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerMissing, "Runtime container was not found.")
		return change
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect runtime container", "ContainerID", containerID)
		change |= setValue(&container.Status.ContainerID, containerID)
		change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonReconciliationFailed, fmt.Sprintf("Failed to inspect runtime container: %v", inspectErr))
		return change
	}

	if container.Spec.Stop {
		stoppedContainer, stopErr := r.stopPhysicalContainerIfNecessary(ctx, containerID, inspectedContainer)
		if errors.Is(stopErr, containers.ErrNotFound) {
			change |= setValue(&container.Status.ContainerID, containerID)
			change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseMissing)
			change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerMissing, "Runtime container was not found.")
			return change
		}
		if stopErr != nil {
			log.Error(stopErr, "Failed to stop runtime container", "ContainerID", containerID)
			change |= setValue(&container.Status.ContainerID, containerID)
			change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
			change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonReconciliationFailed, fmt.Sprintf("Failed to stop runtime container: %v", stopErr))
			return change
		}
		inspectedContainer = stoppedContainer
	}

	return change | applyInspectedPhysicalContainerStatus(container, inspectedContainer, log)
}

func handlePhysicalContainerCreating(
	_ context.Context,
	_ *PhysicalContainerReconciler,
	_ *apiv2.PhysicalContainer,
	_ string,
	_ *physicalContainerData,
	log logr.Logger,
) objectChange {
	log.V(1).Info("Physical container creation is still in progress")
	return noChange
}

func handlePhysicalContainerCreated(
	_ context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	_ string,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	reconciler.ensurePhysicalContainerWatch(container, log)
	stateKey := physicalContainerDataCurrentStateKey(container, data)
	if len(container.Spec.CreateFiles) > 0 {
		return reconciler.schedulePhysicalContainerCreateFiles(container, stateKey, data, log)
	}
	if container.Spec.Stop {
		return reconciler.skipPhysicalContainerStart(container, stateKey, data, log)
	}
	return reconciler.schedulePhysicalContainerStart(container, stateKey, data, log)
}

func handlePhysicalContainerOperationInProgress(
	_ context.Context,
	_ *PhysicalContainerReconciler,
	_ *apiv2.PhysicalContainer,
	_ string,
	_ *physicalContainerData,
	_ logr.Logger,
) objectChange {
	return noChange
}

func handlePhysicalContainerFilesCreated(
	_ context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	_ string,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	reconciler.ensurePhysicalContainerWatch(container, log)
	stateKey := physicalContainerDataCurrentStateKey(container, data)
	if container.Spec.Stop {
		return reconciler.skipPhysicalContainerStart(container, stateKey, data, log)
	}
	return reconciler.schedulePhysicalContainerStart(container, stateKey, data, log)
}

func handlePhysicalContainerStarted(
	_ context.Context,
	_ *PhysicalContainerReconciler,
	_ *apiv2.PhysicalContainer,
	_ string,
	_ *physicalContainerData,
	_ logr.Logger,
) objectChange {
	return noChange
}

func handlePhysicalContainerOperationFailed(
	_ context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	_ string,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	if data.containerID == "" {
		reconciler.containerData.DeleteByNamespacedName(container.NamespacedName())
	}
	log.V(1).Info("Physical container operation failed; saving container status", "Message", data.failureMessage)
	// The failure is terminal: spec is immutable, so no further reconciliation can make progress.
	return noChange
}

func handleUnknownPhysicalContainerDataReason(
	_ context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	conditionReason string,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	reconciler.containerData.DeleteByNamespacedName(container.NamespacedName())
	message := fmt.Sprintf("Physical container operation reached unknown condition reason %q.", conditionReason)
	log.Error(fmt.Errorf("unknown physical container condition reason %q", conditionReason), "Physical container operation reached unknown condition reason")
	change := setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
	change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonReconciliationFailed, message)
	return change | additionalReconciliationNeeded
}

func physicalContainerDataCurrentStateKey(container *apiv2.PhysicalContainer, data *physicalContainerData) physicalContainerDataStateKey {
	if data.containerID != "" {
		return physicalContainerDataContainerIDKey(data.containerID)
	}
	return physicalContainerDataKey(container)
}

func (r *PhysicalContainerReconciler) resolvePhysicalContainerImage(ctx context.Context, container *apiv2.PhysicalContainer, log logr.Logger) (bool, objectChange) {
	image := apiv2.PhysicalContainerImage{}
	getErr := r.Client.Get(ctx, types.NamespacedName{Namespace: container.Namespace, Name: container.Spec.ImageRef}, &image)
	if apierrors.IsNotFound(getErr) {
		change := setValue(&container.Status.Phase, apiv2.PhysicalContainerPhasePending)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonPending, fmt.Sprintf("PhysicalContainerImage %q does not exist.", container.Spec.ImageRef))
		return false, change
	}
	if getErr != nil {
		log.Error(getErr, "Failed to get PhysicalContainerImage", "ImageRef", container.Spec.ImageRef)
		change := setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonReconciliationFailed, fmt.Sprintf("Failed to get PhysicalContainerImage: %v", getErr))
		return false, change
	}
	if image.Status.Phase != apiv2.PhysicalContainerImagePhaseReady || image.Status.Image == "" {
		change := setValue(&container.Status.Phase, apiv2.PhysicalContainerPhasePending)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonPending, fmt.Sprintf("PhysicalContainerImage %q is not ready.", container.Spec.ImageRef))
		return false, change
	}

	return true, setValue(&container.Status.Image, image.Status.Image)
}

func (r *PhysicalContainerReconciler) schedulePhysicalContainerCreate(container *apiv2.PhysicalContainer, log logr.Logger) objectChange {
	stateKey := physicalContainerDataKey(container)
	data := newPhysicalContainerData()
	r.containerData.Store(container.NamespacedName(), stateKey, data)
	containerSnapshot := container.DeepCopy()
	dataSnapshot := data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.createPhysicalContainer(operationCtx, containerSnapshot, stateKey, dataSnapshot, log)
	})
	if enqueueErr != nil {
		r.containerData.DeleteByNamespacedName(container.NamespacedName())
		log.Error(enqueueErr, "Failed to queue PhysicalContainer create")
		change := setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonCreateFailed, fmt.Sprintf("Failed to queue physical container create: %v", enqueueErr))
		return change
	}

	log.V(1).Info("Queued PhysicalContainer create")
	change := setValue(&container.Status.Phase, apiv2.PhysicalContainerPhasePending)
	change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonCreating, "Physical container creation is in progress.")
	return change
}

func (r *PhysicalContainerReconciler) createPhysicalContainer(
	ctx context.Context,
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) {
	containerID, createErr := r.orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:         container.Spec.ContainerName,
		Image:        container.Status.Image,
		Entrypoint:   container.Spec.Entrypoint,
		Command:      container.Spec.Command,
		VolumeMounts: physicalVolumeMountsToCreateContainerVolumeMounts(container.Spec.VolumeMounts),
		Ports:        physicalPortsToCreateContainerPorts(container.Spec.Ports),
		Networks:     physicalNetworksToCreateContainerNetworks(container.Spec.Networks),
		Env:          container.Spec.Env,
		Labels:       physicalContainerCreationLabels(container, log),
	})
	if createErr != nil {
		log.Error(createErr, "Failed to create physical container")
		data.containerID = containerID
		data.conditionReason = apiv2.PhysicalContainerReasonCreateFailed
		data.failureMessage = fmt.Sprintf("Failed to create physical container: %v", createErr)
	} else if containerID == "" {
		missingContainerIDErr := errors.New("physical container create succeeded without returning a runtime container ID")
		log.Error(missingContainerIDErr, "Physical container create succeeded without returning a runtime container ID")
		data.conditionReason = apiv2.PhysicalContainerReasonCreateFailed
		data.failureMessage = "Physical container create succeeded without returning a runtime container ID."
	} else {
		data.containerID = containerID
		data.conditionReason = apiv2.PhysicalContainerReasonCreated
		data.failureMessage = ""
	}

	r.queuePhysicalContainerDataResult(container, stateKey, data)
}

func (r *PhysicalContainerReconciler) schedulePhysicalContainerCreateFiles(
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	data.conditionReason = apiv2.PhysicalContainerReasonCopyingFiles
	data.failureMessage = ""
	if !r.containerData.Update(container.NamespacedName(), stateKey, data) {
		return setValue(&container.Status.ContainerID, data.containerID) | additionalReconciliationNeeded
	}

	containerSnapshot := container.DeepCopy()
	dataSnapshot := data.Clone()
	fileModTime := time.Now()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.copyPhysicalContainerCreateFiles(operationCtx, containerSnapshot, stateKey, dataSnapshot, fileModTime, log)
	})
	if enqueueErr != nil {
		data.conditionReason = apiv2.PhysicalContainerReasonFileCopyFailed
		data.failureMessage = fmt.Sprintf("Failed to queue physical container file copy: %v", enqueueErr)
		_ = r.containerData.Update(container.NamespacedName(), stateKey, data)
		log.Error(enqueueErr, "Failed to queue PhysicalContainer file copy", "ContainerID", data.containerID)
		change := setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
		return change
	}

	log.V(1).Info("Queued PhysicalContainer file copy", "ContainerID", data.containerID)
	change := setValue(&container.Status.ContainerID, data.containerID)
	change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhasePending)
	change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonCopyingFiles, "Physical container file copy is in progress.")
	return change
}

func (r *PhysicalContainerReconciler) copyPhysicalContainerCreateFiles(
	ctx context.Context,
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	fileModTime time.Time,
	log logr.Logger,
) {
	for _, createFileRequest := range container.Spec.CreateFiles {
		umask := osutil.DefaultUmaskBitmask
		if createFileRequest.Umask != nil {
			umask = *createFileRequest.Umask
		}

		createFilesOptions := containers.CreateFilesOptions{
			Container:    data.containerID,
			Entries:      v2FileSystemEntriesToContainerFileSystemEntries(createFileRequest.Entries),
			Destination:  createFileRequest.Destination,
			DefaultOwner: createFileRequest.DefaultOwner,
			DefaultGroup: createFileRequest.DefaultGroup,
			Umask:        umask,
			ModTime:      fileModTime,
		}

		copyErr := r.orchestrator.CreateFiles(ctx, createFilesOptions)
		if copyErr != nil {
			log.Error(copyErr, "Failed to copy files to physical container", "ContainerID", data.containerID, "Destination", createFileRequest.Destination)
			data.conditionReason = apiv2.PhysicalContainerReasonFileCopyFailed
			data.failureMessage = fmt.Sprintf("Failed to copy files to physical container: %v", copyErr)
			r.queuePhysicalContainerDataResult(container, stateKey, data)
			return
		}

		log.V(1).Info("Files copied to the physical container", "ContainerID", data.containerID, "Destination", createFileRequest.Destination)
	}

	data.conditionReason = apiv2.PhysicalContainerReasonFilesCreated
	data.failureMessage = ""
	r.queuePhysicalContainerDataResult(container, stateKey, data)
}

func (r *PhysicalContainerReconciler) schedulePhysicalContainerStart(
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	data.conditionReason = apiv2.PhysicalContainerReasonStarting
	data.failureMessage = ""
	if !r.containerData.Update(container.NamespacedName(), stateKey, data) {
		return setValue(&container.Status.ContainerID, data.containerID) | additionalReconciliationNeeded
	}

	containerSnapshot := container.DeepCopy()
	dataSnapshot := data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.startPhysicalContainer(operationCtx, containerSnapshot, stateKey, dataSnapshot, log)
	})
	if enqueueErr != nil {
		data.conditionReason = apiv2.PhysicalContainerReasonStartFailed
		data.failureMessage = fmt.Sprintf("Failed to queue physical container start: %v", enqueueErr)
		_ = r.containerData.Update(container.NamespacedName(), stateKey, data)
		log.Error(enqueueErr, "Failed to queue PhysicalContainer start", "ContainerID", data.containerID)
		change := setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, data.conditionReason, data.failureMessage)
		return change
	}

	log.V(1).Info("Queued PhysicalContainer start", "ContainerID", data.containerID)
	change := setValue(&container.Status.ContainerID, data.containerID)
	change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhasePending)
	change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonStarting, "Physical container start is in progress.")
	return change
}

func (r *PhysicalContainerReconciler) skipPhysicalContainerStart(
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	updatedData := data.Clone()
	updatedData.conditionReason = apiv2.PhysicalContainerReasonStarted
	updatedData.failureMessage = ""
	if !r.containerData.Update(container.NamespacedName(), stateKey, updatedData) {
		return setValue(&container.Status.ContainerID, data.containerID) | additionalReconciliationNeeded
	}

	data.UpdateFrom(updatedData)
	log.V(1).Info("Skipping PhysicalContainer start because stop is requested", "ContainerID", data.containerID)
	return setValue(&container.Status.ContainerID, data.containerID)
}

func (r *PhysicalContainerReconciler) startPhysicalContainer(
	ctx context.Context,
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) {
	_, startErr := r.orchestrator.StartContainers(ctx, containers.StartContainersOptions{
		Containers: []string{data.containerID},
	})
	if startErr != nil {
		log.Error(startErr, "Failed to start physical container", "ContainerID", data.containerID)
		data.conditionReason = apiv2.PhysicalContainerReasonStartFailed
		data.failureMessage = fmt.Sprintf("Failed to start physical container: %v", startErr)
	} else {
		data.conditionReason = apiv2.PhysicalContainerReasonStarted
		data.failureMessage = ""
	}

	r.queuePhysicalContainerDataResult(container, stateKey, data)
}

func (r *PhysicalContainerReconciler) queuePhysicalContainerDataResult(
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	result *physicalContainerData,
) {
	queued := r.containerData.QueueDeferredOpForStateKey(container.NamespacedName(), stateKey, func(name types.NamespacedName, currentStateKey physicalContainerDataStateKey, _ *apiv2.PhysicalContainer) {
		newStateKey := currentStateKey
		if result.containerID != "" {
			newStateKey = physicalContainerDataContainerIDKey(result.containerID)
		}
		if newStateKey != currentStateKey {
			_ = r.containerData.UpdateChangingStateKey(name, currentStateKey, newStateKey, result)
		} else {
			_ = r.containerData.Update(name, currentStateKey, result)
		}
	})
	if queued {
		r.ScheduleReconciliation(container.NamespacedName())
	}
}

func (r *PhysicalContainerReconciler) inspectPhysicalContainer(ctx context.Context, containerID string) (*containers.InspectedContainer, error) {
	inspectedContainers, inspectErr := r.orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{containerID},
	})
	if inspectErr != nil {
		return nil, inspectErr
	}
	if len(inspectedContainers) == 0 {
		return nil, containers.ErrNotFound
	}

	return &inspectedContainers[0], nil
}

func (r *PhysicalContainerReconciler) stopPhysicalContainerIfNecessary(
	ctx context.Context,
	containerID string,
	inspectedContainer *containers.InspectedContainer,
) (*containers.InspectedContainer, error) {
	if !physicalContainerNeedsStopping(inspectedContainer) {
		return inspectedContainer, nil
	}

	_, stopErr := r.orchestrator.StopContainers(ctx, containers.StopContainersOptions{
		Containers:    []string{containerID},
		SecondsToKill: stopContainerTimeoutSeconds,
	})
	if stopErr != nil {
		return inspectedContainer, stopErr
	}

	return r.inspectPhysicalContainer(ctx, containerID)
}

func physicalContainerNeedsStopping(inspectedContainer *containers.InspectedContainer) bool {
	return inspectedContainer.Status == containers.ContainerStatusRunning ||
		inspectedContainer.Status == containers.ContainerStatusPaused ||
		inspectedContainer.Status == containers.ContainerStatusRestarting
}

func (r *PhysicalContainerReconciler) handleDeletionRequest(ctx context.Context, container *apiv2.PhysicalContainer, log logr.Logger) objectChange {
	_, data := r.containerData.BorrowByNamespacedName(container.NamespacedName())
	if data != nil && data.operationInProgress() {
		log.V(1).Info("Physical container is being deleted while an operation is in progress", "Reason", data.conditionReason)
		return additionalReconciliationNeeded
	}

	containerID := container.Status.ContainerID
	if containerID == "" && data != nil {
		containerID = data.containerID
	}

	if !container.Spec.PreserveOnDeletion && containerID != "" {
		_, removeErr := r.orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
			Containers: []string{containerID},
			Force:      true,
		})
		if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
			log.Error(removeErr, "Failed to remove runtime container", "ContainerID", containerID)
			return additionalReconciliationNeeded
		}
	}

	r.containerData.DeleteByNamespacedName(container.NamespacedName())
	r.releasePhysicalContainerWatch(container, log)
	return deleteFinalizer(container, physicalContainerFinalizer, log)
}

func (r *PhysicalContainerReconciler) processContainerEvent(em containers.EventMessage) {
	switch em.Action {
	case containers.EventActionCreate, containers.EventActionDestroy, containers.EventActionDie, containers.EventActionDied,
		containers.EventActionKill, containers.EventActionOom, containers.EventActionPause, containers.EventActionRestart,
		containers.EventActionStart, containers.EventActionStop, containers.EventActionUnpause, containers.EventActionUpdate,
		containers.EventActionPrune, containers.EventActionExecDie, containers.EventActionHealthStatus:
		if em.Actor.ID == "" {
			return
		}

		owner, data := r.containerData.BorrowByStateKey(physicalContainerDataContainerIDKey(em.Actor.ID))
		if data == nil {
			return
		}

		if r.Log.V(1).Enabled() {
			r.Log.V(1).Info("Physical container event received, scheduling reconciliation", "ContainerID", GetShortId(em.Actor.ID), "Event", em.String())
		}

		r.ScheduleReconciliation(owner)
	}
}

func (r *PhysicalContainerReconciler) ensurePhysicalContainerWatch(container *apiv2.PhysicalContainer, log logr.Logger) {
	if r.ContainerWatcher == nil || container.UID == "" {
		return
	}

	r.EnsureContainerWatchForResource(container.UID, log)
}

func (r *PhysicalContainerReconciler) releasePhysicalContainerWatch(container *apiv2.PhysicalContainer, log logr.Logger) {
	if r.ContainerWatcher == nil || container.UID == "" {
		return
	}

	r.ReleaseContainerWatchForResource(container.UID, log)
}

// physicalPortsToCreateContainerPorts expands each V2 container port (which may describe an
// inclusive range) into one orchestrator port per concrete container port.
func physicalPortsToCreateContainerPorts(ports []apiv2.ContainerPort) []containers.CreateContainerPort {
	retval := make([]containers.CreateContainerPort, 0, len(ports))
	for _, port := range ports {
		containerPortEnd := port.EffectiveContainerPortEnd()
		for containerPortValue := int64(port.ContainerPort); containerPortValue <= int64(containerPortEnd); containerPortValue++ {
			containerPort := int32(containerPortValue)
			hostPort := port.HostPort
			if hostPort != 0 {
				hostPort += containerPort - port.ContainerPort
			}
			retval = append(retval, containers.CreateContainerPort{
				HostPort:      hostPort,
				ContainerPort: containerPort,
				Protocol:      string(port.Protocol),
				HostIP:        port.HostIP,
			})
		}
	}
	return retval
}

func physicalVolumeMountsToCreateContainerVolumeMounts(mounts []apiv2.VolumeMount) []containers.CreateContainerVolumeMount {
	retval := make([]containers.CreateContainerVolumeMount, len(mounts))
	for i, mount := range mounts {
		retval[i] = containers.CreateContainerVolumeMount{
			Type:     containers.VolumeMountType(mount.Type),
			Source:   mount.Source,
			Target:   mount.Target,
			ReadOnly: mount.ReadOnly,
		}
	}
	return retval
}

func physicalNetworksToCreateContainerNetworks(networks []apiv2.ContainerNetworkConnectionConfig) []containers.CreateContainerNetworkOptions {
	retval := make([]containers.CreateContainerNetworkOptions, len(networks))
	for i, network := range networks {
		aliases := make([]string, len(network.Aliases))
		copy(aliases, network.Aliases)
		retval[i] = containers.CreateContainerNetworkOptions{
			Name:    network.Name,
			Aliases: aliases,
		}
	}
	return retval
}

func physicalContainerCreationLabels(container *apiv2.PhysicalContainer, log logr.Logger) []containers.Label {
	labels := append([]containers.Label{}, container.Spec.Labels...)
	labels = append(labels, containers.Label{
		Key:   PersistentLabel,
		Value: fmt.Sprintf("%t", container.Spec.PreserveOnDeletion),
	})

	thisProcess, thisProcessErr := process.This()
	if thisProcessErr != nil {
		log.Error(thisProcessErr, "Could not get the current process information; physical container will not have creator process information")
		return labels
	}

	labels = append(labels, containers.Label{
		Key:   CreatorProcessIdLabel,
		Value: fmt.Sprintf("%d", thisProcess.Pid),
	})
	labels = append(labels, containers.Label{
		Key:   CreatorProcessStartTimeLabel,
		Value: thisProcess.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
	})
	return labels
}

func applyInspectedPhysicalContainerStatus(container *apiv2.PhysicalContainer, inspectedContainer *containers.InspectedContainer, log logr.Logger) objectChange {
	change := noChange
	change |= setValue(&container.Status.ContainerID, inspectedContainer.Id)
	change |= setValue(&container.Status.ContainerName, inspectedContainer.Name)
	change |= setValue(&container.Status.RuntimeStatus, string(inspectedContainer.Status))
	change |= setTimestamp(&container.Status.CreatedAt, metav1.NewMicroTime(inspectedContainer.CreatedAt))
	change |= setTimestamp(&container.Status.StartedAt, metav1.NewMicroTime(inspectedContainer.StartedAt))
	change |= setTimestamp(&container.Status.FinishedAt, metav1.NewMicroTime(inspectedContainer.FinishedAt))
	change |= setPhysicalContainerExitCode(container, inspectedContainer)

	portMappings, portMappingErr := physicalContainerPortMappingsFromInspected(inspectedContainer.Ports)
	if portMappingErr != nil {
		message := fmt.Sprintf("Failed to resolve physical container port mappings: %v", portMappingErr)
		log.Error(portMappingErr, "Failed to resolve physical container port mappings", "ContainerID", inspectedContainer.Id)
		change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseFailed)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonReconciliationFailed, message)
		return change
	}
	change |= setPhysicalContainerPortMappings(container, portMappings)

	switch inspectedContainer.Status {
	case containers.ContainerStatusRunning, containers.ContainerStatusRestarting, containers.ContainerStatusPaused:
		change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseRunning)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionTrue, apiv2.PhysicalContainerReasonRuntimeContainerRunning, "Runtime container is running.")
		// Keep polling slowly so a missed runtime event cannot strand the container in a stale state.
		change |= additionalReconciliationNeeded
	case containers.ContainerStatusExited, containers.ContainerStatusDead:
		change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhaseExited)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerExited, "Runtime container has exited.")
	default:
		change |= setValue(&container.Status.Phase, apiv2.PhysicalContainerPhasePending)
		change |= setReadyCondition(&container.Status.Conditions, container.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerPending, "Runtime container is not running.")
		change |= additionalReconciliationNeeded
	}

	return change
}

func physicalContainerPortMappingsFromInspected(ports containers.InspectedContainerPortMapping) ([]apiv2.PhysicalContainerPortMapping, error) {
	portMappings := make([]apiv2.PhysicalContainerPortMapping, 0, len(ports))
	for portKey, hostPortConfigs := range ports {
		containerPort, protocol, parseErr := parseInspectedContainerPortKey(portKey)
		if parseErr != nil {
			return nil, parseErr
		}

		hostPortMappingFound := false
		for _, hostPortConfig := range hostPortConfigs {
			if hostPortConfig.HostPort == "" {
				continue
			}
			hostPort, hostPortErr := strconv.ParseInt(hostPortConfig.HostPort, 10, 32)
			if hostPortErr != nil {
				return nil, fmt.Errorf("parse host port %q for container port %q: %w", hostPortConfig.HostPort, portKey, hostPortErr)
			}
			if hostPort <= 0 {
				return nil, fmt.Errorf("parse host port %q for container port %q: host port must be greater than zero", hostPortConfig.HostPort, portKey)
			}
			portMappings = append(portMappings, apiv2.PhysicalContainerPortMapping{
				ContainerPort: containerPort,
				Protocol:      protocol,
				HostIP:        hostPortConfig.HostIp,
				HostPort:      int32(hostPort),
			})
			hostPortMappingFound = true
		}
		if !hostPortMappingFound {
			portMappings = append(portMappings, apiv2.PhysicalContainerPortMapping{
				ContainerPort: containerPort,
				Protocol:      protocol,
			})
		}
	}

	std_slices.SortFunc(portMappings, func(left, right apiv2.PhysicalContainerPortMapping) int {
		if left.ContainerPort != right.ContainerPort {
			return cmp.Compare(left.ContainerPort, right.ContainerPort)
		}
		if protocolComparison := strings.Compare(string(left.Protocol), string(right.Protocol)); protocolComparison != 0 {
			return protocolComparison
		}
		if hostIPComparison := strings.Compare(left.HostIP, right.HostIP); hostIPComparison != 0 {
			return hostIPComparison
		}
		return cmp.Compare(left.HostPort, right.HostPort)
	})
	return portMappings, nil
}

func parseInspectedContainerPortKey(portKey string) (int32, commonapi.PortProtocol, error) {
	portParts := strings.Split(portKey, "/")
	if portParts[0] == "" {
		return 0, "", fmt.Errorf("parse container port key %q: container port is missing", portKey)
	}

	containerPort, portErr := strconv.ParseInt(portParts[0], 10, 32)
	if portErr != nil {
		return 0, "", fmt.Errorf("parse container port key %q: %w", portKey, portErr)
	}
	if containerPort <= 0 {
		return 0, "", fmt.Errorf("parse container port key %q: container port must be greater than zero", portKey)
	}

	protocol := commonapi.PortProtocolTCP
	if len(portParts) > 1 && portParts[1] != "" {
		protocol = commonapi.PortProtocol(strings.ToUpper(portParts[1]))
	}
	return int32(containerPort), protocol, nil
}

func setPhysicalContainerPortMappings(container *apiv2.PhysicalContainer, portMappings []apiv2.PhysicalContainerPortMapping) objectChange {
	if std_slices.EqualFunc(container.Status.PortMappings, portMappings, physicalContainerPortMappingEqual) {
		return noChange
	}

	container.Status.PortMappings = portMappings
	return statusChanged
}

func physicalContainerPortMappingEqual(left, right apiv2.PhysicalContainerPortMapping) bool {
	return left.ContainerPort == right.ContainerPort &&
		left.Protocol == right.Protocol &&
		left.HostIP == right.HostIP &&
		left.HostPort == right.HostPort
}

func setPhysicalContainerExitCode(container *apiv2.PhysicalContainer, inspectedContainer *containers.InspectedContainer) objectChange {
	if inspectedContainer.FinishedAt.IsZero() {
		if container.Status.ExitCode == nil {
			return noChange
		}
		container.Status.ExitCode = nil
		return statusChanged
	}

	if container.Status.ExitCode != nil && *container.Status.ExitCode == inspectedContainer.ExitCode {
		return noChange
	}
	exitCode := inspectedContainer.ExitCode
	container.Status.ExitCode = &exitCode
	return statusChanged
}

func v2FileSystemEntriesToContainerFileSystemEntries(entries []apiv2.FileSystemEntry) []containers.FileSystemEntry {
	return slices.Map[containers.FileSystemEntry](entries, func(entry apiv2.FileSystemEntry) containers.FileSystemEntry {
		return containers.FileSystemEntry{
			Type:            containers.FileSystemEntryType(entry.Type),
			Name:            entry.Name,
			Owner:           entry.Owner,
			Group:           entry.Group,
			Mode:            entry.Mode,
			Source:          entry.Source,
			Contents:        entry.Contents,
			RawContents:     entry.RawContents,
			ContinueOnError: entry.ContinueOnError,
			Entries:         v2FileSystemEntriesToContainerFileSystemEntries(entry.Entries),
		}
	})
}
