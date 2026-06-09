/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestCancellableReaderUsesMaxReadSize(t *testing.T) {
	t.Parallel()

	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	reader := usvc_io.NewCancellableReader(testCtx, strings.NewReader("alphabravo"), 5)

	data, readErr := reader.Read(testCtx)
	require.NoError(t, readErr)
	require.Equal(t, "alpha", string(data))

	data, readErr = reader.Read(testCtx)
	require.NoError(t, readErr)
	require.Equal(t, "bravo", string(data))
}

func TestCancellableReaderPreservesDataAfterOperationCancellation(t *testing.T) {
	t.Parallel()

	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	inner := newControlledReader()
	reader := usvc_io.NewCancellableReader(testCtx, inner, 64)

	readCtx, cancelReadCtx := context.WithCancel(testCtx)
	firstRead := readAsync(reader, readCtx)
	firstRequest := inner.waitForRead(t, testCtx)

	cancelReadCtx()
	firstResult := waitForReadResult(t, testCtx, firstRead)
	require.Empty(t, firstResult.data)
	require.ErrorIs(t, firstResult.err, context.Canceled)

	secondRead := readAsync(reader, testCtx)
	firstRequest.complete("saved data", nil)

	secondResult := waitForReadResult(t, testCtx, secondRead)
	require.NoError(t, secondResult.err)
	require.Equal(t, "saved data", string(secondResult.data))
}

func TestCancellableReaderReturnsPartialDataWithError(t *testing.T) {
	t.Parallel()

	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	expectedErr := errors.New("test read error")
	inner := &partialErrorReader{
		data: []byte("partial"),
		err:  expectedErr,
	}
	reader := usvc_io.NewCancellableReader(testCtx, inner, 64)

	data, readErr := reader.Read(testCtx)
	require.Equal(t, "partial", string(data))
	require.ErrorIs(t, readErr, expectedErr)
}

func TestCancellableReaderReturnsLifetimeContextErrorAfterCancellation(t *testing.T) {
	t.Parallel()

	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	lifetimeCtx, cancelLifetimeCtx := context.WithCancel(testCtx)
	reader := usvc_io.NewCancellableReader(lifetimeCtx, strings.NewReader("data"), 64)

	cancelLifetimeCtx()

	data, readErr := reader.Read(testCtx)
	require.Empty(t, data)
	require.ErrorIs(t, readErr, context.Canceled)
}

func TestCancellableReaderValidation(t *testing.T) {
	t.Parallel()

	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	var nilCtx context.Context
	require.Panics(t, func() {
		usvc_io.NewCancellableReader(nilCtx, strings.NewReader("data"), 64)
	})
	require.Panics(t, func() {
		usvc_io.NewCancellableReader(testCtx, nil, 64)
	})
	require.Panics(t, func() {
		usvc_io.NewCancellableReader(testCtx, strings.NewReader("data"), 0)
	})

	reader := usvc_io.NewCancellableReader(testCtx, strings.NewReader("data"), 64)
	var nilReadCtx context.Context
	require.Panics(t, func() {
		_, _ = reader.Read(nilReadCtx)
	})
}

// TestCancellableReaderReturnsAlreadyCancelledReadContextError verifies that calling
// Read with a per-call context that is already cancelled returns the context error
// without ever calling the inner reader. This exercises the early ctx.Err() check
// when no read is pending.
func TestCancellableReaderReturnsAlreadyCancelledReadContextError(t *testing.T) {
	t.Parallel()

	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	inner := newControlledReader()
	reader := usvc_io.NewCancellableReader(testCtx, inner, 64)

	readCtx, cancelReadCtx := context.WithCancel(testCtx)
	cancelReadCtx()

	data, readErr := reader.Read(readCtx)
	require.Empty(t, data)
	require.ErrorIs(t, readErr, context.Canceled)

	// The inner reader must not have been called.
	select {
	case <-inner.readRequests:
		t.Fatalf("inner reader was called even though the per-call context was already cancelled")
	default:
	}
}

