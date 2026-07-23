/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package statestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	gomigrate "github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
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

func requireSchemaMajorVersion(t *testing.T, ctx context.Context, store *Store, expectedVersion int) {
	t.Helper()

	requireSchemaMigrationVersion(t, ctx, store, schemaMajorMigrationTableName, expectedVersion)
}

func requireSchemaMinorVersion(t *testing.T, ctx context.Context, store *Store, expectedVersion int) {
	t.Helper()

	requireSchemaMigrationVersion(t, ctx, store, schemaMajorVersion1Migration.minorTableName, expectedVersion)
}

func requireSchemaMigrationVersion(
	t *testing.T,
	ctx context.Context,
	store *Store,
	tableName string,
	expectedVersion int,
) {
	t.Helper()

	query := fmt.Sprintf(`SELECT version, dirty FROM %s LIMIT 1`, quoteSQLiteIdentifier(tableName))
	row := store.db.QueryRowContext(ctx, query)
	var version int
	var dirty bool
	require.NoError(t, row.Scan(&version, &dirty))
	require.Equal(t, expectedVersion, version)
	require.False(t, dirty)
}

func resourceLocksHasColumn(ctx context.Context, conn *sql.Conn, columnName string) (bool, error) {
	rows, queryErr := conn.QueryContext(ctx, `PRAGMA table_info(resource_locks)`)
	if queryErr != nil {
		return false, queryErr
	}
	defer func() {
		_ = rows.Close()
	}()

	for rows.Next() {
		var cid int
		var name string
		var typeName string
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int
		scanErr := rows.Scan(&cid, &name, &typeName, &notNull, &defaultValue, &primaryKey)
		if scanErr != nil {
			return false, scanErr
		}
		if name == columnName {
			return true, nil
		}
	}
	return false, rows.Err()
}

func TestSchemaMigrationDefinitions(t *testing.T) {
	t.Parallel()

	require.NotEmpty(t, schemaMajorMigrations)
	embeddedMajorPaths, majorGlobErr := fs.Glob(migrationFiles, "migrations/*.up.sql")
	require.NoError(t, majorGlobErr)
	require.Len(t, embeddedMajorPaths, len(schemaMajorMigrations))

	embeddedMinorPaths, minorGlobErr := fs.Glob(migrationFiles, "migrations/*/*.up.sql")
	require.NoError(t, minorGlobErr)
	registeredMinorPathCount := 0
	previousMajorVersion := 0
	for _, migration := range schemaMajorMigrations {
		require.Greater(t, migration.version, previousMajorVersion)
		require.NotEqual(t, legacySchemaVersion2, migration.version)
		require.NotEmpty(t, migration.path)
		require.NotEmpty(t, migration.minorTableName)
		require.GreaterOrEqual(t, migration.latestMinorVersion, 0)

		expectedMajorPrefix := fmt.Sprintf("migrations/%06d_", migration.version)
		require.True(t, strings.HasPrefix(migration.path, expectedMajorPrefix))
		require.True(t, strings.HasSuffix(migration.path, ".up.sql"))
		_, majorStatErr := fs.Stat(migrationFiles, migration.path)
		require.NoError(t, majorStatErr)
		require.Contains(t, embeddedMajorPaths, migration.path)

		minorPaths, minorPathGlobErr := fs.Glob(migrationFiles, migration.minorPath()+"/*.up.sql")
		require.NoError(t, minorPathGlobErr)
		registeredMinorPathCount += len(minorPaths)
		if migration.latestMinorVersion == 0 {
			require.Empty(t, minorPaths)
		} else {
			latestMinorPattern := fmt.Sprintf("%s/%06d_*.up.sql", migration.minorPath(), migration.latestMinorVersion)
			latestMinorPaths, latestMinorGlobErr := fs.Glob(migrationFiles, latestMinorPattern)
			require.NoError(t, latestMinorGlobErr)
			require.Len(t, latestMinorPaths, 1)
		}
		previousMajorVersion = migration.version
	}
	require.Len(t, embeddedMinorPaths, registeredMinorPathCount)
}

