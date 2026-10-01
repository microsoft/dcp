/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
)

// Verifies that a final descendant stop failure preserves the platform error
// and classifies the overall process-tree result as incomplete.
func TestJoinProcessTreeStopErrorsClassifiesDescendantFailure(t *testing.T) {
	t.Parallel()

	childErr := errors.New("child stop failed")
	stopErr := joinProcessTreeStopErrors(nil, nil, nil, []error{childErr})

	require.ErrorIs(t, stopErr, ErrIncompleteProcessTree)
	require.ErrorIs(t, stopErr, childErr)
}

// Verifies that root-exit confirmation returns immediately when a fatal stop
// failure did not produce a wait channel.
func TestWaitForProcessStopConfirmationSkipsMissingWaitChannel(t *testing.T) {
	t.Parallel()

	fatalStopErr := errors.New("root stop failed")
	waitErr := waitForProcessStopConfirmation(context.Background(), nil, fatalStopErr, processStopTimeout)

	require.NoError(t, waitErr)
}

// Verifies that tracked process exit waiting does not return before the shared
// wait state reports completion.
func TestWaitForTrackedProcessExitWaitsForCompletion(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	waitState := &waitState{waitEndedCh: make(chan struct{})}
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- waitForTrackedProcessExit(testCtx, 42, waitState, 0)
	}()

	select {
	case waitErr := <-waitResult:
		require.Fail(t, "wait returned before process exit", "error: %v", waitErr)
	default:
	}

	close(waitState.waitEndedCh)
	select {
	case waitErr := <-waitResult:
		require.NoError(t, waitErr)
	case <-testCtx.Done():
		t.Fatal("timed out waiting for tracked process completion")
	}
}
