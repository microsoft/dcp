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
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/slices"
)

const physicalContainerImageRefField = ".spec.imageRef"

var (
	physicalContainerFinalizer string = fmt.Sprintf("%s/physicalcontainer-reconciler", apiv2.GroupVersion.Group)

	physicalContainerDataInitializers = map[physicalContainerState]physicalContainerDataInitializerFunc{
		physicalContainerStateNamespace: handlePhysicalContainerNamespace,
		physicalContainerStateResolve:   handlePhysicalContainerResolve,
		physicalContainerStateImage:     handlePhysicalContainerImage,
		physicalContainerStateCreate:    handlePhysicalContainerCreate,
		physicalContainerStateReplace:   handlePhysicalContainerCreateFailure,
		physicalContainerStateCopyFiles: handlePhysicalContainerCopyFiles,
		physicalContainerStateStart:     handlePhysicalContainerStart,
		physicalContainerStateCleanup:   handlePhysicalContainerCreateFailure,
		// A failed stop or port mapping resolution is a diagnostic flavor of the runtime
		// observation concern: both recover by observing the container again.
		physicalContainerStateRuntime:     handlePhysicalContainerRuntime,
		physicalContainerStateStop:        handlePhysicalContainerRuntime,
		physicalContainerStatePortMapping: handlePhysicalContainerRuntime,
		physicalContainerStateInvalid:     handleUnknownPhysicalContainerDataReason,
		0:                                 handleUnknownPhysicalContainerDataReason,
	}
)

type physicalContainerDataInitializerFunc = physicalResourceStateHandlerFunc[
	apiv2.PhysicalContainer, *apiv2.PhysicalContainer,
	PhysicalContainerReconciler, *PhysicalContainerReconciler,
	physicalContainerState,
	physicalContainerDataStateKey,
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
		if container.Spec.Container == nil || container.Spec.Container.ImageRef == "" {
			return nil
		}

		return []string{container.Spec.Container.ImageRef}
	}); err != nil {
		r.Log.Error(err, "Failed to create imageRef index for PhysicalContainer", "IndexField", physicalContainerImageRefField)
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.PhysicalContainer{}).
		Watches(&apiv2.PhysicalContainerImage{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForImage), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNamespace(&apiv2.PhysicalContainerList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
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
			r.discardPhysicalContainerData(req.NamespacedName, "", nil, log)
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
	reconciliationDelay := StandardDelay
	patch := ctrl_client.MergeFromWithOptions(container.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if container.DeletionTimestamp != nil && !container.DeletionTimestamp.IsZero() {
		change, reconciliationDelay = r.managePhysicalContainer(ctx, &container, log)
	} else if change = ensureFinalizer(&container, physicalContainerFinalizer, log); change != noChange {
		// Make additional changes during the next reconciliation.
	} else {
		change, reconciliationDelay = r.managePhysicalContainer(ctx, &container, log)
	}

	return r.SaveChangesWithDelay(ctx, &container, patch, change, reconciliationDelay, nil, log)
}

func (r *PhysicalContainerReconciler) managePhysicalContainer(
	ctx context.Context,
	container *apiv2.PhysicalContainer,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	stateKey, data := r.containerData.BorrowByNamespacedName(container.NamespacedName())
	if data == nil {
		data = &physicalContainerData{
			resourceUID: container.UID,
			state:       physicalContainerStateNamespace,
			progress:    physicalResourceProgressNotReady,
		}
		initialStateKey := physicalContainerDataKey(container)
		stateKey = initialStateKey
		// Store() retains the supplied pointer, so keep an unaliased copy for this reconciliation.
		r.containerData.Store(container.NamespacedName(), initialStateKey, data.Clone())
	}

	var change objectChange
	if container.DeletionTimestamp != nil && !container.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, container, stateKey, data, log)
	} else {
		handler := getStateHandler(physicalContainerDataInitializers, data.state, log)
		change = handler(ctx, r, container, data.state, stateKey, data, log)
	}
	if !hasFinalizer(container, physicalContainerFinalizer) {
		return change, StandardDelay
	}

	_ = r.containerData.Update(container.NamespacedName(), stateKey, data)
	change |= data.applyTo(container)
	delay := physicalContainerProjections.reconciliationDelay(data.state, data.progress)
	return change, delay
}

