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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-logr/logr"
	gomigrate "github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/slices"
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

func requireWorkloadIDColumn(t *testing.T, ctx context.Context, db *sql.DB, expected bool) {
	t.Helper()

	row := db.QueryRowContext(ctx, workloadIDsMigrationAppliedProbe)
	var hasColumn bool
	require.NoError(t, row.Scan(&hasColumn))
	require.Equal(t, expected, hasColumn)
}

func requirePersistentVolumesTable(t *testing.T, ctx context.Context, db *sql.DB, expected bool) {
	t.Helper()

	row := db.QueryRowContext(ctx, persistentVolumesMigrationAppliedProbe)
	var hasTable bool
	require.NoError(t, row.Scan(&hasTable))
	require.Equal(t, expected, hasTable)
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

func TestOpenAcceptsDirtyNewerSchemaMinorVersion(t *testing.T) {
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

	reopenedStore := openTestStore(t, ctx, storePath)

	// The interrupted migration belongs to a newer DCP, which is the only binary that can decide
	// whether it was applied. Everything this binary needs is present either way.
	version, found, dirty, readErr := readSchemaVersion(
		ctx,
		reopenedStore.db,
		schemaMajorVersion1Migration.minorTableName,
	)
	require.NoError(t, readErr)
	require.True(t, found)
	require.True(t, dirty)
	require.Equal(t, 999, version)
}

func TestOpenRecoversDirtySchemaMinorVersionWhenMigrationApplied(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)
	updateQuery := fmt.Sprintf(
		`UPDATE %s SET dirty = 1`,
		quoteSQLiteIdentifier(schemaMajorVersion1Migration.minorTableName),
	)
	_, updateErr := store.db.ExecContext(ctx, updateQuery)
	require.NoError(t, updateErr)

	reopenedStore := openTestStore(t, ctx, storePath)

	requireSchemaMajorVersion(t, ctx, reopenedStore, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, reopenedStore, currentSchemaMinorVersion)
	requireWorkloadIDColumn(t, ctx, reopenedStore.db, true)
}

func TestOpenRecoversDirtySchemaMinorVersionWhenMigrationNotApplied(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)
	// Undo the latest minor migration's schema changes to simulate a migration that never committed,
	// leaving only the dirty marker behind.
	_, revertErr := store.db.ExecContext(ctx, `DROP TABLE persistent_volumes`)
	require.NoError(t, revertErr)
	updateQuery := fmt.Sprintf(
		`UPDATE %s SET dirty = 1`,
		quoteSQLiteIdentifier(schemaMajorVersion1Migration.minorTableName),
	)
	_, updateErr := store.db.ExecContext(ctx, updateQuery)
	require.NoError(t, updateErr)
	requirePersistentVolumesTable(t, ctx, store.db, false)

	reopenedStore := openTestStore(t, ctx, storePath)

	requireSchemaMajorVersion(t, ctx, reopenedStore, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, reopenedStore, currentSchemaMinorVersion)
	requireWorkloadIDColumn(t, ctx, reopenedStore.db, true)
	requirePersistentVolumesTable(t, ctx, reopenedStore.db, true)
}

func TestOpenRecoversDirtyInitialSchemaMajorVersion(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	db := openRawSQLiteDB(t, ctx, storePath)
	_, createErr := db.ExecContext(
		ctx,
		`CREATE TABLE schema_migrations (version uint64, dirty bool);
		 CREATE UNIQUE INDEX version_unique ON schema_migrations (version);
		 INSERT INTO schema_migrations (version, dirty) VALUES (1, 1);`,
	)
	require.NoError(t, createErr)
	require.NoError(t, db.Close())

	store := openTestStore(t, ctx, storePath)

	requireSchemaMajorVersion(t, ctx, store, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, store, currentSchemaMinorVersion)
	requireWorkloadIDColumn(t, ctx, store.db, true)
}

