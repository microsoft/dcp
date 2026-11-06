/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

// LogicalBackingFinalizer retains an owned physical API object until its logical owner observes cleanup.
const LogicalBackingFinalizer = "usvc-dev.developer.microsoft.com/logical-backing"

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