func handlePhysicalContainerNamespace(
	ctx context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	_ physicalContainerState,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, reconciler.Client, container.Namespace)
	if !namespaceReady {
		data.state = physicalContainerStateNamespace
		data.failureMessage = namespaceReadinessMessage(container.Namespace, namespaceReason)
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
			data.failureMessage = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
		}
		return noChange
	}

	data.state = physicalContainerStateResolve
	data.progress = physicalResourceProgressInProgress
	data.failureMessage = ""
	return handlePhysicalContainerResolve(ctx, reconciler, container, data.state, stateKey, data, log)
}

func handlePhysicalContainerResolve(
	ctx context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	_ physicalContainerState,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	// An earlier attempt lost the race for the runtime container, so wait before claiming it again.
	if data.progress == physicalResourceProgressRetryPending {
		if time.Now().Before(data.retryAfter) {
			return additionalReconciliationNeeded
		}
		data.progress = physicalResourceProgressInProgress
		data.containerID = ""
		data.failureMessage = ""
		data.retryAfter = time.Time{}
	}

	containerID := container.Spec.ContainerID
	if containerID == "" {
		containerID = data.containerID
	}
	if containerID == "" {
		return handlePhysicalContainerImage(ctx, reconciler, container, data.state, stateKey, data, log)
	}

	if data.containerID == "" {
		owner, stored := storeStartedPhysicalContainerData(reconciler.containerData, container, stateKey, containerID, data)
		if !stored {
			if owner == (types.NamespacedName{}) {
				return additionalReconciliationNeeded
			}
			data.state = physicalContainerStateResolve
			data.progress = physicalResourceProgressRetryPending
			data.containerID = containerID
			data.failureMessage = fmt.Sprintf("Runtime container is already tracked by PhysicalContainer %q.", owner.String())
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			return additionalReconciliationNeeded
		}
		return additionalReconciliationNeeded
	}

	return handlePhysicalContainerRuntime(ctx, reconciler, container, data.state, physicalContainerDataContainerIDKey(containerID), data, log)
}

func handlePhysicalContainerImage(
	ctx context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	_ physicalContainerState,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	imageReady, image, imageProgress, imageMessage, imageChange := reconciler.resolvePhysicalContainerImage(ctx, container, log)
	if !imageReady {
		data.state = physicalContainerStateImage
		data.progress = imageProgress
		data.failureMessage = imageMessage
		return imageChange
	}

	data.image = image
	return imageChange | reconciler.schedulePhysicalContainerCreate(container, stateKey, data, log)
}

func handlePhysicalContainerCreate(
	ctx context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	state physicalContainerState,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalResourceProgressInProgress:
		return handlePhysicalContainerCreating(ctx, reconciler, container, state, stateKey, data, log)
	case physicalResourceProgressCompleted:
		return handlePhysicalContainerCreated(ctx, reconciler, container, state, stateKey, data, log)
	default:
		return handlePhysicalContainerCreateFailure(ctx, reconciler, container, state, stateKey, data, log)
	}
}

func handlePhysicalContainerCopyFiles(
	ctx context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	state physicalContainerState,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	if data.progress == physicalResourceProgressCompleted {
		return handlePhysicalContainerFilesCreated(ctx, reconciler, container, state, stateKey, data, log)
	}
	if data.progress == physicalResourceProgressFailed {
		return handlePhysicalContainerOperationFailed(ctx, reconciler, container, state, stateKey, data, log)
	}
	return handlePhysicalContainerOperationInProgress(ctx, reconciler, container, state, stateKey, data, log)
}

