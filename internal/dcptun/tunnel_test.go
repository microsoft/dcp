/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	stdproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

const autoAllocatePort = 0

// Verifies that a server and client proxy pair can be created and used to establish a functional tunnel.
func TestTunnelServerAndClientProxies(t *testing.T) {
	t.Parallel()

	const testTimeout = 30 * time.Second
	ctx, cancelFunc := testutil.GetTestContext(t, testTimeout)
	defer cancelFunc()
	log := testutil.NewLogForTesting(t.Name())

	testServerListener, testServerPort := setupEchoServer(t)
	defer testServerListener.Close()

	pair := createProxyPair(t, ctx, log, proxyPairConfig{})
	defer pair.cleanup()
	serverProxyClient := pair.control

	tunnelReq := &proto.TunnelReq{
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(testServerPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ClientProxyPort:    stdproto.Int32(autoAllocatePort),
	}

	tunnelSpec, prepareTunnelErr := serverProxyClient.PrepareTunnel(ctx, tunnelReq)
	require.NoError(t, prepareTunnelErr)
	require.NotNil(t, tunnelSpec)
	require.NotNil(t, tunnelSpec.ServerAddress)
	require.Equal(t, networking.IPv4LocalhostDefaultAddress, *tunnelSpec.ServerAddress)
	require.NotNil(t, tunnelSpec.ServerPort)
	require.Equal(t, int32(testServerPort), *tunnelSpec.ServerPort)
	require.Len(t, tunnelSpec.ClientProxyAddresses, 1)
	require.Equal(t, tunnelSpec.ClientProxyAddresses[0], networking.IPv4LocalhostDefaultAddress)
	require.NotNil(t, tunnelSpec.ClientProxyPort)
	require.Greater(t, *tunnelSpec.ClientProxyPort, int32(0))

	testMessage := []byte("Hello tunnel!\n")
	roundTripErr := doRoundTrip(
		ctx,
		networking.IPv4LocalhostDefaultAddress,
		*tunnelSpec.ClientProxyPort,
		testTimeout,
		func() []byte { return testMessage },
	)
	require.NoError(t, roundTripErr, "Failed to perform round trip through tunnel")

	tunnelRef := &proto.TunnelRef{TunnelId: tunnelSpec.TunnelRef.TunnelId}
	_, tunnelDeleteErr := serverProxyClient.DeleteTunnel(ctx, tunnelRef)
	require.NoError(t, tunnelDeleteErr)
}

// Runs 100 simultaneous clients making requests to a single server behind the proxy
// until the context is cancelled (after 30 seconds).
// The test verifies that the tunnel can handle at least 5000 connections
// (with single, successful request round-trip) during that time.
func TestTunnelConnectionThroughput(t *testing.T) {
	testutil.SkipIfNotEnableAdvancedNetworking(t)

	// Do not run this test in parallel with others (no t.Parallel() here).

	const numUsers = 100
	const runDuration = 10 * time.Second
	var successfulRoundTrips atomic.Uint32

	testCtx, testCtxCancel := testutil.GetTestContext(t, 3*runDuration)
	defer testCtxCancel()
	log := testutil.NewLogForTesting(t.Name())

	testServerListener, testServerPort := setupEchoServer(t)
	defer testServerListener.Close()

	pair := createProxyPair(t, testCtx, log, proxyPairConfig{})
	defer pair.cleanup()
	serverProxyClient := pair.control

	tunnelReq := &proto.TunnelReq{
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(testServerPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ClientProxyPort:    stdproto.Int32(autoAllocatePort),
	}
	tunnelSpec, prepareTunnelErr := serverProxyClient.PrepareTunnel(testCtx, tunnelReq)
	require.NoError(t, prepareTunnelErr)

	var usersWg sync.WaitGroup
	usersWg.Add(numUsers)
	userCtx, userCtxCancel := context.WithTimeout(testCtx, runDuration)
	defer userCtxCancel()

	runUser := func(runCtx context.Context, userId int) {
		defer usersWg.Done()

		for {
			if runCtx.Err() != nil {
				return
			}

			roundTripErr := doRoundTrip(
				testCtx,
				networking.IPv4LocalhostDefaultAddress,
				*tunnelSpec.ClientProxyPort,
				2*runDuration, // Longer than run duration but shorter than overall test timeout
				func() []byte {
					return fmt.Appendf([]byte{}, "Hello tunnel user %d!\n", userId)
				},
			)
			if runCtx.Err() == nil {
				require.NoError(t, roundTripErr, "Failed to perform round trip through tunnel for user %d", userId)
				successfulRoundTrips.Add(1)
			}
		}
	}

	for i := 0; i < numUsers; i++ {
		go runUser(userCtx, i)
	}

	usersWg.Wait()
	t.Logf("Successful requests: %d", successfulRoundTrips.Load())
	require.Greater(t, successfulRoundTrips.Load(), uint32(5000), "Tunnel performance is lower than expected")
}

// Uses a .NET-based server and client via the tunnel in order to verify that the tunnel can handle
// a continuous stream of data from a .NET application. In DCP versions before 0.9.2 the data received
// by the client would eventually become corrupted.
func TestTunnelDataThroughput(t *testing.T) {
	testutil.SkipIfNotEnableAdvancedNetworking(t)

	// Do not run this test in parallel with others (no t.Parallel() here).

	const runDuration = 35 * time.Second

	testCtx, testCtxCancel := testutil.GetTestContext(t, 2*runDuration)
	defer testCtxCancel()
	log := testutil.NewLogForTesting(t.Name())

	serverPort, portErr := networking.GetFreePort(commonapi.PortProtocolTCP, networking.IPv4LocalhostDefaultAddress, log)
	require.NoError(t, portErr, "Failed to get free port for server")

	pair := createProxyPair(t, testCtx, log, proxyPairConfig{})
	defer pair.cleanup()
	serverProxyClient := pair.control

	tunnelReq := &proto.TunnelReq{
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(serverPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ClientProxyPort:    stdproto.Int32(autoAllocatePort),
	}
	tunnelSpec, prepareTunnelErr := serverProxyClient.PrepareTunnel(testCtx, tunnelReq)
	require.NoError(t, prepareTunnelErr)

	rootDir, findRootErr := osutil.FindRootFor(osutil.DirTarget, "test")
	require.NoError(t, findRootErr, "Failed to find root directory for ./test")

	runCtx, runCtxCancel := context.WithTimeout(testCtx, runDuration)
	defer runCtxCancel()

	go func() {
		// If the server exits due to an error, cancel the context
		defer runCtxCancel()

		serverCmd := exec.CommandContext(runCtx, "dotnet", "run")
		serverCmd.Dir = filepath.Join(rootDir, "test", "HttpContentStreamRepro.Server")
		serverCmd.Env = os.Environ()
		serverCmd.Env = append(serverCmd.Env, fmt.Sprintf("ASPNETCORE_URLS=http://localhost:%d", serverPort))
		var serverStdout, serverStderr bytes.Buffer
		serverCmd.Stdout = &serverStdout
		serverCmd.Stderr = &serverStderr
		serverErr := serverCmd.Run()
		if runCtx.Err() == nil {
			require.NoError(t, serverErr, "Failed running server\nStdout:\n%s\nStderr:\n%s", serverStdout.String(), serverStderr.String())
		}
	}()

	clientCmd := exec.CommandContext(runCtx, "dotnet", "run")
	clientCmd.Dir = filepath.Join(rootDir, "test", "HttpContentStreamRepro.Client")
	clientCmd.Env = os.Environ()
	clientCmd.Env = append(clientCmd.Env, fmt.Sprintf("SERVER_URL=http://%s", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, *tunnelSpec.ClientProxyPort)))
	var clientStdout, clientStderr bytes.Buffer
	clientCmd.Stdout = &clientStdout
	clientCmd.Stderr = &clientStderr
	clientErr := clientCmd.Run()
	if runCtx.Err() == nil {
		require.NoError(t, clientErr, "Failed running client\nStdout:\n%s\nStderr:\n%s", clientStdout.String(), clientStderr.String())
	}
}

// Verifies that multiple calls to PrepareTunnel with the same parameters are idempotent.
// That is, the same combination of (server address, server port, client proxy address, client proxy port)
// passed as TunnelReq to PrepareTunnel() results in the same TunnelID.
func TestTunnelPreparationIdempotency(t *testing.T) {
	t.Parallel()

	const testTimeout = 30 * time.Second
	ctx, cancelFunc := testutil.GetTestContext(t, testTimeout)
	defer cancelFunc()
	log := testutil.NewLogForTesting(t.Name())

	pair := createProxyPair(t, ctx, log, proxyPairConfig{})
	defer pair.cleanup()
	serverProxyClient := pair.control

	const testServerPort = 29307 // Value does not matter, just need a reasonable port number.

	// Define the same tunnel request parameters
	tunnelReq := &proto.TunnelReq{
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(testServerPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ClientProxyPort:    stdproto.Int32(8080), // Use a fixed port instead of auto-allocate for idempotency
	}

	// Call PrepareTunnel multiple times with the same parameters
	var tunnelSpecs []*proto.TunnelSpec
	const repeatedCalls = 5
	for i := 0; i < repeatedCalls; i++ {
		tunnelSpec, prepareTunnelErr := serverProxyClient.PrepareTunnel(ctx, tunnelReq)
		require.NoError(t, prepareTunnelErr, "PrepareTunnel call %d failed", i+1)
		require.NotNil(t, tunnelSpec, "TunnelSpec is nil for call %d", i+1)
		tunnelSpecs = append(tunnelSpecs, tunnelSpec)
	}

	// Verify that all calls returned the same TunnelID
	baseTunnelId := tunnelSpecs[0].GetTunnelRef().GetTunnelId()
	for i, spec := range tunnelSpecs[1:] {
		require.Equal(t, baseTunnelId, spec.GetTunnelRef().GetTunnelId(),
			"TunnelID from call %d does not match the first call", i+2)
	}

	// Also verify that the tunnel specs are functionally equivalent
	for i, spec := range tunnelSpecs[1:] {
		require.True(t, tunnelSpecs[0].Same(spec),
			"TunnelSpec from call %d is not equivalent to the first call", i+2)
	}

	// Test that a tunnel with different parameters gets a different TunnelID
	differentTunnelReq := &proto.TunnelReq{
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(testServerPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ClientProxyPort:    stdproto.Int32(8081), // Different port
	}

	differentTunnelSpec, prepareDifferentErr := serverProxyClient.PrepareTunnel(ctx, differentTunnelReq)
	require.NoError(t, prepareDifferentErr)
	require.NotNil(t, differentTunnelSpec)

	// Verify that the different tunnel has a different ID
	require.NotEqual(t, baseTunnelId, differentTunnelSpec.GetTunnelRef().GetTunnelId(),
		"Different tunnel parameters should result in different TunnelID")

	// Clean up tunnels
	tunnelRef := &proto.TunnelRef{TunnelId: tunnelSpecs[0].TunnelRef.TunnelId}
	_, tunnelDeleteErr := serverProxyClient.DeleteTunnel(ctx, tunnelRef)
	require.NoError(t, tunnelDeleteErr)

	differentTunnelRef := &proto.TunnelRef{TunnelId: differentTunnelSpec.TunnelRef.TunnelId}
	_, differentTunnelDeleteErr := serverProxyClient.DeleteTunnel(ctx, differentTunnelRef)
	require.NoError(t, differentTunnelDeleteErr)
}

// Runs single round trip to the echo server.
func doRoundTrip(testContext context.Context, address string, port int32, timeout time.Duration, getMessage func() []byte) error {
	testMessage := getMessage()
	clientConn, dialErr := resiliency.RetryGetExponential(testContext, func() (net.Conn, error) {
		var d net.Dialer
		dialCtx, dialCtxCancel := context.WithTimeout(context.Background(), timeout)
		defer dialCtxCancel()
		return d.DialContext(dialCtx, "tcp", networking.AddressAndPort(address, port))
	})
	if dialErr != nil {
		return fmt.Errorf("doRoundTrip: dial failed: %w:", dialErr)
	}
	defer func() { _ = clientConn.Close() }()

	deadlineErr := clientConn.SetDeadline(time.Now().Add(timeout))
	if deadlineErr != nil {
		return fmt.Errorf("doRoundTrip: set deadline failed: %w:", deadlineErr)
	}

	_, clientWriteErr := clientConn.Write(testMessage)
	if clientWriteErr != nil {
		return fmt.Errorf("doRoundTrip: data write failed: %w:", clientWriteErr)
	}

	reader := bufio.NewReader(clientConn)
	retMsg, clientReadErr := reader.ReadBytes('\n')
	if clientReadErr != nil {
		return fmt.Errorf("doRoundTrip: data read failed: %w:", clientReadErr)
	}
	if !bytes.Equal(testMessage, retMsg) {
		return fmt.Errorf("received message does not match sent message: expected %q, got %q", testMessage, retMsg)
	}

	return nil
}

// Creates a TCP server that echoes back any received data
func setupEchoServer(t *testing.T) (net.Listener, int) {
	listener, listenErr := net.Listen("tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, autoAllocatePort))
	require.NoError(t, listenErr, "Failed to create echo server listener")

	effectivePort := listener.Addr().(*net.TCPAddr).Port
	require.Greater(t, effectivePort, 0)

	// Start echo server goroutine
	go func() {
		for {
			conn, connErr := listener.Accept()
			if errors.Is(connErr, net.ErrClosed) {
				return
			}
			require.NoError(t, connErr, "Echo server: failed to accept connection")

			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)

				// Do not fail a test if the network I/O operation fails, just close the connection.

				for {
					line, readErr := reader.ReadBytes('\n')
					if readErr == io.EOF {
						return // Client closed the connection, this is expected.
					}
					if readErr != nil {
						t.Logf("Echo server: failed to read line from connection: %v", readErr)
						return
					}

					_, writeErr := conn.Write(line)
					if writeErr != nil {
						t.Logf("Echo server: failed to write line to connection: %v", writeErr)
						return
					}
				}
			}()
		}
	}()

	return listener, effectivePort
}

// setupListener creates a TCP listener on the specified port
func setupListener(t *testing.T, port int32) (net.Listener, int) {
	listener, err := net.Listen("tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, port))
	require.NoError(t, err)
	require.NotNil(t, listener)

	effectivePort := listener.Addr().(*net.TCPAddr).Port
	require.Greater(t, effectivePort, 0)

	return listener, effectivePort
}

func createProxyPair(t *testing.T, ctx context.Context, log logr.Logger, config proxyPairConfig) proxyPair {
	securityConfig, securityConfigErr := NewTunnelProxySecurityConfig()
	require.NoError(t, securityConfigErr, "Failed to create tunnel proxy security configuration")
	clientCertPool, clientCertPoolErr := securityConfig.GetClientPool()
	require.NoError(t, clientCertPoolErr, "Failed to get client CA certificate pool")
	tlsConfig, tlsConfigErr := securityConfig.GetTlsConfig()
	require.NoError(t, tlsConfigErr, "Failed to create TLS configuration")

	clientControlListener, clientControlPort := setupListener(t, autoAllocatePort)

	clientDataListener, clientDataPort := setupListener(t, autoAllocatePort)

	clientProxyCtx, clientProxyCtxCancel := context.WithCancel(ctx)
	clientLog := log.WithName("ClientProxy")
	clientProxy := NewClientProxy(clientProxyCtx, clientDataListener, clientProxyCtxCancel, clientLog)

	clientServerOptions := []grpc.ServerOption{grpc.Creds(credentials.NewTLS(tlsConfig))}
	if config.controlServerInterceptor != nil {
		clientServerOptions = append(clientServerOptions, grpc.StreamInterceptor(config.controlServerInterceptor))
	}
	clientGrpcServer := grpc.NewServer(clientServerOptions...)
	proto.RegisterTunnelControlServer(clientGrpcServer, clientProxy)

	go func() {
		clientServerErr := clientGrpcServer.Serve(clientControlListener)
		require.NoError(t, clientServerErr, "Client proxy gRPC server failed")
	}()

	serverControlListener, serverControlPort := setupListener(t, autoAllocatePort)

	dialOptions := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(clientCertPool, "")),
	}
	if config.controlDialer != nil {
		dialOptions = append(dialOptions, grpc.WithContextDialer(config.controlDialer))
	}
	if config.controlStreamInterceptor != nil {
		dialOptions = append(dialOptions, grpc.WithStreamInterceptor(config.controlStreamInterceptor))
	}
	clientProxyConn, clientProxyErr := grpc.NewClient(
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, int32(clientControlPort)),
		dialOptions...,
	)
	require.NoError(t, clientProxyErr, "Failed to connect to client-side proxy")

	clientProxyClient := proto.NewTunnelControlClient(clientProxyConn)

	serverProxyCtx, serverProxyCtxCancel := context.WithCancel(ctx)
	serverLog := log.WithName("ServerProxy")
	serverProxy := NewServerProxy(
		serverProxyCtx,
		clientProxyClient,
		networking.IPv4LocalhostDefaultAddress,
		int32(clientDataPort),
		serverProxyCtxCancel,
		config.server,
		serverLog,
	)
	require.NotNil(t, serverProxy)

	serverGrpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	proto.RegisterTunnelControlServer(serverGrpcServer, serverProxy)

	go func() {
		serverErr := serverGrpcServer.Serve(serverControlListener)
		require.NoError(t, serverErr, "Server proxy gRPC server failed")
	}()

	serverProxyConn, serverProxyErr := grpc.NewClient(
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, int32(serverControlPort)),
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(clientCertPool, "")),
	)
	require.NoError(t, serverProxyErr)

	serverProxyClient := proto.NewTunnelControlClient(serverProxyConn)

	cleanup := sync.OnceFunc(func() {
		serverProxyCtxCancel()
		clientProxyCtxCancel()
		_ = serverProxyConn.Close()
		_ = clientProxyConn.Close()
		serverGrpcServer.Stop()
		clientGrpcServer.Stop()
		_ = serverControlListener.Close()
		_ = clientControlListener.Close()
		_ = clientDataListener.Close()
	})

	return proxyPair{
		control: serverProxyClient,
		server:  serverProxy,
		client:  clientProxy,
		cleanup: cleanup,
	}
}

