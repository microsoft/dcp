/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	stdio "io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/resiliency"
	dcpslices "github.com/microsoft/dcp/pkg/slices"
)

var (
	physicalContainerImageFinalizer    string = fmt.Sprintf("%s/physicalcontainerimage-reconciler", apiv2.GroupVersion.Group)
	errPhysicalContainerImageIDMissing        = errors.New("image ID file is empty")

	physicalContainerImageDataHandlers = map[physicalContainerImageState]physicalContainerImageDataHandlerFunc{
		physicalContainerImageStateNamespace: handlePhysicalContainerImageNamespace,
		physicalContainerImageStateResolve:   handlePhysicalContainerImageResolve,
		physicalContainerImageStatePull:      handlePhysicalContainerImageOperation,
		physicalContainerImageStateBuild:     handlePhysicalContainerImageOperation,
		physicalContainerImageStateRuntime:   handlePhysicalContainerImageRuntime,
		physicalContainerImageStateDelete:    handlePhysicalContainerImageDelete,
		physicalContainerImageStateInvalid:   handlePhysicalContainerImageTerminal,
		0:                                    handleUnknownPhysicalContainerImageState,
	}
)

type physicalContainerImageDataHandlerFunc = stateInitializerFunc[
	apiv2.PhysicalContainerImage, *apiv2.PhysicalContainerImage,
	PhysicalContainerImageReconciler, *PhysicalContainerImageReconciler,
	physicalContainerImageState,
	physicalContainerImageData, *physicalContainerImageData,
]

const (
	// Image pulls retry with exponential backoff to absorb transient registry and network failures.
	// The budget is deliberately small so an unreachable or misspelled image still reports failure promptly.
	imagePullRetryInitialInterval = 1 * time.Second
	imagePullRetryMaxInterval     = 5 * time.Second
	imagePullRetryMaxElapsedTime  = 15 * time.Second

	// Number of pull retries used when the image does not specify PullRetryLimit.
	defaultImagePullRetryLimit int32 = 3
)

// Builds the retry policy for pulling the given image. A PullRetryLimit of zero disables
// retries entirely, so the pull fails as soon as the first attempt does.
func imagePullBackoff(image *apiv2.PhysicalContainerImage) backoff.BackOff {
	retryLimit := defaultImagePullRetryLimit
	if image.Spec.Image.PullRetryLimit != nil {
		retryLimit = *image.Spec.Image.PullRetryLimit
	}
	if retryLimit <= 0 {
		return &backoff.StopBackOff{}
	}

	return backoff.WithMaxRetries(
		backoff.NewExponentialBackOff(
			backoff.WithInitialInterval(imagePullRetryInitialInterval),
			backoff.WithMaxInterval(imagePullRetryMaxInterval),
			backoff.WithMaxElapsedTime(imagePullRetryMaxElapsedTime),
		),
		uint64(retryLimit),
	)
}

type PhysicalContainerImageReconciler struct {
	*ReconcilerBase[apiv2.PhysicalContainerImage, *apiv2.PhysicalContainerImage]

	orchestrator   containers.ImageOrchestrator
	imageData      *ObjectStateMap[physicalContainerImageDataStateKey, physicalContainerImageData, *physicalContainerImageData, *apiv2.PhysicalContainerImage]
	operationQueue *resiliency.WorkQueue
}

func NewPhysicalContainerImageReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	orchestrator containers.ImageOrchestrator,
) *PhysicalContainerImageReconciler {
	return &PhysicalContainerImageReconciler{
		ReconcilerBase: NewReconcilerBase[apiv2.PhysicalContainerImage](client, noCacheClient, log, lifetimeCtx),
		orchestrator:   orchestrator,
		imageData:      NewObjectStateMap[physicalContainerImageDataStateKey, physicalContainerImageData, *physicalContainerImageData, *apiv2.PhysicalContainerImage](),
		operationQueue: resiliency.NewWorkQueue(lifetimeCtx, MaxConcurrentReconciles),
	}
}