func handlePhysicalContainerStart(
	ctx context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	state physicalContainerState,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	if data.progress == physicalResourceProgressCompleted {
		return handlePhysicalContainerRuntime(ctx, reconciler, container, state, stateKey, data, log)
	}
	if data.progress == physicalResourceProgressFailed {
		return handlePhysicalContainerOperationFailed(ctx, reconciler, container, state, stateKey, data, log)
	}
	return handlePhysicalContainerOperationInProgress(ctx, reconciler, container, state, stateKey, data, log)
}

// Observes the runtime container and records what was seen. When the spec requests a stop, the
// stop operation takes over before the observed state is applied.
func handlePhysicalContainerRuntime(
	ctx context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	_ physicalContainerState,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	containerID := data.containerID
	if containerID == "" {
		// The runtime identity has not been captured yet, so resolution owns this reconciliation.
		data.state = physicalContainerStateResolve
		data.progress = physicalResourceProgressInProgress
		return handlePhysicalContainerResolve(ctx, reconciler, container, data.state, stateKey, data, log)
	}

	reconciler.ensurePhysicalContainerWatch(container, log)
	inspectedContainer, inspectErr := reconciler.inspectPhysicalContainer(ctx, containerID)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		data.state = physicalContainerStateRuntime
		data.progress = physicalResourceProgressMissing
		data.failureMessage = ""
		return noChange
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect runtime container", "ContainerID", containerID)
		data.state = physicalContainerStateRuntime
		data.progress = physicalResourceProgressRetryPending
		data.failureMessage = fmt.Sprintf("Failed to inspect runtime container: %v", inspectErr)
		return additionalReconciliationNeeded
	}

	if container.Spec.Stop {
		return reconciler.stopPhysicalContainer(ctx, container, stateKey, data, inspectedContainer, log)
	}

	return reconciler.applyInspectedPhysicalContainerStatus(container, stateKey, data, inspectedContainer, log)
}

// Stops the runtime container when it is still active and records the resulting state.
func (r *PhysicalContainerReconciler) stopPhysicalContainer(
	ctx context.Context,
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	inspectedContainer *containers.InspectedContainer,
	log logr.Logger,
) objectChange {
	containerID := data.containerID
	stoppedContainer, stopErr := r.stopPhysicalContainerIfNecessary(ctx, containerID, inspectedContainer)
	if errors.Is(stopErr, containers.ErrNotFound) {
		data.state = physicalContainerStateRuntime
		data.progress = physicalResourceProgressMissing
		data.failureMessage = ""
		return noChange
	}
	if stopErr != nil {
		log.Error(stopErr, "Failed to stop runtime container", "ContainerID", containerID)
		data.state = physicalContainerStateStop
		data.progress = physicalResourceProgressRetryPending
		data.failureMessage = fmt.Sprintf("Failed to stop runtime container: %v", stopErr)
		return applyInspectedPhysicalContainerDetails(container, inspectedContainer, log) | additionalReconciliationNeeded
	}

	return r.applyInspectedPhysicalContainerStatus(container, stateKey, data, stoppedContainer, log)
}

