/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
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
	require.Panics(t, func() { newNetProxy("tcp", "", 0, ctx, logr.Discard()) })
}

func TestTCPModeProxy(t *testing.T) {
	t.Parallel()

	// Set up a server
	serverListener, port := setupTcpServer(t, autoAllocatePort)

	// Set up a proxy
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	proxy, logSink := setupTcpProxy(t, ctx)
	defer logSink.AssertNotCalled(t, "Error")

	// Feed the config to the proxy
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: networking.IPv4LocalhostDefaultAddress, Port: int32(port)},
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

	proxy, logSink := setupTcpProxy(t, ctx)
	defer logSink.AssertNotCalled(t, "Error")

	// Feed config to the proxy, targeting the first server
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: networking.IPv4LocalhostDefaultAddress, Port: int32(port1)},
		},
	}
	err := proxy.Configure(config)
	require.NoError(t, err)

	// Verify the proxy sends data to the fist server
	verifiyProxiedTcpConnection(t, proxy, listener1)

	// Change configuration to target second server
	config = ProxyConfig{
		Endpoints: []Endpoint{
			{Address: networking.IPv4LocalhostDefaultAddress, Port: int32(port2)},
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
	proxy, logSink := setupTcpProxy(t, ctx)
	defer logSink.AssertNotCalled(t, "Error")
	proxy.connectionTimeout = 500 * time.Millisecond

	// Write some data to the proxy
	clientConn, err := net.Dial("tcp", networking.AddressAndPort(proxy.EffectiveAddress(), proxy.EffectivePort()))
	require.NoError(t, err)
	require.NotNil(t, clientConn)
	msg := []byte("This is amazing!")
	_, err = clientConn.Write(msg)
	require.NoError(t, err)

	// Configure the proxy with no endpoints several times
	const emptyConfigTries = 3
	emptyConfig := ProxyConfig{
		Endpoints: []Endpoint{},
	}
	for i := 0; i < emptyConfigTries; i++ {
		err = proxy.Configure(emptyConfig)
		require.NoError(t, err)

		// Delay a bit to make sure empty configuration have been seen by the proxy but do not cause proxy errors
		time.Sleep(300 * time.Millisecond)
	}

	// Configure the proxy with valid endpoint
	port, err := networking.GetFreePort(apiv1.TCP, networking.IPv4LocalhostDefaultAddress, log)
	require.NoError(t, err)
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: networking.IPv4LocalhostDefaultAddress, Port: int32(port)},
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
	proxy, logSink := setupUdpProxy(t, ctx)
	defer logSink.AssertNotCalled(t, "Error")

	// Feed the config to the proxy
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: networking.IPv4LocalhostDefaultAddress, Port: int32(port)},
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
	proxy, logSink := setupUdpProxy(t, ctx)
	defer logSink.AssertNotCalled(t, "Error")

	// Feed the config to the proxy
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: networking.IPv4LocalhostDefaultAddress, Port: int32(port)},
		},
	}
	err := proxy.Configure(config)
	require.NoError(t, err)

	// Set up two clients
	clientConn1, err := net.ListenPacket("udp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, 0))
	require.NoError(t, err)
	require.NotNil(t, clientConn1)
	clientConn2, err := net.ListenPacket("udp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, 0))
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

func TestTCPParkedConnectionMaxBufferSize(t *testing.T) {
	t.Parallel()

	// Set up a proxy with no endpoints configured
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	proxy, logSink := setupTcpProxy(t, ctx)
	defer logSink.AssertNotCalled(t, "Error")

	// Configure the proxy with no endpoints to force connection parking
	emptyConfig := ProxyConfig{
		Endpoints: []Endpoint{},
	}
	err := proxy.Configure(emptyConfig)
	require.NoError(t, err)

	// Connect a client to the proxy
	clientConn, err := net.Dial("tcp", networking.AddressAndPort(proxy.EffectiveAddress(), proxy.EffectivePort()))
	require.NoError(t, err)
	require.NotNil(t, clientConn)
	defer clientConn.Close()

	// The buffer max size is 1MB (1024 * 1024 bytes)
	const maxBufferSize = 1024 * 1024
	const chunkSize = 64 * 1024 // 64KB chunks
	chunk := make([]byte, chunkSize)

	var totalWritten atomic.Int64
	writeCompleted := make(chan struct{}, 1)

	// Write in a goroutine since it should block when buffer is full
	go func() {
		// Try to write 4MB which is well beyond the 1MB limit
		for i := 0; i < 64; i++ {
			n, writeErr := clientConn.Write(chunk)
			if writeErr != nil {
				break
			}
			totalWritten.Add(int64(n))
		}
		close(writeCompleted)
	}()

	// Wait for writes to fill the buffer and block
	select {
	case <-writeCompleted:
		// If write completed, it means the connection was closed or errored
		// This shouldn't happen since we're just buffering with no endpoints
		t.Fatalf("Write unexpectedly completed with %d bytes written", totalWritten.Load())
	case <-time.After(2 * time.Second):
		// Write blocked as expected when buffer is full
		written := totalWritten.Load()
		t.Logf("Blocked after writing %d bytes (max buffer: %d)", written, maxBufferSize)

		// The total buffered should be roughly maxBufferSize + socket buffers
		// Socket buffers can add 256KB-512KB on each side, so we allow for up to 2MB total
		// The key assertion is that it's bounded and didn't grow to 4MB (our attempted write)
		require.Greater(t, written, int64(maxBufferSize/2),
			"Should have written a substantial amount before blocking")
		require.Less(t, written, int64(4*1024*1024),
			"Should have blocked before writing all 4MB, proving the buffer limit works")
	}
}

func TestTCPParkedConnectionFlushesDataOnEndpointConfigure(t *testing.T) {
	t.Parallel()

	// Set up a proxy with no endpoints configured
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	proxy, logSink := setupTcpProxy(t, ctx)
	defer logSink.AssertNotCalled(t, "Error")

	// Configure the proxy with no endpoints to force connection parking
	emptyConfig := ProxyConfig{
		Endpoints: []Endpoint{},
	}
	err := proxy.Configure(emptyConfig)
	require.NoError(t, err)

	// Connect a client to the proxy and send data while no endpoints are configured
	clientConn, err := net.Dial("tcp", networking.AddressAndPort(proxy.EffectiveAddress(), proxy.EffectivePort()))
	require.NoError(t, err)
	require.NotNil(t, clientConn)
	defer clientConn.Close()

	// Generate test data to send
	const dataSize = 128 * 1024 // 128KB of data
	testData := []byte(testutil.GetRandLetters(t, dataSize))

	// Write data while no endpoints are configured (data will be parked)
	bytesWritten, writeErr := clientConn.Write(testData)
	require.NoError(t, writeErr)
	require.Equal(t, dataSize, bytesWritten)

	// Give the proxy time to receive and buffer the data
	time.Sleep(100 * time.Millisecond)

	// Now set up a server to receive the data
	serverListener, port := setupTcpServer(t, autoAllocatePort)
	defer serverListener.Close()

	// Configure the proxy with the new endpoint
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: networking.IPv4LocalhostDefaultAddress, Port: int32(port)},
		},
	}
	configErr := proxy.Configure(config)
	require.NoError(t, configErr)

	// Accept the connection from the proxy
	serverListener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	serverConn, acceptErr := serverListener.Accept()
	require.NoError(t, acceptErr)
	require.NotNil(t, serverConn)
	defer serverConn.Close()

	// Read all the data that was buffered
	receivedData := make([]byte, dataSize)
	totalRead := 0
	serverConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for totalRead < dataSize {
		n, readErr := serverConn.Read(receivedData[totalRead:])
		if readErr != nil {
			t.Fatalf("Error reading from server connection after %d bytes: %v", totalRead, readErr)
		}
		totalRead += n
	}

	// Verify all data was received correctly
	require.Equal(t, dataSize, totalRead, "Should have received all the data that was sent")
	require.Equal(t, testData, receivedData, "Received data should match sent data")
}

