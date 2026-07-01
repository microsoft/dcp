/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package statestore

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/ports"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const stateStoreTestTimeout = 10 * time.Second

func testPortBinding(protocol string, ip string, port int32) ports.Binding {
	return ports.Binding{
		Protocol: protocol,
		IP:       netip.MustParseAddr(ip),
		Port:     port,
	}
}

func openRawSQLiteDB(t *testing.T, ctx context.Context, path string) *sql.DB {
	t.Helper()

	db, openErr := sql.Open(sqliteDriverName, sqliteDSN(path, 500*time.Millisecond))
	require.NoError(t, openErr)

	require.NoError(t, db.PingContext(ctx))
	return db
}

func openTestStore(t *testing.T, ctx context.Context, path string) *Store {
	t.Helper()

	require.NoError(t, usvc_io.EnsureRestrictedDirectory(filepath.Dir(path), osutil.PermissionOnlyOwnerReadWriteTraverse))
	store, openErr := Open(ctx, Options{
		Path:        path,
		BusyTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, openErr)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})
	return store
}

type testLeasableResource string

func (r testLeasableResource) GetLeaseKey() string {
	return string(r)
}

func TestSQLiteDSNUsesURIPathSeparators(t *testing.T) {
	t.Parallel()

	require.Equal(
		t,
		"file:///tmp/dcp/state.sqlite3?_pragma=busy_timeout%3D500",
		sqliteDSN("/tmp/dcp/state.sqlite3", 500*time.Millisecond),
	)
	require.Equal(
		t,
		"file:C:/Users/runner/AppData/Local/Temp/state.sqlite3?_pragma=busy_timeout%3D500",
		sqliteDSN(`C:\Users\runner\AppData\Local\Temp\state.sqlite3`, 500*time.Millisecond),
	)
}

func TestDefaultStateStoreDirRestrictsLeafDirectoryOnly(t *testing.T) {
	t.Parallel()

	dcpFolder := filepath.Join(t.TempDir(), ".dcp")
	require.NoError(t, os.Mkdir(dcpFolder, osutil.PermissionDirectoryOthersRead))

	for _, isAdmin := range []bool{false, true} {
		stateStorePath := defaultStateStorePath(dcpFolder, isAdmin)
		require.NoError(t, ensureStateStoreDir(stateStorePath, false))

		stateStoreDir := filepath.Dir(stateStorePath)
		require.DirExists(t, stateStoreDir)
		require.NoError(t, usvc_io.ValidateRestrictedDirectory(stateStoreDir, osutil.PermissionOnlyOwnerReadWriteTraverse))
	}
	if runtime.GOOS != "windows" {
		rootInfo, rootStatErr := os.Lstat(dcpFolder)
		require.NoError(t, rootStatErr)
		require.Equal(t, osutil.PermissionDirectoryOthersRead, rootInfo.Mode().Perm())
	}
}