func handlePhysicalContainerCreating(
	_ context.Context,
	_ *PhysicalContainerReconciler,
	_ *apiv2.PhysicalContainer,
	_ physicalContainerState,
	_ physicalContainerDataStateKey,
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
	_ physicalContainerState,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	reconciler.ensurePhysicalContainerWatch(container, log)
	if len(container.Spec.Container.CreateFiles) > 0 {
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
	_ physicalContainerState,
	_ physicalContainerDataStateKey,
	_ *physicalContainerData,
	_ logr.Logger,
) objectChange {
	return noChange
}

func handlePhysicalContainerFilesCreated(
	_ context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	_ physicalContainerState,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	reconciler.ensurePhysicalContainerWatch(container, log)
	if container.Spec.Stop {
		return reconciler.skipPhysicalContainerStart(container, stateKey, data, log)
	}
	return reconciler.schedulePhysicalContainerStart(container, stateKey, data, log)
}

func handlePhysicalContainerOperationFailed(
	_ context.Context,
	_ *PhysicalContainerReconciler,
	_ *apiv2.PhysicalContainer,
	_ physicalContainerState,
	_ physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	log.V(1).Info("Physical container operation failed; saving container status", "Message", data.failureMessage)
	// The failure is terminal: spec is immutable, so no further reconciliation can make progress.
	return noChange
}

func handlePhysicalContainerCreateFailure(
	ctx context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	state physicalContainerState,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	switch data.progress {
	case physicalContainerOperationRetryPending:
		return handlePhysicalContainerRecoverableCreateFailed(ctx, reconciler, container, state, stateKey, data, log)
	case physicalContainerOperationFailed:
		return handlePhysicalContainerCreateFailed(ctx, reconciler, container, state, stateKey, data, log)
	default:
		return handleUnknownPhysicalContainerDataReason(ctx, reconciler, container, state, stateKey, data, log)
	}
}

func handlePhysicalContainerCreateFailed(
	ctx context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	_ physicalContainerState,
	_ physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}

	cleanupChange, cleanupComplete := reconciler.removePartiallyCreatedPhysicalContainer(ctx, container, data, log)
	if !cleanupComplete {
		return cleanupChange
	}

	log.V(1).Info("Physical container creation failed; saving container status", "Message", data.failureMessage)
	return cleanupChange
}

func handlePhysicalContainerRecoverableCreateFailed(
	ctx context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	_ physicalContainerState,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	if time.Now().Before(data.retryAfter) {
		return additionalReconciliationNeeded
	}

	cleanupChange, cleanupComplete := reconciler.removePartiallyCreatedPhysicalContainer(ctx, container, data, log)
	if !cleanupComplete {
		return cleanupChange
	}

	log.V(1).Info("Retrying physical container creation", "ContainerName", container.Spec.Container.ContainerName)
	return cleanupChange | reconciler.schedulePhysicalContainerCreate(container, stateKey, data, log)
}

func (r *PhysicalContainerReconciler) removePartiallyCreatedPhysicalContainer(
	ctx context.Context,
	container *apiv2.PhysicalContainer,
	data *physicalContainerData,
	log logr.Logger,
) (objectChange, bool) {
	if data.containerID == "" {
		return noChange, true
	}

	partialContainerID := data.containerID
	_, removeErr := r.orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{partialContainerID},
		Force:      true,
	})
	if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
		log.Error(removeErr, "Failed to remove partially created runtime container", "ContainerID", partialContainerID)
		data.state = physicalContainerStateCleanup
		data.cleanupMessage = fmt.Sprintf("Failed to remove partially created runtime container: %v", removeErr)
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		return additionalReconciliationNeeded, false
	}

	data.state = physicalContainerStateCreate
	data.containerID = ""
	data.cleanupMessage = ""
	data.retryAfter = time.Time{}

	log.V(1).Info("Removed partially created runtime container", "ContainerID", partialContainerID)
	return setValue(&container.Status.ContainerID, ""), true
}

func handleUnknownPhysicalContainerDataReason(
	_ context.Context,
	reconciler *PhysicalContainerReconciler,
	container *apiv2.PhysicalContainer,
	state physicalContainerState,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	data.state = physicalContainerStateInvalid
	data.progress = physicalResourceProgressFailed
	data.failureMessage = fmt.Sprintf("Physical container operation reached unknown state %d.", state)
	log.Error(fmt.Errorf("unknown physical container state %d", state), "Physical container operation reached unknown state")
	return additionalReconciliationNeeded
}

