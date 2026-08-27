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
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/resiliency"
	dcpslices "github.com/microsoft/dcp/pkg/slices"
)

var (
	physicalContainerImageFinalizer    string = fmt.Sprintf("%s/physicalcontainerimage-reconciler", apiv2.GroupVersion.Group)
	errPhysicalContainerImageIDMissing        = errors.New("image ID file is empty")

	physicalContainerImageDataInitializers = map[apiv2.ConditionReason]physicalContainerImageDataInitializerFunc{
		apiv2.PhysicalContainerImageReasonPulling:                   handlePhysicalContainerImageOperationInProgress,
		apiv2.PhysicalContainerImageReasonBuilding:                  handlePhysicalContainerImageOperationInProgress,
		apiv2.PhysicalContainerImageReasonPulled:                    handlePhysicalContainerImageOperationCompleted,
		apiv2.PhysicalContainerImageReasonBuilt:                     handlePhysicalContainerImageOperationCompleted,
		apiv2.PhysicalContainerImageReasonPullFailed:                handlePhysicalContainerImageOperationFailed,
		apiv2.PhysicalContainerImageReasonBuildFailed:               handlePhysicalContainerImageOperationFailed,
		apiv2.PhysicalContainerImageReasonBuildResultMissingImageID: handlePhysicalContainerImageOperationFailed,
		"": handleUnknownPhysicalContainerImageDataReason,
	}
)

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

type physicalContainerImageDataInitializerFunc = stateInitializerFunc[
	apiv2.PhysicalContainerImage, *apiv2.PhysicalContainerImage,
	PhysicalContainerImageReconciler, *PhysicalContainerImageReconciler,
	apiv2.ConditionReason,
	physicalContainerImageData, *physicalContainerImageData,
]

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
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForNamespace), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *PhysicalContainerImageReconciler) requestReconcileForNamespace(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	namespace := obj.(*apiv2.Namespace)
	var imageList apiv2.PhysicalContainerImageList
	listErr := r.List(ctx, &imageList, ctrl_client.InNamespace(namespace.Name))
	if listErr != nil {
		r.Log.Error(listErr, "Failed to list PhysicalContainerImages for namespace", "Namespace", namespace.Name)
		return nil
	}

	requests := make([]reconcile.Request, len(imageList.Items))
	for i := range imageList.Items {
		requests[i] = reconcile.Request{NamespacedName: imageList.Items[i].NamespacedName()}
	}

	r.Log.V(1).Info("Namespace updated, requesting PhysicalContainerImage reconciliation", "Namespace", namespace.Name, "Images", len(requests))
	return requests
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
	var onStatusDurable func()
	patch := ctrl_client.MergeFromWithOptions(image.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if image.DeletionTimestamp != nil && !image.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(&image, log)
	} else if change = ensureFinalizer(&image, physicalContainerImageFinalizer, log); change != noChange {
		// Make additional changes during the next reconciliation.
	} else {
		change, onStatusDurable = r.managePhysicalContainerImage(ctx, &image, log)
	}

	return r.SaveChangesWithDelay(ctx, &image, patch, change, physicalContainerImageReconcileDelay(&image), onStatusDurable, log)
}

func physicalContainerImageReconcileDelay(image *apiv2.PhysicalContainerImage) AdditionalReconciliationDelay {
	readyCondition := apimeta.FindStatusCondition(image.Status.Conditions, string(apiv2.ConditionReady))
	if readyCondition != nil {
		reason := apiv2.ConditionReason(readyCondition.Reason)
		if reason == apiv2.PhysicalContainerImageReasonRuntimeImageInspectFailed ||
			reason == apiv2.PhysicalContainerImageReasonPullResultMissingImageID ||
			reason == apiv2.PhysicalResourceReasonNamespaceLookupFailed ||
			reason == apiv2.PhysicalResourceReasonOperationStateInvalid {
			return LongDelay
		}
	}
	return StandardDelay
}

