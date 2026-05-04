/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/internal/hmp1"
)

// Session owns the lifecycle of a single PTY-attached process plus a single
// HMP v1 connection https://github.com/mitchdenny/hex1b/blob/main/docs/muxer-protocol.md
// that bridges that PTY to the HMP-compatible terminal host via an Unix domain socket (UDS) file.
//
// The session is started by connecting to the UDS socket.
// DCP assumes that the terminal host owns the UDS file and has already created it.

// When the underlying process exits the session sends a final Exit frame to
// the connected viewer (via hmp1.Serve's waitExit hook), then closes the
// connection. Calling Close() forces the same teardown.

type Session struct {
	log     logr.Logger
	udsPath string
	cols    int
	rows    int

	tp *PseudoTerminalProcess

	mu   sync.Mutex
	conn net.Conn

	// Tracks the in-flight handleConn goroutine so graceful shutdown can
	// wait for the HMP1 session to flush its Exit frame before closing the
	// connection.
	connWg sync.WaitGroup

	// exitDone is closed once the underlying PTY-attached process has exited;
	// exitCode then holds the observed exit code. This is the signal used by
	// handleConn's waitExit hook so that hmp1.Serve sends the real exit code
	// on the Exit frame.
	exitOnce sync.Once
	exitCode atomic.Int32
	exitDone chan struct{}

	// stopCh is closed by Close() to request immediate teardown. watchExit
	// is the sole owner of the PTY/conn close sequence and reads stopCh to
	// distinguish a natural process exit from an explicit stop request.
	// stopOnce guards the close(stopCh) call so Close() is idempotent.
	stopOnce sync.Once
	stopCh   chan struct{}

	// doneCh is closed by watchExit's defer when the session has finished
	// tearing itself down. Both Close() and Done() observe it.
	doneCh chan struct{}
}

// dialUDSWithRetry dials the given UDS path, retrying briefly to absorb the
// race where the terminal host has reported running but its listener has not
// yet bound. The WaitAnnotation in the resource model already sequences the
// terminal host before any resource that uses it, so this retry budget is
// only meant to cover the small window between the host process declaring
// itself ready and its listener actually existing on disk.
func dialUDSWithRetry(ctx context.Context, udsPath string) (net.Conn, error) {
	const (
		attempts = 50
		every    = 100 * time.Millisecond
	)
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var d net.Dialer
		dialCtx, cancel := context.WithTimeout(ctx, every)
		conn, err := d.DialContext(dialCtx, "unix", udsPath)
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
	return nil, fmt.Errorf("dial %q after %d attempts: %w", udsPath, attempts, lastErr)
}

// StartSession dials cfg.UDSPath, then spawns an HMP v1 server on that
// connection. The PTY-attached process must already be running (passed via
// tp); the session owns tp from this point forward and will close it on
// shutdown.
func StartSession(ctx context.Context, cfg SessionConfig, tp *PseudoTerminalProcess, log logr.Logger) (*Session, error) {
	udsPath := cfg.UDSPath

	conn, err := dialUDSWithRetry(ctx, udsPath)
	if err != nil {
		_ = tp.PTY.Close()
		return nil, fmt.Errorf("failed to dial terminal UDS %q: %w", udsPath, err)
	}

	cols, rows := normalizeDimensions(cfg.Cols, cfg.Rows)

	s := &Session{
		log:      log.WithValues("UDSPath", udsPath, "PID", tp.PID),
		udsPath:  udsPath,
		cols:     cols,
		rows:     rows,
		tp:       tp,
		conn:     conn,
		exitDone: make(chan struct{}),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}

	s.connWg.Add(1)
	go s.handleConn(ctx, conn)
	go s.watchExit(ctx)

	s.log.Info("Terminal session connected to host")
	return s, nil
}

func (s *Session) handleConn(ctx context.Context, conn net.Conn) {
	defer s.connWg.Done()
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
		}
		s.mu.Unlock()
	}()

	opts := hmp1.ServerOptions{
		InitialCols: s.cols,
		InitialRows: s.rows,
		Log:         s.log,
	}

	// waitExit is invoked by hmp1.Serve after its PTY read pump returns
	// (i.e. when pty.Close has been called or an upstream EOF was observed).
	// It blocks until the underlying process exit has been signalled and
	// returns the observed exit code, so the Exit frame carries the real
	// code rather than a synthetic 0.
	if serveErr := hmp1.Serve(ctx, conn, s.tp.PTY, s.waitProcessExit, opts); serveErr != nil {
		s.log.V(1).Info("HMP v1 serve exited with error", "err", serveErr.Error())
	}
}

// signalProcessExit publishes the process exit code; subsequent calls to
// waitProcessExit return immediately. Safe to call multiple times; only the
// first call wins.
func (s *Session) signalProcessExit(code int32) {
	s.exitOnce.Do(func() {
		s.exitCode.Store(code)
		close(s.exitDone)
	})
}