func (r *PhysicalContainerReconciler) resolvePhysicalContainerImage(
	ctx context.Context,
	container *apiv2.PhysicalContainer,
	log logr.Logger,
) (bool, string, physicalResourceProgress, string, objectChange) {
	image := apiv2.PhysicalContainerImage{}
	imageRef := container.Spec.Container.ImageRef
	getErr := r.Client.Get(ctx, types.NamespacedName{Namespace: container.Namespace, Name: imageRef}, &image)
	if apierrors.IsNotFound(getErr) {
		return false, "", physicalResourceProgressNotFound, fmt.Sprintf("PhysicalContainerImage %q does not exist.", imageRef), noChange
	}
	if getErr != nil {
		log.Error(getErr, "Failed to get PhysicalContainerImage", "ImageRef", imageRef)
		return false, "", physicalResourceProgressRetryPending, fmt.Sprintf("Failed to get PhysicalContainerImage: %v", getErr), additionalReconciliationNeeded
	}
	if image.Status.Phase != apiv2.PhysicalContainerImagePhaseReady || image.Status.ImageID == "" {
		return false, "", physicalResourceProgressNotReady, fmt.Sprintf("PhysicalContainerImage %q is not ready.", imageRef), noChange
	}

	return true, image.Status.ImageID, physicalResourceProgressCompleted, "", setValue(&container.Status.Image, image.Status.ImageID)
}

func (r *PhysicalContainerReconciler) schedulePhysicalContainerCreate(
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	currentData *physicalContainerData,
	log logr.Logger,
) objectChange {
	data := currentData.Clone()
	data.state = physicalContainerStateCreate
	data.progress = physicalResourceProgressInProgress
	data.containerID = ""
	data.failureMessage = ""
	data.cleanupMessage = ""
	data.retryAfter = time.Time{}
	if !r.containerData.Update(container.NamespacedName(), stateKey, data) {
		return additionalReconciliationNeeded
	}
	currentData.UpdateFrom(data)
	containerSnapshot := container.DeepCopy()
	dataSnapshot := data.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.createPhysicalContainer(operationCtx, containerSnapshot, stateKey, dataSnapshot, log)
	})
	if enqueueErr != nil {
		log.Error(enqueueErr, "Failed to queue PhysicalContainer create")
		data.progress = physicalResourceProgressFailed
		data.failureMessage = fmt.Sprintf("Failed to queue physical container create: %v", enqueueErr)
		currentData.UpdateFrom(data)
		return noChange
	}

	log.V(1).Info("Queued PhysicalContainer create")
	return noChange
}

func (r *PhysicalContainerReconciler) createPhysicalContainer(
	ctx context.Context,
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) {
	containerConfig := container.Spec.Container
	if containerConfig.ReplaceExisting {
		replaceErr := r.removePhysicalContainerForReplacement(ctx, containerConfig.ContainerName, log)
		if replaceErr != nil {
			log.Error(replaceErr, "Failed to replace existing physical container")
			data.state = physicalContainerStateReplace
			data.progress = physicalContainerOperationRetryPending
			data.failureMessage = fmt.Sprintf("Failed to replace existing physical container: %v", replaceErr)
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
			r.queuePhysicalContainerDataResult(container, stateKey, data)
			return
		}
	}

	containerID, createErr := r.orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:         containerConfig.ContainerName,
		Image:        data.image,
		Entrypoint:   containerConfig.Entrypoint,
		Command:      containerConfig.Command,
		VolumeMounts: physicalVolumeMountsToCreateContainerVolumeMounts(containerConfig.VolumeMounts),
		Ports:        physicalPortsToCreateContainerPorts(containerConfig.Ports),
		Networks:     physicalNetworksToCreateContainerNetworks(containerConfig.Networks),
		Env:          containerConfig.Env,
		Labels:       physicalContainerCreationLabels(container, log),
	})
	if createErr != nil {
		log.Error(createErr, "Failed to create physical container")
		data.containerID = containerID
		data.failureMessage = fmt.Sprintf("Failed to create physical container: %v", createErr)
		if (errors.Is(createErr, containers.ErrAlreadyExists) && !containerConfig.ReplaceExisting) ||
			errors.Is(createErr, containers.ErrCouldNotAllocate) {
			data.state = physicalContainerStateCreate
			data.progress = physicalContainerOperationFailed
			data.retryAfter = time.Time{}
		} else {
			data.state = physicalContainerStateCreate
			data.progress = physicalContainerOperationRetryPending
			data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
		}
	} else if containerID == "" {
		missingContainerIDErr := errors.New("physical container create succeeded without returning a runtime container ID")
		log.Error(missingContainerIDErr, "Physical container create succeeded without returning a runtime container ID")
		data.state = physicalContainerStateCreate
		data.progress = physicalContainerOperationFailed
		data.failureMessage = "Physical container create succeeded without returning a runtime container ID."
		data.retryAfter = time.Time{}
	} else {
		data.containerID = containerID
		data.state = physicalContainerStateCreate
		data.progress = physicalContainerOperationCompleted
		data.failureMessage = ""
		data.retryAfter = time.Time{}
	}

	r.queuePhysicalContainerDataResult(container, stateKey, data)
}