func (r *PhysicalContainerImageReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.PhysicalContainerImage{}).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNamespace(&apiv2.PhysicalContainerImageList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *PhysicalContainerImageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	image := apiv2.PhysicalContainerImage{}
	getErr := reader.Get(ctx, req.NamespacedName, &image)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			log.V(1).Info("PhysicalContainerImage not found, nothing to do...")
			// The finalizer normally guarantees the deletion is observed, but drop any lingering
			// state in case the object disappeared without it (for example a forced deletion).
			r.discardPhysicalContainerImageData(req.NamespacedName, log)
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}

		log.Error(getErr, "Failed to Get() the PhysicalContainerImage")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)

	r.imageData.RunDeferredOps(req.NamespacedName, &image)

	var change objectChange
	reconciliationDelay := StandardDelay
	patch := ctrl_client.MergeFromWithOptions(image.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if image.DeletionTimestamp != nil && !image.DeletionTimestamp.IsZero() {
		change, reconciliationDelay = r.managePhysicalContainerImage(ctx, &image, log)
	} else if change = ensureFinalizer(&image, physicalContainerImageFinalizer, log); change != noChange {
		// Make additional changes during the next reconciliation.
	} else {
		change, reconciliationDelay = r.managePhysicalContainerImage(ctx, &image, log)
	}

	return r.SaveChangesWithDelay(ctx, &image, patch, change, reconciliationDelay, nil, log)
}

// Removes in-memory state for the image, cancelling the pull or build operation if one may still be running.
func (r *PhysicalContainerImageReconciler) discardPhysicalContainerImageData(name types.NamespacedName, log logr.Logger) {
	_, data := r.imageData.BorrowByNamespacedName(name)
	if data == nil {
		return
	}

	if data.operationInProgress() && data.cancelOperation != nil {
		log.V(1).Info("Cancelling in-flight PhysicalContainerImage operation", "State", data.state)
		data.cancelOperation()
	}

	r.imageData.DeleteByNamespacedName(name)
}

func (r *PhysicalContainerImageReconciler) managePhysicalContainerImage(
	ctx context.Context,
	image *apiv2.PhysicalContainerImage,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	_, data := r.imageData.BorrowByNamespacedName(image.NamespacedName())
	if data == nil {
		data = &physicalContainerImageData{
			state:    physicalContainerImageStateNamespace,
			progress: physicalResourceProgressNotReady,
		}
		initialStateKey := physicalContainerImageDataKey(image)
		// Store() retains the supplied pointer, so keep an unaliased copy for this reconciliation.
		r.imageData.Store(image.NamespacedName(), initialStateKey, data.Clone())
	}

	handler := getStateInitializer(physicalContainerImageDataHandlers, data.state, log)
	change := handler(ctx, r, image, data.state, data, log)

	_, currentData := r.imageData.BorrowByNamespacedName(image.NamespacedName())
	if currentData == nil {
		return change, StandardDelay
	}
	dataChange, delay, valid := currentData.applyTo(image)
	change |= dataChange
	if !valid {
		log.Error(
			fmt.Errorf("invalid physical container image state %v with progress %v", currentData.state, currentData.progress),
			"PhysicalContainerImage reached invalid reconciliation state",
		)
	}
	return change, delay
}

func handlePhysicalContainerImageNamespace(
	ctx context.Context,
	reconciler *PhysicalContainerImageReconciler,
	image *apiv2.PhysicalContainerImage,
	_ physicalContainerImageState,
	data *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	if image.DeletionTimestamp != nil && !image.DeletionTimestamp.IsZero() {
		return beginPhysicalContainerImageDeletion(ctx, reconciler, image, data, log)
	}
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, reconciler.Client, image.Namespace)
	if !namespaceReady {
		data.state = physicalContainerImageStateNamespace
		data.failureMessage = namespaceReadinessMessage(image.Namespace, namespaceReason)
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
			log.Error(namespaceErr, "Failed to get namespace", "Namespace", image.Namespace)
			data.progress = physicalResourceProgressRetryPending
			data.failureMessage = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
		}
		_ = reconciler.imageData.UpdateByNamespacedName(image.NamespacedName(), data)
		return noChange
	}

	data.state = physicalContainerImageStateResolve
	data.progress = physicalResourceProgressInProgress
	data.failureMessage = ""
	if !reconciler.imageData.UpdateByNamespacedName(image.NamespacedName(), data) {
		return additionalReconciliationNeeded
	}
	return handlePhysicalContainerImageResolve(ctx, reconciler, image, data.state, data, log)
}

