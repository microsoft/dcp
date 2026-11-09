/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package statestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/microsoft/dcp/internal/lockfile"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

func initializationTestPath(t *testing.T) string {
	t.Helper()

	storeDir := t.TempDir()
	require.NoError(t, usvc_io.EnsureRestrictedDirectory(storeDir, osutil.PermissionOnlyOwnerReadWriteTraverse))
	return filepath.Join(storeDir, "state.sqlite3")
}

func holdSQLiteTransactionLock(t *testing.T, ctx context.Context, path string, beginStatement string) func() {
	t.Helper()

	db := openRawSQLiteDB(t, ctx, path)
	db.SetMaxOpenConns(1)
	conn, connErr := db.Conn(ctx)
	require.NoError(t, connErr)
	_, createErr := conn.ExecContext(ctx, "CREATE TABLE initialization_contention (value INTEGER)")
	require.NoError(t, createErr)
	_, beginErr := conn.ExecContext(ctx, beginStatement)
	require.NoError(t, beginErr)

	locked := true
	release := func() {
		if locked {
			_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
			require.NoError(t, rollbackErr)
			locked = false
		}
	}
	t.Cleanup(func() {
		release()
		require.NoError(t, conn.Close())
		require.NoError(t, db.Close())
	})
	return release
}

type pendingStoreOpen struct {
	done  chan struct{}
	store *Store
	err   error
}

func startStoreOpen(t *testing.T, ctx context.Context, options Options) *pendingStoreOpen {
	t.Helper()

	openCtx, cancelOpen := context.WithCancel(ctx)
	pending := &pendingStoreOpen{done: make(chan struct{})}
	t.Cleanup(func() {
		cancelOpen()
		<-pending.done
		if pending.store != nil {
			require.NoError(t, pending.store.Close())
		}
	})
	go func() {
		pending.store, pending.err = Open(openCtx, options)
		close(pending.done)
	}()
	return pending
}

func requireSQLiteBusyTimeout(t *testing.T, ctx context.Context, store *Store, expected time.Duration) {
	t.Helper()

	var milliseconds int64
	require.NoError(t, store.db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&milliseconds))
	require.Equal(t, expected.Milliseconds(), milliseconds)
}

func runSynctestWithTestContext(t *testing.T, testTimeout time.Duration, run func(*testing.T, context.Context)) {
	t.Helper()

	deadlineContext, cancelDeadlineContext := testutil.GetTestContext(t, testTimeout)
	deadline, hasDeadline := deadlineContext.Deadline()
	contextTimeout := time.Duration(0)
	if hasDeadline {
		contextTimeout = time.Until(deadline)
	}
	cancelDeadlineContext()

	synctest.Test(t, func(test *testing.T) {
		if !hasDeadline {
			run(test, test.Context())
			return
		}

		ctx, cancelContext := context.WithTimeout(test.Context(), contextTimeout)
		defer cancelContext()

		run(test, ctx)
	})
}

// Verifies that retrySQLiteInitialization does not time out an uncontended first attempt and gives contention a single full retry budget.
func TestRetrySQLiteInitializationStartsBudgetAfterFirstBusy(t *testing.T) {
	t.Parallel()

	for _, withContention := range []bool{false, true} {
		t.Run(fmt.Sprintf("contention=%t", withContention), func(t *testing.T) {
			runSynctestWithTestContext(t, stateStoreTestTimeout, func(test *testing.T, ctx context.Context) {
				const busyTimeout = 100 * time.Millisecond
				busyErr := errors.New("initialization contention")
				started := time.Now()
				var firstAttemptFinished time.Time
				attemptCount := 0

				retryErr := retrySQLiteInitialization(ctx, busyTimeout, logr.Discard(), func(attemptCtx context.Context) (bool, error) {
					attemptCount++
					if attemptCount == 1 {
						require.True(test, attemptCtx == ctx)
						time.Sleep(2 * busyTimeout)
						require.NoError(test, attemptCtx.Err())
						firstAttemptFinished = time.Now()
						if !withContention {
							return false, nil
						}
					} else {
						retryDeadline, hasDeadline := attemptCtx.Deadline()
						require.True(test, hasDeadline)
						require.Equal(test, firstAttemptFinished.Add(busyTimeout), retryDeadline)
					}
					return true, busyErr
				})

				require.NoError(test, ctx.Err())
				if withContention {
					require.ErrorIs(test, retryErr, context.DeadlineExceeded)
					require.ErrorIs(test, retryErr, busyErr)
					require.Greater(test, attemptCount, 2)
					require.Equal(test, 3*busyTimeout, time.Since(started))
				} else {
					require.NoError(test, retryErr)
					require.Equal(test, 1, attemptCount)
					require.Equal(test, 2*busyTimeout, time.Since(started))
				}
			})
		})
	}
}