func legacyMigrationFixture(t *testing.T, includeSchemaVersion2 bool) fstest.MapFS {
	t.Helper()

	initialMigration, initialReadErr := fs.ReadFile(migrationFiles, "migrations/000001_initial.up.sql")
	require.NoError(t, initialReadErr)
	migrationFS := fstest.MapFS{
		"migrations/000001_initial.up.sql": {Data: initialMigration},
	}
	if includeSchemaVersion2 {
		workloadMigrationPath := schemaMajorVersion1Migration.minorPath() + "/000001_workload_ids.up.sql"
		workloadMigration, workloadReadErr := fs.ReadFile(migrationFiles, workloadMigrationPath)
		require.NoError(t, workloadReadErr)
		migrationFS["migrations/000002_workload_ids.up.sql"] = &fstest.MapFile{Data: workloadMigration}
	}
	return migrationFS
}

func runLegacyMigrationRunner(
	ctx context.Context,
	path string,
	migrationFS fs.FS,
) (err error) {
	migrationDB, openErr := openSQLiteDB(ctx, path, 500*time.Millisecond)
	if openErr != nil {
		return openErr
	}

	sourceDriver, sourceErr := iofs.New(migrationFS, "migrations")
	if sourceErr != nil {
		return errors.Join(sourceErr, migrationDB.Close())
	}
	databaseDriver, databaseDriverErr := migratesqlite.WithInstance(migrationDB, &migratesqlite.Config{
		DatabaseName:    path,
		MigrationsTable: schemaMajorMigrationTableName,
	})
	if databaseDriverErr != nil {
		return errors.Join(databaseDriverErr, sourceDriver.Close(), migrationDB.Close())
	}
	migrationRunner, runnerErr := gomigrate.NewWithInstance("iofs", sourceDriver, sqliteDriverName, databaseDriver)
	if runnerErr != nil {
		return errors.Join(runnerErr, sourceDriver.Close(), databaseDriver.Close(), migrationDB.Close())
	}
	defer func() {
		sourceCloseErr, databaseCloseErr := migrationRunner.Close()
		migrationDBCloseErr := migrationDB.Close()
		err = errors.Join(err, sourceCloseErr, databaseCloseErr, migrationDBCloseErr)
	}()

	migrationErr := migrationRunner.Up()
	if errors.Is(migrationErr, gomigrate.ErrNoChange) {
		return nil
	}
	return migrationErr
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

	requireSchemaMajorVersion(t, ctx, store, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, store, currentSchemaMinorVersion)
}

func TestOpenMigratesVersionOneSchema(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	require.NoError(t, runLegacyMigrationRunner(ctx, storePath, legacyMigrationFixture(t, false)))

	store := openTestStore(t, ctx, storePath)

	requireSchemaMajorVersion(t, ctx, store, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, store, currentSchemaMinorVersion)
}

func TestCurrentSchemaRemainsCompatibleWithLegacyVersionOneRunner(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)
	requireSchemaMajorVersion(t, ctx, store, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, store, currentSchemaMinorVersion)

	legacyRunnerErr := runLegacyMigrationRunner(ctx, storePath, legacyMigrationFixture(t, false))

	require.NoError(t, legacyRunnerErr)
}