func handlePhysicalContainerImageResolve(
	ctx context.Context,
	reconciler *PhysicalContainerImageReconciler,
	image *apiv2.PhysicalContainerImage,
	_ physicalContainerImageState,
	data *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	if image.DeletionTimestamp != nil && !image.DeletionTimestamp.IsZero() {
		return beginPhysicalContainerImageDeletion(ctx, reconciler, image, data, log)
	}
	if image.Spec.ImageID != "" {
		change, _ := reconciler.ensureExistingImage(ctx, image, data, log)
		return change
	}
	if image.Spec.Image.Build != nil {
		change, _ := reconciler.ensureBuiltImage(ctx, image, data, log)
		return change
	}
	change, _ := reconciler.ensurePulledImage(ctx, image, data, log)
	return change
}

func handlePhysicalContainerImageOperation(
	ctx context.Context,
	reconciler *PhysicalContainerImageReconciler,
	image *apiv2.PhysicalContainerImage,
	state physicalContainerImageState,
	data *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	if image.DeletionTimestamp != nil && !image.DeletionTimestamp.IsZero() {
		return beginPhysicalContainerImageDeletion(ctx, reconciler, image, data, log)
	}
	if data.progress == physicalResourceProgressInProgress ||
		data.progress == physicalResourceProgressFailed ||
		(data.progress == physicalResourceProgressResultMissing &&
			state == physicalContainerImageStateBuild) {
		return noChange
	}
	if data.progress == physicalResourceProgressResultMissing {
		if time.Now().Before(data.retryAfter) {
			return additionalReconciliationNeeded
		}
		change, _ := reconciler.schedulePhysicalContainerImagePull(image, image.Spec.Image.Image, log)
		return change
	}
	if data.progress != physicalResourceProgressCompleted || data.imageID == "" {
		return handleUnknownPhysicalContainerImageState(ctx, reconciler, image, state, data, log)
	}

	return reconciler.inspectPhysicalContainerImageOperationResult(ctx, image, data, log)
}

func handlePhysicalContainerImageRuntime(
	ctx context.Context,
	reconciler *PhysicalContainerImageReconciler,
	image *apiv2.PhysicalContainerImage,
	_ physicalContainerImageState,
	data *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	if image.DeletionTimestamp != nil && !image.DeletionTimestamp.IsZero() {
		return beginPhysicalContainerImageDeletion(ctx, reconciler, image, data, log)
	}
	if data.progress == physicalResourceProgressFailed {
		return noChange
	}
	if data.imageID != "" {
		return reconciler.inspectPhysicalContainerImageOperationResult(ctx, image, data, log)
	}

	data.state = physicalContainerImageStateResolve
	data.progress = physicalResourceProgressInProgress
	if !reconciler.imageData.UpdateByNamespacedName(image.NamespacedName(), data) {
		return additionalReconciliationNeeded
	}
	return handlePhysicalContainerImageResolve(ctx, reconciler, image, data.state, data, log)
}

func (r *PhysicalContainerImageReconciler) inspectPhysicalContainerImageOperationResult(
	ctx context.Context,
	image *apiv2.PhysicalContainerImage,
	data *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	inspectedImage, inspectErr := inspectPhysicalContainerImage(ctx, r.orchestrator, data.imageID)
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect completed PhysicalContainerImage operation", "ImageID", data.imageID)
		data.state = physicalContainerImageStateRuntime
		data.progress = physicalResourceProgressRetryPending
		data.failureMessage = fmt.Sprintf("Failed to inspect image: %v", inspectErr)
		_ = r.imageData.UpdateByNamespacedName(image.NamespacedName(), data)
		return noChange
	}

	data.state = physicalContainerImageStateRuntime
	data.progress = physicalResourceProgressCompleted
	data.failureMessage = ""
	_ = r.imageData.UpdateByNamespacedName(image.NamespacedName(), data)
	log.V(1).Info("PhysicalContainerImage operation completed; saving image status", "ImageID", data.imageID)
	change, _ := applyReadyPhysicalContainerImageStatus(image, data.image, inspectedImage)
	return change
}

