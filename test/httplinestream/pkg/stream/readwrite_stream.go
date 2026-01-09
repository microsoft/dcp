/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package stream

import (
	"bytes"
	"io"
	"sync"
)

// ReadWriteStream is a thread-safe in-memory stream that can be read from and written to concurrently
type ReadWriteStream struct {
	buf           *bytes.Buffer
	mu            sync.Mutex
	writeSignal   chan struct{}
	endOfStream   bool
	readPosition  int64
	writePosition int64
}

// NewReadWriteStream creates a new ReadWriteStream
func NewReadWriteStream() *ReadWriteStream {
	return &ReadWriteStream{
		buf:         new(bytes.Buffer),
		writeSignal: make(chan struct{}, 1),
	}
}

// Read implements io.Reader
func (s *ReadWriteStream) Read(p []byte) (n int, err error) {
	s.mu.Lock()
	
	// Check if there's data to read
	if s.buf.Len() > 0 {
		n, err = s.buf.Read(p)
		s.readPosition += int64(n)
		s.mu.Unlock()
		return n, err
	}
	
	// No data and end of stream
	if s.endOfStream {
		s.mu.Unlock()
		return 0, io.EOF
	}
	
	// No data, need to wait for a write
	s.mu.Unlock()
	
	// Wait for a write signal
	<-s.writeSignal
	
	// Try reading again
	s.mu.Lock()
	n, err = s.buf.Read(p)
	s.readPosition += int64(n)
	s.mu.Unlock()
	
	return n, err
}

// Write implements io.Writer
func (s *ReadWriteStream) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	n, err = s.buf.Write(p)
	s.writePosition += int64(n)
	
	// Signal that data is available
	select {
	case s.writeSignal <- struct{}{}:
	default:
		// Channel already has signal
	}
	
	return n, err
}

// Close marks the stream as ended
func (s *ReadWriteStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	s.endOfStream = true
	
	// Signal readers one last time
	select {
	case s.writeSignal <- struct{}{}:
	default:
	}
	
	return nil
}

// ReadPosition returns the current read position
func (s *ReadWriteStream) ReadPosition() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readPosition
}

// WritePosition returns the current write position
func (s *ReadWriteStream) WritePosition() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writePosition
}

// IsEndOfStream returns whether the stream has been closed
func (s *ReadWriteStream) IsEndOfStream() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endOfStream
}

// SetEndOfStream marks the stream as ended
func (s *ReadWriteStream) SetEndOfStream(value bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endOfStream = value
}
