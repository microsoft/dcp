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

// East-to-west, single data transfer followed by EOF
// No errors on write.
func TestNetworkStreamReadsExpectedBytes(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {10, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west)

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

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {20, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(10), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	expected := io.ErrShortWrite
	require.ErrorAs(t, westResult.WriteError, &expected)
}

// Less data written than read
func TestNetworkStreamShortWrite(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	east := newTestReaderWriter(
		[]ioResult{{10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{5, nil}, {0, io.EOF}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west)

	require.Equal(t, int64(10), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(5), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.ErrorIs(t, westResult.WriteError, io.ErrShortWrite)
}

// Partial write.
func TestNetworkStreamPartialWrite(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {5, nil}, {5, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(15), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.ErrorIs(t, westResult.WriteError, io.ErrShortWrite)
}

// EOF received mid stream processes expected data.
func TestNetworkStreamEOFMidStream(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, io.EOF}, {10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {10, io.EOF}, {10, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(20), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.NoError(t, westResult.WriteError)
}

// Write closed while data remaining to be read.
func TestNetworkStreamWriteEOFBeforeRead(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, nil}, {10, io.EOF}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {10, io.EOF}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(20), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.NoError(t, westResult.WriteError)
}

// Read returns errors.
func TestNetworkStreamReadError(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	east := newTestReaderWriter(
		[]ioResult{{20, nil}, {0, errCouldNotDoIt}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{20, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.Error(t, eastResult.ReadError, errCouldNotDoIt)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(20), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.NoError(t, westResult.WriteError)
}

// Write returns errors.
func TestNetworkStreamWriteError(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {0, errCouldNotDoIt}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(10), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.Error(t, westResult.WriteError, errCouldNotDoIt)
}

// Read returns an error, but also some data is read.
func TestNetworkStreamPartialReadWithError(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, errCouldNotDoIt}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {10, nil}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.Error(t, eastResult.ReadError, errCouldNotDoIt)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(20), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.NoError(t, westResult.WriteError)
}

// Write returns an error, but also some data is written.
func TestNetworkStreamPartialWriteWithError(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	east := newTestReaderWriter(
		[]ioResult{{10, nil}, {10, nil}},
		nothingButSilence,
	)
	west := newTestReaderWriter(
		nothingButSilence,
		[]ioResult{{10, nil}, {5, errCouldNotDoIt}},
	)

	eastResult, westResult := StreamNetworkData(ctx, east, west)

	require.Equal(t, int64(20), eastResult.BytesRead)
	require.Equal(t, int64(0), eastResult.BytesWritten)
	require.NoError(t, eastResult.ReadError)
	require.NoError(t, eastResult.WriteError)

	require.Equal(t, int64(0), westResult.BytesRead)
	require.Equal(t, int64(15), westResult.BytesWritten)
	require.NoError(t, westResult.ReadError)
	require.Error(t, westResult.WriteError, errCouldNotDoIt)
}
