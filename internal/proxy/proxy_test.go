// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

var (
	log = logger.New("proxy-tests").Logger
)

func TestMain(m *testing.M) {
	networking.EnableStrictMruPortHandling(log)
	code := m.Run()
	os.Exit(code)
}

func TestInvalidProxyMode(t *testing.T) {
	t.Parallel()
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	require.Panics(t, func() { NewProxy("tcp", "", 0, ctx, logr.Discard()) })
}

func TestTCPModeProxy(t *testing.T) {
	t.Parallel()

	// Set up a server
	serverListener, port := setupTcpServer(t, autoAllocatePort)

	// Set up a proxy
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	proxy := setupTcpProxy(t, ctx)

	// Feed the config to the proxy
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: "127.0.0.1", Port: int32(port)},
		},
	}
	err := proxy.Configure(config)
	require.NoError(t, err)

	// Verify the proxy sends data to the server
	verifiyProxiedTcpConnection(t, proxy, serverListener)

	// Clean up
	require.NoError(t, serverListener.Close())
}

func TestTCPConfigurationChange(t *testing.T) {
	t.Parallel()

	// Set up two servers
	listener1, port1 := setupTcpServer(t, autoAllocatePort)
	listener2, port2 := setupTcpServer(t, autoAllocatePort)

	// Set up a proxy
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	proxy := setupTcpProxy(t, ctx)

	// Feed config to the proxy, targeting the first server
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: "127.0.0.1", Port: int32(port1)},
		},
	}
	err := proxy.Configure(config)
	require.NoError(t, err)

	// Verify the proxy sends data to the fist server
	verifiyProxiedTcpConnection(t, proxy, listener1)

	// Change configuration to target second server
	config = ProxyConfig{
		Endpoints: []Endpoint{
			{Address: "127.0.0.1", Port: int32(port2)},
		},
	}
	err = proxy.Configure(config)
	require.NoError(t, err)

	// Verify the proxy sends data to the second server
	verifiyProxiedTcpConnection(t, proxy, listener2)

	// Clean up
	require.NoError(t, listener1.Close())
	require.NoError(t, listener2.Close())
}

func TestTCPClientCanSendDataBeforeEndpointsExist(t *testing.T) {
	t.Parallel()

	// Set up a proxy
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	proxy := setupTcpProxy(t, ctx)
	proxy.connectionTimeout = 500 * time.Millisecond

	// Write some data to the proxy
	clientConn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", proxy.EffectiveAddress, proxy.EffectivePort))
	require.NoError(t, err)
	require.NotNil(t, clientConn)
	msg := []byte("This is amazing!")
	_, err = clientConn.Write(msg)
	require.NoError(t, err)

	// Configure the proxy
	port, err := networking.GetFreePort(apiv1.TCP, "127.0.0.1", log)
	require.NoError(t, err)
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: "127.0.0.1", Port: int32(port)},
		},
	}
	err = proxy.Configure(config)
	require.NoError(t, err)

	// Delay server creation to ensure the proxy buffers the message
	time.Sleep(2 * time.Second)

	// Set up a server
	serverListener, _ := setupTcpServer(t, port)

	// Verify the proxy sends data to the server
	proxiedConnection, err := serverListener.Accept()
	require.NoError(t, err)
	require.NotNil(t, proxiedConnection)

	// Verify the message
	buffer := make([]byte, len(msg))
	_, err = proxiedConnection.Read(buffer)
	require.NoError(t, err)
	require.EqualValues(t, msg, buffer)

	// Clean up
	require.NoError(t, proxiedConnection.Close())
	require.NoError(t, clientConn.Close())
	require.NoError(t, serverListener.Close())
}

func TestUDPModeProxy(t *testing.T) {
	t.Parallel()

	// Set up a server
	serverConn, port := setupUdpServer(t, autoAllocatePort)

	// Set up a proxy
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	proxy := setupUdpProxy(t, ctx)

	// Feed the config to the proxy
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: "127.0.0.1", Port: int32(port)},
		},
	}
	err := proxy.Configure(config)
	require.NoError(t, err)

	// Verify data can be sent both ways
	verifyProxiedUdpConnection(t, proxy, serverConn)

	// Clean up
	require.NoError(t, serverConn.Close())
}

func TestUDPTwoEndpoints(t *testing.T) {
	t.Parallel()

	// Set up server
	serverConn, port := setupUdpServer(t, autoAllocatePort)

	// Set up a proxy
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	proxy := setupUdpProxy(t, ctx)

	// Feed the config to the proxy
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: "127.0.0.1", Port: int32(port)},
		},
	}
	err := proxy.Configure(config)
	require.NoError(t, err)

	// Set up two clients
	clientConn1, err := net.ListenPacket("udp", "127.0.0.1:")
	require.NoError(t, err)
	require.NotNil(t, clientConn1)
	clientConn2, err := net.ListenPacket("udp", "127.0.0.1:")
	require.NoError(t, err)
	require.NotNil(t, clientConn2)

	// We should be able to send data back and forth with these two clients independently
	verifyUDPPacketFlow(t, proxy, serverConn, clientConn1)
	verifyUDPPacketFlow(t, proxy, serverConn, clientConn2)
	verifyUDPPacketFlow(t, proxy, serverConn, clientConn1)
	verifyUDPPacketFlow(t, proxy, serverConn, clientConn2)

	// Clean up
	require.NoError(t, clientConn1.Close())
	require.NoError(t, clientConn2.Close())
	require.NoError(t, serverConn.Close())
}