func (r *PhysicalContainerReconciler) removePhysicalContainerForReplacement(ctx context.Context, containerName string, log logr.Logger) error {
	inspectedContainer, inspectErr := r.inspectPhysicalContainer(ctx, containerName)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		return nil
	}
	if inspectErr != nil {
		return fmt.Errorf("inspect runtime container %q: %w", containerName, inspectErr)
	}
	if inspectedContainer.Id == "" {
		return fmt.Errorf("inspect runtime container %q returned an empty ID", containerName)
	}

	_, removeErr := r.orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{inspectedContainer.Id},
		Force:      true,
	})
	if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
		return fmt.Errorf("remove runtime container %q: %w", inspectedContainer.Id, removeErr)
	}

	log.V(1).Info("Removed existing runtime container before replacement", "ContainerID", inspectedContainer.Id, "ContainerName", containerName)
	return nil
}

func (r *PhysicalContainerReconciler) schedulePhysicalContainerCreateFiles(
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	scheduledData := data.Clone()
	scheduledData.state = physicalContainerStateCopyFiles
	scheduledData.progress = physicalContainerOperationInProgress
	scheduledData.failureMessage = ""
	if !r.containerData.Update(container.NamespacedName(), stateKey, scheduledData) {
		return setValue(&container.Status.ContainerID, data.containerID) | additionalReconciliationNeeded
	}
	data.UpdateFrom(scheduledData)

	containerSnapshot := container.DeepCopy()
	dataSnapshot := scheduledData.Clone()
	fileModTime := time.Now()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.copyPhysicalContainerCreateFiles(operationCtx, containerSnapshot, stateKey, dataSnapshot, fileModTime, log)
	})
	if enqueueErr != nil {
		data.state = physicalContainerStateCopyFiles
		data.progress = physicalContainerOperationFailed
		data.failureMessage = fmt.Sprintf("Failed to queue physical container file copy: %v", enqueueErr)
		log.Error(enqueueErr, "Failed to queue PhysicalContainer file copy", "ContainerID", data.containerID)
		return noChange
	}

	log.V(1).Info("Queued PhysicalContainer file copy", "ContainerID", data.containerID)
	return setValue(&container.Status.ContainerID, data.containerID)
}

func (r *PhysicalContainerReconciler) copyPhysicalContainerCreateFiles(
	ctx context.Context,
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	fileModTime time.Time,
	log logr.Logger,
) {
	for _, createFileRequest := range container.Spec.Container.CreateFiles {
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
			data.state = physicalContainerStateCopyFiles
			data.progress = physicalContainerOperationFailed
			data.failureMessage = fmt.Sprintf("Failed to copy files to physical container: %v", copyErr)
			r.queuePhysicalContainerDataResult(container, stateKey, data)
			return
		}

		log.V(1).Info("Files copied to the physical container", "ContainerID", data.containerID, "Destination", createFileRequest.Destination)
	}

	data.state = physicalContainerStateCopyFiles
	data.progress = physicalContainerOperationCompleted
	data.failureMessage = ""
	r.queuePhysicalContainerDataResult(container, stateKey, data)
}

