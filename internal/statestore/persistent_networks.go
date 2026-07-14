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

var ErrPersistentNetworkNotFound = errors.New("persistent network record not found")

type PersistentNetworkRecord struct {
	ResourceKey string
	NetworkID   string
	NetworkName string
	RuntimeName string
	WorkloadID  commonapi.WorkloadID
	UpdatedAt   time.Time
	store       *Store
}

func (r *PersistentNetworkRecord) Delete(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("%w: persistent network record cannot be nil", ErrInvalidArgument)
	}
	if r.store == nil {
		return fmt.Errorf("%w: persistent network record store cannot be nil", ErrInvalidArgument)
	}

	return r.store.DeletePersistentNetwork(ctx, r.ResourceKey)
}

func (s *Store) UpsertPersistentNetwork(ctx context.Context, record PersistentNetworkRecord) error {
	record.ResourceKey = strings.TrimSpace(record.ResourceKey)
	record.NetworkID = strings.TrimSpace(record.NetworkID)
	record.RuntimeName = strings.TrimSpace(record.RuntimeName)
	record.WorkloadID = commonapi.WorkloadID(strings.TrimSpace(string(record.WorkloadID)))
	if record.ResourceKey == "" {
		return fmt.Errorf("%w: persistent network resource key cannot be empty", ErrInvalidArgument)
	}
	if record.NetworkID == "" {
		return fmt.Errorf("%w: persistent network ID cannot be empty", ErrInvalidArgument)
	}
	if record.RuntimeName == "" {
		return fmt.Errorf("%w: persistent network runtime name cannot be empty", ErrInvalidArgument)
	}
	if record.WorkloadID == "" {
		return fmt.Errorf("%w: persistent network workload ID cannot be empty", ErrInvalidArgument)
	}

	now := time.Now().UTC()
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(
			ctx,
			`INSERT INTO persistent_networks(resource_key, network_id, network_name, runtime_name, workload_id, updated_at_unix_nano)
			 VALUES(?, ?, ?, ?, ?, ?)
			 ON CONFLICT(resource_key) DO UPDATE SET
				network_id = excluded.network_id,
				network_name = excluded.network_name,
				runtime_name = excluded.runtime_name,
				workload_id = excluded.workload_id,
				updated_at_unix_nano = excluded.updated_at_unix_nano`,
			record.ResourceKey,
			record.NetworkID,
			record.NetworkName,
			record.RuntimeName,
			string(record.WorkloadID),
			unixNano(now),
		)
		if execErr != nil {
			return fmt.Errorf("could not upsert persistent network record '%s': %w", record.ResourceKey, execErr)
		}
		return nil
	})
}

func (s *Store) GetPersistentNetwork(ctx context.Context, resourceKey string) (*PersistentNetworkRecord, error) {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return nil, fmt.Errorf("%w: persistent network resource key cannot be empty", ErrInvalidArgument)
	}

	db, dbErr := s.requireDB()
	if dbErr != nil {
		return nil, dbErr
	}

	row := db.QueryRowContext(
		ctx,
		`SELECT resource_key, network_id, network_name, runtime_name, workload_id, updated_at_unix_nano
		 FROM persistent_networks
		 WHERE resource_key = ?`,
		resourceKey,
	)

	record, scanErr := scanPersistentNetwork(row)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrPersistentNetworkNotFound, resourceKey)
	}
	if scanErr != nil {
		return nil, fmt.Errorf("could not get persistent network record '%s': %w", resourceKey, scanErr)
	}
	record.store = s

	return record, nil
}

func (s *Store) ListPersistentNetworksByWorkloadID(ctx context.Context, workloadID commonapi.WorkloadID) ([]PersistentNetworkRecord, error) {
	workloadID = commonapi.WorkloadID(strings.TrimSpace(string(workloadID)))
	if workloadID == "" {
		return nil, fmt.Errorf("%w: persistent network workload ID cannot be empty", ErrInvalidArgument)
	}

	db, dbErr := s.requireDB()
	if dbErr != nil {
		return nil, dbErr
	}

	rows, queryErr := db.QueryContext(
		ctx,
		`SELECT resource_key, network_id, network_name, runtime_name, workload_id, updated_at_unix_nano
		 FROM persistent_networks
		 WHERE workload_id = ?
		 ORDER BY resource_key`,
		string(workloadID),
	)
	if queryErr != nil {
		return nil, fmt.Errorf("could not list persistent network records for workload '%s': %w", workloadID, queryErr)
	}
	defer func() {
		_ = rows.Close()
	}()

	records := []PersistentNetworkRecord{}
	for rows.Next() {
		record, scanErr := scanPersistentNetwork(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read persistent network record: %w", scanErr)
		}
		record.store = s
		records = append(records, *record)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("could not list persistent network records for workload '%s': %w", workloadID, rowsErr)
	}

	return records, nil
}

func (s *Store) DeletePersistentNetwork(ctx context.Context, resourceKey string) error {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return fmt.Errorf("%w: persistent network resource key cannot be empty", ErrInvalidArgument)
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(ctx, `DELETE FROM persistent_networks WHERE resource_key = ?`, resourceKey)
		if execErr != nil {
			return fmt.Errorf("could not delete persistent network record '%s': %w", resourceKey, execErr)
		}
		return nil
	})
}

type persistentNetworkScanner interface {
	Scan(dest ...any) error
}

func scanPersistentNetwork(row persistentNetworkScanner) (*PersistentNetworkRecord, error) {
	var record PersistentNetworkRecord
	var updatedAtUnixNano int64
	scanErr := row.Scan(
		&record.ResourceKey,
		&record.NetworkID,
		&record.NetworkName,
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
