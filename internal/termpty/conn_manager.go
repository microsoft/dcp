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
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/pkg/resiliency"
)

const (
	// connInitialBackoff and acceptMaxBackoff bound the exponential
	// retry schedule applied to transient Accept (listen mode) and Dial
	// (connect mode) failures. The backoff state is reset after each
	// successful Accept/Dial, so unrelated bursts of errors do not accumulate.
	connInitialBackoff = 100 * time.Millisecond
	connMaxBackoff     = 5 * time.Second

	// reconnectMinInterval bounds how frequently the manager will re-dial the peer in connect mode.
	reconnectMinInterval = 500 * time.Millisecond
)

// SocketMode selects how a ConnManager establishes the HMP v1 connection over its Unix domain socket.
type SocketMode int

const (
	// SocketModeListen makes the manager listen on the socket and accept a client connection.
	SocketModeListen SocketMode = iota

	// SocketModeConnect makes the manager assume the client is already listening on the socket
	// and connect (dial) to it.
	SocketModeConnect
)

// ConnManager bridges a running PseudoTerminalProcess to external HMP v1 clients
// over a Unix domain socket. For each connection it establishes, it runs a
// Hmp1Server.Serve loop that ferries terminal I/O between the PTY and the client.
// At most one client connection is served at a time.
//
// The manager's lifetime is bound to the lifetime context passed to
// NewConnManager and to the lifetime of the underlying process. When either
// the lifetime context is cancelled or the process exits, the manager closes
// the listener (in listen mode), terminates any active Serve loop, and transitions
// to a final, dormant state observable via Done().
//
// ConnManager does NOT own the PTY or the process: it neither closes the PTY
// nor reaps the process. The caller that produced the PseudoTerminalProcess
// retains those responsibilities.
type ConnManager struct {
	log        logr.Logger
	socketPath string
	socketMode SocketMode

	ptp    *PseudoTerminalProcess
	server *Hmp1Server

	// Only used when socketMode == SocketModeListen
	listener *net.UnixListener

	// ctx is an internal context cancelled when the manager begins to shut
	// down (because the lifetime context was cancelled, because the process
	// exited, or because shutdown was triggered for some other reason).
	// It supervises the Hmp1Server and every Serve invocation.
	ctx       context.Context
	cancelCtx context.CancelFunc

	// Runs shutdown code at most once.
	shutdownOnce func()

	// done is closed when the accept loop has fully terminated, which
	// happens after any in-progress Serve has returned and the listener has
	// been closed.
	done chan struct{}
}

// NewConnManager creates a connection manager that bridges HMP v1 client
// traffic on socketPath to ptp's pseudo-terminal.
func NewConnManager(
	lifetimeCtx context.Context,
	ptp *PseudoTerminalProcess,
	socketPath string,
	mode SocketMode,
	log logr.Logger,
) (*ConnManager, error) {
	if lifetimeCtx == nil {
		return nil, errors.New("lifetime context must not be nil")
	}
	if ptp == nil {
		return nil, errors.New("PseudoTerminalProcess must not be nil")
	}
	if ptp.PTY == nil {
		return nil, errors.New("PseudoTerminalProcess.PTY must not be nil")
	}
	if ptp.ExitHandler == nil {
		return nil, errors.New("PseudoTerminalProcess.ExitHandler must not be nil")
	}
	if socketPath == "" {
		return nil, errors.New("socket path must not be empty")
	}

	var listener *net.UnixListener
	if mode == SocketModeListen {
		boundListener, listenErr := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
		if listenErr != nil {
			return nil, fmt.Errorf("could not create terminal socket at %s: %w", socketPath, listenErr)
		}

		// Socket file lifecycle is owned by the caller, not by ConnManager.
		// Without this opt-out, net.UnixListener.Close() would unlink the path.
		boundListener.SetUnlinkOnClose(false)

		listener = boundListener
	}

	ctx, cancelCtx := context.WithCancel(context.Background())

	cm := &ConnManager{
		log:        log,
		socketPath: socketPath,
		socketMode: mode,
		ptp:        ptp,
		listener:   listener,
		ctx:        ctx,
		cancelCtx:  cancelCtx,

		done: make(chan struct{}),
	}
	cm.server = NewHmp1Server(ctx, ptp.PTY)
	cm.shutdownOnce = sync.OnceFunc(cm.shutdown)

	if mode == SocketModeConnect {
		cm.log.V(1).Info("Terminal connection manager will dial peer", "SocketPath", socketPath)
	} else {
		cm.log.V(1).Info("Terminal connection manager is listening", "SocketPath", socketPath)
	}

	context.AfterFunc(lifetimeCtx, cm.shutdownOnce)
	go cm.watchProcessExit()
	go cm.connLoop()

	return cm, nil
}

