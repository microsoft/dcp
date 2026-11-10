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
	"testing"

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
		portReservations.Lock()
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

// Verifies that automatic allocation skips a bound listener even after its SQLite claim
// is released, and can reuse the port once that listener closes.
func TestPortReservationAllocationSkipsReleasedButBoundPort(t *testing.T) {
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
	reusedPort, reuseErr := next.GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	require.NoError(t, reuseErr)
	require.Equal(t, port, reusedPort)
}