// Removes in-memory state for the image, cancelling the pull or build operation if one may still be running.
func (r *PhysicalContainerImageReconciler) discardPhysicalContainerImageData(name types.NamespacedName, log logr.Logger) {
	_, data := r.imageData.BorrowByNamespacedName(name)
	if data == nil {
		return
	}

	if data.operationInProgress() && data.cancelOperation != nil {
		log.V(1).Info("Cancelling in-flight PhysicalContainerImage operation", "Reason", data.conditionReason)
		data.cancelOperation()
	}

	r.imageData.DeleteByNamespacedName(name)
}

// Releases the resources tracked for a deleted image and removes the finalizer.
// The image itself is left in the container runtime; it is a shared artifact that
// outlives the resource describing it.
func (r *PhysicalContainerImageReconciler) handleDeletionRequest(image *apiv2.PhysicalContainerImage, log logr.Logger) objectChange {
	r.discardPhysicalContainerImageData(image.NamespacedName(), log)
	return deleteFinalizer(image, physicalContainerImageFinalizer, log)
}

func (r *PhysicalContainerImageReconciler) managePhysicalContainerImage(
	ctx context.Context,
	image *apiv2.PhysicalContainerImage,
	log logr.Logger,
) (objectChange, func()) {
	namespaceReady, namespaceReason, namespaceErr := checkNamespaceReady(ctx, r.Client, image.Namespace)
	if !namespaceReady {
		namespacePhase := apiv2.PhysicalContainerImagePhasePending
		namespaceMessage := namespaceReadinessMessage(image.Namespace, namespaceReason)
		change := noChange
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to get namespace", "Namespace", image.Namespace)
			namespacePhase = apiv2.PhysicalContainerImagePhaseUnknown
			namespaceMessage = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
			change |= additionalReconciliationNeeded
		}
		change |= setValue(&image.Status.Phase, namespacePhase)
		change |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionFalse, namespaceReason, namespaceMessage)
		return change, nil
	}

	change := noChange
	stateKey, data := r.imageData.BorrowByNamespacedName(image.NamespacedName())
	if data != nil {
		change |= data.applyTo(image)
		initializer := getStateInitializer(physicalContainerImageDataInitializers, data.conditionReason, log)
		change |= initializer(ctx, r, image, data.conditionReason, data, log)
		return change, r.physicalContainerImageDataSaveCallback(image, stateKey, data)
	}

	if image.Status.Phase == apiv2.PhysicalContainerImagePhaseFailed {
		return change, nil
	}

	if image.Spec.ImageID != "" {
		return r.ensureExistingImage(ctx, image, log), nil
	}
	if image.Spec.Image.Build != nil {
		return r.ensureBuiltImage(ctx, image, log), nil
	}
	return r.ensurePulledImage(ctx, image, log), nil
}

// Acknowledges a completed operation once its result is durably represented in status.
func (r *PhysicalContainerImageReconciler) physicalContainerImageDataSaveCallback(
	image *apiv2.PhysicalContainerImage,
	stateKey physicalContainerImageDataStateKey,
	data *physicalContainerImageData,
) func() {
	if data == nil ||
		data.operationInProgress() ||
		(data.imageID != "" && image.Status.Phase != apiv2.PhysicalContainerImagePhaseReady) {
		return nil
	}

	expectedReason := data.conditionReason
	expectedImageID := data.imageID
	expectedFailureMessage := data.failureMessage
	return func() {
		r.imageData.DeleteByStateKeyIf(stateKey, func(current *physicalContainerImageData) bool {
			return current.conditionReason == expectedReason &&
				current.imageID == expectedImageID &&
				current.failureMessage == expectedFailureMessage
		})
	}
}