func handlePhysicalContainerImageTerminal(
	ctx context.Context,
	reconciler *PhysicalContainerImageReconciler,
	image *apiv2.PhysicalContainerImage,
	_ physicalContainerImageState,
	data *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	if image.DeletionTimestamp != nil && !image.DeletionTimestamp.IsZero() {
		return beginPhysicalContainerImageDeletion(ctx, reconciler, image, data, log)
	}
	return noChange
}

func beginPhysicalContainerImageDeletion(
	ctx context.Context,
	reconciler *PhysicalContainerImageReconciler,
	image *apiv2.PhysicalContainerImage,
	data *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	data.state = physicalContainerImageStateDelete
	data.progress = physicalResourceProgressInProgress
	_ = reconciler.imageData.UpdateByNamespacedName(image.NamespacedName(), data)
	return handlePhysicalContainerImageDelete(ctx, reconciler, image, data.state, data, log)
}

func handlePhysicalContainerImageDelete(
	_ context.Context,
	reconciler *PhysicalContainerImageReconciler,
	image *apiv2.PhysicalContainerImage,
	_ physicalContainerImageState,
	_ *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	reconciler.discardPhysicalContainerImageData(image.NamespacedName(), log)
	return deleteFinalizer(image, physicalContainerImageFinalizer, log)
}

func handleUnknownPhysicalContainerImageState(
	ctx context.Context,
	reconciler *PhysicalContainerImageReconciler,
	image *apiv2.PhysicalContainerImage,
	state physicalContainerImageState,
	data *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	if image.DeletionTimestamp != nil && !image.DeletionTimestamp.IsZero() {
		return beginPhysicalContainerImageDeletion(ctx, reconciler, image, data, log)
	}
	invalidProgress := data.progress
	data.state = physicalContainerImageStateInvalid
	data.progress = physicalResourceProgressFailed
	data.failureMessage = fmt.Sprintf("PhysicalContainerImage reached invalid reconciliation state %v with progress %v.", state, invalidProgress)
	_ = reconciler.imageData.UpdateByNamespacedName(image.NamespacedName(), data)
	log.Error(fmt.Errorf("invalid PhysicalContainerImage state %v with progress %v", state, invalidProgress), "PhysicalContainerImage reached invalid reconciliation state")
	return additionalReconciliationNeeded
}

func (r *PhysicalContainerImageReconciler) ensurePulledImage(
	ctx context.Context,
	image *apiv2.PhysicalContainerImage,
	data *physicalContainerImageData,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	imageConfig := image.Spec.Image
	if imageConfig.PullPolicy == apiv2.PullPolicyAlways {
		return r.schedulePhysicalContainerImagePull(image, imageConfig.Image, log)
	}

	inspectedImage, inspectErr := inspectPhysicalContainerImage(ctx, r.orchestrator, imageConfig.Image)
	if inspectErr == nil {
		data.state = physicalContainerImageStateRuntime
		data.progress = physicalResourceProgressCompleted
		data.image = imageConfig.Image
		data.imageID = inspectedImage.Id
		data.failureMessage = ""
		_ = r.imageData.UpdateByNamespacedName(image.NamespacedName(), data)
		return applyReadyPhysicalContainerImageStatus(image, imageConfig.Image, inspectedImage)
	}
	if !errors.Is(inspectErr, containers.ErrNotFound) {
		log.Error(inspectErr, "Failed to inspect PhysicalContainerImage source image", "Image", imageConfig.Image)
		data.state = physicalContainerImageStateRuntime
		data.progress = physicalResourceProgressRetryPending
		data.failureMessage = fmt.Sprintf("Failed to inspect image: %v", inspectErr)
		_ = r.imageData.UpdateByNamespacedName(image.NamespacedName(), data)
		return noChange, LongDelay
	}
	if imageConfig.PullPolicy == apiv2.PullPolicyNever {
		data.state = physicalContainerImageStateRuntime
		data.progress = physicalResourceProgressFailed
		data.failureMessage = fmt.Sprintf("Image %q is not available locally.", imageConfig.Image)
		_ = r.imageData.UpdateByNamespacedName(image.NamespacedName(), data)
		return noChange, StandardDelay
	}

	return r.schedulePhysicalContainerImagePull(image, imageConfig.Image, log)
}

