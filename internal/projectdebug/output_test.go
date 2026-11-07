/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that detaching during a partially consumed stdout write releases the producer without
// a pipe error and subsequent writes discard output, while natural completion still delivers EOF.
func TestProjectDebugOutputDetachUnblocksProducer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	output := newProjectDebugOutput()
	defer output.Close()
	type result struct {
		count int
		err   error
	}
	written := make(chan result, 1)
	go func() {
		count, writeErr := output.Write([]byte("final-output"))
		written <- result{count, writeErr}
	}()
	first := make([]byte, 1)
	_, readErr := io.ReadFull(output.reader, first)
	require.NoError(t, readErr)
	require.Equal(t, "f", string(first))
	require.NoError(t, output.detach())
	select {
	case completed := <-written:
		require.NoError(t, completed.err)
		require.Equal(t, len("final-output"), completed.count)
	case <-ctx.Done():
		t.Fatal("detaching did not unblock stdout")
	}
	count, writeErr := output.Write([]byte("discarded"))
	require.NoError(t, writeErr)
	require.Equal(t, len("discarded"), count)
}

// Verifies that normal adapter completion preserves every buffered stdout byte before EOF.
func TestProjectDebugOutputCompletionDrains(t *testing.T) {
	t.Parallel()
	output := newProjectDebugOutput()
	defer func() { require.NoError(t, output.detach()) }()
	go func() {
		_, _ = output.Write([]byte("final DAP results"))
		_ = output.Close()
	}()
	payload, readErr := io.ReadAll(output.reader)
	require.NoError(t, readErr)
	require.Equal(t, "final DAP results", string(payload))
}