func requireMigrationVersion(t *testing.T, ctx context.Context, store *Store, expectedVersion int) {
	t.Helper()

	row := store.db.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`)
	var version int
	var dirty bool
	require.NoError(t, row.Scan(&version, &dirty))
	require.Equal(t, expectedVersion, version)
	require.False(t, dirty)
}

func createUnversionedCurrentSchema(t *testing.T, ctx context.Context, path string) {
	t.Helper()

	db := openRawSQLiteDB(t, ctx, path)
	defer func() {
		require.NoError(t, db.Close())
	}()

	initialMigration, readErr := fs.ReadFile(migrationFiles, "migrations/000001_initial.up.sql")
	require.NoError(t, readErr)

	_, execErr := db.ExecContext(ctx, string(initialMigration))
	require.NoError(t, execErr)
}

func testResourceLeaseOwner(t *testing.T, identityOffset time.Duration) (process.ProcessHandle, error) {
	t.Helper()

	currentProcess, currentProcessErr := process.This()
	if currentProcessErr != nil {
		return process.ProcessHandle{}, currentProcessErr
	}

	currentProcess.IdentityTime = currentProcess.IdentityTime.Add(identityOffset)
	return normalizeResourceLeaseOwner(currentProcess)
}

func setResourceLeaseUpdatedAt(t *testing.T, ctx context.Context, store *Store, resourceKey string, updatedAt time.Time) {
	t.Helper()

	_, updateErr := store.db.ExecContext(
		ctx,
		`UPDATE resource_locks SET updated_at_unix_nano = ? WHERE resource_key = ?`,
		unixNano(updatedAt),
		resourceKey,
	)
	require.NoError(t, updateErr)
}

func TestOpenCreatesSchema(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)

	require.Equal(t, storePath, store.Path())
	require.FileExists(t, storePath)

	requireMigrationVersion(t, ctx, store, currentSchemaVersion)
}

func TestOpenWithExplicitPathRejectsPermissiveExistingParentDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory permissions are not represented by Unix mode bits")
	}
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	stateStoreDir := filepath.Join(t.TempDir(), "state-store")
	require.NoError(t, os.Mkdir(stateStoreDir, osutil.PermissionDirectoryOthersRead))
	require.NoError(t, os.Chmod(stateStoreDir, osutil.PermissionDirectoryOthersRead))
	storePath := filepath.Join(stateStoreDir, "state.sqlite3")

	_, openErr := Open(ctx, Options{Path: storePath})

	require.ErrorContains(t, openErr, "explicit state store directory must already be restricted")
	info, statErr := os.Lstat(stateStoreDir)
	require.NoError(t, statErr)
	require.Equal(t, osutil.PermissionDirectoryOthersRead, info.Mode().Perm())
}

func TestOpenWithEnvPathRejectsPermissiveExistingParentDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory permissions are not represented by Unix mode bits")
	}

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	stateStoreDir := filepath.Join(t.TempDir(), "state-store")
	require.NoError(t, os.Mkdir(stateStoreDir, osutil.PermissionDirectoryOthersRead))
	require.NoError(t, os.Chmod(stateStoreDir, osutil.PermissionDirectoryOthersRead))
	storePath := filepath.Join(stateStoreDir, "state.sqlite3")
	t.Setenv(DCP_STATE_STORE_PATH, storePath)

	_, openErr := Open(ctx, Options{})

	require.ErrorContains(t, openErr, "explicit state store directory must already be restricted")
	info, statErr := os.Lstat(stateStoreDir)
	require.NoError(t, statErr)
	require.Equal(t, osutil.PermissionDirectoryOthersRead, info.Mode().Perm())
}

func TestOpenWithExplicitPathCreatesMissingParentDirectoryRestricted(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	stateStoreDir := filepath.Join(t.TempDir(), "state-store")
	storePath := filepath.Join(stateStoreDir, "state.sqlite3")

	store, openErr := Open(ctx, Options{
		Path:        storePath,
		BusyTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, openErr)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	require.Equal(t, storePath, store.Path())
	require.FileExists(t, storePath)
	info, statErr := os.Lstat(stateStoreDir)
	require.NoError(t, statErr)
	require.True(t, info.IsDir())
	require.NoError(t, usvc_io.ValidateRestrictedDirectory(stateStoreDir, osutil.PermissionOnlyOwnerReadWriteTraverse))
	if runtime.GOOS != "windows" {
		require.Equal(t, osutil.PermissionOnlyOwnerReadWriteTraverse, info.Mode().Perm())
	}
}

func TestOpenConfiguresWALAutoCheckpoint(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)

	row := store.db.QueryRowContext(ctx, "PRAGMA wal_autocheckpoint")
	var autoCheckpointPages int
	require.NoError(t, row.Scan(&autoCheckpointPages))
	require.Equal(t, defaultWALAutoCheckpointPages, autoCheckpointPages)
}

func TestOpenMigratesUnversionedCurrentSchemaWithoutLosingResourceLocks(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	createUnversionedCurrentSchema(t, ctx, storePath)

	db := openRawSQLiteDB(t, ctx, storePath)
	owner, ownerErr := testResourceLeaseOwner(t, 0)
	require.NoError(t, ownerErr)

	now := time.Now().UTC()
	_, insertErr := db.ExecContext(
		ctx,
		`INSERT INTO resource_locks(resource_key, owner_pid, owner_identity_time, updated_at_unix_nano)
		 VALUES(?, ?, ?, ?)`,
		"container/existing",
		owner.Pid,
		timeString(owner.IdentityTime),
		unixNano(now),
	)
	require.NoError(t, insertErr)
	require.NoError(t, db.Close())

	store := openTestStore(t, ctx, storePath)
	requireMigrationVersion(t, ctx, store, currentSchemaVersion)

	otherOwner, otherOwnerErr := testResourceLeaseOwner(t, -time.Hour)
	require.NoError(t, otherOwnerErr)
	_, acquireErr := store.AcquireResourceLease(ctx, testLeasableResource("container/existing"), otherOwner, time.Minute)
	require.ErrorIs(t, acquireErr, ErrResourceLeaseHeld)
}

func TestOpenMigratesUnversionedLegacyResourceLocksTable(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")

	db := openRawSQLiteDB(t, ctx, storePath)
	_, createErr := db.ExecContext(
		ctx,
		`CREATE TABLE resource_locks (
			resource_key TEXT PRIMARY KEY,
			owner_instance_id TEXT NOT NULL,
			updated_at_unix_nano INTEGER NOT NULL
		)`,
	)
	require.NoError(t, createErr)
	require.NoError(t, db.Close())

	store := openTestStore(t, ctx, storePath)
	requireMigrationVersion(t, ctx, store, currentSchemaVersion)

	conn, connErr := store.db.Conn(ctx)
	require.NoError(t, connErr)
	defer func() {
		require.NoError(t, conn.Close())
	}()

	hasOwnerPIDColumn, ownerPIDColumnErr := resourceLocksHasColumn(ctx, conn, "owner_pid")
	require.NoError(t, ownerPIDColumnErr)
	require.True(t, hasOwnerPIDColumn)

	hasLegacyOwnerColumn, legacyOwnerColumnErr := resourceLocksHasColumn(ctx, conn, "owner_instance_id")
	require.NoError(t, legacyOwnerColumnErr)
	require.False(t, hasLegacyOwnerColumn)
}

func TestResourceLeaseCoordinatesAcrossStoreHandles(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)

	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, -time.Hour)
	require.NoError(t, owner2Err)

	lease, acquireErr := store1.AcquireResourceLease(ctx, testLeasableResource("container/test"), owner1, time.Minute)
	require.NoError(t, acquireErr)
	require.Equal(t, owner1, lease.OwnerProcess)

	_, blockedErr := store2.AcquireResourceLease(ctx, testLeasableResource("container/test"), owner2, time.Minute)
	require.ErrorIs(t, blockedErr, ErrResourceLeaseHeld)
	heldLease, foundHeldLease := HeldResourceLease(blockedErr)
	require.True(t, foundHeldLease)
	require.Equal(t, "container/test", heldLease.ResourceKey)
	require.Equal(t, owner1, heldLease.OwnerProcess)

	require.NoError(t, store1.ReleaseResourceLease(ctx, testLeasableResource("container/test"), owner1))

	lease, acquireErr = store2.AcquireResourceLease(ctx, testLeasableResource("container/test"), owner2, time.Minute)
	require.NoError(t, acquireErr)
	require.Equal(t, owner2, lease.OwnerProcess)
}

func TestResourceLeasePreservesOutOfUnixNanoRangeOwnerIdentityTime(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)

	owner, ownerErr := normalizeResourceLeaseOwner(process.ProcessHandle{
		Pid:          process.Pid_t(1234),
		IdentityTime: time.Date(1, time.January, 1, 0, 3, 10, 720000000, time.UTC),
	})
	require.NoError(t, ownerErr)

	lease, acquireErr := store.AcquireResourceLease(ctx, testLeasableResource("container/test"), owner, time.Minute)
	require.NoError(t, acquireErr)
	require.Equal(t, owner, lease.OwnerProcess)

	require.NoError(t, store.ReleaseResourceLease(ctx, testLeasableResource("container/test"), owner))
}

func TestResourceLeaseReleaseRequiresOwnedLease(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)

	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, -time.Hour)
	require.NoError(t, owner2Err)

	missingReleaseErr := store.ReleaseResourceLease(ctx, testLeasableResource("container/missing"), owner1)
	require.ErrorIs(t, missingReleaseErr, ErrResourceLeaseNotHeld)

	_, acquireErr := store.AcquireResourceLease(ctx, testLeasableResource("container/test"), owner1, time.Minute)
	require.NoError(t, acquireErr)

	wrongOwnerReleaseErr := store.ReleaseResourceLease(ctx, testLeasableResource("container/test"), owner2)
	require.ErrorIs(t, wrongOwnerReleaseErr, ErrResourceLeaseNotHeld)

	require.NoError(t, store.ReleaseResourceLease(ctx, testLeasableResource("container/test"), owner1))
}

func TestResourceLeaseVerifyRequiresOwnedLease(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)

	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, -time.Hour)
	require.NoError(t, owner2Err)

	missingVerifyErr := store.VerifyResourceLeaseHeld(ctx, testLeasableResource("container/missing"), owner1)
	require.ErrorIs(t, missingVerifyErr, ErrResourceLeaseNotHeld)

	_, acquireErr := store.AcquireResourceLease(ctx, testLeasableResource("container/test"), owner1, time.Minute)
	require.NoError(t, acquireErr)

	wrongOwnerVerifyErr := store.VerifyResourceLeaseHeld(ctx, testLeasableResource("container/test"), owner2)
	require.ErrorIs(t, wrongOwnerVerifyErr, ErrResourceLeaseNotHeld)

	require.NoError(t, store.VerifyResourceLeaseHeld(ctx, testLeasableResource("container/test"), owner1))
}

func TestResourceLeaseDoesNotExpireWhileOwnerIsActive(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)

	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, -time.Hour)
	require.NoError(t, owner2Err)

	_, acquireErr := store1.AcquireResourceLease(ctx, testLeasableResource("container/test"), owner1, time.Minute)
	require.NoError(t, acquireErr)
	setResourceLeaseUpdatedAt(t, ctx, store1, "container/test", time.Now().UTC().Add(-time.Hour))

	_, retryErr := store2.AcquireResourceLease(ctx, testLeasableResource("container/test"), owner2, time.Minute)
	require.ErrorIs(t, retryErr, ErrResourceLeaseHeld)
}

func TestResourceLeaseCanBeAcquiredFromInactiveOwnerAfterRevalidationInterval(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)

	currentProcess, currentProcessErr := process.This()
	require.NoError(t, currentProcessErr)
	staleOwner, staleOwnerErr := normalizeResourceLeaseOwner(process.ProcessHandle{
		Pid:          currentProcess.Pid,
		IdentityTime: currentProcess.IdentityTime.Add(-time.Hour),
	})
	require.NoError(t, staleOwnerErr)
	activeOwner, activeOwnerErr := normalizeResourceLeaseOwner(currentProcess)
	require.NoError(t, activeOwnerErr)

	_, acquireErr := store1.AcquireResourceLease(ctx, testLeasableResource("container/test"), staleOwner, time.Minute)
	require.NoError(t, acquireErr)
	setResourceLeaseUpdatedAt(t, ctx, store1, "container/test", time.Now().UTC().Add(-time.Hour))

	_, retryErr := store2.AcquireResourceLease(ctx, testLeasableResource("container/test"), activeOwner, time.Minute)
	require.NoError(t, retryErr)
}

func TestWithResourceLeaseDoesNotRetryHeldLease(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)

	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, -time.Hour)
	require.NoError(t, owner2Err)

	_, acquireErr := store1.AcquireResourceLease(ctx, testLeasableResource("container/test"), owner1, time.Minute)
	require.NoError(t, acquireErr)

	callbackCalled := false
	leaseErr := store2.WithResourceLease(ctx, testLeasableResource("container/test"), owner2, time.Minute, func(context.Context, *ResourceLease) error {
		callbackCalled = true
		return nil
	})

	require.ErrorIs(t, leaseErr, ErrResourceLeaseHeld)
	require.False(t, callbackCalled)
}

func TestDeleteInactiveResourceLeasesUsesOwnerProcessIdentity(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)

	currentProcess, currentProcessErr := process.This()
	require.NoError(t, currentProcessErr)
	activeOwner, activeOwnerErr := normalizeResourceLeaseOwner(currentProcess)
	require.NoError(t, activeOwnerErr)
	staleOwner, staleOwnerErr := normalizeResourceLeaseOwner(process.ProcessHandle{
		Pid:          currentProcess.Pid,
		IdentityTime: currentProcess.IdentityTime.Add(-time.Hour),
	})
	require.NoError(t, staleOwnerErr)

	now := time.Now().UTC()
	_, activeAcquireErr := store1.AcquireResourceLease(ctx, testLeasableResource("container/active"), activeOwner, time.Minute)
	require.NoError(t, activeAcquireErr)
	_, staleAcquireErr := store1.AcquireResourceLease(ctx, testLeasableResource("container/stale"), staleOwner, time.Minute)
	require.NoError(t, staleAcquireErr)
	_, invalidOwnerInsertErr := store1.db.ExecContext(
		ctx,
		`INSERT INTO resource_locks(resource_key, owner_pid, owner_identity_time, updated_at_unix_nano)
		 VALUES(?, ?, ?, ?)`,
		"container/invalid-owner",
		process.UnknownPID,
		timeString(now),
		unixNano(now),
	)
	require.NoError(t, invalidOwnerInsertErr)

	require.NoError(t, store1.DeleteInactiveResourceLeases(ctx))

	otherOwner, otherOwnerErr := testResourceLeaseOwner(t, -2*time.Hour)
	require.NoError(t, otherOwnerErr)

	_, activeBlockedErr := store2.AcquireResourceLease(ctx, testLeasableResource("container/active"), otherOwner, time.Minute)
	require.ErrorIs(t, activeBlockedErr, ErrResourceLeaseHeld)

	_, staleReacquireErr := store2.AcquireResourceLease(ctx, testLeasableResource("container/stale"), otherOwner, time.Minute)
	require.NoError(t, staleReacquireErr)
	_, invalidOwnerReacquireErr := store2.AcquireResourceLease(ctx, testLeasableResource("container/invalid-owner"), otherOwner, time.Minute)
	require.NoError(t, invalidOwnerReacquireErr)
}

func TestCreatePortReservationBlocksOtherStore(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)
	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, time.Second)
	require.NoError(t, owner2Err)

	_, reserveErr := store1.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26001),
		OwnerProcess: owner1,
	})
	require.NoError(t, reserveErr)

	_, blockedErr := store2.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26001),
		OwnerProcess: owner2,
	})

	require.ErrorIs(t, blockedErr, ErrPortReservationHeld)
}

func TestCreateOrUpdatePortReservationReusesSameOwner(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)
	owner, ownerErr := testResourceLeaseOwner(t, 0)
	require.NoError(t, ownerErr)

	first, firstErr := store.CreateOrUpdatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26002),
		OwnerProcess: owner,
	})
	require.NoError(t, firstErr)
	second, secondErr := store.CreateOrUpdatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26002),
		OwnerProcess: owner,
	})

	require.NoError(t, secondErr)
	require.Equal(t, first.Protocol, second.Protocol)
	require.Equal(t, first.IP, second.IP)
	require.Equal(t, first.Port, second.Port)
}

func TestReleasePortAllowsOtherOwnerToReserve(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)
	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, time.Second)
	require.NoError(t, owner2Err)

	_, reserveErr := store1.CreateOrUpdatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26005),
		OwnerProcess: owner1,
	})
	require.NoError(t, reserveErr)
	_, blockedErr := store2.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26005),
		OwnerProcess: owner2,
	})
	require.ErrorIs(t, blockedErr, ErrPortReservationHeld)

	releaseErr := store1.ReleasePort(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26005),
		OwnerProcess: owner1,
	})
	require.NoError(t, releaseErr)

	_, reserveAfterReleaseErr := store2.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26005),
		OwnerProcess: owner2,
	})
	require.NoError(t, reserveAfterReleaseErr)
}

func TestReleasePortDoesNotReleaseOtherOwnerReservation(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)
	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, time.Second)
	require.NoError(t, owner2Err)

	_, reserveErr := store1.CreateOrUpdatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26006),
		OwnerProcess: owner1,
	})
	require.NoError(t, reserveErr)
	releaseErr := store2.ReleasePort(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26006),
		OwnerProcess: owner2,
	})
	require.NoError(t, releaseErr)

	_, blockedErr := store2.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26006),
		OwnerProcess: owner2,
	})
	require.ErrorIs(t, blockedErr, ErrPortReservationHeld)
}

func TestPortReservationAllIPv4InterfacesBlocksSpecificAddress(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)
	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, time.Second)
	require.NoError(t, owner2Err)

	_, reserveErr := store1.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "0.0.0.0", 26008),
		OwnerProcess: owner1,
	})
	require.NoError(t, reserveErr)

	_, blockedErr := store2.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26008),
		OwnerProcess: owner2,
	})

	require.ErrorIs(t, blockedErr, ErrPortReservationHeld)
}

func TestPortReservationSpecificAddressBlocksAllIPv4Interfaces(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)
	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, time.Second)
	require.NoError(t, owner2Err)

	_, reserveErr := store1.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26009),
		OwnerProcess: owner1,
	})
	require.NoError(t, reserveErr)

	_, blockedErr := store2.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "0.0.0.0", 26009),
		OwnerProcess: owner2,
	})

	require.ErrorIs(t, blockedErr, ErrPortReservationHeld)
}

func TestCreateOrUpdatePortReservationSameOwnerSpecificAddressBlocksAllIPv4Interfaces(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)
	owner, ownerErr := testResourceLeaseOwner(t, 0)
	require.NoError(t, ownerErr)

	_, reserveErr := store.CreateOrUpdatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26012),
		OwnerProcess: owner,
	})
	require.NoError(t, reserveErr)

	_, blockedErr := store.CreateOrUpdatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "0.0.0.0", 26012),
		OwnerProcess: owner,
	})

	require.ErrorIs(t, blockedErr, ErrPortReservationHeld)
}

func TestCreateOrUpdatePortReservationSameOwnerAllIPv4InterfacesBlocksSpecificAddress(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)
	owner, ownerErr := testResourceLeaseOwner(t, 0)
	require.NoError(t, ownerErr)

	_, reserveErr := store.CreateOrUpdatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "0.0.0.0", 26013),
		OwnerProcess: owner,
	})
	require.NoError(t, reserveErr)

	_, blockedErr := store.CreateOrUpdatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26013),
		OwnerProcess: owner,
	})

	require.ErrorIs(t, blockedErr, ErrPortReservationHeld)
}

func TestPortReservationAllIPv6InterfacesBlocksSpecificAddress(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)
	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, time.Second)
	require.NoError(t, owner2Err)

	_, reserveErr := store1.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "::", 26010),
		OwnerProcess: owner1,
	})
	require.NoError(t, reserveErr)

	_, blockedErr := store2.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "::1", 26010),
		OwnerProcess: owner2,
	})

	require.ErrorIs(t, blockedErr, ErrPortReservationHeld)
}

func TestPortReservationIPv4WildcardDoesNotBlockIPv6(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)
	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, time.Second)
	require.NoError(t, owner2Err)

	_, reserveErr := store1.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "0.0.0.0", 26011),
		OwnerProcess: owner1,
	})
	require.NoError(t, reserveErr)

	reservation, reserveIPv6Err := store2.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "::1", 26011),
		OwnerProcess: owner2,
	})

	require.NoError(t, reserveIPv6Err)
	require.Equal(t, netip.MustParseAddr("::1"), reservation.IP)
}

func TestPortReservationStoresIPAsBlob(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)
	owner, ownerErr := testResourceLeaseOwner(t, 0)
	require.NoError(t, ownerErr)

	reservation, reserveErr := store.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "::1", 26007),
		OwnerProcess: owner,
	})
	require.NoError(t, reserveErr)

	require.Equal(t, netip.MustParseAddr("::1"), reservation.IP)
	row := store.db.QueryRowContext(ctx, `SELECT typeof(ip), length(ip) FROM port_allocations WHERE protocol = ? AND port = ?`, "TCP", 26007)
	var addressType string
	var addressLength int
	require.NoError(t, row.Scan(&addressType, &addressLength))
	require.Equal(t, "blob", addressType)
	require.Equal(t, 16, addressLength)
}

func TestDeleteInactivePortReservationsUsesOwnerProcessIdentity(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, ctx, storePath)
	store2 := openTestStore(t, ctx, storePath)
	activeOwner, activeOwnerErr := testResourceLeaseOwner(t, 0)
	require.NoError(t, activeOwnerErr)
	staleOwner, staleOwnerErr := testResourceLeaseOwner(t, time.Second)
	require.NoError(t, staleOwnerErr)
	otherOwner, otherOwnerErr := testResourceLeaseOwner(t, 2*time.Second)
	require.NoError(t, otherOwnerErr)

	_, activeReserveErr := store1.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26003),
		OwnerProcess: activeOwner,
	})
	require.NoError(t, activeReserveErr)
	_, staleReserveErr := store1.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26004),
		OwnerProcess: staleOwner,
	})
	require.NoError(t, staleReserveErr)

	require.NoError(t, store1.DeleteInactivePortReservations(ctx))

	_, activeBlockedErr := store2.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26003),
		OwnerProcess: otherOwner,
	})
	require.ErrorIs(t, activeBlockedErr, ErrPortReservationHeld)
	_, staleReacquireErr := store2.CreatePortReservation(ctx, PortReservationRequest{
		Binding:      testPortBinding("tcp", "127.0.0.1", 26004),
		OwnerProcess: otherOwner,
	})
	require.NoError(t, staleReacquireErr)
}

func TestPersistentProcessRecordRoundTrip(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)

	record := PersistentProcessRecord{
		ResourceKey:       "api",
		LifecycleKey:      "lk1",
		PID:               process.Pid_t(1234),
		IdentityTime:      time.Unix(100, 200).UTC(),
		RunID:             "1234",
		StdOutFile:        "/tmp/stdout",
		StdErrFile:        "/tmp/stderr",
		LifecycleMetadata: `{"args":["--port","5000"]}`,
	}

	require.NoError(t, store.UpsertPersistentProcess(ctx, record))

	actual, getErr := store.GetPersistentProcess(ctx, record.ResourceKey)
	require.NoError(t, getErr)
	require.Equal(t, record.ResourceKey, actual.ResourceKey)
	require.Equal(t, record.LifecycleKey, actual.LifecycleKey)
	require.Equal(t, record.PID, actual.PID)
	require.Equal(t, record.IdentityTime, actual.IdentityTime)
	require.Equal(t, record.RunID, actual.RunID)
	require.Equal(t, record.StdOutFile, actual.StdOutFile)
	require.Equal(t, record.StdErrFile, actual.StdErrFile)
	require.Equal(t, record.LifecycleMetadata, actual.LifecycleMetadata)
	require.False(t, actual.UpdatedAt.IsZero())

	require.NoError(t, actual.Delete(ctx))

	_, getErr = store.GetPersistentProcess(ctx, record.ResourceKey)
	require.True(t, errors.Is(getErr, ErrPersistentProcessNotFound), "expected ErrPersistentProcessNotFound, got %v", getErr)
}