func (r *PhysicalContainerImageReconciler) ensureBuiltImage(
	ctx context.Context,
	image *apiv2.PhysicalContainerImage,
	data *physicalContainerImageData,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	imageConfig := image.Spec.Image
	outputImage := physicalContainerImageOutputTag(image)
	buildContext := *imageConfig.Build
	buildContext.Tags = append([]string{}, buildContext.Tags...)
	buildContext.Args = append([]commonapi.EnvVar{}, buildContext.Args...)
	buildContext.Secrets = append([]apiv2.ContainerBuildSecret{}, buildContext.Secrets...)
	buildContext.Labels = physicalResourceCreationLabels(buildContext.Labels, true, image.UID, log)
	buildContext.Tags = physicalContainerImageBuildTags(buildContext.Tags, outputImage)

	return r.schedulePhysicalContainerImageBuild(image, outputImage, &buildContext, log)
}

func (r *PhysicalContainerImageReconciler) ensureExistingImage(
	ctx context.Context,
	image *apiv2.PhysicalContainerImage,
	data *physicalContainerImageData,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	inspectedImage, inspectErr := inspectPhysicalContainerImage(ctx, r.orchestrator, image.Spec.ImageID)
	if inspectErr == nil {
		data.state = physicalContainerImageStateRuntime
		data.progress = physicalResourceProgressCompleted
		data.image = image.Spec.ImageID
		data.imageID = inspectedImage.Id
		data.failureMessage = ""
		_ = r.imageData.UpdateByNamespacedName(image.NamespacedName(), data)
		return applyReadyPhysicalContainerImageStatus(image, image.Spec.ImageID, inspectedImage)
	}
	if errors.Is(inspectErr, containers.ErrNotFound) {
		data.state = physicalContainerImageStateRuntime
		data.progress = physicalResourceProgressFailed
		data.failureMessage = fmt.Sprintf("Image %q is not available locally.", image.Spec.ImageID)
		_ = r.imageData.UpdateByNamespacedName(image.NamespacedName(), data)
		return noChange, StandardDelay
	}

	log.Error(inspectErr, "Failed to inspect existing PhysicalContainerImage", "ImageID", image.Spec.ImageID)
	data.state = physicalContainerImageStateRuntime
	data.progress = physicalResourceProgressRetryPending
	data.failureMessage = fmt.Sprintf("Failed to inspect image: %v", inspectErr)
	_ = r.imageData.UpdateByNamespacedName(image.NamespacedName(), data)
	return noChange, LongDelay
}