// TestCancellableReaderHandlesLifetimeCancellationDuringRead verifies that cancelling
// the lifetime context while an inner Read is blocked promptly returns a context
// error from the in-flight Read call.
func TestCancellableReaderHandlesLifetimeCancellationDuringRead(t *testing.T) {
	t.Parallel()

	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	inner := newControlledReader()
	lifetimeCtx, cancelLifetimeCtx := context.WithCancel(testCtx)
	reader := usvc_io.NewCancellableReader(lifetimeCtx, inner, 64)

	firstRead := readAsync(reader, testCtx)
	firstRequest := inner.waitForRead(t, testCtx)

	cancelLifetimeCtx()

	firstResult := waitForReadResult(t, testCtx, firstRead)
	require.Empty(t, firstResult.data)
	require.ErrorIs(t, firstResult.err, context.Canceled)

	// Unblock the inner reader so the worker goroutine can exit cleanly.
	firstRequest.complete("late data", nil)

	// Subsequent Read calls must also return the lifetime context error.
	data, readErr := reader.Read(testCtx)
	require.Empty(t, data)
	require.ErrorIs(t, readErr, context.Canceled)
}

// TestCancellableReaderHandlesMultipleConsecutiveCancellations verifies that
// multiple consecutive Read cancellations leave the reader in a consistent state
// and that a subsequent successful Read returns the data produced by the original
// inner Read call.
func TestCancellableReaderHandlesMultipleConsecutiveCancellations(t *testing.T) {
	t.Parallel()

	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	inner := newControlledReader()
	reader := usvc_io.NewCancellableReader(testCtx, inner, 64)

	// First Read starts the inner Read, then gets cancelled.
	readCtx1, cancelReadCtx1 := context.WithCancel(testCtx)
	firstRead := readAsync(reader, readCtx1)
	firstRequest := inner.waitForRead(t, testCtx)
	cancelReadCtx1()
	firstResult := waitForReadResult(t, testCtx, firstRead)
	require.Empty(t, firstResult.data)
	require.ErrorIs(t, firstResult.err, context.Canceled)

	// Second Read - with cancelled context - should also return the context error
	// while the original inner Read is still pending.
	readCtx2, cancelReadCtx2 := context.WithCancel(testCtx)
	cancelReadCtx2()
	data, readErr := reader.Read(readCtx2)
	require.Empty(t, data)
	require.ErrorIs(t, readErr, context.Canceled)

	// Third Read with a live context - should receive the data from the first
	// (originally-cancelled) inner Read.
	thirdRead := readAsync(reader, testCtx)
	firstRequest.complete("preserved", nil)
	thirdResult := waitForReadResult(t, testCtx, thirdRead)
	require.NoError(t, thirdResult.err)
	require.Equal(t, "preserved", string(thirdResult.data))

	// The inner reader must not have been called more than once: the original
	// Read result was preserved, so no extra inner Read should be issued.
	select {
	case <-inner.readRequests:
		t.Fatalf("inner reader was called more times than expected")
	default:
	}
}

// TestCancellableReaderReturnsEOF verifies the behavior when the inner reader
// returns io.EOF with no data.
func TestCancellableReaderReturnsEOF(t *testing.T) {
	t.Parallel()

	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	reader := usvc_io.NewCancellableReader(testCtx, strings.NewReader(""), 64)

	data, readErr := reader.Read(testCtx)
	require.Empty(t, data)
	require.ErrorIs(t, readErr, io.EOF)
}

// TestCancellableReaderMultipleReadsAfterPreservationCycle verifies that after
// a cancellation+preservation cycle the reader continues to operate normally
// for a sequence of subsequent reads.
func TestCancellableReaderMultipleReadsAfterPreservationCycle(t *testing.T) {
	t.Parallel()

	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	inner := newControlledReader()
	reader := usvc_io.NewCancellableReader(testCtx, inner, 64)

	// Start, cancel, then preserve the first Read's data.
	readCtx, cancelReadCtx := context.WithCancel(testCtx)
	firstRead := readAsync(reader, readCtx)
	firstRequest := inner.waitForRead(t, testCtx)
	cancelReadCtx()
	firstResult := waitForReadResult(t, testCtx, firstRead)
	require.ErrorIs(t, firstResult.err, context.Canceled)

	secondRead := readAsync(reader, testCtx)
	firstRequest.complete("first", nil)
	secondResult := waitForReadResult(t, testCtx, secondRead)
	require.NoError(t, secondResult.err)
	require.Equal(t, "first", string(secondResult.data))

	// Several subsequent normal reads should work as expected.
	for _, expected := range []string{"second", "third", "fourth"} {
		readResult := readAsync(reader, testCtx)
		request := inner.waitForRead(t, testCtx)
		request.complete(expected, nil)
		result := waitForReadResult(t, testCtx, readResult)
		require.NoError(t, result.err)
		require.Equal(t, expected, string(result.data))
	}
}

