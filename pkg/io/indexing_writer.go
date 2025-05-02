package io

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/microsoft/usvc-apiserver/pkg/osutil"
)

const (
	defaultIndexStride uint32 = 1000 // Add an index line for every 1000 lines of log data

)

var (
	// Each index line contains a timestamp, line number, and the offset of the corresponding data line in the data stream.
	// Line numbers are 0-based, so the first line is 0.
	// The first line of the index corresponds to the line number 0. It is not super useful (the offset and the line number are both 0, obviously),
	// but it allows for easy determination of the earliest timestamp in the file.
	// The offset points to the first byte of a line, or more precisely, the first byte of the timestamp preceding the line content.
	indexLineFormat = "%s %d %d" + string(osutil.LineSep())
)

type IndexLine struct {
	Timestamp time.Time
	Line      uint32
	Offset    uint64
}

// indexingWriter is a flavor of TimestampWriter that, in addition to writing a stream of timestamped lines,
// also writes "index line" to a separate index data stream.
// indexingWriter is generally NOT goroutine-safe. It depends on state that is maintained across multiple writes.
// Calls to Close(), Sync(), and IndexInvalid() are goroutine-safe.

type IndexingWriter struct {
	// The underlying data writer
	dataW WriteSyncerCloser

	// The index writer to write the index lines to
	indexW WriteSyncerCloser

	// Do we need to write a timestamp before we output the next byte?
	needsTimestamp bool

	// The current line number
	line uint32

	// The current offset in the data stream
	offset uint64

	// Buffers for processing output
	dataBuf  *bytes.Buffer
	indexBuf *bytes.Buffer

	// The index "stride", i.e. the number of data lines written before we write an index line.
	indexStride uint32

	// Flag to indicate if the writer is closed
	closed bool

	// Flag to indicate the index is invalid and should not be used (e.g. because some writes failed in the past).
	indexInvalid *atomic.Bool
}

func NewIndexingWriter(data, index WriteSyncerCloser) *IndexingWriter {
	return &IndexingWriter{
		dataW:          data,
		indexW:         index,
		needsTimestamp: true,
		line:           0,
		offset:         0,
		dataBuf:        new(bytes.Buffer),
		indexBuf:       new(bytes.Buffer),
		closed:         false,
		indexInvalid:   new(atomic.Bool),
		indexStride:    defaultIndexStride,
	}
}

func NewIndexingWriterWithStride(data, index WriteSyncerCloser, indexStride uint32) *IndexingWriter {
	iw := NewIndexingWriter(data, index)
	if indexStride != 0 {
		iw.indexStride = indexStride
	}
	return iw
}

func (iw *IndexingWriter) Write(p []byte) (int, error) {
	if iw.closed {
		return 0, ErrClosedWriter
	}

	reset(&iw.dataBuf)
	reset(&iw.indexBuf)
	indexInvalid := iw.indexInvalid.Load()

	// Note: buffer writes never return an error (they may panic if the buffer grows beyond 2GB)

	for _, b := range p {
		if iw.needsTimestamp {
			timestamp := time.Now().UTC().Format(osutil.RFC3339MiliTimestampFormat)
			if !indexInvalid && iw.line%iw.indexStride == 0 {
				indexLine := fmt.Sprintf(indexLineFormat, timestamp, iw.line, iw.offset)
				_, _ = iw.indexBuf.WriteString(indexLine)
			}
			iw.line++
			timestampLen, _ := iw.dataBuf.WriteString(timestamp + " ")
			iw.offset += uint64(timestampLen)
			iw.needsTimestamp = false
		}

		if b == '\n' {
			iw.needsTimestamp = true
		}

		iw.dataBuf.WriteByte(b)
		iw.offset += 1
	}

	data := iw.dataBuf.Bytes()
	n, dataErr := iw.dataW.Write(data)
	if dataErr != nil {
		iw.indexInvalid.Store(true)
		return n, dataErr
	}
	if n != len(data) {
		iw.indexInvalid.Store(true)
		return n, io.ErrShortWrite
	}

	if iw.indexBuf.Len() > 0 && !indexInvalid {
		// Make sure data is visible to other processes before writing the index
		// This is potentially a costly operation, but we do it only for "indexStride" lines,
		// so it should not be a problem in practice.
		syncErr := iw.dataW.Sync()

		if syncErr != nil {
			iw.indexInvalid.Store(true)
		} else {
			indexEntries := iw.indexBuf.Bytes()
			in, indexErr := iw.indexW.Write(indexEntries)
			if indexErr != nil || in != len(indexEntries) {
				iw.indexInvalid.Store(true)
			}
		}
	}

	// Return original number of bytes we were expected to write to avoid triggering
	// a short write error in the caller.
	return len(p), nil
}

func (iw *IndexingWriter) Close() error {
	if iw.closed {
		return nil
	}
	iw.closed = true

	var err error
	err = errors.Join(err, iw.dataW.Close())
	err = errors.Join(err, iw.indexW.Close())
	return err
}

func (iw *IndexingWriter) Sync() error {
	var err error
	err = errors.Join(err, iw.dataW.Sync())
	err = errors.Join(err, iw.indexW.Sync())
	return err
}

func (iw *IndexingWriter) IndexInvalid() bool {
	return iw.indexInvalid.Load()
}

var _ WriteSyncerCloser = (*IndexingWriter)(nil)
