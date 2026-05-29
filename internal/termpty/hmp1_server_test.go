/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
)

const (
	defaultTestTimeout = 20 * time.Second
)

// The in-memory pseudo-terminal used by these tests lives in internal/testutil
// (as testutil.TestPty) so other packages' tests can reuse it without depending
// on this package's test code. We refer to it via internal_testutil below.

func TestServeSendsHelloAndStateSyncOnConnect(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	pty := internal_testutil.NewTestPty()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	done := make(chan error, 1)
	server := NewHmp1Server(ctx, pty)
	go func() {
		done <- server.Serve(ctx, b, nil, Hmp1ServerOptions{InitialCols: 100, InitialRows: 30})
	}()

	// Read Hello.
	frameType, payload := readFrameHelper(t, ctx, a)
	if frameType != Hmp1FrameHello {
		t.Fatalf("expected Hello frame, got 0x%02x", frameType)
	}
	var hello Hmp1HelloPayload
	if err := json.Unmarshal(payload, &hello); err != nil {
		t.Fatalf("unmarshal Hello: %v", err)
	}
	if hello.Version != 1 || hello.Width != 100 || hello.Height != 30 {
		t.Errorf("unexpected Hello payload: %+v", hello)
	}

	// Read StateSync.
	frameType, payload = readFrameHelper(t, ctx, a)
	if frameType != Hmp1FrameStateSync {
		t.Fatalf("expected StateSync frame, got 0x%02x", frameType)
	}
	if len(payload) != 0 {
		t.Errorf("expected empty StateSync, got %d bytes", len(payload))
	}

	// Trigger PTY exit so Serve returns.
	pty.Close()

	// Read terminal Exit frame.
	frameType, _ = readFrameHelper(t, ctx, a)
	if frameType != Hmp1FrameExit {
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
	pty := internal_testutil.NewTestPty()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	done := make(chan error, 1)
	server := NewHmp1Server(ctx, pty)
	go func() {
		done <- server.Serve(ctx, b, nil, Hmp1ServerOptions{InitialCols: 80, InitialRows: 24})
	}()

	// Drain Hello + StateSync.
	readFrameHelper(t, ctx, a)
	readFrameHelper(t, ctx, a)

	// Push some PTY output.
	pty.Inbound <- []byte("hello world")

	// Should arrive as an Output frame.
	frameType, payload := readFrameHelper(t, ctx, a)
	if frameType != Hmp1FrameOutput {
		t.Fatalf("expected Output frame, got 0x%02x", frameType)
	}
	if string(payload) != "hello world" {
		t.Errorf("unexpected Output payload: %q", string(payload))
	}

	pty.Close()
	readFrameHelper(t, ctx, a) // Exit
	<-done
}

func TestServeForwardsInputToPty(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	pty := internal_testutil.NewTestPty()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	done := make(chan error, 1)
	server := NewHmp1Server(ctx, pty)
	go func() {
		done <- server.Serve(ctx, b, nil, Hmp1ServerOptions{})
	}()

	// Drain Hello + StateSync.
	readFrameHelper(t, ctx, a)
	readFrameHelper(t, ctx, a)

	// Send an Input frame.
	writeFrameHelper(t, a, Hmp1FrameInput, []byte("ls\r\n"))

	select {
	case got := <-pty.Outbound:
		if string(got) != "ls\r\n" {
			t.Errorf("unexpected PTY input: %q", string(got))
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for PTY input")
	}

	pty.Close()
	readFrameHelper(t, ctx, a)
	<-done
}

func TestServeForwardsResizeToPty(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	pty := internal_testutil.NewTestPty()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	done := make(chan error, 1)
	server := NewHmp1Server(ctx, pty)
	go func() {
		done <- server.Serve(ctx, b, nil, Hmp1ServerOptions{})
	}()

	readFrameHelper(t, ctx, a)
	readFrameHelper(t, ctx, a)

	resizePayload := make([]byte, 8)
	binary.LittleEndian.PutUint32(resizePayload[0:4], 132)
	binary.LittleEndian.PutUint32(resizePayload[4:8], 50)
	writeFrameHelper(t, a, Hmp1FrameResize, resizePayload)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(pty.ResizesSnapshot()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	resizes := pty.ResizesSnapshot()
	if len(resizes) != 1 || resizes[0].Cols != 132 || resizes[0].Rows != 50 {
		t.Errorf("unexpected PTY resize history: %+v", resizes)
	}

	pty.Close()
	readFrameHelper(t, ctx, a)
	<-done
}

func TestServeSendsExitFrameWithExitCode(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	pty := internal_testutil.NewTestPty()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	done := make(chan error, 1)
	server := NewHmp1Server(ctx, pty)
	exitCodeCh := make(chan int32, 1)
	exitCodeCh <- 42
	go func() {
		done <- server.Serve(ctx, b, exitCodeCh, Hmp1ServerOptions{})
	}()

	readFrameHelper(t, ctx, a)
	readFrameHelper(t, ctx, a)

	pty.Close()

	frameType, payload := readFrameHelper(t, ctx, a)
	if frameType != Hmp1FrameExit {
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
	pty := internal_testutil.NewTestPty()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	exitCodeCh := make(chan int32)

	done := make(chan error, 1)
	server := NewHmp1Server(ctx, pty)
	go func() {
		done <- server.Serve(ctx, b, exitCodeCh, Hmp1ServerOptions{})
	}()

	readFrameHelper(t, ctx, a) // Hello
	readFrameHelper(t, ctx, a) // StateSync

	// Craft a header that claims an oversize length without sending the body.
	header := [5]byte{byte(Hmp1FrameInput), 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(header[1:5], uint32(Hmp1MaxPayloadLength+1))
	if _, err := a.Write(header[:]); err != nil {
		t.Fatalf("write header: %v", err)
	}

	// Defer the PTY close to ensure it's closed AFTER the server has a chance
	// to report the oversize frame error.
	defer pty.Close()

	// Server should treat the frame as a fatal protocol error and exit; we
	// observe that by Serve completing.
	select {
	case err := <-done:
		require.Error(t, err, "expected Serve to return an error on oversize frame")
	case <-ctx.Done():
		t.Fatal("timed out waiting for Serve to exit on oversize frame")
	}
}

// runSession is a small fixture helper that starts a Hmp1Server in a
// goroutine and returns the client end of the pipe, the testPty, a done
// channel that receives Serve's return value, and the cancel function for the
// test context.
//
// The returned cleanup function closes both ends of the pipe and waits for
// the Serve goroutine to terminate. It is safe to call multiple times.
type sessionFixture struct {
	t         *testing.T
	ctx       context.Context
	cancel    context.CancelFunc
	a         net.Conn
	b         net.Conn
	pty       *internal_testutil.TestPty
	server    *Hmp1Server
	serveErr  error
	serveDone chan struct{}
}

func startSession(t *testing.T, opts Hmp1ServerOptions, exitCodeCh <-chan int32) *sessionFixture {
	t.Helper()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	a, b := net.Pipe()
	pty := internal_testutil.NewTestPty()
	server := NewHmp1Server(ctx, pty)

	f := &sessionFixture{
		t:         t,
		ctx:       ctx,
		cancel:    cancel,
		a:         a,
		b:         b,
		pty:       pty,
		server:    server,
		serveDone: make(chan struct{}),
	}
	go func() {
		defer close(f.serveDone)
		f.serveErr = server.Serve(ctx, b, exitCodeCh, opts)
	}()

	return f
}

func (f *sessionFixture) drainHelloAndStateSync() {
	f.t.Helper()
	if ft, _ := readFrameHelper(f.t, f.ctx, f.a); ft != Hmp1FrameHello {
		f.t.Fatalf("expected Hello, got 0x%02x", ft)
	}
	if ft, _ := readFrameHelper(f.t, f.ctx, f.a); ft != Hmp1FrameStateSync {
		f.t.Fatalf("expected StateSync, got 0x%02x", ft)
	}
}

// drainUntilExit reads frames until it sees the Exit frame, returning its
// payload. Other frames are discarded.
func (f *sessionFixture) drainUntilExit() []byte {
	f.t.Helper()
	for {
		ft, payload := readFrameHelper(f.t, f.ctx, f.a)
		if ft == Hmp1FrameExit {
			return payload
		}
	}
}

func (f *sessionFixture) cleanup() {
	_ = f.a.Close()
	_ = f.b.Close()
	// Best-effort wake up of any blocked pump.
	if !f.pty.IsClosed() {
		_ = f.pty.Close()
	}
	select {
	case <-f.serveDone:
	case <-f.ctx.Done():
		f.t.Errorf("Serve did not exit during cleanup")
	}
	f.cancel()
}

// --- Server reuse / concurrency ---

func TestServeIsReusableAcrossSequentialConnections(t *testing.T) {
	t.Parallel()
	pty := internal_testutil.NewTestPty()
	defer pty.Close()

	textCtx, textCtxCancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer textCtxCancel()
	server := NewHmp1Server(textCtx, pty)

	runSession := func(sessionName string) {
		sessionCtx, sessionCancel := context.WithCancel(textCtx)
		defer sessionCancel()
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()

		done := make(chan error, 1)
		go func() {
			done <- server.Serve(sessionCtx, b, nil, Hmp1ServerOptions{InitialCols: 80, InitialRows: 24})
		}()

		if ft, _ := readFrameHelper(t, sessionCtx, a); ft != Hmp1FrameHello {
			t.Fatalf("[%s] expected Hello, got 0x%02x", sessionName, ft)
		}
		if ft, _ := readFrameHelper(t, sessionCtx, a); ft != Hmp1FrameStateSync {
			t.Fatalf("[%s] expected StateSync, got 0x%02x", sessionName, ft)
		}

		// Round-trip a client -> PTY Input frame to prove the conn -> PTY
		// direction works on this session.
		writeFrameHelper(t, a, Hmp1FrameInput, []byte(sessionName))
		select {
		case got := <-pty.Outbound:
			if string(got) != sessionName {
				t.Errorf("[%s] PTY got %q, want %q", sessionName, got, sessionName)
			}
		case <-sessionCtx.Done():
			t.Fatalf("[%s] timed out waiting for PTY input", sessionName)
		}

		expectedOut := "out-" + sessionName
		select {
		case pty.Inbound <- []byte(expectedOut):
		case <-sessionCtx.Done():
			t.Fatalf("[%s] timed out pushing to PTY inbound", sessionName)
		}
		ft, payload := readFrameHelper(t, sessionCtx, a)
		if ft != Hmp1FrameOutput {
			t.Fatalf("[%s] expected Output, got 0x%02x", sessionName, ft)
		}
		if string(payload) != expectedOut {
			t.Errorf("[%s] PTY -> client got %q, want %q", sessionName, payload, expectedOut)
		}

		// End the session by closing the client end of the pipe (graceful).
		_ = a.Close()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("[%s] Serve returned error: %v", sessionName, err)
			}
		case <-sessionCtx.Done():
			t.Fatalf("[%s] timed out waiting for Serve to return", sessionName)
		}
	}

	runSession("first")
	if pty.IsClosed() {
		t.Fatalf("Serve must not close the PTY between sessions")
	}
	runSession("second")
	if pty.IsClosed() {
		t.Fatalf("Serve must not close the PTY between sessions (post-second)")
	}
}

// Ensures that even if some PTY output is produced between sessions,
// it is preserved and delivered to the next session.
func TestServePreservesPtyOutputAcrossSessionBoundary(t *testing.T) {
	t.Parallel()
	pty := internal_testutil.NewTestPty()
	defer pty.Close()

	// A single lifetime context spans both sessions so the Hmp1Server's
	// shared PTY reader worker stays alive between Serve invocations.
	lifetimeCtx, lifetimeCancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer lifetimeCancel()
	server := NewHmp1Server(lifetimeCtx, pty)

	// --- Session 1: connect, drain Hello/StateSync, then gracefully close.
	session1Ctx, session1Cancel := context.WithCancel(lifetimeCtx)
	a1, b1 := net.Pipe()
	defer a1.Close()
	defer b1.Close()
	done1 := make(chan error, 1)
	go func() {
		done1 <- server.Serve(session1Ctx, b1, nil, Hmp1ServerOptions{InitialCols: 80, InitialRows: 24})
	}()

	if ft, _ := readFrameHelper(t, session1Ctx, a1); ft != Hmp1FrameHello {
		t.Fatalf("session 1: expected Hello, got 0x%02x", ft)
	}
	if ft, _ := readFrameHelper(t, session1Ctx, a1); ft != Hmp1FrameStateSync {
		t.Fatalf("session 1: expected StateSync, got 0x%02x", ft)
	}

	// Close only the client end to signal a graceful session end. Serve will
	// observe EOF on the server end and close it via its own defer.
	_ = a1.Close()
	select {
	case err := <-done1:
		if err != nil {
			t.Errorf("session 1: Serve returned error: %v", err)
		}
	case <-lifetimeCtx.Done():
		t.Fatalf("session 1: timed out waiting for Serve to return")
	}
	session1Cancel()

	if pty.IsClosed() {
		t.Fatalf("Serve must not close the PTY between sessions")
	}

	// --- Push data into the PTY between sessions.
	// The Hmp1Server's CancellableReader worker is still alive (the lifetime
	// context was not cancelled) and is blocked in pty.Read. It will
	// consume this chunk and buffer it in the reader's internal slot until
	// the next session reads from the reader.
	betweenSessionData := []byte("preserved-across-boundary")
	select {
	case pty.Inbound <- betweenSessionData:
	case <-lifetimeCtx.Done():
		t.Fatalf("timed out pushing data between sessions")
	}

	// --- Session 2: connect and verify the between-sessions chunk arrives.
	session2Ctx, session2Cancel := context.WithCancel(lifetimeCtx)
	defer session2Cancel()
	a2, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	done2 := make(chan error, 1)
	go func() {
		done2 <- server.Serve(session2Ctx, b2, nil, Hmp1ServerOptions{InitialCols: 80, InitialRows: 24})
	}()

	if ft, _ := readFrameHelper(t, session2Ctx, a2); ft != Hmp1FrameHello {
		t.Fatalf("session 2: expected Hello, got 0x%02x", ft)
	}
	if ft, _ := readFrameHelper(t, session2Ctx, a2); ft != Hmp1FrameStateSync {
		t.Fatalf("session 2: expected StateSync, got 0x%02x", ft)
	}

	// The next frame the client sees must carry the data pushed between
	// sessions. If this fails (e.g. timeout, wrong frame type, wrong payload)
	// the cross-session preservation guarantee has regressed.
	ft, payload := readFrameHelper(t, session2Ctx, a2)
	if ft != Hmp1FrameOutput {
		t.Fatalf("session 2: expected Output, got 0x%02x (payload %q)", ft, payload)
	}
	if string(payload) != string(betweenSessionData) {
		t.Errorf("session 2: PTY -> client got %q, want %q", payload, betweenSessionData)
	}

	// End session 2 cleanly.
	_ = a2.Close()
	select {
	case err := <-done2:
		if err != nil {
			t.Errorf("session 2: Serve returned error: %v", err)
		}
	case <-lifetimeCtx.Done():
		t.Fatalf("session 2: timed out waiting for Serve to return")
	}
}

func TestServeRejectsConcurrentInvocation(t *testing.T) {
	t.Parallel()
	pty := internal_testutil.NewTestPty()
	defer pty.Close()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	server := NewHmp1Server(ctx, pty)

	// First session, live.
	a1, b1 := net.Pipe()
	defer a1.Close()
	defer b1.Close()
	first := make(chan error, 1)
	go func() {
		first <- server.Serve(ctx, b1, nil, Hmp1ServerOptions{})
	}()

	// Drain Hello/StateSync so we know the first Serve is running.
	if ft, _ := readFrameHelper(t, ctx, a1); ft != Hmp1FrameHello {
		t.Fatalf("first session: expected Hello, got 0x%02x", ft)
	}
	if ft, _ := readFrameHelper(t, ctx, a1); ft != Hmp1FrameStateSync {
		t.Fatalf("first session: expected StateSync, got 0x%02x", ft)
	}

	// Concurrent second invocation must fail quickly without writing anything to its conn.
	a2, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	second := make(chan error, 1)
	go func() {
		second <- server.Serve(ctx, b2, nil, Hmp1ServerOptions{})
	}()

	select {
	case err := <-second:
		if err == nil {
			t.Fatalf("expected concurrent Serve to fail, got nil")
		}
		if !errorMessageContains(err, "already serving") {
			t.Errorf("expected error mentioning 'already serving', got %v", err)
		}
	case <-ctx.Done():
		t.Fatal("concurrent Serve did not return promptly")
	}

	// Wind down the first session.
	_ = a1.Close()
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("first Serve did not return after client close")
	}

	// A subsequent Serve must still work — the rejected concurrent attempt
	// must not have flipped the serving flag back to false prematurely.
	a3, b3 := net.Pipe()
	defer a3.Close()
	defer b3.Close()
	third := make(chan error, 1)
	go func() {
		third <- server.Serve(ctx, b3, nil, Hmp1ServerOptions{})
	}()
	if ft, _ := readFrameHelper(t, ctx, a3); ft != Hmp1FrameHello {
		t.Fatalf("third session: expected Hello, got 0x%02x", ft)
	}
	if ft, _ := readFrameHelper(t, ctx, a3); ft != Hmp1FrameStateSync {
		t.Fatalf("third session: expected StateSync, got 0x%02x", ft)
	}
	_ = a3.Close()
	select {
	case err := <-third:
		if err != nil {
			t.Errorf("third Serve returned error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("third Serve did not return")
	}
}

// --- Defaults ---

func TestServeUsesDefaultDimensionsWhenZero(t *testing.T) {
	t.Parallel()
	f := startSession(t, Hmp1ServerOptions{}, nil)
	defer f.cleanup()

	ft, payload := readFrameHelper(t, f.ctx, f.a)
	if ft != Hmp1FrameHello {
		t.Fatalf("expected Hello, got 0x%02x", ft)
	}
	var hello Hmp1HelloPayload
	if err := json.Unmarshal(payload, &hello); err != nil {
		t.Fatalf("unmarshal Hello: %v", err)
	}
	if hello.Width != defaultInitialCols || hello.Height != defaultInitialRows {
		t.Errorf("expected default %dx%d, got %dx%d",
			defaultInitialCols, defaultInitialRows, hello.Width, hello.Height)
	}
}

// --- Exit code paths ---

func TestServeSendsZeroExitCodeWhenChannelIsNil(t *testing.T) {
	t.Parallel()
	f := startSession(t, Hmp1ServerOptions{}, nil)
	defer f.cleanup()

	f.drainHelloAndStateSync()
	_ = f.pty.Close()

	payload := f.drainUntilExit()
	if len(payload) != 4 {
		t.Fatalf("Exit payload length = %d, want 4", len(payload))
	}
	if code := int32(binary.LittleEndian.Uint32(payload)); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestServeSendsZeroExitCodeWhenChannelClosedWithoutValue(t *testing.T) {
	t.Parallel()
	exitCodeCh := make(chan int32)
	close(exitCodeCh) // closed before Serve reaches the select

	f := startSession(t, Hmp1ServerOptions{}, exitCodeCh)
	defer f.cleanup()

	f.drainHelloAndStateSync()
	_ = f.pty.Close()

	payload := f.drainUntilExit()
	if len(payload) != 4 {
		t.Fatalf("Exit payload length = %d, want 4", len(payload))
	}
	if code := int32(binary.LittleEndian.Uint32(payload)); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestServeForwardsNegativeExitCode(t *testing.T) {
	t.Parallel()
	exitCodeCh := make(chan int32, 1)
	exitCodeCh <- -1

	f := startSession(t, Hmp1ServerOptions{}, exitCodeCh)
	defer f.cleanup()

	f.drainHelloAndStateSync()
	_ = f.pty.Close()

	payload := f.drainUntilExit()
	if len(payload) != 4 {
		t.Fatalf("Exit payload length = %d, want 4", len(payload))
	}
	if code := int32(binary.LittleEndian.Uint32(payload)); code != -1 {
		t.Errorf("exit code = %d, want -1", code)
	}
}

// --- Lifecycle ---

func TestServeReturnsWhenContextCancelled(t *testing.T) {
	t.Parallel()
	pty := internal_testutil.NewTestPty()
	defer pty.Close()

	parentCtx, parentCancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer parentCancel()
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	server := NewHmp1Server(parentCtx, pty)

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx, b, nil, Hmp1ServerOptions{})
	}()

	// Drain Hello + StateSync so we know Serve is running.
	if ft, _ := readFrameHelper(t, parentCtx, a); ft != Hmp1FrameHello {
		t.Fatalf("expected Hello, got 0x%02x", ft)
	}
	if ft, _ := readFrameHelper(t, parentCtx, a); ft != Hmp1FrameStateSync {
		t.Fatalf("expected StateSync, got 0x%02x", ft)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned error on context cancellation: %v", err)
		}
	case <-parentCtx.Done():
		t.Fatal("Serve did not return after context cancellation")
	}

	if pty.IsClosed() {
		t.Errorf("PTY must not be closed by Serve on context cancellation")
	}
}

func TestServeReturnsWhenClientClosesConnection(t *testing.T) {
	t.Parallel()
	pty := internal_testutil.NewTestPty()
	defer pty.Close()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	server := NewHmp1Server(ctx, pty)

	a, b := net.Pipe()
	defer b.Close()

	exitCodeCh := make(chan int32)

	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx, b, exitCodeCh, Hmp1ServerOptions{})
	}()

	if ft, _ := readFrameHelper(t, ctx, a); ft != Hmp1FrameHello {
		t.Fatalf("expected Hello, got 0x%02x", ft)
	}
	if ft, _ := readFrameHelper(t, ctx, a); ft != Hmp1FrameStateSync {
		t.Fatalf("expected StateSync, got 0x%02x", ft)
	}

	_ = a.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned error on client close: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Serve did not return after client closed connection")
	}

	if pty.IsClosed() {
		t.Errorf("PTY must not be closed by Serve on client disconnect")
	}
}