func (r *PhysicalContainerImageReconciler) ensurePulledImage(ctx context.Context, image *apiv2.PhysicalContainerImage, log logr.Logger) objectChange {
	imageConfig := image.Spec.Image
	if image.Status.Phase == apiv2.PhysicalContainerImagePhaseReady && image.Status.Image != "" {
		inspectedImage, inspectErr := inspectPhysicalContainerImage(ctx, r.orchestrator, image.Status.Image)
		if inspectErr == nil {
			r.imageData.DeleteByNamespacedName(image.NamespacedName())
			return applyReadyPhysicalContainerImageStatus(image, image.Status.Image, inspectedImage)
		}
		if !errors.Is(inspectErr, containers.ErrNotFound) {
			log.Error(inspectErr, "Failed to inspect ready PhysicalContainerImage source image", "Image", image.Status.Image)
			change := setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhaseUnknown)
			change |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonRuntimeImageInspectFailed, fmt.Sprintf("Failed to inspect image: %v", inspectErr))
			return change | additionalReconciliationNeeded
		}
	}

	if imageConfig.PullPolicy == apiv2.PullPolicyAlways {
		return r.schedulePhysicalContainerImagePull(image, imageConfig.Image, log)
	}

	inspectedImage, inspectErr := inspectPhysicalContainerImage(ctx, r.orchestrator, imageConfig.Image)
	if inspectErr == nil {
		return applyReadyPhysicalContainerImageStatus(image, imageConfig.Image, inspectedImage)
	}
	if !errors.Is(inspectErr, containers.ErrNotFound) {
		log.Error(inspectErr, "Failed to inspect PhysicalContainerImage source image", "Image", imageConfig.Image)
		change := setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhaseUnknown)
		change |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonRuntimeImageInspectFailed, fmt.Sprintf("Failed to inspect image: %v", inspectErr))
		return change | additionalReconciliationNeeded
	}
	if imageConfig.PullPolicy == apiv2.PullPolicyNever {
		change := setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhaseFailed)
		change |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonLocalImageNotFound, fmt.Sprintf("Image %q is not available locally.", imageConfig.Image))
		return change
	}

	return r.schedulePhysicalContainerImagePull(image, imageConfig.Image, log)
}

func (r *PhysicalContainerImageReconciler) ensureBuiltImage(ctx context.Context, image *apiv2.PhysicalContainerImage, log logr.Logger) objectChange {
	imageConfig := image.Spec.Image
	outputImage := physicalContainerImageOutputTag(image)
	if image.Status.Phase == apiv2.PhysicalContainerImagePhaseReady && image.Status.Image != "" {
		inspectedImage, inspectErr := inspectPhysicalContainerImage(ctx, r.orchestrator, image.Status.Image)
		if inspectErr == nil {
			r.imageData.DeleteByNamespacedName(image.NamespacedName())
			return applyReadyPhysicalContainerImageStatus(image, image.Status.Image, inspectedImage)
		}
		if !errors.Is(inspectErr, containers.ErrNotFound) {
			log.Error(inspectErr, "Failed to inspect ready PhysicalContainerImage build output", "Image", image.Status.Image)
			change := setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhaseUnknown)
			change |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonRuntimeImageInspectFailed, fmt.Sprintf("Failed to inspect image: %v", inspectErr))
			return change | additionalReconciliationNeeded
		}
	}

	buildContext := *imageConfig.Build
	buildContext.Tags = append([]string{}, buildContext.Tags...)
	buildContext.Args = append([]commonapi.EnvVar{}, buildContext.Args...)
	buildContext.Secrets = append([]apiv2.ContainerBuildSecret{}, buildContext.Secrets...)
	buildContext.Labels = append([]commonapi.Label{}, buildContext.Labels...)
	if image.UID != "" {
		buildContext.Labels = setRuntimeLabel(buildContext.Labels, uidLabel, string(image.UID))
	}
	buildContext.Tags = physicalContainerImageBuildTags(buildContext.Tags, outputImage)

	return r.schedulePhysicalContainerImageBuild(image, outputImage, &buildContext, log)
}

