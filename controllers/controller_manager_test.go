/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	clientgorest "k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_cache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	ctrl_manager "sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/microsoft/dcp/pkg/testutil"
)

const managerStartupValueKey ControllerContextOption = "manager-startup-test"

type managerStartupTest struct {
	ctrl_manager.Manager
	elected chan struct{}
	started chan context.Context
	run     func(context.Context) error
}

type managerStartupGateCache struct {
	*informertest.FakeInformers
	started          chan context.Context
	cacheSyncStarted chan struct{}
	releaseCacheSync chan struct{}
}

func (mgr *managerStartupTest) Start(ctx context.Context) error {
	mgr.started <- ctx
	return mgr.run(ctx)
}

func (mgr *managerStartupTest) Elected() <-chan struct{} {
	return mgr.elected
}

func (startupCache *managerStartupGateCache) Start(ctx context.Context) error {
	startupCache.started <- ctx
	<-ctx.Done()
	return nil
}

func (startupCache *managerStartupGateCache) WaitForCacheSync(ctx context.Context) bool {
	close(startupCache.cacheSyncStarted)
	select {
	case <-startupCache.releaseCacheSync:
		return true
	case <-ctx.Done():
		return false
	}
}

// Verifies that NewControllerManagerOptions retains context values without inheriting cancellation or deadlines and disables leader election.
func TestControllerManagerOptionsDetachCancellation(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	valueCtx := context.WithValue(testCtx, managerStartupValueKey, "startup")
	expiredCtx, cancelExpired := context.WithDeadline(valueCtx, time.Now())
	defer cancelExpired()

	options := NewControllerManagerOptions(expiredCtx, runtime.NewScheme(), logr.Discard())
	baseCtx := options.BaseContext()

	require.False(t, options.LeaderElection)
	require.Equal(t, "startup", baseCtx.Value(managerStartupValueKey))
	require.Nil(t, baseCtx.Done())
	require.NoError(t, baseCtx.Err())
	require.Nil(t, context.Cause(baseCtx))
	_, hasDeadline := baseCtx.Deadline()
	require.False(t, hasDeadline)
}