// waitProcessExit blocks until the underlying process has exited, then
// returns the observed exit code. Used as the waitExit callback for hmp1.Serve.
func (s *Session) waitProcessExit() int32 {
	<-s.exitDone
	return s.exitCode.Load()
}

// watchExit is the sole owner of session teardown. It runs concurrently with
// the in-flight HMP v1 handler and is the only goroutine that ever closes the
// PTY handles or the connection. It blocks until either:
//
//   - the underlying PTY-attached process exits naturally (the inner wait
//     goroutine produces an exit code on waitDone), or
//   - Close() requests an explicit teardown by closing stopCh.
//
// In both cases watchExit calls PTY.Close exactly once, drains the inner wait
// goroutine, gives the in-flight HMP v1 handler a bounded window to flush its
// Exit frame, then closes the connection. The single-owner pattern eliminates
// the historical race where Close() and watchExit() both closed the conpty
// handles concurrently with cp.Wait's WaitForSingleObject(pi.Process) loop —
// that race could close a stale handle that Windows had already recycled for
// another component in the same process (e.g. DCP's process cleanup job
// handle), cascade-killing every job-assigned sibling resource.
//
// The UDS file itself is owned by the terminal host (the listener side) and
// is not removed here.
func (s *Session) watchExit(ctx context.Context) {
	defer close(s.doneCh)

	// Isolate the WaitForSingleObject call to a single goroutine so no other
	// code path observes pi.Process. It will return when the process exits
	// naturally or when PTY.Close (called by us, below) closes pi.Process.
	waitDone := make(chan int32, 1)
	go func() {
		waitDone <- s.tp.WaitExit(ctx)
	}()

	var (
		exitCode    int32
		waitDrained bool
	)
	select {
	case exitCode = <-waitDone:
		waitDrained = true
		s.log.Info("Terminal-attached process exited", "exitCode", exitCode)
	case <-s.stopCh:
		s.log.V(1).Info("Terminal session received explicit stop request")
	}

	// Single PTY.Close call. This kills the conhost (and the attached child
	// if it was still running) and closes all conpty handles. After this,
	// pty.Read in hmp1.Serve's read pump will fail and the in-flight handler
	// will unwind so we can close the connection.
	if s.tp != nil && s.tp.PTY != nil {
		_ = s.tp.PTY.Close()
	}

	// Drain the inner wait goroutine if we took the stopCh path. Closing the
	// PTY caused pi.Process to be closed; the wait will return with
	// WAIT_FAILED / ERROR_INVALID_HANDLE and the closure in pty_windows.go
	// will report -1.
	if !waitDrained {
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			s.log.V(1).Info("Timed out waiting for PTY wait goroutine to drain")
		}
	}

	s.signalProcessExit(exitCode)

	// Give the in-flight HMP v1 handler a moment to flush its Exit frame
	// before we slam the connection shut.
	s.waitConnsOrTimeout(2 * time.Second)

	s.mu.Lock()
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// waitConnsOrTimeout blocks until the in-flight handleConn goroutine has
// finished, or until the timeout elapses, whichever comes first.
func (s *Session) waitConnsOrTimeout(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		s.connWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		s.log.V(1).Info("Timed out waiting for HMP v1 handler to drain; forcing teardown")
	}
}

// ExitCode returns the observed process exit code if the underlying process
// has exited; the second return value is true if the exit has been observed,
// false otherwise. Safe to call concurrently with watchExit.
func (s *Session) ExitCode() (int32, bool) {
	select {
	case <-s.exitDone:
		return s.exitCode.Load(), true
	default:
		return 0, false
	}
}

// Done returns a channel that is closed once the underlying process exits and
// the session has finished cleaning up.
func (s *Session) Done() <-chan struct{} {
	return s.doneCh
}

// Close requests an immediate teardown of the session and blocks until
// watchExit has finished its single-owner cleanup of the PTY and connection.
// Safe to call multiple times.
//
// Unlike watchExit's natural-exit path, Close interrupts the in-flight HMP v1
// handler immediately. Use it when an external caller (e.g.
// controller-runtime reconciliation tearing down the run) wants the session
// gone NOW. The UDS file is owned by the terminal host and is not removed
// here.
//
// The bounded wait protects the caller against a stuck teardown (e.g. the
// inner WaitForSingleObject hanging on a kernel handle that never signals);
// in practice teardown completes well within the timeout.
func (s *Session) Close() error {
	// Best-effort: publish a 0 exit code if we don't have a real one yet, so
	// any in-flight Serve invocation doesn't block forever inside waitExit.
	s.signalProcessExit(0)

	// Trigger watchExit's stop branch. Idempotent.
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})

	// Wait for watchExit to complete teardown. We do not touch any conpty
	// handles or the connection ourselves — that is watchExit's exclusive
	// responsibility.
	const closeTimeout = 5 * time.Second
	select {
	case <-s.doneCh:
	case <-time.After(closeTimeout):
		s.log.Error(nil, "Timed out waiting for terminal session teardown", "timeout", closeTimeout.String())
	}

	return nil
}
