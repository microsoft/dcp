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
	"google.golang.org/grpc/credentials/insecure"
	stdproto "google.golang.org/protobuf/proto"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/dcptun/proto"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
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

	serverProxyClient, proxyCleanup := createProxyPair(t, ctx, log)
	defer proxyCleanup()

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

	serverProxyClient, proxyCleanup := createProxyPair(t, testCtx, log)
	defer proxyCleanup()

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

	serverPort, portErr := networking.GetFreePort(apiv1.TCP, networking.IPv4LocalhostDefaultAddress, log)
	require.NoError(t, portErr, "Failed to get free port for server")

	serverProxyClient, proxyCleanup := createProxyPair(t, testCtx, log)
	defer proxyCleanup()

	tunnelReq := &proto.TunnelReq{
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(serverPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ClientProxyPort:    stdproto.Int32(autoAllocatePort),
	}
	tunnelSpec, prepareTunnelErr := serverProxyClient.PrepareTunnel(testCtx, tunnelReq)
	require.NoError(t, prepareTunnelErr)

	rootDir, findRootErr := testutil.FindRootFor(testutil.DirTarget, "test")
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

func createProxyPair(t *testing.T, ctx context.Context, log logr.Logger) (proto.TunnelControlClient, context.CancelFunc) {
	clientControlListener, clientControlPort := setupListener(t, autoAllocatePort)

	clientDataListener, clientDataPort := setupListener(t, autoAllocatePort)

	clientProxyCtx, clientProxyCtxCancel := context.WithCancel(ctx)
	clientLog := log.WithName("ClientProxy")
	clientProxy := NewClientProxy(clientProxyCtx, clientDataListener, clientProxyCtxCancel, clientLog)

	clientGrpcServer := grpc.NewServer()
	proto.RegisterTunnelControlServer(clientGrpcServer, clientProxy)

	go func() {
		clientServerErr := clientGrpcServer.Serve(clientControlListener)
		require.NoError(t, clientServerErr, "Client proxy gRPC server failed")
	}()

	serverControlListener, serverControlPort := setupListener(t, autoAllocatePort)

	clientProxyConn, clientProxyErr := grpc.NewClient(
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, int32(clientControlPort)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
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
		serverLog,
	)
	require.NotNil(t, serverProxy)

	serverGrpcServer := grpc.NewServer()
	proto.RegisterTunnelControlServer(serverGrpcServer, serverProxy)

	go func() {
		serverErr := serverGrpcServer.Serve(serverControlListener)
		require.NoError(t, serverErr, "Server proxy gRPC server failed")
	}()

	// Step 4: Create gRPC client to talk to server-side proxy
	serverProxyConn, serverProxyErr := grpc.NewClient(
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, int32(serverControlPort)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, serverProxyErr)

	serverProxyClient := proto.NewTunnelControlClient(serverProxyConn)

	const gracefulShutdownTimeout = 5 * time.Second

	cleanup := func() {
		if serverProxyConn != nil {
			_ = serverProxyConn.Close()
		}
		if serverGrpcServer != nil {
			serverProxyCtxCancel()
			select {
			case <-serverProxy.Done():
				// Stopped gracefully
			case <-time.After(gracefulShutdownTimeout):
				serverGrpcServer.Stop()
			}
		}
		if clientProxyConn != nil {
			_ = clientProxyConn.Close()
		}
		if clientGrpcServer != nil {
			clientProxyCtxCancel()
			select {
			case <-clientProxy.Done():
				// Stopped gracefully
			case <-time.After(gracefulShutdownTimeout):
				clientGrpcServer.Stop()
			}
		}
	}

	return serverProxyClient, cleanup
}
