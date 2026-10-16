/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

type namespaceCleanupResult struct {
	gvr       schema.GroupVersionResource
	remaining int
	err       error
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

// Deletes the namespace-scoped resources owned by the namespace, processing each dependency-ready
// batch concurrently.
// Returns a description of the resources still awaiting deletion, or an empty string once
// cleanup is complete.
func (r *NamespaceReconciler) cleanupNamespace(ctx context.Context, namespace *apiv2.Namespace, log logr.Logger) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	cleaned := map[schema.GroupVersionResource]bool{}
	attempted := map[schema.GroupVersionResource]bool{}
	pending := map[schema.GroupVersionResource]int{}
	for {
		ready := slices.Select(resourcecleanup.NamespaceResources, func(cleanupResource *resourcecleanup.CleanupResource) bool {
			return !attempted[cleanupResource.GVR] &&
				namespaceCleanupDependenciesComplete(cleanupResource, cleaned)
		})
		if len(ready) == 0 {
			break
		}
		for _, cleanupResource := range ready {
			attempted[cleanupResource.GVR] = true
		}

		results := slices.MapConcurrent[namespaceCleanupResult](ready, func(cleanupResource *resourcecleanup.CleanupResource) namespaceCleanupResult {
			remaining, cleanupErr := r.cleanupNamespacedResources(ctx, namespace, cleanupResource.GVR, log)
			return namespaceCleanupResult{
				gvr:       cleanupResource.GVR,
				remaining: remaining,
				err:       cleanupErr,
			}
		}, 0)

		cleanupErrors := slices.Map[error](results, func(result namespaceCleanupResult) error {
			return result.err
		})
		if cleanupErr := errors.Join(cleanupErrors...); cleanupErr != nil {
			return "", cleanupErr
		}
		for _, result := range results {
			if result.remaining == 0 {
				cleaned[result.gvr] = true
			} else {
				pending[result.gvr] = result.remaining
			}
		}
	}

	if len(cleaned) == len(resourcecleanup.NamespaceResources) {
		return "", nil
	}
	if len(pending) == 0 {
		return "", fmt.Errorf("namespace cleanup resource dependencies are not satisfiable")
	}

	pendingDescriptions := make([]string, 0, len(pending))
	for _, cleanupResource := range resourcecleanup.NamespaceResources {
		if remaining := pending[cleanupResource.GVR]; remaining > 0 {
			pendingDescriptions = append(pendingDescriptions, fmt.Sprintf("%d %s", remaining, cleanupResource.GVR.Resource))
		}
	}
	return strings.Join(pendingDescriptions, " and "), nil
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
	kind, mappingErr := r.RESTMapper().KindFor(gvr)
	if mappingErr != nil {
		return 0, fmt.Errorf("resolve namespace cleanup resource %q: %w", gvr.String(), mappingErr)
	}
	resources := &unstructured.UnstructuredList{}
	resources.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))
	listErr := r.NoCacheClient.List(ctx, resources, ctrl_client.InNamespace(namespace.Name))
	if listErr != nil {
		return 0, fmt.Errorf("list %s in namespace %q: %w", gvr.Resource, namespace.Name, listErr)
	}
	deleteErrors := slices.MapConcurrent[error](resources.Items, func(item unstructured.Unstructured) error {
		if !item.GetDeletionTimestamp().IsZero() {
			return nil
		}
		item.SetGroupVersionKind(kind)
		log.V(1).Info("Deleting resource during namespace cleanup", "Resource", gvr.String(), "Namespace", namespace.Name, "Name", item.GetName())
		deleteErr := r.Client.Delete(ctx, &item)
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete %s %q in namespace %q: %w", gvr.Resource, item.GetName(), namespace.Name, deleteErr)
		}
		return nil
	}, namespaceCleanupMaxConcurrentDeletes)
	return len(resources.Items), errors.Join(deleteErrors...)
}