func TestRandomEndpointSelection(t *testing.T) {
	t.Parallel()

	const tries = 20

	ctx, cancelFunc := context.WithCancel(context.Background())
	proxy := NewProxy(apiv1.TCP, "127.0.0.1", 0, ctx, logr.Discard())
	require.NotNil(t, proxy)
	defer cancelFunc()

	config := &ProxyConfig{
		Endpoints: []Endpoint{
			{Address: "127.0.0.1", Port: 8080},
			{Address: "127.0.0.2", Port: 8081},
		},
	}

	haveSeenZeroOne := false
	haveSeenZeroTwo := false

	for range make([]any, tries) {
		endpoint, err := chooseEndpoint(config)
		require.NoError(t, err)
		require.NotNil(t, endpoint)
		if endpoint.Address == "127.0.0.1" {
			haveSeenZeroOne = true
		} else if endpoint.Address == "127.0.0.2" {
			haveSeenZeroTwo = true
		}
	}

	require.True(t, haveSeenZeroOne)
	require.True(t, haveSeenZeroTwo)

	config = &ProxyConfig{}
	endpoint, err := chooseEndpoint(config)
	require.Error(t, err)
	require.Nil(t, endpoint)
}

const autoAllocatePort = 0

func setupTcpServer(t *testing.T, port int32) (net.Listener, int) {
	serverListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	require.NotNil(t, serverListener)
	effectivePort := serverListener.Addr().(*net.TCPAddr).Port
	require.Greater(t, effectivePort, 0)
	return serverListener, effectivePort
}

func setupTcpProxy(t *testing.T, ctx context.Context) *Proxy {
	// For debugging you can replace logr.Discard() with "log" and set DCP_DIAGNOSTICS_LOG_LEVEL=debug
	proxy := NewProxy(apiv1.TCP, "127.0.0.1", 0, ctx, logr.Discard())
	require.NotNil(t, proxy)

	err := proxy.Start()
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", proxy.EffectiveAddress)
	require.Greater(t, proxy.EffectivePort, int32(0))
	return proxy
}

func verifiyProxiedTcpConnection(t *testing.T, proxy *Proxy, server net.Listener) {
	// Set up a client
	clientConn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", proxy.EffectiveAddress, proxy.EffectivePort))
	require.NoError(t, err)
	require.NotNil(t, clientConn)

	// Have the client send a message
	msg := []byte(testutil.GetRandLetters(t, 6))
	_, err = clientConn.Write(msg)
	require.NoError(t, err)

	// Have the server receive a message
	proxiedConnection, err := server.Accept()
	require.NoError(t, err)
	require.NotNil(t, proxiedConnection)

	// Verify the message
	buffer := make([]byte, len(msg))
	_, err = proxiedConnection.Read(buffer)
	require.NoError(t, err)
	require.EqualValues(t, msg, buffer, "Message received by server is not the same as the message sent by the client")

	require.NoError(t, proxiedConnection.Close())
	require.NoError(t, clientConn.Close())
}

func setupUdpServer(t *testing.T, port int32) (net.PacketConn, int) {
	serverConn, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	require.NotNil(t, serverConn)
	effectivePort := serverConn.LocalAddr().(*net.UDPAddr).Port
	require.Greater(t, effectivePort, 0)
	return serverConn, effectivePort
}

func setupUdpProxy(t *testing.T, ctx context.Context) *Proxy {
	proxy := NewProxy(apiv1.UDP, "127.0.0.1", 0, ctx, logr.Discard())
	require.NotNil(t, proxy)

	err := proxy.Start()
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", proxy.EffectiveAddress)
	require.Greater(t, proxy.EffectivePort, int32(0))
	return proxy
}

func verifyProxiedUdpConnection(t *testing.T, proxy *Proxy, serverConn net.PacketConn) {
	// Set up a client
	clientConn, err := net.ListenPacket("udp", "127.0.0.1:")
	require.NoError(t, err)
	require.NotNil(t, clientConn)

	verifyUDPPacketFlow(t, proxy, serverConn, clientConn)

	// Clean up
	require.NoError(t, clientConn.Close())
}

func verifyUDPPacketFlow(
	t *testing.T,
	proxy *Proxy,
	serverConn net.PacketConn,
	clientConn net.PacketConn,
) {
	// Have the client send a message
	msg := []byte(testutil.GetRandLetters(t, 6))
	n, err := clientConn.WriteTo(msg, &net.UDPAddr{IP: net.ParseIP(proxy.EffectiveAddress), Port: int(proxy.EffectivePort)})
	require.NoError(t, err)
	require.Equal(t, len(msg), n)

	// Have the server receive a message
	buffer := make([]byte, len(msg))
	n, clientAddr, err := serverConn.ReadFrom(buffer)
	require.NoError(t, err)
	require.Equal(t, len(msg), n)
	// Note that the client addr will be, in fact, the proxy address/port

	// Verify the message
	require.True(t, bytes.Equal(buffer, msg), "Message received by the server ('%s') is not the same as the message sent by the client ('%s')", string(buffer), string(msg))

	// Have the server send a message back
	msg = []byte(testutil.GetRandLetters(t, 6))
	n, err = serverConn.WriteTo(msg, clientAddr)
	require.NoError(t, err)
	require.Equal(t, len(msg), n)

	// Have the client receive the response message
	buffer = make([]byte, len(msg))
	n, proxyAddr, err := clientConn.ReadFrom(buffer)
	require.NoError(t, err)
	require.Equal(t, len(msg), n)
	require.Equal(t, proxy.EffectiveAddress, proxyAddr.(*net.UDPAddr).IP.String())
	require.Equal(t, proxy.EffectivePort, int32(proxyAddr.(*net.UDPAddr).Port))
	require.True(t, bytes.Equal(buffer, msg), "Response message received by the client ('%s') is not the same as the message sent by the server ('%s')", string(buffer), string(msg))
}