func (r *PhysicalContainerImageReconciler) schedulePhysicalContainerImagePull(
	image *apiv2.PhysicalContainerImage,
	outputImage string,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	stateKey := physicalContainerImageDataKey(image)
	operationCtx, cancelOperation := context.WithCancel(r.LifetimeCtx)
	data := &physicalContainerImageData{
		state:           physicalContainerImageStatePull,
		progress:        physicalResourceProgressInProgress,
		image:           outputImage,
		cancelOperation: cancelOperation,
	}
	r.imageData.Store(image.NamespacedName(), stateKey, data)
	imageSnapshot := image.DeepCopy()
	dataSnapshot := data.Clone()
	// The work queue supplies the reconciler lifetime context; operationCtx derives from it and
	// additionally lets deletion of the image cancel the pull.
	enqueueErr := r.operationQueue.Enqueue(func(context.Context) {
		defer cancelOperation()
		r.pullPhysicalContainerImage(operationCtx, imageSnapshot, stateKey, dataSnapshot, outputImage, log)
	})
	if enqueueErr != nil {
		cancelOperation()
		log.Error(enqueueErr, "Failed to queue PhysicalContainerImage pull", "Image", outputImage)
		data.progress = physicalResourceProgressFailed
		data.failureMessage = fmt.Sprintf("Failed to queue image pull: %v", enqueueErr)
		_ = r.imageData.Update(image.NamespacedName(), stateKey, data)
		change, delay, _ := data.applyTo(image)
		return change, delay
	}

	log.V(1).Info("Queued PhysicalContainerImage pull", "Image", outputImage)
	change, delay, _ := data.applyTo(image)
	return change, delay
}

func (r *PhysicalContainerImageReconciler) schedulePhysicalContainerImageBuild(
	image *apiv2.PhysicalContainerImage,
	outputImage string,
	buildContext *apiv2.ContainerBuildContext,
	log logr.Logger,
) (objectChange, AdditionalReconciliationDelay) {
	stateKey := physicalContainerImageDataKey(image)
	operationCtx, cancelOperation := context.WithCancel(r.LifetimeCtx)
	data := &physicalContainerImageData{
		state:           physicalContainerImageStateBuild,
		progress:        physicalResourceProgressInProgress,
		image:           outputImage,
		cancelOperation: cancelOperation,
	}
	r.imageData.Store(image.NamespacedName(), stateKey, data)
	imageSnapshot := image.DeepCopy()
	dataSnapshot := data.Clone()
	buildContextSnapshot := *buildContext
	// The work queue supplies the reconciler lifetime context; operationCtx derives from it and
	// additionally lets deletion of the image cancel the build.
	enqueueErr := r.operationQueue.Enqueue(func(context.Context) {
		defer cancelOperation()
		r.buildPhysicalContainerImage(operationCtx, imageSnapshot, stateKey, dataSnapshot, outputImage, &buildContextSnapshot, log)
	})
	if enqueueErr != nil {
		cancelOperation()
		log.Error(enqueueErr, "Failed to queue PhysicalContainerImage build", "Image", outputImage)
		data.progress = physicalResourceProgressFailed
		data.failureMessage = fmt.Sprintf("Failed to queue image build: %v", enqueueErr)
		_ = r.imageData.Update(image.NamespacedName(), stateKey, data)
		change, delay, _ := data.applyTo(image)
		return change, delay
	}

	log.V(1).Info("Queued PhysicalContainerImage build", "Context", buildContext.Context, "Dockerfile", buildContext.Dockerfile, "Image", outputImage)
	change, delay, _ := data.applyTo(image)
	return change, delay
}

func (r *PhysicalContainerImageReconciler) pullPhysicalContainerImage(
	ctx context.Context,
	image *apiv2.PhysicalContainerImage,
	stateKey physicalContainerImageDataStateKey,
	data *physicalContainerImageData,
	outputImage string,
	log logr.Logger,
) {
	log.V(1).Info("Pulling PhysicalContainerImage source image", "Image", outputImage)
	attempt := 0
	pulledImageID, pullErr := resiliency.RetryGet(ctx, imagePullBackoff(image), func() (string, error) {
		attempt++
		imageID, attemptErr := r.orchestrator.PullImage(ctx, containers.PullImageOptions{Image: outputImage})
		if attemptErr != nil {
			log.V(1).Info("PhysicalContainerImage pull attempt failed", "Image", outputImage, "Attempt", attempt, "Error", attemptErr)
		}
		return imageID, attemptErr
	})
	if pullErr != nil {
		log.Error(pullErr, "Failed to pull PhysicalContainerImage source image", "Image", outputImage)
		data.progress = physicalResourceProgressFailed
		data.failureMessage = fmt.Sprintf("Failed to pull image: %v", pullErr)
	} else if pulledImageID == "" {
		data.state = physicalContainerImageStatePull
		data.progress = physicalResourceProgressResultMissing
		data.failureMessage = "Image pull completed without an image ID."
		data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
	} else {
		data.progress = physicalResourceProgressCompleted
		data.imageID = pulledImageID
		data.failureMessage = ""
		data.retryAfter = time.Time{}
	}

	r.queuePhysicalContainerImageDataResult(image, stateKey, data)
}