// Verifies that ServerProxy replaces a failed control transport without losing
// prepared tunnels, client listeners, or already-established data connections.
func TestTunnelControlReconnectPreservesDataStreams(t *testing.T) {
	t.Parallel()
	testTunnelControlReconnectPreservesDataStreams(t, false)
}

// Verifies that cancelling only the control RPC reuses the TLS transport while
// preserving tunnel identity, listeners, and established data streams.
func TestTunnelControlRPCCancellationPreservesDataStreams(t *testing.T) {
	t.Parallel()
	testTunnelControlReconnectPreservesDataStreams(t, true)
}

func testTunnelControlReconnectPreservesDataStreams(t *testing.T, cancelRPCOnly bool) {
	t.Helper()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	transport := &controlledTunnelTransport{}
	attempts := make(chan context.CancelFunc, 4)
	ready := make(chan struct{}, 4)
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		server:        ServerProxyConfig{ReconnectTimeout: 3 * time.Second},
		controlDialer: transport.dial,
		controlStreamInterceptor: func(callCtx context.Context, desc *grpc.StreamDesc, connection *grpc.ClientConn, method string, streamer grpc.Streamer, options ...grpc.CallOption) (grpc.ClientStream, error) {
			attemptCtx, cancelAttempt := context.WithCancel(callCtx)
			stream, streamErr := streamer(attemptCtx, desc, connection, method, options...)
			if streamErr != nil {
				cancelAttempt()
				return nil, streamErr
			}
			select {
			case attempts <- cancelAttempt:
			case <-callCtx.Done():
				cancelAttempt()
			}
			return &observedTunnelControlStream{ClientStream: stream, ready: ready}, nil
		},
	})
	defer pair.cleanup()
	echoListener, echoPort := setupEchoServer(t)
	defer echoListener.Close()
	request := &proto.TunnelReq{
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(echoPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	}
	spec, prepareErr := pair.control.PrepareTunnel(ctx, request)
	require.NoError(t, prepareErr)
	var dialer net.Dialer
	connection, dialErr := dialer.DialContext(ctx, "tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort()))
	require.NoError(t, dialErr)
	defer connection.Close()
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.NoError(t, connection.SetDeadline(deadline))
	reader := bufio.NewReader(connection)
	roundTrip := func(message string) {
		_, writeErr := io.WriteString(connection, message)
		require.NoError(t, writeErr)
		reply, readErr := reader.ReadString('\n')
		require.NoError(t, readErr)
		require.Equal(t, message, reply)
	}
	roundTrip("before control loss\n")
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("The initial control RPC did not become ready")
	}

	for cycle := range 3 {
		var cancelAttempt context.CancelFunc
		select {
		case cancelAttempt = <-attempts:
		case <-ctx.Done():
			t.Fatal("Control RPC was not established")
		}
		if cancelRPCOnly {
			cancelAttempt()
		} else {
			require.NoError(t, transport.interrupt(false))
		}
		roundTrip(fmt.Sprintf("existing stream during reconnect %d\n", cycle))
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("The replacement control RPC did not acknowledge readiness")
		}
		newConnectionErr := doRoundTrip(ctx, networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort(), 10*time.Second, func() []byte {
			return []byte("new stream after reconnect\n")
		})
		require.NoError(t, newConnectionErr)
		observed, observeErr := pair.control.PrepareTunnel(ctx, request)
		require.NoError(t, observeErr)
		require.True(t, stdproto.Equal(spec, observed), "Reconnect must preserve the complete tunnel spec")
	}
	require.NoError(t, pair.server.Err())
	select {
	case <-pair.client.Done():
		t.Fatal("Control transport loss disposed the client proxy")
	default:
	}
	if cancelRPCOnly {
		transport.lock.Lock()
		dialCount := transport.dialCount
		transport.lock.Unlock()
		require.Equal(t, 1, dialCount, "RPC-only cancellation must not replace the TLS transport")
	}
}

