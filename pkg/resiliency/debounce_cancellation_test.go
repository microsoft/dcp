/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

func runDebounceTest(t *testing.T, run func(*testing.T, context.Context)) {
	t.Helper()
	testCtx, cancelTest := testutil.GetTestContext(t, 0)
	deadline, hasDeadline := testCtx.Deadline()
	budget := time.Until(deadline)
	cancelTest()
	synctest.Test(t, func(test *testing.T) {
		if !hasDeadline {
			run(test, test.Context())
			return
		}
		ctx, cancel := context.WithTimeout(test.Context(), budget)
		defer cancel()
		run(test, ctx)
	})
}

// Verifies that cancelling a pending debounce returns cancellation without executing the runner.
func TestReturnsErrorIfContextCancelled(t *testing.T) {
	t.Parallel()
	runDebounceTest(t, func(test *testing.T, testCtx context.Context) {
		ctx, cancel := context.WithCancel(testCtx)
		defer cancel()
		called := false
		deb := resiliency.NewDebounceLast(func() (int, error) { called = true; return 7, nil }, time.Second, 2*time.Second)
		result := make(chan error, 1)
		go func() {
			_, runErr := deb.Run(ctx)
			result <- runErr
		}()
		synctest.Wait()
		cancel()
		select {
		case runErr := <-result:
			require.ErrorIs(test, runErr, context.Canceled)
		case <-testCtx.Done():
			test.Fatal("pending debounce did not return after cancellation")
		}
		synctest.Wait()
		require.False(test, called)
	})
}

// Verifies that cancelling a pending debounce action prevents runner execution.
func TestDebounceActionDoesNotExecuteRunnerIfContextCancelled(t *testing.T) {
	t.Parallel()
	runDebounceTest(t, func(test *testing.T, testCtx context.Context) {
		ctx, cancel := context.WithCancel(testCtx)
		defer cancel()
		called := false
		deb := resiliency.NewDebounceLastAction(func() { called = true }, time.Second, 2*time.Second)
		deb.Run(ctx)
		synctest.Wait()
		cancel()
		synctest.Wait()
		require.False(test, called)
	})
}