// SocketPath returns the filesystem path of the Unix domain socket the
// manager is (or was) listening on.
func (cm *ConnManager) SocketPath() string {
	return cm.socketPath
}

// Done returns a channel that is closed once the manager has fully
// transitioned to its final dormant state: the listener is closed, no Serve
// loop is in flight, and no new connections will be accepted.
func (cm *ConnManager) Done() <-chan struct{} {
	return cm.done
}

// watchProcessExit triggers shutdown when the underlying process exits.
// It also returns silently if the manager is shut down for any other reason
// (lifetime context cancellation, explicit shutdown) so it does not leak even
// if the process outlives the manager.
func (cm *ConnManager) watchProcessExit() {
	select {
	case <-cm.ptp.ExitHandler.Exited():
		cm.log.V(1).Info("Terminal process exited; shutting down connection manager", "SocketPath", cm.socketPath)
		cm.shutdownOnce()
	case <-cm.ctx.Done():
		// Shutdown already in progress.
	}
}

// connLoop establishes client connections and serves them one at a time.
func (cm *ConnManager) connLoop() {
	defer close(cm.done)

	var lastConnectAttempt time.Time
	for {
		if cm.socketMode == SocketModeConnect && !lastConnectAttempt.IsZero() {
			if !cm.waitBeforeRedial(lastConnectAttempt) {
				return
			}
		}
		lastConnectAttempt = time.Now()

		conn, err := cm.nextConn()
		if err != nil {
			// Either the listener has been closed / the dial was abandoned
			// (shutdown is in progress) or the internal context was cancelled.
			// In all cases the loop is done.
			return
		}

		cm.serveConnection(conn)
	}
}

// waitBeforeRedial blocks until connectReconnectMinInterval has elapsed since
// lastAttempt, returning true once it is safe to re-dial. It returns false if
// the manager is shutting down (internal context cancelled) before then.
func (cm *ConnManager) waitBeforeRedial(lastAttempt time.Time) bool {
	wait := reconnectMinInterval - time.Since(lastAttempt)
	if wait <= 0 {
		return true
	}

	select {
	case <-time.After(wait):
		return true
	case <-cm.ctx.Done():
		return false
	}
}

// nextConn obtains the next client connection according to the manager's mode.
func (cm *ConnManager) nextConn() (net.Conn, error) {
	if cm.socketMode == SocketModeConnect {
		return cm.dialOne()
	}
	return cm.acceptOne()
}

// acceptOne accepts a single client connection, retrying transient errors with bounded exponential backoff.
func (cm *ConnManager) acceptOne() (*net.UnixConn, error) {
	var conn *net.UnixConn

	retryErr := resiliency.Retry(cm.ctx, createConnBackoff(), func() error {
		var acceptErr error
		conn, acceptErr = cm.listener.AcceptUnix()
		if acceptErr == nil {
			return nil
		}

		// net.ErrClosed is the expected outcome once shutdown has closed
		// the listener: stop retrying immediately. We do not log this at
		// Error level because it is a normal shutdown signal, not a failure.
		if errors.Is(acceptErr, net.ErrClosed) {
			return resiliency.Permanent(acceptErr)
		}

		cm.log.Error(acceptErr, "Accept on terminal socket failed", "SocketPath", cm.socketPath)
		return acceptErr
	})

	if retryErr != nil {
		return nil, retryErr
	}
	return conn, nil
}

