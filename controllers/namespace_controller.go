/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	namespaceCleanupCompleteCondition = "CleanupComplete"
)

var (
	namespaceFinalizer string = fmt.Sprintf("%s/namespace-reconciler", apiv2.GroupVersion.Group)
)

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
	if namespace.Status.Phase == apiv2.NamespacePhaseActive {
		return noChange
	}

	namespace.Status.Phase = apiv2.NamespacePhaseActive
	return statusChanged
}

func (r *NamespaceReconciler) handleDeletionRequest(ctx context.Context, namespace *apiv2.Namespace, log logr.Logger) objectChange {
	if namespace.Status.Phase != apiv2.NamespacePhaseTerminating {
		namespace.Status.Phase = apiv2.NamespacePhaseTerminating
		apimeta.SetStatusCondition(&namespace.Status.Conditions, metav1.Condition{
			Type:               namespaceCleanupCompleteCondition,
			Status:             metav1.ConditionFalse,
			Reason:             "CleanupInProgress",
			Message:            "Namespace cleanup is in progress.",
			ObservedGeneration: namespace.Generation,
		})
		return statusChanged | additionalReconciliationNeeded
	}

	cleanupComplete, cleanupErr := r.cleanupNamespace(ctx, namespace, log)
	if cleanupErr != nil {
		log.Error(cleanupErr, "Namespace cleanup failed")
		return additionalReconciliationNeeded
	}
	if !cleanupComplete {
		log.V(1).Info("Namespace cleanup is still in progress")
		return additionalReconciliationNeeded
	}

	cleanupCompleteCondition := apimeta.FindStatusCondition(namespace.Status.Conditions, namespaceCleanupCompleteCondition)
	if cleanupCompleteCondition == nil || cleanupCompleteCondition.Status != metav1.ConditionTrue {
		apimeta.SetStatusCondition(&namespace.Status.Conditions, metav1.Condition{
			Type:               namespaceCleanupCompleteCondition,
			Status:             metav1.ConditionTrue,
			Reason:             "CleanupComplete",
			Message:            "Namespace cleanup is complete.",
			ObservedGeneration: namespace.Generation,
		})
		return statusChanged | additionalReconciliationNeeded
	}

	return deleteFinalizer(namespace, namespaceFinalizer, log)
}

func (r *NamespaceReconciler) cleanupNamespace(ctx context.Context, namespace *apiv2.Namespace, log logr.Logger) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	cleaned := map[schema.GroupVersionResource]bool{}
	for len(cleaned) < len(resourcecleanup.NamespaceResources) {
		progress := false
		for _, cleanupResource := range resourcecleanup.NamespaceResources {
			if cleaned[cleanupResource.GVR] || !namespaceCleanupDependenciesComplete(cleanupResource, cleaned) {
				continue
			}

			complete, cleanupErr := r.cleanupNamespaceResource(ctx, namespace, cleanupResource.GVR, log)
			if cleanupErr != nil {
				return false, cleanupErr
			}
			if !complete {
				return false, nil
			}

			cleaned[cleanupResource.GVR] = true
			progress = true
		}

		if !progress {
			return false, fmt.Errorf("namespace cleanup resource dependencies are not satisfiable")
		}
	}

	return true, nil
}

func namespaceCleanupDependenciesComplete(cleanupResource *resourcecleanup.CleanupResource, cleaned map[schema.GroupVersionResource]bool) bool {
	return !slices.Any(cleanupResource.CleanUpAfter, func(gvr schema.GroupVersionResource) bool {
		return !cleaned[gvr]
	})
}

func (r *NamespaceReconciler) cleanupNamespaceResource(
	ctx context.Context,
	namespace *apiv2.Namespace,
	gvr schema.GroupVersionResource,
	log logr.Logger,
) (bool, error) {
	switch gvr {
	case (&apiv2.PhysicalContainer{}).GetGroupVersionResource():
		return r.cleanupPhysicalContainers(ctx, namespace, log)
	case (&apiv2.PhysicalContainerImage{}).GetGroupVersionResource():
		return r.cleanupPhysicalContainerImages(ctx, namespace, log)
	default:
		return false, fmt.Errorf("unsupported namespace cleanup resource %q", gvr.String())
	}
}

func (r *NamespaceReconciler) cleanupPhysicalContainers(ctx context.Context, namespace *apiv2.Namespace, log logr.Logger) (bool, error) {
	physicalContainers := apiv2.PhysicalContainerList{}
	listErr := r.Client.List(ctx, &physicalContainers, ctrl_client.InNamespace(namespace.Name))
	if listErr != nil {
		return false, fmt.Errorf("failed to list PhysicalContainers in namespace %q: %w", namespace.Name, listErr)
	}

	for i := range physicalContainers.Items {
		physicalContainer := &physicalContainers.Items[i]
		if physicalContainer.DeletionTimestamp != nil && !physicalContainer.DeletionTimestamp.IsZero() {
			continue
		}

		log.V(1).Info("Deleting PhysicalContainer during namespace cleanup", "Namespace", namespace.Name, "PhysicalContainer", physicalContainer.Name)
		deleteErr := r.Client.Delete(ctx, physicalContainer)
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return false, fmt.Errorf("failed to delete PhysicalContainer %q in namespace %q: %w", physicalContainer.Name, namespace.Name, deleteErr)
		}
	}

	if len(physicalContainers.Items) != 0 {
		return false, nil
	}

	return true, nil
}

func (r *NamespaceReconciler) cleanupPhysicalContainerImages(ctx context.Context, namespace *apiv2.Namespace, log logr.Logger) (bool, error) {
	physicalContainerImages := apiv2.PhysicalContainerImageList{}
	listImagesErr := r.Client.List(ctx, &physicalContainerImages, ctrl_client.InNamespace(namespace.Name))
	if listImagesErr != nil {
		return false, fmt.Errorf("failed to list PhysicalContainerImages in namespace %q: %w", namespace.Name, listImagesErr)
	}

	for i := range physicalContainerImages.Items {
		physicalContainerImage := &physicalContainerImages.Items[i]
		if physicalContainerImage.DeletionTimestamp != nil && !physicalContainerImage.DeletionTimestamp.IsZero() {
			continue
		}

		log.V(1).Info("Deleting PhysicalContainerImage during namespace cleanup", "Namespace", namespace.Name, "PhysicalContainerImage", physicalContainerImage.Name)
		deleteErr := r.Client.Delete(ctx, physicalContainerImage)
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return false, fmt.Errorf("failed to delete PhysicalContainerImage %q in namespace %q: %w", physicalContainerImage.Name, namespace.Name, deleteErr)
		}
	}

	return len(physicalContainerImages.Items) == 0, nil
}
