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
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/microsoft/dcp/pkg/process"
)

func openRawSQLiteDB(t *testing.T, path string) *sql.DB {
	t.Helper()

	db, openErr := sql.Open(sqliteDriverName, sqliteDSN(path, 500*time.Millisecond))
	require.NoError(t, openErr)

	require.NoError(t, db.PingContext(context.Background()))
	return db
}

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()

	store, openErr := Open(context.Background(), Options{
		Path:        path,
		BusyTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, openErr)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})
	return store
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

func requireMigrationVersion(t *testing.T, store *Store, expectedVersion int) {
	t.Helper()

	row := store.db.QueryRowContext(context.Background(), `SELECT version, dirty FROM schema_migrations LIMIT 1`)
	var version int
	var dirty bool
	require.NoError(t, row.Scan(&version, &dirty))
	require.Equal(t, expectedVersion, version)
	require.False(t, dirty)
}

func createUnversionedCurrentSchema(t *testing.T, path string) {
	t.Helper()

	db := openRawSQLiteDB(t, path)
	defer func() {
		require.NoError(t, db.Close())
	}()

	initialMigration, readErr := fs.ReadFile(migrationFiles, "migrations/000001_initial.up.sql")
	require.NoError(t, readErr)

	_, execErr := db.ExecContext(context.Background(), string(initialMigration))
	require.NoError(t, execErr)
}

func testResourceLeaseOwner(t *testing.T, identityOffset time.Duration) (process.ProcessTreeItem, error) {
	t.Helper()

	currentProcess, currentProcessErr := process.This()
	if currentProcessErr != nil {
		return process.ProcessTreeItem{}, currentProcessErr
	}

	currentProcess.IdentityTime = currentProcess.IdentityTime.Add(identityOffset)
	return normalizeResourceLeaseOwner(currentProcess)
}

func TestOpenCreatesSchema(t *testing.T) {
	t.Parallel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, storePath)

	require.Equal(t, storePath, store.Path())
	require.FileExists(t, storePath)

	requireMigrationVersion(t, store, currentSchemaVersion)
}

func TestOpenMigratesUnversionedCurrentSchemaWithoutLosingResourceLocks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	createUnversionedCurrentSchema(t, storePath)

	db := openRawSQLiteDB(t, storePath)
	owner, ownerErr := testResourceLeaseOwner(t, 0)
	require.NoError(t, ownerErr)

	now := time.Now().UTC()
	_, insertErr := db.ExecContext(
		ctx,
		`INSERT INTO resource_locks(resource_key, owner_pid, owner_identity_time, updated_at_unix_nano, metadata)
		 VALUES(?, ?, ?, ?, ?)`,
		"container/existing",
		owner.Pid,
		timeString(owner.IdentityTime),
		unixNano(now),
		"",
	)
	require.NoError(t, insertErr)
	require.NoError(t, db.Close())

	store := openTestStore(t, storePath)
	requireMigrationVersion(t, store, currentSchemaVersion)

	otherOwner, otherOwnerErr := testResourceLeaseOwner(t, -time.Hour)
	require.NoError(t, otherOwnerErr)
	_, acquireErr := store.AcquireResourceLease(ctx, "container/existing", otherOwner, time.Minute, "")
	require.ErrorIs(t, acquireErr, ErrResourceLeaseHeld)
}

func TestOpenMigratesUnversionedLegacyResourceLocksTable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")

	db := openRawSQLiteDB(t, storePath)
	_, createErr := db.ExecContext(
		ctx,
		`CREATE TABLE resource_locks (
			resource_key TEXT PRIMARY KEY,
			owner_instance_id TEXT NOT NULL,
			updated_at_unix_nano INTEGER NOT NULL,
			metadata TEXT NOT NULL DEFAULT ''
		)`,
	)
	require.NoError(t, createErr)
	require.NoError(t, db.Close())

	store := openTestStore(t, storePath)
	requireMigrationVersion(t, store, currentSchemaVersion)

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

	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, storePath)
	store2 := openTestStore(t, storePath)

	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, -time.Hour)
	require.NoError(t, owner2Err)

	lease, acquireErr := store1.AcquireResourceLease(ctx, "container/test", owner1, time.Minute, "metadata")
	require.NoError(t, acquireErr)
	require.Equal(t, owner1, lease.OwnerProcess)

	_, blockedErr := store2.AcquireResourceLease(ctx, "container/test", owner2, time.Minute, "")
	require.ErrorIs(t, blockedErr, ErrResourceLeaseHeld)

	renewed, renewErr := store1.RenewResourceLease(ctx, "container/test", owner1)
	require.NoError(t, renewErr)
	require.Equal(t, owner1, renewed.OwnerProcess)

	require.NoError(t, store1.ReleaseResourceLease(ctx, "container/test", owner1))

	lease, acquireErr = store2.AcquireResourceLease(ctx, "container/test", owner2, time.Minute, "")
	require.NoError(t, acquireErr)
	require.Equal(t, owner2, lease.OwnerProcess)
}

