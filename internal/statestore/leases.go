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

	"github.com/cenkalti/backoff/v4"

	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

var (
	ErrResourceLeaseHeld    = errors.New("resource lease is held by another owner")
	ErrResourceLeaseNotHeld = errors.New("resource lease is not held by owner")
)

type ResourceLease struct {
	ResourceKey  string
	OwnerProcess process.ProcessHandle
	UpdatedAt    time.Time
	store        *Store
}

type ResourceLeaseHeldError struct {
	Lease ResourceLease
}

func (e *ResourceLeaseHeldError) Error() string {
	return fmt.Sprintf("%s: %s", ErrResourceLeaseHeld, e.Lease.ResourceKey)
}

func (e *ResourceLeaseHeldError) Unwrap() error {
	return ErrResourceLeaseHeld
}

func HeldResourceLease(err error) (*ResourceLease, bool) {
	var heldErr *ResourceLeaseHeldError
	if !errors.As(err, &heldErr) {
		return nil, false
	}

	lease := heldErr.Lease
	return &lease, true
}

func (l *ResourceLease) Release(ctx context.Context) error {
	if l == nil {
		return fmt.Errorf("%w: resource lease cannot be nil", ErrInvalidArgument)
	}
	if l.store == nil {
		return fmt.Errorf("%w: resource lease store cannot be nil", ErrInvalidArgument)
	}

	return l.store.releaseResourceLease(ctx, l.ResourceKey, l.OwnerProcess)
}

type LeasableResource interface {
	GetLeaseKey() string
}

func resourceLeaseKey(resource LeasableResource) (string, error) {
	if resource == nil {
		return "", fmt.Errorf("%w: resource cannot be nil", ErrInvalidArgument)
	}
	resourceKey := strings.TrimSpace(resource.GetLeaseKey())
	if resourceKey == "" {
		return "", fmt.Errorf("%w: resource lease key cannot be empty", ErrInvalidArgument)
	}
	return resourceKey, nil
}

func (s *Store) AcquireResourceLease(ctx context.Context, resource LeasableResource, ownerProcess process.ProcessHandle, revalidationInterval time.Duration) (*ResourceLease, error) {
	resourceKey, resourceKeyErr := resourceLeaseKey(resource)
	if resourceKeyErr != nil {
		return nil, resourceKeyErr
	}
	normalizedOwner, ownerErr := normalizeResourceLeaseOwner(ownerProcess)
	if ownerErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidArgument, ownerErr)
	}
	if revalidationInterval <= 0 {
		return nil, fmt.Errorf("%w: lease revalidation interval must be positive", ErrInvalidArgument)
	}

	now := time.Now().UTC()
	ownerPID := normalizedOwner.Pid
	ownerIdentityTime := timeString(normalizedOwner.IdentityTime)
	var lease *ResourceLease

	txErr := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		insertResult, insertErr := conn.ExecContext(
			ctx,
			`INSERT OR IGNORE INTO resource_locks(resource_key, owner_pid, owner_identity_time, updated_at_unix_nano)
			 VALUES(?, ?, ?, ?)`,
			resourceKey,
			ownerPID,
			ownerIdentityTime,
			unixNano(now),
		)
		if insertErr != nil {
			return fmt.Errorf("could not acquire resource lease '%s': %w", resourceKey, insertErr)
		}

		rowsAffected, rowsErr := insertResult.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("could not confirm resource lease acquisition for '%s': %w", resourceKey, rowsErr)
		}
		if rowsAffected == 1 {
			var getErr error
			lease, getErr = getResourceLease(ctx, conn, resourceKey)
			return getErr
		}

		existingLease, getErr := getResourceLease(ctx, conn, resourceKey)
		if getErr != nil {
			return getErr
		}
		sameOwner := existingLease.OwnerProcess.Pid == normalizedOwner.Pid &&
			existingLease.OwnerProcess.IdentityTime.Equal(normalizedOwner.IdentityTime)
		if !sameOwner && now.Sub(existingLease.UpdatedAt) < revalidationInterval {
			existingLease.store = s
			return &ResourceLeaseHeldError{Lease: *existingLease}
		}
		if !sameOwner && resourceLeaseOwnerIsActive(existingLease.OwnerProcess) {
			updateErr := updateResourceLeaseTimestamp(ctx, conn, resourceKey, existingLease, now)
			if updateErr != nil {
				return updateErr
			}
			existingLease.store = s
			return &ResourceLeaseHeldError{Lease: *existingLease}
		}

		updateErr := updateResourceLeaseOwner(ctx, conn, resourceKey, normalizedOwner, now)
		if updateErr != nil {
			return updateErr
		}
		lease, getErr = getResourceLease(ctx, conn, resourceKey)
		return getErr
	})
	if txErr != nil {
		return nil, txErr
	}
	lease.store = s

	return lease, nil
}

