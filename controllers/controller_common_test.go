// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that callWithRetryAndVerification() can be stopped from retrying by returning a permanent error
func TestCallWithRetryAndVerificationPermanentError(t *testing.T) {
	t.Parallel()

	const timeout = 10 * time.Second
	ctx, cancel := testutil.GetTestContext(t, timeout)
	defer cancel()

	attempt := 0
	start := time.Now()
	permanentError := errors.New("permanent error")
	temporaryError := errors.New("temporary error")

	_, err := callWithRetryAndVerification(
		ctx,
		exponentialBackoff(timeout),
		func(_ context.Context) error { return nil },
		func(_ context.Context) (string, error) {
			attempt++
			if attempt == 2 {
				return "", resiliency.Permanent(permanentError)
			}
			return "", temporaryError
		},
	)

	require.Error(t, err)
	require.ErrorIs(t, err, permanentError)
	require.Equal(t, 2, attempt)
	require.WithinRangef(t, time.Now(), start, start.Add(timeout/2), "the call should have stopped after the second attmept, much sooner than the timeout")
}