func (r *PhysicalContainerImageReconciler) ensureExistingImage(ctx context.Context, image *apiv2.PhysicalContainerImage, log logr.Logger) objectChange {
	inspectedImage, inspectErr := inspectPhysicalContainerImage(ctx, r.orchestrator, image.Spec.ImageID)
	if inspectErr == nil {
		return applyReadyPhysicalContainerImageStatus(image, image.Spec.ImageID, inspectedImage)
	}
	if errors.Is(inspectErr, containers.ErrNotFound) {
		change := setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhaseFailed)
		change |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonLocalImageNotFound, fmt.Sprintf("Image %q is not available locally.", image.Spec.ImageID))
		return change
	}

	log.Error(inspectErr, "Failed to inspect existing PhysicalContainerImage", "ImageID", image.Spec.ImageID)
	change := setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhaseUnknown)
	change |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonRuntimeImageInspectFailed, fmt.Sprintf("Failed to inspect image: %v", inspectErr))
	return change | additionalReconciliationNeeded
}

func (r *PhysicalContainerImageReconciler) schedulePhysicalContainerImagePull(
	image *apiv2.PhysicalContainerImage,
	outputImage string,
	log logr.Logger,
) objectChange {
	stateKey := physicalContainerImageDataKey(image)
	operationCtx, cancelOperation := context.WithCancel(r.LifetimeCtx)
	data := &physicalContainerImageData{
		conditionReason: apiv2.PhysicalContainerImageReasonPulling,
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
		r.imageData.DeleteByNamespacedName(image.NamespacedName())
		log.Error(enqueueErr, "Failed to queue PhysicalContainerImage pull", "Image", outputImage)
		change := setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhaseFailed)
		change |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonPullFailed, fmt.Sprintf("Failed to queue image pull: %v", enqueueErr))
		return change
	}

	log.V(1).Info("Queued PhysicalContainerImage pull", "Image", outputImage)
	return data.applyTo(image)
}

func (r *PhysicalContainerImageReconciler) schedulePhysicalContainerImageBuild(
	image *apiv2.PhysicalContainerImage,
	outputImage string,
	buildContext *apiv2.ContainerBuildContext,
	log logr.Logger,
) objectChange {
	stateKey := physicalContainerImageDataKey(image)
	operationCtx, cancelOperation := context.WithCancel(r.LifetimeCtx)
	data := &physicalContainerImageData{
		conditionReason: apiv2.PhysicalContainerImageReasonBuilding,
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
		r.imageData.DeleteByNamespacedName(image.NamespacedName())
		log.Error(enqueueErr, "Failed to queue PhysicalContainerImage build", "Image", outputImage)
		change := setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhaseFailed)
		change |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonBuildFailed, fmt.Sprintf("Failed to queue image build: %v", enqueueErr))
		return change
	}

	log.V(1).Info("Queued PhysicalContainerImage build", "Context", buildContext.Context, "Dockerfile", buildContext.Dockerfile, "Image", outputImage)
	return data.applyTo(image)
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
		data.conditionReason = apiv2.PhysicalContainerImageReasonPullFailed
		data.failureMessage = fmt.Sprintf("Failed to pull image: %v", pullErr)
	} else {
		data.conditionReason = apiv2.PhysicalContainerImageReasonPulled
		data.imageID = pulledImageID
		data.failureMessage = ""
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
		data.conditionReason = apiv2.PhysicalContainerImageReasonBuildFailed
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
		data.conditionReason = apiv2.PhysicalContainerImageReasonBuildFailed
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
		data.conditionReason = apiv2.PhysicalContainerImageReasonBuildFailed
		data.failureMessage = fmt.Sprintf("Failed to build image: %v", buildErr)
		return
	}

	imageID, readErr := readPhysicalContainerImageIDFile(iidFileName)
	if readErr != nil {
		log.Error(readErr, "Failed to read PhysicalContainerImage build image ID", "Image", outputImage)
		if errors.Is(readErr, errPhysicalContainerImageIDMissing) {
			data.conditionReason = apiv2.PhysicalContainerImageReasonBuildResultMissingImageID
		} else {
			data.conditionReason = apiv2.PhysicalContainerImageReasonBuildFailed
		}
		data.failureMessage = fmt.Sprintf("Failed to read image ID: %v", readErr)
		return
	}

	data.conditionReason = apiv2.PhysicalContainerImageReasonBuilt
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

