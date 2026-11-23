/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
	dcpio "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/ports"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

func newPortReservationTestStore(t *testing.T) (context.Context, *statestore.Store, *sql.DB) {
	t.Helper()
	ctx, cancel := testutil.GetTestContext(t, 0)
	t.Cleanup(cancel)
	storeDirectory := t.TempDir()
	require.NoError(t, dcpio.EnsureRestrictedDirectory(storeDirectory, osutil.PermissionOnlyOwnerReadWriteTraverse))
	store, openErr := statestore.Open(ctx, statestore.Options{Path: filepath.Join(storeDirectory, "state.sqlite3")})
	require.NoError(t, openErr)
	db, dbErr := sql.Open("sqlite", store.Path())
	require.NoError(t, dbErr)
	owner, ownerErr := process.This()
	require.NoError(t, ownerErr)
	t.Setenv(DCP_PORT_ALLOCATOR, portAllocatorModeStateStore)
	t.Setenv(DCP_PORT_ALLOCATION_RANGE, "26010-26020")
	ConfigureStateStorePortAllocator(store, owner)
	t.Cleanup(func() {
		require.NoError(t, portReservations.Lock(context.WithoutCancel(ctx)))
		ownerCount := len(portReservations.owners)
		portReservations.Unlock()
		require.Zero(t, ownerCount)
		ConfigureStateStorePortAllocator(nil, process.ProcessHandle{})
		require.NoError(t, db.Close())
		require.NoError(t, store.Close())
	})
	return ctx, store, db
}

func portReservationCount(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM port_allocations").Scan(&count))
	return count
}

