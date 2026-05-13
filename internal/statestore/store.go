/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package statestore provides the local, SQLite-backed DCP state store.
package statestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/microsoft/dcp/internal/dcppaths"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

const (
	DCP_STATE_STORE_PATH = "DCP_STATE_STORE_PATH"

	sqliteDriverName = "sqlite"

	defaultStoreFileName         = "state.sqlite3"
	defaultElevatedStoreFileName = "state.elevated.sqlite3"

	DefaultBusyTimeout = 5 * time.Second

	// DCP's state store does small, frequent writes with concurrent readers. A 256-page threshold
	// keeps the WAL bounded to about 1 MiB with SQLite's default 4 KiB page size without
	// checkpointing on every small transaction.
	defaultWALAutoCheckpointPages = 256
)

var (
	ErrStoreNotInitialized = errors.New("state store is not initialized")
	ErrInvalidArgument     = errors.New("invalid state store argument")
)

type Options struct {
	// Path is the SQLite database file path. If empty, the default DCP user store path is used.
	Path string

	// BusyTimeout is the SQLite busy timeout used when another process holds a database lock.
	BusyTimeout time.Duration
}

type Store struct {
	db   *sql.DB
	path string
}

func DefaultPath() (string, error) {
	if stateStorePath, found := os.LookupEnv(DCP_STATE_STORE_PATH); found && strings.TrimSpace(stateStorePath) != "" {
		return strings.TrimSpace(stateStorePath), nil
	}

	dcpFolder, dcpFolderErr := dcppaths.EnsureUserDcpDir()
	if dcpFolderErr != nil {
		return "", dcpFolderErr
	}

	isAdmin, isAdminErr := osutil.IsAdmin()
	if isAdminErr != nil {
		return "", isAdminErr
	}

	if isAdmin {
		return filepath.Join(dcpFolder, defaultElevatedStoreFileName), nil
	}
	return filepath.Join(dcpFolder, defaultStoreFileName), nil
}

func Open(ctx context.Context, options Options) (*Store, error) {
	storePath := strings.TrimSpace(options.Path)
	if storePath == "" {
		defaultPath, defaultPathErr := DefaultPath()
		if defaultPathErr != nil {
			return nil, fmt.Errorf("could not determine default state store path: %w", defaultPathErr)
		}
		storePath = defaultPath
	}

	absPath, absPathErr := filepath.Abs(storePath)
	if absPathErr != nil {
		return nil, fmt.Errorf("could not determine absolute state store path for '%s': %w", storePath, absPathErr)
	}

	if ensureErr := ensureStateStoreDir(absPath); ensureErr != nil {
		return nil, ensureErr
	}

	busyTimeout := options.BusyTimeout
	if busyTimeout <= 0 {
		busyTimeout = DefaultBusyTimeout
	}

	db, openErr := openSQLiteDB(ctx, absPath, busyTimeout)
	if openErr != nil {
		return nil, fmt.Errorf("could not initialize state store database '%s': %w", absPath, openErr)
	}

	store := &Store{
		db:   db,
		path: absPath,
	}

	if configureErr := store.configure(ctx, busyTimeout); configureErr != nil {
		closeErr := db.Close()
		return nil, fmt.Errorf("could not configure state store database '%s': %w", absPath, errors.Join(configureErr, closeErr))
	}

	migrationDB, migrationOpenErr := openSQLiteDB(ctx, absPath, busyTimeout)
	if migrationOpenErr != nil {
		closeErr := db.Close()
		return nil, fmt.Errorf("could not initialize state store migration database '%s': %w", absPath, errors.Join(migrationOpenErr, closeErr))
	}
	if migrateErr := store.migrate(ctx, migrationDB, busyTimeout); migrateErr != nil {
		migrationCloseErr := migrationDB.Close()
		closeErr := db.Close()
		return nil, fmt.Errorf("could not migrate state store database '%s': %w", absPath, errors.Join(migrateErr, migrationCloseErr, closeErr))
	}
	if migrationCloseErr := migrationDB.Close(); migrationCloseErr != nil {
		closeErr := db.Close()
		return nil, fmt.Errorf("could not close state store migration database '%s': %w", absPath, errors.Join(migrationCloseErr, closeErr))
	}

	return store, nil
}