type observedTunnelControlStream struct {
	grpc.ClientStream
	ready chan<- struct{}
}

func (stream *observedTunnelControlStream) Header() (metadata.MD, error) {
	headers, headerErr := stream.ClientStream.Header()
	if headerErr != nil {
		return nil, headerErr
	}
	select {
	case stream.ready <- struct{}{}:
	case <-stream.Context().Done():
		return nil, stream.Context().Err()
	}
	return headers, nil
}

// Verifies that ServerProxy retires its establishment watchdog after readiness,
// allowing a healthy idle control stream to outlive the reconnect budget.
func TestTunnelControlReconnectBudgetDoesNotLimitHealthyStream(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	const reconnectTimeout = time.Second
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		server: ServerProxyConfig{ReconnectTimeout: reconnectTimeout},
	})
	defer pair.cleanup()
	echoListener, echoPort := setupEchoServer(t)
	defer echoListener.Close()
	spec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		ServerPort: stdproto.Int32(int32(echoPort)),
	})
	require.NoError(t, prepareErr)
	message := func() []byte { return []byte("healthy control stream\n") }
	require.NoError(t, doRoundTrip(ctx, networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort(), 10*time.Second, message))

	// Waiting past this exact boundary is necessary to detect a deadline
	// accidentally retained on the long-lived RPC.
	timer := time.NewTimer(2 * reconnectTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatal("Test context ended before the establishment budget elapsed")
	case <-timer.C:
	}
	require.NoError(t, doRoundTrip(ctx, networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort(), 10*time.Second, message))
	require.NoError(t, pair.server.Err())
}

