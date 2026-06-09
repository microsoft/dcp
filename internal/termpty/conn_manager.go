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
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/pkg/concurrency"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
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

	// terminalSocketNamePrefix is prepended to the random suffix when DCP generates a listen-mode
	// terminal socket path because the spec left UDSPath empty.
	terminalSocketNamePrefix = "term-sock-"
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

	// Initial dimensions for the terminal.
	initialCols uint16
	initialRows uint16

	// ptpP stores PseudoTerminalProcess instance that ConnManager works with.
	ptpP *concurrency.ValuePromise[*PseudoTerminalProcess]

	// Listener for the incoming HMP v1 connections.
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
// traffic on socketPath to a pseudo-terminal and a proccess behind it.
// The process may (ptp != nil) or may not (ptp == nil) not be started
// when ConnManager is created. In the latter case the ConnManager can be made aware
// of the process later via AttachProcess().
// In either case the manager will connect to- (SocketModeConnect)
// or listen on- (SocketModeListen) the client socket immediately,
// but the terminal data will flow only after the process is available.
//
// In listen mode socketPath may be empty, in which case the manager generates a
// unique path in the DCP session/temp folder and owns the socket file (creating
// it and removing it on shutdown). When socketPath is provided, it must not already
// exist; a file already present at the path is treated as an error. The resolved
// path is available via SocketPath(). In connect mode socketPath is required.
func NewConnManager(
	lifetimeCtx context.Context,
	ptp *PseudoTerminalProcess,
	socketPath string,
	mode SocketMode,
	initialCols uint16,
	initialRows uint16,
	log logr.Logger,
) (*ConnManager, error) {
	if lifetimeCtx == nil {
		return nil, errors.New("lifetime context must not be nil")
	}
	if socketPath == "" && mode != SocketModeListen {
		return nil, errors.New("socket path must not be empty")
	}
	// In eager mode the process must be valid up front; in attach-process mode
	// it is validated later by AttachProcess. Validate before binding the
	// listener so a rejected eager process does not leave a socket file behind.
	if ptp != nil {
		if err := validatePtp(ptp); err != nil {
			return nil, err
		}
	}

	cm, err := createConnManager(lifetimeCtx, socketPath, mode, initialCols, initialRows, log)
	if err != nil {
		return nil, err
	}

	if ptp == nil {
		go cm.prepareAndServe()
		return cm, nil
	} else {
		cm.ptpP.Set(ptp)
		go cm.watchProcessExit()
		go func() {
			defer close(cm.done)
			cm.serveLoop(time.Time{})
		}()
		return cm, nil
	}
}

func createConnManager(
	lifetimeCtx context.Context,
	socketPath string,
	mode SocketMode,
	initialCols uint16,
	initialRows uint16,
	log logr.Logger,
) (*ConnManager, error) {
	var listener *net.UnixListener
	if mode == SocketModeListen {
		if socketPath == "" {
			generatedPath, genErr := generateListenSocketPath()
			if genErr != nil {
				return nil, genErr
			}
			socketPath = generatedPath
		}

		// DCP owns the socket file in listen mode: the path must be free before binding. A file
		// already present at the path is treated as an error and left untouched.
		if prepErr := prepareListenSocketPath(socketPath); prepErr != nil {
			return nil, prepErr
		}

		boundListener, listenErr := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
		if listenErr != nil {
			return nil, fmt.Errorf("could not create terminal socket at %s: %w", socketPath, listenErr)
		}

		// The socket file is removed explicitly during shutdown (see shutdown()), so opt out of
		// the listener's own unlink-on-close to keep removal ordering under our control.
		boundListener.SetUnlinkOnClose(false)

		listener = boundListener
	}

	ctx, cancelCtx := context.WithCancel(lifetimeCtx)
	initialCols, initialRows = NormalizeTerminalDimensions(initialCols, initialRows)

	cm := &ConnManager{
		log:         log,
		socketPath:  socketPath,
		socketMode:  mode,
		listener:    listener,
		initialCols: initialCols,
		initialRows: initialRows,
		ctx:         ctx,
		cancelCtx:   cancelCtx,
		ptpP:        concurrency.NewValuePromise[*PseudoTerminalProcess](),
		done:        make(chan struct{}),
	}
	cm.shutdownOnce = sync.OnceFunc(cm.shutdown)

	if mode == SocketModeConnect {
		cm.log.V(1).Info("Terminal connection manager will dial peer", "SocketPath", socketPath)
	} else {
		cm.log.V(1).Info("Terminal connection manager is listening", "SocketPath", socketPath)
	}
	context.AfterFunc(lifetimeCtx, cm.shutdownOnce)

	return cm, nil
}

// generateListenSocketPath returns a unique terminal socket path in the DCP
// session/temp folder, used when a listen-mode manager is created without an
// explicit socket path.
func generateListenSocketPath() (string, error) {
	socketPath, err := osutil.CreateRandomSocketPath(usvc_io.DcpTempDir(), terminalSocketNamePrefix)
	if err != nil {
		return "", fmt.Errorf("could not generate a terminal socket path: %w", err)
	}
	return socketPath, nil
}

// prepareListenSocketPath verifies that the path is free for binding a listen-mode
// socket that DCP owns. DCP never reuses an existing file: any entry already present
// at the path (socket, regular file, or directory) is treated as an error and left
// intact. It returns nil only when nothing exists at the path.
func prepareListenSocketPath(socketPath string) error {
	_, lstatErr := os.Lstat(socketPath)
	if errors.Is(lstatErr, os.ErrNotExist) {
		return nil
	}
	if lstatErr != nil {
		return fmt.Errorf("could not inspect existing terminal socket path %s: %w", socketPath, lstatErr)
	}

	return fmt.Errorf("terminal socket path %s already exists", socketPath)
}