func TestResourceLeasePreservesOutOfUnixNanoRangeOwnerIdentityTime(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, storePath)

	owner, ownerErr := normalizeResourceLeaseOwner(process.ProcessTreeItem{
		Pid:          process.Pid_t(1234),
		IdentityTime: time.Date(1, time.January, 1, 0, 3, 10, 720000000, time.UTC),
	})
	require.NoError(t, ownerErr)

	lease, acquireErr := store.AcquireResourceLease(ctx, "container/test", owner, time.Minute, "")
	require.NoError(t, acquireErr)
	require.Equal(t, owner, lease.OwnerProcess)

	renewed, renewErr := store.RenewResourceLease(ctx, "container/test", owner)
	require.NoError(t, renewErr)
	require.Equal(t, owner, renewed.OwnerProcess)
}

func TestResourceLeaseDoesNotExpireWhileOwnerIsActive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, storePath)
	store2 := openTestStore(t, storePath)

	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, -time.Hour)
	require.NoError(t, owner2Err)

	_, acquireErr := store1.AcquireResourceLease(ctx, "container/test", owner1, 20*time.Millisecond, "")
	require.NoError(t, acquireErr)

	require.Eventually(t, func() bool {
		_, retryErr := store2.AcquireResourceLease(ctx, "container/test", owner2, 20*time.Millisecond, "")
		return errors.Is(retryErr, ErrResourceLeaseHeld)
	}, time.Second, 20*time.Millisecond)
}

func TestResourceLeaseCanBeAcquiredFromInactiveOwnerAfterRevalidationInterval(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, storePath)
	store2 := openTestStore(t, storePath)

	currentProcess, currentProcessErr := process.This()
	require.NoError(t, currentProcessErr)
	staleOwner, staleOwnerErr := normalizeResourceLeaseOwner(process.ProcessTreeItem{
		Pid:          currentProcess.Pid,
		IdentityTime: currentProcess.IdentityTime.Add(-time.Hour),
	})
	require.NoError(t, staleOwnerErr)
	activeOwner, activeOwnerErr := normalizeResourceLeaseOwner(currentProcess)
	require.NoError(t, activeOwnerErr)

	_, acquireErr := store1.AcquireResourceLease(ctx, "container/test", staleOwner, 20*time.Millisecond, "")
	require.NoError(t, acquireErr)

	require.Eventually(t, func() bool {
		_, retryErr := store2.AcquireResourceLease(ctx, "container/test", activeOwner, 20*time.Millisecond, "")
		return retryErr == nil
	}, time.Second, 20*time.Millisecond)
}

func TestWithResourceLeaseDoesNotRetryHeldLease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, storePath)
	store2 := openTestStore(t, storePath)

	owner1, owner1Err := testResourceLeaseOwner(t, 0)
	require.NoError(t, owner1Err)
	owner2, owner2Err := testResourceLeaseOwner(t, -time.Hour)
	require.NoError(t, owner2Err)

	_, acquireErr := store1.AcquireResourceLease(ctx, "container/test", owner1, time.Minute, "")
	require.NoError(t, acquireErr)

	retryCtx, retryCtxCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer retryCtxCancel()

	callbackCalled := false
	leaseErr := store2.WithResourceLease(retryCtx, "container/test", owner2, time.Minute, "", func(context.Context, *ResourceLease) error {
		callbackCalled = true
		return nil
	})

	require.ErrorIs(t, leaseErr, ErrResourceLeaseHeld)
	require.NotErrorIs(t, leaseErr, context.DeadlineExceeded)
	require.False(t, callbackCalled)
}