// Verifies that ServerProxy bounds reconnect even while WaitForReady blocks,
// reports budget exhaustion, and leaves pair recreation to the controller.
func TestTunnelControlReconnectBudgetExhaustion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	transport := &controlledTunnelTransport{}
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		server:        ServerProxyConfig{ReconnectTimeout: time.Second},
		controlDialer: transport.dial,
	})
	defer pair.cleanup()
	echoListener, echoPort := setupEchoServer(t)
	defer echoListener.Close()
	spec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		ServerPort: stdproto.Int32(int32(echoPort)),
	})
	require.NoError(t, prepareErr)
	require.NoError(t, doRoundTrip(ctx, networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort(), 10*time.Second, func() []byte {
		return []byte("before blocked transport\n")
	}))
	require.NoError(t, transport.interrupt(true))

	select {
	case <-ctx.Done():
		t.Fatal("Server proxy did not stop after exhausting its reconnect budget")
	case <-pair.server.Done():
	}
	require.ErrorIs(t, pair.server.Err(), context.DeadlineExceeded)
	select {
	case <-pair.client.Done():
		t.Fatal("RPC failure directly disposed the client instead of letting the controller replace the pair")
	default:
	}
}

// Verifies that ServerProxy bounds a control RPC that never acknowledges
// ownership, including cancellation of the blocked client handler.
func TestTunnelControlReconnectBudgetBoundsReadiness(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	handlerStopped := make(chan struct{})
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		server: ServerProxyConfig{ReconnectTimeout: time.Second},
		controlServerInterceptor: func(_ any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
			defer close(handlerStopped)
			<-stream.Context().Done()
			return status.FromContextError(stream.Context().Err()).Err()
		},
	})
	defer pair.cleanup()
	select {
	case <-pair.server.Done():
	case <-ctx.Done():
		t.Fatal("Control readiness did not respect the reconnect budget")
	}
	require.ErrorIs(t, pair.server.Err(), context.DeadlineExceeded)
	select {
	case <-handlerStopped:
	case <-ctx.Done():
		t.Fatal("The abandoned control RPC handler did not stop")
	}
}

