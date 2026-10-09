/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/pubsub"
	"github.com/microsoft/dcp/pkg/testutil"
)

type containerWatcherTestOrchestrator struct {
	containers.ContainerOrchestrator
	watchContainers func(chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error)
	watchNetworks   func(chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error)
}

func (orchestrator containerWatcherTestOrchestrator) WatchContainers(
	sink chan<- containers.EventMessage,
) (*pubsub.Subscription[containers.EventMessage], error) {
	return orchestrator.watchContainers(sink)
}

func (orchestrator containerWatcherTestOrchestrator) WatchNetworks(
	sink chan<- containers.EventMessage,
) (*pubsub.Subscription[containers.EventMessage], error) {
	return orchestrator.watchNetworks(sink)
}

// Verifies that partial, failed, and missing subscriptions cancel every successfully created watch and forwarding channel.
func TestContainerWatcherSubscriptionFailureCleansPartialState(t *testing.T) {
	t.Parallel()

	watchErr := errors.New("watch unavailable")
	for _, testCase := range []struct {
		name            string
		containerResult string
		networkResult   string
	}{
		{name: "container success network failure", containerResult: "success", networkResult: "error"},
		{name: "container failure network success", containerResult: "error", networkResult: "success"},
		{name: "missing container subscription", containerResult: "missing", networkResult: "success"},
		{name: "missing network subscription", containerResult: "success", networkResult: "missing"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			lifetimeCtx, lifetimeCancel := testutil.GetTestContext(t, 0)
			defer lifetimeCancel()
			containerSubscriptions := pubsub.NewSubscriptionSet(
				func(ctx context.Context, _ *pubsub.SubscriptionSet[containers.EventMessage]) { <-ctx.Done() },
				lifetimeCtx,
			)
			networkSubscriptions := pubsub.NewSubscriptionSet(
				func(ctx context.Context, _ *pubsub.SubscriptionSet[containers.EventMessage]) { <-ctx.Done() },
				lifetimeCtx,
			)

			var watcher *ContainerWatcher[apiv1.Container]
			var containerOutput, networkOutput <-chan containers.EventMessage
			var containerSubscription, networkSubscription *pubsub.Subscription[containers.EventMessage]
			orchestrator := containerWatcherTestOrchestrator{
				watchContainers: func(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
					containerOutput = watcher.containerEvtCh.Out
					switch testCase.containerResult {
					case "success":
						containerSubscription = containerSubscriptions.Subscribe(sink)
						return containerSubscription, nil
					case "error":
						return nil, watchErr
					default:
						return nil, nil
					}
				},
				watchNetworks: func(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
					networkOutput = watcher.networkEvtCh.Out
					switch testCase.networkResult {
					case "success":
						networkSubscription = networkSubscriptions.Subscribe(sink)
						return networkSubscription, nil
					case "error":
						return nil, watchErr
					default:
						return nil, nil
					}
				},
			}

			watcher = NewContainerWatcher[apiv1.Container](orchestrator, &sync.Mutex{}, lifetimeCtx)
			watcher.ProcessContainerEvent = func(containers.EventMessage) {}
			watcher.ProcessNetworkEvent = func(containers.EventMessage) {}
			watcher.EnsureContainerWatchForResource(types.UID("resource"), testr.New(t))

			require.Nil(t, watcher.containerEvtSub)
			require.Nil(t, watcher.networkEvtSub)
			require.Nil(t, watcher.containerEvtCh)
			require.Nil(t, watcher.networkEvtCh)
			require.Nil(t, watcher.containerEvtChCancel)
			require.Nil(t, watcher.networkEvtChCancel)
			require.Nil(t, watcher.containerEvtWorkerStop)
			if containerSubscription != nil {
				require.True(t, containerSubscription.Cancelled())
			}
			if networkSubscription != nil {
				require.True(t, networkSubscription.Cancelled())
			}
			requireContainerWatcherEventChannelClosed(t, lifetimeCtx, containerOutput)
			requireContainerWatcherEventChannelClosed(t, lifetimeCtx, networkOutput)
			require.NoError(t, lifetimeCtx.Err())
		})
	}
}

// Verifies that adding resources preserves one shared watch until the final resource releases it.
func TestContainerWatcherRepeatedRegistrationPreservesSubscriptions(t *testing.T) {
	t.Parallel()

	lifetimeCtx, lifetimeCancel := testutil.GetTestContext(t, 0)
	defer lifetimeCancel()
	containerSubscriptions := pubsub.NewSubscriptionSet(
		func(ctx context.Context, _ *pubsub.SubscriptionSet[containers.EventMessage]) { <-ctx.Done() },
		lifetimeCtx,
	)
	networkSubscriptions := pubsub.NewSubscriptionSet(
		func(ctx context.Context, _ *pubsub.SubscriptionSet[containers.EventMessage]) { <-ctx.Done() },
		lifetimeCtx,
	)

	var watcher *ContainerWatcher[apiv1.Container]
	var containerOutput, networkOutput <-chan containers.EventMessage
	containerCalls := 0
	networkCalls := 0
	orchestrator := containerWatcherTestOrchestrator{
		watchContainers: func(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
			containerCalls++
			containerOutput = watcher.containerEvtCh.Out
			return containerSubscriptions.Subscribe(sink), nil
		},
		watchNetworks: func(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
			networkCalls++
			networkOutput = watcher.networkEvtCh.Out
			return networkSubscriptions.Subscribe(sink), nil
		},
	}

	watcher = NewContainerWatcher[apiv1.Container](orchestrator, &sync.Mutex{}, lifetimeCtx)
	watcher.ProcessContainerEvent = func(containers.EventMessage) {}
	watcher.ProcessNetworkEvent = func(containers.EventMessage) {}
	log := testr.New(t)

	watcher.EnsureContainerWatchForResource(types.UID("first"), log)
	containerSubscription := watcher.containerEvtSub
	networkSubscription := watcher.networkEvtSub
	watcher.EnsureContainerWatchForResource(types.UID("second"), log)

	require.Equal(t, 1, containerCalls)
	require.Equal(t, 1, networkCalls)
	require.Same(t, containerSubscription, watcher.containerEvtSub)
	require.Same(t, networkSubscription, watcher.networkEvtSub)
	require.False(t, containerSubscription.Cancelled())
	require.False(t, networkSubscription.Cancelled())

	watcher.ReleaseContainerWatchForResource(types.UID("first"), log)
	require.False(t, containerSubscription.Cancelled())
	require.False(t, networkSubscription.Cancelled())
	watcher.ReleaseContainerWatchForResource(types.UID("second"), log)

	require.True(t, containerSubscription.Cancelled())
	require.True(t, networkSubscription.Cancelled())
	require.Nil(t, watcher.containerEvtSub)
	require.Nil(t, watcher.networkEvtSub)
	require.Nil(t, watcher.containerEvtCh)
	require.Nil(t, watcher.networkEvtCh)
	requireContainerWatcherEventChannelClosed(t, lifetimeCtx, containerOutput)
	requireContainerWatcherEventChannelClosed(t, lifetimeCtx, networkOutput)
	require.NoError(t, lifetimeCtx.Err())
}

func requireContainerWatcherEventChannelClosed(t *testing.T, ctx context.Context, output <-chan containers.EventMessage) {
	t.Helper()
	require.NotNil(t, output)
	select {
	case _, open := <-output:
		require.False(t, open, "watcher event channel should close without controller shutdown")
	case <-ctx.Done():
		t.Fatalf("watcher event channel was not released: %v", ctx.Err())
	}
}