func TestTCPParkedConnectionClosedBeforeEndpointConfigured(t *testing.T) {
	t.Parallel()

	// Set up a proxy with no endpoints configured
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	proxy, logSink := setupTcpProxy(t, ctx)
	defer logSink.AssertNotCalled(t, "Error")

	// Configure the proxy with no endpoints to force connection parking
	emptyConfig := ProxyConfig{
		Endpoints: []Endpoint{},
	}
	err := proxy.Configure(emptyConfig)
	require.NoError(t, err)

	// Connect a client to the proxy and send some data
	clientConn, err := net.Dial("tcp", networking.AddressAndPort(proxy.EffectiveAddress(), proxy.EffectivePort()))
	require.NoError(t, err)
	require.NotNil(t, clientConn)

	// Send some data while no endpoints are configured
	testData := []byte("data that should not be received")
	bytesWritten, writeErr := clientConn.Write(testData)
	require.NoError(t, writeErr)
	require.Equal(t, len(testData), bytesWritten)

	// Give the proxy time to receive and buffer the data
	time.Sleep(500 * time.Millisecond)

	// Close the client connection before configuring an endpoint
	require.NoError(t, clientConn.Close())

	// Give the proxy time to detect the closed connection and clean up
	time.Sleep(500 * time.Millisecond)

	// Now set up a server
	serverListener, port := setupTcpServer(t, autoAllocatePort)
	defer serverListener.Close()

	// Configure the proxy with the new endpoint
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: networking.IPv4LocalhostDefaultAddress, Port: int32(port)},
		},
	}
	configErr := proxy.Configure(config)
	require.NoError(t, configErr)

	// Try to accept a connection - there should be none since the client closed before endpoint was configured
	serverListener.(*net.TCPListener).SetDeadline(time.Now().Add(500 * time.Millisecond))
	serverConn, acceptErr := serverListener.Accept()

	// We expect either no connection (timeout) or a connection that immediately closes with no data
	if acceptErr != nil {
		// Timeout is expected - no connection should be forwarded
		require.True(t, errors.Is(acceptErr, os.ErrDeadlineExceeded),
			"Expected timeout error, got: %v", acceptErr)
	} else {
		// If we got a connection, it should have no data and close immediately
		defer serverConn.Close()
		serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		buffer := make([]byte, 1024)
		n, readErr := serverConn.Read(buffer)
		require.Equal(t, 0, n, "Should not have received any data from a closed parked connection")
		require.Error(t, readErr, "Should get an error reading from a closed connection")
	}
}

