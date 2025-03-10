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

	silenceForever    = ioResult{0, errors.Join(os.ErrDeadlineExceeded, testErrRepeatForever)}
	nothingButSilence = []ioResult{silenceForever}
)

// East-to-west, single data transfer with timeout followed by EOF
// No errors on write.
func TestNetworkStreamDeadlineExceededOnRead(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	bufferPool := newGenericPool(func() []byte {
		return make([]byte, 1024)
	})

	east := newTestReaderWriter(
		[]ioResult{{20, nil}, {0, os.ErrDeadlineExceeded}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{20, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west, bufferPool)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(20), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.NoError(t, westResult.WriteError)
}

// Two data transfers with timeout on write.
func TestNetworkStreamDeadlineExceededOnWrite(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	bufferPool := newGenericPool(func() []byte {
		return make([]byte, 1024)
	})

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {0, os.ErrDeadlineExceeded}, {10, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west, bufferPool)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(20), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.NoError(t, westResult.WriteError)
}

// Tests invalid write handling (more data written than read)
func TestNetworkStreamWrittenMoreThanRead(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	bufferPool := newGenericPool(func() []byte {
		return make([]byte, 1024)
	})

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {20, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west, bufferPool)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(30), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	expected := ErrInconsistentWrite{}
	require.ErrorAs(t, westResult.WriteError, &expected)
}

// Less data written than read
func TestNetworkStreamShortWrite(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	bufferPool := newGenericPool(func() []byte {
		return make([]byte, 1024)
	})

	east := newTestReaderWriter(
		[]ioResult{{10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{5, nil}, {0, io.EOF}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west, bufferPool)

	require.Equal(t, int64(10), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(5), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.ErrorIs(t, westResult.WriteError, io.ErrShortWrite)
}

// Deadline exceeded on read, but the read also returned some data.
func TestNetworkStreamDeadlineExceededWithReadData(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	bufferPool := newGenericPool(func() []byte {
		return make([]byte, 1024)
	})

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, os.ErrDeadlineExceeded}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {10, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west, bufferPool)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(20), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.NoError(t, westResult.WriteError)
}

// Deadline exceeded on write, but the write was also partially successful.
func TestNetworkStreamDeadlineExceededPartialWrite(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	bufferPool := newGenericPool(func() []byte {
		return make([]byte, 1024)
	})

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {5, os.ErrDeadlineExceeded}, {5, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west, bufferPool)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(20), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.NoError(t, westResult.WriteError)
}

// Deadline exceeded on read and on write, but the write retry is successful.
func TestNetworkStreamDoubleDeadlineExceeded(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	bufferPool := newGenericPool(func() []byte {
		return make([]byte, 1024)
	})

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, os.ErrDeadlineExceeded}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {0, os.ErrDeadlineExceeded}, {10, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west, bufferPool)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(20), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.NoError(t, westResult.WriteError)
}

// Deadline exceeded on read and on write, and the retry also ends with a deadline exceeded.
func TestNetworkStreamTripleDeadlineExceeded(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	bufferPool := newGenericPool(func() []byte {
		return make([]byte, 1024)
	})

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, os.ErrDeadlineExceeded}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {0, os.ErrDeadlineExceeded}, {0, os.ErrDeadlineExceeded}, {0, io.EOF}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west, bufferPool)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(10), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.Error(t, westResult.WriteError, io.EOF)
}

// Read returns an error other than deadline exceeded.
func TestNetworkStreamReadError(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	bufferPool := newGenericPool(func() []byte {
		return make([]byte, 1024)
	})

	east := newTestReaderWriter(
		[]ioResult{{20, nil}, {0, errCouldNotDoIt}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{20, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west, bufferPool)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.Error(t, eastResult.ReadError, errCouldNotDoIt)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(20), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.NoError(t, westResult.WriteError)
}

// Write returns an error other than deadline exceeded.
func TestNetworkStreamWriteError(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	bufferPool := newGenericPool(func() []byte {
		return make([]byte, 1024)
	})

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {0, errCouldNotDoIt}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west, bufferPool)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(10), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.Error(t, westResult.WriteError, errCouldNotDoIt)
}

// Read returns an error other than deadline exceeded, but also some data is read.
func TestNetworkStreamPartialReadWithError(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	bufferPool := newGenericPool(func() []byte {
		return make([]byte, 1024)
	})

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, errCouldNotDoIt}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {10, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west, bufferPool)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.Error(t, eastResult.ReadError, errCouldNotDoIt)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(20), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.NoError(t, westResult.WriteError)
}

// Write returns an error other than deadline exceeded, but also some data is written.
func TestNetworkStreamPartialWriteWithError(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	bufferPool := newGenericPool(func() []byte {
		return make([]byte, 1024)
	})

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {5, errCouldNotDoIt}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west, bufferPool)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(15), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.Error(t, westResult.WriteError, errCouldNotDoIt)
}