// Verifies that cancelling an ambiguous stream handshake closes its pending
// sockets and cannot deliver a stale completion through the replacement RPC.
func TestTunnelControlReconnectAbandonsPendingHandshake(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	transport := &controlledTunnelTransport{}
	blocked := make(chan struct{})
	var blockFirst sync.Once
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		controlDialer: transport.dial,
		controlStreamInterceptor: func(callCtx context.Context, desc *grpc.StreamDesc, connection *grpc.ClientConn, method string, streamer grpc.Streamer, options ...grpc.CallOption) (grpc.ClientStream, error) {
			stream, streamErr := streamer(callCtx, desc, connection, method, options...)
			if streamErr != nil {
				return nil, streamErr
			}
			return &blockedTunnelResult{ClientStream: stream, blockFirst: &blockFirst, blocked: blocked}, nil
		},
	})
	defer pair.cleanup()
	backend, backendPort := setupListener(t, autoAllocatePort)
	defer backend.Close()
	stopBackend := context.AfterFunc(ctx, func() { _ = backend.Close() })
	defer stopBackend()
	spec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{ServerPort: stdproto.Int32(int32(backendPort))})
	require.NoError(t, prepareErr)
	var dialer net.Dialer
	pending, dialErr := dialer.DialContext(ctx, "tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort()))
	require.NoError(t, dialErr)
	defer pending.Close()
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.NoError(t, pending.SetDeadline(deadline))
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("The first handshake did not reach the control reply")
	}
	abandonedBackend, acceptErr := backend.Accept()
	require.NoError(t, acceptErr)
	defer abandonedBackend.Close()
	require.NoError(t, abandonedBackend.SetDeadline(deadline))
	require.NoError(t, transport.interrupt(false))
	_, backendReadErr := abandonedBackend.Read(make([]byte, 1))
	require.Error(t, backendReadErr)
	require.False(t, os.IsTimeout(backendReadErr), "The abandoned backend socket must close promptly")
	_, clientReadErr := pending.Read(make([]byte, 1))
	require.Error(t, clientReadErr)
	require.False(t, os.IsTimeout(clientReadErr), "The ambiguous client handshake must fail rather than be replayed")

	echoDone := make(chan error, 1)
	go func() {
		connection, newAcceptErr := backend.Accept()
		if newAcceptErr != nil {
			echoDone <- newAcceptErr
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(deadline)
		_, echoErr := io.Copy(connection, connection)
		echoDone <- echoErr
	}()
	require.NoError(t, doRoundTrip(ctx, networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort(), 10*time.Second, func() []byte {
		return []byte("only the new handshake completes\n")
	}))
	select {
	case echoErr := <-echoDone:
		require.NoError(t, echoErr)
	case <-ctx.Done():
		t.Fatal("The replacement stream did not finish")
	}
	require.NoError(t, pair.server.Err())
}