func (s *Store) ReleaseResourceLease(ctx context.Context, resource LeasableResource, ownerProcess process.ProcessHandle) error {
	resourceKey, resourceKeyErr := resourceLeaseKey(resource)
	if resourceKeyErr != nil {
		return resourceKeyErr
	}
	return s.releaseResourceLease(ctx, resourceKey, ownerProcess)
}

func (s *Store) releaseResourceLease(ctx context.Context, resourceKey string, ownerProcess process.ProcessHandle) error {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return fmt.Errorf("%w: resource lease key cannot be empty", ErrInvalidArgument)
	}
	normalizedOwner, ownerErr := normalizeResourceLeaseOwner(ownerProcess)
	if ownerErr != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArgument, ownerErr)
	}
	ownerPID := normalizedOwner.Pid
	ownerIdentityTime := timeString(normalizedOwner.IdentityTime)

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		result, execErr := conn.ExecContext(
			ctx,
			`DELETE FROM resource_locks
			 WHERE resource_key = ? AND owner_pid = ? AND owner_identity_time = ?`,
			resourceKey,
			ownerPID,
			ownerIdentityTime,
		)
		if execErr != nil {
			return fmt.Errorf("could not release resource lease '%s': %w", resourceKey, execErr)
		}
		rowsAffected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("could not confirm resource lease release for '%s': %w", resourceKey, rowsErr)
		}
		if rowsAffected == 0 {
			return fmt.Errorf("%w: %s", ErrResourceLeaseNotHeld, resourceKey)
		}

		return nil
	})
}

func (s *Store) VerifyResourceLeaseHeld(ctx context.Context, resource LeasableResource, ownerProcess process.ProcessHandle) error {
	resourceKey, resourceKeyErr := resourceLeaseKey(resource)
	if resourceKeyErr != nil {
		return resourceKeyErr
	}
	normalizedOwner, ownerErr := normalizeResourceLeaseOwner(ownerProcess)
	if ownerErr != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArgument, ownerErr)
	}

	db, dbErr := s.requireDB()
	if dbErr != nil {
		return dbErr
	}

	row := db.QueryRowContext(
		ctx,
		`SELECT owner_pid, owner_identity_time FROM resource_locks WHERE resource_key = ?`,
		resourceKey,
	)

	var ownerPID int64
	var ownerIdentityTime string
	scanErr := row.Scan(&ownerPID, &ownerIdentityTime)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrResourceLeaseNotHeld, resourceKey)
	}
	if scanErr != nil {
		return fmt.Errorf("could not verify resource lease '%s': %w", resourceKey, scanErr)
	}

	heldOwner, heldOwnerErr := resourceLeaseOwnerFromDB(ownerPID, ownerIdentityTime)
	if heldOwnerErr != nil {
		return heldOwnerErr
	}
	if heldOwner.Pid != normalizedOwner.Pid || !heldOwner.IdentityTime.Equal(normalizedOwner.IdentityTime) {
		return fmt.Errorf("%w: %s", ErrResourceLeaseNotHeld, resourceKey)
	}

	return nil
}

func (s *Store) WithResourceLease(ctx context.Context, resource LeasableResource, ownerProcess process.ProcessHandle, revalidationInterval time.Duration, f func(context.Context, *ResourceLease) error) error {
	if f == nil {
		return fmt.Errorf("%w: lease callback cannot be nil", ErrInvalidArgument)
	}

	lease, acquireErr := s.AcquireResourceLease(ctx, resource, ownerProcess, revalidationInterval)
	if acquireErr != nil {
		return acquireErr
	}

	return withResourceLeaseCallback(ctx, lease, f)
}

// WithResourceLeaseRetry retries held leases until the lease is acquired or ctx is cancelled.
func (s *Store) WithResourceLeaseRetry(
	ctx context.Context,
	resource LeasableResource,
	ownerProcess process.ProcessHandle,
	revalidationInterval time.Duration,
	retryInterval time.Duration,
	f func(context.Context, *ResourceLease) error,
) error {
	if f == nil {
		return fmt.Errorf("%w: lease callback cannot be nil", ErrInvalidArgument)
	}
	if retryInterval <= 0 {
		return fmt.Errorf("%w: lease retry interval must be positive", ErrInvalidArgument)
	}

	return resiliency.Retry(ctx, backoff.NewConstantBackOff(retryInterval), func() error {
		lease, acquireErr := s.AcquireResourceLease(ctx, resource, ownerProcess, revalidationInterval)
		if acquireErr == nil {
			callbackErr := withResourceLeaseCallback(ctx, lease, f)
			if callbackErr != nil {
				return resiliency.Permanent(callbackErr)
			}
			return nil
		}
		if !errors.Is(acquireErr, ErrResourceLeaseHeld) {
			return resiliency.Permanent(acquireErr)
		}

		return acquireErr
	})
}

func withResourceLeaseCallback(ctx context.Context, lease *ResourceLease, f func(context.Context, *ResourceLease) error) error {
	callbackErr := f(ctx, lease)
	releaseErr := lease.Release(context.Background())
	return errors.Join(callbackErr, releaseErr)
}