func handlePhysicalContainerImageOperationInProgress(
	_ context.Context,
	_ *PhysicalContainerImageReconciler,
	_ *apiv2.PhysicalContainerImage,
	conditionReason apiv2.ConditionReason,
	_ *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	log.V(1).Info("PhysicalContainerImage operation is still in progress", "Reason", conditionReason)
	return noChange
}

func handlePhysicalContainerImageOperationCompleted(
	ctx context.Context,
	reconciler *PhysicalContainerImageReconciler,
	image *apiv2.PhysicalContainerImage,
	conditionReason apiv2.ConditionReason,
	data *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	if data.imageID == "" {
		log.V(1).Info("PhysicalContainerImage operation completed without an image ID")
		if conditionReason != apiv2.PhysicalContainerImageReasonPulled {
			return handleUnknownPhysicalContainerImageDataReason(ctx, reconciler, image, conditionReason, data, log)
		}
		retryChange := setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhasePending)
		retryChange |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonPullResultMissingImageID, "Image pull completed without an image ID.")
		return retryChange | additionalReconciliationNeeded
	}

	inspectedImage, inspectErr := inspectPhysicalContainerImage(ctx, reconciler.orchestrator, data.imageID)
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect completed PhysicalContainerImage operation", "ImageID", data.imageID)
		unknownChange := setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhaseUnknown)
		unknownChange |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionFalse, apiv2.PhysicalContainerImageReasonRuntimeImageInspectFailed, fmt.Sprintf("Failed to inspect image: %v", inspectErr))
		return unknownChange | additionalReconciliationNeeded
	}
	log.V(1).Info("PhysicalContainerImage operation completed; saving image status", "ImageID", data.imageID)
	return applyReadyPhysicalContainerImageStatus(image, physicalContainerImageOutputTag(image), inspectedImage) | additionalReconciliationNeeded
}

func handlePhysicalContainerImageOperationFailed(
	_ context.Context,
	reconciler *PhysicalContainerImageReconciler,
	image *apiv2.PhysicalContainerImage,
	_ apiv2.ConditionReason,
	data *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	log.V(1).Info("PhysicalContainerImage operation failed; saving image status", "Message", data.failureMessage)
	// The failure is terminal: pulls already retry with backoff before reporting failure,
	// and spec is immutable, so no further reconciliation can make progress.
	return noChange
}

func handleUnknownPhysicalContainerImageDataReason(
	_ context.Context,
	_ *PhysicalContainerImageReconciler,
	image *apiv2.PhysicalContainerImage,
	conditionReason apiv2.ConditionReason,
	_ *physicalContainerImageData,
	log logr.Logger,
) objectChange {
	message := fmt.Sprintf("PhysicalContainerImage operation reached unknown condition reason %q.", conditionReason)
	log.Error(fmt.Errorf("unknown physical container image condition reason %q", conditionReason), "PhysicalContainerImage operation reached unknown condition reason")
	change := setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhaseUnknown)
	change |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionFalse, apiv2.PhysicalResourceReasonOperationStateInvalid, message)
	return change | additionalReconciliationNeeded
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

func applyReadyPhysicalContainerImageStatus(image *apiv2.PhysicalContainerImage, outputImage string, inspectedImage *containers.InspectedImage) objectChange {
	change := setValue(&image.Status.Phase, apiv2.PhysicalContainerImagePhaseReady)
	change |= setValue(&image.Status.Image, outputImage)
	change |= setValue(&image.Status.ImageID, inspectedImage.Id)
	change |= setValue(&image.Status.Digest, inspectedImage.Digest)
	change |= setPhysicalContainerImageTags(image, inspectedImage.Tags)
	change |= setCondition(&image.Status.Conditions, apiv2.ConditionReady, image.Generation, metav1.ConditionTrue, apiv2.PhysicalContainerImageReasonImageAvailable, "Image is available to the container runtime.")
	return change
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
