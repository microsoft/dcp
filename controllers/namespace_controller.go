/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/resourcecleanup"
	"github.com/microsoft/dcp/pkg/slices"
)

const (
	namespaceCleanupMaxConcurrentDeletes = 6

	namespaceCleanupCompleteCondition apiv2.ConditionType = "CleanupComplete"

	namespaceCleanupInProgressReason apiv2.ConditionReason = "CleanupInProgress"
	namespaceCleanupCompleteReason   apiv2.ConditionReason = "CleanupComplete"
	namespaceCleanupFailedReason     apiv2.ConditionReason = "CleanupFailed"
)

var (
	namespaceFinalizer string = apiv2.NamespaceFinalizer
)

type namespaceCleanupResourceHandler func(*NamespaceReconciler, context.Context, *apiv2.Namespace, logr.Logger) (int, error)

var namespaceCleanupResourceHandlers = map[schema.GroupVersionResource]namespaceCleanupResourceHandler{
	(&apiv2.PhysicalContainer{}).GetGroupVersionResource():        (*NamespaceReconciler).cleanupPhysicalContainers,
	(&apiv2.PhysicalContainerImage{}).GetGroupVersionResource():   (*NamespaceReconciler).cleanupPhysicalContainerImages,
	(&apiv2.PhysicalContainerNetwork{}).GetGroupVersionResource(): (*NamespaceReconciler).cleanupPhysicalContainerNetworks,
}

type NamespaceReconciler struct {
	*ReconcilerBase[apiv2.Namespace, *apiv2.Namespace]
}

func NewNamespaceReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
) *NamespaceReconciler {
	return &NamespaceReconciler{
		ReconcilerBase: NewReconcilerBase[apiv2.Namespace](client, noCacheClient, log, lifetimeCtx),
	}
}

func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.Namespace{}).
		Named(name).
		Complete(r)
}

func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	namespace := apiv2.Namespace{}
	getErr := reader.Get(ctx, req.NamespacedName, &namespace)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			log.V(1).Info("Namespace not found, nothing to do...")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}

		log.Error(getErr, "Failed to Get() the Namespace")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(namespace.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if namespace.DeletionTimestamp != nil && !namespace.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, &namespace, log)
	} else if change = ensureFinalizer(&namespace, namespaceFinalizer, log); change != noChange {
		// Make additional changes during the next reconciliation.
	} else {
		change = r.manageNamespace(&namespace)
	}

	return r.SaveChanges(ctx, &namespace, patch, change, nil, log)
}

func (r *NamespaceReconciler) manageNamespace(namespace *apiv2.Namespace) objectChange {
	return setValue(&namespace.Status.Phase, apiv2.NamespacePhaseActive)
}

func (r *NamespaceReconciler) handleDeletionRequest(ctx context.Context, namespace *apiv2.Namespace, log logr.Logger) objectChange {
	if namespace.Status.Phase != apiv2.NamespacePhaseTerminating {
		change := setValue(&namespace.Status.Phase, apiv2.NamespacePhaseTerminating)
		change |= r.setNamespaceCleanupInProgress(namespace, "")
		return change | additionalReconciliationNeeded
	}

	cleanupPending, cleanupErr := r.cleanupNamespace(ctx, namespace, log)
	if cleanupErr != nil {
		log.Error(cleanupErr, "Namespace cleanup failed")
		// Cleanup is retried indefinitely, because giving up would leak the runtime resources the
		// namespace owns. Record the failure so a stuck shutdown is diagnosable from the resource.
		change := setCondition(
			&namespace.Status.Conditions,
			namespaceCleanupCompleteCondition,
			namespace.Generation,
			metav1.ConditionFalse,
			namespaceCleanupFailedReason,
			fmt.Sprintf("Namespace cleanup failed: %v", cleanupErr),
		)
		return change | additionalReconciliationNeeded
	}
	if cleanupPending != "" {
		log.V(1).Info("Namespace cleanup is still in progress", "Pending", cleanupPending)
		// Naming what cleanup is waiting for also clears any previously recorded failure.
		return r.setNamespaceCleanupInProgress(namespace, cleanupPending) | additionalReconciliationNeeded
	}

	if change := setCondition(
		&namespace.Status.Conditions,
		namespaceCleanupCompleteCondition,
		namespace.Generation,
		metav1.ConditionTrue,
		namespaceCleanupCompleteReason,
		"Namespace cleanup is complete.",
	); change != noChange {
		return change | additionalReconciliationNeeded
	}

	return deleteFinalizer(namespace, namespaceFinalizer, log)
}

