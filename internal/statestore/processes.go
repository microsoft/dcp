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
	"strings"
	"time"

	"github.com/microsoft/dcp/pkg/process"
)

var ErrPersistentProcessNotFound = errors.New("persistent process record not found")

type PersistentProcessRecord struct {
	ResourceKey       string
	LifecycleKey      string
	PID               process.Pid_t
	IdentityTime      time.Time
	RunID             string
	StdOutFile        string
	StdErrFile        string
	LifecycleMetadata string
	UpdatedAt         time.Time
	store             *Store
}

func (r PersistentProcessRecord) ProcessHandle() process.ProcessHandle {
	return process.NewHandle(r.PID, r.IdentityTime)
}

func (r *PersistentProcessRecord) Delete(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("%w: persistent process record cannot be nil", ErrInvalidArgument)
	}
	if r.store == nil {
		return fmt.Errorf("%w: persistent process record store cannot be nil", ErrInvalidArgument)
	}

	return r.store.DeletePersistentProcess(ctx, r.ResourceKey)
}

func (s *Store) UpsertPersistentProcess(ctx context.Context, record PersistentProcessRecord) error {
	record.ResourceKey = strings.TrimSpace(record.ResourceKey)
	if record.ResourceKey == "" {
		return fmt.Errorf("%w: persistent process resource key cannot be empty", ErrInvalidArgument)
	}
	if record.PID <= 0 {
		return fmt.Errorf("%w: persistent process PID must be positive", ErrInvalidArgument)
	}
	if record.IdentityTime.IsZero() {
		return fmt.Errorf("%w: persistent process identity time cannot be zero", ErrInvalidArgument)
	}
	if strings.TrimSpace(record.RunID) == "" {
		return fmt.Errorf("%w: persistent process run ID cannot be empty", ErrInvalidArgument)
	}

	now := time.Now().UTC()
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(
			ctx,
			`INSERT INTO persistent_processes(
				resource_key, lifecycle_key, pid,
				identity_time, run_id,
				stdout_file, stderr_file, lifecycle_metadata, updated_at_unix_nano
			 )
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(resource_key) DO UPDATE SET
				lifecycle_key = excluded.lifecycle_key,
				pid = excluded.pid,
				identity_time = excluded.identity_time,
				run_id = excluded.run_id,
				stdout_file = excluded.stdout_file,
				stderr_file = excluded.stderr_file,
				lifecycle_metadata = excluded.lifecycle_metadata,
				updated_at_unix_nano = excluded.updated_at_unix_nano`,
			record.ResourceKey,
			record.LifecycleKey,
			int64(record.PID),
			timeString(record.IdentityTime),
			record.RunID,
			record.StdOutFile,
			record.StdErrFile,
			record.LifecycleMetadata,
			unixNano(now),
		)
		if execErr != nil {
			return fmt.Errorf("could not upsert persistent process record '%s': %w", record.ResourceKey, execErr)
		}

		return nil
	})
}

func (s *Store) GetPersistentProcess(ctx context.Context, resourceKey string) (*PersistentProcessRecord, error) {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return nil, fmt.Errorf("%w: persistent process resource key cannot be empty", ErrInvalidArgument)
	}

	db, dbErr := s.requireDB()
	if dbErr != nil {
		return nil, dbErr
	}

	row := db.QueryRowContext(
		ctx,
		`SELECT resource_key, lifecycle_key, pid,
			identity_time, run_id,
			stdout_file, stderr_file, lifecycle_metadata, updated_at_unix_nano
		 FROM persistent_processes WHERE resource_key = ?`,
		resourceKey,
	)

	record, scanErr := scanPersistentProcess(row)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrPersistentProcessNotFound, resourceKey)
	}
	if scanErr != nil {
		return nil, fmt.Errorf("could not get persistent process record '%s': %w", resourceKey, scanErr)
	}
	record.store = s

	return record, nil
}

func (s *Store) ListPersistentProcesses(ctx context.Context) ([]PersistentProcessRecord, error) {
	db, dbErr := s.requireDB()
	if dbErr != nil {
		return nil, dbErr
	}

	rows, queryErr := db.QueryContext(
		ctx,
		`SELECT resource_key, lifecycle_key, pid,
			identity_time, run_id,
			stdout_file, stderr_file, lifecycle_metadata, updated_at_unix_nano
		 FROM persistent_processes
		 ORDER BY resource_key`,
	)
	if queryErr != nil {
		return nil, fmt.Errorf("could not list persistent process records: %w", queryErr)
	}
	defer func() {
		_ = rows.Close()
	}()

	records := []PersistentProcessRecord{}
	for rows.Next() {
		record, scanErr := scanPersistentProcess(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read persistent process record: %w", scanErr)
		}
		record.store = s
		records = append(records, *record)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("could not list persistent process records: %w", rowsErr)
	}

	return records, nil
}

func (s *Store) DeletePersistentProcess(ctx context.Context, resourceKey string) error {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return fmt.Errorf("%w: persistent process resource key cannot be empty", ErrInvalidArgument)
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(ctx, `DELETE FROM persistent_processes WHERE resource_key = ?`, resourceKey)
		if execErr != nil {
			return fmt.Errorf("could not delete persistent process record '%s': %w", resourceKey, execErr)
		}
		return nil
	})
}

type persistentProcessScanner interface {
	Scan(dest ...any) error
}

func scanPersistentProcess(row persistentProcessScanner) (*PersistentProcessRecord, error) {
	var record PersistentProcessRecord
	var pid int64
	var identityTime string
	var updatedAtUnixNano int64

	scanErr := row.Scan(
		&record.ResourceKey,
		&record.LifecycleKey,
		&pid,
		&identityTime,
		&record.RunID,
		&record.StdOutFile,
		&record.StdErrFile,
		&record.LifecycleMetadata,
		&updatedAtUnixNano,
	)
	if scanErr != nil {
		return nil, scanErr
	}

	pidValue, pidErr := process.Int64_ToPidT(pid)
	if pidErr != nil {
		return nil, pidErr
	}

	record.PID = pidValue
	identityTimeValue, identityTimeErr := timeFromString(identityTime)
	if identityTimeErr != nil {
		return nil, identityTimeErr
	}
	record.IdentityTime = identityTimeValue
	record.UpdatedAt = timeFromUnixNano(updatedAtUnixNano)
	return &record, nil
}
