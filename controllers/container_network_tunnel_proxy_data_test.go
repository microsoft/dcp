/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/process"
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

// Verifies that proxy recovery state is cloned independently and an old pair's
// completion cannot overwrite a replacement under the same resource UID.
func TestContainerNetworkTunnelProxyDataRecoveryGeneration(t *testing.T) {
	t.Parallel()
	current := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateRunning, "same-resource")
	current.pairGeneration = 2
	current.clientPhysicalUID = "current-container"
	current.recovery.backoff = backoff.NewConstantBackOff(time.Second)
	current.publishedPorts["service"] = 45001
	clone := current.Clone()
	require.Same(t, current.recovery.backoff, clone.recovery.backoff)
	clone.publishedPorts["service"] = 45002
	require.Equal(t, int32(45001), current.publishedPorts["service"])
	clone.State = apiv1.ContainerNetworkTunnelProxyStateRecoveryWait
	clone.recovery.retryAfter = time.Now().Add(time.Second)
	require.True(t, current.recovery.retryAfter.IsZero())
	require.True(t, current.UpdateFrom(clone))
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateRecoveryWait, current.State)
	require.Equal(t, clone.recovery.retryAfter, current.recovery.retryAfter)
	require.Equal(t, int32(45002), current.publishedPorts["service"])
	clone.publishedPorts["service"] = 45003
	require.Equal(t, int32(45002), current.publishedPorts["service"])

	missingIdentity := current.Clone()
	missingIdentity.clientPhysicalUID = ""
	missingIdentity.Message = "state update without physical identity"
	require.True(t, current.UpdateFrom(missingIdentity))
	require.Equal(t, types.UID("current-container"), current.clientPhysicalUID)

	stale := current.Clone()
	stale.pairGeneration = 1
	stale.State = apiv1.ContainerNetworkTunnelProxyStateFailed
	stale.clientPhysicalUID = "old-container"
	require.False(t, current.UpdateFrom(stale))
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateRecoveryWait, current.State)
	require.Equal(t, types.UID("current-container"), current.clientPhysicalUID)
}

// Verifies that containerNetworkTunnelProxyData publishes every lifecycle state
// unchanged, preserves it through API serialization, and avoids redundant updates.
func TestContainerNetworkTunnelProxyDataPublishesLifecycleState(t *testing.T) {
	t.Parallel()
	for _, state := range []apiv1.ContainerNetworkTunnelProxyState{
		apiv1.ContainerNetworkTunnelProxyStateEmpty,
		apiv1.ContainerNetworkTunnelProxyStatePending,
		apiv1.ContainerNetworkTunnelProxyStateBuildingImage,
		apiv1.ContainerNetworkTunnelProxyStateStarting,
		apiv1.ContainerNetworkTunnelProxyStateStarted,
		apiv1.ContainerNetworkTunnelProxyStateRunning,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryPending,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryCleanup,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryWait,
		apiv1.ContainerNetworkTunnelProxyStateFailed,
		apiv1.ContainerNetworkTunnelProxyStateDeletePending,
		apiv1.ContainerNetworkTunnelProxyStateDeleting,
		apiv1.ContainerNetworkTunnelProxyStateDeleted,
	} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			data := newContainerNetworkTunnelProxyData(state, "proxy")
			require.Equal(t, state, data.Clone().State)
			resource := &apiv1.ContainerNetworkTunnelProxy{
				Status: apiv1.ContainerNetworkTunnelProxyStatus{State: apiv1.ContainerNetworkTunnelProxyStateEmpty},
			}
			if state == apiv1.ContainerNetworkTunnelProxyStateEmpty {
				resource.Status.State = apiv1.ContainerNetworkTunnelProxyStatePending
			}
			require.Equal(t, statusChanged, data.applyTo(resource))
			require.Equal(t, state, resource.Status.State)
			require.Equal(t, state, data.State)
			require.Equal(t, noChange, data.applyTo(resource))
			encoded, encodeErr := json.Marshal(resource)
			require.NoError(t, encodeErr)
			fromAPI := &apiv1.ContainerNetworkTunnelProxy{}
			require.NoError(t, json.Unmarshal(encoded, fromAPI))
			require.Equal(t, state, fromAPI.Status.State)
			require.Equal(t, noChange, data.applyTo(fromAPI))
		})
	}
}

