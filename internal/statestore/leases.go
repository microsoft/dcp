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

var (
	ErrResourceLeaseHeld    = errors.New("resource lease is held by another owner")
	ErrResourceLeaseNotHeld = errors.New("resource lease is not held by owner")
)

type ResourceLease struct {
	ResourceKey  string
	OwnerProcess process.ProcessTreeItem
	LeaseUntil   time.Time
	UpdatedAt    time.Time
	Metadata     string
}

func (s *Store) AcquireResourceLease(ctx context.Context, resourceKey string, ownerProcess process.ProcessTreeItem, ttl time.Duration, metadata string) (*ResourceLease, error) {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return nil, fmt.Errorf("%w: resource key cannot be empty", ErrInvalidArgument)
	}
	normalizedOwner, ownerErr := normalizeResourceLeaseOwner(ownerProcess)
	if ownerErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidArgument, ownerErr)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("%w: lease TTL must be positive", ErrInvalidArgument)
	}

	now := time.Now().UTC()
	leaseUntil := now.Add(ttl)
	ownerPID := normalizedOwner.Pid
	ownerIdentityTimeUnixNano := unixNano(normalizedOwner.IdentityTime)
	lease := &ResourceLease{
		ResourceKey:  resourceKey,
		OwnerProcess: normalizedOwner,
		LeaseUntil:   leaseUntil,
		UpdatedAt:    now,
		Metadata:     metadata,
	}

	txErr := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		result, execErr := conn.ExecContext(
			ctx,
			`INSERT INTO resource_locks(resource_key, owner_pid, owner_identity_time_unix_nano, lease_until_unix_nano, updated_at_unix_nano, metadata)
			 VALUES(?, ?, ?, ?, ?, ?)
			 ON CONFLICT(resource_key) DO UPDATE SET
				owner_pid = excluded.owner_pid,
				owner_identity_time_unix_nano = excluded.owner_identity_time_unix_nano,
				lease_until_unix_nano = excluded.lease_until_unix_nano,
				updated_at_unix_nano = excluded.updated_at_unix_nano,
				metadata = excluded.metadata
			 WHERE resource_locks.lease_until_unix_nano <= ?
				OR (resource_locks.owner_pid = ? AND resource_locks.owner_identity_time_unix_nano = ?)`,
			resourceKey,
			ownerPID,
			ownerIdentityTimeUnixNano,
			unixNano(leaseUntil),
			unixNano(now),
			metadata,
			unixNano(now),
			ownerPID,
			ownerIdentityTimeUnixNano,
		)
		if execErr != nil {
			return fmt.Errorf("could not acquire resource lease '%s': %w", resourceKey, execErr)
		}

		rowsAffected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("could not confirm resource lease acquisition for '%s': %w", resourceKey, rowsErr)
		}
		if rowsAffected == 0 {
			return fmt.Errorf("%w: %s", ErrResourceLeaseHeld, resourceKey)
		}

		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	return lease, nil
}

func (s *Store) RenewResourceLease(ctx context.Context, resourceKey string, ownerProcess process.ProcessTreeItem, ttl time.Duration) (*ResourceLease, error) {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return nil, fmt.Errorf("%w: resource key cannot be empty", ErrInvalidArgument)
	}
	normalizedOwner, ownerErr := normalizeResourceLeaseOwner(ownerProcess)
	if ownerErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidArgument, ownerErr)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("%w: lease TTL must be positive", ErrInvalidArgument)
	}

	now := time.Now().UTC()
	leaseUntil := now.Add(ttl)
	ownerPID := normalizedOwner.Pid
	ownerIdentityTimeUnixNano := unixNano(normalizedOwner.IdentityTime)
	var lease *ResourceLease

	txErr := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		result, execErr := conn.ExecContext(
			ctx,
			`UPDATE resource_locks
			 SET lease_until_unix_nano = ?, updated_at_unix_nano = ?
			 WHERE resource_key = ? AND owner_pid = ? AND owner_identity_time_unix_nano = ? AND lease_until_unix_nano > ?`,
			unixNano(leaseUntil),
			unixNano(now),
			resourceKey,
			ownerPID,
			ownerIdentityTimeUnixNano,
			unixNano(now),
		)
		if execErr != nil {
			return fmt.Errorf("could not renew resource lease '%s': %w", resourceKey, execErr)
		}

		rowsAffected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("could not confirm resource lease renewal for '%s': %w", resourceKey, rowsErr)
		}
		if rowsAffected == 0 {
			return fmt.Errorf("%w: %s", ErrResourceLeaseNotHeld, resourceKey)
		}

		record, getErr := getResourceLease(ctx, conn, resourceKey)
		if getErr != nil {
			return getErr
		}
		lease = record
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	return lease, nil
}

func (s *Store) ReleaseResourceLease(ctx context.Context, resourceKey string, ownerProcess process.ProcessTreeItem) error {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return fmt.Errorf("%w: resource key cannot be empty", ErrInvalidArgument)
	}
	normalizedOwner, ownerErr := normalizeResourceLeaseOwner(ownerProcess)
	if ownerErr != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArgument, ownerErr)
	}
	ownerPID := normalizedOwner.Pid
	ownerIdentityTimeUnixNano := unixNano(normalizedOwner.IdentityTime)

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(
			ctx,
			`DELETE FROM resource_locks
			 WHERE resource_key = ? AND owner_pid = ? AND owner_identity_time_unix_nano = ?`,
			resourceKey,
			ownerPID,
			ownerIdentityTimeUnixNano,
		)
		if execErr != nil {
			return fmt.Errorf("could not release resource lease '%s': %w", resourceKey, execErr)
		}

		return nil
	})
}

