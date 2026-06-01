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
	// acceptInitialBackoff and acceptMaxBackoff bound the exponential
	// retry schedule applied to transient Accept failures. The backoff
	// state is reset after each successful Accept, so unrelated bursts of
	// errors do not accumulate.
	acceptInitialBackoff = 100 * time.Millisecond
	acceptMaxBackoff     = 5 * time.Second
)

// ConnManager bridges a running PseudoTerminalProcess to external HMP v1 clients
// over a Unix domain socket. It listens on the socket and, for each accepted
// connection, runs a Hmp1Server.Serve loop that ferries terminal I/O between
// the PTY and the client.
//
// At most one client connection is served at a time; while a connection is
// active subsequent client connect() calls are queued by the OS and picked up
// once the active session ends.
//
// The manager's lifetime is bound to the lifetime context passed to
// NewConnManager and to the lifetime of the underlying process. When either
// the lifetime context is cancelled or the process exits, the manager closes
// the listener, terminates any active Serve loop, and transitions to a final,
// dormant state observable via Done().
//
// ConnManager does NOT own the PTY or the process: it neither closes the PTY
// nor reaps the process. The caller that produced the PseudoTerminalProcess
// retains those responsibilities.
type ConnManager struct {
	log        logr.Logger
	socketPath string

	ptp    *PseudoTerminalProcess
	server *Hmp1Server

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

// NewConnManager creates a connection manager that listens for HMP v1 client
// connections on socketPath and bridges them to ptp's pseudo-terminal.
//
// lifetimeCtx supervises the manager. When it is cancelled the manager shuts
// down gracefully. The manager also shuts down on its own when the process
// described by ptp exits.
//
// The supplied logger is used for all diagnostic output (both by the manager
// and by the Hmp1Server it owns). A zero-value logr.Logger is acceptable.
//
// ConnManager does not own the lifecycle of the socket file at socketPath: it
// will fail if the path is already bound at startup, and it will leave the
// file in place on shutdown. The caller is responsible for ensuring the path
// is free before NewConnManager is called and for removing the file once the
// manager has shut down.
func NewConnManager(
	lifetimeCtx context.Context,
	ptp *PseudoTerminalProcess,
	socketPath string,
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

	listener, listenErr := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if listenErr != nil {
		return nil, fmt.Errorf("could not create terminal socket at %s: %w", socketPath, listenErr)
	}
	// Socket file lifecycle is owned by the caller, not by ConnManager. Without
	// this opt-out, net.UnixListener.Close() would unlink the path because Go
	// created it via ListenUnix.
	listener.SetUnlinkOnClose(false)

	ctx, cancelCtx := context.WithCancel(context.Background())

	cm := &ConnManager{
		log:        log,
		socketPath: socketPath,
		ptp:        ptp,
		listener:   listener,
		ctx:        ctx,
		cancelCtx:  cancelCtx,

		done: make(chan struct{}),
	}
	cm.server = NewHmp1Server(ctx, ptp.PTY)
	cm.shutdownOnce = sync.OnceFunc(cm.shutdown)

	context.AfterFunc(lifetimeCtx, cm.shutdownOnce)
	go cm.watchProcessExit()
	go cm.acceptLoop()

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

// acceptLoop accepts client connections one at a time. Each accepted
// connection is serviced inline by serveConnection before returning to
// Accept, which gives us the single-connection-at-a-time guarantee without
// any additional synchronization.
func (cm *ConnManager) acceptLoop() {
	defer close(cm.done)

	for {
		conn, err := cm.acceptOne()
		if err != nil {
			// Either the listener has been closed (shutdown is in
			// progress) or the internal context was cancelled. In both
			// cases the loop is done; the underlying error was logged at
			// Error level by the retry operation when it first occurred.
			return
		}

		cm.serveConnection(conn)
	}
}

// acceptOne accepts a single client connection, retrying transient errors
// with bounded exponential backoff. It returns the accepted connection on
// success, or an error (and a nil connection) when the manager is shutting
// down — either because the listener was closed or because the internal
// context was cancelled.
func (cm *ConnManager) acceptOne() (*net.UnixConn, error) {
	b := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(acceptInitialBackoff),
		backoff.WithMaxInterval(acceptMaxBackoff),
		// No max elapsed time: retry until the lifetime context is
		// cancelled or the listener is closed (signalled as a permanent
		// error below).
		backoff.WithMaxElapsedTime(0),
	)

	var conn *net.UnixConn
	retryErr := resiliency.Retry(cm.ctx, b, func() error {
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
// internal context (which terminates any in-progress Serve loop and the
// underlying Hmp1Server's PTY reader worker) and closes the listener (which
// unblocks Accept). The socket file at socketPath is intentionally left in
// place — the caller owns its lifecycle.
// Note: cm.done is closed when acceptLoop() exits.
func (cm *ConnManager) shutdown() {
	cm.log.V(1).Info("Shutting down terminal connection manager", "SocketPath", cm.socketPath)

	cm.cancelCtx()

	if closeErr := cm.listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		cm.log.V(1).Info("Closing terminal socket listener failed", "error", closeErr.Error())
	}
}