func TestRandomEndpointSelection(t *testing.T) {
	t.Parallel()

	const tries = 20

	ctx, cancelFunc := context.WithCancel(context.Background())
	proxy := newNetProxy(apiv1.TCP, networking.IPv4LocalhostDefaultAddress, 0, ctx, logr.Discard())
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

// Run 100 simultaneous clients making requests to a single server behind the proxy
// until the context is cancelled (after 30 seconds).
// The test verifies that the proxy can handle at least 10 000 connections
// (with single, successful request round-trip) during that time.
func TestTCPProxyConnectionThroughput(t *testing.T) {
	testutil.SkipIfNotEnableAdvancedNetworking(t)

	t.Parallel()

	// Set up a TCP server that echoes back data and closes the connection
	serverListener, port := setupTcpServer(t, autoAllocatePort)
	defer serverListener.Close()

	var successfulRequests atomic.Int32

	// Start a goroutine to handle client connections
	go func() {
		for {
			conn, err := serverListener.Accept()
			if err != nil {
				// Server closed, exit the goroutine
				return
			}

			go func(c net.Conn) {
				defer c.Close()

				// Read data from client
				buffer := make([]byte, 1024)
				n, readErr := c.Read(buffer)
				if readErr != nil {
					return
				}

				// Echo the data back
				_, _ = c.Write(buffer[:n])
			}(conn)
		}
	}()

	// Setup test context
	ctx, cancelFunc := testutil.GetTestContext(t, 30*time.Second)
	defer cancelFunc()

	proxy, logSink := setupTcpProxy(t, ctx)
	defer logSink.AssertNotCalled(t, "Error")

	// Feed the config to the proxy
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: networking.IPv4LocalhostDefaultAddress, Port: int32(port)},
		},
	}
	err := proxy.Configure(config)
	require.NoError(t, err)

	// Run 100 simultaneous clients making requests until the context is cancelled
	const numUsers = 100
	var wg sync.WaitGroup
	wg.Add(numUsers)

	errorsChan := make(chan error, numUsers)

	clientsCtx, clientsCancel := context.WithTimeout(ctx, 10*time.Second)
	defer clientsCancel()

	// Function to run a single client connection
	runUser := func(runCtx context.Context) {
		defer wg.Done()

		runClient := func() error {
			// Set up a client connection to the proxy
			clientConn, dialErr := net.Dial("tcp", networking.AddressAndPort(proxy.EffectiveAddress(), proxy.EffectivePort()))
			if dialErr != nil {
				return fmt.Errorf("failed to connect to proxy: %w", dialErr)
			}
			defer clientConn.Close()

			clientStop := context.AfterFunc(runCtx, func() {
				clientConn.Close()
			})
			defer clientStop()

			// Generate 500 bytes of random data
			randomData := []byte(testutil.GetRandLetters(t, 500))

			// Send the data to the proxy
			_, writeErr := clientConn.Write(randomData)
			if writeErr != nil {
				return fmt.Errorf("failed to write to proxy: %w", writeErr)
			}

			// Read the response
			responseData := make([]byte, len(randomData))
			_, readErr := clientConn.Read(responseData)
			if readErr != nil {
				return fmt.Errorf("failed to read from proxy: %w", readErr)
			}

			// Verify the received data matches what was sent
			if !slices.Equal(randomData, responseData) {
				return fmt.Errorf("data mismatch: sent %d bytes, received %d bytes with different content",
					len(randomData), len(responseData))
			}

			successfulRequests.Add(1)

			return nil
		}

		// Run the client in a loop until the context is cancelled or an error occurs
		for {
			if runCtx.Err() != nil {
				// Context was cancelled, exit the loop
				return
			}

			clientErr := runClient()
			if clientErr != nil {
				if runCtx.Err() == nil {
					// Report the error if the context hasn't been cancelled
					errorsChan <- clientErr
				}
				return
			}
		}
	}

	// Launch all clients in parallel
	for i := 0; i < numUsers; i++ {
		go runUser(clientsCtx)
	}

	// Wait for all clients to finish
	wg.Wait()
	close(errorsChan)

	// Check for errors
	var clientErr error
	for err := range errorsChan {
		clientErr = errors.Join(clientErr, err)
	}

	require.NoError(t, clientErr, "Error running client connections")

	t.Logf("Successful requests: %d", successfulRequests.Load())
	require.Greater(t, successfulRequests.Load(), int32(10_000), "Client throughput is less than expected")
}

