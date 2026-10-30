/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

type unobservedBackingClient struct {
	ctrl_client.Client
	images       bool
	created      chan ctrl_client.Object
	returnCreate <-chan struct{}
	observe      <-chan struct{}
}

func (visibility *unobservedBackingClient) Get(
	ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption,
) error {
	_, image := obj.(*apiv2.PhysicalContainerImage)
	_, physical := obj.(*apiv2.PhysicalContainer)
	if (visibility.images && image) || (!visibility.images && physical) {
		select {
		case <-visibility.observe:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return visibility.Client.Get(ctx, key, obj, options...)
}

func (visibility *unobservedBackingClient) Create(
	ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption,
) error {
	owner := metav1.GetControllerOf(obj)
	owned := owner != nil && owner.APIVersion == apiv2.GroupVersion.String() && owner.Kind == "LogicalContainer"
	if _, physical := obj.(*apiv2.PhysicalContainer); physical && owned && visibility.images {
		return fmt.Errorf("injected physical declaration failure")
	}
	createErr := visibility.Client.Create(ctx, obj, options...)
	if createErr != nil || !owned {
		return createErr
	}
	var created ctrl_client.Object
	switch child := obj.(type) {
	case *apiv2.PhysicalContainer:
		if !visibility.images {
			created = child.DeepCopy()
		}
	case *apiv2.PhysicalContainerImage:
		if visibility.images {
			created = child.DeepCopy()
		}
	}
	if created == nil {
		return nil
	}
	select {
	case visibility.created <- created:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-visibility.returnCreate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (visibility *unobservedBackingClient) List(
	ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption,
) error {
	listErr := visibility.Client.List(ctx, list, options...)
	if listErr != nil {
		return listErr
	}
	select {
	case <-visibility.observe:
		return nil
	default:
	}
	listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
	if listOptions.FieldSelector == nil {
		return nil
	}
	if _, ownerQuery := listOptions.FieldSelector.RequiresExactMatch(".metadata.logicalContainerOwnerUID"); !ownerQuery {
		return nil
	}
	switch children := list.(type) {
	case *apiv2.PhysicalContainerList:
		if !visibility.images {
			children.Items = nil
		}
	case *apiv2.PhysicalContainerImageList:
		if visibility.images {
			children.Items = nil
		}
	}
	return nil
}

// Verifies that logical deletion confirms an empty cached owner list after successful creation,
// deletes every owned backing before finalizing, and leaves unrelated resources untouched.
func TestV2LogicalContainerDeletesCreatedBackingsBeforeCacheObservation(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		images bool
	}{
		{name: "duplicate-containers"},
		{name: "duplicate-images", images: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			returnCreate, observe := make(chan struct{}), make(chan struct{})
			var returnOnce, observeOnce sync.Once
			allowCreateReturn := func() { returnOnce.Do(func() { close(returnCreate) }) }
			allowObservation := func() { observeOnce.Do(func() { close(observe) }) }
			defer allowCreateReturn()
			defer allowObservation()
			var visibility *unobservedBackingClient
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
				NamespaceController|LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
				t.Name(), t.TempDir(), TestEnvironmentOptions{
					DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						visibility = &unobservedBackingClient{
							Client: apiClient, images: testCase.images, created: make(chan ctrl_client.Object, 1),
							returnCreate: returnCreate, observe: observe,
						}
						return visibility
					},
				})
			require.NoError(t, startErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "created-backing"}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
				return current.Status.Phase == apiv2.NamespacePhaseActive, nil
			})
			var unrelated ctrl_client.Object
			config := &apiv2.LogicalContainerConfig{ImageRef: "ready-image"}
			if testCase.images {
				config = &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "created-backing-image"}}
				unrelated = &apiv2.PhysicalContainerImage{
					ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: namespace.Name},
					Spec:       apiv2.PhysicalContainerImageSpec{ImageID: "unrelated-image-id"},
				}
			} else {
				image := &apiv2.PhysicalContainerImage{
					ObjectMeta: metav1.ObjectMeta{Name: "ready-image", Namespace: namespace.Name},
					Spec:       apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: "ready-image"}},
				}
				require.NoError(t, server.Client.Create(ctx, image))
				waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
				})
				unrelated = &apiv2.PhysicalContainer{
					ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: namespace.Name},
					Spec:       apiv2.PhysicalContainerSpec{ContainerID: "unrelated-container-id"},
				}
			}
			require.NoError(t, server.Client.Create(ctx, unrelated))
			logical := &apiv2.LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
				Spec:       apiv2.LogicalContainerSpec{ContainerName: "created-backing-runtime", Container: config},
			}
			require.NoError(t, server.Client.Create(ctx, logical))
			var created ctrl_client.Object
			select {
			case created = <-visibility.created:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var duplicate ctrl_client.Object
			switch child := created.(type) {
			case *apiv2.PhysicalContainer:
				duplicate = child.DeepCopy()
			case *apiv2.PhysicalContainerImage:
				duplicate = child.DeepCopy()
			}
			duplicate.SetName("duplicate")
			duplicate.SetUID("")
			duplicate.SetResourceVersion("")
			require.NoError(t, visibility.Client.Create(ctx, duplicate))
			backings := []ctrl_client.Object{created, duplicate}
			require.NoError(t, server.Client.Delete(ctx, logical))
			waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return !current.DeletionTimestamp.IsZero(), nil
			})
			allowCreateReturn()
			require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(readCtx context.Context) (bool, error) {
				for _, backing := range backings {
					getErr := visibility.Client.Get(readCtx, ctrl_client.ObjectKeyFromObject(backing), backing)
					if getErr != nil {
						return false, getErr
					}
					if backing.GetDeletionTimestamp().IsZero() {
						return false, nil
					}
				}
				return true, nil
			}))
			for _, backing := range backings {
				require.Contains(t, backing.GetFinalizers(), controllers.LogicalBackingFinalizer)
			}
			require.NoError(t, visibility.Client.Get(ctx, logical.NamespacedName(), logical))
			require.Contains(t, logical.Finalizers, apiv2.GroupName+"/logicalcontainer-reconciler")
			require.NoError(t, visibility.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(unrelated), unrelated))
			require.True(t, unrelated.GetDeletionTimestamp().IsZero())
			allowObservation()
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
			for _, backing := range backings {
				switch child := backing.(type) {
				case *apiv2.PhysicalContainer:
					ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, child)
				case *apiv2.PhysicalContainerImage:
					ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, child)
				}
			}
			require.NoError(t, visibility.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(unrelated), unrelated))
			require.True(t, unrelated.GetDeletionTimestamp().IsZero())
			orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			require.Zero(t, orchestrator.CreateContainerCallCount(logical.Spec.ContainerName))
			require.Zero(t, orchestrator.PullImageCallCount("created-backing-image"))
		})
	}
}
