/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that InitializeLogicalBackingFinalizers preserves existing finalizers without aliasing
// their storage and installs each required finalizer only once.
func TestInitializeLogicalBackingFinalizers(t *testing.T) {
	t.Parallel()
	existing := make([]string, 1, 3)
	existing[0] = "other.example.com/owner"
	image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Finalizers: existing}}
	InitializeLogicalBackingFinalizers(image, physicalContainerImageFinalizer)
	InitializeLogicalBackingFinalizers(image, physicalContainerImageFinalizer)
	require.Equal(t, []string{existing[0], LogicalBackingFinalizer, physicalContainerImageFinalizer}, image.Finalizers)
	require.Equal(t, []string{existing[0], "", ""}, existing[:cap(existing)])
}

// Verifies that validatePhysicalResourceIdentity accepts only the pinned logical owner's live physical resource,
// retaining its logical hold and rejecting foreign owners, changed UIDs, or terminating resources.
func TestValidatePhysicalResourceIdentity(t *testing.T) {
	t.Parallel()
	owner := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "test", UID: "owner-uid"}}
	for _, testCase := range []struct {
		name    string
		change  func(*apiv2.PhysicalContainerImage)
		invalid bool
	}{
		{name: "matching"},
		{name: "foreign", invalid: true, change: func(image *apiv2.PhysicalContainerImage) { image.OwnerReferences[0].UID = "another-owner" }},
		{name: "changed-uid", invalid: true, change: func(image *apiv2.PhysicalContainerImage) { image.UID = "another-image" }},
		{name: "released-hold", invalid: true, change: func(image *apiv2.PhysicalContainerImage) { image.Finalizers = nil }},
		{name: "terminating", invalid: true, change: func(image *apiv2.PhysicalContainerImage) { now := metav1.Now(); image.DeletionTimestamp = &now }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: owner.Namespace, UID: "image-uid",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, apiv2.GroupVersion.WithKind("LogicalContainer"))},
				Finalizers:      []string{LogicalBackingFinalizer}}}
			if testCase.change != nil {
				testCase.change(image)
			}
			identityErr := validatePhysicalResourceIdentity(owner, image, "image-uid")
			if testCase.invalid {
				require.Error(t, identityErr)
			} else {
				require.NoError(t, identityErr)
			}
		})
	}
}

// Verifies that ReleaseLogicalBackingFinalizer removes only the logical hold, preserves physical
// and independent cleanup finalizers, and safely repeats after a successful release.
func TestReleaseLogicalBackingFinalizer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{
		Name: "image", Namespace: "test",
		Finalizers: []string{LogicalBackingFinalizer, physicalContainerImageFinalizer, "other.example.com/owner"},
	}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(image).Build()
	require.NoError(t, client.Get(ctx, image.NamespacedName(), image))
	require.NoError(t, ReleaseLogicalBackingFinalizer(ctx, client, image))
	require.NoError(t, ReleaseLogicalBackingFinalizer(ctx, client, image))
	saved := &apiv2.PhysicalContainerImage{}
	require.NoError(t, client.Get(ctx, image.NamespacedName(), saved))
	require.Equal(t, []string{physicalContainerImageFinalizer, "other.example.com/owner"}, saved.Finalizers)
}
