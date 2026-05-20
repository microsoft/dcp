/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package testutil

import (
	"io"
	"sync"
)

// TestPtyResize records one observed (cols, rows) Resize() invocation on a TestPty.
type TestPtyResize struct {
	Cols, Rows uint16
}

// TestPty is an in-memory pseudo-terminal stand-in for tests. It structurally satisfies
// the internal/termpty.PTY interface (io.ReadWriteCloser plus Resize(cols, rows uint16) error)
// without importing that package, which avoids an import cycle between internal/termpty's
// own tests and this shared test utility package.
//
// Reads pull from Inbound (data "produced" by the simulated user process); writes push
// into Outbound (data going "to" the process). Resize calls are recorded in Resizes.
type TestPty struct {
	Inbound  chan []byte
	Outbound chan []byte

	mu       sync.Mutex
	closed   bool
	Resizes  []TestPtyResize
	readErr  error
	writeErr error
}

// NewTestPty creates a TestPty with bounded inbound/outbound buffers.
func NewTestPty() *TestPty {
	return &TestPty{
		Inbound:  make(chan []byte, 16),
		Outbound: make(chan []byte, 16),
	}
}

func (f *TestPty) Read(p []byte) (int, error) {
	f.mu.Lock()
	err := f.readErr
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	chunk, ok := <-f.Inbound
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, chunk)
	return n, nil
}

func (f *TestPty) Write(p []byte) (int, error) {
	f.mu.Lock()
	err := f.writeErr
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	out := make([]byte, len(p))
	copy(out, p)
	f.Outbound <- out
	return len(p), nil
}

// SetReadErr installs an error that subsequent Read calls return immediately.
func (f *TestPty) SetReadErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readErr = err
}

// SetWriteErr installs an error that subsequent Write calls return immediately.
func (f *TestPty) SetWriteErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeErr = err
}

// IsClosed reports whether Close has been called on this TestPty.
func (f *TestPty) IsClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// ResizesSnapshot returns a goroutine-safe copy of the TestPty's resize history.
func (f *TestPty) ResizesSnapshot() []TestPtyResize {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]TestPtyResize, len(f.Resizes))
	copy(out, f.Resizes)
	return out
}

func (f *TestPty) Resize(cols, rows uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Resizes = append(f.Resizes, TestPtyResize{Cols: cols, Rows: rows})
	return nil
}

func (f *TestPty) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.Inbound)
	}
	return nil
}