func (r *PhysicalContainerImageReconciler) buildPhysicalContainerImage(
	ctx context.Context,
	image *apiv2.PhysicalContainerImage,
	stateKey physicalContainerImageDataStateKey,
	data *physicalContainerImageData,
	outputImage string,
	buildContext *apiv2.ContainerBuildContext,
	log logr.Logger,
) {
	log.V(1).Info("Building PhysicalContainerImage", "Context", buildContext.Context, "Dockerfile", buildContext.Dockerfile, "Image", outputImage)
	defer r.queuePhysicalContainerImageDataResult(image, stateKey, data)

	iidFile, openErr := usvc_io.OpenTempFile(fmt.Sprintf("%s_iid_%s", image.Name, image.UID), os.O_RDWR|os.O_CREATE|os.O_TRUNC, osutil.PermissionOnlyOwnerReadWrite)
	if openErr != nil {
		log.Error(openErr, "Failed to create PhysicalContainerImage build image ID file", "Image", outputImage)
		data.progress = physicalResourceProgressFailed
		data.failureMessage = fmt.Sprintf("Failed to create image ID file: %v", openErr)
		return
	}
	iidFileName := iidFile.Name()
	defer func() {
		removeErr := os.Remove(iidFileName)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			log.Error(removeErr, "Failed to remove PhysicalContainerImage build image ID file", "Path", iidFileName)
		}
	}()
	closeErr := iidFile.Close()
	if closeErr != nil {
		log.Error(closeErr, "Failed to close PhysicalContainerImage build image ID file", "Image", outputImage)
		data.progress = physicalResourceProgressFailed
		data.failureMessage = fmt.Sprintf("Failed to close image ID file: %v", closeErr)
		return
	}

	buildErr := r.orchestrator.BuildImage(ctx, containers.BuildImageOptions{
		Pull:                  image.Spec.Image.PullPolicy == apiv2.PullPolicyAlways,
		IidFile:               iidFileName,
		ContainerBuildContext: v2BuildContextToContainerBuildContext(buildContext),
	})
	if buildErr != nil {
		log.Error(buildErr, "Failed to build PhysicalContainerImage", "Image", outputImage)
		data.progress = physicalResourceProgressFailed
		data.failureMessage = fmt.Sprintf("Failed to build image: %v", buildErr)
		return
	}

	imageID, readErr := readPhysicalContainerImageIDFile(iidFileName)
	if readErr != nil {
		log.Error(readErr, "Failed to read PhysicalContainerImage build image ID", "Image", outputImage)
		if errors.Is(readErr, errPhysicalContainerImageIDMissing) {
			data.state = physicalContainerImageStateBuild
			data.progress = physicalResourceProgressResultMissing
		} else {
			data.state = physicalContainerImageStateBuild
			data.progress = physicalResourceProgressFailed
		}
		data.failureMessage = fmt.Sprintf("Failed to read image ID: %v", readErr)
		return
	}

	data.progress = physicalResourceProgressCompleted
	data.imageID = imageID
	data.failureMessage = ""
}

func (r *PhysicalContainerImageReconciler) queuePhysicalContainerImageDataResult(
	image *apiv2.PhysicalContainerImage,
	stateKey physicalContainerImageDataStateKey,
	result *physicalContainerImageData,
) {
	queued := r.imageData.QueueDeferredOpForStateKey(image.NamespacedName(), stateKey, func(name types.NamespacedName, currentStateKey physicalContainerImageDataStateKey, _ *apiv2.PhysicalContainerImage) {
		_ = r.imageData.Update(name, currentStateKey, result)
	})
	if queued {
		r.ScheduleReconciliation(image.NamespacedName())
	}
}