// Verifies that Serve does not wait for an exit code when the client disconnects.
func TestServeDoesNotWaitForExitCodeWhenClientDisconnects(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	exitCodeCh := make(chan int32)
	f := startSession(t, Hmp1ServerOptions{}, exitCodeCh)
	defer f.cleanup()
	f.drainHelloAndStateSync()

	_ = f.a.Close()

	// (not sending an exit code into the channel)

	select {
	case <-f.serveDone:
		require.NoError(t, f.serveErr, "Serve returned error on client disconnect")
	case <-ctx.Done():
		t.Fatal("Serve did not return promptly after client disconnect; the exit-code wait was not skipped")
	}
}

// --- Client frame edge cases ---

func TestServeDropsMalformedResizeFrameAndContinues(t *testing.T) {
	t.Parallel()
	f := startSession(t, Hmp1ServerOptions{}, nil)
	defer f.cleanup()
	f.drainHelloAndStateSync()

	// Malformed: only 4 bytes (need 8).
	writeFrameHelper(t, f.a, Hmp1FrameResize, []byte{0, 0, 0, 0})

	// Followed by a valid Input frame to prove the loop still runs.
	writeFrameHelper(t, f.a, Hmp1FrameInput, []byte("ping"))

	select {
	case got := <-f.pty.Outbound:
		if string(got) != "ping" {
			t.Errorf("PTY input = %q, want %q", got, "ping")
		}
	case <-f.ctx.Done():
		t.Fatal("timed out waiting for Input after malformed Resize")
	}

	resizes := f.pty.ResizesSnapshot()
	if len(resizes) != 0 {
		t.Errorf("expected no PTY resizes, got %+v", resizes)
	}
}

