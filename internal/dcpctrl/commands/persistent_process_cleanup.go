/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

const persistentProcessCleanupLeaseRevalidationInterval = 30 * time.Second

type persistentProcessLeaseResource string

func (r persistentProcessLeaseResource) GetLeaseKey() string {
	return string(r)
}

func startInvalidPersistentExecutableRecordCleanup(
	ctx context.Context,
	stateStore *statestore.Store,
	leaseOwner process.ProcessTreeItem,
	log logr.Logger,
) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = resiliency.MakePanicError(recover(), log) }()

		cleanupErr := cleanupInvalidPersistentExecutableRecords(ctx, stateStore, leaseOwner, log)
		if cleanupErr != nil {
			log.Error(cleanupErr, "Failed to clean up invalid persistent Executable process records")
		}
	}()
	return done
}

func cleanupInvalidPersistentExecutableRecords(
	ctx context.Context,
	stateStore *statestore.Store,
	leaseOwner process.ProcessTreeItem,
	log logr.Logger,
) error {
	if stateStore == nil {
		return fmt.Errorf("state store is not configured")
	}

	records, listErr := stateStore.ListPersistentProcesses(ctx)
	if listErr != nil {
		return listErr
	}

	var cleanupErr error
	for _, record := range records {
		if _, findErr := process.FindProcess(record.PID, record.IdentityTime); findErr == nil {
			continue
		}

		recordCleanupErr := cleanupInvalidPersistentExecutableRecord(ctx, stateStore, leaseOwner, record, log)
		if recordCleanupErr != nil {
			cleanupErr = errors.Join(cleanupErr, recordCleanupErr)
		}
	}

	return cleanupErr
}

func cleanupInvalidPersistentExecutableRecord(
	ctx context.Context,
	stateStore *statestore.Store,
	leaseOwner process.ProcessTreeItem,
	record statestore.PersistentProcessRecord,
	log logr.Logger,
) error {
	resource := persistentProcessLeaseResource(record.ResourceKey)
	leaseErr := stateStore.WithResourceLease(ctx, resource, leaseOwner, persistentProcessCleanupLeaseRevalidationInterval, func(ctx context.Context, _ *statestore.ResourceLease) error {
		currentRecord, getErr := stateStore.GetPersistentProcess(ctx, record.ResourceKey)
		if errors.Is(getErr, statestore.ErrPersistentProcessNotFound) {
			log.V(1).Info("Persistent Executable process record was removed before cleanup, leaving it intact",
				"ResourceKey", record.ResourceKey)
			return nil
		}
		if getErr != nil {
			return fmt.Errorf("could not reload persistent Executable process record '%s': %w", record.ResourceKey, getErr)
		}

		_, findErr := process.FindProcess(currentRecord.PID, currentRecord.IdentityTime)
		if findErr == nil {
			log.V(1).Info("Persistent Executable process record became valid before cleanup, leaving it intact",
				"ResourceKey", currentRecord.ResourceKey,
				"PID", currentRecord.PID)
			return nil
		}

		log.Info("Deleting invalid persistent Executable process record",
			"ResourceKey", currentRecord.ResourceKey,
			"PID", currentRecord.PID,
			"Error", findErr.Error())

		if deleteErr := currentRecord.Delete(ctx); deleteErr != nil {
			return fmt.Errorf("could not delete invalid persistent Executable process record '%s': %w", currentRecord.ResourceKey, deleteErr)
		}
		return removePersistentExecutableRecordLogs(ctx, *currentRecord, log)
	})
	if errors.Is(leaseErr, statestore.ErrResourceLeaseHeld) {
		if lease, held := statestore.HeldResourceLease(leaseErr); held {
			log.V(1).Info("Invalid persistent Executable process record cleanup lease is held by another DCP instance, leaving it intact",
				"ResourceKey", record.ResourceKey,
				"LeaseOwnerPID", lease.OwnerProcess.Pid)
		}
		return nil
	}
	if leaseErr != nil {
		return fmt.Errorf("could not clean up invalid persistent Executable process record '%s': %w", record.ResourceKey, leaseErr)
	}

	return nil
}

func removePersistentExecutableRecordLogs(ctx context.Context, record statestore.PersistentProcessRecord, log logr.Logger) error {
	removeLog := func(path string, stream string) error {
		if path == "" {
			return nil
		}
		if removeErr := logs.RemoveWithRetry(ctx, path); removeErr != nil {
			log.Error(removeErr, "Could not remove persistent Executable log file", "ResourceKey", record.ResourceKey, "Path", path, "Stream", stream)
			return removeErr
		}
		return nil
	}

	return errors.Join(
		removeLog(record.StdOutFile, "stdout"),
		removeLog(record.StdErrFile, "stderr"),
	)
}
