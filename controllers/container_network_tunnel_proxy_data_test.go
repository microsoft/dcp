/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that proxy data clones share startup ownership, only one owner can acquire it,
// and an earlier owner's idempotent release cannot unlock a later owner.
func TestStartupLeaseIsSharedAcrossProxyDataClones(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	data := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, "test")
	clone := data.Clone()

	releaseFirst, firstAcquired := data.startup.TryAcquire(testCtx)
	require.True(t, firstAcquired)
	_, cloneAcquired := clone.startup.TryAcquire(testCtx)
	require.False(t, cloneAcquired)

	releaseFirst()
	releaseSecond, secondAcquired := clone.startup.TryAcquire(testCtx)
	require.True(t, secondAcquired)
	releaseFirst()
	_, thirdAcquired := data.startup.TryAcquire(testCtx)
	require.False(t, thirdAcquired)
	releaseSecond()
}

// Verifies that cancelling an owner's lifetime context releases the shared startup lease,
// permits a new owner to acquire it, and makes the old owner's release harmless.
func TestStartupLeaseReleasesWhenLifetimeEnds(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	ownerCtx, ownerCancel := context.WithCancel(testCtx)
	data := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, "test")

	releaseFirst, firstAcquired := data.startup.TryAcquire(ownerCtx)
	require.True(t, firstAcquired)
	ownerCancel()

	var releaseSecond func()
	acquireErr := wait.PollUntilContextCancel(testCtx, time.Millisecond, true, func(context.Context) (bool, error) {
		var acquired bool
		releaseSecond, acquired = data.startup.TryAcquire(testCtx)
		return acquired, nil
	})
	require.NoError(t, acquireErr)

	releaseFirst()
	_, thirdAcquired := data.startup.TryAcquire(testCtx)
	require.False(t, thirdAcquired)
	releaseSecond()
}

// Verifies that in-memory proxy state cannot merge updates owned by a replacement
// resource with the same namespaced name but a different UID.
func TestContainerNetworkTunnelProxyDataRejectsDifferentResourceUID(t *testing.T) {
	t.Parallel()

	current := newContainerNetworkTunnelProxyData(
		apiv1.ContainerNetworkTunnelProxyStateStarting,
		"current",
	)
	replacement := newContainerNetworkTunnelProxyData(
		apiv1.ContainerNetworkTunnelProxyStateRunning,
		"replacement",
	)

	require.False(t, current.UpdateFrom(replacement))
	require.Equal(t, types.UID("current"), current.resourceUID)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateStarting, current.State)
}

func requireSortedByName(t *testing.T, statuses []apiv1.TunnelStatus) {
	for i := 1; i < len(statuses); i++ {
		require.Less(t, statuses[i-1].Name, statuses[i].Name, "%s: TunnelStatuses should be sorted by name")
	}
}

func TestContainerProxyDataFirstTunnelStatus(t *testing.T) {
	t.Parallel()

	tpd := &containerNetworkTunnelProxyData{}
	tunnel := apiv1.TunnelStatus{
		Name:      "alpha",
		State:     apiv1.TunnelStateReady,
		Timestamp: metav1.NewMicroTime(time.Now()),
	}

	tpd.setTunnelStatus(tunnel)

	require.Len(t, tpd.TunnelStatuses, 1)
	require.Equal(t, "alpha", tpd.TunnelStatuses[0].Name)
	requireSortedByName(t, tpd.TunnelStatuses)
}

func TestContainerProxyDataMultipleTunnelStatuses(t *testing.T) {
	t.Parallel()

	tpd := &containerNetworkTunnelProxyData{}

	// Add tunnel statuses with names that are out of order
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "zulu", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "alpha", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "mike", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "bravo", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})

	require.Len(t, tpd.TunnelStatuses, 4)
	requireSortedByName(t, tpd.TunnelStatuses)
}

func TestContainerProxyDataTunnelStatusUpdates(t *testing.T) {
	t.Parallel()

	tpd := &containerNetworkTunnelProxyData{}

	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "alpha", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "bravo", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "charlie", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})

	// Update middle tunnel
	updatedTunnel := apiv1.TunnelStatus{
		Name:         "bravo",
		State:        apiv1.TunnelStateFailed,
		ErrorMessage: "Connection failed",
		Timestamp:    metav1.NewMicroTime(time.Now()),
	}
	tpd.setTunnelStatus(updatedTunnel)

	require.Len(t, tpd.TunnelStatuses, 3)
	requireSortedByName(t, tpd.TunnelStatuses)

	// Verify the tunnel was updated
	require.Equal(t, "bravo", tpd.TunnelStatuses[1].Name)
	require.Equal(t, apiv1.TunnelStateFailed, tpd.TunnelStatuses[1].State)
	require.Equal(t, "Connection failed", tpd.TunnelStatuses[1].ErrorMessage)
}

