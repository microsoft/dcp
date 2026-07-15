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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const stateStoreTestTimeout = 10 * time.Second

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
		WorkloadID:        "workload-a",
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
	require.Equal(t, record.WorkloadID, actual.WorkloadID)
	require.False(t, actual.UpdatedAt.IsZero())

	require.NoError(t, actual.Delete(ctx))

	_, getErr = store.GetPersistentProcess(ctx, record.ResourceKey)
	require.True(t, errors.Is(getErr, ErrPersistentProcessNotFound), "expected ErrPersistentProcessNotFound, got %v", getErr)
}

func TestPersistentProcessRecordsListByWorkloadID(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)

	for _, record := range []PersistentProcessRecord{
		{
			ResourceKey:  "api",
			LifecycleKey: "api-lifecycle",
			PID:          process.Pid_t(1234),
			IdentityTime: time.Unix(100, 200).UTC(),
			RunID:        "api-run",
			WorkloadID:   " workload-a ",
		},
		{
			ResourceKey:  "worker",
			LifecycleKey: "worker-lifecycle",
			PID:          process.Pid_t(1235),
			IdentityTime: time.Unix(101, 200).UTC(),
			RunID:        "worker-run",
			WorkloadID:   "workload-b",
		},
	} {
		require.NoError(t, store.UpsertPersistentProcess(ctx, record))
	}

	records, listErr := store.ListPersistentProcessesByWorkloadID(ctx, "workload-a")
	require.NoError(t, listErr)
	require.Len(t, records, 1)
	require.Equal(t, "api", records[0].ResourceKey)
	require.Equal(t, commonapi.WorkloadID("workload-a"), records[0].WorkloadID)
}

func TestPersistentResourceWorkloadIDRejectsTooLong(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)
	tooLongWorkloadID := commonapi.WorkloadID(strings.Repeat("a", commonapi.MaxWorkloadIDLength+1))

	processErr := store.UpsertPersistentProcess(ctx, PersistentProcessRecord{
		ResourceKey:  "api",
		LifecycleKey: "api-lifecycle",
		PID:          process.Pid_t(1234),
		IdentityTime: time.Unix(100, 200).UTC(),
		RunID:        "api-run",
		WorkloadID:   tooLongWorkloadID,
	})
	require.ErrorIs(t, processErr, ErrInvalidArgument)
	require.ErrorContains(t, processErr, "workload ID cannot be longer than")

	containerErr := store.UpsertPersistentContainer(ctx, PersistentContainerRecord{
		ResourceKey: "containers/api",
		ContainerID: "container-id",
		RuntimeName: "docker",
		WorkloadID:  tooLongWorkloadID,
	})
	require.ErrorIs(t, containerErr, ErrInvalidArgument)
	require.ErrorContains(t, containerErr, "workload ID cannot be longer than")

	networkErr := store.UpsertPersistentNetwork(ctx, PersistentNetworkRecord{
		ResourceKey: "containernetworks/app-network",
		NetworkID:   "network-id",
		RuntimeName: "docker",
		WorkloadID:  tooLongWorkloadID,
	})
	require.ErrorIs(t, networkErr, ErrInvalidArgument)
	require.ErrorContains(t, networkErr, "workload ID cannot be longer than")

	_, processListErr := store.ListPersistentProcessesByWorkloadID(ctx, tooLongWorkloadID)
	require.ErrorIs(t, processListErr, ErrInvalidArgument)
	_, containerListErr := store.ListPersistentContainersByWorkloadID(ctx, tooLongWorkloadID)
	require.ErrorIs(t, containerListErr, ErrInvalidArgument)
	_, networkListErr := store.ListPersistentNetworksByWorkloadID(ctx, tooLongWorkloadID)
	require.ErrorIs(t, networkListErr, ErrInvalidArgument)
}