type blockedTunnelResult struct {
	grpc.ClientStream
	blockFirst *sync.Once
	blocked    chan<- struct{}
}

func (stream *blockedTunnelResult) SendMsg(message any) error {
	block := false
	stream.blockFirst.Do(func() {
		block = true
		close(stream.blocked)
	})
	if block {
		<-stream.Context().Done()
		return status.FromContextError(stream.Context().Err()).Err()
	}
	return stream.ClientStream.SendMsg(message)
}

// Verifies that shutdown cancels a blocked reconnect and closes parked client
// sockets without waiting for the reconnect budget or reporting recovery failure.
func TestTunnelControlReconnectShutdown(t *testing.T) {
	t.Parallel()
	for _, shutdown := range []string{"lifetime", "rpc"} {
		t.Run(shutdown, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
			defer cancel()
			pairCtx, cancelPair := context.WithCancel(ctx)
			defer cancelPair()
			transport := &controlledTunnelTransport{}
			pair := createProxyPair(t, pairCtx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
				controlDialer: transport.dial,
			})
			defer pair.cleanup()
			echoListener, echoPort := setupEchoServer(t)
			defer echoListener.Close()
			spec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{ServerPort: stdproto.Int32(int32(echoPort))})
			require.NoError(t, prepareErr)
			require.NoError(t, doRoundTrip(ctx, networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort(), 10*time.Second, func() []byte {
				return []byte("before shutdown\n")
			}))
			require.NoError(t, transport.interrupt(true))
			var dialer net.Dialer
			parked, dialErr := dialer.DialContext(ctx, "tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort()))
			require.NoError(t, dialErr)
			defer parked.Close()
			deadline, hasDeadline := ctx.Deadline()
			require.True(t, hasDeadline)
			require.NoError(t, parked.SetDeadline(deadline))
			if shutdown == "lifetime" {
				cancelPair()
			} else {
				// Shutdown must cancel local recovery even if the peer is unreachable.
				shutdownCtx, cancelShutdown := context.WithCancel(ctx)
				cancelShutdown()
				_, shutdownErr := pair.server.Shutdown(shutdownCtx, &emptypb.Empty{})
				require.Error(t, shutdownErr)
			}
			select {
			case <-pair.server.Done():
			case <-ctx.Done():
				t.Fatal("Shutdown did not interrupt reconnect")
			}
			require.NoError(t, pair.server.Err())
			if shutdown == "rpc" {
				cancelPair()
			}
			_, readErr := parked.Read(make([]byte, 1))
			require.Error(t, readErr, "Shutdown must close pending client connections")
			require.False(t, os.IsTimeout(readErr), "The parked connection must close before the test deadline")
		})
	}
}

