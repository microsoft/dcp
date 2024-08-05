// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/microsoft/usvc-apiserver/pkg/testutil"
	"github.com/stretchr/testify/require"
)

var (
	errCouldNotDoIt    = errors.New("just could not do it")
	defaultTestTimeout = 10 * time.Second
)

// Tests normal use case for bufferedNetworkStream() (deadline on read)
func TestBufferedNetworkStreamDeadlineExceededOnRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	incoming := newTestReaderWriter([]ioResult{{20, nil}, {0, os.ErrDeadlineExceeded}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{20, nil}})

	stream := StreamNetworkData(ctx, 1024, incoming, outgoing, 3*time.Second)

	require.Error(t, stream.ReadError, io.EOF)
	require.NoError(t, stream.WriteError)
	require.Equal(t, int64(20), stream.BytesRead)
	require.Equal(t, int64(20), stream.BytesWritten)
}

// Tests normal use case for bufferedNetworkStream() (deadline on write)
// We expect the write to be retried.
func TestBufferedNetworkStreamDeadlineExceededOnWrite(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, nil}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {0, os.ErrDeadlineExceeded}, {10, nil}})

	stream := StreamNetworkData(ctx, 1024, incoming, outgoing, 3*time.Second)

	require.Error(t, stream.ReadError, io.EOF)
	require.NoError(t, stream.WriteError)
	require.Equal(t, int64(20), stream.BytesRead)
	require.Equal(t, int64(20), stream.BytesWritten)
}

// Tests invalid write handling (more data written than read)
func TestBufferedNetworkStreamWrittenMoreThanRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, nil}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {20, nil}})

	stream := StreamNetworkData(ctx, 1024, incoming, outgoing, 3*time.Second)

	require.NoError(t, stream.ReadError)
	require.Error(t, stream.WriteError, errInvalidWrite)
	require.Equal(t, int64(20), stream.BytesRead)
	require.Equal(t, int64(30), stream.BytesWritten)
}

// Less data written than read
func TestBufferedNetworkStreamShortWrite(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{5, nil}})

	stream := StreamNetworkData(ctx, 1024, incoming, outgoing, 3*time.Second)

	require.NoError(t, stream.ReadError)
	require.Error(t, stream.WriteError, io.ErrShortWrite)
	require.Equal(t, int64(10), stream.BytesRead)
	require.Equal(t, int64(5), stream.BytesWritten)
}

// Deadline exceeded on read, but the read also returned some data.
func TestBufferedNetworkStreamDeadlineExceededWithReadData(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, os.ErrDeadlineExceeded}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {10, nil}})

	stream := StreamNetworkData(ctx, 1024, incoming, outgoing, 3*time.Second)

	require.Error(t, stream.ReadError, io.EOF)
	require.NoError(t, stream.WriteError)
	require.Equal(t, int64(20), stream.BytesRead)
	require.Equal(t, int64(20), stream.BytesWritten)
}

// Deadline exceeded on write, but the write was also partially successful.
func TestBufferedNetworkStreamDeadlineExceededPartialWrite(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, nil}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {5, os.ErrDeadlineExceeded}, {5, nil}})

	stream := StreamNetworkData(ctx, 1024, incoming, outgoing, 3*time.Second)

	require.Error(t, stream.ReadError, io.EOF)
	require.NoError(t, stream.WriteError)
	require.Equal(t, int64(20), stream.BytesRead)
	require.Equal(t, int64(20), stream.BytesWritten)
}

// Deadline exceeded on read and on write, but the write retry is successful.
func TestBufferedNetworkStreamDoubleDeadlineExceeded(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, os.ErrDeadlineExceeded}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {0, os.ErrDeadlineExceeded}, {10, nil}})

	stream := StreamNetworkData(ctx, 1024, incoming, outgoing, 3*time.Second)

	require.Error(t, stream.ReadError, io.EOF)
	require.NoError(t, stream.WriteError)
	require.Equal(t, int64(20), stream.BytesRead)
	require.Equal(t, int64(20), stream.BytesWritten)
}

// Deadline exceeded on read and on write, and the retry also ends with a deadline exceeded.
func TestBufferedNetworkStreamTripleDeadlineExceeded(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, os.ErrDeadlineExceeded}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {0, os.ErrDeadlineExceeded}, {0, os.ErrDeadlineExceeded}})

	stream := StreamNetworkData(ctx, 1024, incoming, outgoing, 3*time.Second)

	require.NoError(t, stream.ReadError)
	require.Error(t, stream.WriteError, os.ErrDeadlineExceeded)
	require.Equal(t, int64(20), stream.BytesRead)
	require.Equal(t, int64(10), stream.BytesWritten)
}

// Read returns an error other than deadline exceeded.
func TestBufferedNetworkStreamReadError(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	incoming := newTestReaderWriter([]ioResult{{20, nil}, {0, errCouldNotDoIt}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{20, nil}})

	stream := StreamNetworkData(ctx, 1024, incoming, outgoing, 3*time.Second)

	require.ErrorIs(t, stream.ReadError, errCouldNotDoIt)
	require.NoError(t, stream.WriteError)
	require.Equal(t, int64(20), stream.BytesRead)
	require.Equal(t, int64(20), stream.BytesWritten)
}

// Write returns an error other than deadline exceeded.
func TestBufferedNetworkStreamWriteError(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, nil}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {0, errCouldNotDoIt}})

	stream := StreamNetworkData(ctx, 1024, incoming, outgoing, 3*time.Second)

	require.NoError(t, stream.ReadError)
	require.ErrorIs(t, stream.WriteError, errCouldNotDoIt)
	require.Equal(t, int64(20), stream.BytesRead)
	require.Equal(t, int64(10), stream.BytesWritten)
}

// Read returns an error other than deadline exceeded, but also some data is read.
func TestBufferedNetworkStreamPartialReadWithError(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, errCouldNotDoIt}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {10, nil}})

	stream := StreamNetworkData(ctx, 1024, incoming, outgoing, 3*time.Second)

	require.ErrorIs(t, stream.ReadError, errCouldNotDoIt)
	require.NoError(t, stream.WriteError)
	require.Equal(t, int64(20), stream.BytesRead)
	require.Equal(t, int64(20), stream.BytesWritten)
}

// Write returns an error other than deadline exceeded, but also some data is written.
func TestBufferedNetworkStreamPartialWriteWithError(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, nil}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {5, errCouldNotDoIt}})

	stream := StreamNetworkData(ctx, 1024, incoming, outgoing, 3*time.Second)

	require.NoError(t, stream.ReadError)
	require.ErrorIs(t, stream.WriteError, errCouldNotDoIt)
	require.Equal(t, int64(20), stream.BytesRead)
	require.Equal(t, int64(15), stream.BytesWritten)
}