// Verifies that PortReservationSet allocation and reservation calls return cancellation
// while another operation still holds the ownership lock, without retaining any claims.
func TestPortReservationLockWaitHonorsCancellation(t *testing.T) {
	for _, operation := range []string{"allocate", "reserve", "check"} {
		t.Run(operation, func(t *testing.T) {
			ctx, _, db := newPortReservationTestStore(t)
			callerCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			var owner PortReservationSet
			defer owner.Release(ctx, log)
			require.NoError(t, portReservations.Lock(ctx))
			locked := true
			defer func() {
				if locked {
					portReservations.Unlock()
				}
			}()
			started := make(chan struct{})
			completed := make(chan error, 1)
			go func() {
				close(started)
				switch operation {
				case "allocate":
					_, allocationErr := owner.GetFreePort(callerCtx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
					completed <- allocationErr
				case "reserve":
					completed <- owner.Reserve(callerCtx, ports.Binding{
						Protocol: "TCP", IP: netip.MustParseAddr(IPv4LocalhostDefaultAddress), Port: 26010,
					}, log)
				case "check":
					completed <- owner.CheckPortAvailable(callerCtx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, 26010, log)
				}
			}()
			<-started
			cancel()
			select {
			case operationErr, received := <-completed:
				require.True(t, received)
				require.ErrorIs(t, operationErr, context.Canceled)
			case <-ctx.Done():
				t.Fatal("cancelled reservation operation did not stop waiting for the lock")
			}
			require.Empty(t, owner.bindings)
			portReservations.Unlock()
			locked = false
			require.Zero(t, portReservationCount(t, ctx, db))
		})
	}
}

// Verifies that PortReservationSet.Release discards cancelled owners without releasing shared
// claims, and does not retry the last owner's backend cleanup after cancellation.
func TestPortReservationCancelledReleaseDiscardsOwnershipWithoutRetry(t *testing.T) {
	ctx, _, db := newPortReservationTestStore(t)
	var first, second PortReservationSet
	defer first.Release(ctx, log)
	defer second.Release(ctx, log)
	binding := ports.Binding{Protocol: "TCP", IP: netip.MustParseAddr(IPv4LocalhostDefaultAddress), Port: 26010}
	require.NoError(t, first.Reserve(ctx, binding, log))
	require.NoError(t, second.Reserve(ctx, binding, log))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	first.Release(cancelled, log)
	require.Empty(t, first.bindings)
	require.Equal(t, 1, portReservationCount(t, ctx, db))
	second.Release(cancelled, log)
	require.Empty(t, second.bindings)
	require.NoError(t, portReservations.Lock(ctx))
	ownerCount := len(portReservations.owners)
	portReservations.Unlock()
	require.Zero(t, ownerCount)
	require.Equal(t, 1, portReservationCount(t, ctx, db))
	first.Release(ctx, log)
	second.Release(ctx, log)
	require.Equal(t, 1, portReservationCount(t, ctx, db))
}

// Verifies that PortReservationSet shares exact claims, releases idempotently,
// and allows a new owner to reserve a released binding.
func TestPortReservationSetSharesAndReacquiresClaims(t *testing.T) {
	ctx, _, db := newPortReservationTestStore(t)
	var first, second, replacement PortReservationSet
	defer first.Release(ctx, log)
	defer second.Release(ctx, log)
	defer replacement.Release(ctx, log)
	binding := ports.Binding{Protocol: "tcp", IP: netip.MustParseAddr("::ffff:127.0.0.1"), Port: 26010}
	require.NoError(t, first.Reserve(ctx, binding, log))
	require.NoError(t, second.Reserve(ctx, binding, log))
	require.Equal(t, 1, portReservationCount(t, ctx, db))
	var key portReservationKey
	for retained := range first.bindings {
		key = retained
	}
	require.Equal(t, "TCP", key.binding.Protocol)
	require.Equal(t, netip.MustParseAddr("127.0.0.1"), key.binding.IP)

	first.Release(ctx, log)
	first.Release(ctx, log)
	require.Equal(t, 1, portReservationCount(t, ctx, db), "the second owner still needs the claim")
	second.Release(ctx, log)
	require.Zero(t, portReservationCount(t, ctx, db))
	require.NoError(t, replacement.Reserve(ctx, binding, log))
	require.Equal(t, 1, portReservationCount(t, ctx, db))
	replacement.Release(ctx, log)
	require.Zero(t, portReservationCount(t, ctx, db))
}

// Verifies that PortReservationSet.Reserve avoids repeated SQLite writes for a retained
// binding, including normalized protocol/IP aliases and allocator reconfiguration.
func TestPortReservationSetReserveIsIdempotent(t *testing.T) {
	ctx, store, db := newPortReservationTestStore(t)
	var owner PortReservationSet
	defer owner.Release(ctx, log)
	binding := ports.Binding{Protocol: "tcp", IP: netip.MustParseAddr("::ffff:127.0.0.1"), Port: 26010}
	require.NoError(t, owner.Reserve(ctx, binding, log))
	_, triggerErr := db.ExecContext(ctx, `CREATE TRIGGER fail_reservation_refresh BEFORE UPDATE ON port_allocations
		BEGIN SELECT RAISE(FAIL, 'unexpected reservation refresh'); END`)
	require.NoError(t, triggerErr)
	binding.Protocol = ""
	binding.IP = netip.MustParseAddr("127.0.0.1")
	require.NoError(t, owner.Reserve(ctx, binding, log))
	processOwner, processErr := process.This()
	require.NoError(t, processErr)
	ConfigureStateStorePortAllocator(store, processOwner)
	require.NoError(t, owner.Reserve(ctx, binding, log))
	require.Equal(t, 1, portReservationCount(t, ctx, db))
	owner.Release(ctx, log)
	require.Zero(t, portReservationCount(t, ctx, db))
}

// Verifies that networking allocation and availability APIs treat an empty protocol as TCP
// and retain SQLite-backed claims rather than falling back to MRU.
func TestPortAllocationDefaultsEmptyProtocolToTCP(t *testing.T) {
	ctx, _, db := newPortReservationTestStore(t)
	port, allocationErr := GetFreePort(ctx, "", IPv4LocalhostDefaultAddress, log)
	require.NoError(t, allocationErr)
	require.NoError(t, CheckPortAvailable(ctx, "", IPv4LocalhostDefaultAddress, port, log))
	var first, second PortReservationSet
	defer first.Release(ctx, log)
	defer second.Release(ctx, log)
	require.NoError(t, first.CheckPortAvailable(ctx, "", IPv4LocalhostDefaultAddress, port, log))
	require.NoError(t, second.Reserve(ctx, ports.Binding{IP: netip.MustParseAddr(IPv4LocalhostDefaultAddress), Port: port}, log))
	ownedPort, ownedErr := first.GetFreePort(ctx, "", IPv4LocalhostDefaultAddress, log)
	require.NoError(t, ownedErr)
	require.NotEqual(t, port, ownedPort)
	var nonTCPClaims int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM port_allocations WHERE protocol <> 'TCP'").Scan(&nonTCPClaims))
	require.Zero(t, nonTCPClaims)
	require.Equal(t, 2, portReservationCount(t, ctx, db))
	first.Release(ctx, log)
	require.Equal(t, 1, portReservationCount(t, ctx, db))
	second.Release(ctx, log)
	require.Zero(t, portReservationCount(t, ctx, db))
}

// Verifies that PortReservationSet leaves failed releases reserved without retrying,
// and that subsequent allocations skip the retained claim.
func TestPortReservationFailedReleaseIsNotRetried(t *testing.T) {
	ctx, _, db := newPortReservationTestStore(t)
	var owner PortReservationSet
	port, allocationErr := owner.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	require.NoError(t, allocationErr)
	require.GreaterOrEqual(t, port, int32(26010))
	require.LessOrEqual(t, port, int32(26020))
	_, triggerErr := db.ExecContext(ctx, `CREATE TRIGGER fail_port_release BEFORE DELETE ON port_allocations
		BEGIN SELECT RAISE(FAIL, 'injected port release failure'); END`)
	require.NoError(t, triggerErr)
	owner.Release(ctx, log)
	require.Equal(t, 1, portReservationCount(t, ctx, db))
	_, dropErr := db.ExecContext(ctx, "DROP TRIGGER fail_port_release")
	require.NoError(t, dropErr)

	owner.Release(ctx, log)
	require.Equal(t, 1, portReservationCount(t, ctx, db))
	var next PortReservationSet
	defer next.Release(ctx, log)
	nextPort, nextErr := next.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	require.NoError(t, nextErr)
	require.NotEqual(t, port, nextPort)
	require.Equal(t, 2, portReservationCount(t, ctx, db))
	next.Release(ctx, log)
	require.Equal(t, 1, portReservationCount(t, ctx, db))
}

// Verifies that allocator configuration is cached until explicitly reconfigured and that MRU
// allocations retain their original backend even if the configured backend subsequently changes.
func TestPortReservationBackendAndConfigurationSnapshot(t *testing.T) {
	ctx, store, db := newPortReservationTestStore(t)
	config, _, configErr := getStateStorePortAllocationConfig(log)
	require.NoError(t, configErr)
	t.Setenv(DCP_PORT_ALLOCATOR, portAllocatorModeMru)
	cached, _, cachedErr := getStateStorePortAllocationConfig(log)
	require.NoError(t, cachedErr)
	require.Same(t, config, cached)
	ConfigureStateStorePortAllocator(store, config.owner)
	var owner PortReservationSet
	defer owner.Release(ctx, log)
	_, allocationErr := owner.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	require.NoError(t, allocationErr)
	var key portReservationKey
	for retained := range owner.bindings {
		key = retained
	}
	require.Nil(t, key.config)
	t.Setenv(DCP_PORT_ALLOCATOR, portAllocatorModeStateStore)
	ConfigureStateStorePortAllocator(store, config.owner)
	owner.Release(ctx, log)
	require.Zero(t, portReservationCount(t, ctx, db))
}

// Verifies that a failed SQLite allocation falls back to an MRU-recorded ephemeral port,
// without writing that claim to SQLite, and releases it through the same MRU backend.
func TestPortReservationFallbackUsesOnlyMru(t *testing.T) {
	ctx, store, db := newPortReservationTestStore(t)
	owner, ownerErr := process.This()
	require.NoError(t, ownerErr)
	t.Setenv(DCP_PORT_ALLOCATOR, portAllocatorModeStateStoreWithFallback)
	ConfigureStateStorePortAllocator(store, owner)
	_, triggerErr := db.ExecContext(ctx, `CREATE TRIGGER fail_port_allocation BEFORE INSERT ON port_allocations
		BEGIN SELECT RAISE(FAIL, 'injected allocation failure'); END`)
	require.NoError(t, triggerErr)
	params := defaultMruPortFileUsageParameters()
	params.failOnPortFileError = true
	mru, mruErr := newMruPortFile(filepath.Join(t.TempDir(), "mru.list"), params)
	require.NoError(t, mruErr)
	portFileLock.Lock()
	previousMru := packageMruPortFile
	packageMruPortFile = mru
	portFileLock.Unlock()
	defer func() {
		portFileLock.Lock()
		packageMruPortFile = previousMru
		portFileLock.Unlock()
		require.NoError(t, mru.Close())
	}()
	var reservations PortReservationSet
	port, allocationErr := reservations.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	require.NoError(t, allocationErr)
	require.Zero(t, portReservationCount(t, ctx, db))
	records, readErr := mru.tryLockAndRead(ctx)
	require.NoError(t, readErr)
	require.NoError(t, mru.Unlock())
	require.Len(t, records, 1)
	require.Equal(t, port, records[0].Port)
	require.Equal(t, programInstanceID, records[0].Instance)
	var key portReservationKey
	for retained := range reservations.bindings {
		key = retained
	}
	require.Nil(t, key.config)
	reservations.Release(ctx, log)
	remaining, remainingErr := mru.tryLockAndRead(ctx)
	require.NoError(t, remainingErr)
	require.NoError(t, mru.Unlock())
	require.Empty(t, remaining)
}

// Verifies that cancelled and expired caller contexts fail MRU allocation and state-store
// fallback, while an allocator-only timeout still permits best-effort allocation.
func TestPortAllocationMruFallbackHonorsCallerContext(t *testing.T) {
	ctx, store, db := newPortReservationTestStore(t)
	config, _, configErr := getStateStorePortAllocationConfig(log)
	require.NoError(t, configErr)
	t.Setenv(DCP_PORT_ALLOCATOR, portAllocatorModeStateStoreWithFallback)
	ConfigureStateStorePortAllocator(store, config.owner)
	params := defaultMruPortFileUsageParameters()
	params.failOnPortFileError = false
	params.portAllocationTimeout = -time.Nanosecond
	mru, mruErr := newMruPortFile(filepath.Join(t.TempDir(), "mru.list"), params)
	require.NoError(t, mruErr)
	portFileLock.Lock()
	previousMru := packageMruPortFile
	packageMruPortFile = mru
	portFileLock.Unlock()
	defer func() {
		portFileLock.Lock()
		packageMruPortFile = previousMru
		portFileLock.Unlock()
		require.NoError(t, mru.Close())
	}()
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	expiredCtx, cancelExpired := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancelExpired()
	for _, callerCtx := range []context.Context{cancelledCtx, expiredCtx} {
		port, allocationErr := allocateRandomEphemeralPortWithMruFile(callerCtx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress,
			[]string{IPv4LocalhostDefaultAddress}, log)
		require.ErrorIs(t, allocationErr, callerCtx.Err())
		require.Zero(t, port)
		fallbackPort, fallbackErr := GetFreePort(callerCtx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
		require.ErrorIs(t, fallbackErr, callerCtx.Err())
		require.Zero(t, fallbackPort)
		var owner PortReservationSet
		ownedPort, ownedErr := owner.GetFreePort(callerCtx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
		require.ErrorIs(t, ownedErr, callerCtx.Err())
		require.Zero(t, ownedPort)
		require.Empty(t, owner.bindings)
	}
	_, readErr := mru.tryLockAndRead(ctx)
	require.NoError(t, readErr)
	_, writeErr := mru.Write([]byte("invalid MRU record\n"))
	require.NoError(t, writeErr)
	require.NoError(t, mru.Unlock())
	port, timeoutFallbackErr := allocateRandomEphemeralPortWithMruFile(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress,
		[]string{IPv4LocalhostDefaultAddress}, log)
	require.NoError(t, timeoutFallbackErr)
	require.Positive(t, port)
	require.NoError(t, ctx.Err())
	require.Zero(t, portReservationCount(t, ctx, db))
}

// Verifies that availability checks still reject occupied ephemeral ports when SQLite
// reservations intentionally exclude the ephemeral range.
func TestPortReservationCheckRejectsOccupiedEphemeralPort(t *testing.T) {
	ctx, _, _ := newPortReservationTestStore(t)
	listener, listenErr := net.Listen("tcp", net.JoinHostPort(IPv4LocalhostDefaultAddress, "0"))
	require.NoError(t, listenErr)
	defer func() { require.NoError(t, listener.Close()) }()
	var owner PortReservationSet
	defer owner.Release(ctx, log)
	require.Error(t, owner.CheckPortAvailable(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress,
		int32(listener.Addr().(*net.TCPAddr).Port), log))
}

// Verifies that GetFreePort and CheckPortAvailable reject localhost ports occupied on any loopback address,
// without creating claims, in state-store and MRU modes.
func TestLocalhostPortChecksProbeEveryLoopbackAddress(t *testing.T) {
	normalizedAddress, normalizeErr := NormalizePortAllocationAddress(Localhost)
	require.NoError(t, normalizeErr)
	loopbackIps, lookupErr := LookupIP(Localhost)
	require.NoError(t, lookupErr)
	otherLoopbackAddress := ""
	for _, ip := range loopbackIps {
		if address := IpToString(ip); address != normalizedAddress {
			otherLoopbackAddress = address
			break
		}
	}
	if otherLoopbackAddress == "" {
		t.Skip("localhost resolves to a single loopback address")
	}

	for _, mode := range []string{portAllocatorModeStateStore, portAllocatorModeMru} {
		t.Run(mode, func(t *testing.T) {
			ctx, _, db := newPortReservationTestStore(t)
			listener, listenErr := net.Listen("tcp", AddressAndPort(otherLoopbackAddress, 0))
			require.NoError(t, listenErr)
			defer func() { require.NoError(t, listener.Close()) }()
			occupiedPort := int32(listener.Addr().(*net.TCPAddr).Port)
			t.Setenv(DCP_PORT_ALLOCATOR, mode)
			t.Setenv(DCP_PORT_ALLOCATION_RANGE, fmt.Sprintf("%d-%d", occupiedPort, occupiedPort))
			t.Setenv(DCP_PORT_ALLOCATION_ALLOW_EPHEMERAL_OVERLAP, "true")

			require.Error(t, CheckPortAvailable(ctx, commonapi.PortProtocolTCP, Localhost, occupiedPort, log))
			if mode == portAllocatorModeStateStore {
				_, allocationErr := GetFreePort(ctx, commonapi.PortProtocolTCP, Localhost, log)
				require.Error(t, allocationErr)
			}
			require.Zero(t, portReservationCount(t, ctx, db))
		})
	}
}

// Verifies that declared bindings can be reserved while their owner's listener is already bound,
// without weakening the bindability check used to select an available port.
func TestPortReservationRetainsAlreadyBoundDeclaredPort(t *testing.T) {
	ctx, store, db := newPortReservationTestStore(t)
	t.Setenv(DCP_PORT_ALLOCATION_ALLOW_EPHEMERAL_OVERLAP, "true")
	processOwner, processErr := process.This()
	require.NoError(t, processErr)
	ConfigureStateStorePortAllocator(store, processOwner)
	listener, listenErr := net.Listen("tcp", net.JoinHostPort(IPv4LocalhostDefaultAddress, "0"))
	require.NoError(t, listenErr)
	defer func() { require.NoError(t, listener.Close()) }()
	binding := ports.Binding{Protocol: string(commonapi.PortProtocolTCP),
		IP: netip.MustParseAddr(IPv4LocalhostDefaultAddress), Port: int32(listener.Addr().(*net.TCPAddr).Port)}
	var owner PortReservationSet
	defer owner.Release(ctx, log)
	require.NoError(t, owner.Reserve(ctx, binding, log))
	require.Equal(t, 1, portReservationCount(t, ctx, db))
	require.Error(t, owner.CheckPortAvailable(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, binding.Port, log))
	require.Equal(t, 1, portReservationCount(t, ctx, db))
}

// Verifies that automatic allocation retains occupied-port exclusions until reconfiguration,
// even after the listener closes, while successfully released claims remain reusable.
func TestPortReservationAllocationRetainsOccupiedPortExclusion(t *testing.T) {
	ctx, store, db := newPortReservationTestStore(t)
	var first PortReservationSet
	defer first.Release(ctx, log)
	port, allocationErr := first.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	require.NoError(t, allocationErr)
	listener, listenErr := net.Listen("tcp", net.JoinHostPort(IPv4LocalhostDefaultAddress, fmt.Sprint(port)))
	require.NoError(t, listenErr)
	t.Cleanup(func() { _ = listener.Close() })
	first.Release(ctx, log)
	require.Zero(t, portReservationCount(t, ctx, db))
	t.Setenv(DCP_PORT_ALLOCATION_RANGE, fmt.Sprintf("%d-%d", port, port))
	owner, ownerErr := process.This()
	require.NoError(t, ownerErr)
	ConfigureStateStorePortAllocator(store, owner)
	var next PortReservationSet
	defer next.Release(ctx, log)
	_, occupiedErr := next.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	require.ErrorContains(t, occupiedErr, "could not find an available port")
	require.Zero(t, portReservationCount(t, ctx, db))
	require.NoError(t, listener.Close())
	_, excludedErr := next.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	require.ErrorContains(t, excludedErr, "could not find an available port")
	require.Zero(t, portReservationCount(t, ctx, db))
	ConfigureStateStorePortAllocator(store, owner)
	reusedPort, reuseErr := next.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	require.NoError(t, reuseErr)
	require.Equal(t, port, reusedPort)
	next.Release(ctx, log)
	var replacement PortReservationSet
	defer replacement.Release(ctx, log)
	releasedPort, releaseReuseErr := replacement.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	require.NoError(t, releaseReuseErr)
	require.Equal(t, port, releasedPort, "released scan claims must become available without reconfiguring")
}

// Verifies that state-store allocation skips known reservations incrementally but resumes
// the random walk when an incremental candidate is blocked by SQLite or a listener.
func TestStateStorePortAllocationHybridScan(t *testing.T) {
	for _, blocker := range []string{"owned reservation", "database reservation", "listener"} {
		t.Run(blocker, func(t *testing.T) {
			ctx, store, db := newPortReservationTestStore(t)
			config, _, configErr := getStateStorePortAllocationConfig(log)
			require.NoError(t, configErr)
			binding := ports.Binding{Protocol: string(commonapi.PortProtocolTCP),
				IP: netip.MustParseAddr(IPv4LocalhostDefaultAddress), Port: 26011}
			expected := int32(26013)
			switch blocker {
			case "owned reservation":
				var declared PortReservationSet
				defer declared.Release(ctx, log)
				require.NoError(t, declared.Reserve(ctx, binding, log))
				expected = 26012
			case "database reservation":
				_, reserveErr := store.CreatePortReservation(ctx, stateStorePortReservationRequest(binding, config.owner))
				require.NoError(t, reserveErr)
			case "listener":
				listener, listenErr := net.Listen("tcp", net.JoinHostPort(IPv4LocalhostDefaultAddress, fmt.Sprint(binding.Port)))
				require.NoError(t, listenErr)
				defer func() { require.NoError(t, listener.Close()) }()
			}

			require.NoError(t, stateStorePortReservationLock.Lock(ctx))
			scan := config.getPortAllocationScan(commonapi.PortProtocolTCP, binding.IP)
			scan.randomWalkCursor = 0
			scan.randomWalkStep = 3
			stateStorePortReservationLock.Unlock()

			var owner PortReservationSet
			defer owner.Release(ctx, log)
			first, firstErr := owner.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
			require.NoError(t, firstErr)
			require.Equal(t, int32(26010), first)
			second, secondErr := owner.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
			require.NoError(t, secondErr)
			require.Equal(t, expected, second)
			third, thirdErr := owner.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
			require.NoError(t, thirdErr)
			require.Equal(t, expected+1, third)
			expectedClaims := 4
			if blocker == "listener" {
				expectedClaims = 3
			}
			require.Equal(t, expectedClaims, portReservationCount(t, ctx, db))
		})
	}
}

// Verifies that state-store allocation can reconsider a conflicting SQLite claim after
// its release, without treating that conflict as a failed bind probe or reconfiguring.
func TestStateStorePortAllocationReconsidersReleasedDatabaseClaim(t *testing.T) {
	ctx, store, db := newPortReservationTestStore(t)
	t.Setenv(DCP_PORT_ALLOCATION_RANGE, "26010-26010")
	config, _, configErr := getStateStorePortAllocationConfig(log)
	require.NoError(t, configErr)
	binding := ports.Binding{Protocol: string(commonapi.PortProtocolTCP),
		IP: netip.MustParseAddr(IPv4LocalhostDefaultAddress), Port: 26010}
	request := stateStorePortReservationRequest(binding, config.owner)
	_, reserveErr := store.CreatePortReservation(ctx, request)
	require.NoError(t, reserveErr)
	var owner PortReservationSet
	defer owner.Release(ctx, log)
	_, conflictErr := owner.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	require.ErrorContains(t, conflictErr, "could not find an available port")
	require.Equal(t, 1, portReservationCount(t, ctx, db))
	require.NoError(t, store.ReleasePort(ctx, request))
	port, allocationErr := owner.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	require.NoError(t, allocationErr)
	require.Equal(t, binding.Port, port)
	require.Equal(t, 1, portReservationCount(t, ctx, db))
}

// Verifies that concurrent GetFreePort calls sharing a stateful scan return distinct
// ports and commit every allocation to SQLite.
func TestStateStorePortAllocationConcurrentScan(t *testing.T) {
	ctx, _, db := newPortReservationTestStore(t)
	allocated := make([]int32, 6)
	allocationErrors := make([]error, len(allocated))
	var pending sync.WaitGroup
	for index := range allocated {
		pending.Add(1)
		go func() {
			defer pending.Done()
			allocated[index], allocationErrors[index] = GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
		}()
	}
	pending.Wait()
	unique := make(map[int32]struct{})
	for index, port := range allocated {
		require.NoError(t, allocationErrors[index])
		unique[port] = struct{}{}
	}
	require.Len(t, unique, len(allocated))
	require.Equal(t, len(allocated), portReservationCount(t, ctx, db))
}
