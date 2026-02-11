/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/process"

	"github.com/go-logr/logr"
)

// PortPlaceholder is the placeholder in adapter args that will be replaced with allocated port.
const PortPlaceholder = "{{port}}"

// ErrInvalidAdapterConfig is returned when the debug adapter configuration is invalid.
var ErrInvalidAdapterConfig = errors.New("invalid debug adapter configuration: Args must have at least one element")

// ErrAdapterConnectionTimeout is returned when the adapter fails to connect within the timeout.
var ErrAdapterConnectionTimeout = errors.New("debug adapter connection timeout")

// LaunchedAdapter represents a running debug adapter process with its transport.
type LaunchedAdapter struct {
	// Transport provides DAP message I/O with the debug adapter.
	Transport Transport

	// handle identifies the debug adapter process.
	handle process.ProcessHandle

	// listener is the TCP listener for callback mode (nil for other modes).
	listener net.Listener

	// done signals when the process has exited.
	done chan struct{}

	// exitCode contains the process exit code (if any).
	exitCode int32

	// exitErr contains the process exit error (if any).
	exitErr error

	// mu protects exitCode and exitErr.
	mu sync.Mutex
}

// Pid returns the process ID of the debug adapter.
func (la *LaunchedAdapter) Pid() process.Pid_t {
	return la.handle.Pid
}

// Done returns a channel that is closed when the debug adapter process exits.
func (la *LaunchedAdapter) Done() <-chan struct{} {
	return la.done
}

// Close cleans up the adapter resources.
// This closes the transport and listener, but does NOT stop the process.
// The process is stopped automatically when the context passed to LaunchDebugAdapter is cancelled.
func (la *LaunchedAdapter) Close() error {
	var errs []error
	if la.listener != nil {
		if err := la.listener.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if la.Transport != nil {
		if err := la.Transport.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// LaunchDebugAdapter launches a debug adapter process using the provided configuration.
// The process lifetime is tied to the provided context - when the context is cancelled,
// the process will be killed by the executor.
//
// The returned LaunchedAdapter provides:
// - Transport: for DAP message I/O with the adapter
// - Wait(): to block until the process exits
// - Done(): a channel that closes when the process exits
// - Pid(): the process ID
//
// The caller must close the Transport when done.
func LaunchDebugAdapter(ctx context.Context, executor process.Executor, config *DebugAdapterConfig, log logr.Logger) (*LaunchedAdapter, error) {
	if config == nil || len(config.Args) == 0 {
		return nil, ErrInvalidAdapterConfig
	}

	switch config.EffectiveMode() {
	case DebugAdapterModeStdio:
		return launchStdioAdapter(ctx, executor, config, log)
	case DebugAdapterModeTCPCallback:
		return launchTCPCallbackAdapter(ctx, executor, config, log)
	case DebugAdapterModeTCPConnect:
		return launchTCPConnectAdapter(ctx, executor, config, log)
	default:
		return launchStdioAdapter(ctx, executor, config, log)
	}
}

// launchStdioAdapter launches an adapter in stdio mode.
func launchStdioAdapter(ctx context.Context, executor process.Executor, config *DebugAdapterConfig, log logr.Logger) (*LaunchedAdapter, error) {
	cmd := exec.Command(config.Args[0], config.Args[1:]...)
	cmd.Env = buildEnv(config)

	stdin, stdinErr := cmd.StdinPipe()
	if stdinErr != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", stdinErr)
	}

	stdout, stdoutErr := cmd.StdoutPipe()
	if stdoutErr != nil {
		stdin.Close()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", stdoutErr)
	}

	stderr, stderrErr := cmd.StderrPipe()
	if stderrErr != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("failed to create stderr pipe: %w", stderrErr)
	}

	adapter := &LaunchedAdapter{
		done:     make(chan struct{}),
		exitCode: process.UnknownExitCode,
	}

	exitHandler := process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
		adapter.mu.Lock()
		adapter.exitCode = exitCode
		adapter.exitErr = err
		adapter.mu.Unlock()
		close(adapter.done)

		if err != nil {
			log.V(1).Info("Debug adapter process exited with error",
				"pid", pid,
				"exitCode", exitCode,
				"error", err)
		} else {
			log.V(1).Info("Debug adapter process exited",
				"pid", pid,
				"exitCode", exitCode)
		}
	})

	handle, startWaitForExit, startErr := executor.StartProcess(ctx, cmd, exitHandler, process.CreationFlagEnsureKillOnDispose)
	if startErr != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return nil, fmt.Errorf("failed to start debug adapter: %w", startErr)
	}

	// Start waiting for process exit
	startWaitForExit()

	go logStderr(stderr, log)

	log.Info("Launched debug adapter process (stdio mode)",
		"command", config.Args[0],
		"args", config.Args[1:],
		"pid", handle.Pid)

	adapter.Transport = NewStdioTransportWithContext(ctx, stdout, stdin)
	adapter.handle = handle

	return adapter, nil
}