func openSQLiteDB(ctx context.Context, path string, busyTimeout time.Duration) (*sql.DB, error) {
	db, openErr := sql.Open(sqliteDriverName, sqliteDSN(path, busyTimeout))
	if openErr != nil {
		return nil, openErr
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if pingErr := db.PingContext(ctx); pingErr != nil {
		closeErr := db.Close()
		return nil, errors.Join(pingErr, closeErr)
	}

	return db, nil
}

func sqliteDSN(path string, busyTimeout time.Duration) string {
	sqlitePath := strings.ReplaceAll(path, "\\", "/")
	dsn := url.URL{Scheme: "file"}
	if isWindowsDrivePath(sqlitePath) {
		dsn.Opaque = sqlitePath
	} else {
		dsn.Path = sqlitePath
	}
	query := dsn.Query()
	query.Add("_pragma", fmt.Sprintf("busy_timeout=%d", busyTimeout.Milliseconds()))
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func isWindowsDrivePath(path string) bool {
	if len(path) < 3 || path[1] != ':' || path[2] != '/' {
		return false
	}
	driveLetter := path[0]
	return (driveLetter >= 'A' && driveLetter <= 'Z') || (driveLetter >= 'a' && driveLetter <= 'z')
}

func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}

	closeErr := s.db.Close()
	s.db = nil
	return closeErr
}

func (s *Store) requireDB() (*sql.DB, error) {
	if s == nil || s.db == nil {
		return nil, ErrStoreNotInitialized
	}
	return s.db, nil
}

func (s *Store) configure(ctx context.Context, busyTimeout time.Duration) error {
	db, dbErr := s.requireDB()
	if dbErr != nil {
		return dbErr
	}

	busyTimeoutMilliseconds := busyTimeout.Milliseconds()
	if busyTimeoutMilliseconds <= 0 {
		busyTimeoutMilliseconds = 1
	}

	if _, busyErr := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMilliseconds)); busyErr != nil {
		return fmt.Errorf("could not configure SQLite busy timeout: %w", busyErr)
	}

	var journalMode string
	journalRow := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL")
	if journalErr := journalRow.Scan(&journalMode); journalErr != nil {
		return fmt.Errorf("could not enable SQLite WAL mode: %w", journalErr)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("could not enable SQLite WAL mode: got journal mode %q", journalMode)
	}

	if _, syncErr := db.ExecContext(ctx, "PRAGMA synchronous = NORMAL"); syncErr != nil {
		return fmt.Errorf("could not configure SQLite synchronous mode: %w", syncErr)
	}

	var autoCheckpointPages int
	autoCheckpointRow := db.QueryRowContext(ctx, fmt.Sprintf("PRAGMA wal_autocheckpoint = %d", defaultWALAutoCheckpointPages))
	if autoCheckpointErr := autoCheckpointRow.Scan(&autoCheckpointPages); autoCheckpointErr != nil {
		return fmt.Errorf("could not configure SQLite WAL auto-checkpoint threshold: %w", autoCheckpointErr)
	}
	if autoCheckpointPages != defaultWALAutoCheckpointPages {
		return fmt.Errorf("could not configure SQLite WAL auto-checkpoint threshold: got %d pages", autoCheckpointPages)
	}

	if _, foreignKeyErr := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); foreignKeyErr != nil {
		return fmt.Errorf("could not enable SQLite foreign key checks: %w", foreignKeyErr)
	}

	return nil
}

func ensureStateStoreDir(path string) error {
	parentDir := filepath.Dir(path)
	if ensureErr := usvc_io.EnsureRestrictedDirectory(parentDir, osutil.PermissionOnlyOwnerReadWriteTraverse); ensureErr != nil {
		return fmt.Errorf("could not prepare state store directory '%s': %w", parentDir, ensureErr)
	}
	return nil
}

func (s *Store) withImmediateTx(ctx context.Context, f func(*sql.Conn) error) (err error) {
	db, dbErr := s.requireDB()
	if dbErr != nil {
		return dbErr
	}

	conn, connErr := db.Conn(ctx)
	if connErr != nil {
		return fmt.Errorf("could not get state store connection: %w", connErr)
	}

	begun := false
	committed := false
	defer func() {
		if begun && !committed {
			_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
			err = errors.Join(err, rollbackErr)
		}
		closeErr := conn.Close()
		err = errors.Join(err, closeErr)
	}()

	if _, beginErr := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); beginErr != nil {
		return fmt.Errorf("could not start state store transaction: %w", beginErr)
	}
	begun = true

	if fErr := f(conn); fErr != nil {
		return fErr
	}

	if _, commitErr := conn.ExecContext(ctx, "COMMIT"); commitErr != nil {
		return fmt.Errorf("could not commit state store transaction: %w", commitErr)
	}
	committed = true
	return nil
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}

func timeFromUnixNano(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v).UTC()
}

func timeString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func timeFromString(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	parsed, parseErr := time.Parse(time.RFC3339Nano, v)
	if parseErr != nil {
		return time.Time{}, parseErr
	}
	return parsed.UTC(), nil
}