func TestServeIgnoresUnknownFrameTypesAndContinues(t *testing.T) {
	t.Parallel()
	f := startSession(t, Hmp1ServerOptions{}, nil)
	defer f.cleanup()
	f.drainHelloAndStateSync()

	// Unknown frame type (0xFF) with some payload.
	writeFrameHelper(t, f.a, Hmp1FrameType(0xFF), []byte("ignored"))

	// Valid Input must still flow.
	writeFrameHelper(t, f.a, Hmp1FrameInput, []byte("hello"))

	select {
	case got := <-f.pty.Outbound:
		if string(got) != "hello" {
			t.Errorf("PTY input = %q, want %q", got, "hello")
		}
	case <-f.ctx.Done():
		t.Fatal("timed out waiting for Input after unknown frame")
	}
}

func TestServeAcceptsFrameAtMaxPayloadLength(t *testing.T) {
	t.Parallel()
	f := startSession(t, Hmp1ServerOptions{}, nil)
	defer f.cleanup()
	f.drainHelloAndStateSync()

	payload := make([]byte, Hmp1MaxPayloadLength)
	// Write a recognizable pattern at the boundaries to detect truncation.
	payload[0] = 0xAB
	payload[len(payload)-1] = 0xCD

	// Send on a goroutine because net.Pipe is synchronous and the consumer is
	// our testPty.outbound channel of bounded capacity.
	writeDone := make(chan struct{})
	go func() {
		writeFrameHelper(t, f.a, Hmp1FrameInput, payload)
		close(writeDone)
	}()

	select {
	case got := <-f.pty.Outbound:
		if len(got) != len(payload) {
			t.Fatalf("PTY input length = %d, want %d", len(got), len(payload))
		}
		if got[0] != payload[0] || got[len(got)-1] != payload[len(payload)-1] {
			t.Errorf("payload boundary bytes mismatch: first=0x%02x last=0x%02x",
				got[0], got[len(got)-1])
		}
	case <-f.ctx.Done():
		t.Fatal("timed out waiting for max-sized payload to reach PTY")
	}

	select {
	case <-writeDone:
	case <-f.ctx.Done():
		t.Fatal("frame writer did not finish")
	}
}