func (r *PhysicalContainerReconciler) schedulePhysicalContainerStart(
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	scheduledData := data.Clone()
	scheduledData.state = physicalContainerStateStart
	scheduledData.progress = physicalContainerOperationInProgress
	scheduledData.failureMessage = ""
	if !r.containerData.Update(container.NamespacedName(), stateKey, scheduledData) {
		return setValue(&container.Status.ContainerID, data.containerID) | additionalReconciliationNeeded
	}
	data.UpdateFrom(scheduledData)

	containerSnapshot := container.DeepCopy()
	dataSnapshot := scheduledData.Clone()
	enqueueErr := r.operationQueue.Enqueue(func(operationCtx context.Context) {
		r.startPhysicalContainer(operationCtx, containerSnapshot, stateKey, dataSnapshot, log)
	})
	if enqueueErr != nil {
		data.state = physicalContainerStateStart
		data.progress = physicalContainerOperationFailed
		data.failureMessage = fmt.Sprintf("Failed to queue physical container start: %v", enqueueErr)
		log.Error(enqueueErr, "Failed to queue PhysicalContainer start", "ContainerID", data.containerID)
		return noChange
	}

	log.V(1).Info("Queued PhysicalContainer start", "ContainerID", data.containerID)
	return setValue(&container.Status.ContainerID, data.containerID)
}

func (r *PhysicalContainerReconciler) skipPhysicalContainerStart(
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	data.state = physicalContainerStateStart
	data.progress = physicalContainerOperationCompleted
	data.failureMessage = ""
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
		data.state = physicalContainerStateStart
		data.progress = physicalContainerOperationFailed
		data.failureMessage = fmt.Sprintf("Failed to start physical container: %v", startErr)
	} else {
		data.state = physicalContainerStateStart
		data.progress = physicalContainerOperationCompleted
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
			owner, updated := r.containerData.UpdateChangingStateKeyIfUnclaimed(name, currentStateKey, newStateKey, result)
			if !updated && owner != (types.NamespacedName{}) && owner != name {
				conflictedResult := result.Clone()
				conflictedResult.state = physicalContainerStateResolve
				conflictedResult.progress = physicalResourceProgressRetryPending
				conflictedResult.failureMessage = fmt.Sprintf("Runtime container is already tracked by PhysicalContainer %q.", owner.String())
				conflictedResult.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
				_ = r.containerData.Update(name, currentStateKey, conflictedResult)
			}
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

func (r *PhysicalContainerReconciler) handleDeletionRequest(
	ctx context.Context,
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	log logr.Logger,
) objectChange {
	if data.operationInProgress() {
		log.V(1).Info("Physical container is being deleted while an operation is in progress", "State", data.state)
		return additionalReconciliationNeeded
	}

	containerID := data.containerID
	if containerID == "" {
		containerID = container.Spec.ContainerID
	}

	if container.Spec.Container != nil && !container.Spec.Container.RetainRuntimeContainer && containerID != "" {
		_, removeErr := r.orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
			Containers: []string{containerID},
			Force:      true,
		})
		if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
			log.Error(removeErr, "Failed to remove runtime container", "ContainerID", containerID)
			data.state = physicalContainerStateRemove
			data.progress = physicalResourceProgressRetryPending
			data.containerID = containerID
			data.failureMessage = fmt.Sprintf("Failed to remove runtime container: %v", removeErr)
			return setValue(&container.Status.ContainerID, containerID) | additionalReconciliationNeeded
		}
	}

	r.discardPhysicalContainerData(container.NamespacedName(), container.UID, data, log)
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

// Removes in-memory state and releases the runtime event watch for a PhysicalContainer.
func (r *PhysicalContainerReconciler) discardPhysicalContainerData(
	name types.NamespacedName,
	resourceUID types.UID,
	data *physicalContainerData,
	log logr.Logger,
) {
	if data == nil {
		_, data = r.containerData.BorrowByNamespacedName(name)
	}
	if data != nil && data.resourceUID != "" {
		resourceUID = data.resourceUID
	}

	r.containerData.DeleteByNamespacedName(name)
	if r.ContainerWatcher != nil && resourceUID != "" {
		r.ReleaseContainerWatchForResource(resourceUID, log)
	}
}

