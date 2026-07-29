/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package statestore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	gomigrate "github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/microsoft/dcp/internal/lockfile"
	"github.com/microsoft/dcp/pkg/slices"
)

const (
	currentSchemaMajorVersion        = 1
	currentSchemaMinorVersion        = 1
	legacySchemaVersion2             = 2
	legacySchemaVersion2MajorVersion = 1
	legacySchemaVersion2MinorVersion = 1

	schemaMajorMigrationTableName = "schema_migrations"
	migrationLockFileSuffix       = ".migrate.lock"

	// noSchemaVersion clears a golang-migrate version table, which is how the library
	// represents a stream where no migration has been applied.
	noSchemaVersion = -1

	// Applied probes for individual migrations. Each query must return a single boolean column.
	initialMigrationAppliedProbe     = `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'persistent_processes')`
	workloadIDsMigrationAppliedProbe = `SELECT EXISTS(SELECT 1 FROM pragma_table_info('persistent_processes') WHERE name = 'workload_id')`
)

// schemaMigrationProbe reports whether a single migration's schema changes are present in the database.
// golang-migrate applies each migration inside its own transaction, so a dirty version marker means
// the migration either committed in full or never committed at all. The probe tells the two apart so
// a dirty marker can be repaired instead of failing startup.
type schemaMigrationProbe struct {
	version int
	// appliedQuery must return a single boolean column that is true when the migration has been applied.
	appliedQuery string
}

type schemaMajorMigration struct {
	version            int
	path               string
	minorTableName     string
	latestMinorVersion int
	minorProbes        []schemaMigrationProbe
}

func (m schemaMajorMigration) minorPath() string {
	return strings.TrimSuffix(m.path, ".up.sql")
}

var (
	schemaMajorVersion1Migration = schemaMajorMigration{
		version:            currentSchemaMajorVersion,
		path:               "migrations/000001_initial.up.sql",
		minorTableName:     "schema_minor_migrations_v1",
		latestMinorVersion: currentSchemaMinorVersion,
		minorProbes: []schemaMigrationProbe{
			{version: 1, appliedQuery: workloadIDsMigrationAppliedProbe},
		},
	}
	schemaMajorMigrations = []schemaMajorMigration{schemaMajorVersion1Migration}

	// schemaMajorProbes covers every version that can appear in schema_migrations, including the
	// legacy version 2 marker written by the original single-stream migration layout.
	schemaMajorProbes = []schemaMigrationProbe{
		{version: currentSchemaMajorVersion, appliedQuery: initialMigrationAppliedProbe},
		{version: legacySchemaVersion2, appliedQuery: workloadIDsMigrationAppliedProbe},
	}
)

// migrationFiles embeds the SQL migration files into the DCP binary so runtime
// schema initialization does not depend on external files being present.
//
//go:embed migrations/*.sql migrations/*/*.sql
var migrationFiles embed.FS

func (s *Store) migrate(ctx context.Context, busyTimeout time.Duration) (err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if _, dbErr := s.requireDB(); dbErr != nil {
		return dbErr
	}

	migrationLock, lockErr := lockfile.NewLockfile(s.path + migrationLockFileSuffix)
	if lockErr != nil {
		return fmt.Errorf("could not create state store migration lock: %w", lockErr)
	}
	if lockErr = migrationLock.TryLock(ctx, lockfile.DefaultLockRetryInterval); lockErr != nil {
		closeErr := migrationLock.Close()
		return fmt.Errorf("could not acquire state store migration lock: %w", errors.Join(lockErr, closeErr))
	}
	defer func() {
		err = errors.Join(err, migrationLock.Close())
	}()

	// Reject unknown major versions before making any changes to their schema.
	if storedVersionErr := s.validateStoredSchemaMajorVersion(ctx); storedVersionErr != nil {
		return storedVersionErr
	}
	if transitionErr := s.importSchemaVersion2(ctx, busyTimeout); transitionErr != nil {
		return transitionErr
	}

	return s.runSchemaMigrations(ctx, busyTimeout)
}

func isSupportedSchemaMajorVersion(version int) bool {
	for _, migration := range schemaMajorMigrations {
		if version == migration.version {
			return true
		}
	}
	return false
}

type schemaVersionQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// readSchemaVersion reads the current migration version recorded in a golang-migrate version table.
func readSchemaVersion(ctx context.Context, queryer schemaVersionQueryer, tableName string) (int, bool, bool, error) {
	tableRow := queryer.QueryRowContext(
		ctx,
		`SELECT EXISTS(
			SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?
		)`,
		tableName,
	)
	var tableExists bool
	if scanErr := tableRow.Scan(&tableExists); scanErr != nil {
		return 0, false, false, fmt.Errorf("could not inspect schema migration table '%s': %w", tableName, scanErr)
	}
	if !tableExists {
		return 0, false, false, nil
	}

	versionQuery := fmt.Sprintf(
		`SELECT version, dirty FROM %s LIMIT 1`,
		quoteSQLiteIdentifier(tableName),
	)
	versionRow := queryer.QueryRowContext(ctx, versionQuery)
	var version int
	var dirty bool
	if scanErr := versionRow.Scan(&version, &dirty); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return 0, false, false, nil
		}
		return 0, false, false, fmt.Errorf("could not read schema version from table '%s': %w", tableName, scanErr)
	}
	return version, true, dirty, nil
}

// readSchemaMajorVersion reads the current major migration version from schema_migrations.
func readSchemaMajorVersion(ctx context.Context, queryer schemaVersionQueryer) (int, bool, bool, error) {
	return readSchemaVersion(ctx, queryer, schemaMajorMigrationTableName)
}