// Verifies that retrySQLiteInitialization observes an earlier caller deadline without losing the contention error.
func TestRetrySQLiteInitializationHonorsCallerDeadline(t *testing.T) {
	t.Parallel()

	runSynctestWithTestContext(t, stateStoreTestTimeout, func(test *testing.T, ctx context.Context) {
		const busyTimeout = 100 * time.Millisecond
		const callerTimeout = busyTimeout / 2
		callerCtx, cancelCaller := context.WithTimeout(ctx, callerTimeout)
		defer cancelCaller()
		busyErr := errors.New("initialization contention")
		started := time.Now()

		retryErr := retrySQLiteInitialization(callerCtx, busyTimeout, logr.Discard(), func(context.Context) (bool, error) {
			return true, busyErr
		})

		require.ErrorIs(test, retryErr, context.DeadlineExceeded)
		require.ErrorIs(test, retryErr, busyErr)
		require.Equal(test, 1, strings.Count(retryErr.Error(), context.DeadlineExceeded.Error()))
		require.NoError(test, ctx.Err())
		require.Equal(test, callerTimeout, time.Since(started))
	})
}

// Verifies that retrySQLiteInitialization cancels each retry phase while preserving contention and cleanup errors without duplicating cancellation.
func TestRetrySQLiteInitializationPreservesErrorsOnCancellation(t *testing.T) {
	t.Parallel()

	for _, phase := range []string{
		"before first attempt",
		"before first retry",
		"during backoff",
		"inside retry",
		"inside retry with cleanup failure",
		"before later notification",
	} {
		t.Run(phase, func(t *testing.T) {
			runSynctestWithTestContext(t, stateStoreTestTimeout, func(test *testing.T, ctx context.Context) {
				callerCtx, cancelCaller := context.WithCancel(ctx)
				defer cancelCaller()
				firstBusyErr := errors.New("first initialization contention")
				laterBusyErr := errors.New("later initialization contention")
				cleanupErr := errors.New("could not close failed connection")
				backoffReady := make(chan struct{})
				attemptCount := 0
				logCount := 0

				if phase == "before first attempt" {
					cancelCaller()
				}
				if phase == "during backoff" {
					go func() {
						<-backoffReady
						synctest.Wait()
						cancelCaller()
					}()
				}
				log := funcr.New(func(_, _ string) {
					logCount++
					switch {
					case phase == "before first retry":
						cancelCaller()
					case phase == "during backoff" && logCount == 2:
						close(backoffReady)
					case phase == "before later notification" && logCount == 2:
						cancelCaller()
					}
				}, funcr.Options{Verbosity: 1})
				started := time.Now()
				retryErr := retrySQLiteInitialization(callerCtx, time.Second, log, func(attemptCtx context.Context) (bool, error) {
					attemptCount++
					if attemptCount == 1 {
						return true, firstBusyErr
					}
					if phase == "inside retry" || phase == "inside retry with cleanup failure" {
						cancelCaller()
						configureErr := fmt.Errorf("could not configure SQLite: %w", attemptCtx.Err())
						if phase == "inside retry with cleanup failure" {
							return false, errors.Join(configureErr, cleanupErr)
						}
						return false, configureErr
					}
					return true, laterBusyErr
				})

				require.ErrorIs(test, retryErr, context.Canceled)
				require.Equal(test, 1, strings.Count(retryErr.Error(), context.Canceled.Error()))
				require.NoError(test, ctx.Err())
				require.Zero(test, time.Since(started))
				switch phase {
				case "before first attempt":
					require.Zero(test, attemptCount)
				case "before first retry":
					require.Equal(test, 1, attemptCount)
					require.ErrorIs(test, retryErr, firstBusyErr)
				default:
					require.Equal(test, 2, attemptCount)
					if phase == "during backoff" || phase == "before later notification" {
						require.ErrorIs(test, retryErr, laterBusyErr)
					} else {
						require.ErrorIs(test, retryErr, firstBusyErr)
					}
				}
				if phase == "inside retry with cleanup failure" {
					require.ErrorIs(test, retryErr, cleanupErr)
				}
			})
		})
	}
}

