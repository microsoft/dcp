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
)

const (
	currentSchemaMajorVersion        = 1
	currentSchemaMinorVersion        = 1
	legacySchemaVersion2             = 2
	legacySchemaVersion2MajorVersion = 1
	legacySchemaVersion2MinorVersion = 1

	schemaMajorMigrationTableName = "schema_migrations"
	migrationLockFileSuffix       = ".migrate.lock"
)

type schemaMajorMigration struct {
	version            int
	path               string
	minorTableName     string
	latestMinorVersion int
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
	}
	schemaMajorMigrations = []schemaMajorMigration{schemaMajorVersion1Migration}
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

// readSchemaMajorVersion reads the current major migration version from schema_migrations.
func readSchemaMajorVersion(ctx context.Context, queryer schemaVersionQueryer) (int, bool, bool, error) {
	tableRow := queryer.QueryRowContext(
		ctx,
		`SELECT EXISTS(
			SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?
		)`,
		schemaMajorMigrationTableName,
	)
	var tableExists bool
	if scanErr := tableRow.Scan(&tableExists); scanErr != nil {
		return 0, false, false, fmt.Errorf("could not inspect schema major migration table: %w", scanErr)
	}
	if !tableExists {
		return 0, false, false, nil
	}

	versionQuery := fmt.Sprintf(
		`SELECT version, dirty FROM %s LIMIT 1`,
		quoteSQLiteIdentifier(schemaMajorMigrationTableName),
	)
	versionRow := queryer.QueryRowContext(ctx, versionQuery)
	var version int
	var dirty bool
	if scanErr := versionRow.Scan(&version, &dirty); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return 0, false, false, nil
		}
		return 0, false, false, fmt.Errorf("could not read schema major version: %w", scanErr)
	}
	return version, true, dirty, nil
}

// validateStoredSchemaMajorVersion rejects dirty major versions and versions with no supported migration path.
func (s *Store) validateStoredSchemaMajorVersion(ctx context.Context) error {
	version, found, dirty, readErr := readSchemaMajorVersion(ctx, s.db)
	if readErr != nil {
		return readErr
	}
	if !found {
		return nil
	}
	if dirty {
		return fmt.Errorf("schema major version %d is dirty", version)
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

// runSchemaMinorMigrations accepts a clean newer version without asking golang-migrate to resolve unknown files.
func (s *Store) runSchemaMinorMigrations(
	ctx context.Context,
	busyTimeout time.Duration,
	migration schemaMajorMigration,
) error {
	migrationRunner, runnerErr := s.newMigrationRunner(
		ctx,
		busyTimeout,
		migration.minorPath(),
		migration.minorTableName,
	)
	if runnerErr != nil {
		return runnerErr
	}

	databaseVersion, dirty, versionErr := migrationRunner.Version()
	if versionErr != nil && !errors.Is(versionErr, gomigrate.ErrNilVersion) {
		sourceCloseErr, databaseCloseErr := migrationRunner.Close()
		return errors.Join(
			fmt.Errorf("could not read schema major %d minor version: %w", migration.version, versionErr),
			sourceCloseErr,
			databaseCloseErr,
		)
	}
	if dirty {
		sourceCloseErr, databaseCloseErr := migrationRunner.Close()
		return errors.Join(
			fmt.Errorf("schema major %d minor version %d is dirty", migration.version, databaseVersion),
			sourceCloseErr,
			databaseCloseErr,
		)
	}
	// A newer compatible migration implies that every older migration in this stream was applied.
	if versionErr == nil && int(databaseVersion) > migration.latestMinorVersion {
		sourceCloseErr, databaseCloseErr := migrationRunner.Close()
		return errors.Join(sourceCloseErr, databaseCloseErr)
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