func readPhysicalContainerImageIDFile(name string) (string, error) {
	file, openErr := usvc_io.OpenFile(name, os.O_RDONLY, osutil.PermissionOnlyOwnerReadWrite)
	if openErr != nil {
		return "", fmt.Errorf("open image ID file: %w", openErr)
	}

	contents, readErr := stdio.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return "", fmt.Errorf("read image ID file: %w", readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close image ID file: %w", closeErr)
	}

	imageID := strings.TrimSpace(string(contents))
	if imageID == "" {
		return "", errPhysicalContainerImageIDMissing
	}
	return imageID, nil
}

func inspectPhysicalContainerImage(ctx context.Context, orchestrator containers.ImageOrchestrator, image string) (*containers.InspectedImage, error) {
	inspectedImages, inspectErr := orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
		Images: []string{image},
	})
	if inspectErr != nil {
		return nil, inspectErr
	}
	if len(inspectedImages) == 0 {
		return nil, containers.ErrNotFound
	}

	return &inspectedImages[0], nil
}

func physicalContainerImageOutputTag(image *apiv2.PhysicalContainerImage) string {
	if image.Spec.Image.Image != "" {
		return image.Spec.Image.Image
	}
	if image.Spec.Image.Build != nil && len(image.Spec.Image.Build.Tags) > 0 {
		return image.Spec.Image.Build.Tags[0]
	}

	uid := "latest"
	if image.UID != "" {
		uid = string(image.UID)
	}
	return fmt.Sprintf("dcp-v2-%s-%s:%s", image.Namespace, image.Name, uid)
}

func physicalContainerImageBuildTags(tags []string, outputImage string) []string {
	if len(tags) == 0 {
		return []string{outputImage}
	}
	if tags[0] == outputImage {
		return tags
	}

	buildTags := make([]string, 0, len(tags)+1)
	buildTags = append(buildTags, outputImage)
	for _, tag := range tags {
		if tag != outputImage {
			buildTags = append(buildTags, tag)
		}
	}
	return buildTags
}

func applyReadyPhysicalContainerImageStatus(
	image *apiv2.PhysicalContainerImage,
	outputImage string,
	inspectedImage *containers.InspectedImage,
) (objectChange, AdditionalReconciliationDelay) {
	change := noChange
	change |= setValue(&image.Status.Image, outputImage)
	change |= setValue(&image.Status.ImageID, inspectedImage.Id)
	change |= setValue(&image.Status.Digest, inspectedImage.Digest)
	change |= setPhysicalContainerImageTags(image, inspectedImage.Tags)
	stateChange, delay, _ := physicalContainerImageProjections.apply(
		physicalContainerImageStateRuntime,
		physicalResourceProgressCompleted,
		"",
		&image.Status.Phase,
		&image.Status.Conditions,
		image.Generation,
	)
	return change | stateChange, delay
}

func setPhysicalContainerImageTags(image *apiv2.PhysicalContainerImage, tags []string) objectChange {
	if slices.Equal(image.Status.Tags, tags) {
		return noChange
	}
	image.Status.Tags = append([]string{}, tags...)
	return statusChanged
}

func v2BuildContextToContainerBuildContext(build *apiv2.ContainerBuildContext) *containers.ContainerBuildContext {
	if build == nil {
		return nil
	}

	return &containers.ContainerBuildContext{
		Context:    build.Context,
		Dockerfile: build.Dockerfile,
		Tags:       build.Tags,
		Args:       build.Args,
		Secrets: dcpslices.Map[containers.ContainerBuildSecret](build.Secrets, func(secret apiv2.ContainerBuildSecret) containers.ContainerBuildSecret {
			return containers.ContainerBuildSecret{
				Type:   containers.BuildSecretType(secret.Type),
				ID:     secret.ID,
				Source: secret.Source,
				Value:  secret.Value,
			}
		}),
		Stage:    build.Stage,
		Labels:   build.Labels,
		Platform: build.Platform,
	}
}
