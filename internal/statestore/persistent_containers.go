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
)

var ErrPersistentContainerNotFound = errors.New("persistent container record not found")

type PersistentContainerRecord struct {
	ResourceKey   string
	ContainerID   string
	ContainerName string
	RuntimeName   string
	WorkloadID    string
	UpdatedAt     time.Time
	store         *Store
}

func (r *PersistentContainerRecord) Delete(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("%w: persistent container record cannot be nil", ErrInvalidArgument)
	}
	if r.store == nil {
		return fmt.Errorf("%w: persistent container record store cannot be nil", ErrInvalidArgument)
	}

	return r.store.DeletePersistentContainer(ctx, r.ResourceKey)
}

func (s *Store) UpsertPersistentContainer(ctx context.Context, record PersistentContainerRecord) error {
	record.ResourceKey = strings.TrimSpace(record.ResourceKey)
	record.ContainerID = strings.TrimSpace(record.ContainerID)
	record.RuntimeName = strings.TrimSpace(record.RuntimeName)
	record.WorkloadID = strings.TrimSpace(record.WorkloadID)
	if record.ResourceKey == "" {
		return fmt.Errorf("%w: persistent container resource key cannot be empty", ErrInvalidArgument)
	}
	if record.ContainerID == "" {
		return fmt.Errorf("%w: persistent container ID cannot be empty", ErrInvalidArgument)
	}
	if record.RuntimeName == "" {
		return fmt.Errorf("%w: persistent container runtime name cannot be empty", ErrInvalidArgument)
	}
	if record.WorkloadID == "" {
		return fmt.Errorf("%w: persistent container workload ID cannot be empty", ErrInvalidArgument)
	}

	now := time.Now().UTC()
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(
			ctx,
			`INSERT INTO persistent_containers(resource_key, container_id, container_name, runtime_name, workload_id, updated_at_unix_nano)
			 VALUES(?, ?, ?, ?, ?, ?)
			 ON CONFLICT(resource_key) DO UPDATE SET
				container_id = excluded.container_id,
				container_name = excluded.container_name,
				runtime_name = excluded.runtime_name,
				workload_id = excluded.workload_id,
				updated_at_unix_nano = excluded.updated_at_unix_nano`,
			record.ResourceKey,
			record.ContainerID,
			record.ContainerName,
			record.RuntimeName,
			record.WorkloadID,
			unixNano(now),
		)
		if execErr != nil {
			return fmt.Errorf("could not upsert persistent container record '%s': %w", record.ResourceKey, execErr)
		}
		return nil
	})
}

func (s *Store) GetPersistentContainer(ctx context.Context, resourceKey string) (*PersistentContainerRecord, error) {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return nil, fmt.Errorf("%w: persistent container resource key cannot be empty", ErrInvalidArgument)
	}

	db, dbErr := s.requireDB()
	if dbErr != nil {
		return nil, dbErr
	}

	row := db.QueryRowContext(
		ctx,
		`SELECT resource_key, container_id, container_name, runtime_name, workload_id, updated_at_unix_nano
		 FROM persistent_containers
		 WHERE resource_key = ?`,
		resourceKey,
	)

	record, scanErr := scanPersistentContainer(row)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrPersistentContainerNotFound, resourceKey)
	}
	if scanErr != nil {
		return nil, fmt.Errorf("could not get persistent container record '%s': %w", resourceKey, scanErr)
	}
	record.store = s

	return record, nil
}

func (s *Store) ListPersistentContainersByWorkloadID(ctx context.Context, workloadID string) ([]PersistentContainerRecord, error) {
	workloadID = strings.TrimSpace(workloadID)
	if workloadID == "" {
		return nil, fmt.Errorf("%w: persistent container workload ID cannot be empty", ErrInvalidArgument)
	}

	db, dbErr := s.requireDB()
	if dbErr != nil {
		return nil, dbErr
	}

	rows, queryErr := db.QueryContext(
		ctx,
		`SELECT resource_key, container_id, container_name, runtime_name, workload_id, updated_at_unix_nano
		 FROM persistent_containers
		 WHERE workload_id = ?
		 ORDER BY resource_key`,
		workloadID,
	)
	if queryErr != nil {
		return nil, fmt.Errorf("could not list persistent container records for workload '%s': %w", workloadID, queryErr)
	}
	defer func() {
		_ = rows.Close()
	}()

	records := []PersistentContainerRecord{}
	for rows.Next() {
		record, scanErr := scanPersistentContainer(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read persistent container record: %w", scanErr)
		}
		record.store = s
		records = append(records, *record)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("could not list persistent container records for workload '%s': %w", workloadID, rowsErr)
	}

	return records, nil
}

func (s *Store) DeletePersistentContainer(ctx context.Context, resourceKey string) error {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return fmt.Errorf("%w: persistent container resource key cannot be empty", ErrInvalidArgument)
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(ctx, `DELETE FROM persistent_containers WHERE resource_key = ?`, resourceKey)
		if execErr != nil {
			return fmt.Errorf("could not delete persistent container record '%s': %w", resourceKey, execErr)
		}
		return nil
	})
}

type persistentContainerScanner interface {
	Scan(dest ...any) error
}

func scanPersistentContainer(row persistentContainerScanner) (*PersistentContainerRecord, error) {
	var record PersistentContainerRecord
	var updatedAtUnixNano int64
	scanErr := row.Scan(
		&record.ResourceKey,
		&record.ContainerID,
		&record.ContainerName,
		&record.RuntimeName,
		&record.WorkloadID,
		&updatedAtUnixNano,
	)
	if scanErr != nil {
		return nil, scanErr
	}
	record.UpdatedAt = timeFromUnixNano(updatedAtUnixNano)
	return &record, nil
}