// TestCancellableReaderHandlesLifetimeCancellationWithPendingResult verifies that
// when the lifetime context is cancelled after the worker has produced a result
// but before the caller observes it, the next Read call returns the lifetime
// context error rather than blocking.
func TestCancellableReaderHandlesLifetimeCancellationAfterPreservedRead(t *testing.T) {
	t.Parallel()

	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	inner := newControlledReader()
	lifetimeCtx, cancelLifetimeCtx := context.WithCancel(testCtx)
	reader := usvc_io.NewCancellableReader(lifetimeCtx, inner, 64)

	// Start a read, cancel it, leaving readPending=true.
	readCtx, cancelReadCtx := context.WithCancel(testCtx)
	firstRead := readAsync(reader, readCtx)
	firstRequest := inner.waitForRead(t, testCtx)
	cancelReadCtx()
	firstResult := waitForReadResult(t, testCtx, firstRead)
	require.ErrorIs(t, firstResult.err, context.Canceled)

	// Cancel the lifetime context while the inner Read is still in flight.
	cancelLifetimeCtx()

	// Allow the inner Read to complete (worker should handle this gracefully).
	firstRequest.complete("ignored", nil)

	// Any subsequent Read must return the lifetime context error.
	data, readErr := reader.Read(testCtx)
	require.Empty(t, data)
	require.ErrorIs(t, readErr, context.Canceled)
}

type cancellableReaderReadResult struct {
	data []byte
	err  error
}

func readAsync(reader *usvc_io.CancellableReader, ctx context.Context) <-chan cancellableReaderReadResult {
	resultCh := make(chan cancellableReaderReadResult, 1)
	go func() {
		data, readErr := reader.Read(ctx)
		resultCh <- cancellableReaderReadResult{
			data: append([]byte(nil), data...),
			err:  readErr,
		}
	}()
	return resultCh
}

func waitForReadResult(
	t *testing.T,
	ctx context.Context,
	resultCh <-chan cancellableReaderReadResult,
) cancellableReaderReadResult {
	t.Helper()

	select {
	case result := <-resultCh:
		return result
	case <-ctx.Done():
		t.Fatalf("timed out waiting for Read result: %v", ctx.Err())
		return cancellableReaderReadResult{}
	}
}

type controlledReader struct {
	readRequests chan controlledReadRequest
}

type controlledReadRequest struct {
	resultCh chan controlledReadResult
}

type controlledReadResult struct {
	data []byte
	err  error
}

func newControlledReader() *controlledReader {
	return &controlledReader{
		readRequests: make(chan controlledReadRequest, 1),
	}
}

func (reader *controlledReader) Read(p []byte) (int, error) {
	request := controlledReadRequest{
		resultCh: make(chan controlledReadResult, 1),
	}
	reader.readRequests <- request

	result := <-request.resultCh
	n := copy(p, result.data)
	return n, result.err
}

func (reader *controlledReader) waitForRead(t *testing.T, ctx context.Context) controlledReadRequest {
	t.Helper()

	select {
	case request := <-reader.readRequests:
		return request
	case <-ctx.Done():
		t.Fatalf("timed out waiting for underlying Read call: %v", ctx.Err())
		return controlledReadRequest{}
	}
}

func (request controlledReadRequest) complete(data string, err error) {
	request.resultCh <- controlledReadResult{
		data: []byte(data),
		err:  err,
	}
}

type partialErrorReader struct {
	data []byte
	err  error
	read bool
}

func (reader *partialErrorReader) Read(p []byte) (int, error) {
	if reader.read {
		return 0, io.EOF
	}

	reader.read = true
	n := copy(p, reader.data)
	return n, reader.err
}