// Records that cleanup is still running. When pending is non-empty it describes the resources
// cleanup is waiting for, so a namespace that cannot finish terminating is diagnosable.
func (r *NamespaceReconciler) setNamespaceCleanupInProgress(namespace *apiv2.Namespace, pending string) objectChange {
	message := "Namespace cleanup is in progress."
	if pending != "" {
		message = fmt.Sprintf("Namespace cleanup is waiting for %s to be deleted.", pending)
	}

	return setCondition(
		&namespace.Status.Conditions,
		namespaceCleanupCompleteCondition,
		namespace.Generation,
		metav1.ConditionFalse,
		namespaceCleanupInProgressReason,
		message,
	)
}

// Deletes the namespace-scoped resources owned by the namespace, in dependency order.
// Returns a description of the resources still awaiting deletion, or an empty string once
// cleanup is complete.
func (r *NamespaceReconciler) cleanupNamespace(ctx context.Context, namespace *apiv2.Namespace, log logr.Logger) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	cleaned := map[schema.GroupVersionResource]bool{}
	for len(cleaned) < len(resourcecleanup.NamespaceResources) {
		progress := false
		for _, cleanupResource := range resourcecleanup.NamespaceResources {
			if cleaned[cleanupResource.GVR] || !namespaceCleanupDependenciesComplete(cleanupResource, cleaned) {
				continue
			}

			remaining, cleanupErr := r.cleanupNamespacedResources(ctx, namespace, cleanupResource.GVR, log)
			if cleanupErr != nil {
				return "", cleanupErr
			}
			if remaining > 0 {
				return fmt.Sprintf("%d %s", remaining, cleanupResource.GVR.Resource), nil
			}

			cleaned[cleanupResource.GVR] = true
			progress = true
		}

		if !progress {
			return "", fmt.Errorf("namespace cleanup resource dependencies are not satisfiable")
		}
	}

	return "", nil
}

func namespaceCleanupDependenciesComplete(cleanupResource *resourcecleanup.CleanupResource, cleaned map[schema.GroupVersionResource]bool) bool {
	return !slices.Any(cleanupResource.CleanUpAfter, func(gvr schema.GroupVersionResource) bool {
		return !cleaned[gvr]
	})
}

func (r *NamespaceReconciler) cleanupNamespacedResources(
	ctx context.Context,
	namespace *apiv2.Namespace,
	gvr schema.GroupVersionResource,
	log logr.Logger,
) (int, error) {
	handler, ok := namespaceCleanupResourceHandlers[gvr]
	if !ok {
		return 0, fmt.Errorf("unsupported namespace cleanup resource %q", gvr.String())
	}

	return handler(r, ctx, namespace, log)
}

