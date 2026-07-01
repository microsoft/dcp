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
	"time"

	gomigrate "github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/microsoft/dcp/internal/lockfile"
)

const (
	currentSchemaVersion = 2

	migrationTableName      = "schema_migrations"
	migrationLockFileSuffix = ".migrate.lock"
)

// migrationFiles embeds the SQL migration files into the DCP binary so runtime
// schema initialization does not depend on external files being present.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

func (s *Store) migrate(ctx context.Context, migrationDB *sql.DB, busyTimeout time.Duration) (err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if _, dbErr := s.requireDB(); dbErr != nil {
		return dbErr
	}
	if migrationDB == nil {
		return fmt.Errorf("%w: migration database is nil", ErrInvalidArgument)
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

	if migrationErr := s.dropPreMigrationLegacyResourceLocksTable(ctx); migrationErr != nil {
		return migrationErr
	}

	return s.runMigrations(ctx, migrationDB, busyTimeout)
}

func (s *Store) runMigrations(ctx context.Context, migrationDB *sql.DB, busyTimeout time.Duration) (err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	if migrationDB == nil {
		return fmt.Errorf("%w: migration database is nil", ErrInvalidArgument)
	}

	sourceDriver, sourceErr := iofs.New(migrationFiles, "migrations")
	if sourceErr != nil {
		return fmt.Errorf("could not load state store migrations: %w", sourceErr)
	}

	databaseDriver, databaseDriverErr := migratesqlite.WithInstance(migrationDB, &migratesqlite.Config{
		DatabaseName:    s.path,
		MigrationsTable: migrationTableName,
	})
	if databaseDriverErr != nil {
		sourceCloseErr := sourceDriver.Close()
		return fmt.Errorf("could not initialize state store migration database driver: %w", errors.Join(databaseDriverErr, sourceCloseErr))
	}

	migrationRunner, runnerErr := gomigrate.NewWithInstance("iofs", sourceDriver, sqliteDriverName, databaseDriver)
	if runnerErr != nil {
		sourceCloseErr := sourceDriver.Close()
		databaseCloseErr := databaseDriver.Close()
		return fmt.Errorf("could not initialize state store migration runner: %w", errors.Join(runnerErr, sourceCloseErr, databaseCloseErr))
	}
	defer func() {
		sourceCloseErr, databaseCloseErr := migrationRunner.Close()
		err = errors.Join(err, sourceCloseErr, databaseCloseErr)
	}()
	migrationRunner.LockTimeout = busyTimeout

	if migrationErr := migrationRunner.Up(); migrationErr != nil && !errors.Is(migrationErr, gomigrate.ErrNoChange) {
		return fmt.Errorf("could not apply state store migrations: %w", migrationErr)
	}

	return nil
}

func (s *Store) dropPreMigrationLegacyResourceLocksTable(ctx context.Context) error {
	// Early development builds created an unversioned resource_locks table with
	// string owners. Future schema changes should be expressed as numbered
	// migrations instead of adding more pre-migration repair paths.
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		return dropLegacyResourceLocksTable(ctx, conn)
	})
}

func dropLegacyResourceLocksTable(ctx context.Context, conn *sql.Conn) error {
	hasLegacyOwnerColumn, legacyOwnerColumnErr := resourceLocksHasColumn(ctx, conn, "owner_instance_id")
	if legacyOwnerColumnErr != nil {
		return legacyOwnerColumnErr
	}
	if !hasLegacyOwnerColumn {
		return nil
	}

	hasOwnerPIDColumn, ownerPIDColumnErr := resourceLocksHasColumn(ctx, conn, "owner_pid")
	if ownerPIDColumnErr != nil {
		return ownerPIDColumnErr
	}
	if hasOwnerPIDColumn {
		return nil
	}

	if _, dropErr := conn.ExecContext(ctx, `DROP TABLE resource_locks`); dropErr != nil {
		return fmt.Errorf("could not drop legacy resource_locks table: %w", dropErr)
	}
	return nil
}

func resourceLocksHasColumn(ctx context.Context, conn *sql.Conn, columnName string) (bool, error) {
	rows, queryErr := conn.QueryContext(ctx, `PRAGMA table_info(resource_locks)`)
	if queryErr != nil {
		return false, fmt.Errorf("could not inspect resource_locks schema: %w", queryErr)
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
			return false, fmt.Errorf("could not read resource_locks schema: %w", scanErr)
		}
		if name == columnName {
			return true, nil
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return false, fmt.Errorf("could not inspect resource_locks schema: %w", rowsErr)
	}

	return false, nil
}
