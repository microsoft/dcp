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

	"k8s.io/apimachinery/pkg/types"

	"github.com/microsoft/dcp/pkg/process"
)

var ErrPersistentProcessNotFound = errors.New("persistent process record not found")

type PersistentProcessRecord struct {
	ResourceKey       string
	Name              types.NamespacedName
	UID               types.UID
	LifecycleKey      string
	PID               process.Pid_t
	IdentityTime      time.Time
	DisplayStartTime  time.Time
	RunID             string
	StdOutFile        string
	StdErrFile        string
	ExecutionType     string
	LifecycleMetadata string
	UpdatedAt         time.Time
}

func (s *Store) UpsertPersistentProcess(ctx context.Context, record PersistentProcessRecord) error {
	record.ResourceKey = strings.TrimSpace(record.ResourceKey)
	if record.ResourceKey == "" {
		return fmt.Errorf("%w: persistent process resource key cannot be empty", ErrInvalidArgument)
	}
	if record.Name.Name == "" {
		return fmt.Errorf("%w: persistent process name cannot be empty", ErrInvalidArgument)
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
				resource_key, namespace, name, uid, lifecycle_key, pid,
				identity_time_unix_nano, display_start_time_unix_nano, run_id,
				stdout_file, stderr_file, execution_type, lifecycle_metadata, updated_at_unix_nano
			 )
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(resource_key) DO UPDATE SET
				namespace = excluded.namespace,
				name = excluded.name,
				uid = excluded.uid,
				lifecycle_key = excluded.lifecycle_key,
				pid = excluded.pid,
				identity_time_unix_nano = excluded.identity_time_unix_nano,
				display_start_time_unix_nano = excluded.display_start_time_unix_nano,
				run_id = excluded.run_id,
				stdout_file = excluded.stdout_file,
				stderr_file = excluded.stderr_file,
				execution_type = excluded.execution_type,
				lifecycle_metadata = excluded.lifecycle_metadata,
				updated_at_unix_nano = excluded.updated_at_unix_nano`,
			record.ResourceKey,
			record.Name.Namespace,
			record.Name.Name,
			string(record.UID),
			record.LifecycleKey,
			int64(record.PID),
			unixNano(record.IdentityTime),
			unixNano(record.DisplayStartTime),
			record.RunID,
			record.StdOutFile,
			record.StdErrFile,
			record.ExecutionType,
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
		`SELECT resource_key, namespace, name, uid, lifecycle_key, pid,
			identity_time_unix_nano, display_start_time_unix_nano, run_id,
			stdout_file, stderr_file, execution_type, lifecycle_metadata, updated_at_unix_nano
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

	return record, nil
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
	var uid string
	var pid int64
	var identityTimeUnixNano int64
	var displayStartTimeUnixNano int64
	var updatedAtUnixNano int64

	scanErr := row.Scan(
		&record.ResourceKey,
		&record.Name.Namespace,
		&record.Name.Name,
		&uid,
		&record.LifecycleKey,
		&pid,
		&identityTimeUnixNano,
		&displayStartTimeUnixNano,
		&record.RunID,
		&record.StdOutFile,
		&record.StdErrFile,
		&record.ExecutionType,
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

	record.UID = types.UID(uid)
	record.PID = pidValue
	record.IdentityTime = timeFromUnixNano(identityTimeUnixNano)
	record.DisplayStartTime = timeFromUnixNano(displayStartTimeUnixNano)
	record.UpdatedAt = timeFromUnixNano(updatedAtUnixNano)
	return &record, nil
}
