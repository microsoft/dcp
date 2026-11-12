/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
)

// DialBackListener serves gRPC over outbound TCP connections, reconnecting after each connection closes.
type DialBackListener struct {
	ctx      context.Context
	cancel   context.CancelFunc
	target   string
	log      logr.Logger
	acceptMu sync.Mutex
	mu       sync.Mutex
	active   *dialBackConnection
}

type dialBackConnection struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (connection *dialBackConnection) Close() error {
	closeErr := connection.Conn.Close()
	connection.once.Do(func() { close(connection.closed) })
	return closeErr
}

// NewDialBackListener creates a listener whose Accept dials the controller instead of accepting TCP.
func NewDialBackListener(ctx context.Context, target string, log logr.Logger) *DialBackListener {
	lifetime, cancel := context.WithCancel(ctx)
	listener := &DialBackListener{ctx: lifetime, cancel: cancel, target: target, log: log}
	context.AfterFunc(lifetime, func() { _ = listener.Close() })
	return listener
}

func (listener *DialBackListener) Accept() (net.Conn, error) {
	listener.acceptMu.Lock()
	defer listener.acceptMu.Unlock()
	listener.mu.Lock()
	previous := listener.active
	listener.mu.Unlock()
	if previous != nil {
		select {
		case <-previous.closed:
		case <-listener.ctx.Done():
			return nil, net.ErrClosed
		}
	}
	for listener.ctx.Err() == nil {
		connection, dialErr := (&net.Dialer{}).DialContext(listener.ctx, "tcp", listener.target)
		if dialErr == nil {
			accepted := &dialBackConnection{Conn: connection, closed: make(chan struct{})}
			listener.mu.Lock()
			if listener.ctx.Err() != nil {
				listener.mu.Unlock()
				_ = accepted.Close()
				return nil, net.ErrClosed
			}
			listener.active = accepted
			listener.mu.Unlock()
			return accepted, nil
		}
		listener.log.V(1).Info("Failed to connect tunnel control transport, retrying", "Error", dialErr)
		timer := time.NewTimer(time.Second)
		select {
		case <-listener.ctx.Done():
			timer.Stop()
			return nil, net.ErrClosed
		case <-timer.C:
		}
	}
	return nil, net.ErrClosed
}

func (listener *DialBackListener) Close() error {
	listener.cancel()
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.active != nil {
		return listener.active.Close()
	}
	return nil
}

func (listener *DialBackListener) Addr() net.Addr { return reverseControlAddress(listener.target) }

type reverseControlAddress string

func (reverseControlAddress) Network() string        { return "tcp" }
func (address reverseControlAddress) String() string { return string(address) }

// ReverseControlConnection owns a loopback callback listener and an authenticated gRPC client connection.
// TCP direction is reversed; the controller remains the TLS and gRPC client.
type ReverseControlConnection struct {
	Connection *grpc.ClientConn
	listener   net.Listener
	cancel     context.CancelFunc
	done       chan struct{}
}

// NewReverseControlConnection binds an ephemeral port before the server process is declared.
// changed is called when authenticated gRPC readiness changes or the accept loop fails.
func NewReverseControlConnection(
	ctx context.Context, security TunnelProxySecurityConfig, changed func(), log logr.Logger,
) (*ReverseControlConnection, error) {
	pool, poolErr := security.GetClientPool()
	if poolErr != nil {
		return nil, poolErr
	}
	lifetime, cancel := context.WithCancel(ctx)
	listener, listenErr := (&net.ListenConfig{}).Listen(lifetime, "tcp", "127.0.0.1:0")
	if listenErr != nil {
		cancel()
		return nil, listenErr
	}
	incoming := make(chan net.Conn)
	connection, connectionErr := grpc.NewClient("passthrough:///127.0.0.1",
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(pool, "127.0.0.1")),
		grpc.WithContextDialer(func(dialCtx context.Context, _ string) (net.Conn, error) {
			select {
			case accepted, open := <-incoming:
				if !open || accepted == nil {
					return nil, net.ErrClosed
				}
				return accepted, nil
			case <-dialCtx.Done():
				return nil, dialCtx.Err()
			case <-lifetime.Done():
				return nil, net.ErrClosed
			}
		}))
	if connectionErr != nil {
		cancel()
		_ = listener.Close()
		return nil, connectionErr
	}
	result := &ReverseControlConnection{Connection: connection, listener: listener, cancel: cancel, done: make(chan struct{})}
	context.AfterFunc(lifetime, func() {
		_ = listener.Close()
		_ = connection.Close()
	})
	go func() {
		defer close(result.done)
		for {
			accepted, acceptErr := listener.Accept()
			if acceptErr != nil {
				if lifetime.Err() == nil && !errors.Is(acceptErr, net.ErrClosed) {
					log.Error(acceptErr, "Failed to accept reverse tunnel control connection")
				}
				cancel()
				changed()
				return
			}
			select {
			case incoming <- accepted:
			case <-lifetime.Done():
				_ = accepted.Close()
				return
			}
		}
	}()
	go func() {
		state := connection.GetState()
		for connection.WaitForStateChange(lifetime, state) {
			state = connection.GetState()
			if state == connectivity.Idle {
				connection.Connect()
			}
			changed()
		}
	}()
	connection.Connect()
	return result, nil
}

// Address is the bound callback authority passed to dcptun.
func (connection *ReverseControlConnection) Address() string {
	return connection.listener.Addr().String()
}

// Ready reports an authenticated, usable HTTP/2 control connection, not just an accepted TCP socket.
func (connection *ReverseControlConnection) Ready() bool {
	return connection.Connection.GetState() == connectivity.Ready
}

func (connection *ReverseControlConnection) Close() error {
	connection.cancel()
	<-connection.done
	closeErr := connection.Connection.Close()
	if closeErr != nil && !errors.Is(closeErr, grpc.ErrClientConnClosing) {
		return fmt.Errorf("close reverse tunnel control connection: %w", closeErr)
	}
	return nil
}