// Verifies that requests queued during a transport outage resume exactly once,
// and tunnels added or deleted while disconnected retain their intended lifecycle.
func TestTunnelControlReconnectQueuedRequestsAndTunnelChanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	transport := &controlledTunnelTransport{}
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		controlDialer: transport.dial,
		server:        ServerProxyConfig{ReconnectTimeout: 10 * time.Second},
	})
	defer pair.cleanup()
	echoListener, echoPort := setupEchoServer(t)
	defer echoListener.Close()
	spec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{ServerPort: stdproto.Int32(int32(echoPort))})
	require.NoError(t, prepareErr)
	require.NoError(t, doRoundTrip(ctx, networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort(), 10*time.Second, func() []byte {
		return []byte("before queued requests\n")
	}))
	retired, retirePrepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		ServerPort: stdproto.Int32(int32(echoPort)), ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	})
	require.NoError(t, retirePrepareErr)
	require.NoError(t, transport.interrupt(true))

	oldHandlerStopped := wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		if !pair.client.controlStreamLock.TryLock() {
			return false, nil
		}
		pair.client.controlStreamLock.Unlock()
		return true, nil
	})
	require.NoError(t, oldHandlerStopped)
	_, offlineDeleteErr := pair.control.DeleteTunnel(ctx, retired.GetTunnelRef())
	if offlineDeleteErr != nil {
		require.Equal(t, codes.Internal, status.Code(offlineDeleteErr), "The unreachable client may reject deletion until reconnect")
	}
	pair.server.lock.Lock()
	retiredData := pair.server.tunnels[TunnelID(retired.GetTunnelRef().GetTunnelId())]
	retiredLocally := retiredData != nil && retiredData.deleted.Load()
	pair.server.lock.Unlock()
	require.True(t, retiredLocally, "An offline delete must immediately stop new stream creation on the server")
	const queuedCount = 4
	var pending []net.Conn
	t.Cleanup(func() {
		for _, connection := range pending {
			_ = connection.Close()
		}
	})
	for i := range queuedCount {
		var dialer net.Dialer
		connection, dialErr := dialer.DialContext(ctx, "tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort()))
		require.NoError(t, dialErr)
		pending = append(pending, connection)
		deadline, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline)
		require.NoError(t, connection.SetDeadline(deadline))
		_, writeErr := fmt.Fprintf(connection, "queued request %d\n", i)
		require.NoError(t, writeErr)
	}
	queuedErr := wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		pair.client.lock.Lock()
		defer pair.client.lock.Unlock()
		data := pair.client.tunnels[TunnelID(spec.GetTunnelRef().GetTunnelId())]
		pendingCount := 0
		if data != nil {
			for _, stream := range data.streams {
				if stream.proxyConn == nil {
					pendingCount++
				}
			}
		}
		return pendingCount == queuedCount, nil
	})
	require.NoError(t, queuedErr)
	added := make(chan *proto.TunnelSpec, 1)
	addErrs := make(chan error, 1)
	go func() {
		newSpec, newErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
			ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
			ServerPort:    stdproto.Int32(int32(echoPort)),
		})
		added <- newSpec
		addErrs <- newErr
	}()
	transport.lock.Lock()
	transport.blocked = false
	transport.lock.Unlock()
	for i, connection := range pending {
		reply, readErr := bufio.NewReader(connection).ReadString('\n')
		require.NoError(t, readErr)
		require.Equal(t, fmt.Sprintf("queued request %d\n", i), reply)
	}
	var newSpec *proto.TunnelSpec
	select {
	case newSpec = <-added:
	case <-ctx.Done():
		t.Fatal("A tunnel added during reconnect did not complete")
	}
	require.NoError(t, <-addErrs)
	require.NotEqual(t, spec.GetTunnelRef().GetTunnelId(), newSpec.GetTunnelRef().GetTunnelId())
	_, retireErr := pair.control.DeleteTunnel(ctx, retired.GetTunnelRef())
	require.NoError(t, retireErr, "Deletion must be retryable after reconnect")
	_, deleteErr := pair.control.DeleteTunnel(ctx, spec.GetTunnelRef())
	require.NoError(t, deleteErr)
	_, writeErr := io.WriteString(pending[0], "established stream after deletion\n")
	require.NoError(t, writeErr)
	reply, readErr := bufio.NewReader(pending[0]).ReadString('\n')
	require.NoError(t, readErr)
	require.Equal(t, "established stream after deletion\n", reply)
	require.NoError(t, doRoundTrip(ctx, networking.IPv4LocalhostDefaultAddress, newSpec.GetClientProxyPort(), 10*time.Second, func() []byte {
		return []byte("new tunnel after reconnect\n")
	}))
}