func (r *NamespaceReconciler) cleanupPhysicalContainers(ctx context.Context, namespace *apiv2.Namespace, log logr.Logger) (int, error) {
	physicalContainers := apiv2.PhysicalContainerList{}
	listErr := r.Client.List(ctx, &physicalContainers, ctrl_client.InNamespace(namespace.Name))
	if listErr != nil {
		return 0, fmt.Errorf("failed to list PhysicalContainers in namespace %q: %w", namespace.Name, listErr)
	}

	deleteErrors := slices.MapConcurrent[error](physicalContainers.Items, func(physicalContainer apiv2.PhysicalContainer) error {
		if physicalContainer.DeletionTimestamp != nil && !physicalContainer.DeletionTimestamp.IsZero() {
			return nil
		}

		log.V(1).Info("Deleting PhysicalContainer during namespace cleanup", "Namespace", namespace.Name, "PhysicalContainer", physicalContainer.Name)
		deletePhysicalContainerErr := r.Client.Delete(ctx, &physicalContainer)
		if deletePhysicalContainerErr != nil && !apierrors.IsNotFound(deletePhysicalContainerErr) {
			return fmt.Errorf("failed to delete PhysicalContainer %q in namespace %q: %w", physicalContainer.Name, namespace.Name, deletePhysicalContainerErr)
		}
		return nil
	}, namespaceCleanupMaxConcurrentDeletes)
	deleteErr := errors.Join(deleteErrors...)
	if deleteErr != nil {
		return 0, deleteErr
	}

	return len(physicalContainers.Items), nil
}

func (r *NamespaceReconciler) cleanupPhysicalContainerImages(ctx context.Context, namespace *apiv2.Namespace, log logr.Logger) (int, error) {
	physicalContainerImages := apiv2.PhysicalContainerImageList{}
	listImagesErr := r.Client.List(ctx, &physicalContainerImages, ctrl_client.InNamespace(namespace.Name))
	if listImagesErr != nil {
		return 0, fmt.Errorf("failed to list PhysicalContainerImages in namespace %q: %w", namespace.Name, listImagesErr)
	}

	deleteErrors := slices.MapConcurrent[error](physicalContainerImages.Items, func(physicalContainerImage apiv2.PhysicalContainerImage) error {
		if physicalContainerImage.DeletionTimestamp != nil && !physicalContainerImage.DeletionTimestamp.IsZero() {
			return nil
		}

		log.V(1).Info("Deleting PhysicalContainerImage during namespace cleanup", "Namespace", namespace.Name, "PhysicalContainerImage", physicalContainerImage.Name)
		deletePhysicalContainerImageErr := r.Client.Delete(ctx, &physicalContainerImage)
		if deletePhysicalContainerImageErr != nil && !apierrors.IsNotFound(deletePhysicalContainerImageErr) {
			return fmt.Errorf("failed to delete PhysicalContainerImage %q in namespace %q: %w", physicalContainerImage.Name, namespace.Name, deletePhysicalContainerImageErr)
		}
		return nil
	}, namespaceCleanupMaxConcurrentDeletes)
	deleteErr := errors.Join(deleteErrors...)
	if deleteErr != nil {
		return 0, deleteErr
	}

	return len(physicalContainerImages.Items), nil
}

func (r *NamespaceReconciler) cleanupPhysicalContainerNetworks(ctx context.Context, namespace *apiv2.Namespace, log logr.Logger) (int, error) {
	physicalContainerNetworks := apiv2.PhysicalContainerNetworkList{}
	listErr := r.Client.List(ctx, &physicalContainerNetworks, ctrl_client.InNamespace(namespace.Name))
	if listErr != nil {
		return 0, fmt.Errorf("failed to list PhysicalContainerNetworks in namespace %q: %w", namespace.Name, listErr)
	}

	for i := range physicalContainerNetworks.Items {
		physicalContainerNetwork := &physicalContainerNetworks.Items[i]
		if physicalContainerNetwork.DeletionTimestamp != nil && !physicalContainerNetwork.DeletionTimestamp.IsZero() {
			continue
		}

		log.V(1).Info("Deleting PhysicalContainerNetwork during namespace cleanup", "Namespace", namespace.Name, "PhysicalContainerNetwork", physicalContainerNetwork.Name)
		deleteErr := r.Client.Delete(ctx, physicalContainerNetwork)
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return 0, fmt.Errorf("failed to delete PhysicalContainerNetwork %q in namespace %q: %w", physicalContainerNetwork.Name, namespace.Name, deleteErr)
		}
	}

	return len(physicalContainerNetworks.Items), nil
}