func TestOpenRejectsDirtyUnknownSchemaMajorVersion(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	require.NoError(t, runLegacyMigrationRunner(ctx, storePath, legacyMigrationFixture(t, false)))
	db := openRawSQLiteDB(t, ctx, storePath)
	_, updateErr := db.ExecContext(ctx, `UPDATE schema_migrations SET version = 3, dirty = 1`)
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

	require.ErrorContains(t, openErr, "schema major version 3 is dirty")
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

func TestOpenRecoversDirtySchemaVersion2WhenMigrationApplied(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	require.NoError(t, runLegacyMigrationRunner(ctx, storePath, legacyMigrationFixture(t, true)))
	schemaVersion2DB := openRawSQLiteDB(t, ctx, storePath)
	_, insertErr := schemaVersion2DB.ExecContext(
		ctx,
		`INSERT INTO persistent_processes (
			resource_key, lifecycle_key, pid, identity_time, run_id, stdout_file, stderr_file, updated_at_unix_nano, workload_id
		 ) VALUES ('executable/existing', 'lifecycle', 1234, '', 'run', '', '', 0, 'workload-a')`,
	)
	require.NoError(t, insertErr)
	_, updateErr := schemaVersion2DB.ExecContext(ctx, `UPDATE schema_migrations SET dirty = 1`)
	require.NoError(t, updateErr)
	require.NoError(t, schemaVersion2DB.Close())
	require.NoError(
		t,
		usvc_io.EnsureRestrictedDirectory(filepath.Dir(storePath), osutil.PermissionOnlyOwnerReadWriteTraverse),
	)

	store := openTestStore(t, ctx, storePath)

	requireSchemaMajorVersion(t, ctx, store, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, store, currentSchemaMinorVersion)
	requireWorkloadIDColumn(t, ctx, store.db, true)
	row := store.db.QueryRowContext(
		ctx,
		`SELECT workload_id FROM persistent_processes WHERE resource_key = ?`,
		"executable/existing",
	)
	var workloadID string
	require.NoError(t, row.Scan(&workloadID))
	require.Equal(t, "workload-a", workloadID)
}

func TestOpenRecoversDirtySchemaVersion2WhenMigrationNotApplied(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	// Only the initial legacy migration committed, but the version marker already advanced to the
	// dirty version 2 that the interrupted migration was about to apply.
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	require.NoError(t, runLegacyMigrationRunner(ctx, storePath, legacyMigrationFixture(t, false)))
	schemaVersion1DB := openRawSQLiteDB(t, ctx, storePath)
	_, updateErr := schemaVersion1DB.ExecContext(ctx, `UPDATE schema_migrations SET version = 2, dirty = 1`)
	require.NoError(t, updateErr)
	require.NoError(t, schemaVersion1DB.Close())
	require.NoError(
		t,
		usvc_io.EnsureRestrictedDirectory(filepath.Dir(storePath), osutil.PermissionOnlyOwnerReadWriteTraverse),
	)

	store := openTestStore(t, ctx, storePath)

	requireSchemaMajorVersion(t, ctx, store, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, store, currentSchemaMinorVersion)
	requireWorkloadIDColumn(t, ctx, store.db, true)
}

// TestOpenRecoversSchemaDirtiedByOlderDcp covers how a dirty marker is produced in practice: a store
// this binary normalized to major version 1 is opened by an older DCP, whose single-stream migration
// layout re-applies the workload ID migration. That migration fails because the column already
// exists, leaving the dirty version 2 marker behind for this binary to recover.
func TestOpenRecoversSchemaDirtiedByOlderDcp(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)
	requireSchemaMajorVersion(t, ctx, store, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, store, currentSchemaMinorVersion)
	require.NoError(t, store.Close())

	// An older DCP sees major version 1 and tries to advance its own stream to version 2.
	legacyErr := runLegacyMigrationRunner(ctx, storePath, legacyMigrationFixture(t, true))
	require.ErrorContains(t, legacyErr, "duplicate column name: workload_id")

	dirtyDB := openRawSQLiteDB(t, ctx, storePath)
	row := dirtyDB.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`)
	var version int
	var dirty bool
	require.NoError(t, row.Scan(&version, &dirty))
	require.Equal(t, legacySchemaVersion2, version)
	require.True(t, dirty)
	require.NoError(t, dirtyDB.Close())
	require.NoError(
		t,
		usvc_io.EnsureRestrictedDirectory(filepath.Dir(storePath), osutil.PermissionOnlyOwnerReadWriteTraverse),
	)

	recovered := openTestStore(t, ctx, storePath)

	requireSchemaMajorVersion(t, ctx, recovered, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, recovered, currentSchemaMinorVersion)
	requireWorkloadIDColumn(t, ctx, recovered.db, true)
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

	volumeErr := store.UpsertPersistentVolume(ctx, PersistentVolumeRecord{
		ResourceKey: "containervolumes/data",
		VolumeName:  "data",
		RuntimeName: "docker",
		WorkloadID:  tooLongWorkloadID,
	})
	require.ErrorIs(t, volumeErr, ErrInvalidArgument)
	require.ErrorContains(t, volumeErr, "workload ID cannot be longer than")

	_, processListErr := store.ListPersistentProcessesByWorkloadID(ctx, tooLongWorkloadID)
	require.ErrorIs(t, processListErr, ErrInvalidArgument)
	_, containerListErr := store.ListPersistentContainersByWorkloadID(ctx, tooLongWorkloadID)
	require.ErrorIs(t, containerListErr, ErrInvalidArgument)
	_, networkListErr := store.ListPersistentNetworksByWorkloadID(ctx, tooLongWorkloadID)
	require.ErrorIs(t, networkListErr, ErrInvalidArgument)
	_, volumeListErr := store.ListPersistentVolumesByWorkloadID(ctx, tooLongWorkloadID)
	require.ErrorIs(t, volumeListErr, ErrInvalidArgument)
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

func TestPersistentVolumeRecordRoundTripByWorkloadID(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	store := openTestStore(t, ctx, storePath)

	record := PersistentVolumeRecord{
		ResourceKey: "containervolumes/data",
		VolumeName:  "data",
		RuntimeName: "docker",
		WorkloadID:  "workload-a",
	}
	require.NoError(t, store.UpsertPersistentVolume(ctx, record))

	actual, getErr := store.GetPersistentVolume(ctx, record.ResourceKey)
	require.NoError(t, getErr)
	require.Equal(t, record.ResourceKey, actual.ResourceKey)
	require.Equal(t, record.VolumeName, actual.VolumeName)
	require.Equal(t, record.RuntimeName, actual.RuntimeName)
	require.Equal(t, record.WorkloadID, actual.WorkloadID)
	require.False(t, actual.UpdatedAt.IsZero())

	records, listErr := store.ListPersistentVolumesByWorkloadID(ctx, record.WorkloadID)
	require.NoError(t, listErr)
	require.Len(t, records, 1)
	require.Equal(t, record.ResourceKey, records[0].ResourceKey)
	require.Equal(t, record.VolumeName, records[0].VolumeName)
	require.Equal(t, record.RuntimeName, records[0].RuntimeName)
	require.Equal(t, record.WorkloadID, records[0].WorkloadID)
	require.False(t, records[0].UpdatedAt.IsZero())

	require.NoError(t, records[0].Delete(ctx))

	_, getErr = store.GetPersistentVolume(ctx, record.ResourceKey)
	require.True(t, errors.Is(getErr, ErrPersistentVolumeNotFound), "expected ErrPersistentVolumeNotFound, got %v", getErr)

	records, listErr = store.ListPersistentVolumesByWorkloadID(ctx, record.WorkloadID)
	require.NoError(t, listErr)
	require.Empty(t, records)
}

// migrationVersionsInDir returns the migration versions declared by the up migration files
// directly under the given embedded directory.
func migrationVersionsInDir(t *testing.T, dir string) []int {
	t.Helper()

	entries, readErr := fs.ReadDir(migrationFiles, dir)
	require.NoError(t, readErr)

	versions := []int{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		versionText, _, found := strings.Cut(entry.Name(), "_")
		require.True(t, found, "migration file '%s' does not start with a version prefix", entry.Name())
		version, parseErr := strconv.Atoi(versionText)
		require.NoError(t, parseErr)
		versions = append(versions, version)
	}
	return versions
}

// TestEveryMigrationHasAnAppliedProbe guards recovery of interrupted migrations: without a probe
// for every migration, a dirty version marker cannot be resolved and startup fails.
func TestEveryMigrationHasAnAppliedProbe(t *testing.T) {
	t.Parallel()

	majorVersions := migrationVersionsInDir(t, "migrations")
	require.NotEmpty(t, majorVersions)
	for _, version := range majorVersions {
		require.True(
			t,
			slices.Any(schemaMajorProbes, func(probe schemaMigrationProbe) bool { return probe.version == version }),
			"schemaMajorProbes has no probe for major version %d",
			version,
		)
	}
	require.True(
		t,
		slices.Any(schemaMajorProbes, func(probe schemaMigrationProbe) bool {
			return probe.version == legacySchemaVersion2
		}),
		"schemaMajorProbes has no probe for the legacy schema version 2 marker",
	)

	for _, migration := range schemaMajorMigrations {
		minorVersions := migrationVersionsInDir(t, migration.minorPath())
		require.Equal(
			t,
			len(minorVersions),
			migration.latestMinorVersion,
			"major version %d declares latestMinorVersion %d but has %d minor migration files",
			migration.version,
			migration.latestMinorVersion,
			len(minorVersions),
		)
		for _, version := range minorVersions {
			require.True(
				t,
				slices.Any(migration.minorProbes, func(probe schemaMigrationProbe) bool {
					return probe.version == version
				}),
				"major version %d has no probe for minor version %d",
				migration.version,
				version,
			)
		}
	}
}

// newUnmigratedTestStore opens a store database without running any migration, so a test can
// drive individual migration streams to a specific version.
func newUnmigratedTestStore(t *testing.T, ctx context.Context, path string) *Store {
	t.Helper()

	require.NoError(t, usvc_io.EnsureRestrictedDirectory(filepath.Dir(path), osutil.PermissionOnlyOwnerReadWriteTraverse))
	db, openErr := openSQLiteDB(ctx, path, 500*time.Millisecond)
	require.NoError(t, openErr)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	return &Store{db: db, path: path, log: logr.Discard()}
}

// migrateStreamTo advances a single migration stream to the requested version.
// Passing noSchemaVersion leaves the stream untouched.
func migrateStreamTo(
	t *testing.T,
	ctx context.Context,
	store *Store,
	sourcePath string,
	tableName string,
	version int,
) {
	t.Helper()

	if version <= 0 {
		return
	}
	migrationRunner, runnerErr := store.newMigrationRunner(ctx, 500*time.Millisecond, sourcePath, tableName)
	require.NoError(t, runnerErr)
	migrationErr := migrationRunner.Migrate(uint(version))
	sourceCloseErr, databaseCloseErr := migrationRunner.Close()
	require.NoError(t, sourceCloseErr)
	require.NoError(t, databaseCloseErr)
	if migrationErr != nil && !errors.Is(migrationErr, gomigrate.ErrNoChange) {
		require.NoError(t, migrationErr)
	}
}

func evaluateAppliedProbe(t *testing.T, ctx context.Context, store *Store, probe schemaMigrationProbe) bool {
	t.Helper()

	var applied bool
	require.NoError(t, store.db.QueryRowContext(ctx, probe.appliedQuery).Scan(&applied))
	return applied
}

// TestAppliedProbesDetectTheirOwnMigration verifies that each probe actually distinguishes its
// migration having been applied from not. A probe that is always true (a copy of a neighboring
// migration's probe, for example) satisfies TestEveryMigrationHasAnAppliedProbe but makes
// recoverDirtySchemaVersion keep a version marker for a migration that never ran, permanently
// skipping it and corrupting the schema.
func TestAppliedProbesDetectTheirOwnMigration(t *testing.T) {
	t.Parallel()

	type probedStream struct {
		name       string
		sourcePath string
		tableName  string
		probes     []schemaMigrationProbe
		// prepare brings the database to the state this stream starts migrating from.
		prepare func(t *testing.T, ctx context.Context, store *Store)
	}

	streams := []probedStream{{
		name:       "major",
		sourcePath: "migrations",
		tableName:  schemaMajorMigrationTableName,
		probes:     schemaMajorProbes,
	}}
	for _, migration := range schemaMajorMigrations {
		streams = append(streams, probedStream{
			name:       fmt.Sprintf("major-%d-minor", migration.version),
			sourcePath: migration.minorPath(),
			tableName:  migration.minorTableName,
			probes:     migration.minorProbes,
			prepare: func(t *testing.T, ctx context.Context, store *Store) {
				migrateStreamTo(t, ctx, store, "migrations", schemaMajorMigrationTableName, migration.version)
			},
		})
	}

	for _, stream := range streams {
		for _, version := range migrationVersionsInDir(t, stream.sourcePath) {
			t.Run(fmt.Sprintf("%s-%d", stream.name, version), func(t *testing.T) {
				t.Parallel()

				ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
				defer cancel()

				probeIndex := slices.IndexFunc(stream.probes, func(probe schemaMigrationProbe) bool {
					return probe.version == version
				})
				require.GreaterOrEqual(t, probeIndex, 0, "no probe registered for %s version %d", stream.name, version)
				probe := stream.probes[probeIndex]

				store := newUnmigratedTestStore(t, ctx, filepath.Join(t.TempDir(), "state.sqlite3"))
				if stream.prepare != nil {
					stream.prepare(t, ctx, store)
				}

				migrateStreamTo(t, ctx, store, stream.sourcePath, stream.tableName, version-1)
				require.False(
					t,
					evaluateAppliedProbe(t, ctx, store, probe),
					"probe for %s version %d reports applied before the migration ran",
					stream.name,
					version,
				)

				migrateStreamTo(t, ctx, store, stream.sourcePath, stream.tableName, version)
				require.True(
					t,
					evaluateAppliedProbe(t, ctx, store, probe),
					"probe for %s version %d reports not applied after the migration ran",
					stream.name,
					version,
				)
			})
		}
	}
}

// sqlCommentPattern matches SQL line and block comments. They are removed before looking for
// transaction statements so that prose does not trip the check.
var sqlCommentPattern = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/`)

