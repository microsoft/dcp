/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package hmp1

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakePty is an in-memory PTY for tests. Reads pull from inbound (data
// "produced" by the user process); writes push into outbound (data going
// "to" the process). Resize calls are recorded for assertions.
type fakePty struct {
	inbound  chan []byte
	outbound chan []byte

	mu      sync.Mutex
	closed  bool
	resizes []struct{ Cols, Rows int }
}

func newFakePty() *fakePty {
	return &fakePty{
		inbound:  make(chan []byte, 16),
		outbound: make(chan []byte, 16),
	}
}

func (f *fakePty) Read(p []byte) (int, error) {
	chunk, ok := <-f.inbound
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, chunk)
	return n, nil
}

func (f *fakePty) Write(p []byte) (int, error) {
	out := make([]byte, len(p))
	copy(out, p)
	f.outbound <- out
	return len(p), nil
}

func (f *fakePty) Resize(cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, struct{ Cols, Rows int }{cols, rows})
	return nil
}

func (f *fakePty) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.inbound)
	}
	return nil
}

func TestServeSendsHelloAndStateSyncOnConnect(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	pty := newFakePty()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, b, pty, nil, ServerOptions{InitialCols: 100, InitialRows: 30})
	}()

	// Read Hello.
	frameType, payload := readFrameHelper(t, a)
	if frameType != FrameHello {
		t.Fatalf("expected Hello frame, got 0x%02x", frameType)
	}
	var hello HelloPayload
	if err := json.Unmarshal(payload, &hello); err != nil {
		t.Fatalf("unmarshal Hello: %v", err)
	}
	if hello.Version != 1 || hello.Width != 100 || hello.Height != 30 {
		t.Errorf("unexpected Hello payload: %+v", hello)
	}

	// Read StateSync.
	frameType, payload = readFrameHelper(t, a)
	if frameType != FrameStateSync {
		t.Fatalf("expected StateSync frame, got 0x%02x", frameType)
	}
	if len(payload) != 0 {
		t.Errorf("expected empty StateSync, got %d bytes", len(payload))
	}

	// Trigger PTY exit so Serve returns.
	pty.Close()

	// Read terminal Exit frame.
	frameType, _ = readFrameHelper(t, a)
	if frameType != FrameExit {
		t.Errorf("expected Exit frame after PTY EOF, got 0x%02x", frameType)
	}

	if err := <-done; err != nil {
		t.Errorf("Serve returned error: %v", err)
	}
}

func TestServeForwardsOutputFromPty(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	pty := newFakePty()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, b, pty, nil, ServerOptions{InitialCols: 80, InitialRows: 24})
	}()

	// Drain Hello + StateSync.
	readFrameHelper(t, a)
	readFrameHelper(t, a)

	// Push some PTY output.
	pty.inbound <- []byte("hello world")

	// Should arrive as an Output frame.
	frameType, payload := readFrameHelper(t, a)
	if frameType != FrameOutput {
		t.Fatalf("expected Output frame, got 0x%02x", frameType)
	}
	if string(payload) != "hello world" {
		t.Errorf("unexpected Output payload: %q", string(payload))
	}

	pty.Close()
	readFrameHelper(t, a) // Exit
	<-done
}

func TestServeForwardsInputToPty(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	pty := newFakePty()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, b, pty, nil, ServerOptions{})
	}()

	// Drain Hello + StateSync.
	readFrameHelper(t, a)
	readFrameHelper(t, a)

	// Send an Input frame.
	writeFrameHelper(t, a, FrameInput, []byte("ls\r\n"))

	select {
	case got := <-pty.outbound:
		if string(got) != "ls\r\n" {
			t.Errorf("unexpected PTY input: %q", string(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for PTY input")
	}

	pty.Close()
	readFrameHelper(t, a)
	<-done
}

func TestServeForwardsResizeToPty(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	pty := newFakePty()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, b, pty, nil, ServerOptions{})
	}()

	readFrameHelper(t, a)
	readFrameHelper(t, a)

	resizePayload := make([]byte, 8)
	binary.LittleEndian.PutUint32(resizePayload[0:4], 132)
	binary.LittleEndian.PutUint32(resizePayload[4:8], 50)
	writeFrameHelper(t, a, FrameResize, resizePayload)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pty.mu.Lock()
		count := len(pty.resizes)
		pty.mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	pty.mu.Lock()
	if len(pty.resizes) != 1 || pty.resizes[0].Cols != 132 || pty.resizes[0].Rows != 50 {
		t.Errorf("unexpected PTY resize history: %+v", pty.resizes)
	}
	pty.mu.Unlock()

	pty.Close()
	readFrameHelper(t, a)
	<-done
}

func TestServeSendsExitFrameWithExitCode(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	pty := newFakePty()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, b, pty, func() int32 { return 42 }, ServerOptions{})
	}()

	readFrameHelper(t, a)
	readFrameHelper(t, a)

	pty.Close()

	frameType, payload := readFrameHelper(t, a)
	if frameType != FrameExit {
		t.Fatalf("expected Exit frame, got 0x%02x", frameType)
	}
	if len(payload) != 4 {
		t.Fatalf("expected 4-byte Exit payload, got %d", len(payload))
	}
	exitCode := int32(binary.LittleEndian.Uint32(payload))
	if exitCode != 42 {
		t.Errorf("expected exit code 42, got %d", exitCode)
	}

	<-done
}

func TestServeRejectsOversizedFrame(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	pty := newFakePty()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, b, pty, nil, ServerOptions{})
	}()

	readFrameHelper(t, a) // Hello
	readFrameHelper(t, a) // StateSync

	// Craft a header that claims an oversize length without sending the body.
	header := [5]byte{byte(FrameInput), 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(header[1:5], uint32(MaxPayloadLength+1))
	if _, err := a.Write(header[:]); err != nil {
		t.Fatalf("write header: %v", err)
	}

	// In production the PTY persists across viewer reconnects, so Serve does
	// not close it on abort. Close it explicitly here to let the read pump
	// exit, which is what production would do on shutdown anyway.
	pty.Close()

	// Server should treat the frame as a fatal protocol error and exit; we
	// observe that by Serve completing.
	select {
	case err := <-done:
		if err == nil {
			t.Errorf("expected Serve to return an error on oversize frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Serve to exit on oversize frame")
	}
}

// readFrameHelper performs a blocking read of one HMP v1 frame from r,
// failing the test on error.
func readFrameHelper(t *testing.T, r net.Conn) (FrameType, []byte) {
	t.Helper()
	_ = r.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer func() {
		_ = r.SetReadDeadline(time.Time{})
	}()
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("connection closed while expecting frame")
		}
		t.Fatalf("read header: %v", err)
	}
	t.Logf("got frame type=0x%02x len=%d", header[0], binary.LittleEndian.Uint32(header[1:5]))
	length := binary.LittleEndian.Uint32(header[1:5])
	if length == 0 {
		return FrameType(header[0]), nil
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return FrameType(header[0]), payload
}

func writeFrameHelper(t *testing.T, w net.Conn, ft FrameType, payload []byte) {
	t.Helper()
	header := [5]byte{}
	header[0] = byte(ft)
	binary.LittleEndian.PutUint32(header[1:5], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			t.Fatalf("write payload: %v", err)
		}
	}
}