// Verifies that containerNetworkTunnelProxyData.attemptProxyRecovery initiates recovery
// for operational-pair and replacement-startup failures, keeps initial startup
// failures terminal, and preserves the failure message.
func TestContainerNetworkTunnelProxyDataAttemptsProxyRecovery(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		state      apiv1.ContainerNetworkTunnelProxyState
		generation uint64
		expected   apiv1.ContainerNetworkTunnelProxyState
	}{
		{apiv1.ContainerNetworkTunnelProxyStatePending, 0, apiv1.ContainerNetworkTunnelProxyStateFailed},
		{apiv1.ContainerNetworkTunnelProxyStateBuildingImage, 0, apiv1.ContainerNetworkTunnelProxyStateFailed},
		{apiv1.ContainerNetworkTunnelProxyStateStarting, 0, apiv1.ContainerNetworkTunnelProxyStateFailed},
		{apiv1.ContainerNetworkTunnelProxyStateStarted, 0, apiv1.ContainerNetworkTunnelProxyStateFailed},
		{apiv1.ContainerNetworkTunnelProxyStateRunning, 0, apiv1.ContainerNetworkTunnelProxyStateRecoveryPending},
		{apiv1.ContainerNetworkTunnelProxyStateBuildingImage, 1, apiv1.ContainerNetworkTunnelProxyStateRecoveryPending},
		{apiv1.ContainerNetworkTunnelProxyStateStarting, 1, apiv1.ContainerNetworkTunnelProxyStateRecoveryPending},
		{apiv1.ContainerNetworkTunnelProxyStateRunning, 1, apiv1.ContainerNetworkTunnelProxyStateRecoveryPending},
		{apiv1.ContainerNetworkTunnelProxyStateBuildingImage, 2, apiv1.ContainerNetworkTunnelProxyStateRecoveryPending},
		{apiv1.ContainerNetworkTunnelProxyStateStarting, 2, apiv1.ContainerNetworkTunnelProxyStateRecoveryPending},
	} {
		t.Run(fmt.Sprintf("%s/Generation%d", testCase.state, testCase.generation), func(t *testing.T) {
			t.Parallel()
			data := newContainerNetworkTunnelProxyData(testCase.state, "proxy")
			data.pairGeneration = testCase.generation
			data.attemptProxyRecovery("tunnel proxy operation failed")
			require.Equal(t, testCase.expected, data.State)
			require.Equal(t, "tunnel proxy operation failed", data.Message)
			clone := data.Clone()
			require.Equal(t, testCase.expected, clone.State)
			require.Equal(t, testCase.generation, clone.pairGeneration)
		})
	}
}

// Verifies that publishing a replacement server identity also clears the old
// startup timestamp, rather than pairing a new PID with an old process identity.
func TestContainerNetworkTunnelProxyDataReplacesServerIdentity(t *testing.T) {
	t.Parallel()
	oldPID := int64(123)
	resource := apiv1.ContainerNetworkTunnelProxy{
		Status: apiv1.ContainerNetworkTunnelProxyStatus{
			ServerProxyProcessID:        &oldPID,
			ServerProxyStartupTimestamp: metav1.NewMicroTime(time.Now().Add(-time.Hour)),
		},
	}
	data := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, "proxy")
	data.applyTo(&resource)
	require.Nil(t, resource.Status.ServerProxyProcessID)
	require.True(t, resource.Status.ServerProxyStartupTimestamp.IsZero())

	newPID := int64(456)
	data.ServerProxyProcessID = &newPID
	data.ServerProxyStartupTimestamp = metav1.NewMicroTime(time.Now())
	data.applyTo(&resource)
	require.Equal(t, newPID, *resource.Status.ServerProxyProcessID)
	require.Equal(t, data.ServerProxyStartupTimestamp, resource.Status.ServerProxyStartupTimestamp)
}

// Verifies that proxy status stays unchanged after API timestamp rounding while
// in-memory process identity retains its sub-microsecond precision.
func TestContainerNetworkTunnelProxyDataTimestampSerialization(t *testing.T) {
	t.Parallel()
	pid := int64(123)
	identityTime := time.Unix(1_700_000_000, 123_456_789).UTC()
	data := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateRunning, "proxy")
	data.ServerProxyProcessID = &pid
	data.ServerProxyStartupTimestamp = metav1.NewMicroTime(identityTime)
	resource := &apiv1.ContainerNetworkTunnelProxy{}
	data.applyTo(resource)
	encoded, encodeErr := json.Marshal(resource)
	require.NoError(t, encodeErr)
	fromAPI := &apiv1.ContainerNetworkTunnelProxy{}
	require.NoError(t, json.Unmarshal(encoded, fromAPI))
	require.Equal(t, noChange, data.applyTo(fromAPI), "Serialization precision must not cause a status-update loop")
	require.True(t, data.hasServerProxy(process.NewHandle(process.Pid_t(pid), identityTime)))
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
