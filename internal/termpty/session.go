/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/internal/hmp1"
)

// Session owns the lifecycle of a single PTY-attached process plus the
// HMP v1 listener that bridges viewer connections to that PTY. It is shared
// between the executable runner (one Session per executable replica started
// under WithTerminal) and the container reconciler (one Session per
// container started under WithTerminal).
//
// When the underlying process exits the session sends a final Exit frame to
// any connected viewer (via hmp1.Serve's waitExit hook), then tears down the
// listener. Calling Close() forces the same teardown.
type Session struct {
	log      logr.Logger
	listener net.Listener
	udsPath  string
	cols     int
	rows     int

	tp *Process

	mu       sync.Mutex
	currConn net.Conn
	closed   atomic.Bool

	// Tracks active handleConn goroutines so graceful shutdown can wait for
	// in-flight HMP1 sessions to flush their Exit frame before closing the
	// listener.
	connWg sync.WaitGroup

	// exitDone is closed once the underlying PTY-attached process has exited;
	// exitCode then holds the observed exit code. This is the signal used by
	// each handleConn's waitExit hook so that hmp1.Serve sends the real exit
	// code on the per-connection Exit frame.
	exitOnce sync.Once
	exitCode atomic.Int32
	exitDone chan struct{}

	doneCh chan struct{}
}

// StartSession opens a listener at cfg.UDSPath, then spawns an accept loop
// that runs hmp1.Serve on each accepted connection. The PTY-attached process
// must already be running (passed via tp); the session owns tp from this
// point forward and will close it on shutdown.
func StartSession(ctx context.Context, cfg SessionConfig, tp *Process, log logr.Logger) (*Session, error) {
	udsPath := cfg.UDSPath
	// Best-effort cleanup of a stale socket file from a previous run.
	if _, statErr := os.Stat(udsPath); statErr == nil {
		_ = os.Remove(udsPath)
	}

	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "unix", udsPath)
	if err != nil {
		_ = tp.PTY.Close()
		return nil, fmt.Errorf("failed to listen on terminal UDS %q: %w", udsPath, err)
	}

	cols, rows := normalizeDimensions(cfg.Cols, cfg.Rows)

	s := &Session{
		log:      log.WithValues("UDSPath", udsPath, "PID", tp.PID),
		listener: lis,
		udsPath:  udsPath,
		cols:     cols,
		rows:     rows,
		tp:       tp,
		exitDone: make(chan struct{}),
		doneCh:   make(chan struct{}),
	}

	go s.acceptLoop(ctx)
	go s.watchExit()

	s.log.Info("Terminal session listening")
	return s, nil
}

// acceptLoop accepts incoming connections one at a time. If a new connection
// arrives while a previous one is still active, the previous connection is
// closed (last-writer-wins), matching the single-viewer model of the initial
// slice.
func (s *Session) acceptLoop(ctx context.Context) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if !s.closed.Load() && !errors.Is(err, net.ErrClosed) {
				s.log.Error(err, "Terminal session accept loop terminated unexpectedly")
			}
			return
		}

		// Take over the "current connection" slot, closing any prior one.
		s.mu.Lock()
		if prev := s.currConn; prev != nil {
			s.log.V(1).Info("Closing previous terminal viewer connection in favor of new one")
			_ = prev.Close()
		}
		s.currConn = conn
		s.mu.Unlock()

		s.connWg.Add(1)
		go s.handleConn(ctx, conn)
	}
}

func (s *Session) handleConn(ctx context.Context, conn net.Conn) {
	defer s.connWg.Done()
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		if s.currConn == conn {
			s.currConn = nil
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
	// returns the observed exit code, so the per-connection Exit frame
	// carries the real code rather than a synthetic 0.
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

// watchExit blocks until the PTY process exits, then performs a graceful
// teardown: it publishes the exit code, closes the PTY (so any in-flight
// hmp1.Serve invocation drains and sends its Exit frame), waits a bounded
// amount of time for those handlers to finish, then closes the listener and
// removes the UDS file.
func (s *Session) watchExit() {
	defer close(s.doneCh)
	exitCode := s.tp.WaitExit()
	s.log.Info("Terminal-attached process exited", "exitCode", exitCode)
	s.signalProcessExit(exitCode)

	// Close the PTY to wake up any blocked Serve PTY pump. After this, Serve
	// will drain, call waitProcessExit (already unblocked), write the Exit
	// frame, and return.
	if s.tp != nil && s.tp.PTY != nil {
		_ = s.tp.PTY.Close()
	}

	// Give in-flight HMP v1 handlers a moment to flush their Exit frame
	// before we slam the listener shut.
	s.waitConnsOrTimeout(2 * time.Second)

	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	_ = s.listener.Close()
	_ = os.Remove(s.udsPath)
}

// waitConnsOrTimeout blocks until all in-flight handleConn goroutines have
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
		s.log.V(1).Info("Timed out waiting for HMP v1 handlers to drain; forcing teardown")
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

// Close stops the listener, closes any active connection, closes the PTY
// master, and removes the UDS file. Safe to call multiple times.
//
// Unlike watchExit's graceful path, Close interrupts in-flight HMP v1 handlers
// immediately. Use it when an external caller (e.g. controller-runtime
// reconciliation tearing down the run) wants the listener gone NOW.
func (s *Session) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}

	// Best-effort: publish a 0 exit code if we don't have a real one yet, so
	// any in-flight Serve invocations don't block forever inside waitExit.
	s.signalProcessExit(0)

	var firstErr error
	if err := s.listener.Close(); err != nil {
		firstErr = err
	}

	s.mu.Lock()
	conn := s.currConn
	s.currConn = nil
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}

	if s.tp != nil && s.tp.PTY != nil {
		if err := s.tp.PTY.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Best-effort UDS file cleanup.
	_ = os.Remove(s.udsPath)
	return firstErr
}