// physicalPortsToCreateContainerPorts expands each V2 container port range into one orchestrator
// port per concrete container port.
func physicalPortsToCreateContainerPorts(ports []apiv2.ContainerPort) []containers.CreateContainerPort {
	retval := make([]containers.CreateContainerPort, 0, len(ports))
	for _, port := range ports {
		for portOffset := int32(0); portOffset < port.EffectiveRangeSize(); portOffset++ {
			containerPort := port.ContainerPort + portOffset
			hostPort := port.HostPort
			if hostPort != 0 {
				hostPort += portOffset
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
	return physicalResourceCreationLabels(
		container.Spec.Container.Labels,
		container.Spec.Container.RetainRuntimeContainer,
		container.UID,
		log,
	)
}

func applyInspectedPhysicalContainerDetails(container *apiv2.PhysicalContainer, inspectedContainer *containers.InspectedContainer, _ logr.Logger) objectChange {
	change := noChange
	change |= setValue(&container.Status.ContainerID, inspectedContainer.Id)
	change |= setValue(&container.Status.ContainerName, inspectedContainer.Name)
	change |= setValue(&container.Status.RuntimeStatus, string(inspectedContainer.Status))
	change |= setTimestamp(&container.Status.CreatedAt, metav1.NewMicroTime(inspectedContainer.CreatedAt))
	change |= setTimestamp(&container.Status.StartedAt, metav1.NewMicroTime(inspectedContainer.StartedAt))
	change |= setTimestamp(&container.Status.FinishedAt, metav1.NewMicroTime(inspectedContainer.FinishedAt))
	change |= setPhysicalContainerExitCode(container, inspectedContainer)
	return change
}

func (r *PhysicalContainerReconciler) applyInspectedPhysicalContainerStatus(
	container *apiv2.PhysicalContainer,
	stateKey physicalContainerDataStateKey,
	data *physicalContainerData,
	inspectedContainer *containers.InspectedContainer,
	log logr.Logger,
) objectChange {
	change := applyInspectedPhysicalContainerDetails(container, inspectedContainer, log)
	portMappings, portMappingErr := physicalContainerPortMappingsFromInspected(inspectedContainer.Ports)
	if portMappingErr != nil {
		log.Error(portMappingErr, "Failed to resolve physical container port mappings", "ContainerID", inspectedContainer.Id)
		data.state = physicalContainerStatePortMapping
		data.progress = physicalResourceProgressRetryPending
		data.failureMessage = fmt.Sprintf("Failed to resolve physical container port mappings: %v", portMappingErr)
	} else {
		change |= setPhysicalContainerPortMappings(container, portMappings)
		data.progress = physicalResourceProgressCompleted
		data.failureMessage = ""
		switch inspectedContainer.Status {
		case containers.ContainerStatusRunning:
			data.state = physicalContainerStateRuntime
			data.progress = physicalResourceProgressRunning
		case containers.ContainerStatusPaused:
			data.state = physicalContainerStateRuntime
			data.progress = physicalResourceProgressPaused
		case containers.ContainerStatusRestarting:
			data.state = physicalContainerStateRuntime
			data.progress = physicalResourceProgressRestarting
		case containers.ContainerStatusCreated:
			data.state = physicalContainerStateRuntime
			data.progress = physicalResourceProgressCreated
		case containers.ContainerStatusRemoving:
			data.state = physicalContainerStateRuntime
			data.progress = physicalResourceProgressRemoving
		case containers.ContainerStatusExited:
			data.state = physicalContainerStateRuntime
			data.progress = physicalResourceProgressExited
		case containers.ContainerStatusDead:
			data.state = physicalContainerStateRuntime
			data.progress = physicalResourceProgressDead
		default:
			data.state = physicalContainerStateRuntime
			data.progress = physicalResourceProgressUnknown
			data.failureMessage = fmt.Sprintf("Runtime container returned unrecognized status %q.", inspectedContainer.Status)
		}
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
