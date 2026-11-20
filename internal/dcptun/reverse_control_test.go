/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/pkg/testutil"
)

type reverseControlTestServer struct {
	proto.UnimplementedTunnelControlServer
}

func (*reverseControlTestServer) Shutdown(context.Context, *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// Verifies that reverse control authenticates dcptun, makes RPCs over the outbound connection,
// rejects an impostor, and reconnects after the server transport closes.
func TestReverseControlConnection(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	security, securityErr := NewTunnelProxySecurityConfig()
	require.NoError(t, securityErr)
	changed := make(chan struct{}, 1)
	connection, connectionErr := NewReverseControlConnection(ctx, security.ControllerConfig(), func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}, logr.Discard())
	require.NoError(t, connectionErr)
	defer func() { require.NoError(t, connection.Close()) }()
	impostor, impostorErr := net.Dial("tcp", connection.Address())
	require.NoError(t, impostorErr)
	_, writeErr := impostor.Write([]byte("not a TLS server"))
	require.NoError(t, writeErr)
	require.NoError(t, impostor.Close())
	require.False(t, connection.Ready())
	listener := NewDialBackListener(ctx, connection.Address(), logr.Discard())
	server, serverErr := NewControlServer(security.ServerProxyConfig(), &reverseControlTestServer{})
	require.NoError(t, serverErr)
	defer server.Stop()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	defer func() {
		server.Stop()
		_ = listener.Close()
		serveErr := <-serveDone
		if !errors.Is(serveErr, net.ErrClosed) {
			require.NoError(t, serveErr)
		}
	}()
	awaitReady := func() {
		for !connection.Ready() {
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatal("reverse control did not become ready")
			}
		}
	}
	awaitReady()
	client := proto.NewTunnelControlClient(connection.Connection)
	_, rpcErr := client.Shutdown(ctx, &emptypb.Empty{})
	require.NoError(t, rpcErr)
	listener.mu.Lock()
	active := listener.active
	listener.mu.Unlock()
	require.NotNil(t, active)
	require.NoError(t, active.Close())
	for {
		listener.mu.Lock()
		replacement := listener.active
		listener.mu.Unlock()
		if replacement != active && connection.Ready() {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("reverse control did not reconnect")
		}
	}
	_, reconnectErr := client.Shutdown(ctx, &emptypb.Empty{})
	require.NoError(t, reconnectErr)
}

// Verifies that closing a dial-back listener cancels an outstanding dial without leaking Accept.
func TestDialBackListenerCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	listener := NewDialBackListener(ctx, "127.0.0.1:0", logr.Discard())
	done := make(chan error, 1)
	go func() { _, acceptErr := listener.Accept(); done <- acceptErr }()
	require.NoError(t, listener.Close())
	select {
	case acceptErr := <-done:
		require.ErrorIs(t, acceptErr, net.ErrClosed)
	case <-ctx.Done():
		t.Fatal("Accept did not stop")
	}
}

// Verifies that tunnel status serialization contains connection details but no control credentials.
func TestTunnelProxyStatusOmitsPrivateKey(t *testing.T) {
	t.Parallel()
	config := TunnelProxyConfig{ServerControlPort: 1234}
	encoded, encodeErr := json.Marshal(config)
	require.NoError(t, encodeErr)
	require.NotContains(t, string(encoded), "server_key_base64")
	require.NotContains(t, string(encoded), "outgoing_token")
	require.NotContains(t, string(encoded), "incoming_token_hash_base64")
	var decoded TunnelProxyConfig
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, int32(1234), decoded.ServerControlPort)
}