// This test connects a .NET server and client via the proxy in order to reproduce a specific issue with continuous streams
// in older DCP releases. In versions before v0.9.2, the data received by the client would eventually be corrupted.
func TestTCPProxyContinuousStream(t *testing.T) {
	testutil.SkipIfNotEnableAdvancedNetworking(t)

	t.Parallel()

	serverPort, portErr := networking.GetFreePort(apiv1.TCP, networking.IPv4LocalhostDefaultAddress, log)
	require.NoError(t, portErr, "Failed to get free port for server")

	// Set up a proxy
	ctx, cancelFunc := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancelFunc()
	proxy, logSink := setupTcpProxy(t, ctx)
	defer logSink.AssertNotCalled(t, "Error")

	rootDir, findRootErr := osutil.FindRootFor(osutil.DirTarget, "test")
	require.NoError(t, findRootErr, "Failed to find root directory for ./test")

	go func() {
		// If the server exits due to an error, cancel the context
		defer cancelFunc()

		serverCmd := exec.CommandContext(ctx, "dotnet", "run")
		serverCmd.Dir = filepath.Join(rootDir, "test", "HttpContentStreamRepro.Server")
		serverCmd.Env = os.Environ()
		serverCmd.Env = append(serverCmd.Env, fmt.Sprintf("ASPNETCORE_URLS=http://localhost:%d", serverPort))
		serverErr := serverCmd.Run()
		if ctx.Err() == nil {
			// If the context is not cancelled, make sure the server didn't return an error
			require.NoError(t, serverErr, "Failed running server")
		}
	}()

	// Feed the config to the proxy
	config := ProxyConfig{
		Endpoints: []Endpoint{
			{Address: networking.IPv4LocalhostDefaultAddress, Port: int32(serverPort)},
		},
	}
	err := proxy.Configure(config)
	require.NoError(t, err)

	clientCmd := exec.CommandContext(ctx, "dotnet", "run")
	clientCmd.Dir = filepath.Join(rootDir, "test", "HttpContentStreamRepro.Client")
	clientCmd.Env = os.Environ()
	clientCmd.Env = append(clientCmd.Env, fmt.Sprintf("SERVER_URL=http://%s", networking.AddressAndPort(proxy.EffectiveAddress(), proxy.EffectivePort())))
	clientCmd.Stderr = os.Stderr
	clientErr := clientCmd.Run()

	if ctx.Err() == nil {
		// If the context is not cancelled, make sure the client didn't return an error
		require.NoError(t, clientErr)
	}
}

