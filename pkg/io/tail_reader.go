/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// package io implements various io utilities.
package io

import (
	"bufio"
	"io"
	"sync/atomic"

	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/container"
)

const (
	readBufferSize     = 32 * 1024   // 32KB
	lineEOF        int = -1          // A sentinel value indicating we have given all "last N" lines to the caller
	MaxTailSize        = 1000 * 1000 // Million rows
)

type TailReaderStats struct {
	TotalLines int64 // How many lines have been read from the inner reader
	TailLines  int   // How many "tail" lines are available/will be returned to the reader
}

// TailReader is an io.ReadCloser that returns the last N lines of text from an inner reader.
// It reads all content from the inner reader until EOF, then allows reading only
// the last N lines from it. If the inner reader contains fewer than N lines, all lines will be returned.
//
// TailReader remains usable after EOF is reached, i.e. it will just delegate to the inner reader once
// the last N lines are returned.
//
// TailReader is not thread-safe, except from the Filled event and the Stats atomic pointer.
// The Stats value is valid only after the Filled event is set.
type TailReader struct {
	// Set (frozen) when tailLines is filled with data and the reader stats are available.
	Filled    *concurrency.AutoResetEvent
	FillStats *atomic.Pointer[TailReaderStats] // The stats of the reader

	// TailReader inner state
	inner       io.ReadCloser                 // The inner reader to read from
	tailLines   *container.RingBuffer[[]byte] // The last N lines read from the inner reader
	currentLine int                           // What line to read from in response to a Read() call
	linePos     int                           // Position within the current line
	closed      bool                          // Whether the reader is closed
}

func NewTailReader(inner io.ReadCloser, tailSize int) *TailReader {
	if tailSize <= 0 {
		panic("tailSize must be greater than 0")
	}
	if tailSize > MaxTailSize {
		panic("tailSize too large")
	}

	tr := &TailReader{
		Filled:      concurrency.NewAutoResetEvent(false),
		FillStats:   &atomic.Pointer[TailReaderStats]{},
		inner:       inner,
		tailLines:   container.NewBoundedRingBuffer[[]byte](tailSize),
		currentLine: 0,
		linePos:     0,
		closed:      false,
	}

	return tr
}

// Read implements the io.Reader interface.
// On the first call, it reads all content from the inner reader,
// stores the last N lines, and then returns them on subsequent calls.
func (tr *TailReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, io.ErrShortBuffer
	}

	if tr.closed {
		return 0, ErrClosedReader
	}

	if tr.currentLine == lineEOF {
		// All data from "last N" lines have been returned, so we just delegate to the inner reader.
		return tr.inner.Read(p)
	}

	if !tr.Filled.Frozen() {
		if fillErr := tr.fill(); fillErr != nil {
			return 0, fillErr
		}
	}

	if tr.tailLines.Empty() && tr.currentLine != lineEOF {
		// Special case: initial fill returned no lines, we need to return EOF to the caller,
		// and set the currentLine to EOF so that we start delegating to the inner reader.
		tr.currentLine = lineEOF
		return 0, io.EOF
	}

	toWrite := len(p)
	ppos := 0    // Position within the provided buffer
	written := 0 // Number of bytes written

	for toWrite > 0 {
		line, _ := tr.tailLines.PeekAt(tr.currentLine)
		available := len(line) - tr.linePos

		if available > toWrite {
			// We can only write part of the line
			copy(p[ppos:], line[tr.linePos:tr.linePos+toWrite])
			tr.linePos += toWrite
			written += toWrite
			break
		}

		// We can write the whole line
		copy(p[ppos:], line[tr.linePos:])
		ppos += available
		toWrite -= available
		tr.linePos = 0 // Reset position for the next line
		tr.currentLine++
		written += available

		if tr.currentLine == tr.tailLines.Len() {
			// No more lines to read, set currentLine to EOF
			tr.currentLine = lineEOF
			break
		}
	}

	return written, nil
}

// Close implements the io.Closer interface.
func (tr *TailReader) Close() error {
	if tr.closed {
		return nil
	}

	tr.closed = true
	return tr.inner.Close()
}

// fills the buffer with content from the inner reader
func (tr *TailReader) fill() error {
	r := bufio.NewReaderSize(tr.inner, readBufferSize)

	stats := tr.FillStats.Load()
	if stats == nil {
		stats = &TailReaderStats{TotalLines: 0, TailLines: 0}
	}
	defer tr.FillStats.Store(stats)

	line, found := tr.tailLines.PeekTail()
	if found && len(line) > 0 && line[len(line)-1] != '\n' {
		// Incomplete line: we are going to append to it, so remove it from the set of lines to work on it.
		_, _ = tr.tailLines.PopTail()
	} else {
		line = nil
	}

	for {
		chunk, err := r.ReadBytes('\n') // Result will include the newline character, if present.
		line = append(line, chunk...)
		if len(line) > 0 {
			tr.tailLines.Push(line)
			if line[len(line)-1] == '\n' {
				stats.TotalLines++
			}
		}

		if err != nil {
			if err == io.EOF {
				if len(line) > 0 && line[len(line)-1] != '\n' {
					// The last line does not end with a newline and it was not counted above.
					stats.TotalLines++
				}
				stats.TailLines = tr.tailLines.Len()
				tr.FillStats.Store(stats) // Mke sure stats are updated before we advertise them
				tr.Filled.SetAndFreeze()
				return nil
			} else {
				return err
			}
		} else {
			line = nil // Reset line for the next iteration
		}
	}
}

var _ io.ReadCloser = (*TailReader)(nil)
