/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2resourcelogs

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

// Verifies that network log preflight accepts system diagnostics in pending and failed states
// while rejecting output and startup sources before any capture is initialized.
func TestNetworkLogPreflightSupportsOnlySystem(t *testing.T) {
	streamer := NewLogStreamer(logr.Discard())
	defer func() { require.NoError(t, streamer.Dispose()) }()
	for _, obj := range []resource.Object{
		&apiv2.LogicalContainerNetwork{
			ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: "namespace", UID: "logical-uid"},
			Status:     apiv2.LogicalContainerNetworkStatus{Phase: apiv2.LogicalContainerNetworkPhasePending},
		},
		&apiv2.PhysicalContainerNetwork{
			ObjectMeta: metav1.ObjectMeta{Name: "physical", Namespace: "namespace", UID: "physical-uid"},
			Status:     apiv2.PhysicalContainerNetworkStatus{Phase: apiv2.PhysicalContainerNetworkPhaseFailed},
		},
	} {
		require.NoError(t, streamer.Preflight(obj, &apiv2.ResourceLogOptions{Source: "system", Follow: true}))
		for _, source := range []string{"", "stdout", "stderr", "startup_stdout", "startup_stderr"} {
			require.True(t, apierrors.IsBadRequest(streamer.Preflight(obj, &apiv2.ResourceLogOptions{Source: source})))
		}

		require.Nil(t, streamer.captures)
		require.Nil(t, streamer.source)
	}
}

// Verifies that network system sessions ignore readiness/backing changes and physical-child
// deletion, and stop only when their own watched API resource is deleted.
func TestNetworkSystemSessionsFollowOwnResourceLifecycle(t *testing.T) {
	physical := &apiv2.PhysicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{UID: "physical-uid"}}
	logical := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{UID: "logical-uid"},
		Status:     apiv2.LogicalContainerNetworkStatus{Phase: apiv2.LogicalContainerNetworkPhasePending},
	}
	logicalCtx, cancelLogical := context.WithCancel(t.Context())
	defer cancelLogical()
	physicalCtx, cancelPhysical := context.WithCancel(t.Context())
	defer cancelPhysical()
	logicalSession := &logSession{source: "system", cancel: cancelLogical}
	physicalSession := &logSession{source: "system", cancel: cancelPhysical}
	streamer := &resourceLogStreamer{
		log: logr.Discard(),
		sessions: map[types.UID]map[uint64]*logSession{
			logical.UID: {1: logicalSession}, physical.UID: {2: physicalSession},
		},
	}
	logical.Status.Phase = apiv2.LogicalContainerNetworkPhaseFailed
	logical.Status.PhysicalNetworkRef = "changed-backing"
	streamer.OnResourceUpdated(apiv2.ResourceWatcherEvent{Type: watch.Modified, Object: logical}, logr.Discard())
	require.False(t, logicalSession.finishing)
	require.NoError(t, logicalCtx.Err())
	streamer.OnResourceUpdated(apiv2.ResourceWatcherEvent{Type: watch.Deleted, Object: physical}, logr.Discard())
	require.ErrorIs(t, physicalCtx.Err(), context.Canceled)
	require.False(t, logicalSession.finishing)
	require.NoError(t, logicalCtx.Err())
	streamer.OnResourceUpdated(apiv2.ResourceWatcherEvent{Type: watch.Deleted, Object: logical}, logr.Discard())
	require.ErrorIs(t, logicalCtx.Err(), context.Canceled)
}