func TestPersistentContainerRecordRoundTripByWorkloadID(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)

	record := PersistentContainerRecord{
		ResourceKey:   "containers/api",
		ContainerID:   "container-id",
		ContainerName: "api-container",
		RuntimeName:   "docker",
		WorkloadID:    "workload-a",
	}
	require.NoError(t, store.UpsertPersistentContainer(ctx, record))

	actual, getErr := store.GetPersistentContainer(ctx, record.ResourceKey)
	require.NoError(t, getErr)
	require.Equal(t, record.ResourceKey, actual.ResourceKey)
	require.Equal(t, record.ContainerID, actual.ContainerID)
	require.Equal(t, record.ContainerName, actual.ContainerName)
	require.Equal(t, record.RuntimeName, actual.RuntimeName)
	require.Equal(t, record.WorkloadID, actual.WorkloadID)
	require.False(t, actual.UpdatedAt.IsZero())

	records, listErr := store.ListPersistentContainersByWorkloadID(ctx, record.WorkloadID)
	require.NoError(t, listErr)
	require.Len(t, records, 1)
	require.Equal(t, record.ResourceKey, records[0].ResourceKey)
	require.Equal(t, record.ContainerID, records[0].ContainerID)
	require.Equal(t, record.ContainerName, records[0].ContainerName)
	require.Equal(t, record.RuntimeName, records[0].RuntimeName)
	require.Equal(t, record.WorkloadID, records[0].WorkloadID)
	require.False(t, records[0].UpdatedAt.IsZero())

	require.NoError(t, records[0].Delete(ctx))

	_, getErr = store.GetPersistentContainer(ctx, record.ResourceKey)
	require.True(t, errors.Is(getErr, ErrPersistentContainerNotFound), "expected ErrPersistentContainerNotFound, got %v", getErr)

	records, listErr = store.ListPersistentContainersByWorkloadID(ctx, record.WorkloadID)
	require.NoError(t, listErr)
	require.Empty(t, records)
}

func TestPersistentNetworkRecordRoundTripByWorkloadID(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)

	record := PersistentNetworkRecord{
		ResourceKey: "containernetworks/app-network",
		NetworkID:   "network-id",
		NetworkName: "app-network",
		RuntimeName: "docker",
		WorkloadID:  "workload-a",
	}
	require.NoError(t, store.UpsertPersistentNetwork(ctx, record))

	actual, getErr := store.GetPersistentNetwork(ctx, record.ResourceKey)
	require.NoError(t, getErr)
	require.Equal(t, record.ResourceKey, actual.ResourceKey)
	require.Equal(t, record.NetworkID, actual.NetworkID)
	require.Equal(t, record.NetworkName, actual.NetworkName)
	require.Equal(t, record.RuntimeName, actual.RuntimeName)
	require.Equal(t, record.WorkloadID, actual.WorkloadID)
	require.False(t, actual.UpdatedAt.IsZero())

	records, listErr := store.ListPersistentNetworksByWorkloadID(ctx, record.WorkloadID)
	require.NoError(t, listErr)
	require.Len(t, records, 1)
	require.Equal(t, record.ResourceKey, records[0].ResourceKey)
	require.Equal(t, record.NetworkID, records[0].NetworkID)
	require.Equal(t, record.NetworkName, records[0].NetworkName)
	require.Equal(t, record.RuntimeName, records[0].RuntimeName)
	require.Equal(t, record.WorkloadID, records[0].WorkloadID)
	require.False(t, records[0].UpdatedAt.IsZero())

	require.NoError(t, records[0].Delete(ctx))

	_, getErr = store.GetPersistentNetwork(ctx, record.ResourceKey)
	require.True(t, errors.Is(getErr, ErrPersistentNetworkNotFound), "expected ErrPersistentNetworkNotFound, got %v", getErr)

	records, listErr = store.ListPersistentNetworksByWorkloadID(ctx, record.WorkloadID)
	require.NoError(t, listErr)
	require.Empty(t, records)
}