func (s *Store) WithResourceLease(ctx context.Context, resourceKey string, ownerProcess process.ProcessTreeItem, ttl time.Duration, metadata string, f func(context.Context, *ResourceLease) error) error {
	if f == nil {
		return fmt.Errorf("%w: lease callback cannot be nil", ErrInvalidArgument)
	}

	lease, acquireErr := s.AcquireResourceLease(ctx, resourceKey, ownerProcess, ttl, metadata)
	if acquireErr != nil {
		return acquireErr
	}

	callbackErr := f(ctx, lease)
	releaseErr := s.ReleaseResourceLease(context.Background(), resourceKey, ownerProcess)
	return errors.Join(callbackErr, releaseErr)
}

func (s *Store) DeleteExpiredResourceLeases(ctx context.Context) error {
	now := time.Now().UTC()
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(ctx, `DELETE FROM resource_locks WHERE lease_until_unix_nano <= ?`, unixNano(now))
		if execErr != nil {
			return fmt.Errorf("could not delete expired resource leases: %w", execErr)
		}
		return nil
	})
}

type inactiveResourceLeaseCandidate struct {
	resourceKey               string
	ownerPID                  int64
	ownerIdentityTimeUnixNano int64
	leaseUntilUnixNano        int64
	updatedAtUnixNano         int64
}

func (s *Store) DeleteInactiveResourceLeases(ctx context.Context) error {
	now := time.Now().UTC()
	candidates, candidatesErr := s.inactiveResourceLeaseCandidates(ctx, now)
	if candidatesErr != nil {
		return candidatesErr
	}
	if len(candidates) == 0 {
		return nil
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		for _, candidate := range candidates {
			_, execErr := conn.ExecContext(
				ctx,
				`DELETE FROM resource_locks
				 WHERE resource_key = ? AND owner_pid = ? AND owner_identity_time_unix_nano = ?
					AND lease_until_unix_nano = ? AND updated_at_unix_nano = ?`,
				candidate.resourceKey,
				candidate.ownerPID,
				candidate.ownerIdentityTimeUnixNano,
				candidate.leaseUntilUnixNano,
				candidate.updatedAtUnixNano,
			)
			if execErr != nil {
				return fmt.Errorf("could not delete inactive resource lease '%s': %w", candidate.resourceKey, execErr)
			}
		}
		return nil
	})
}

func (s *Store) inactiveResourceLeaseCandidates(ctx context.Context, now time.Time) ([]inactiveResourceLeaseCandidate, error) {
	db, dbErr := s.requireDB()
	if dbErr != nil {
		return nil, dbErr
	}

	rows, queryErr := db.QueryContext(
		ctx,
		`SELECT resource_key, owner_pid, owner_identity_time_unix_nano, lease_until_unix_nano, updated_at_unix_nano
		 FROM resource_locks`,
	)
	if queryErr != nil {
		return nil, fmt.Errorf("could not list resource leases for cleanup: %w", queryErr)
	}
	defer func() {
		_ = rows.Close()
	}()

	nowUnixNano := unixNano(now)
	candidates := []inactiveResourceLeaseCandidate{}
	for rows.Next() {
		var candidate inactiveResourceLeaseCandidate
		scanErr := rows.Scan(
			&candidate.resourceKey,
			&candidate.ownerPID,
			&candidate.ownerIdentityTimeUnixNano,
			&candidate.leaseUntilUnixNano,
			&candidate.updatedAtUnixNano,
		)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read resource lease for cleanup: %w", scanErr)
		}

		if resourceLeaseIsInactive(candidate, nowUnixNano) {
			candidates = append(candidates, candidate)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("could not list resource leases for cleanup: %w", rowsErr)
	}

	return candidates, nil
}

func resourceLeaseIsInactive(candidate inactiveResourceLeaseCandidate, nowUnixNano int64) bool {
	if candidate.leaseUntilUnixNano <= nowUnixNano {
		return true
	}

	ownerProcess, ownerErr := resourceLeaseOwnerFromDB(candidate.ownerPID, candidate.ownerIdentityTimeUnixNano)
	return ownerErr != nil || !resourceLeaseOwnerIsActive(ownerProcess)
}

func getResourceLease(ctx context.Context, conn *sql.Conn, resourceKey string) (*ResourceLease, error) {
	row := conn.QueryRowContext(
		ctx,
		`SELECT resource_key, owner_pid, owner_identity_time_unix_nano, lease_until_unix_nano, updated_at_unix_nano, metadata
		 FROM resource_locks WHERE resource_key = ?`,
		resourceKey,
	)

	var lease ResourceLease
	var ownerPID int64
	var ownerIdentityTimeUnixNano int64
	var leaseUntilUnixNano int64
	var updatedAtUnixNano int64
	scanErr := row.Scan(
		&lease.ResourceKey,
		&ownerPID,
		&ownerIdentityTimeUnixNano,
		&leaseUntilUnixNano,
		&updatedAtUnixNano,
		&lease.Metadata,
	)
	if scanErr != nil {
		return nil, fmt.Errorf("could not read resource lease '%s': %w", resourceKey, scanErr)
	}

	ownerProcess, ownerErr := resourceLeaseOwnerFromDB(ownerPID, ownerIdentityTimeUnixNano)
	if ownerErr != nil {
		return nil, fmt.Errorf("could not read resource lease owner '%s': %w", resourceKey, ownerErr)
	}
	lease.OwnerProcess = ownerProcess
	lease.LeaseUntil = timeFromUnixNano(leaseUntilUnixNano)
	lease.UpdatedAt = timeFromUnixNano(updatedAtUnixNano)
	return &lease, nil
}
