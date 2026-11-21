/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/commonapi"
)

type logicalChildIdentity struct {
	resource schema.GroupVersionResource
	name     string
}

var errLogicalNetworkFailed = errors.New("logical network failed")

// resolveLogicalContainerNetworks translates logical network declarations to their published physical references.
func resolveLogicalContainerNetworks(
	ctx context.Context, reader ctrl_client.Reader, namespace string, networks []apiv2.LogicalContainerNetworkConnectionConfig,
) (bool, []apiv2.ContainerNetworkConnectionConfig, string, error) {
	var resolved []apiv2.ContainerNetworkConnectionConfig
	var pendingMessage string
	var resolutionErr error
	for _, requested := range networks {
		network := &apiv2.LogicalContainerNetwork{}
		networkName := commonapi.AsNamespacedName(requested.NetworkRef, namespace)
		getErr := reader.Get(ctx, networkName, network)
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
			NetworkRef: network.Status.PhysicalNetworkRef, Aliases: slices.Clone(requested.Aliases),
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

func logicalBackingStatusMessage(kind string, conditions []metav1.Condition) string {
	ready := apimeta.FindStatusCondition(conditions, string(apiv2.ConditionReady))
	if ready == nil {
		return fmt.Sprintf("%s has not reported its state.", kind)
	}
	return fmt.Sprintf("%s reports %s: %s", kind, ready.Reason, ready.Message)
}

// unobservedLogicalChildren records possible creations not yet observed through the cache.
type unobservedLogicalChildren map[logicalChildIdentity]struct{}

func (children *unobservedLogicalChildren) recordCreateAttempt(child commonapi.DcpModelObject) {
	if *children == nil {
		*children = make(unobservedLogicalChildren)
	}
	identity := logicalChildIdentity{resource: child.GetGroupVersionResource(), name: child.GetName()}
	(*children)[identity] = struct{}{}
}

func (children unobservedLogicalChildren) observeCached(child commonapi.DcpModelObject) {
	identity := logicalChildIdentity{resource: child.GetGroupVersionResource(), name: child.GetName()}
	delete(children, identity)
}

// resolveUnobservedLogicalChildren supplements cached owner queries only where creation may
// have committed without reaching the cache. Confirmed absence remains resolved during deletion.
func resolveUnobservedLogicalChildren[T commonapi.ObjectStruct, PT commonapi.PObjectStruct[T]](
	ctx context.Context, reader ctrl_client.Reader, owner ctrl_client.Object, pending unobservedLogicalChildren, cached *[]T,
) error {
	for i := range *cached {
		child := PT(&(*cached)[i])
		if metav1.IsControlledBy(child, owner) {
			pending.observeCached(child)
		}
	}
	prototype := PT(new(T))
	for identity := range pending {
		if identity.resource != prototype.GetGroupVersionResource() {
			continue
		}
		child := PT(new(T))
		name := types.NamespacedName{Namespace: owner.GetNamespace(), Name: identity.name}
		getErr := reader.Get(ctx, name, child)
		if apierrors.IsNotFound(getErr) {
			delete(pending, identity)
			continue
		}
		if getErr != nil {
			return fmt.Errorf("confirm logical child %s %q: %w", identity.resource.Resource, name, getErr)
		}
		if !metav1.IsControlledBy(child, owner) {
			delete(pending, identity)
			continue
		}
		*cached = append(*cached, *child)
	}
	return nil
}

// LogicalBackingFinalizer retains an owned physical API object until its logical owner observes cleanup.
const LogicalBackingFinalizer = "usvc-dev.developer.microsoft.com/logical-backing"

// createLogicalBacking declares an owned physical resource and validates AlreadyExists collisions.
// validateExisting checks immutable inputs where an earlier committed declaration is not authoritative.
func createLogicalBacking[T commonapi.ObjectStruct, PT commonapi.PObjectStruct[T]](
	ctx context.Context, client ctrl_client.Client, reader ctrl_client.Reader, owner ctrl_client.Object,
	child PT, physicalFinalizer string, expectedUID *types.UID, pending *unobservedLogicalChildren,
	validateExisting func(PT) error,
) (PT, error) {
	InitializeLogicalBackingFinalizers(child, physicalFinalizer)
	if ownerErr := ctrl.SetControllerReference(owner, child, client.Scheme()); ownerErr != nil {
		return nil, fmt.Errorf("set physical resource owner: %w", ownerErr)
	}
	pending.recordCreateAttempt(child)
	createErr := client.Create(ctx, child)
	if child.GetUID() != "" {
		*expectedUID = child.GetUID()
	}
	if createErr == nil || !apierrors.IsAlreadyExists(createErr) {
		return child, createErr
	}
	existing := PT(new(T))
	if lookupErr := reader.Get(ctx, ctrl_client.ObjectKeyFromObject(child), existing); lookupErr != nil {
		return child, fmt.Errorf("get existing physical resource: %w", lookupErr)
	}
	if identityErr := validatePhysicalResourceIdentity(owner, existing, *expectedUID); identityErr != nil {
		return nil, fmt.Errorf("%v: %w", identityErr, createErr)
	}
	if validateExisting != nil {
		if validationErr := validateExisting(existing); validationErr != nil {
			return nil, fmt.Errorf("%v: %w", validationErr, createErr)
		}
	}
	*expectedUID = existing.GetUID()
	return existing, nil
}

func validatePhysicalResourceIdentity(owner, physical ctrl_client.Object, expectedUID types.UID) error {
	if !metav1.IsControlledBy(physical, owner) || (expectedUID != "" && physical.GetUID() != expectedUID) ||
		!hasFinalizer(physical, LogicalBackingFinalizer) || !physical.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("physical resource %q has a different owner, UID, or logical hold, or is terminating", ctrl_client.ObjectKeyFromObject(physical))
	}
	return nil
}

// InitializeLogicalBackingFinalizers installs the logical hold and physical cleanup finalizer at creation.
func InitializeLogicalBackingFinalizers(obj ctrl_client.Object, physicalFinalizer string) {
	finalizers := slices.Clone(obj.GetFinalizers())
	for _, finalizer := range []string{LogicalBackingFinalizer, physicalFinalizer} {
		if !slices.Contains(finalizers, finalizer) {
			finalizers = append(finalizers, finalizer)
		}
	}
	obj.SetFinalizers(finalizers)
}

// ReleaseLogicalBackingFinalizer removes only the logical owner's hold using optimistic concurrency.
// The caller must verify the physical resource's identity and controller owner first.
func ReleaseLogicalBackingFinalizer(ctx context.Context, client ctrl_client.Client, obj ctrl_client.Object) error {
	if !slices.Contains(obj.GetFinalizers(), LogicalBackingFinalizer) {
		return nil
	}
	patch := ctrl_client.MergeFromWithOptions(obj.DeepCopyObject().(ctrl_client.Object), ctrl_client.MergeFromWithOptimisticLock{})
	obj.SetFinalizers(slices.DeleteFunc(slices.Clone(obj.GetFinalizers()), func(finalizer string) bool {
		return finalizer == LogicalBackingFinalizer
	}))
	return client.Patch(ctx, obj, patch)
}
