// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

var errCouldNotDoIt = errors.New("just could not do it")

// Tests normal use case for copyData() (deadline on read)
func TestCopyDataDeadlineExceededOnRead(t *testing.T) {
	t.Parallel()

	incoming := newTestReaderWriter([]ioResult{{20, nil}, {0, os.ErrDeadlineExceeded}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{20, nil}})

	buf := make([]byte, 1024)
	cdr := copyData(incoming, outgoing, buf)
	require.ErrorIs(t, cdr.readError, os.ErrDeadlineExceeded)
	require.NoError(t, cdr.writeError)
	require.Equal(t, int64(20), cdr.bytesRead)
	require.Equal(t, int64(20), cdr.bytesWritten)
}

// Tests normal use case for copyData() (deadline on write)
// We expect the write to be retried.
func TestCopyDataDeadlineExceededOnWrite(t *testing.T) {
	t.Parallel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, nil}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {0, os.ErrDeadlineExceeded}, {10, nil}})

	buf := make([]byte, 1024)
	cdr := copyData(incoming, outgoing, buf)
	require.NoError(t, cdr.readError)
	require.Error(t, cdr.writeError, os.ErrDeadlineExceeded)
	require.Equal(t, int64(20), cdr.bytesRead)
	require.Equal(t, int64(20), cdr.bytesWritten)
}

// Tests invalid write handling (more data written than read)
func TestCopyDataWrittenMoreThanRead(t *testing.T) {
	t.Parallel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, nil}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {20, nil}})

	buf := make([]byte, 1024)
	cdr := copyData(incoming, outgoing, buf)
	require.NoError(t, cdr.readError)
	require.Error(t, cdr.writeError, errInvalidWrite)
	require.Equal(t, int64(20), cdr.bytesRead)
	require.Equal(t, int64(10), cdr.bytesWritten, "The secood write was invalid, so only the first write should be counted")
}

// Less data written than read
func TestCopyDataShortWrite(t *testing.T) {
	t.Parallel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{5, nil}})

	buf := make([]byte, 1024)
	cdr := copyData(incoming, outgoing, buf)
	require.ErrorIs(t, cdr.readError, nil)
	require.Error(t, cdr.writeError, io.ErrShortWrite)
	require.Equal(t, int64(10), cdr.bytesRead)
	require.Equal(t, int64(5), cdr.bytesWritten)
}

// Deadline exceeded on read, but the read also returned some data.
func TestCopyDataDeadlineExceededWithReadData(t *testing.T) {
	t.Parallel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, os.ErrDeadlineExceeded}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {10, nil}})

	buf := make([]byte, 1024)
	cdr := copyData(incoming, outgoing, buf)
	require.ErrorIs(t, cdr.readError, os.ErrDeadlineExceeded)
	require.NoError(t, cdr.writeError)
	require.Equal(t, int64(20), cdr.bytesRead)
	require.Equal(t, int64(20), cdr.bytesWritten)
}

// Deadline exceeded on write, but the write was also partially successful.
func TestCopyDataDeadlineExceededPartialWrite(t *testing.T) {
	t.Parallel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, nil}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {5, os.ErrDeadlineExceeded}, {5, nil}})

	buf := make([]byte, 1024)
	cdr := copyData(incoming, outgoing, buf)
	require.ErrorIs(t, cdr.readError, nil)
	require.Error(t, cdr.writeError, os.ErrDeadlineExceeded)
	require.Equal(t, int64(20), cdr.bytesRead)
	require.Equal(t, int64(20), cdr.bytesWritten)
}

// Deadline exceeded on read and on write, but the write retry is successful.
func TestCopyDataDoubleDeadlineExceeded(t *testing.T) {
	t.Parallel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, os.ErrDeadlineExceeded}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {0, os.ErrDeadlineExceeded}, {10, nil}})

	buf := make([]byte, 1024)
	cdr := copyData(incoming, outgoing, buf)
	require.ErrorIs(t, cdr.readError, os.ErrDeadlineExceeded)
	require.Error(t, cdr.writeError, os.ErrDeadlineExceeded)
	require.Equal(t, int64(20), cdr.bytesRead)
	require.Equal(t, int64(20), cdr.bytesWritten)
}

// Deadline exceeded on read and on write, and the retry also ends with a deadline exceeded.
func TestCopyDataTripleDeadlineExceeded(t *testing.T) {
	t.Parallel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, os.ErrDeadlineExceeded}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {0, os.ErrDeadlineExceeded}, {0, os.ErrDeadlineExceeded}})

	buf := make([]byte, 1024)
	cdr := copyData(incoming, outgoing, buf)
	require.ErrorIs(t, cdr.readError, os.ErrDeadlineExceeded)
	require.Error(t, cdr.writeError, os.ErrDeadlineExceeded)
	require.Equal(t, int64(20), cdr.bytesRead)
	require.Equal(t, int64(10), cdr.bytesWritten)
}

// Read returns an error other than deadline exceeded.
func TestCopyDataReadError(t *testing.T) {
	t.Parallel()

	incoming := newTestReaderWriter([]ioResult{{20, nil}, {0, errCouldNotDoIt}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{20, nil}})

	buf := make([]byte, 1024)
	cdr := copyData(incoming, outgoing, buf)
	require.ErrorIs(t, cdr.readError, errCouldNotDoIt)
	require.NoError(t, cdr.writeError)
	require.Equal(t, int64(20), cdr.bytesRead)
	require.Equal(t, int64(20), cdr.bytesWritten)
}

// Write returns an error other than deadline exceeded.
func TestCopyDataWriteError(t *testing.T) {
	t.Parallel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, nil}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {0, errCouldNotDoIt}})

	buf := make([]byte, 1024)
	cdr := copyData(incoming, outgoing, buf)
	require.NoError(t, cdr.readError)
	require.ErrorIs(t, cdr.writeError, errCouldNotDoIt)
	require.Equal(t, int64(20), cdr.bytesRead)
	require.Equal(t, int64(10), cdr.bytesWritten)
}

// Read returns an error other than deadline exceeded, but also some data is read.
func TestCopyDataPartialReadWithError(t *testing.T) {
	t.Parallel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, errCouldNotDoIt}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {10, nil}})

	buf := make([]byte, 1024)
	cdr := copyData(incoming, outgoing, buf)
	require.ErrorIs(t, cdr.readError, errCouldNotDoIt)
	require.NoError(t, cdr.writeError)
	require.Equal(t, int64(20), cdr.bytesRead)
	require.Equal(t, int64(20), cdr.bytesWritten)
}

// Write returns an error other than deadline exceeded, but also some data is written.
func TestCopyDataPartialWriteWithError(t *testing.T) {
	t.Parallel()

	incoming := newTestReaderWriter([]ioResult{{10, nil}, {10, nil}}, []ioResult{})
	outgoing := newTestReaderWriter([]ioResult{}, []ioResult{{10, nil}, {5, errCouldNotDoIt}})

	buf := make([]byte, 1024)
	cdr := copyData(incoming, outgoing, buf)
	require.NoError(t, cdr.readError)
	require.ErrorIs(t, cdr.writeError, errCouldNotDoIt)
	require.Equal(t, int64(20), cdr.bytesRead)
	require.Equal(t, int64(15), cdr.bytesWritten)
}