func TestOpenImportsSchemaVersion2(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	require.NoError(t, runLegacyMigrationRunner(ctx, storePath, legacyMigrationFixture(t, true)))

	schemaVersion2DB := openRawSQLiteDB(t, ctx, storePath)
	_, insertErr := schemaVersion2DB.ExecContext(
		ctx,
		`INSERT INTO persistent_processes(
			resource_key, lifecycle_key, pid, identity_time, run_id,
			stdout_file, stderr_file, lifecycle_metadata, workload_id, updated_at_unix_nano
		 ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"executable/existing",
		"lifecycle",
		123,
		timeString(time.Now().UTC()),
		"run",
		"stdout",
		"stderr",
		"metadata",
		"workload-a",
		unixNano(time.Now().UTC()),
	)
	require.NoError(t, insertErr)
	require.NoError(t, schemaVersion2DB.Close())

	store := openTestStore(t, ctx, storePath)

	requireSchemaMajorVersion(t, ctx, store, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, store, currentSchemaMinorVersion)
	row := store.db.QueryRowContext(
		ctx,
		`SELECT workload_id FROM persistent_processes WHERE resource_key = ?`,
		"executable/existing",
	)
	var workloadID string
	require.NoError(t, row.Scan(&workloadID))
	require.Equal(t, "workload-a", workloadID)
	require.NoError(t, runLegacyMigrationRunner(ctx, storePath, legacyMigrationFixture(t, false)))
}

func TestOpenIgnoresUnknownSchemaMinorMigrations(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)
	updateQuery := fmt.Sprintf(
		`UPDATE %s SET version = ?`,
		quoteSQLiteIdentifier(schemaMajorVersion1Migration.minorTableName),
	)
	_, updateErr := store.db.ExecContext(ctx, updateQuery, 999)
	require.NoError(t, updateErr)

	reopenedStore := openTestStore(t, ctx, storePath)

	requireSchemaMajorVersion(t, ctx, reopenedStore, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, reopenedStore, 999)
}

func TestOpenRejectsDirtyNewerSchemaMinorVersion(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)
	updateQuery := fmt.Sprintf(
		`UPDATE %s SET version = ?, dirty = 1`,
		quoteSQLiteIdentifier(schemaMajorVersion1Migration.minorTableName),
	)
	_, updateErr := store.db.ExecContext(ctx, updateQuery, 999)
	require.NoError(t, updateErr)

	_, openErr := Open(ctx, Options{
		Path:        storePath,
		BusyTimeout: 500 * time.Millisecond,
	})

	require.ErrorContains(t, openErr, "minor version 999 is dirty")
}

func TestOpenRejectsUnknownSchemaMajorVersion(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	require.NoError(t, runLegacyMigrationRunner(ctx, storePath, legacyMigrationFixture(t, false)))
	db := openRawSQLiteDB(t, ctx, storePath)
	_, updateErr := db.ExecContext(
		ctx,
		`UPDATE schema_migrations SET version = 3;
		 DROP TABLE resource_locks;
		 CREATE TABLE resource_locks (
			resource_key TEXT PRIMARY KEY,
			owner_instance_id TEXT NOT NULL,
			updated_at_unix_nano INTEGER NOT NULL
		 );`,
	)
	require.NoError(t, updateErr)
	require.NoError(t, db.Close())
	require.NoError(
		t,
		usvc_io.EnsureRestrictedDirectory(filepath.Dir(storePath), osutil.PermissionOnlyOwnerReadWriteTraverse),
	)

	_, openErr := Open(ctx, Options{
		Path:        storePath,
		BusyTimeout: 500 * time.Millisecond,
	})

	require.ErrorContains(t, openErr, "unsupported schema major version 3")
	db = openRawSQLiteDB(t, ctx, storePath)
	defer func() {
		require.NoError(t, db.Close())
	}()
	minorTableRow := db.QueryRowContext(
		ctx,
		`SELECT EXISTS(
			SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?
		)`,
		schemaMajorVersion1Migration.minorTableName,
	)
	var minorTableExists bool
	require.NoError(t, minorTableRow.Scan(&minorTableExists))
	require.False(t, minorTableExists)
	conn, connErr := db.Conn(ctx)
	require.NoError(t, connErr)
	defer func() {
		require.NoError(t, conn.Close())
	}()
	hasLegacyOwnerColumn, legacyOwnerColumnErr := resourceLocksHasColumn(ctx, conn, "owner_instance_id")
	require.NoError(t, legacyOwnerColumnErr)
	require.True(t, hasLegacyOwnerColumn)
}

func TestOpenRejectsDirtySchemaVersion2(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	require.NoError(t, runLegacyMigrationRunner(ctx, storePath, legacyMigrationFixture(t, true)))
	schemaVersion2DB := openRawSQLiteDB(t, ctx, storePath)
	_, updateErr := schemaVersion2DB.ExecContext(ctx, `UPDATE schema_migrations SET dirty = 1`)
	require.NoError(t, updateErr)
	require.NoError(t, schemaVersion2DB.Close())
	require.NoError(
		t,
		usvc_io.EnsureRestrictedDirectory(filepath.Dir(storePath), osutil.PermissionOnlyOwnerReadWriteTraverse),
	)

	_, openErr := Open(ctx, Options{
		Path:        storePath,
		BusyTimeout: 500 * time.Millisecond,
	})

	require.ErrorContains(t, openErr, "schema major version 2 is dirty")
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
	requireSchemaMajorVersion(t, ctx, store, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, store, currentSchemaMinorVersion)

	otherOwner, otherOwnerErr := testResourceLeaseOwner(t, -time.Hour)
	require.NoError(t, otherOwnerErr)
	_, acquireErr := store.AcquireResourceLease(ctx, testLeasableResource("container/existing"), otherOwner, time.Minute)
	require.ErrorIs(t, acquireErr, ErrResourceLeaseHeld)
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

func TestWithResourceLeaseRetryWaitsForHeldLease(t *testing.T) {
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

	resource := testLeasableResource("container/test")
	_, acquireErr := store1.AcquireResourceLease(ctx, resource, owner1, time.Minute)
	require.NoError(t, acquireErr)

	type leaseResult struct {
		callbackCalled bool
		err            error
	}
	resultCh := make(chan leaseResult, 1)
	leaseRetryInterval := 500 * time.Millisecond
	// Start acquisition on a goroutine so it has to wait on the held lease.
	go func() {
		callbackCalled := false
		leaseErr := store2.WithResourceLeaseRetry(ctx, resource, owner2, time.Minute, leaseRetryInterval, func(context.Context, *ResourceLease) error {
			callbackCalled = true
			return nil
		})
		resultCh <- leaseResult{callbackCalled: callbackCalled, err: leaseErr}
	}()

	select {
	case result := <-resultCh:
		require.FailNow(t, "lease retry finished before the held lease was released", result.err)
	default:
	}

	// Hold the lease long enough for at least one retry, then release it while the goroutine is still waiting.
	time.Sleep(2*leaseRetryInterval + 100*time.Millisecond)
	require.NoError(t, store1.ReleaseResourceLease(ctx, resource, owner1))

	var result leaseResult
	require.NoError(t, resiliency.RetryExponential(ctx, func() error {
		select {
		case result = <-resultCh:
			return nil
		default:
			return errors.New("lease retry did not finish after the held lease was released")
		}
	}))
	require.NoError(t, result.err)
	require.True(t, result.callbackCalled)
}

func TestWithResourceLeaseRetryStopsWhenContextIsCanceled(t *testing.T) {
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

	resource := testLeasableResource("container/test")
	_, acquireErr := store1.AcquireResourceLease(ctx, resource, owner1, time.Minute)
	require.NoError(t, acquireErr)

	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()

	type leaseResult struct {
		callbackCalled bool
		err            error
	}
	resultCh := make(chan leaseResult, 1)
	go func() {
		callbackCalled := false
		leaseErr := store2.WithResourceLeaseRetry(waitCtx, resource, owner2, time.Minute, 10*time.Millisecond, func(context.Context, *ResourceLease) error {
			callbackCalled = true
			return nil
		})
		resultCh <- leaseResult{callbackCalled: callbackCalled, err: leaseErr}
	}()

	select {
	case result := <-resultCh:
		require.FailNow(t, "lease retry finished before the context was canceled", result.err)
	default:
	}

	cancelWait()

	var result leaseResult
	require.NoError(t, resiliency.RetryExponential(ctx, func() error {
		select {
		case result = <-resultCh:
			return nil
		default:
			return errors.New("lease retry did not finish after the context was canceled")
		}
	}))
	require.ErrorIs(t, result.err, context.Canceled)
	require.False(t, result.callbackCalled)
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