// ownsSocketFile reports whether the manager is responsible for the lifecycle of
// the socket file (true in listen mode, where DCP creates and removes it).
func (cm *ConnManager) ownsSocketFile() bool {
	return cm.socketMode == SocketModeListen
}

// validatePtp checks that a PseudoTerminalProcess is usable by a ConnManager.
func validatePtp(ptp *PseudoTerminalProcess) error {
	if ptp == nil {
		return errors.New("PseudoTerminalProcess must not be nil")
	}
	if ptp.PTY == nil {
		return errors.New("PseudoTerminalProcess.PTY must not be nil")
	}
	if ptp.ExitHandler == nil {
		return errors.New("PseudoTerminalProcess.ExitHandler must not be nil")
	}
	return nil
}

// AttachProcess supplies the pseudo-terminal process to a manager created in
// attach-process mode (i.e. with a nil ptp). The manager creates its Hmp1Server
// and serves the warmed client connection, then continues serving subsequent
// clients via the normal reconnect loop.
func (cm *ConnManager) AttachProcess(ptp *PseudoTerminalProcess) error {
	if err := validatePtp(ptp); err != nil {
		return err
	}
	if !cm.ptpP.Set(ptp) {
		return errors.New("a process has already been attached to this connection manager")
	}
	if cm.ctx.Err() != nil {
		return errors.New("connection manager is shutting down")
	}
	return nil
}

// SocketPath returns the filesystem path of the Unix domain socket the manager
// is (or was) listening on (or, in connect mode, dialing). When listen mode was
// given an empty path at construction, this returns the path DCP generated.
func (cm *ConnManager) SocketPath() string {
	return cm.socketPath
}

// Shutdown deterministically tears down the manager: it terminates any active
// Serve loop or pending dial and, in listen mode, removes the socket file and
// closes the listener. It is idempotent and safe to call from any goroutine.
func (cm *ConnManager) Shutdown() {
	cm.shutdownOnce()
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
	ptp := cm.ptpP.Get()
	select {
	case <-ptp.ExitHandler.Exited():
		cm.log.V(1).Info("Terminal process exited; shutting down connection manager", "SocketPath", cm.socketPath)
		cm.shutdownOnce()
	case <-cm.ctx.Done():
		// Shutdown already in progress.
	}
}

// prepareAndServe runs the attach-process (pre-connect) startup sequence: it
// warms a client connection before any process is attached, waits for the
// process to be supplied via AttachProcess, then serves the warmed connection and
// continues into the normal connection-serving loop.
func (cm *ConnManager) prepareAndServe() {
	defer close(cm.done)

	conn, err := cm.nextConn()
	if err != nil {
		// Shutting down before any client connection could be established.
		return
	}
	connectedAt := time.Now()

	select {
	case <-cm.ptpP.Wait():
		// A process has been attached, continue.
	case <-cm.ctx.Done():
		if closeErr := conn.Close(); closeErr != nil {
			cm.log.V(1).Info("Closing warmed terminal connection failed", "error", closeErr.Error())
		}
		return
	}

	go cm.watchProcessExit()

	// Serve the connection warmed before the process existed, then carry the
	// connection timestamp into the loop so the first reconnect is still
	// throttled in connect mode.
	cm.serveConnection(conn)
	cm.serveLoop(connectedAt)
}

// serveLoop establishes client connections and serves them one at a time until
// the manager shuts down. lastConnectAttempt seeds the connect-mode redial
// throttle: pass the zero time when there is no prior connection to throttle
// against.
func (cm *ConnManager) serveLoop(lastConnectAttempt time.Time) {
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

// waitBeforeRedial blocks until reconnectMinInterval has elapsed since
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

	// serveConnection will not be called before the PseudoTerminalProcess is available.
	ptp := cm.ptpP.Get()
	server := NewHmp1Server(cm.ctx, ptp.PTY)

	wg := sync.WaitGroup{}
	wg.Add(1)

	go func() {
		defer wg.Done()
		defer close(exitCodeCh)

		// Wait for either the process to exit or the Serve loop to end.
		select {
		case <-ptp.ExitHandler.Exited():
			ec := ptp.ExitHandler.ExitInfo().ExitCode
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

	serveErr := server.Serve(serveCtx, conn, exitCodeCh, Hmp1ServerOptions{
		InitialCols: cm.initialCols,
		InitialRows: cm.initialRows,
		Log:         cm.log,
	})
	if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		if isExpectedClientDisconnect(serveErr) {
			// The client closed or dropped the connection.
			// This is a normal terminal-viewer lifecycle event, not a server failure.
		} else {
			cm.log.Error(serveErr, "HMP1 Serve returned an error")
		}
	}

	serveCancel()

	// Make sure the exit code feeder goroutine has finished before we return.
	wg.Wait()
}

// shutdown() is the manager's shutdown sequence.
// Idempotecy is guaranteed via wrapping the method in a sync.Once.
// Note: cm.done is closed when connLoop() exits.
func (cm *ConnManager) shutdown() {
	cm.log.V(1).Info("Shutting down terminal connection manager", "SocketPath", cm.socketPath)

	// Cancels HMP1 server and any data exchange with the client.
	cm.cancelCtx()

	if cm.listener != nil {
		if closeErr := cm.listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			cm.log.V(1).Info("Closing terminal socket listener failed", "error", closeErr.Error())
		}

		if cm.ownsSocketFile() {
			if removeErr := os.Remove(cm.socketPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				cm.log.Error(removeErr, "Removing terminal socket file failed")
			}
		}
	}
}
