package testutil

import (
	"io"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
)

// The maximum number of entries in the timeline
const bufferSize = 4096

type testReaderTimelineEntry interface {
	Value() (byte, error)
}

type byteTimelineEntry struct {
	value byte
}

func AsByteTimelineEntries(b ...byte) []testReaderTimelineEntry {
	entries := make([]testReaderTimelineEntry, len(b))
	for i := range b {
		entries[i] = &byteTimelineEntry{value: b[i]}
	}

	return entries
}

func (bte *byteTimelineEntry) Value() (byte, error) {
	return bte.value, nil
}

type errorTimelineEntry struct {
	err error
}

func AsErrorTimelineEntry(err error) testReaderTimelineEntry {
	// If err is nil, return EOF as the default error
	if err == nil {
		err = io.EOF
	}

	return &errorTimelineEntry{err: err}
}

func (ete *errorTimelineEntry) Value() (byte, error) {
	return 0, ete.err
}

type TestReader struct {
	timeline chan testReaderTimelineEntry
	closed   bool
}

func NewTestReader() *TestReader {
	return &TestReader{
		timeline: make(chan testReaderTimelineEntry, bufferSize),
	}
}

func (tr *TestReader) AddEntry(entries ...testReaderTimelineEntry) {
	for i := range entries {
		tr.timeline <- entries[i]
	}
}

func (tr *TestReader) Read(p []byte) (int, error) {
	if tr.closed {
		return 0, usvc_io.ErrClosedReader
	}

	for i := range p {
		select {
		case entry := <-tr.timeline:
			b, err := entry.Value()
			if err != nil {
				return i, err
			}

			p[i] = b
		default:
			// If we go to read from the timeline and there's nothing there, we need to respond with EOF
			return i, io.EOF
		}
	}

	return len(p), nil
}

func (tr *TestReader) Close() error {
	tr.closed = true
	return nil
}