// --- PTY error propagation ---

var errFakeRead = errors.New("fake pty read failure")
var errFakeWrite = errors.New("fake pty write failure")

func TestServeReturnsErrorOnPtyReadFailure(t *testing.T) {
	t.Parallel()
	pty := internal_testutil.NewTestPty()
	defer pty.Close()
	pty.SetReadErr(errFakeRead)

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	server := NewHmp1Server(ctx, pty)

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx, b, nil, Hmp1ServerOptions{})
	}()

	// Drain bytes from `a` until it closes, so the server's writes don't
	// block on the synchronous net.Pipe. Use a raw drain (not readFrameHelper)
	// so it never calls t.Fatalf after the test goroutine has moved on.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		buf := make([]byte, 4096)
		for {
			if _, err := a.Read(buf); err != nil {
				return
			}
		}
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Serve to return an error")
		}
		if !errors.Is(err, errFakeRead) {
			t.Errorf("expected error wrapping errFakeRead, got %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Serve did not return after PTY read failure")
	}

	_ = a.Close()
	<-drainDone
}

func TestServeReturnsErrorOnPtyWriteFailure(t *testing.T) {
	t.Parallel()
	pty := internal_testutil.NewTestPty()
	defer pty.Close()
	pty.SetWriteErr(errFakeWrite)

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	server := NewHmp1Server(ctx, pty)

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx, b, nil, Hmp1ServerOptions{})
	}()

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		buf := make([]byte, 4096)
		for {
			if _, err := a.Read(buf); err != nil {
				return
			}
		}
	}()

	// Send an Input frame that will hit the failing Write on the server side.
	writeFrameHelper(t, a, Hmp1FrameInput, []byte("trigger"))

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Serve to return an error")
		}
		if !errors.Is(err, errFakeWrite) {
			t.Errorf("expected error wrapping errFakeWrite, got %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Serve did not return after PTY write failure")
	}

	_ = a.Close()
	<-drainDone
}