const autoAllocatePort = 0

func setupTcpServer(t *testing.T, port int32) (net.Listener, int) {
	serverListener, err := net.Listen("tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, port))
	require.NoError(t, err)
	require.NotNil(t, serverListener)
	effectivePort := serverListener.Addr().(*net.TCPAddr).Port
	require.Greater(t, effectivePort, 0)
	return serverListener, effectivePort
}

func setupTcpProxy(t *testing.T, ctx context.Context) (*netProxy, *testutil.MockLoggerSink) {
	logSink := testutil.NewMockLoggerSink()
	proxy := newNetProxy(apiv1.TCP, networking.IPv4LocalhostDefaultAddress, 0, ctx, logr.New(logSink))
	require.NotNil(t, proxy)

	err := proxy.Start()
	require.NoError(t, err)
	require.Equal(t, networking.IPv4LocalhostDefaultAddress, proxy.EffectiveAddress())
	require.Greater(t, proxy.EffectivePort(), int32(0))
	return proxy, logSink
}

func verifiyProxiedTcpConnection(t *testing.T, proxy *netProxy, server net.Listener) {
	// Set up a client
	clientConn, err := net.Dial("tcp", networking.AddressAndPort(proxy.EffectiveAddress(), proxy.EffectivePort()))
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
	serverConn, err := net.ListenPacket("udp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, port))
	require.NoError(t, err)
	require.NotNil(t, serverConn)
	effectivePort := serverConn.LocalAddr().(*net.UDPAddr).Port
	require.Greater(t, effectivePort, 0)
	return serverConn, effectivePort
}

func setupUdpProxy(t *testing.T, ctx context.Context) (*netProxy, *testutil.MockLoggerSink) {
	logSink := testutil.NewMockLoggerSink()
	proxy := newNetProxy(apiv1.UDP, networking.IPv4LocalhostDefaultAddress, 0, ctx, logr.New(logSink))
	require.NotNil(t, proxy)

	err := proxy.Start()
	require.NoError(t, err)
	require.Equal(t, networking.IPv4LocalhostDefaultAddress, proxy.EffectiveAddress())
	require.Greater(t, proxy.EffectivePort(), int32(0))
	return proxy, logSink
}

func verifyProxiedUdpConnection(t *testing.T, proxy *netProxy, serverConn net.PacketConn) {
	// Set up a client
	clientConn, err := net.ListenPacket("udp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, 0))
	require.NoError(t, err)
	require.NotNil(t, clientConn)

	verifyUDPPacketFlow(t, proxy, serverConn, clientConn)

	// Clean up
	require.NoError(t, clientConn.Close())
}

func verifyUDPPacketFlow(
	t *testing.T,
	proxy *netProxy,
	serverConn net.PacketConn,
	clientConn net.PacketConn,
) {
	// Have the client send a message
	msg := []byte(testutil.GetRandLetters(t, 6))
	n, err := clientConn.WriteTo(msg, &net.UDPAddr{IP: net.ParseIP(proxy.EffectiveAddress()), Port: int(proxy.EffectivePort())})
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
	require.Equal(t, proxy.EffectiveAddress(), proxyAddr.(*net.UDPAddr).IP.String())
	require.Equal(t, proxy.EffectivePort(), int32(proxyAddr.(*net.UDPAddr).Port))
	require.True(t, bytes.Equal(buffer, msg), "Response message received by the client ('%s') is not the same as the message sent by the server ('%s')", string(buffer), string(msg))
}