type inactiveResourceLeaseCandidate struct {
	resourceKey       string
	ownerPID          int64
	ownerIdentityTime string
	updatedAtUnixNano int64
}

func (s *Store) DeleteInactiveResourceLeases(ctx context.Context) error {
	candidates, candidatesErr := s.inactiveResourceLeaseCandidates(ctx)
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
				 WHERE resource_key = ? AND owner_pid = ? AND owner_identity_time = ?
					AND updated_at_unix_nano = ?`,
				candidate.resourceKey,
				candidate.ownerPID,
				candidate.ownerIdentityTime,
				candidate.updatedAtUnixNano,
			)
			if execErr != nil {
				return fmt.Errorf("could not delete inactive resource lease '%s': %w", candidate.resourceKey, execErr)
			}
		}
		return nil
	})
}

func (s *Store) inactiveResourceLeaseCandidates(ctx context.Context) ([]inactiveResourceLeaseCandidate, error) {
	db, dbErr := s.requireDB()
	if dbErr != nil {
		return nil, dbErr
	}

	rows, queryErr := db.QueryContext(
		ctx,
		`SELECT resource_key, owner_pid, owner_identity_time, updated_at_unix_nano
		 FROM resource_locks`,
	)
	if queryErr != nil {
		return nil, fmt.Errorf("could not list resource leases for cleanup: %w", queryErr)
	}
	defer func() {
		_ = rows.Close()
	}()

	candidates := []inactiveResourceLeaseCandidate{}
	for rows.Next() {
		var candidate inactiveResourceLeaseCandidate
		scanErr := rows.Scan(
			&candidate.resourceKey,
			&candidate.ownerPID,
			&candidate.ownerIdentityTime,
			&candidate.updatedAtUnixNano,
		)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read resource lease for cleanup: %w", scanErr)
		}

		if resourceLeaseIsInactive(candidate) {
			candidates = append(candidates, candidate)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("could not list resource leases for cleanup: %w", rowsErr)
	}

	return candidates, nil
}

func resourceLeaseIsInactive(candidate inactiveResourceLeaseCandidate) bool {
	ownerProcess, ownerErr := resourceLeaseOwnerFromDB(candidate.ownerPID, candidate.ownerIdentityTime)
	return ownerErr != nil || !resourceLeaseOwnerIsActive(ownerProcess)
}

func getResourceLease(ctx context.Context, conn *sql.Conn, resourceKey string) (*ResourceLease, error) {
	row := conn.QueryRowContext(
		ctx,
		`SELECT resource_key, owner_pid, owner_identity_time, updated_at_unix_nano
		 FROM resource_locks WHERE resource_key = ?`,
		resourceKey,
	)

	var lease ResourceLease
	var ownerPID int64
	var ownerIdentityTime string
	var updatedAtUnixNano int64
	scanErr := row.Scan(
		&lease.ResourceKey,
		&ownerPID,
		&ownerIdentityTime,
		&updatedAtUnixNano,
	)
	if scanErr != nil {
		return nil, fmt.Errorf("could not read resource lease '%s': %w", resourceKey, scanErr)
	}

	ownerProcess, ownerErr := resourceLeaseOwnerFromDB(ownerPID, ownerIdentityTime)
	if ownerErr != nil {
		return nil, fmt.Errorf("could not read resource lease owner '%s': %w", resourceKey, ownerErr)
	}
	lease.OwnerProcess = ownerProcess
	lease.UpdatedAt = timeFromUnixNano(updatedAtUnixNano)
	return &lease, nil
}

func updateResourceLeaseTimestamp(ctx context.Context, conn *sql.Conn, resourceKey string, lease *ResourceLease, now time.Time) error {
	_, execErr := conn.ExecContext(
		ctx,
		`UPDATE resource_locks
		 SET updated_at_unix_nano = ?
		 WHERE resource_key = ? AND owner_pid = ? AND owner_identity_time = ? AND updated_at_unix_nano = ?`,
		unixNano(now),
		resourceKey,
		lease.OwnerProcess.Pid,
		timeString(lease.OwnerProcess.IdentityTime),
		unixNano(lease.UpdatedAt),
	)
	if execErr != nil {
		return fmt.Errorf("could not update resource lease validation time '%s': %w", resourceKey, execErr)
	}
	return nil
}

func updateResourceLeaseOwner(ctx context.Context, conn *sql.Conn, resourceKey string, ownerProcess process.ProcessHandle, now time.Time) error {
	_, execErr := conn.ExecContext(
		ctx,
		`UPDATE resource_locks
		 SET owner_pid = ?, owner_identity_time = ?, updated_at_unix_nano = ?
		 WHERE resource_key = ?`,
		ownerProcess.Pid,
		timeString(ownerProcess.IdentityTime),
		unixNano(now),
		resourceKey,
	)
	if execErr != nil {
		return fmt.Errorf("could not update resource lease owner '%s': %w", resourceKey, execErr)
	}
	return nil
}