// Verifies that retrySQLiteInitialization does not retry permanent failures or failed cleanup even when contention is also reported.
func TestRetrySQLiteInitializationStopsOnPermanentFailure(t *testing.T) {
	t.Parallel()

	permanentErr := errors.New("invalid database")
	busyErr := errors.New("initialization contention")
	cleanupErr := errors.New("could not close failed connection")
	for _, testCase := range []struct {
		name       string
		attemptErr error
	}{
		{name: "permanent error", attemptErr: permanentErr},
		{name: "cleanup failure", attemptErr: errors.Join(busyErr, cleanupErr)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runSynctestWithTestContext(t, stateStoreTestTimeout, func(test *testing.T, ctx context.Context) {
				attemptCount := 0
				started := time.Now()
				retryErr := retrySQLiteInitialization(ctx, time.Second, logr.Discard(), func(context.Context) (bool, error) {
					attemptCount++
					return false, testCase.attemptErr
				})

				require.ErrorIs(test, retryErr, testCase.attemptErr)
				require.Equal(test, 1, attemptCount)
				require.Zero(test, time.Since(started))
				require.NoError(test, ctx.Err())
			})
		})
	}
}

// Verifies that Open serializes concurrent first opens of one store and creates a clean current schema.
func TestOpenConcurrentColdStore(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	storePath := initializationTestPath(t)
	const openerCount = 16
	start := make(chan struct{})
	results := make(chan error, openerCount)
	var openers sync.WaitGroup
	defer func() {
		cancel()
		openers.Wait()
	}()

	for range openerCount {
		openers.Go(func() {
			select {
			case <-start:
			case <-ctx.Done():
				results <- ctx.Err()
				return
			}
			store, openErr := Open(ctx, Options{Path: storePath})
			if store != nil {
				openErr = errors.Join(openErr, store.Close())
			}
			results <- openErr
		})
	}
	close(start)

	var openErrors []error
	for range openerCount {
		select {
		case openErr, ok := <-results:
			require.True(t, ok)
			openErrors = append(openErrors, openErr)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.NoError(t, errors.Join(openErrors...))

	store := openTestStore(t, ctx, storePath)
	requireSchemaMajorVersion(t, ctx, store, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, store, currentSchemaMinorVersion)
}

// Verifies that Open waits for the initialization lock before creating or configuring the database.
func TestOpenWaitsForInitializationLockBeforeCreatingDatabase(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := initializationTestPath(t)
	initializationLock, newLockErr := lockfile.NewLockfile(storePath + ".migrate.lock")
	require.NoError(t, newLockErr)
	require.NoError(t, initializationLock.TryLock(ctx, lockfile.DefaultLockRetryInterval))
	defer func() {
		require.NoError(t, initializationLock.Close())
	}()

	waitCtx, cancelWait := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelWait()
	started := time.Now()
	store, openErr := Open(waitCtx, Options{Path: storePath, BusyTimeout: 25 * time.Millisecond})
	if store != nil {
		require.NoError(t, store.Close())
	}
	t.Logf("Open while initialization lock held: elapsed=%s, error=%v", time.Since(started), openErr)
	require.ErrorIs(t, openErr, context.DeadlineExceeded)
	require.NoFileExists(t, storePath)
}

// Verifies that Open retries a non-cooperating SQLite lock holder and succeeds after an observed BUSY attempt.
func TestOpenRetriesSQLiteContentionUntilReleased(t *testing.T) {
	for _, beginStatement := range []string{"BEGIN IMMEDIATE", "BEGIN EXCLUSIVE"} {
		t.Run(beginStatement, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
			defer cancel()
			storePath := initializationTestPath(t)
			releaseSQLiteLock := holdSQLiteTransactionLock(t, ctx, storePath, beginStatement)
			retryObserved := make(chan struct{}, 1)
			log := funcr.New(func(_, _ string) {
				select {
				case retryObserved <- struct{}{}:
				default:
				}
			}, funcr.Options{Verbosity: 1})
			const busyTimeout = DefaultBusyTimeout
			pending := startStoreOpen(t, ctx, Options{Path: storePath, BusyTimeout: busyTimeout, Log: log})

			select {
			case <-retryObserved:
				releaseSQLiteLock()
			case <-pending.done:
				t.Fatalf("Open returned before the SQLite lock was released: %v", pending.err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case <-pending.done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			require.NoError(t, pending.err)
			requireSchemaMajorVersion(t, ctx, pending.store, currentSchemaMajorVersion)
			requireSchemaMinorVersion(t, ctx, pending.store, currentSchemaMinorVersion)
			requireSQLiteBusyTimeout(t, ctx, pending.store, busyTimeout)

			pending.store.db.SetMaxIdleConns(0)
			pending.store.db.SetMaxIdleConns(1)
			requireSQLiteBusyTimeout(t, ctx, pending.store, busyTimeout)
		})
	}
}

// Verifies that Open exhausts its contention budget before the caller context while retaining the typed BUSY error.
func TestOpenExhaustsSQLiteContentionBudget(t *testing.T) {
	for _, beginStatement := range []string{"BEGIN IMMEDIATE", "BEGIN EXCLUSIVE"} {
		t.Run(beginStatement, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
			defer cancel()
			storePath := initializationTestPath(t)
			releaseSQLiteLock := holdSQLiteTransactionLock(t, ctx, storePath, beginStatement)
			retryCount := 0
			log := funcr.New(func(_, _ string) {
				retryCount++
			}, funcr.Options{Verbosity: 1})
			const busyTimeout = 200 * time.Millisecond
			started := time.Now()
			store, openErr := Open(ctx, Options{Path: storePath, BusyTimeout: busyTimeout, Log: log})
			elapsed := time.Since(started)
			if store != nil {
				require.NoError(t, store.Close())
			}
			t.Logf("Persistent %s: budget=%s, elapsed=%s, retries=%d, error=%v",
				beginStatement, busyTimeout, elapsed, retryCount, openErr)
			require.Nil(t, store)
			require.ErrorIs(t, openErr, context.DeadlineExceeded)
			require.Equal(t, 1, strings.Count(openErr.Error(), context.DeadlineExceeded.Error()))
			var sqliteErr *sqlite.Error
			require.ErrorAs(t, openErr, &sqliteErr)
			require.Equal(t, sqlite3.SQLITE_BUSY, sqliteErr.Code()&0xff)
			require.NoError(t, ctx.Err())

			releaseSQLiteLock()
			recovered := openTestStore(t, ctx, storePath)
			requireSchemaMajorVersion(t, ctx, recovered, currentSchemaMajorVersion)
		})
	}
}

// Verifies that Open stops context-aware backoff when canceled after a real SQLite BUSY attempt.
func TestOpenCancelsSQLiteContentionRetry(t *testing.T) {
	for _, beginStatement := range []string{"BEGIN IMMEDIATE", "BEGIN EXCLUSIVE"} {
		t.Run(beginStatement, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
			defer cancel()
			storePath := initializationTestPath(t)
			releaseSQLiteLock := holdSQLiteTransactionLock(t, ctx, storePath, beginStatement)
			retryObserved := make(chan struct{}, 1)
			log := funcr.New(func(_, _ string) {
				select {
				case retryObserved <- struct{}{}:
				default:
				}
			}, funcr.Options{Verbosity: 1})
			waitCtx, cancelWait := context.WithCancel(ctx)
			defer cancelWait()
			// A native busy wait must outlast the test context, so completion proves it was not used.
			pending := startStoreOpen(t, waitCtx, Options{Path: storePath, BusyTimeout: time.Hour, Log: log})

			select {
			case <-retryObserved:
			case <-pending.done:
				t.Fatalf("Open returned before cancellation: %v", pending.err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			cancelWait()
			select {
			case <-pending.done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			require.Nil(t, pending.store)
			require.ErrorIs(t, pending.err, context.Canceled)
			require.True(t, isSQLiteBusy(pending.err))
			require.NoError(t, ctx.Err())

			releaseSQLiteLock()
			openTestStore(t, ctx, storePath)
		})
	}
}

// Verifies that Open retains the actual SQLite BUSY error if cancellation occurs before backoff can record it.
func TestOpenPreservesBusyErrorWhenCanceledBeforeBackoff(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := initializationTestPath(t)
	holdSQLiteTransactionLock(t, ctx, storePath, "BEGIN IMMEDIATE")
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	log := funcr.New(func(_, _ string) {
		cancelWait()
	}, funcr.Options{Verbosity: 1})

	store, openErr := Open(waitCtx, Options{Path: storePath, Log: log})
	if store != nil {
		require.NoError(t, store.Close())
	}
	require.Nil(t, store)
	require.ErrorIs(t, openErr, context.Canceled)
	require.True(t, isSQLiteBusy(openErr), "the BUSY error must survive cancellation before retry notification")
}

// Verifies that Open starts its retry budget after acquiring the initialization lock, even after a longer lock wait.
func TestOpenInitializesAfterLongInitializationLockWait(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := initializationTestPath(t)
	initializationLock, newLockErr := lockfile.NewLockfile(storePath + initializationLockFileSuffix)
	require.NoError(t, newLockErr)
	require.NoError(t, initializationLock.TryLock(ctx, lockfile.DefaultLockRetryInterval))
	t.Cleanup(func() {
		require.NoError(t, initializationLock.Close())
	})

	const busyTimeout = time.Second
	pending := startStoreOpen(t, ctx, Options{Path: storePath, BusyTimeout: busyTimeout})
	holdTimer := time.NewTimer(2 * busyTimeout)
	defer holdTimer.Stop()
	select {
	case <-holdTimer.C:
	case <-pending.done:
		t.Fatalf("Open returned while the initialization lock was held: %v", pending.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoFileExists(t, storePath)
	require.NoError(t, initializationLock.Close())

	select {
	case <-pending.done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, pending.err)
	requireSQLiteBusyTimeout(t, ctx, pending.store, busyTimeout)
	requireSchemaMajorVersion(t, ctx, pending.store, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, pending.store, currentSchemaMinorVersion)
}

// Verifies that Open keeps the lock throughout schema recovery without applying the initialization retry deadline to migrations.
func TestOpenHoldsInitializationLockThroughoutSchemaRecovery(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := initializationTestPath(t)
	seedStore := openTestStore(t, ctx, storePath)
	_, dirtyErr := seedStore.db.ExecContext(ctx, "UPDATE schema_minor_migrations_v1 SET dirty = 1")
	require.NoError(t, dirtyErr)
	require.NoError(t, seedStore.Close())

	recoveryObserved := make(chan struct{}, 1)
	resumeRecovery := make(chan struct{})
	log := funcr.New(func(_, _ string) {
		select {
		case recoveryObserved <- struct{}{}:
		default:
		}
		select {
		case <-resumeRecovery:
		case <-ctx.Done():
		}
	}, funcr.Options{Verbosity: 1})
	const busyTimeout = time.Second
	pending := startStoreOpen(t, ctx, Options{Path: storePath, BusyTimeout: busyTimeout, Log: log})
	select {
	case <-recoveryObserved:
	case <-pending.done:
		t.Fatalf("Open returned before schema recovery was observed: %v", pending.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	holdTimer := time.NewTimer(2 * busyTimeout)
	defer holdTimer.Stop()

	competingCtx, cancelCompeting := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelCompeting()
	competingStore, competingErr := Open(competingCtx, Options{Path: storePath, BusyTimeout: busyTimeout})
	if competingStore != nil {
		require.NoError(t, competingStore.Close())
	}
	require.Nil(t, competingStore)
	require.ErrorIs(t, competingErr, context.DeadlineExceeded)
	require.ErrorContains(t, competingErr, "could not acquire state store initialization lock")
	select {
	case <-holdTimer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(resumeRecovery)
	select {
	case <-pending.done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, pending.err)
	requireSchemaMajorVersion(t, ctx, pending.store, currentSchemaMajorVersion)
	requireSchemaMinorVersion(t, ctx, pending.store, currentSchemaMinorVersion)
}

// Verifies that Open does not retry permanent SQLite errors and releases failed initialization resources.
func TestOpenDoesNotRetryPermanentSQLiteErrors(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		directory    bool
		expectedCode int
	}{
		{name: "cannot open", directory: true, expectedCode: sqlite3.SQLITE_CANTOPEN},
		{name: "invalid database", expectedCode: sqlite3.SQLITE_NOTADB},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
			defer cancel()
			storePath := initializationTestPath(t)
			if testCase.directory {
				require.NoError(t, os.Mkdir(storePath, osutil.PermissionOnlyOwnerReadWriteTraverse))
			} else {
				require.NoError(t, usvc_io.WriteFile(storePath, []byte("not a SQLite database"), osutil.PermissionOnlyOwnerReadWrite))
			}
			retryCount := 0
			log := funcr.New(func(_, _ string) {
				retryCount++
			}, funcr.Options{Verbosity: 1})

			store, openErr := Open(ctx, Options{Path: storePath, BusyTimeout: time.Second, Log: log})
			if store != nil {
				require.NoError(t, store.Close())
			}
			require.Nil(t, store)
			var sqliteErr *sqlite.Error
			require.ErrorAs(t, openErr, &sqliteErr)
			require.Equal(t, testCase.expectedCode, sqliteErr.Code())
			require.False(t, isSQLiteBusy(openErr))
			require.Zero(t, retryCount)

			require.NoError(t, os.Remove(storePath))
			openTestStore(t, ctx, storePath)
		})
	}
}

// Verifies that Open normalizes BusyTimeout and preserves it when SQLite replaces an idle connection.
func TestOpenPreservesNormalOperationBusyTimeout(t *testing.T) {
	for _, testCase := range []struct {
		busyTimeout time.Duration
		expected    time.Duration
	}{
		{busyTimeout: 0, expected: DefaultBusyTimeout},
		{busyTimeout: -time.Second, expected: DefaultBusyTimeout},
		{busyTimeout: 750 * time.Millisecond, expected: 750 * time.Millisecond},
		{busyTimeout: 500 * time.Microsecond, expected: time.Millisecond},
	} {
		t.Run(testCase.busyTimeout.String(), func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
			defer cancel()
			store, openErr := Open(ctx, Options{Path: initializationTestPath(t), BusyTimeout: testCase.busyTimeout})
			require.NoError(t, openErr)
			t.Cleanup(func() {
				require.NoError(t, store.Close())
			})
			requireSQLiteBusyTimeout(t, ctx, store, testCase.expected)
			store.db.SetMaxIdleConns(0)
			store.db.SetMaxIdleConns(1)
			requireSQLiteBusyTimeout(t, ctx, store, testCase.expected)
		})
	}
}

// Verifies that Open can reopen a current WAL store while another connection has an uncommitted write transaction.
func TestOpenWarmStoreWithActiveSQLiteWriter(t *testing.T) {
	for _, beginStatement := range []string{"BEGIN IMMEDIATE", "BEGIN EXCLUSIVE"} {
		t.Run(beginStatement, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
			defer cancel()
			storePath := initializationTestPath(t)
			seed := openTestStore(t, ctx, storePath)
			require.NoError(t, seed.Close())
			writer := openRawSQLiteDB(t, ctx, storePath)
			conn, connErr := writer.Conn(ctx)
			require.NoError(t, connErr)
			t.Cleanup(func() {
				_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
				require.NoError(t, rollbackErr)
				require.NoError(t, conn.Close())
				require.NoError(t, writer.Close())
			})
			_, beginErr := conn.ExecContext(ctx, beginStatement)
			require.NoError(t, beginErr)
			_, writeErr := conn.ExecContext(ctx, "UPDATE schema_minor_migrations_v1 SET dirty = dirty")
			require.NoError(t, writeErr)

			reopened := openTestStore(t, ctx, storePath)
			requireSchemaMajorVersion(t, ctx, reopened, currentSchemaMajorVersion)
			requireSchemaMinorVersion(t, ctx, reopened, currentSchemaMinorVersion)
		})
	}
}

// Verifies that isSQLiteBusy recognizes wrapped extended BUSY codes but rejects permanent and string-only errors.
func TestIsSQLiteBusyRecognizesExtendedSnapshotError(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stateStoreTestTimeout)
	defer cancel()
	storePath := initializationTestPath(t)
	store := openTestStore(t, ctx, storePath)
	_, createErr := store.db.ExecContext(ctx, "CREATE TABLE snapshot_contention (value INTEGER)")
	require.NoError(t, createErr)
	conn, connErr := store.db.Conn(ctx)
	require.NoError(t, connErr)
	t.Cleanup(func() {
		_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
		require.NoError(t, rollbackErr)
		require.NoError(t, conn.Close())
	})
	_, beginErr := conn.ExecContext(ctx, "BEGIN")
	require.NoError(t, beginErr)
	var rowCount int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM snapshot_contention").Scan(&rowCount))

	writer := openRawSQLiteDB(t, ctx, storePath)
	t.Cleanup(func() {
		require.NoError(t, writer.Close())
	})
	_, writeErr := writer.ExecContext(ctx, "INSERT INTO snapshot_contention VALUES (1)")
	require.NoError(t, writeErr)
	_, snapshotErr := conn.ExecContext(ctx, "INSERT INTO snapshot_contention VALUES (2)")
	var sqliteErr *sqlite.Error
	require.ErrorAs(t, snapshotErr, &sqliteErr)
	require.Equal(t, sqlite3.SQLITE_BUSY_SNAPSHOT, sqliteErr.Code())
	require.True(t, isSQLiteBusy(fmt.Errorf("wrapped SQLite failure: %w", snapshotErr)))

	_, invalidSQLErr := writer.ExecContext(ctx, "SELECT FROM snapshot_contention")
	require.Error(t, invalidSQLErr)
	require.False(t, isSQLiteBusy(invalidSQLErr))
	require.False(t, isSQLiteBusy(errors.New("database is locked (5) (SQLITE_BUSY)")))
	require.False(t, isSQLiteBusy(nil))
}
