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