func TestContainerProxyDataTunnelStatusRemoveFromEmpty(t *testing.T) {
	t.Parallel()

	tpd := &containerNetworkTunnelProxyData{}

	// Should be a no-op and should not panic
	tpd.removeTunnelStatus("nonexistent")

	require.Empty(t, tpd.TunnelStatuses)
}

func TestContainerProxyDataTunnelStatusRemoveNonexistent(t *testing.T) {
	t.Parallel()

	tpd := &containerNetworkTunnelProxyData{}
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "alpha", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "bravo", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})

	originalLength := len(tpd.TunnelStatuses)

	// Should be a no-op
	tpd.removeTunnelStatus("nonexistent")

	require.Len(t, tpd.TunnelStatuses, originalLength)
	requireSortedByName(t, tpd.TunnelStatuses)
}

func TestContainerProxyDataTunnelStatusRemoveFirst(t *testing.T) {
	t.Parallel()

	tpd := &containerNetworkTunnelProxyData{}
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "alpha", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "bravo", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "charlie", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})

	tpd.removeTunnelStatus("alpha")

	require.Len(t, tpd.TunnelStatuses, 2)
	requireSortedByName(t, tpd.TunnelStatuses)

	require.Equal(t, "bravo", tpd.TunnelStatuses[0].Name)
	require.Equal(t, "charlie", tpd.TunnelStatuses[1].Name)
}

func TestContainerProxyDataTunnelStatusRemoveMiddle(t *testing.T) {
	t.Parallel()

	tpd := &containerNetworkTunnelProxyData{}
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "alpha", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "bravo", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "charlie", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})

	tpd.removeTunnelStatus("bravo")

	require.Len(t, tpd.TunnelStatuses, 2)
	requireSortedByName(t, tpd.TunnelStatuses)

	require.Equal(t, "alpha", tpd.TunnelStatuses[0].Name)
	require.Equal(t, "charlie", tpd.TunnelStatuses[1].Name)
}

func TestContainerProxyDataTunnelStatusRemoveLast(t *testing.T) {
	t.Parallel()

	tpd := &containerNetworkTunnelProxyData{}
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "alpha", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "bravo", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "charlie", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})

	tpd.removeTunnelStatus("charlie")

	require.Len(t, tpd.TunnelStatuses, 2)
	requireSortedByName(t, tpd.TunnelStatuses)

	require.Equal(t, "alpha", tpd.TunnelStatuses[0].Name)
	require.Equal(t, "bravo", tpd.TunnelStatuses[1].Name)
}

func TestContainerProxyDataTunnelStatusComplexMixedOps(t *testing.T) {
	t.Parallel()

	tpd := &containerNetworkTunnelProxyData{}

	// Complex scenario with many operations
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "echo", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "bravo", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "delta", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})

	requireSortedByName(t, tpd.TunnelStatuses)

	// Remove one
	tpd.removeTunnelStatus("bravo")
	requireSortedByName(t, tpd.TunnelStatuses)

	// Add more
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "alpha", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "foxtrot", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})

	requireSortedByName(t, tpd.TunnelStatuses)

	// Update existing
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "delta", State: apiv1.TunnelStateFailed, ErrorMessage: "Failed", Timestamp: metav1.NewMicroTime(time.Now())})

	requireSortedByName(t, tpd.TunnelStatuses)

	// Add in middle
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "charlie", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})

	requireSortedByName(t, tpd.TunnelStatuses)

	// Verify final order
	expectedOrder := []string{"alpha", "charlie", "delta", "echo", "foxtrot"}
	require.Len(t, tpd.TunnelStatuses, len(expectedOrder))
	for i, expected := range expectedOrder {
		require.Equal(t, expected, tpd.TunnelStatuses[i].Name, "position %d should be %s", i, expected)
	}
}

func TestContainerProxyDataTunnelStatusRepeatedOps(t *testing.T) {
	t.Parallel()

	tpd := &containerNetworkTunnelProxyData{}

	// Add base tunnels
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "alpha", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
	tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "charlie", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})

	// Repeatedly add and remove the same tunnel
	for i := 0; i < 3; i++ {
		tpd.setTunnelStatus(apiv1.TunnelStatus{Name: "bravo", State: apiv1.TunnelStateReady, Timestamp: metav1.NewMicroTime(time.Now())})
		require.Len(t, tpd.TunnelStatuses, 3)
		requireSortedByName(t, tpd.TunnelStatuses)

		tpd.removeTunnelStatus("bravo")
		require.Len(t, tpd.TunnelStatuses, 2)
		requireSortedByName(t, tpd.TunnelStatuses)

		require.Equal(t, "alpha", tpd.TunnelStatuses[0].Name)
		require.Equal(t, "charlie", tpd.TunnelStatuses[1].Name)
	}
}