type proxyPairConfig struct {
	server                   ServerProxyConfig
	controlDialer            func(context.Context, string) (net.Conn, error)
	controlStreamInterceptor grpc.StreamClientInterceptor
	controlServerInterceptor grpc.StreamServerInterceptor
}

type proxyPair struct {
	control proto.TunnelControlClient
	server  *ServerProxy
	client  *ClientProxy
	cleanup context.CancelFunc
}

type controlledTunnelTransport struct {
	lock       sync.Mutex
	connection net.Conn
	blocked    bool
	dialCount  int
}

func (transport *controlledTunnelTransport) dial(ctx context.Context, address string) (net.Conn, error) {
	transport.lock.Lock()
	blocked := transport.blocked
	transport.lock.Unlock()
	if blocked {
		return nil, errors.New("tunnel control transport is disabled by the test")
	}
	var dialer net.Dialer
	connection, dialErr := dialer.DialContext(ctx, "tcp", address)
	if dialErr != nil {
		return nil, dialErr
	}
	transport.lock.Lock()
	defer transport.lock.Unlock()
	if transport.blocked {
		_ = connection.Close()
		return nil, errors.New("tunnel control transport was disabled while dialing")
	}
	transport.connection = connection
	transport.dialCount++
	return connection, nil
}

func (transport *controlledTunnelTransport) interrupt(blockReconnect bool) error {
	transport.lock.Lock()
	transport.blocked = blockReconnect
	connection := transport.connection
	transport.lock.Unlock()
	if connection == nil {
		return errors.New("no tunnel control transport has connected")
	}
	return connection.Close()
}