// --- Output framing ---

func TestServeEmitsSeparateOutputFramesForSeparatePtyReads(t *testing.T) {
	t.Parallel()
	f := startSession(t, Hmp1ServerOptions{}, nil)
	defer f.cleanup()
	f.drainHelloAndStateSync()

	f.pty.Inbound <- []byte("alpha")
	f.pty.Inbound <- []byte("beta")

	ft, payload := readFrameHelper(t, f.ctx, f.a)
	if ft != Hmp1FrameOutput || string(payload) != "alpha" {
		t.Fatalf("frame 1: type=0x%02x payload=%q", ft, payload)
	}
	ft, payload = readFrameHelper(t, f.ctx, f.a)
	if ft != Hmp1FrameOutput || string(payload) != "beta" {
		t.Fatalf("frame 2: type=0x%02x payload=%q", ft, payload)
	}
}

func errorMessageContains(err error, substr string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), substr)
}

// readFrameHelper performs a blocking read of one HMP v1 frame from r,
// failing the test on error.
func readFrameHelper(t *testing.T, ctx context.Context, conn net.Conn) (Hmp1FrameType, []byte) {
	t.Helper()

	var header [5]byte
	r := usvc_io.NewContextReader(ctx, conn, false /* don't auto-close */)

	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("connection closed while expecting frame")
		}
		t.Fatalf("read header: %v", err)
	}
	t.Logf("got frame type=0x%02x len=%d", header[0], binary.LittleEndian.Uint32(header[1:5]))
	length := binary.LittleEndian.Uint32(header[1:5])
	if length == 0 {
		return Hmp1FrameType(header[0]), nil
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return Hmp1FrameType(header[0]), payload
}

func writeFrameHelper(t *testing.T, w net.Conn, ft Hmp1FrameType, payload []byte) {
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