// sqlWordPattern matches a whole SQL identifier or keyword, so an identifier that merely contains a
// keyword (a "commit_sha" column, for example) is not mistaken for the keyword.
var sqlWordPattern = regexp.MustCompile(`[A-Za-z0-9_]+`)

var transactionBreakingStatements = []string{
	"BEGIN", "COMMIT", "END", "ROLLBACK", "VACUUM", "PRAGMA", "ATTACH", "DETACH",
}

// findTransactionBreakingStatement returns the first statement in migrationSQL that would end the
// transaction the migration driver wraps around each migration, or an empty string when there is
// none. SQLite treats END as an alias for COMMIT, but END also closes a CASE expression, so an END
// is only reported when no CASE is open.
func findTransactionBreakingStatement(migrationSQL string) string {
	withoutComments := sqlCommentPattern.ReplaceAllString(migrationSQL, " ")

	caseDepth := 0
	for _, word := range sqlWordPattern.FindAllString(withoutComments, -1) {
		upperWord := strings.ToUpper(word)
		switch upperWord {
		case "CASE":
			caseDepth++
		case "END":
			if caseDepth > 0 {
				caseDepth--
				continue
			}
			return upperWord
		default:
			if slices.Contains(transactionBreakingStatements, upperWord) {
				return upperWord
			}
		}
	}
	return ""
}

