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

	"github.com/microsoft/dcp/pkg/commonapi"
)

var ErrPersistentVolumeNotFound = errors.New("persistent volume record not found")

type PersistentVolumeRecord struct {
	ResourceKey string
	VolumeName  string
	RuntimeName string
	WorkloadID  commonapi.WorkloadID
	UpdatedAt   time.Time
	store       *Store
}

func (r *PersistentVolumeRecord) Delete(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("%w: persistent volume record cannot be nil", ErrInvalidArgument)
	}
	if r.store == nil {
		return fmt.Errorf("%w: persistent volume record store cannot be nil", ErrInvalidArgument)
	}

	return r.store.DeletePersistentVolume(ctx, r.ResourceKey)
}

func (s *Store) UpsertPersistentVolume(ctx context.Context, record PersistentVolumeRecord) error {
	record.ResourceKey = strings.TrimSpace(record.ResourceKey)
	record.VolumeName = strings.TrimSpace(record.VolumeName)
	record.RuntimeName = strings.TrimSpace(record.RuntimeName)
	normalizedWorkloadID, workloadIDErr := normalizePersistentWorkloadID(record.WorkloadID)
	if workloadIDErr != nil {
		return workloadIDErr
	}
	record.WorkloadID = normalizedWorkloadID
	if record.ResourceKey == "" {
		return fmt.Errorf("%w: persistent volume resource key cannot be empty", ErrInvalidArgument)
	}
	if record.VolumeName == "" {
		return fmt.Errorf("%w: persistent volume name cannot be empty", ErrInvalidArgument)
	}
	if record.RuntimeName == "" {
		return fmt.Errorf("%w: persistent volume runtime name cannot be empty", ErrInvalidArgument)
	}
	if record.WorkloadID == "" {
		return fmt.Errorf("%w: persistent volume workload ID cannot be empty", ErrInvalidArgument)
	}

	now := time.Now().UTC()
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(
			ctx,
			`INSERT INTO persistent_volumes(resource_key, volume_name, runtime_name, workload_id, updated_at_unix_nano)
			 VALUES(?, ?, ?, ?, ?)
			 ON CONFLICT(resource_key) DO UPDATE SET
				volume_name = excluded.volume_name,
				runtime_name = excluded.runtime_name,
				workload_id = excluded.workload_id,
				updated_at_unix_nano = excluded.updated_at_unix_nano`,
			record.ResourceKey,
			record.VolumeName,
			record.RuntimeName,
			string(record.WorkloadID),
			unixNano(now),
		)
		if execErr != nil {
			return fmt.Errorf("could not upsert persistent volume record '%s': %w", record.ResourceKey, execErr)
		}
		return nil
	})
}

func (s *Store) GetPersistentVolume(ctx context.Context, resourceKey string) (*PersistentVolumeRecord, error) {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return nil, fmt.Errorf("%w: persistent volume resource key cannot be empty", ErrInvalidArgument)
	}

	db, dbErr := s.requireDB()
	if dbErr != nil {
		return nil, dbErr
	}

	row := db.QueryRowContext(
		ctx,
		`SELECT resource_key, volume_name, runtime_name, workload_id, updated_at_unix_nano
		 FROM persistent_volumes
		 WHERE resource_key = ?`,
		resourceKey,
	)

	record, scanErr := scanPersistentVolume(row)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrPersistentVolumeNotFound, resourceKey)
	}
	if scanErr != nil {
		return nil, fmt.Errorf("could not get persistent volume record '%s': %w", resourceKey, scanErr)
	}
	record.store = s

	return record, nil
}

func (s *Store) ListPersistentVolumesByWorkloadID(ctx context.Context, workloadID commonapi.WorkloadID) ([]PersistentVolumeRecord, error) {
	normalizedWorkloadID, workloadIDErr := normalizePersistentWorkloadID(workloadID)
	if workloadIDErr != nil {
		return nil, workloadIDErr
	}
	workloadID = normalizedWorkloadID
	if workloadID == "" {
		return nil, fmt.Errorf("%w: persistent volume workload ID cannot be empty", ErrInvalidArgument)
	}

	db, dbErr := s.requireDB()
	if dbErr != nil {
		return nil, dbErr
	}

	rows, queryErr := db.QueryContext(
		ctx,
		`SELECT resource_key, volume_name, runtime_name, workload_id, updated_at_unix_nano
		 FROM persistent_volumes
		 WHERE workload_id = ?
		 ORDER BY resource_key`,
		string(workloadID),
	)
	if queryErr != nil {
		return nil, fmt.Errorf("could not list persistent volume records for workload '%s': %w", workloadID, queryErr)
	}
	defer func() {
		_ = rows.Close()
	}()

	records := []PersistentVolumeRecord{}
	for rows.Next() {
		record, scanErr := scanPersistentVolume(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read persistent volume record: %w", scanErr)
		}
		record.store = s
		records = append(records, *record)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("could not list persistent volume records for workload '%s': %w", workloadID, rowsErr)
	}

	return records, nil
}

func (s *Store) DeletePersistentVolume(ctx context.Context, resourceKey string) error {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return fmt.Errorf("%w: persistent volume resource key cannot be empty", ErrInvalidArgument)
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(ctx, `DELETE FROM persistent_volumes WHERE resource_key = ?`, resourceKey)
		if execErr != nil {
			return fmt.Errorf("could not delete persistent volume record '%s': %w", resourceKey, execErr)
		}
		return nil
	})
}

type persistentVolumeScanner interface {
	Scan(dest ...any) error
}

func scanPersistentVolume(row persistentVolumeScanner) (*PersistentVolumeRecord, error) {
	var record PersistentVolumeRecord
	var updatedAtUnixNano int64
	scanErr := row.Scan(
		&record.ResourceKey,
		&record.VolumeName,
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