// launchTCPCallbackAdapter launches an adapter in TCP callback mode.
// We start a listener and the adapter connects to us.
func launchTCPCallbackAdapter(ctx context.Context, executor process.Executor, config *DebugAdapterConfig, log logr.Logger) (*LaunchedAdapter, error) {
	// Start a listener on a free port
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		return nil, fmt.Errorf("failed to create listener: %w", listenErr)
	}

	listenerAddr := listener.Addr().String()
	log.Info("Listening for debug adapter callback", "address", listenerAddr)

	// Substitute {{port}} placeholder with our listening port
	_, portStr, _ := net.SplitHostPort(listenerAddr)
	args := substitutePort(config.Args, portStr)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = buildEnv(config)

	stderr, stderrErr := cmd.StderrPipe()
	if stderrErr != nil {
		listener.Close()
		return nil, fmt.Errorf("failed to create stderr pipe: %w", stderrErr)
	}

	adapter := &LaunchedAdapter{
		listener: listener,
		done:     make(chan struct{}),
		exitCode: process.UnknownExitCode,
	}

	exitHandler := process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
		adapter.mu.Lock()
		adapter.exitCode = exitCode
		adapter.exitErr = err
		adapter.mu.Unlock()
		close(adapter.done)

		if err != nil {
			log.V(1).Info("Debug adapter process exited with error",
				"pid", pid,
				"exitCode", exitCode,
				"error", err)
		} else {
			log.V(1).Info("Debug adapter process exited",
				"pid", pid,
				"exitCode", exitCode)
		}
	})

	handle, startWaitForExit, startErr := executor.StartProcess(ctx, cmd, exitHandler, process.CreationFlagEnsureKillOnDispose)
	if startErr != nil {
		listener.Close()
		stderr.Close()
		return nil, fmt.Errorf("failed to start debug adapter: %w", startErr)
	}

	// Start waiting for process exit
	startWaitForExit()

	go logStderr(stderr, log)

	log.Info("Launched debug adapter process (tcp-callback mode)",
		"command", args[0],
		"args", args[1:],
		"pid", handle.Pid,
		"listenAddress", listenerAddr)

	adapter.handle = handle

	// Wait for adapter to connect
	timeout := config.GetConnectionTimeout()

	connCh := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			errCh <- acceptErr
			return
		}
		connCh <- conn
	}()

	var conn net.Conn
	select {
	case conn = <-connCh:
		log.Info("Debug adapter connected", "remoteAddr", conn.RemoteAddr().String())
	case acceptErr := <-errCh:
		_ = executor.StopProcess(adapter.handle)
		listener.Close()
		return nil, fmt.Errorf("failed to accept adapter connection: %w", acceptErr)
	case <-time.After(timeout):
		_ = executor.StopProcess(adapter.handle)
		listener.Close()
		return nil, ErrAdapterConnectionTimeout
	case <-ctx.Done():
		// Executor will handle stopping the process when context is cancelled
		listener.Close()
		return nil, ctx.Err()
	}

	adapter.Transport = NewTCPTransportWithContext(ctx, conn)
	return adapter, nil
}

