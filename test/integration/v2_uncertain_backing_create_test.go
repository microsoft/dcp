/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/testutil"
)

type uncertainBackingCreateClient struct {
	ctrl_client.Client
	kind     string
	attempts atomic.Int32
	created  chan struct{}
	release  <-chan struct{}
}

func (client *uncertainBackingCreateClient) Create(ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	target := false
	switch obj.(type) {
	case *apiv2.PhysicalContainer:
		target = client.kind == "container"
	case *apiv2.PhysicalContainerImage:
		target = client.kind == "image"
	}
	if target {
		client.attempts.Add(1)
	}
	createErr := client.Client.Create(ctx, obj, options...)
	if createErr != nil || !target {
		return createErr
	}
	close(client.created)
	select {
	case <-client.release:
		return errors.New("injected response failure after committed backing create")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Verifies that uncertain backing creation is resolved immediately through an uncached lookup
// without retrying Create, and a concurrent stop reaches the successfully created physical container.
func TestV2LogicalContainerResolvesUncertainBackingCreate(t *testing.T) {
	for _, kind := range []string{"container", "image"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseResponse := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseResponse()
			var decorated *uncertainBackingCreateClient
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
				NamespaceController|LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
				t.Name(), t.TempDir(), TestEnvironmentOptions{
					DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
						decorated = &uncertainBackingCreateClient{
							Client: client, kind: kind, created: make(chan struct{}), release: release,
						}
						return decorated
					},
				})
			require.NoError(t, startErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "uncertain-create"}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			logical := &apiv2.LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
				Spec: apiv2.LogicalContainerSpec{
					ContainerName: "uncertain-runtime",
					Container:     &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "uncertain-image"}},
				},
			}
			require.NoError(t, server.Client.Create(ctx, logical))
			select {
			case <-decorated.created:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			name := types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(logical.UID)}
			expectedPhase := apiv2.LogicalContainerPhaseRunning
			if kind == "container" {
				waitObjectAssumesStateEx(t, ctx, server.Client, name, func(current *apiv2.PhysicalContainer) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerPhaseRunning, nil
				})
				require.NoError(t, retryOnConflictEx(ctx, server.Client, logical.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
					current.Spec.Stop = true
					return server.Client.Update(updateCtx, current)
				}))
				expectedPhase = apiv2.LogicalContainerPhaseExited
			}
			releaseResponse()
			settled := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return current.Status.Phase == expectedPhase, nil
			})
			require.NotEmpty(t, settled.Status.ContainerID)
			require.Equal(t, name.Name, settled.Status.PhysicalContainerRef)
			require.Equal(t, int32(1), decorated.attempts.Load())
			physical := &apiv2.PhysicalContainer{}
			require.NoError(t, server.Client.Get(ctx, name, physical))
			require.True(t, metav1.IsControlledBy(physical, logical))
			if kind == "container" {
				require.True(t, physical.Spec.Stop)
				require.Equal(t, apiv2.PhysicalContainerPhaseExited, physical.Status.Phase)
			}
		})
	}
}
