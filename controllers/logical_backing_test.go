/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/testutil"
)

type logicalBackingCreateErrorClient struct {
	ctrl_client.Client
	creationErr error
	calls       int
}

func (client *logicalBackingCreateErrorClient) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	client.calls++
	if client.creationErr != nil {
		return client.creationErr
	}
	return client.Client.Create(ctx, object, options...)
}

// Verifies that shared backing declaration performs one Create, never reads after an ordinary create error,
// and validates an existing resource only after AlreadyExists, preserving the original logical-container flow.
func TestCreateLogicalBackingPreservesCreationFlow(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"success", "ordinary-error", "already-exists"} {
		t.Run(scenario, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, apiv2.AddToScheme(scheme))
			owner := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "test", UID: "owner-uid"}}
			desired := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: owner.Namespace}}
			existing := desired.DeepCopy()
			existing.UID = "committed-image"
			existing.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, apiv2.GroupVersion.WithKind("LogicalContainer"))}
			InitializeLogicalBackingFinalizers(existing, physicalContainerImageFinalizer)
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if scenario != "success" {
				builder = builder.WithObjects(existing)
			}
			client := &logicalBackingCreateErrorClient{Client: builder.Build()}
			injectedErr := errors.New("uncertain create outcome")
			if scenario == "ordinary-error" {
				client.creationErr = injectedErr
			}
			reader := &logicalChildConfirmationReader{Reader: client}
			var pending unobservedLogicalChildren
			var uid types.UID
			validated := false
			declared, declarationErr := createLogicalBacking(t.Context(), client, reader, owner, desired, physicalContainerImageFinalizer,
				&uid, &pending, func(*apiv2.PhysicalContainerImage) error { validated = true; return nil })
			require.Equal(t, 1, client.calls)
			require.Len(t, pending, 1)
			if scenario == "already-exists" {
				require.NoError(t, declarationErr)
				require.Len(t, reader.calls, 1)
				require.True(t, validated)
				require.Equal(t, existing.UID, uid)
				require.NotSame(t, desired, declared)
			} else {
				require.Empty(t, reader.calls)
				require.False(t, validated)
				require.Same(t, desired, declared)
				if scenario == "ordinary-error" {
					require.ErrorIs(t, declarationErr, injectedErr)
					require.False(t, apierrors.IsAlreadyExists(declarationErr))
				} else {
					require.NoError(t, declarationErr)
				}
			}
		})
	}
}

type logicalChildConfirmationReader struct {
	ctrl_client.Reader
	calls []types.NamespacedName
}

// Verifies that logicalBackingStatusMessage retains backing identity and detailed reasons in
// messages without requiring those details in the logical resource's public reason vocabulary.
func TestLogicalBackingStatusMessage(t *testing.T) {
	t.Parallel()
	require.Equal(t, "PhysicalProcess has not reported its state.", logicalBackingStatusMessage("PhysicalProcess", nil))
	require.Equal(t, "PhysicalProcess reports LaunchFailed: executable unavailable",
		logicalBackingStatusMessage("PhysicalProcess", []metav1.Condition{{
			Type: string(apiv2.ConditionReady), Reason: "LaunchFailed", Message: "executable unavailable",
		}}))
}

func (reader *logicalChildConfirmationReader) Get(
	ctx context.Context, key ctrl_client.ObjectKey, object ctrl_client.Object, options ...ctrl_client.GetOption,
) error {
	reader.calls = append(reader.calls, key)
	return reader.Reader.Get(ctx, key, object, options...)
}

// Verifies that child confirmation separates resource kinds with identical generated names
// and retains unexpected-name owned resources discovered by cached queries.
func TestLogicalChildConfirmationSeparatesKinds(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	owner := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "test", UID: "owner-uid"}}
	image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: "logical-owner-uid", Namespace: owner.Namespace}}
	physical := &apiv2.PhysicalContainer{ObjectMeta: image.ObjectMeta}
	var pending unobservedLogicalChildren
	pending.recordCreateAttempt(image)
	pending.recordCreateAttempt(physical)
	unexpected := apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{
		Name: "unexpected", Namespace: owner.Namespace,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, apiv2.GroupVersion.WithKind("LogicalContainer"))},
	}}
	cached := []apiv2.PhysicalContainerImage{unexpected}
	reader := &logicalChildConfirmationReader{Reader: fake.NewClientBuilder().WithScheme(scheme).Build()}
	require.NoError(t, resolveUnobservedLogicalChildren(ctx, reader, owner, pending, &cached))
	require.Equal(t, []apiv2.PhysicalContainerImage{unexpected}, cached)
	require.Len(t, pending, 1)
	require.Equal(t, []types.NamespacedName{image.NamespacedName()}, reader.calls)
}

// Verifies that logical state snapshots do not share mutable child-creation tracking.
func TestLogicalChildTrackingClonesIndependently(t *testing.T) {
	t.Parallel()
	for name, clone := range map[string]func(unobservedLogicalChildren) unobservedLogicalChildren{
		"container": func(pending unobservedLogicalChildren) unobservedLogicalChildren {
			data := &logicalContainerData{unobservedChildren: pending}
			return data.Clone().unobservedChildren
		},
		"process": func(pending unobservedLogicalChildren) unobservedLogicalChildren {
			data := &logicalProcessData{unobservedChildren: pending}
			return data.Clone().unobservedChildren
		},
		"network": func(pending unobservedLogicalChildren) unobservedLogicalChildren {
			data := &logicalContainerNetworkData{unobservedChildren: pending}
			return data.Clone().unobservedChildren
		},
		"exec": func(pending unobservedLogicalChildren) unobservedLogicalChildren {
			data := &logicalContainerExecData{unobservedChildren: pending}
			return data.Clone().unobservedChildren
		},
	} {
		t.Run(name, func(t *testing.T) {
			child := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "child"}}
			var pending unobservedLogicalChildren
			pending.recordCreateAttempt(child)
			snapshot := clone(pending)
			snapshot.observeCached(child)
			require.Empty(t, snapshot)
			require.Len(t, pending, 1)
		})
	}
}

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