// launchTCPConnectAdapter launches an adapter in TCP connect mode.
// The adapter listens on a port and we connect to it.
func launchTCPConnectAdapter(ctx context.Context, executor process.Executor, config *DebugAdapterConfig, log logr.Logger) (*LaunchedAdapter, error) {
	// Allocate a free port for the adapter
	port, portErr := networking.GetFreePort(apiv1.TCP, "127.0.0.1", log)
	if portErr != nil {
		return nil, fmt.Errorf("failed to allocate port: %w", portErr)
	}

	portStr := strconv.Itoa(int(port))
	args := substitutePort(config.Args, portStr)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = buildEnv(config)

	stderr, stderrErr := cmd.StderrPipe()
	if stderrErr != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", stderrErr)
	}

	adapter := &LaunchedAdapter{
		done:     make(chan struct{}),
		exitCode: process.UnknownExitCode,
	}

	exitHandler := process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
		adapter.mu.Lock()
		adapter.exitCode = exitCode
		adapter.exitErr = err
		adapter.mu.Unlock()
		close(adapter.done)

		if err != nil {
			log.V(1).Info("Debug adapter process exited with error",
				"pid", pid,
				"exitCode", exitCode,
				"error", err)
		} else {
			log.V(1).Info("Debug adapter process exited",
				"pid", pid,
				"exitCode", exitCode)
		}
	})

	handle, startWaitForExit, startErr := executor.StartProcess(ctx, cmd, exitHandler, process.CreationFlagEnsureKillOnDispose)
	if startErr != nil {
		stderr.Close()
		return nil, fmt.Errorf("failed to start debug adapter: %w", startErr)
	}

	// Start waiting for process exit
	startWaitForExit()

	go logStderr(stderr, log)

	log.Info("Launched debug adapter process (tcp-connect mode)",
		"command", args[0],
		"args", args[1:],
		"pid", handle.Pid,
		"port", port)

	adapter.handle = handle

	// Connect to the adapter with retry
	timeout := config.GetConnectionTimeout()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var conn net.Conn
	var connectErr error
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			// Executor will handle stopping the process when context is cancelled
			return nil, ctx.Err()
		case <-adapter.done:
			// Process exited before we could connect
			return nil, fmt.Errorf("debug adapter process exited before connection could be established")
		default:
		}

		conn, connectErr = net.DialTimeout("tcp", addr, time.Second)
		if connectErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if connectErr != nil {
		_ = executor.StopProcess(adapter.handle)
		return nil, fmt.Errorf("%w: failed to connect to adapter at %s: %v", ErrAdapterConnectionTimeout, addr, connectErr)
	}

	log.Info("Connected to debug adapter", "address", addr)

	adapter.Transport = NewTCPTransportWithContext(ctx, conn)
	return adapter, nil
}

// substitutePort replaces {{port}} placeholder in args with the actual port.
func substitutePort(args []string, port string) []string {
	result := make([]string, len(args))
	for i, arg := range args {
		result[i] = strings.ReplaceAll(arg, PortPlaceholder, port)
	}
	return result
}

// buildEnv builds the environment for the adapter process.
// Only the environment variables from the config are used; the current process
// environment is intentionally NOT inherited.
func buildEnv(config *DebugAdapterConfig) []string {
	env := make([]string, 0, len(config.Env))
	for _, e := range config.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	return env
}

// logStderr reads and logs stderr from the adapter.
func logStderr(stderr interface{ Read([]byte) (int, error) }, log logr.Logger) {
	buf := make([]byte, 1024)
	for {
		n, readErr := stderr.Read(buf)
		if n > 0 {
			log.Info("Debug adapter stderr", "output", string(buf[:n]))
		}
		if readErr != nil {
			return
		}
	}
}