func TestDeleteInactiveResourceLeasesUsesOwnerProcessIdentity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store1 := openTestStore(t, storePath)
	store2 := openTestStore(t, storePath)

	currentProcess, currentProcessErr := process.This()
	require.NoError(t, currentProcessErr)
	activeOwner, activeOwnerErr := normalizeResourceLeaseOwner(currentProcess)
	require.NoError(t, activeOwnerErr)
	staleOwner, staleOwnerErr := normalizeResourceLeaseOwner(process.ProcessTreeItem{
		Pid:          currentProcess.Pid,
		IdentityTime: currentProcess.IdentityTime.Add(-time.Hour),
	})
	require.NoError(t, staleOwnerErr)

	now := time.Now().UTC()
	_, activeAcquireErr := store1.AcquireResourceLease(ctx, "container/active", activeOwner, time.Minute, "")
	require.NoError(t, activeAcquireErr)
	_, staleAcquireErr := store1.AcquireResourceLease(ctx, "container/stale", staleOwner, time.Minute, "")
	require.NoError(t, staleAcquireErr)
	_, invalidOwnerInsertErr := store1.db.ExecContext(
		ctx,
		`INSERT INTO resource_locks(resource_key, owner_pid, owner_identity_time, updated_at_unix_nano, metadata)
		 VALUES(?, ?, ?, ?, ?)`,
		"container/invalid-owner",
		process.UnknownPID,
		timeString(now),
		unixNano(now),
		"",
	)
	require.NoError(t, invalidOwnerInsertErr)

	require.NoError(t, store1.DeleteInactiveResourceLeases(ctx))

	otherOwner, otherOwnerErr := testResourceLeaseOwner(t, -2*time.Hour)
	require.NoError(t, otherOwnerErr)

	_, activeBlockedErr := store2.AcquireResourceLease(ctx, "container/active", otherOwner, time.Minute, "")
	require.ErrorIs(t, activeBlockedErr, ErrResourceLeaseHeld)

	_, staleReacquireErr := store2.AcquireResourceLease(ctx, "container/stale", otherOwner, time.Minute, "")
	require.NoError(t, staleReacquireErr)
	_, invalidOwnerReacquireErr := store2.AcquireResourceLease(ctx, "container/invalid-owner", otherOwner, time.Minute, "")
	require.NoError(t, invalidOwnerReacquireErr)
}

func TestPersistentProcessRecordRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, storePath)

	record := PersistentProcessRecord{
		ResourceKey:       ResourceKey(ResourceKindExecutable, types.NamespacedName{Name: "api"}),
		Name:              types.NamespacedName{Name: "api"},
		UID:               types.UID("uid1"),
		LifecycleKey:      "lk1",
		PID:               process.Pid_t(1234),
		IdentityTime:      time.Unix(100, 200).UTC(),
		DisplayStartTime:  time.Unix(101, 300).UTC(),
		RunID:             "1234",
		StdOutFile:        "/tmp/stdout",
		StdErrFile:        "/tmp/stderr",
		ExecutionType:     "Process",
		LifecycleMetadata: `{"args":["--port","5000"]}`,
	}

	require.NoError(t, store.UpsertPersistentProcess(ctx, record))

	actual, getErr := store.GetPersistentProcess(ctx, record.ResourceKey)
	require.NoError(t, getErr)
	require.Equal(t, record.ResourceKey, actual.ResourceKey)
	require.Equal(t, record.Name, actual.Name)
	require.Equal(t, record.UID, actual.UID)
	require.Equal(t, record.LifecycleKey, actual.LifecycleKey)
	require.Equal(t, record.PID, actual.PID)
	require.Equal(t, record.IdentityTime, actual.IdentityTime)
	require.Equal(t, record.DisplayStartTime, actual.DisplayStartTime)
	require.Equal(t, record.RunID, actual.RunID)
	require.Equal(t, record.StdOutFile, actual.StdOutFile)
	require.Equal(t, record.StdErrFile, actual.StdErrFile)
	require.Equal(t, record.ExecutionType, actual.ExecutionType)
	require.Equal(t, record.LifecycleMetadata, actual.LifecycleMetadata)
	require.False(t, actual.UpdatedAt.IsZero())

	require.NoError(t, store.DeletePersistentProcess(ctx, record.ResourceKey))

	_, getErr = store.GetPersistentProcess(ctx, record.ResourceKey)
	require.True(t, errors.Is(getErr, ErrPersistentProcessNotFound), "expected ErrPersistentProcessNotFound, got %v", getErr)
}
