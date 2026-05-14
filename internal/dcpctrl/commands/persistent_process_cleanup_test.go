/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/statestore"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestCleanupInvalidPersistentExecutableRecordsDeletesInvalidRecordsAndLogs(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	stateStore := openPersistentProcessCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)

	logDir := t.TempDir()
	activeStdout := createPersistentProcessCleanupLog(t, logDir, "active.out")
	activeStderr := createPersistentProcessCleanupLog(t, logDir, "active.err")
	invalidStdout := createPersistentProcessCleanupLog(t, logDir, "invalid.out")
	invalidStderr := createPersistentProcessCleanupLog(t, logDir, "invalid.err")

	activeRecord := statestore.PersistentProcessRecord{
		ResourceKey:  "default/active",
		LifecycleKey: "active-lifecycle",
		PID:          leaseOwner.Pid,
		IdentityTime: leaseOwner.IdentityTime,
		RunID:        "active",
		StdOutFile:   activeStdout,
		StdErrFile:   activeStderr,
	}
	invalidRecord := statestore.PersistentProcessRecord{
		ResourceKey:  "default/invalid",
		LifecycleKey: "invalid-lifecycle",
		PID:          leaseOwner.Pid,
		IdentityTime: time.Unix(1, 0).UTC(),
		RunID:        "invalid",
		StdOutFile:   invalidStdout,
		StdErrFile:   invalidStderr,
	}
	require.NoError(t, stateStore.UpsertPersistentProcess(ctx, activeRecord))
	require.NoError(t, stateStore.UpsertPersistentProcess(ctx, invalidRecord))

	require.NoError(t, cleanupInvalidPersistentExecutableRecords(ctx, stateStore, leaseOwner, logr.Discard()))

	_, invalidGetErr := stateStore.GetPersistentProcess(ctx, invalidRecord.ResourceKey)
	require.ErrorIs(t, invalidGetErr, statestore.ErrPersistentProcessNotFound)
	_, activeGetErr := stateStore.GetPersistentProcess(ctx, activeRecord.ResourceKey)
	require.NoError(t, activeGetErr)
	require.NoFileExists(t, invalidStdout)
	require.NoFileExists(t, invalidStderr)
	require.FileExists(t, activeStdout)
	require.FileExists(t, activeStderr)
}

func TestStartInvalidPersistentExecutableRecordCleanupRunsCleanupAsync(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	stateStore := openPersistentProcessCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)

	logDir := t.TempDir()
	invalidStdout := createPersistentProcessCleanupLog(t, logDir, "async-invalid.out")
	invalidStderr := createPersistentProcessCleanupLog(t, logDir, "async-invalid.err")

	invalidRecord := statestore.PersistentProcessRecord{
		ResourceKey:  "default/async-invalid",
		LifecycleKey: "async-invalid-lifecycle",
		PID:          leaseOwner.Pid,
		IdentityTime: time.Unix(1, 0).UTC(),
		RunID:        "async-invalid",
		StdOutFile:   invalidStdout,
		StdErrFile:   invalidStderr,
	}
	require.NoError(t, stateStore.UpsertPersistentProcess(ctx, invalidRecord))

	done := startInvalidPersistentExecutableRecordCleanup(ctx, stateStore, leaseOwner, logr.Discard())
	select {
	case <-done:
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	_, invalidGetErr := stateStore.GetPersistentProcess(ctx, invalidRecord.ResourceKey)
	require.ErrorIs(t, invalidGetErr, statestore.ErrPersistentProcessNotFound)
	require.NoFileExists(t, invalidStdout)
	require.NoFileExists(t, invalidStderr)
}

func TestCleanupInvalidPersistentExecutableRecordsSkipsRecordsWithValidHeldLease(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	stateStore := openPersistentProcessCleanupTestStore(t, ctx)
	heldLeaseOwner, heldLeaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, heldLeaseOwnerErr)
	cleanupLeaseOwner := process.ProcessTreeItem{
		Pid:          heldLeaseOwner.Pid,
		IdentityTime: heldLeaseOwner.IdentityTime.Add(-time.Hour),
	}

	logDir := t.TempDir()
	heldStdout := createPersistentProcessCleanupLog(t, logDir, "held.out")
	heldStderr := createPersistentProcessCleanupLog(t, logDir, "held.err")

	heldRecord := statestore.PersistentProcessRecord{
		ResourceKey:  "default/held",
		LifecycleKey: "held-lifecycle",
		PID:          heldLeaseOwner.Pid,
		IdentityTime: time.Unix(1, 0).UTC(),
		RunID:        "held",
		StdOutFile:   heldStdout,
		StdErrFile:   heldStderr,
	}
	require.NoError(t, stateStore.UpsertPersistentProcess(ctx, heldRecord))
	_, leaseErr := stateStore.AcquireResourceLease(ctx, persistentProcessLeaseResource(heldRecord.ResourceKey), heldLeaseOwner, time.Minute)
	require.NoError(t, leaseErr)

	require.NoError(t, cleanupInvalidPersistentExecutableRecords(ctx, stateStore, cleanupLeaseOwner, logr.Discard()))

	_, getErr := stateStore.GetPersistentProcess(ctx, heldRecord.ResourceKey)
	require.NoError(t, getErr)
	require.FileExists(t, heldStdout)
	require.FileExists(t, heldStderr)
}

func TestListPersistentProcessesReturnsRecordsInResourceKeyOrder(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	stateStore := openPersistentProcessCleanupTestStore(t, ctx)
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)

	for _, resourceKey := range []string{"default/z", "default/a"} {
		require.NoError(t, stateStore.UpsertPersistentProcess(ctx, statestore.PersistentProcessRecord{
			ResourceKey:  resourceKey,
			LifecycleKey: resourceKey + "-lifecycle",
			PID:          leaseOwner.Pid,
			IdentityTime: leaseOwner.IdentityTime,
			RunID:        resourceKey + "-run",
		}))
	}

	records, listErr := stateStore.ListPersistentProcesses(ctx)
	require.NoError(t, listErr)
	require.Len(t, records, 2)
	require.Equal(t, "default/a", records[0].ResourceKey)
	require.Equal(t, "default/z", records[1].ResourceKey)
}

func openPersistentProcessCleanupTestStore(t *testing.T, ctx context.Context) *statestore.Store {
	t.Helper()

	stateStorePath := filepath.Join(t.TempDir(), "state-store", "state.sqlite3")
	stateStore, openErr := statestore.Open(ctx, statestore.Options{
		Path:        stateStorePath,
		BusyTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, openErr)
	t.Cleanup(func() {
		require.NoError(t, stateStore.Close())
	})
	return stateStore
}

func createPersistentProcessCleanupLog(t *testing.T, dir string, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	file, openErr := usvc_io.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, openErr)
	require.NoError(t, file.Close())
	t.Cleanup(func() {
		removeErr := os.Remove(path)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			require.NoError(t, removeErr)
		}
	})
	return path
}