// TestFindTransactionBreakingStatement covers the guard that TestMigrationsAreTransactional relies
// on, including the SQL that legitimately uses the keywords it looks for.
func TestFindTransactionBreakingStatement(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		sql      string
		expected string
	}{
		{name: "plain DDL", sql: "CREATE TABLE t (a TEXT NOT NULL);", expected: ""},
		{name: "commit", sql: "UPDATE t SET a = 1;\nCOMMIT;", expected: "COMMIT"},
		{name: "begin", sql: "BEGIN;\nUPDATE t SET a = 1;", expected: "BEGIN"},
		{name: "end as commit alias", sql: "UPDATE t SET a = 1;\nEND;", expected: "END"},
		{name: "end transaction", sql: "END TRANSACTION;", expected: "END"},
		{name: "pragma", sql: "PRAGMA foreign_keys = ON;", expected: "PRAGMA"},
		{
			name:     "case expression is allowed",
			sql:      "UPDATE t SET a = CASE WHEN b IS NULL THEN 0 ELSE b END;",
			expected: "",
		},
		{
			name:     "nested case expressions are allowed",
			sql:      "UPDATE t SET a = CASE WHEN b THEN CASE WHEN c THEN 1 ELSE 2 END ELSE 3 END;",
			expected: "",
		},
		{
			name:     "end after a closed case still reports",
			sql:      "UPDATE t SET a = CASE WHEN b THEN 1 ELSE 2 END;\nEND;",
			expected: "END",
		},
		{
			name:     "keywords inside identifiers are allowed",
			sql:      "CREATE TABLE t (commit_sha TEXT, begin_time INTEGER, appended TEXT, end2 TEXT);",
			expected: "",
		},
		{name: "keywords inside a line comment are allowed", sql: "-- no COMMIT needed at the end\nSELECT 1;", expected: ""},
		{name: "keywords inside a block comment are allowed", sql: "/* do not\nVACUUM here */\nSELECT 1;", expected: ""},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, testCase.expected, findTransactionBreakingStatement(testCase.sql))
		})
	}
}

// TestMigrationsAreTransactional guards the invariant that lets an interrupted migration be recovered:
// the migration driver wraps every migration in a transaction, so a migration either commits in full
// or not at all. Statements that commit implicitly would break that invariant.
func TestMigrationsAreTransactional(t *testing.T) {
	t.Parallel()

	walkErr := fs.WalkDir(migrationFiles, "migrations", func(path string, entry fs.DirEntry, entryErr error) error {
		if entryErr != nil {
			return entryErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			return nil
		}
		contents, readErr := fs.ReadFile(migrationFiles, path)
		require.NoError(t, readErr)
		statement := findTransactionBreakingStatement(string(contents))
		require.Empty(
			t,
			statement,
			"migration '%s' must not use %s; it breaks migration atomicity",
			path,
			statement,
		)
		return nil
	})
	require.NoError(t, walkErr)
}
