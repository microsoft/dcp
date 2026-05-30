/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/resiliency"
)

const (
	// dialInitialBackoff and dialMaxBackoff bound the exponential retry
	// schedule applied to transient dial failures (e.g. the terminal host
	// has not yet bound its listener). The backoff state is reset after a
	// successful dial.
	dialInitialBackoff = 100 * time.Millisecond
	dialMaxBackoff     = 5 * time.Second
)

// ConnManager bridges a running PseudoTerminalProcess to an external HMP v1
// server over a Unix domain socket. DCP DIALS the socket at socketPath; the
// peer (the Aspire terminal host) owns the listener and the socket file
// lifecycle. Once dialed, the manager runs a Hmp1Server.Serve loop that
// ferries terminal I/O between the PTY and the peer for the lifetime of that
// one connection.
//
// Only one connection is established per ConnManager. If the connection is
// lost the manager shuts down rather than re-dialing; reconnection (and any
// multi-peer behavior) is the responsibility of the terminal host.
//
// The manager's lifetime is bound to the lifetime context passed to
// NewConnManager and to the lifetime of the underlying process. When either
// the lifetime context is cancelled or the process exits, the manager
// terminates the in-progress Serve loop and transitions to a final, dormant
// state observable via Done().
//
// ConnManager does NOT own the PTY or the process: it neither closes the PTY
// nor reaps the process. The caller that produced the PseudoTerminalProcess
// retains those responsibilities.
type ConnManager struct {
	log        logr.Logger
	socketPath string

	ptp    *PseudoTerminalProcess
	server *Hmp1Server

	// ctx is an internal context cancelled when the manager begins to shut
	// down (because the lifetime context was cancelled, because the process
	// exited, or because shutdown was triggered for some other reason).
	// It supervises the Hmp1Server and every Serve invocation.
	ctx       context.Context
	cancelCtx context.CancelFunc

	// dispose ensures shutdown runs at most once.
	dispose *concurrency.OneTimeJob[struct{}]

	// done is closed when the dial+serve sequence has fully terminated,
	// which happens after any in-progress Serve has returned.
	done chan struct{}
}

// NewConnManager creates a connection manager that DIALS the HMP v1 listener
// at socketPath and bridges the resulting connection to ptp's pseudo-terminal.
// The peer (the Aspire terminal host) is expected to have bound a Unix domain
// socket listener at socketPath; the dial is retried with bounded exponential
// backoff to absorb the race between the peer's listener becoming ready and
// the process starting up.
//
// lifetimeCtx supervises the manager. When it is cancelled the manager shuts
// down gracefully. The manager also shuts down on its own when the process
// described by ptp exits.
//
// The supplied logger is used for all diagnostic output (both by the manager
// and by the Hmp1Server it owns). A zero-value logr.Logger is acceptable.
//
// ConnManager does not own the lifecycle of the socket file at socketPath:
// the peer (the listener side) created it and is responsible for removing it
// once the session ends.
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

	ctx, cancelCtx := context.WithCancel(context.Background())

	cm := &ConnManager{
		log:        log,
		socketPath: socketPath,
		ptp:        ptp,
		ctx:        ctx,
		cancelCtx:  cancelCtx,
		dispose:    concurrency.NewOneTimeJob[struct{}](),
		done:       make(chan struct{}),
	}
	cm.server = NewHmp1Server(ctx, ptp.PTY)

	context.AfterFunc(lifetimeCtx, cm.shutdown)
	go cm.watchProcessExit()
	go cm.dialAndServe()

	return cm, nil
}

// SocketPath returns the filesystem path of the Unix domain socket the
// manager is dialing (or has dialed).
func (cm *ConnManager) SocketPath() string {
	return cm.socketPath
}

// Done returns a channel that is closed once the manager has fully
// transitioned to its final dormant state: no Serve loop is in flight and no
// further dial attempts will be made.
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
		cm.shutdown()
	case <-cm.ctx.Done():
		// Shutdown already in progress.
	}
}

// dialAndServe dials the peer's HMP v1 listener (retrying transient failures
// with bounded exponential backoff) and runs a single Serve loop on the
// resulting connection. When the Serve loop ends — because the process
// exited, the peer disconnected, or the manager was shut down — done is
// closed and the manager terminates. There is no automatic re-dial.
func (cm *ConnManager) dialAndServe() {
	defer close(cm.done)

	conn, err := cm.dialOne()
	if err != nil {
		// The lifetime context was cancelled (or shutdown triggered)
		// before a successful dial. Underlying errors were logged at
		// Error level inside dialOne.
		return
	}

	cm.serveConnection(conn)
}

// dialOne dials cm.socketPath, retrying transient errors with bounded
// exponential backoff. It returns the connected socket on success, or an
// error (and a nil connection) when the manager is shutting down before a
// successful dial.
func (cm *ConnManager) dialOne() (net.Conn, error) {
	b := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(dialInitialBackoff),
		backoff.WithMaxInterval(dialMaxBackoff),
		// No max elapsed time: retry until the lifetime context is
		// cancelled. The peer may legitimately take a while to bind its
		// listener (it is racing the process startup), so we don't want
		// to give up prematurely.
		backoff.WithMaxElapsedTime(0),
	)

	var conn net.Conn
	retryErr := resiliency.Retry(cm.ctx, b, func() error {
		var d net.Dialer
		c, dialErr := d.DialContext(cm.ctx, "unix", cm.socketPath)
		if dialErr == nil {
			conn = c
			return nil
		}

		// Context cancellation is a clean shutdown signal — stop retrying.
		if errors.Is(dialErr, context.Canceled) {
			return resiliency.Permanent(dialErr)
		}

		// The peer's listener may not exist yet (ECONNREFUSED, ENOENT,
		// EAGAIN) — that's the expected race during startup, retry quietly.
		// Log at V(1) so operators can investigate slow setups without
		// flooding default-level logs.
		cm.log.V(1).Info(
			"Dial on terminal socket failed; will retry",
			"SocketPath", cm.socketPath,
			"error", dialErr.Error(),
		)
		return dialErr
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

// shutdown is the manager's idempotent shutdown sequence. It cancels the
// internal context, which terminates any in-progress Serve loop, unblocks an
// in-flight dial retry, and cancels the Hmp1Server's PTY reader worker. The
// peer owns the listener and the socket file lifecycle; we never touched
// either, so there is nothing to clean up here.
func (cm *ConnManager) shutdown() {
	if !cm.dispose.TryTake() {
		return
	}
	defer cm.dispose.Complete(struct{}{})

	cm.log.V(1).Info("Shutting down terminal connection manager", "SocketPath", cm.socketPath)

	cm.cancelCtx()
}