// dialOne dials the peer's socket, retrying transient failures with bounded exponential backoff.
func (cm *ConnManager) dialOne() (net.Conn, error) {
	var conn net.Conn
	dialer := net.Dialer{}

	retryErr := resiliency.Retry(cm.ctx, createConnBackoff(), func() error {
		var dialErr error
		conn, dialErr = dialer.DialContext(cm.ctx, "unix", cm.socketPath)
		if dialErr == nil {
			return nil
		}

		// Context cancellation means shutdown is in progress: stop retrying.
		// This is a normal shutdown signal, not a failure, so we do not log it.
		if cm.ctx.Err() != nil || errors.Is(dialErr, context.Canceled) {
			return resiliency.Permanent(dialErr)
		}

		cm.log.V(1).Info("Dial of terminal socket failed; will retry",
			"SocketPath", cm.socketPath, "error", dialErr.Error())
		return dialErr
	})

	if retryErr != nil {
		return nil, retryErr
	}
	return conn, nil
}

func createConnBackoff() backoff.BackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(connInitialBackoff),
		backoff.WithMaxInterval(connMaxBackoff),
		// No max elapsed time: retry until the lifetime context is cancelled
		// (signalled by resiliency.Retry returning a context error).
		backoff.WithMaxElapsedTime(0),
	)
}

// serveConnection runs an Hmp1Server Serve loop on the given connection, feeding the
// process exit code into the server as soon as it becomes available.
func (cm *ConnManager) serveConnection(conn net.Conn) {
	// Buffered so that the feeder goroutine can deposit the exit code
	// without blocking even if the server has not yet reached the exit-code select.
	exitCodeCh := make(chan int32, 1)

	serveCtx, serveCancel := context.WithCancel(cm.ctx)
	defer serveCancel()

	wg := sync.WaitGroup{}
	wg.Add(1)

	go func() {
		defer wg.Done()
		defer close(exitCodeCh)

		// Wait for either the process to exit or the Serve loop to end.
		select {
		case <-cm.ptp.ExitHandler.Exited():
			ec := cm.ptp.ExitHandler.ExitInfo().ExitCode
			select {
			case exitCodeCh <- ec:
			case <-time.After(Hmp1ExitCodeWaitTimeout):
				// Server has abandoned its exit-code read; close still
				// happens via the deferred close below.
			}

		case <-serveCtx.Done():
			// Process has not exited but we are done serving.
		}
	}()

	serveErr := cm.server.Serve(serveCtx, conn, exitCodeCh, Hmp1ServerOptions{Log: cm.log})
	if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		cm.log.Error(serveErr, "HMP1 Serve returned an error")
	}

	serveCancel()

	// Make sure the exit code feeder goroutine has finished before we return.
	wg.Wait()
}

// shutdown() is the manager's idempotent shutdown sequence. It cancels the
// internal context (which terminates any in-progress Serve loop or pending
// dial, and the underlying Hmp1Server's PTY reader worker) and, in listen mode,
// closes the listener (which unblocks Accept). The socket file at socketPath is
// intentionally left in place — in listen mode the caller owns its lifecycle,
// and in connect mode the peer owns it.
// Note: cm.done is closed when connLoop() exits.
func (cm *ConnManager) shutdown() {
	cm.log.V(1).Info("Shutting down terminal connection manager", "SocketPath", cm.socketPath)

	cm.cancelCtx()

	if cm.listener != nil {
		if closeErr := cm.listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			cm.log.V(1).Info("Closing terminal socket listener failed", "error", closeErr.Error())
		}
	}
}
