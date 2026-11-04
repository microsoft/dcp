/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/resiliency"
)

// Verifies that rejected cleanup after host cancellation does not fall back to inline execution.
func TestOutputCleanupAfterShutdownDoesNotRunInline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	queue := resiliency.NewWorkQueue(ctx, 1)
	var called atomic.Bool
	enqueueOutputCleanup(queue, logr.Discard(), func(context.Context) error {
		called.Store(true)
		return nil
	})
	require.False(t, called.Load())
}
