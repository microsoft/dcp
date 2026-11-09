/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/pubsub"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

type networkWatchOrchestrator struct {
	containers.ContainerOrchestrator
	watch     func(chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error)
	calls     chan *pubsub.Subscription[containers.EventMessage]
	callCount atomic.Int32
}

func (orchestrator *networkWatchOrchestrator) WatchNetworks(
	sink chan<- containers.EventMessage,
) (*pubsub.Subscription[containers.EventMessage], error) {
	subscription, watchErr := orchestrator.watch(sink)
	orchestrator.callCount.Add(1)
	orchestrator.calls <- subscription
	return subscription, watchErr
}

// Verifies that NetworkReconciler cancels partial subscriptions and retries failed or missing watches as API resources are created.
// A later successful subscription must remain active until its network is deleted.
func TestNetworkWatchFailureRetriesThroughReconciliation(t *testing.T) {
	t.Parallel()

	watchErr := errors.New("native event watching is unavailable")
	for _, testCase := range []struct {
		name               string
		returnSubscription bool
		err                error
	}{
		{name: "unsupported", err: watchErr},
		{name: "partial subscription", returnSubscription: true, err: watchErr},
		{name: "missing subscription"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var recordingOrchestrator *networkWatchOrchestrator
			serverInfo, _, startErr := StartTestEnvironmentWithOptions(
				t, ctx, NetworkController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{
					DecorateContainerOrchestrator: func(
						orchestrator containers.ContainerOrchestrator,
						_ *statestore.Store,
					) containers.ContainerOrchestrator {
						recordingOrchestrator = &networkWatchOrchestrator{
							ContainerOrchestrator: orchestrator,
							calls:                 make(chan *pubsub.Subscription[containers.EventMessage], 4),
						}
						recordingOrchestrator.watch = func(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
							if recordingOrchestrator.callCount.Load() < 3 {
								if testCase.returnSubscription {
									subscription, subscribeErr := orchestrator.WatchNetworks(sink)
									return subscription, errors.Join(testCase.err, subscribeErr)
								}
								return nil, testCase.err
							}
							return orchestrator.WatchNetworks(sink)
						}
						return recordingOrchestrator
					},
				},
			)
			require.NoError(t, startErr)

			for attempt := 0; attempt < 4; attempt++ {
				network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("network-watch-retry-%d", attempt),
				}}
				require.NoError(t, serverInfo.Client.Create(ctx, network))
				readyNetwork := ensureNetworkCreatedEx(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, network)
				subscription := receiveNetworkWatchSubscription(t, ctx, recordingOrchestrator.calls)
				require.Equal(t, int32(attempt+1), recordingOrchestrator.callCount.Load())
				if attempt < 3 {
					if testCase.returnSubscription {
						require.NotNil(t, subscription)
						require.True(t, subscription.Cancelled())
					} else {
						require.Nil(t, subscription)
					}
				} else {
					require.NotNil(t, subscription)
					require.False(t, subscription.Cancelled())
				}

				deleteWatchedNetwork(t, ctx, serverInfo, readyNetwork)
				if subscription != nil {
					require.True(t, subscription.Cancelled())
				}
				require.NoError(t, ctx.Err())
			}
		})
	}
}

// Verifies that NetworkReconciler shares a subscription across API resources, cancels it after the final deletion, and starts a fresh watch for a later network.
func TestNetworkWatchReleaseThroughResourceDeletion(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var recordingOrchestrator *networkWatchOrchestrator
	serverInfo, _, startErr := StartTestEnvironmentWithOptions(
		t, ctx, NetworkController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{
			DecorateContainerOrchestrator: func(
				orchestrator containers.ContainerOrchestrator,
				_ *statestore.Store,
			) containers.ContainerOrchestrator {
				recordingOrchestrator = &networkWatchOrchestrator{
					ContainerOrchestrator: orchestrator,
					watch:                 orchestrator.WatchNetworks,
					calls:                 make(chan *pubsub.Subscription[containers.EventMessage], 2),
				}
				return recordingOrchestrator
			},
		},
	)
	require.NoError(t, startErr)

	first := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "network-watch-first"}}
	require.NoError(t, serverInfo.Client.Create(ctx, first))
	readyFirst := ensureNetworkCreatedEx(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, first)
	subscription := receiveNetworkWatchSubscription(t, ctx, recordingOrchestrator.calls)
	require.NotNil(t, subscription)

	second := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "network-watch-second"}}
	require.NoError(t, serverInfo.Client.Create(ctx, second))
	readySecond := ensureNetworkCreatedEx(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, second)
	require.Equal(t, int32(1), recordingOrchestrator.callCount.Load())
	require.False(t, subscription.Cancelled())

	deleteWatchedNetwork(t, ctx, serverInfo, readyFirst)
	require.False(t, subscription.Cancelled())
	deleteWatchedNetwork(t, ctx, serverInfo, readySecond)
	require.True(t, subscription.Cancelled())
	require.NoError(t, ctx.Err())

	third := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "network-watch-third"}}
	require.NoError(t, serverInfo.Client.Create(ctx, third))
	readyThird := ensureNetworkCreatedEx(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, third)
	restartedSubscription := receiveNetworkWatchSubscription(t, ctx, recordingOrchestrator.calls)
	require.NotNil(t, restartedSubscription)
	require.NotSame(t, subscription, restartedSubscription)
	require.Equal(t, int32(2), recordingOrchestrator.callCount.Load())
	require.False(t, restartedSubscription.Cancelled())
	deleteWatchedNetwork(t, ctx, serverInfo, readyThird)
	require.True(t, restartedSubscription.Cancelled())
	require.NoError(t, ctx.Err())
}

func receiveNetworkWatchSubscription(
	t *testing.T,
	ctx context.Context,
	calls <-chan *pubsub.Subscription[containers.EventMessage],
) *pubsub.Subscription[containers.EventMessage] {
	t.Helper()
	select {
	case subscription, open := <-calls:
		require.True(t, open, "watch calls closed before the expected subscription")
		return subscription
	case <-ctx.Done():
		t.Fatalf("waiting for network watch subscription: %v", ctx.Err())
		return nil
	}
}

func deleteWatchedNetwork(
	t *testing.T,
	ctx context.Context,
	serverInfo *ctrl_testutil.ApiServerInfo,
	network *apiv1.ContainerNetwork,
) {
	t.Helper()
	require.NoError(t, serverInfo.Client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, network)
	remainingNetworks, inspectErr := serverInfo.ContainerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: []string{network.Status.ID},
	})
	require.Empty(t, remainingNetworks)
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
}
