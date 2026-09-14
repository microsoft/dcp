/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/pubsub"
	"github.com/microsoft/dcp/pkg/testutil"
)

type networkWatchTestOrchestrator struct {
	containers.ContainerOrchestrator
	watch func(chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error)
}

func (orchestrator networkWatchTestOrchestrator) WatchNetworks(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
	return orchestrator.watch(sink)
}

func TestNetworkWatchFailureReleasesChannels(t *testing.T) {
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

			lifetimeCtx, lifetimeCancel := testutil.GetTestContext(t, 0)
			defer lifetimeCancel()
			subscriptions := pubsub.NewSubscriptionSet(func(ctx context.Context, _ *pubsub.SubscriptionSet[containers.EventMessage]) {
				<-ctx.Done()
			}, lifetimeCtx)
			var reconciler *NetworkReconciler
			var output <-chan containers.EventMessage
			var subscription *pubsub.Subscription[containers.EventMessage]
			orchestrator := networkWatchTestOrchestrator{
				watch: func(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
					output = reconciler.networkEvtCh.Out
					subscription = nil
					if testCase.returnSubscription {
						subscription = subscriptions.Subscribe(sink)
					}
					return subscription, testCase.err
				},
			}
			log := testr.New(t)
			reconciler = NewNetworkReconciler(lifetimeCtx, nil, nil, log, orchestrator, nil)
			network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{UID: "network"}}

			for attempt := 0; attempt < 3; attempt++ {
				reconciler.ensureNetworkWatch(network, log)
				require.Nil(t, reconciler.networkEvtCh)
				require.Nil(t, reconciler.networkEvtChCancel)
				require.Nil(t, reconciler.networkEvtSub)
				require.Nil(t, reconciler.networkEvtWorkerStop)
				if subscription != nil {
					require.True(t, subscription.Cancelled())
				}
				requireNetworkEventChannelClosed(t, lifetimeCtx, output)
				require.NoError(t, lifetimeCtx.Err())
			}
		})
	}
}

func TestNetworkWatchReleaseClosesChannelsBeforeShutdown(t *testing.T) {
	t.Parallel()

	lifetimeCtx, lifetimeCancel := testutil.GetTestContext(t, 0)
	defer lifetimeCancel()
	subscriptions := pubsub.NewSubscriptionSet(func(ctx context.Context, _ *pubsub.SubscriptionSet[containers.EventMessage]) {
		<-ctx.Done()
	}, lifetimeCtx)
	orchestrator := networkWatchTestOrchestrator{watch: func(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
		return subscriptions.Subscribe(sink), nil
	}}
	log := testr.New(t)
	reconciler := NewNetworkReconciler(lifetimeCtx, nil, nil, log, orchestrator, nil)
	first := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{UID: "first"}}
	second := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{UID: "second"}}

	reconciler.ensureNetworkWatch(first, log)
	subscription := reconciler.networkEvtSub
	output := reconciler.networkEvtCh.Out
	reconciler.ensureNetworkWatch(second, log)
	require.Same(t, subscription, reconciler.networkEvtSub)

	reconciler.releaseNetworkWatch(first, log)
	require.False(t, subscription.Cancelled())
	reconciler.releaseNetworkWatch(second, log)
	require.True(t, subscription.Cancelled())
	require.Nil(t, reconciler.networkEvtCh)
	require.Nil(t, reconciler.networkEvtChCancel)
	requireNetworkEventChannelClosed(t, lifetimeCtx, output)
	require.NoError(t, lifetimeCtx.Err())
}

func requireNetworkEventChannelClosed(t *testing.T, ctx context.Context, output <-chan containers.EventMessage) {
	t.Helper()
	require.NotNil(t, output)
	select {
	case _, open := <-output:
		require.False(t, open, "network event channel should close without controller shutdown")
	case <-ctx.Done():
		t.Fatalf("network event channel was not released: %v", ctx.Err())
	}
}