// Verifies that StartControllerManager defers cancellation until a real manager completes cache readiness and then shuts down its cache.
func TestControllerManagerStartupUsesRealManagerCacheReadiness(t *testing.T) {
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	lifetimeCtx, cancelLifetime := context.WithCancel(testCtx)
	defer cancelLifetime()

	startupCache := &managerStartupGateCache{
		FakeInformers:    &informertest.FakeInformers{},
		started:          make(chan context.Context, 1),
		cacheSyncStarted: make(chan struct{}),
		releaseCacheSync: make(chan struct{}),
	}
	var releaseCacheSyncOnce sync.Once
	releaseCacheSync := func() {
		releaseCacheSyncOnce.Do(func() {
			close(startupCache.releaseCacheSync)
		})
	}
	defer releaseCacheSync()

	options := NewControllerManagerOptions(lifetimeCtx, runtime.NewScheme(), logr.Discard())
	options.MapperProvider = func(*clientgorest.Config, *http.Client) (apimeta.RESTMapper, error) {
		return apimeta.NewDefaultRESTMapper(nil), nil
	}
	options.NewCache = func(*clientgorest.Config, ctrl_cache.Options) (ctrl_cache.Cache, error) {
		return startupCache, nil
	}
	mgr, managerCreateErr := ctrl.NewManager(&clientgorest.Config{Host: "https://127.0.0.1"}, options)
	require.NoError(t, managerCreateErr)

	startResult := make(chan error, 1)
	go func() {
		startResult <- StartControllerManager(lifetimeCtx, mgr)
	}()

	var cacheCtx context.Context
	select {
	case cacheCtx = <-startupCache.started:
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	select {
	case <-startupCache.cacheSyncStarted:
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	select {
	case <-mgr.Elected():
		t.Fatal("manager reported election before cache readiness")
	default:
	}

	cancelLifetime()
	require.NoError(t, cacheCtx.Err())
	select {
	case startErr := <-startResult:
		t.Fatalf("manager returned before cache readiness: %v", startErr)
	default:
	}
	select {
	case <-mgr.Elected():
		t.Fatal("manager reported election while cache readiness was blocked")
	default:
	}

	releaseCacheSync()
	select {
	case startErr := <-startResult:
		require.NoError(t, startErr)
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	select {
	case <-mgr.Elected():
	default:
		t.Fatal("manager did not report election after cache readiness")
	}
	require.ErrorIs(t, cacheCtx.Err(), context.Canceled)
}

// Verifies that StartControllerManager skips startup for already-cancelled callers and preserves expired deadline errors.
func TestControllerManagerStartupSkipsCancelledCallers(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	cancelledCtx, cancelCaller := context.WithCancel(testCtx)
	cancelCaller()
	expiredCtx, cancelExpired := context.WithDeadline(testCtx, time.Now())
	defer cancelExpired()

	for _, testCase := range []struct {
		name        string
		ctx         context.Context
		expectedErr error
	}{
		{name: "cancelled", ctx: cancelledCtx},
		{name: "deadline", ctx: expiredCtx, expectedErr: context.DeadlineExceeded},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mgr := &managerStartupTest{
				elected: make(chan struct{}),
				started: make(chan context.Context, 1),
				run:     func(context.Context) error { panic("cancelled manager must not start") },
			}
			startErr := StartControllerManager(testCase.ctx, mgr)
			require.ErrorIs(t, startErr, testCase.expectedErr)
			require.Empty(t, mgr.started)
		})
	}
}

// Verifies that StartControllerManager withholds cancellation until Elected closes, forwards later cancellation, and preserves context values and causes.
func TestControllerManagerStartupDefersCancellation(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name              string
		cancelBeforeReady bool
		cause             error
	}{
		{name: "during-startup", cancelBeforeReady: true, cause: context.Canceled},
		{name: "after-startup", cause: context.Canceled},
		{name: "monitor-error", cancelBeforeReady: true, cause: errors.New("monitor failed")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			testCtx, cancelTest := testutil.GetTestContext(t, 0)
			defer cancelTest()
			valueCtx := context.WithValue(testCtx, managerStartupValueKey, "startup")
			lifetimeCtx, cancelLifetime := context.WithCancelCause(valueCtx)
			defer cancelLifetime(nil)
			mgr := &managerStartupTest{
				elected: make(chan struct{}),
				started: make(chan context.Context, 1),
				run: func(managerCtx context.Context) error {
					select {
					case <-managerCtx.Done():
						return nil
					case <-testCtx.Done():
						return testCtx.Err()
					}
				},
			}
			startResult := make(chan error, 1)
			go func() { startResult <- StartControllerManager(lifetimeCtx, mgr) }()

			var startCtx context.Context
			select {
			case observedCtx, received := <-mgr.started:
				require.True(t, received)
				startCtx = observedCtx
			case <-testCtx.Done():
				t.Fatal(testCtx.Err())
			}
			require.Equal(t, "startup", startCtx.Value(managerStartupValueKey))
			_, hasDeadline := startCtx.Deadline()
			require.False(t, hasDeadline)

			if testCase.cancelBeforeReady {
				cancelLifetime(testCase.cause)
				require.NoError(t, startCtx.Err())
				require.Nil(t, context.Cause(startCtx))
				select {
				case prematureErr, received := <-startResult:
					t.Fatalf("manager returned before readiness: error=%v, received=%v", prematureErr, received)
				default:
				}
			}
			close(mgr.elected)
			if !testCase.cancelBeforeReady {
				cancelLifetime(testCase.cause)
			}

			select {
			case startErr, received := <-startResult:
				require.True(t, received)
				require.NoError(t, startErr)
			case <-testCtx.Done():
				t.Fatal(testCtx.Err())
			}
			require.ErrorIs(t, startCtx.Err(), context.Canceled)
			require.ErrorIs(t, context.Cause(startCtx), testCase.cause)
		})
	}
}