// setSchemaVersion rewrites a golang-migrate version table so it records a single clean version.
// Passing noSchemaVersion clears the table, marking the stream as having no applied migration.
func (s *Store) setSchemaVersion(ctx context.Context, tableName string, version int) error {
	quotedTable := quoteSQLiteIdentifier(tableName)
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		if _, deleteErr := conn.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s`, quotedTable)); deleteErr != nil {
			return fmt.Errorf("could not clear schema version table '%s': %w", tableName, deleteErr)
		}
		if version < 0 {
			return nil
		}
		insertQuery := fmt.Sprintf(`INSERT INTO %s (version, dirty) VALUES (?, 0)`, quotedTable)
		if _, insertErr := conn.ExecContext(ctx, insertQuery, version); insertErr != nil {
			return fmt.Errorf("could not record schema version %d in table '%s': %w", version, tableName, insertErr)
		}
		return nil
	})
}

// repairDirtySchemaVersion clears a dirty migration marker by determining whether the interrupted
// migration actually committed, and returns the repaired version (noSchemaVersion when the stream is
// left empty). It fails when no probe is registered for the dirty version, because the applied state
// of an unknown migration cannot be determined.
//
// The caller must only pass a version that the migration driver marked dirty. Migrations are applied
// in ascending order and a version is only marked dirty once the preceding one is recorded clean, so
// version-1 is known to be applied and is a valid repair target when the probe reports false.
func (s *Store) repairDirtySchemaVersion(
	ctx context.Context,
	tableName string,
	version int,
	probes []schemaMigrationProbe,
) (int, error) {
	probeIndex := slices.IndexFunc(probes, func(probe schemaMigrationProbe) bool { return probe.version == version })
	if probeIndex < 0 {
		return 0, fmt.Errorf("no applied probe is registered for dirty schema version %d", version)
	}

	var applied bool
	probeRow := s.db.QueryRowContext(ctx, probes[probeIndex].appliedQuery)
	if scanErr := probeRow.Scan(&applied); scanErr != nil {
		return 0, fmt.Errorf("could not determine whether dirty schema version %d was applied: %w", version, scanErr)
	}

	repairedVersion := version
	if !applied {
		repairedVersion = version - 1
		if repairedVersion < 1 {
			repairedVersion = noSchemaVersion
		}
	}

	if setErr := s.setSchemaVersion(ctx, tableName, repairedVersion); setErr != nil {
		return 0, fmt.Errorf("could not repair dirty schema version %d: %w", version, setErr)
	}
	s.log.Info(
		"Repaired an interrupted state store migration",
		"Table", tableName,
		"DirtyVersion", version,
		"RepairedVersion", repairedVersion,
	)
	return repairedVersion, nil
}

// validateStoredSchemaMajorVersion repairs a dirty major version and rejects versions with no supported migration path.
func (s *Store) validateStoredSchemaMajorVersion(ctx context.Context) error {
	version, found, dirty, readErr := readSchemaMajorVersion(ctx, s.db)
	if readErr != nil {
		return readErr
	}
	if !found {
		return nil
	}
	if dirty {
		repairedVersion, repairErr := s.repairDirtySchemaVersion(ctx, schemaMajorMigrationTableName, version, schemaMajorProbes)
		if repairErr != nil {
			return fmt.Errorf("schema major version %d is dirty: %w", version, repairErr)
		}
		if repairedVersion == noSchemaVersion {
			return nil
		}
		version = repairedVersion
	}
	if version != legacySchemaVersion2 && !isSupportedSchemaMajorVersion(version) {
		return fmt.Errorf("unsupported schema major version %d", version)
	}
	return nil
}

// importSchemaVersion2 transfers the already-applied workload migration into major 1's minor stream.
func (s *Store) importSchemaVersion2(ctx context.Context, busyTimeout time.Duration) error {
	version, found, dirty, readErr := readSchemaMajorVersion(ctx, s.db)
	if readErr != nil {
		return readErr
	}
	if !found || version != legacySchemaVersion2 {
		return nil
	}
	if dirty {
		return fmt.Errorf("schema major version %d is dirty", version)
	}

	migrationRunner, runnerErr := s.newMigrationRunner(
		ctx,
		busyTimeout,
		schemaMajorVersion1Migration.minorPath(),
		schemaMajorVersion1Migration.minorTableName,
	)
	if runnerErr != nil {
		return runnerErr
	}
	forceErr := migrationRunner.Force(legacySchemaVersion2MinorVersion)
	sourceCloseErr, databaseCloseErr := migrationRunner.Close()
	if importErr := errors.Join(forceErr, sourceCloseErr, databaseCloseErr); importErr != nil {
		return fmt.Errorf("could not import schema version 2 as a minor migration: %w", importErr)
	}

	updateQuery := fmt.Sprintf(
		`UPDATE %s SET version = ? WHERE version = ? AND dirty = 0`,
		quoteSQLiteIdentifier(schemaMajorMigrationTableName),
	)
	updateResult, updateErr := s.db.ExecContext(
		ctx,
		updateQuery,
		legacySchemaVersion2MajorVersion,
		legacySchemaVersion2,
	)
	if updateErr != nil {
		return fmt.Errorf("could not restore schema major version after importing schema version 2: %w", updateErr)
	}
	rowsAffected, rowsErr := updateResult.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("could not confirm restored schema major version: %w", rowsErr)
	}
	if rowsAffected != 1 {
		return fmt.Errorf("could not restore schema major version: expected one version 2 row, updated %d", rowsAffected)
	}
	return nil
}

func quoteSQLiteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

// runSchemaMigrations applies each major's compatible stream before advancing to the next major.
func (s *Store) runSchemaMigrations(ctx context.Context, busyTimeout time.Duration) error {
	version, found, dirty, readErr := readSchemaMajorVersion(ctx, s.db)
	if readErr != nil {
		return readErr
	}
	if dirty {
		return fmt.Errorf("schema major version %d is dirty", version)
	}
	if found && !isSupportedSchemaMajorVersion(version) {
		return fmt.Errorf("unsupported schema major version %d", version)
	}

	for _, migration := range schemaMajorMigrations {
		if !found || version < migration.version {
			if majorMigrationErr := s.runSchemaMajorMigration(ctx, busyTimeout, migration.version); majorMigrationErr != nil {
				return majorMigrationErr
			}
			version = migration.version
			found = true
		}
		if version == migration.version {
			if migration.latestMinorVersion == 0 {
				continue
			}
			if minorMigrationErr := s.runSchemaMinorMigrations(ctx, busyTimeout, migration); minorMigrationErr != nil {
				return minorMigrationErr
			}
		}
	}

	return nil
}

func (s *Store) runSchemaMajorMigration(ctx context.Context, busyTimeout time.Duration, targetVersion int) error {
	migrationRunner, runnerErr := s.newMigrationRunner(
		ctx,
		busyTimeout,
		"migrations",
		schemaMajorMigrationTableName,
	)
	if runnerErr != nil {
		return runnerErr
	}

	migrationErr := migrationRunner.Migrate(uint(targetVersion))
	sourceCloseErr, databaseCloseErr := migrationRunner.Close()
	if migrationErr != nil && !errors.Is(migrationErr, gomigrate.ErrNoChange) {
		return errors.Join(
			fmt.Errorf("could not migrate schema major version to %d: %w", targetVersion, migrationErr),
			sourceCloseErr,
			databaseCloseErr,
		)
	}
	return errors.Join(sourceCloseErr, databaseCloseErr)
}

// runSchemaMinorMigrations repairs an interrupted migration and accepts a clean newer version
// without asking golang-migrate to resolve unknown files.
func (s *Store) runSchemaMinorMigrations(
	ctx context.Context,
	busyTimeout time.Duration,
	migration schemaMajorMigration,
) error {
	databaseVersion, found, dirty, versionErr := readSchemaVersion(ctx, s.db, migration.minorTableName)
	if versionErr != nil {
		return fmt.Errorf("could not read schema major %d minor version: %w", migration.version, versionErr)
	}
	if dirty {
		repairedVersion, repairErr := s.repairDirtySchemaVersion(
			ctx,
			migration.minorTableName,
			databaseVersion,
			migration.minorProbes,
		)
		if repairErr != nil {
			// A dirty version this binary does not know about belongs to a newer DCP. Minor migrations
			// are additive, so every migration this binary needs is already present regardless of
			// whether the interrupted one committed. Leave the marker for the binary that owns it.
			if databaseVersion > migration.latestMinorVersion {
				s.log.Info(
					"State store has an interrupted migration from a newer DCP version. A newer DCP will repair it.",
					"Table", migration.minorTableName,
					"DirtyVersion", databaseVersion,
					"SupportedVersion", migration.latestMinorVersion,
				)
				return nil
			}
			return fmt.Errorf(
				"schema major %d minor version %d is dirty: %w",
				migration.version,
				databaseVersion,
				repairErr,
			)
		}
		databaseVersion = repairedVersion
		found = repairedVersion != noSchemaVersion
	}
	// A newer compatible migration implies that every older migration in this stream was applied.
	if found && databaseVersion > migration.latestMinorVersion {
		return nil
	}

	migrationRunner, runnerErr := s.newMigrationRunner(
		ctx,
		busyTimeout,
		migration.minorPath(),
		migration.minorTableName,
	)
	if runnerErr != nil {
		return runnerErr
	}

	migrationErr := migrationRunner.Migrate(uint(migration.latestMinorVersion))
	sourceCloseErr, databaseCloseErr := migrationRunner.Close()
	if migrationErr != nil && !errors.Is(migrationErr, gomigrate.ErrNoChange) {
		return errors.Join(
			fmt.Errorf(
				"could not migrate schema major %d minor version to %d: %w",
				migration.version,
				migration.latestMinorVersion,
				migrationErr,
			),
			sourceCloseErr,
			databaseCloseErr,
		)
	}
	return errors.Join(sourceCloseErr, databaseCloseErr)
}

// newMigrationRunner wires an embedded source to a dedicated golang-migrate version table.
func (s *Store) newMigrationRunner(
	ctx context.Context,
	busyTimeout time.Duration,
	sourcePath string,
	migrationTableName string,
) (*gomigrate.Migrate, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	migrationDB, openErr := openSQLiteDB(ctx, s.path, busyTimeout)
	if openErr != nil {
		return nil, fmt.Errorf("could not open migration database for table '%s': %w", migrationTableName, openErr)
	}
	sourceDriver, sourceErr := iofs.New(migrationFiles, sourcePath)
	if sourceErr != nil {
		return nil, fmt.Errorf("could not load migrations for table '%s': %w", migrationTableName, errors.Join(sourceErr, migrationDB.Close()))
	}
	databaseDriver, databaseDriverErr := migratesqlite.WithInstance(migrationDB, &migratesqlite.Config{
		DatabaseName:    s.path,
		MigrationsTable: migrationTableName,
	})
	if databaseDriverErr != nil {
		sourceCloseErr := sourceDriver.Close()
		migrationCloseErr := migrationDB.Close()
		return nil, fmt.Errorf(
			"could not initialize migration database driver for table '%s': %w",
			migrationTableName,
			errors.Join(databaseDriverErr, sourceCloseErr, migrationCloseErr),
		)
	}
	migrationRunner, runnerErr := gomigrate.NewWithInstance("iofs", sourceDriver, sqliteDriverName, databaseDriver)
	if runnerErr != nil {
		sourceCloseErr := sourceDriver.Close()
		databaseCloseErr := databaseDriver.Close()
		return nil, fmt.Errorf(
			"could not initialize migration runner for table '%s': %w",
			migrationTableName,
			errors.Join(runnerErr, sourceCloseErr, databaseCloseErr),
		)
	}
	migrationRunner.LockTimeout = busyTimeout
	return migrationRunner, nil
}