// Verifies that StartControllerManager defers a deadline during startup without losing its error or cancellation cause.
func TestControllerManagerStartupPreservesDeferredDeadline(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	lifetimeCtx, cancelLifetime := context.WithTimeout(testCtx, time.Second)
	defer cancelLifetime()
	mgr := &managerStartupTest{
		elected: make(chan struct{}),
		started: make(chan context.Context, 1),
		run: func(managerCtx context.Context) error {
			select {
			case <-managerCtx.Done():
				return nil
			case <-testCtx.Done():
				return testCtx.Err()
			}
		},
	}
	startResult := make(chan error, 1)
	go func() { startResult <- StartControllerManager(lifetimeCtx, mgr) }()

	var startCtx context.Context
	select {
	case observedCtx, received := <-mgr.started:
		require.True(t, received)
		startCtx = observedCtx
	case prematureErr, received := <-startResult:
		t.Fatalf("manager returned before the deadline test started: error=%v, received=%v", prematureErr, received)
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	<-lifetimeCtx.Done()
	require.ErrorIs(t, lifetimeCtx.Err(), context.DeadlineExceeded)
	require.NoError(t, startCtx.Err())
	close(mgr.elected)

	select {
	case startErr, received := <-startResult:
		require.True(t, received)
		require.ErrorIs(t, startErr, context.DeadlineExceeded)
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	require.ErrorIs(t, context.Cause(startCtx), context.DeadlineExceeded)
}

// Verifies that StartControllerManager releases forwarding work when startup returns or panics before readiness and preserves manager errors.
func TestControllerManagerStartupCleansUpEarlyReturns(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		runErr error
		panics bool
	}{
		{name: "clean"},
		{name: "error", runErr: errors.New("manager failed")},
		{name: "panic", panics: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			testCtx, cancelTest := testutil.GetTestContext(t, 0)
			defer cancelTest()
			mgr := &managerStartupTest{
				elected: make(chan struct{}),
				started: make(chan context.Context, 1),
				run: func(context.Context) error {
					if testCase.panics {
						panic("manager panic")
					}
					return testCase.runErr
				},
			}
			if testCase.panics {
				require.PanicsWithValue(t, "manager panic", func() {
					_ = StartControllerManager(testCtx, mgr)
				})
			} else {
				require.ErrorIs(t, StartControllerManager(testCtx, mgr), testCase.runErr)
			}
			select {
			case startCtx, received := <-mgr.started:
				require.True(t, received)
				require.ErrorIs(t, startCtx.Err(), context.Canceled)
			case <-testCtx.Done():
				t.Fatal(testCtx.Err())
			}
			require.NoError(t, testCtx.Err())
		})
	}
}

// Verifies that StartControllerManager preserves manager errors and releases its context when cancellation, readiness, and return race.
func TestControllerManagerStartupCancellationRaces(t *testing.T) {
	t.Parallel()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	defer cancelTest()
	managerErr := errors.New("manager failed")
	for range 64 {
		lifetimeCtx, cancelLifetime := context.WithCancelCause(testCtx)
		finishManager := make(chan struct{})
		mgr := &managerStartupTest{
			elected: make(chan struct{}),
			started: make(chan context.Context, 1),
			run: func(context.Context) error {
				select {
				case <-finishManager:
					return managerErr
				case <-testCtx.Done():
					return testCtx.Err()
				}
			},
		}
		startResult := make(chan error, 1)
		go func() { startResult <- StartControllerManager(lifetimeCtx, mgr) }()
		var startCtx context.Context
		select {
		case observedCtx, received := <-mgr.started:
			require.True(t, received)
			startCtx = observedCtx
		case <-testCtx.Done():
			t.Fatal(testCtx.Err())
		}

		var racers sync.WaitGroup
		racers.Go(func() { cancelLifetime(context.Canceled) })
		racers.Go(func() { close(mgr.elected) })
		racers.Go(func() { close(finishManager) })
		racers.Wait()

		select {
		case startErr, received := <-startResult:
			require.True(t, received)
			require.ErrorIs(t, startErr, managerErr)
		case <-testCtx.Done():
			t.Fatal(testCtx.Err())
		}
		require.ErrorIs(t, startCtx.Err(), context.Canceled)
	}
}
