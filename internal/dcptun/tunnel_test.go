/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/binary"
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
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	stdproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/concurrency"
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
		TunnelName:         stdproto.String("echo-tunnel"),
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

// Verifies that control role configurations isolate per-hop TLS keys and that both client-proxy
// and server-proxy control servers reject missing, wrong-pair, wrong-direction, wrong-certificate,
// and plaintext credentials on unary and streaming RPCs without mutating tunnel state.
func TestTunnelControlAuthenticationRejectsUnauthorizedClients(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	securityConfig, securityConfigErr := NewTunnelProxySecurityConfig()
	require.NoError(t, securityConfigErr)
	wrongPairConfig, wrongPairConfigErr := NewTunnelProxySecurityConfig()
	require.NoError(t, wrongPairConfigErr)

	clientRoleConfig := securityConfig.ClientProxyConfig()
	serverRoleConfig := securityConfig.ServerProxyConfig()
	controllerRoleConfig := securityConfig.ControllerConfig()

	// Verify role-scoped credential separation across controller, server proxy, and client proxy.
	require.NoError(t, clientRoleConfig.ValidateServer())
	require.Error(t, clientRoleConfig.ValidateClient())
	require.Empty(t, clientRoleConfig.PeerCACertBase64)
	require.Empty(t, clientRoleConfig.OutgoingToken)

	require.NoError(t, controllerRoleConfig.ValidateClient())
	require.Error(t, controllerRoleConfig.ValidateServer())
	require.Empty(t, controllerRoleConfig.ServerCertBase64)
	require.Empty(t, controllerRoleConfig.ServerKeyBase64)
	require.Empty(t, controllerRoleConfig.IncomingTokenHashBase64)

	require.NoError(t, serverRoleConfig.ValidateServer())
	require.NoError(t, serverRoleConfig.ValidateClient())
	require.NotEqual(t, clientRoleConfig.ServerKeyBase64, serverRoleConfig.ServerKeyBase64)
	require.NotEqual(t, clientRoleConfig.ServerCertBase64, serverRoleConfig.ServerCertBase64)
	require.NotEqual(t, controllerRoleConfig.PeerCACertBase64, serverRoleConfig.PeerCACertBase64)

	// Stand up real authenticated client-proxy and server-proxy control servers via NewControlServer.
	clientControlListener, clientControlPort := setupListener(t, autoAllocatePort)
	clientDataListener, clientDataPort := setupListener(t, autoAllocatePort)
	clientCtx, cancelClient := context.WithCancel(ctx)
	clientProxy := NewClientProxy(clientCtx, clientDataListener, cancelClient, ClientProxyConfig{}, testutil.NewLogForTesting(t.Name()+"/client"))
	clientControlServer, clientServerErr := NewControlServer(clientRoleConfig, clientProxy, ClientKeepaliveServerOptions()...)
	require.NoError(t, clientServerErr)
	go func() {
		serveErr := clientControlServer.Serve(clientControlListener)
		if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
			t.Errorf("Client control server failed: %v", serveErr)
		}
	}()

	clientAddress := networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, int32(clientControlPort))
	validServerToClientOpts, validServerToClientOptsErr := serverRoleConfig.ClientDialOptions()
	require.NoError(t, validServerToClientOptsErr)
	validClientConn, validClientConnErr := grpc.NewClient(clientAddress, validServerToClientOpts...)
	require.NoError(t, validClientConnErr)

	serverControlListener, serverControlPort := setupListener(t, autoAllocatePort)
	serverCtx, cancelServer := context.WithCancel(ctx)
	serverProxy := NewServerProxy(
		serverCtx,
		proto.NewTunnelControlClient(validClientConn),
		networking.IPv4LocalhostDefaultAddress,
		int32(clientDataPort),
		cancelServer,
		ServerProxyConfig{},
		testutil.NewLogForTesting(t.Name()+"/server"),
	)
	serverControlServer, serverControlServerErr := NewControlServer(serverRoleConfig, serverProxy)
	require.NoError(t, serverControlServerErr)
	go func() {
		serveErr := serverControlServer.Serve(serverControlListener)
		if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
			t.Errorf("Server control server failed: %v", serveErr)
		}
	}()
	defer func() {
		cancelServer()
		cancelClient()
		_ = validClientConn.Close()
		serverControlServer.Stop()
		clientControlServer.Stop()
		_ = serverControlListener.Close()
		_ = clientControlListener.Close()
		_ = clientDataListener.Close()
	}()

	serverAddress := networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, int32(serverControlPort))
	validControllerOpts, validControllerOptsErr := controllerRoleConfig.ClientDialOptions()
	require.NoError(t, validControllerOptsErr)
	validServerConn, validServerConnErr := grpc.NewClient(serverAddress, validControllerOpts...)
	require.NoError(t, validServerConnErr)
	defer validServerConn.Close()

	// Seed one valid tunnel through the authenticated controller -> server-proxy -> client-proxy chain.
	seededSpec, seedErr := proto.NewTunnelControlClient(validServerConn).PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:         stdproto.String("seeded"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(31001),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	})
	require.NoError(t, seedErr)
	seededTunnelID := TunnelID(seededSpec.GetTunnelRef().GetTunnelId())

	clientPeerPool, clientPeerPoolErr := serverRoleConfig.GetClientPool()
	require.NoError(t, clientPeerPoolErr)
	serverPeerPool, serverPeerPoolErr := controllerRoleConfig.GetClientPool()
	require.NoError(t, serverPeerPoolErr)

	type endpointCase struct {
		name               string
		address            string
		validPeerPool      *x509.CertPool
		wrongLegDialConfig TunnelControlSecurityConfig
		wrongPairToken     string
		oppositeRoleToken  string
	}
	endpoints := []endpointCase{
		{
			name:          "client-proxy",
			address:       clientAddress,
			validPeerPool: clientPeerPool,
			// ControllerConfig trusts serverTLS CA, so dialing client-proxy (which presents clientTLS) must fail TLS verification.
			wrongLegDialConfig: controllerRoleConfig,
			wrongPairToken:     wrongPairConfig.serverToken,
			oppositeRoleToken:  securityConfig.controllerToken,
		},
		{
			name:          "server-proxy",
			address:       serverAddress,
			validPeerPool: serverPeerPool,
			// ServerProxyConfig trusts clientTLS CA, so dialing server-proxy (which presents serverTLS) must fail TLS verification.
			wrongLegDialConfig: serverRoleConfig,
			wrongPairToken:     wrongPairConfig.controllerToken,
			oppositeRoleToken:  securityConfig.serverToken,
		},
	}

	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			makeTokenDialOptions := func(token string) []grpc.DialOption {
				return []grpc.DialOption{
					grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(ep.validPeerPool, "")),
					grpc.WithPerRPCCredentials(controlTokenCredentials{
						authorization: "Bearer " + token,
					}),
				}
			}

			wrongLegOpts, wrongLegOptsErr := ep.wrongLegDialConfig.ClientDialOptions()
			require.NoError(t, wrongLegOptsErr)

			credentialCases := []struct {
				name            string
				dialOptions     []grpc.DialOption
				expectTransport bool
			}{
				{
					name: "missing-token",
					dialOptions: []grpc.DialOption{
						grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(ep.validPeerPool, "")),
					},
				},
				{
					name:        "wrong-pair-token",
					dialOptions: makeTokenDialOptions(ep.wrongPairToken),
				},
				{
					name:        "opposite-direction-token",
					dialOptions: makeTokenDialOptions(ep.oppositeRoleToken),
				},
				{
					name:            "wrong-leg-tls-cert",
					dialOptions:     wrongLegOpts,
					expectTransport: true,
				},
				{
					name: "plaintext",
					dialOptions: []grpc.DialOption{
						grpc.WithTransportCredentials(insecure.NewCredentials()),
					},
					expectTransport: true,
				},
			}

			for _, credCase := range credentialCases {
				t.Run(credCase.name, func(t *testing.T) {
					conn, connErr := grpc.NewClient(ep.address, credCase.dialOptions...)
					require.NoError(t, connErr)
					defer conn.Close()
					client := proto.NewTunnelControlClient(conn)

					callCtx, callCancel := context.WithTimeout(ctx, 3*time.Second)
					defer callCancel()

					_, prepareErr := client.PrepareTunnel(callCtx, &proto.TunnelReq{
						TunnelName:         stdproto.String("unauthorized"),
						ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
						ServerPort:         stdproto.Int32(31099),
						ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
					})
					require.Error(t, prepareErr)

					_, updateErr := client.UpdateTunnel(callCtx, &proto.TunnelUpdate{
						TunnelRef:        seededSpec.GetTunnelRef(),
						ServerAddress:    stdproto.String(networking.IPv4LocalhostDefaultAddress),
						ServerPort:       stdproto.Int32(31002),
						AcceptNewStreams: stdproto.Bool(true),
					})
					require.Error(t, updateErr)

					streamCtx, streamCancel := context.WithTimeout(ctx, 3*time.Second)
					defer streamCancel()
					stream, openStreamErr := client.NewStreamsConnection(streamCtx)
					streamOutcomeErr := openStreamErr
					if openStreamErr == nil {
						_, recvErr := stream.Recv()
						streamOutcomeErr = recvErr
					}
					require.Error(t, streamOutcomeErr)

					if credCase.expectTransport {
						for _, rpcErr := range []error{prepareErr, updateErr, streamOutcomeErr} {
							code := status.Code(rpcErr)
							require.Contains(t, []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Internal}, code, "unexpected status for transport rejection: %v", rpcErr)
						}
					} else {
						require.Equal(t, codes.Unauthenticated, status.Code(prepareErr))
						require.Equal(t, codes.Unauthenticated, status.Code(updateErr))
						require.Equal(t, codes.Unauthenticated, status.Code(streamOutcomeErr))
					}
				})
			}
		})
	}

	// Confirm unauthorized calls never mutated or created tunnels on either proxy.
	clientProxy.lock.Lock()
	require.Len(t, clientProxy.tunnels, 1)
	currentClientTunnel := clientProxy.tunnels[seededTunnelID]
	require.Equal(t, int32(31001), currentClientTunnel.spec.GetServerPort())
	require.True(t, currentClientTunnel.accepting.Load())
	clientProxy.lock.Unlock()

	serverProxy.lock.Lock()
	require.Len(t, serverProxy.tunnels, 1)
	currentServerTunnel := serverProxy.tunnels[seededTunnelID]
	require.Equal(t, int32(31001), currentServerTunnel.spec.GetServerPort())
	require.True(t, currentServerTunnel.accepting.Load())
	serverProxy.lock.Unlock()
}

// Verifies that deleting a tunnel releases its listener for immediate rebinding
// while preserving data streams that were already established.
func TestTunnelDeletionReleasesListenerAndPreservesStreams(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{})
	defer pair.cleanup()
	echoListener, echoPort := setupEchoServer(t)
	defer echoListener.Close()

	request := &proto.TunnelReq{
		TunnelName:         stdproto.String("deletable-tunnel"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(echoPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	}
	original, prepareErr := pair.control.PrepareTunnel(ctx, request)
	require.NoError(t, prepareErr)

	var dialer net.Dialer
	existingConnection, dialErr := dialer.DialContext(
		ctx,
		"tcp",
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, original.GetClientProxyPort()),
	)
	require.NoError(t, dialErr)
	defer existingConnection.Close()
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.NoError(t, existingConnection.SetDeadline(deadline))
	existingReader := bufio.NewReader(existingConnection)
	roundTripExisting := func(message string) {
		_, writeErr := io.WriteString(existingConnection, message)
		require.NoError(t, writeErr)
		reply, readErr := existingReader.ReadString('\n')
		require.NoError(t, readErr)
		require.Equal(t, message, reply)
	}
	roundTripExisting("before deletion\n")

	_, deleteErr := pair.control.DeleteTunnel(ctx, original.GetTunnelRef())
	require.NoError(t, deleteErr)
	roundTripExisting("after deletion\n")

	replacement, replacementErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:         stdproto.String("replacement-tunnel"),
		ServerAddress:      request.ServerAddress,
		ServerPort:         request.ServerPort,
		ClientProxyAddress: request.ClientProxyAddress,
		ClientProxyPort:    stdproto.Int32(original.GetClientProxyPort()),
	})
	require.NoError(t, replacementErr)
	require.NotEqual(t, original.GetTunnelRef().GetTunnelId(), replacement.GetTunnelRef().GetTunnelId())
	require.Equal(t, original.GetClientProxyPort(), replacement.GetClientProxyPort())
	require.NoError(t, doRoundTrip(
		ctx,
		networking.IPv4LocalhostDefaultAddress,
		replacement.GetClientProxyPort(),
		10*time.Second,
		func() []byte { return []byte("replacement stream\n") },
	))
	roundTripExisting("old stream remains active\n")
}

// Verifies that UpdateTunnel keeps the published listener and existing streams
// while suspended, rejects new connections, and resumes against a changed backend.
func TestTunnelUpdateSuspendsAndResumesListener(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{})
	defer pair.cleanup()
	firstBackend, firstBackendPort := setupEchoServer(t)
	defer firstBackend.Close()
	secondBackend, secondBackendPort := setupEchoServer(t)
	defer secondBackend.Close()

	request := &proto.TunnelReq{
		TunnelName:         stdproto.String("updatable-tunnel"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(firstBackendPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	}
	original, prepareErr := pair.control.PrepareTunnel(ctx, request)
	require.NoError(t, prepareErr)

	var dialer net.Dialer
	existingConnection, dialErr := dialer.DialContext(
		ctx,
		"tcp",
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, original.GetClientProxyPort()),
	)
	require.NoError(t, dialErr)
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.NoError(t, existingConnection.SetDeadline(deadline))
	existingReader := bufio.NewReader(existingConnection)
	roundTripExisting := func(message string) {
		_, writeErr := io.WriteString(existingConnection, message)
		require.NoError(t, writeErr)
		reply, readErr := existingReader.ReadString('\n')
		require.NoError(t, readErr)
		require.Equal(t, message, reply)
	}
	roundTripExisting("before suspension\n")

	_, suspendErr := pair.control.UpdateTunnel(ctx, &proto.TunnelUpdate{
		TunnelRef:        original.GetTunnelRef(),
		AcceptNewStreams: stdproto.Bool(false),
	})
	require.NoError(t, suspendErr)
	roundTripExisting("existing stream while suspended\n")

	rejectedCtx, cancelRejected := context.WithTimeout(ctx, 3*time.Second)
	defer cancelRejected()
	rejectedErr := doRoundTrip(
		rejectedCtx,
		networking.IPv4LocalhostDefaultAddress,
		original.GetClientProxyPort(),
		time.Second,
		func() []byte { return []byte("rejected while suspended\n") },
	)
	require.Error(t, rejectedErr)

	require.NoError(t, existingConnection.Close())
	require.NoError(t, firstBackend.Close())
	_, resumeErr := pair.control.UpdateTunnel(ctx, &proto.TunnelUpdate{
		TunnelRef:        original.GetTunnelRef(),
		ServerAddress:    stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:       stdproto.Int32(int32(secondBackendPort)),
		AcceptNewStreams: stdproto.Bool(true),
	})
	require.NoError(t, resumeErr)

	resumedRequest := stdproto.CloneOf(request)
	resumedRequest.ServerPort = stdproto.Int32(int32(secondBackendPort))
	resumed, observeErr := pair.control.PrepareTunnel(ctx, resumedRequest)
	require.NoError(t, observeErr)
	require.Equal(t, original.GetTunnelRef().GetTunnelId(), resumed.GetTunnelRef().GetTunnelId())
	require.Equal(t, original.GetClientProxyPort(), resumed.GetClientProxyPort())
	require.Equal(t, int32(secondBackendPort), resumed.GetServerPort())
	require.NoError(t, doRoundTrip(
		ctx,
		networking.IPv4LocalhostDefaultAddress,
		resumed.GetClientProxyPort(),
		10*time.Second,
		func() []byte { return []byte("resumed against replacement backend\n") },
	))
}

// Verifies that an ambiguous resume failure leaves the server side suspended,
// preventing traffic until an idempotent retry updates both proxies.
func TestTunnelUpdateResumeFailureRemainsSuspended(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	var loseResumeResponse atomic.Bool
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		controlUnaryServerInterceptor: func(
			callCtx context.Context,
			request any,
			info *grpc.UnaryServerInfo,
			handler grpc.UnaryHandler,
		) (any, error) {
			response, handlerErr := handler(callCtx, request)
			update, isUpdate := request.(*proto.TunnelUpdate)
			if handlerErr == nil &&
				isUpdate &&
				info.FullMethod == proto.TunnelControl_UpdateTunnel_FullMethodName &&
				update.GetAcceptNewStreams() &&
				loseResumeResponse.CompareAndSwap(true, false) {
				return nil, status.Error(codes.Unavailable, "resume response was lost")
			}
			return response, handlerErr
		},
	})
	defer pair.cleanup()
	echoListener, echoPort := setupEchoServer(t)
	defer echoListener.Close()
	tunnelSpec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:         stdproto.String("ambiguous-resume"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(echoPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	})
	require.NoError(t, prepareErr)
	_, suspendErr := pair.control.UpdateTunnel(ctx, &proto.TunnelUpdate{
		TunnelRef:        tunnelSpec.GetTunnelRef(),
		AcceptNewStreams: stdproto.Bool(false),
	})
	require.NoError(t, suspendErr)

	loseResumeResponse.Store(true)
	resume := &proto.TunnelUpdate{
		TunnelRef:        tunnelSpec.GetTunnelRef(),
		ServerAddress:    stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:       stdproto.Int32(int32(echoPort)),
		AcceptNewStreams: stdproto.Bool(true),
	}
	_, firstResumeErr := pair.control.UpdateTunnel(ctx, resume)
	require.Error(t, firstResumeErr)
	pair.server.lock.Lock()
	serverTunnel := pair.server.tunnels[TunnelID(tunnelSpec.GetTunnelRef().GetTunnelId())]
	serverAccepting := serverTunnel != nil && serverTunnel.accepting.Load()
	pair.server.lock.Unlock()
	require.False(t, serverAccepting)

	rejectedErr := doRoundTrip(
		ctx,
		networking.IPv4LocalhostDefaultAddress,
		tunnelSpec.GetClientProxyPort(),
		3*time.Second,
		func() []byte { return []byte("rejected after ambiguous resume\n") },
	)
	require.Error(t, rejectedErr)

	_, retryResumeErr := pair.control.UpdateTunnel(ctx, resume)
	require.NoError(t, retryResumeErr)
	require.NoError(t, doRoundTrip(
		ctx,
		networking.IPv4LocalhostDefaultAddress,
		tunnelSpec.GetClientProxyPort(),
		10*time.Second,
		func() []byte { return []byte("accepted after resume retry\n") },
	))
}

// Verifies that ServerProxy serializes tunnel updates through rollback so an
// older timed-out resume cannot disable a later successful retry.
func TestTunnelUpdateSerializesRollbackWithRetry(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	firstResumeStarted := make(chan struct{})
	releaseFirstResume := make(chan struct{})
	secondResumeStarted := make(chan struct{})
	client := &serializedUpdateTunnelClient{
		firstResumeStarted:  firstResumeStarted,
		releaseFirstResume:  releaseFirstResume,
		secondResumeStarted: secondResumeStarted,
	}
	tunnelSpec := &proto.TunnelSpec{
		TunnelRef:     &proto.TunnelRef{TunnelId: stdproto.Uint32(1)},
		ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:    stdproto.Int32(31001),
	}
	tunnel := newTunnelData[stopStreamFn](tunnelSpec)
	server := &ServerProxy{
		log:         logr.Discard(),
		clientProxy: client,
		tunnels: map[TunnelID]*tunnelData[stopStreamFn]{
			1: tunnel,
		},
		lock:    &sync.Mutex{},
		dispose: concurrency.NewOneTimeJob[struct{}](),
	}

	update := &proto.TunnelUpdate{
		TunnelRef:        tunnelSpec.GetTunnelRef(),
		ServerAddress:    stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:       stdproto.Int32(31002),
		AcceptNewStreams: stdproto.Bool(true),
	}
	firstResult := make(chan error, 1)
	go func() {
		_, updateErr := server.UpdateTunnel(ctx, update)
		firstResult <- updateErr
	}()
	select {
	case <-firstResumeStarted:
	case <-ctx.Done():
		t.Fatal("First resume did not reach the client")
	}

	secondWaitingOnLock := make(chan struct{})
	var secondWaitOnce sync.Once
	secondCtx := &doneObserverContext{
		Context: ctx,
		onDone: func() {
			secondWaitOnce.Do(func() { close(secondWaitingOnLock) })
		},
	}
	secondResult := make(chan error, 1)
	go func() {
		_, updateErr := server.UpdateTunnel(secondCtx, update)
		secondResult <- updateErr
	}()
	select {
	case <-secondWaitingOnLock:
	case <-ctx.Done():
		t.Fatal("Second UpdateTunnel call did not block on the per-tunnel operation lock")
	}
	select {
	case <-secondResumeStarted:
		t.Fatal("Retry reached the client before the first update completed rollback")
	default:
	}
	require.Equal(t, uint32(1), client.resumeCalls.Load())

	close(releaseFirstResume)
	require.Equal(t, codes.Internal, status.Code(<-firstResult))
	require.NoError(t, <-secondResult)
	select {
	case <-secondResumeStarted:
	case <-ctx.Done():
		t.Fatal("Retry did not reach the client")
	}

	require.True(t, tunnel.accepting.Load())
	require.Equal(t, int32(31002), tunnel.spec.GetServerPort())
}

// Verifies that a connection accepted before suspension remains owned while its
// success response is blocked, rather than becoming a half-open parked stream.
func TestTunnelUpdatePreservesStreamRegisteredBeforeSuspension(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	resultBlocked := make(chan struct{})
	releaseResult := make(chan struct{})
	var blockFirst sync.Once
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		controlStreamInterceptor: func(callCtx context.Context, desc *grpc.StreamDesc, connection *grpc.ClientConn, method string, streamer grpc.Streamer, options ...grpc.CallOption) (grpc.ClientStream, error) {
			stream, streamErr := streamer(callCtx, desc, connection, method, options...)
			if streamErr != nil {
				return nil, streamErr
			}
			return &gatedTunnelResult{
				ClientStream: stream,
				blockFirst:   &blockFirst,
				blocked:      resultBlocked,
				release:      releaseResult,
			}, nil
		},
	})
	defer pair.cleanup()
	echoListener, echoPort := setupEchoServer(t)
	defer echoListener.Close()
	tunnelSpec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:         stdproto.String("gated-stream"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(echoPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	})
	require.NoError(t, prepareErr)

	var dialer net.Dialer
	connection, dialErr := dialer.DialContext(
		ctx,
		"tcp",
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, tunnelSpec.GetClientProxyPort()),
	)
	require.NoError(t, dialErr)
	defer connection.Close()
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.NoError(t, connection.SetDeadline(deadline))
	_, writeErr := io.WriteString(connection, "accepted before suspension\n")
	require.NoError(t, writeErr)
	select {
	case <-resultBlocked:
	case <-ctx.Done():
		t.Fatal("Tunnel stream success did not reach the gated control send")
	}

	_, suspendErr := pair.control.UpdateTunnel(ctx, &proto.TunnelUpdate{
		TunnelRef:        tunnelSpec.GetTunnelRef(),
		AcceptNewStreams: stdproto.Bool(false),
	})
	require.NoError(t, suspendErr)
	close(releaseResult)
	reply, readErr := bufio.NewReader(connection).ReadString('\n')
	require.NoError(t, readErr)
	require.Equal(t, "accepted before suspension\n", reply)
}

// Verifies that a stream connection created for an old backend cannot register
// after the tunnel has resumed with a replacement backend.
func TestTunnelStreamRegistrationRejectsSupersededBackend(t *testing.T) {
	t.Parallel()

	oldSpec := &proto.TunnelSpec{
		TunnelRef:     &proto.TunnelRef{TunnelId: stdproto.Uint32(1)},
		ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:    stdproto.Int32(31001),
	}
	tunnel := newTunnelData[stopStreamFn](oldSpec)
	serverProxy := &ServerProxy{
		tunnels: map[TunnelID]*tunnelData[stopStreamFn]{
			1: tunnel,
		},
		lock: &sync.Mutex{},
	}
	replacementSpec := stdproto.CloneOf(oldSpec)
	replacementSpec.ServerPort = stdproto.Int32(31002)
	tunnel.spec = replacementSpec

	_, registrationErr := serverProxy.registerTunnelStream(1, 1, oldSpec, func() {})
	require.Error(t, registrationErr)
	require.Contains(t, registrationErr.Error(), "changed its server endpoint")
	require.Empty(t, tunnel.streams)
}

// Verifies that retrying DeleteTunnel, by tunnel ID or by tunnel name, reaches the client proxy after
// the server-side tunnel entry drains, releasing the retained listener, and that retiring the drained
// entry also removes its tunnel name mapping.
func TestTunnelDeletionForwardsAfterServerEntryDrains(t *testing.T) {
	t.Parallel()
	for _, deleteByName := range []bool{false, true} {
		testName := "delete-by-id"
		if deleteByName {
			testName = "delete-by-name"
		}
		t.Run(testName, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
			defer cancel()
			var blockClientDeletion atomic.Bool
			blockClientDeletion.Store(true)
			pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
				controlUnaryServerInterceptor: func(
					callCtx context.Context,
					request any,
					info *grpc.UnaryServerInfo,
					handler grpc.UnaryHandler,
				) (any, error) {
					if info.FullMethod == proto.TunnelControl_DeleteTunnel_FullMethodName && blockClientDeletion.Load() {
						return nil, status.Error(codes.Unavailable, "client deletion is temporarily blocked")
					}
					return handler(callCtx, request)
				},
			})
			defer pair.cleanup()
			echoListener, echoPort := setupEchoServer(t)
			defer echoListener.Close()

			request := &proto.TunnelReq{
				TunnelName:         stdproto.String("draining-delete"),
				ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
				ServerPort:         stdproto.Int32(int32(echoPort)),
				ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
			}
			tunnelSpec, prepareErr := pair.control.PrepareTunnel(ctx, request)
			require.NoError(t, prepareErr)
			tunnelRef := tunnelSpec.GetTunnelRef()
			if deleteByName {
				tunnelRef = &proto.TunnelRef{TunnelName: request.TunnelName}
			}

			var dialer net.Dialer
			connection, dialErr := dialer.DialContext(
				ctx,
				"tcp",
				networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, tunnelSpec.GetClientProxyPort()),
			)
			require.NoError(t, dialErr)
			require.NoError(t, connection.SetDeadline(time.Now().Add(10*time.Second)))
			_, writeErr := io.WriteString(connection, "establish stream\n")
			require.NoError(t, writeErr)
			reply, readErr := bufio.NewReader(connection).ReadString('\n')
			require.NoError(t, readErr)
			require.Equal(t, "establish stream\n", reply)

			_, firstDeleteErr := pair.control.DeleteTunnel(ctx, tunnelRef)
			require.Error(t, firstDeleteErr)
			require.NoError(t, connection.Close())
			tunnelID := TunnelID(tunnelSpec.GetTunnelRef().GetTunnelId())
			drainErr := wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
				pair.server.lock.Lock()
				_, found := pair.server.tunnels[tunnelID]
				pair.server.lock.Unlock()
				return !found, nil
			})
			require.NoError(t, drainErr)
			pair.server.lock.Lock()
			_, nameMapped := pair.server.tunnelsByName[request.GetTunnelName()]
			pair.server.lock.Unlock()
			require.False(t, nameMapped, "Retiring a drained tunnel must also remove its name mapping")

			blockClientDeletion.Store(false)
			_, retryDeleteErr := pair.control.DeleteTunnel(ctx, tunnelRef)
			require.NoError(t, retryDeleteErr)

			replacement, replacementErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
				TunnelName:         stdproto.String("draining-delete"),
				ServerAddress:      request.ServerAddress,
				ServerPort:         request.ServerPort,
				ClientProxyAddress: request.ClientProxyAddress,
				ClientProxyPort:    stdproto.Int32(tunnelSpec.GetClientProxyPort()),
			})
			require.NoError(t, replacementErr)
			require.Equal(t, tunnelSpec.GetClientProxyPort(), replacement.GetClientProxyPort())
		})
	}
}

// Verifies that random client-listener allocation rejects reserved ports before
// publishing a tunnel and continues with another OS-selected port.
func TestClientProxyRandomPortExcludesReservations(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	dataListener, _ := setupListener(t, autoAllocatePort)
	clientCtx, cancelClient := context.WithCancel(ctx)
	reservedListener, reservedPort := setupListener(t, autoAllocatePort)
	allowedListener, allowedPort := setupListener(t, autoAllocatePort)
	listeners := []net.Listener{reservedListener, allowedListener}
	clientProxy := NewClientProxy(clientCtx, dataListener, cancelClient, ClientProxyConfig{
		Listen: func(context.Context, string, string) (net.Listener, error) {
			require.NotEmpty(t, listeners)
			listener := listeners[0]
			listeners = listeners[1:]
			return listener, nil
		},
	}, testutil.NewLogForTesting(t.Name()))
	defer clientProxy.disposeOnce()

	tunnelSpec, prepareErr := clientProxy.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:               stdproto.String("reserved-port-test"),
		ServerPort:               stdproto.Int32(12345),
		ClientProxyAddress:       stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ExcludedClientProxyPorts: []int32{int32(reservedPort)},
	})
	require.NoError(t, prepareErr)
	require.Equal(t, int32(allowedPort), tunnelSpec.GetClientProxyPort())

	reboundListener, reboundErr := net.Listen(
		"tcp",
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, int32(reservedPort)),
	)
	require.NoError(t, reboundErr, "The rejected reserved listener must be closed before PrepareTunnel returns")
	require.NoError(t, reboundListener.Close())
}

// Verifies that request deduplication never returns a tunnel whose random port is excluded,
// ignores exclusions for requests that ask for an exact port, and remains idempotent for each resulting request.
func TestClientProxyDeduplicationRespectsExcludedPorts(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	dataListener, _ := setupListener(t, autoAllocatePort)
	clientCtx, cancelClient := context.WithCancel(ctx)
	firstListener, firstPort := setupListener(t, autoAllocatePort)
	secondListener, secondPort := setupListener(t, autoAllocatePort)
	exactListener, exactPort := setupListener(t, autoAllocatePort)
	listeners := []net.Listener{firstListener, secondListener, exactListener}
	clientProxy := NewClientProxy(clientCtx, dataListener, cancelClient, ClientProxyConfig{
		Listen: func(context.Context, string, string) (net.Listener, error) {
			require.NotEmpty(t, listeners)
			listener := listeners[0]
			listeners = listeners[1:]
			return listener, nil
		},
	}, testutil.NewLogForTesting(t.Name()))
	defer clientProxy.disposeOnce()

	request := &proto.TunnelReq{
		TunnelName:         stdproto.String("first"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(12345),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	}
	first, firstErr := clientProxy.PrepareTunnel(ctx, request)
	require.NoError(t, firstErr)
	require.Equal(t, int32(firstPort), first.GetClientProxyPort())

	sameNameExcluded := stdproto.CloneOf(request)
	sameNameExcluded.ExcludedClientProxyPorts = []int32{int32(firstPort)}
	_, sameNameExcludedErr := clientProxy.PrepareTunnel(ctx, sameNameExcluded)
	require.Equal(t, codes.AlreadyExists, status.Code(sameNameExcludedErr))

	excludingRequest := stdproto.CloneOf(request)
	excludingRequest.TunnelName = stdproto.String("second")
	excludingRequest.ExcludedClientProxyPorts = []int32{int32(firstPort)}
	second, secondErr := clientProxy.PrepareTunnel(ctx, excludingRequest)
	require.NoError(t, secondErr)
	require.Equal(t, int32(secondPort), second.GetClientProxyPort())
	require.NotEqual(t, first.GetTunnelRef().GetTunnelId(), second.GetTunnelRef().GetTunnelId())

	firstRetry, firstRetryErr := clientProxy.PrepareTunnel(ctx, request)
	require.NoError(t, firstRetryErr)
	require.Equal(t, first.GetTunnelRef().GetTunnelId(), firstRetry.GetTunnelRef().GetTunnelId())
	require.Equal(t, first.GetClientProxyPort(), firstRetry.GetClientProxyPort())

	secondRetry, secondRetryErr := clientProxy.PrepareTunnel(ctx, excludingRequest)
	require.NoError(t, secondRetryErr)
	require.Equal(t, second.GetTunnelRef().GetTunnelId(), secondRetry.GetTunnelRef().GetTunnelId())
	require.Equal(t, second.GetClientProxyPort(), secondRetry.GetClientProxyPort())

	exactRequest := stdproto.CloneOf(request)
	exactRequest.TunnelName = stdproto.String("exact")
	exactRequest.ClientProxyPort = stdproto.Int32(int32(exactPort))
	exactRequest.ExcludedClientProxyPorts = []int32{int32(exactPort)}
	exact, exactErr := clientProxy.PrepareTunnel(ctx, exactRequest)
	require.NoError(t, exactErr)
	require.Equal(t, int32(exactPort), exact.GetClientProxyPort())

	exactRetry, exactRetryErr := clientProxy.PrepareTunnel(ctx, exactRequest)
	require.NoError(t, exactRetryErr, "Exclusions are ignored for an exact port, so an identical retry must return the same tunnel")
	require.Equal(t, exact.GetTunnelRef().GetTunnelId(), exactRetry.GetTunnelRef().GetTunnelId())
}

// Verifies that ClientProxy.PrepareTunnel binds the preferred port of a request that allows port fallback
// when that port is usable, and selects a random port without attempting the preferred port when the request excludes it.
func TestClientProxyPortFallbackUsesPreferredPortUnlessExcluded(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	dataListener, _ := setupListener(t, autoAllocatePort)
	clientCtx, cancelClient := context.WithCancel(ctx)
	preferredListener, preferredPort := setupListener(t, autoAllocatePort)
	randomListener, randomPort := setupListener(t, autoAllocatePort)
	listeners := []net.Listener{preferredListener, randomListener}
	var listenAddresses []string
	clientProxy := NewClientProxy(clientCtx, dataListener, cancelClient, ClientProxyConfig{
		Listen: func(_ context.Context, _ string, address string) (net.Listener, error) {
			require.NotEmpty(t, listeners)
			listenAddresses = append(listenAddresses, address)
			listener := listeners[0]
			listeners = listeners[1:]
			return listener, nil
		},
	}, testutil.NewLogForTesting(t.Name()))
	defer clientProxy.disposeOnce()

	preferredRequest := &proto.TunnelReq{
		TunnelName:                   stdproto.String("preferred"),
		ServerAddress:                stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:                   stdproto.Int32(12345),
		ClientProxyAddress:           stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ClientProxyPort:              stdproto.Int32(int32(preferredPort)),
		AllowClientProxyPortFallback: stdproto.Bool(true),
	}
	preferred, preferredErr := clientProxy.PrepareTunnel(ctx, preferredRequest)
	require.NoError(t, preferredErr)
	require.Equal(t, int32(preferredPort), preferred.GetClientProxyPort())

	excludingRequest := stdproto.CloneOf(preferredRequest)
	excludingRequest.TunnelName = stdproto.String("fallback")
	excludingRequest.ServerPort = stdproto.Int32(12346)
	excludingRequest.ExcludedClientProxyPorts = []int32{int32(preferredPort)}
	fallback, fallbackErr := clientProxy.PrepareTunnel(ctx, excludingRequest)
	require.NoError(t, fallbackErr)
	require.Equal(t, int32(randomPort), fallback.GetClientProxyPort())

	require.Equal(t, []string{
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, int32(preferredPort)),
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, 0),
	}, listenAddresses, "An excluded preferred port must not be attempted")
}

// Verifies that ClientProxy.PrepareTunnel selects a random port, within the same request, when the preferred port
// of a request that allows port fallback cannot be bound, returns the same tunnel when that request is retried,
// and fails a request without port fallback for the same port.
func TestClientProxyPortFallbackSelectsRandomPortWhenPreferredPortIsBusy(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	dataListener, _ := setupListener(t, autoAllocatePort)
	clientCtx, cancelClient := context.WithCancel(ctx)
	clientProxy := NewClientProxy(clientCtx, dataListener, cancelClient, ClientProxyConfig{}, testutil.NewLogForTesting(t.Name()))
	defer clientProxy.disposeOnce()

	busyListener, busyPort := setupListener(t, autoAllocatePort)
	defer func() { _ = busyListener.Close() }()

	exactRequest := &proto.TunnelReq{
		TunnelName:         stdproto.String("busy-port-tunnel"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(12345),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ClientProxyPort:    stdproto.Int32(int32(busyPort)),
	}
	_, exactErr := clientProxy.PrepareTunnel(ctx, exactRequest)
	require.Error(t, exactErr, "A request without port fallback must fail when its port cannot be bound")

	fallbackRequest := stdproto.CloneOf(exactRequest)
	fallbackRequest.AllowClientProxyPortFallback = stdproto.Bool(true)
	fallback, fallbackErr := clientProxy.PrepareTunnel(ctx, fallbackRequest)
	require.NoError(t, fallbackErr)
	require.NotEqual(t, int32(busyPort), fallback.GetClientProxyPort())

	retried, retryErr := clientProxy.PrepareTunnel(ctx, stdproto.CloneOf(fallbackRequest))
	require.NoError(t, retryErr)
	require.Equal(t, fallback.GetTunnelRef().GetTunnelId(), retried.GetTunnelRef().GetTunnelId())
	require.Equal(t, fallback.GetClientProxyPort(), retried.GetClientProxyPort())
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
		TunnelName:         stdproto.String("conn-throughput"),
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

	serverPort, portErr := networking.GetFreePort(testCtx, commonapi.PortProtocolTCP, networking.IPv4LocalhostDefaultAddress, log)
	require.NoError(t, portErr, "Failed to get free port for server")

	pair := createProxyPair(t, testCtx, log, proxyPairConfig{})
	defer pair.cleanup()
	serverProxyClient := pair.control

	tunnelReq := &proto.TunnelReq{
		TunnelName:         stdproto.String("data-throughput"),
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

	// Missing tunnel_name must be rejected.
	_, missingNameErr := serverProxyClient.PrepareTunnel(ctx, &proto.TunnelReq{
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(testServerPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ClientProxyPort:    stdproto.Int32(8080),
	})
	require.Equal(t, codes.InvalidArgument, status.Code(missingNameErr))

	// Define the same tunnel request parameters
	tunnelReq := &proto.TunnelReq{
		TunnelName:         stdproto.String("idempotent-primary"),
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

	// Reusing an active tunnel name with conflicting parameters must fail.
	conflictingReq := stdproto.CloneOf(tunnelReq)
	conflictingReq.ClientProxyPort = stdproto.Int32(8081)
	_, conflictErr := serverProxyClient.PrepareTunnel(ctx, conflictingReq)
	require.Error(t, conflictErr)

	// Test that a tunnel with a different name and parameters gets a different TunnelID
	differentTunnelReq := &proto.TunnelReq{
		TunnelName:         stdproto.String("idempotent-secondary"),
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
			if connErr != nil {
				t.Errorf("Echo server: failed to accept connection: %v", connErr)
				return
			}

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

type serializedUpdateTunnelClient struct {
	proto.TunnelControlClient
	resumeCalls         atomic.Uint32
	firstResumeStarted  chan struct{}
	releaseFirstResume  chan struct{}
	secondResumeStarted chan struct{}
}

func (c *serializedUpdateTunnelClient) UpdateTunnel(
	_ context.Context,
	update *proto.TunnelUpdate,
	_ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	if !update.GetAcceptNewStreams() {
		return &emptypb.Empty{}, nil
	}

	switch c.resumeCalls.Add(1) {
	case 1:
		close(c.firstResumeStarted)
		<-c.releaseFirstResume
		return nil, status.Error(codes.Unavailable, "first resume response was lost")
	case 2:
		close(c.secondResumeStarted)
	}
	return &emptypb.Empty{}, nil
}

func createProxyPair(t *testing.T, ctx context.Context, log logr.Logger, config proxyPairConfig) proxyPair {
	securityConfig, securityConfigErr := NewTunnelProxySecurityConfig()
	require.NoError(t, securityConfigErr, "Failed to create tunnel proxy security configuration")
	clientRoleConfig := securityConfig.ClientProxyConfig()
	serverRoleConfig := securityConfig.ServerProxyConfig()
	serverDialOptions, serverDialOptionsErr := serverRoleConfig.ClientDialOptions()
	require.NoError(t, serverDialOptionsErr, "Failed to create server proxy gRPC client options")
	controllerDialOptions, controllerDialOptionsErr := securityConfig.ControllerConfig().ClientDialOptions()
	require.NoError(t, controllerDialOptionsErr, "Failed to create controller gRPC client options")

	clientControlListener, clientControlPort := setupListener(t, autoAllocatePort)

	clientDataListener, clientDataPort := setupListener(t, autoAllocatePort)

	clientProxyCtx, clientProxyCtxCancel := context.WithCancel(ctx)
	clientLog := log.WithName("ClientProxy")
	clientProxy := NewClientProxy(clientProxyCtx, clientDataListener, clientProxyCtxCancel, config.client, clientLog)

	var clientExtraOptions []grpc.ServerOption
	if config.controlServerInterceptor != nil {
		clientExtraOptions = append(clientExtraOptions, grpc.ChainStreamInterceptor(config.controlServerInterceptor))
	}
	if config.controlUnaryServerInterceptor != nil {
		clientExtraOptions = append(clientExtraOptions, grpc.ChainUnaryInterceptor(config.controlUnaryServerInterceptor))
	}
	if config.controlServerEnforcement == nil && config.controlServerKeepalive == nil {
		clientExtraOptions = append(clientExtraOptions, ClientKeepaliveServerOptions()...)
	} else {
		if config.controlServerEnforcement != nil {
			clientExtraOptions = append(clientExtraOptions, grpc.KeepaliveEnforcementPolicy(*config.controlServerEnforcement))
		}
		if config.controlServerKeepalive != nil {
			clientExtraOptions = append(clientExtraOptions, grpc.KeepaliveParams(*config.controlServerKeepalive))
		}
	}
	clientGrpcServer, clientServerOptionsErr := NewControlServer(clientRoleConfig, clientProxy, clientExtraOptions...)
	require.NoError(t, clientServerOptionsErr, "Failed to create client proxy gRPC server")

	go func() {
		clientServerErr := clientGrpcServer.Serve(clientControlListener)
		if clientServerErr != nil && !errors.Is(clientServerErr, net.ErrClosed) {
			t.Errorf("Client proxy gRPC server failed: %v", clientServerErr)
		}
	}()

	serverControlListener, serverControlPort := setupListener(t, autoAllocatePort)

	dialOptions := serverDialOptions
	if config.controlDialer != nil {
		dialOptions = append(dialOptions, grpc.WithContextDialer(config.controlDialer))
	}
	if config.controlStreamInterceptor != nil {
		dialOptions = append(dialOptions, grpc.WithStreamInterceptor(config.controlStreamInterceptor))
	}
	if config.controlUnaryInterceptor != nil {
		dialOptions = append(dialOptions, grpc.WithUnaryInterceptor(config.controlUnaryInterceptor))
	}
	if config.controlClientKeepalive != nil {
		dialOptions = append(dialOptions, grpc.WithKeepaliveParams(*config.controlClientKeepalive))
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

	serverGrpcServer, serverServerOptionsErr := NewControlServer(serverRoleConfig, serverProxy)
	require.NoError(t, serverServerOptionsErr, "Failed to create server proxy gRPC server")

	go func() {
		serverErr := serverGrpcServer.Serve(serverControlListener)
		if serverErr != nil && !errors.Is(serverErr, net.ErrClosed) {
			t.Errorf("Server proxy gRPC server failed: %v", serverErr)
		}
	}()

	serverProxyConn, serverProxyErr := grpc.NewClient(
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, int32(serverControlPort)),
		controllerDialOptions...,
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

		clientProxyConn: clientProxyConn,
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
		TunnelName:         stdproto.String("reconnect-preserve"),
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
	// A stream that ends before readiness returns no headers, which must not count as readiness.
	if values := headers.Get(controlStreamReadyHeader); len(values) != 1 || values[0] != "true" {
		return headers, nil
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
		TunnelName: stdproto.String("healthy-idle"),
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

// Verifies that gRPC keepalive detects a half-open idle control transport and
// reconnects it without replacing prepared tunnels.
func TestTunnelControlKeepaliveRecoversHalfOpenTransport(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	transport := &halfOpenTunnelTransport{}
	clientKeepalive := keepalive.ClientParameters{
		Time:    10 * time.Second,
		Timeout: time.Second,
	}
	serverEnforcement := keepalive.EnforcementPolicy{MinTime: 10 * time.Second}
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		server:                   ServerProxyConfig{ReconnectTimeout: 20 * time.Second},
		controlDialer:            transport.dial,
		controlClientKeepalive:   &clientKeepalive,
		controlServerEnforcement: &serverEnforcement,
	})
	defer pair.cleanup()
	echoListener, echoPort := setupEchoServer(t)
	defer echoListener.Close()
	tunnelSpec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:         stdproto.String("half-open-keepalive"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(echoPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	})
	require.NoError(t, prepareErr)
	require.NoError(t, doRoundTrip(
		ctx,
		networking.IPv4LocalhostDefaultAddress,
		tunnelSpec.GetClientProxyPort(),
		5*time.Second,
		func() []byte { return []byte("before half-open transport\n") },
	))

	require.NoError(t, transport.blackholeCurrent())
	reconnectedErr := wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		return transport.connectionCount() >= 2, nil
	})
	require.NoError(t, reconnectedErr)
	require.NoError(t, doRoundTrip(
		ctx,
		networking.IPv4LocalhostDefaultAddress,
		tunnelSpec.GetClientProxyPort(),
		10*time.Second,
		func() []byte { return []byte("after keepalive reconnect\n") },
	))
	require.NoError(t, pair.server.Err())
}

// Verifies that client proxy keepalive releases the control stream of a connection that
// the server proxy abandoned without notifying the client proxy, so the replacement control
// stream becomes ready within the reconnect budget and prepared tunnels keep working.
func TestTunnelControlServerKeepaliveReleasesAbandonedConnection(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	transport := &abandonedTunnelTransport{}
	defer transport.closeAll()
	ready := make(chan struct{}, 4)
	serverKeepalive := keepalive.ServerParameters{
		Time:    time.Second,
		Timeout: time.Second,
	}
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		server:        ServerProxyConfig{ReconnectTimeout: 10 * time.Second},
		controlDialer: transport.dial,
		controlStreamInterceptor: func(callCtx context.Context, desc *grpc.StreamDesc, connection *grpc.ClientConn, method string, streamer grpc.Streamer, options ...grpc.CallOption) (grpc.ClientStream, error) {
			stream, streamErr := streamer(callCtx, desc, connection, method, options...)
			if streamErr != nil {
				return nil, streamErr
			}
			return &observedTunnelControlStream{ClientStream: stream, ready: ready}, nil
		},
		controlServerKeepalive: &serverKeepalive,
	})
	defer pair.cleanup()
	waitControlStreamReady := func(stage string) {
		select {
		case <-ready:
		case <-pair.server.Done():
			t.Fatalf("Server proxy stopped before the %s control stream became ready: %v", stage, pair.server.Err())
		case <-ctx.Done():
			t.Fatalf("The %s control stream did not become ready", stage)
		}
	}
	echoListener, echoPort := setupEchoServer(t)
	defer echoListener.Close()
	tunnelSpec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:         stdproto.String("abandoned-keepalive"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(echoPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	})
	require.NoError(t, prepareErr)
	waitControlStreamReady("initial")
	require.NoError(t, doRoundTrip(
		ctx,
		networking.IPv4LocalhostDefaultAddress,
		tunnelSpec.GetClientProxyPort(),
		5*time.Second,
		func() []byte { return []byte("before abandoned connection\n") },
	))

	require.NoError(t, transport.abandonCurrent())
	waitControlStreamReady("replacement")
	require.GreaterOrEqual(t, transport.connectionCount(), 2, "The replacement control stream must use a new connection")
	require.NoError(t, doRoundTrip(
		ctx,
		networking.IPv4LocalhostDefaultAddress,
		tunnelSpec.GetClientProxyPort(),
		10*time.Second,
		func() []byte { return []byte("after abandoned connection\n") },
	))
	require.NoError(t, pair.server.Err())
	select {
	case <-pair.client.Done():
		t.Fatal("Abandoning a control connection disposed the client proxy")
	default:
	}
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
		TunnelName: stdproto.String("budget-exhaustion"),
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

// Verifies that a trailers-only transient failure before readiness is recovered
// using the RPC status returned by Recv rather than failing the proxy pair.
func TestTunnelControlReconnectsAfterHeaderlessTransientFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	var attempts atomic.Uint32
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		server: ServerProxyConfig{ReconnectTimeout: 3 * time.Second},
		controlServerInterceptor: func(server any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if attempts.Add(1) == 1 {
				return status.Error(codes.Unavailable, "transient readiness failure")
			}
			return handler(server, stream)
		},
	})
	defer pair.cleanup()
	echoListener, echoPort := setupEchoServer(t)
	defer echoListener.Close()

	spec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName: stdproto.String("headerless-transient"),
		ServerPort: stdproto.Int32(int32(echoPort)),
	})
	require.NoError(t, prepareErr)
	require.NoError(t, doRoundTrip(
		ctx,
		networking.IPv4LocalhostDefaultAddress,
		spec.GetClientProxyPort(),
		10*time.Second,
		func() []byte { return []byte("reconnected after headerless failure\n") },
	))
	require.GreaterOrEqual(t, attempts.Load(), uint32(2))
	require.NoError(t, pair.server.Err())
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
	spec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName: stdproto.String("abandoned-handshake"),
		ServerPort: stdproto.Int32(int32(backendPort)),
	})
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

type gatedTunnelResult struct {
	grpc.ClientStream
	blockFirst *sync.Once
	blocked    chan<- struct{}
	release    <-chan struct{}
}

func (stream *gatedTunnelResult) SendMsg(message any) error {
	block := false
	stream.blockFirst.Do(func() {
		block = true
		close(stream.blocked)
	})
	if block {
		select {
		case <-stream.release:
		case <-stream.Context().Done():
			return status.FromContextError(stream.Context().Err()).Err()
		}
	}
	return stream.ClientStream.SendMsg(message)
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
			spec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
				TunnelName: stdproto.String("reconnect-shutdown"),
				ServerPort: stdproto.Int32(int32(echoPort)),
			})
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

// Verifies that requests queued during a transport outage resume exactly once, and that tunnels added or
// deleted while disconnected retain their intended lifecycle: a deletion stops new streams on the server
// immediately and completes on the client proxy once the control connection recovers.
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
	spec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName: stdproto.String("queued-active"),
		ServerPort: stdproto.Int32(int32(echoPort)),
	})
	require.NoError(t, prepareErr)
	require.NoError(t, doRoundTrip(ctx, networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort(), 10*time.Second, func() []byte {
		return []byte("before queued requests\n")
	}))
	retired, retirePrepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:         stdproto.String("queued-retired"),
		ServerPort:         stdproto.Int32(int32(echoPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
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
	offlineDeleteResult := make(chan error, 1)
	go func() {
		_, offlineDeleteErr := pair.control.DeleteTunnel(ctx, retired.GetTunnelRef())
		offlineDeleteResult <- offlineDeleteErr
	}()
	retiredLocally := wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		pair.server.lock.Lock()
		defer pair.server.lock.Unlock()
		retiredData := pair.server.tunnels[TunnelID(retired.GetTunnelRef().GetTunnelId())]
		return retiredData != nil && retiredData.deleted.Load(), nil
	})
	require.NoError(t, retiredLocally, "An offline delete must immediately stop new stream creation on the server")
	select {
	case offlineDeleteErr := <-offlineDeleteResult:
		t.Fatalf("Deletion must wait for the unreachable client proxy, but it completed with: %v", offlineDeleteErr)
	default:
	}
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
			TunnelName:    stdproto.String("queued-added"),
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
	select {
	case offlineDeleteErr := <-offlineDeleteResult:
		require.NoError(t, offlineDeleteErr, "A deletion issued while disconnected must complete after reconnect")
	case <-ctx.Done():
		t.Fatal("A deletion issued while disconnected did not complete after reconnect")
	}
	_, retireErr := pair.control.DeleteTunnel(ctx, retired.GetTunnelRef())
	require.NoError(t, retireErr, "Deletion must remain idempotent after reconnect")
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
	client                        ClientProxyConfig
	server                        ServerProxyConfig
	controlDialer                 func(context.Context, string) (net.Conn, error)
	controlStreamInterceptor      grpc.StreamClientInterceptor
	controlUnaryInterceptor       grpc.UnaryClientInterceptor
	controlServerInterceptor      grpc.StreamServerInterceptor
	controlUnaryServerInterceptor grpc.UnaryServerInterceptor
	controlClientKeepalive        *keepalive.ClientParameters
	controlServerKeepalive        *keepalive.ServerParameters
	controlServerEnforcement      *keepalive.EnforcementPolicy
}

type proxyPair struct {
	control proto.TunnelControlClient
	server  *ServerProxy
	client  *ClientProxy
	cleanup context.CancelFunc

	// The server proxy's control connection to the client proxy.
	clientProxyConn *grpc.ClientConn
}

type halfOpenTunnelTransport struct {
	lock        sync.Mutex
	current     *halfOpenConn
	connections int
}

func (transport *halfOpenTunnelTransport) dial(ctx context.Context, address string) (net.Conn, error) {
	var dialer net.Dialer
	connection, dialErr := dialer.DialContext(ctx, "tcp", address)
	if dialErr != nil {
		return nil, dialErr
	}
	wrapped := &halfOpenConn{Conn: connection}
	transport.lock.Lock()
	transport.current = wrapped
	transport.connections++
	transport.lock.Unlock()
	return wrapped, nil
}

func (transport *halfOpenTunnelTransport) blackholeCurrent() error {
	transport.lock.Lock()
	current := transport.current
	transport.lock.Unlock()
	if current == nil {
		return errors.New("no tunnel control transport has connected")
	}
	current.blackhole.Store(true)
	return nil
}

func (transport *halfOpenTunnelTransport) connectionCount() int {
	transport.lock.Lock()
	defer transport.lock.Unlock()
	return transport.connections
}

type halfOpenConn struct {
	net.Conn
	blackhole atomic.Bool
}

func (connection *halfOpenConn) Read(buffer []byte) (int, error) {
	for {
		deadlineErr := connection.Conn.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
		if deadlineErr != nil {
			return 0, deadlineErr
		}
		count, readErr := connection.Conn.Read(buffer)
		if connection.blackhole.Load() && count > 0 {
			continue
		}
		if timeoutErr, isTimeout := readErr.(net.Error); isTimeout && timeoutErr.Timeout() {
			continue
		}
		return count, readErr
	}
}

func (connection *halfOpenConn) Write(buffer []byte) (int, error) {
	if connection.blackhole.Load() {
		return len(buffer), nil
	}
	return connection.Conn.Write(buffer)
}

// abandonedTunnelTransport lets a test abandon the current control connection: the
// server proxy observes it as closed, while the client proxy keeps an open connection
// whose peer never responds.
type abandonedTunnelTransport struct {
	lock        sync.Mutex
	current     *abandonableConn
	connections []*abandonableConn
}

func (transport *abandonedTunnelTransport) dial(ctx context.Context, address string) (net.Conn, error) {
	var dialer net.Dialer
	connection, dialErr := dialer.DialContext(ctx, "tcp", address)
	if dialErr != nil {
		return nil, dialErr
	}
	wrapped := &abandonableConn{Conn: connection}
	transport.lock.Lock()
	transport.current = wrapped
	transport.connections = append(transport.connections, wrapped)
	transport.lock.Unlock()
	return wrapped, nil
}

func (transport *abandonedTunnelTransport) abandonCurrent() error {
	transport.lock.Lock()
	current := transport.current
	transport.lock.Unlock()
	if current == nil {
		return errors.New("no tunnel control transport has connected")
	}
	current.abandoned.Store(true)
	// Unblock the pending read of the server proxy without sending anything to the client proxy.
	return current.Conn.SetReadDeadline(time.Now())
}

func (transport *abandonedTunnelTransport) connectionCount() int {
	transport.lock.Lock()
	defer transport.lock.Unlock()
	return len(transport.connections)
}

func (transport *abandonedTunnelTransport) closeAll() {
	transport.lock.Lock()
	defer transport.lock.Unlock()
	for _, connection := range transport.connections {
		_ = connection.Conn.Close()
	}
}

// abandonableConn appears closed to its owner once abandoned, but keeps the
// underlying socket open, so its peer is never notified.
type abandonableConn struct {
	net.Conn
	abandoned atomic.Bool
}

func (connection *abandonableConn) Read(buffer []byte) (int, error) {
	count, readErr := connection.Conn.Read(buffer)
	if connection.abandoned.Load() {
		return 0, net.ErrClosed
	}
	return count, readErr
}

func (connection *abandonableConn) Write(buffer []byte) (int, error) {
	if connection.abandoned.Load() {
		return 0, net.ErrClosed
	}
	return connection.Conn.Write(buffer)
}

func (connection *abandonableConn) Close() error {
	if connection.abandoned.Load() {
		return nil
	}
	return connection.Conn.Close()
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

// Verifies that ClientProxy serializes PrepareTunnel and name-based DeleteTunnel per tunnel name,
// abandons a preparation whose request was canceled before the tunnel could be registered (closing its
// listener before a retry or deletion proceeds), and closes listeners bound concurrently with proxy disposal.
func TestClientProxyPrepareTunnelRaces(t *testing.T) {
	t.Parallel()

	type prepareOutcome struct {
		spec *proto.TunnelSpec
		err  error
	}
	startPrepare := func(ctx context.Context, clientProxy *ClientProxy, request *proto.TunnelReq) <-chan prepareOutcome {
		outcome := make(chan prepareOutcome, 1)
		go func() {
			spec, prepareErr := clientProxy.PrepareTunnel(ctx, stdproto.CloneOf(request))
			outcome <- prepareOutcome{spec: spec, err: prepareErr}
		}()
		return outcome
	}
	// Returns a context that reports when the operation using it starts waiting for the tunnel name lease.
	waitingOnLease := func(ctx context.Context) (context.Context, <-chan struct{}) {
		waiting := make(chan struct{})
		var waitingOnce sync.Once
		return &doneObserverContext{
			Context: ctx,
			onDone:  func() { waitingOnce.Do(func() { close(waiting) }) },
		}, waiting
	}
	awaitSignal := func(t *testing.T, ctx context.Context, signal <-chan struct{}, description string) {
		t.Helper()
		select {
		case <-signal:
		case <-ctx.Done():
			t.Fatalf("Timed out waiting until %s", description)
		}
	}
	newGatedClientProxy := func(t *testing.T, ctx context.Context) (*ClientProxy, *gatedTunnelListen) {
		t.Helper()
		dataListener, _ := setupListener(t, autoAllocatePort)
		t.Cleanup(func() { _ = dataListener.Close() })
		clientCtx, cancelClient := context.WithCancel(ctx)
		t.Cleanup(cancelClient)
		gate := newGatedTunnelListen()
		t.Cleanup(gate.releaseFirst)
		clientProxy := NewClientProxy(clientCtx, dataListener, cancelClient, ClientProxyConfig{Listen: gate.listen}, testutil.NewLogForTesting(t.Name()))
		return clientProxy, gate
	}

	t.Run("identical-requests-share-one-listener", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
		defer cancel()
		clientProxy, gate := newGatedClientProxy(t, ctx)

		request := &proto.TunnelReq{
			TunnelName:                   stdproto.String("shared-retry"),
			ServerAddress:                stdproto.String(networking.IPv4LocalhostDefaultAddress),
			ServerPort:                   stdproto.Int32(32001),
			ClientProxyAddress:           stdproto.String(networking.IPv4LocalhostDefaultAddress),
			ClientProxyPort:              stdproto.Int32(0),
			AllowClientProxyPortFallback: stdproto.Bool(true),
		}
		firstOutcome := startPrepare(ctx, clientProxy, request)
		awaitSignal(t, ctx, gate.entered, "the first request binds its listener")
		secondCtx, secondWaiting := waitingOnLease(ctx)
		secondOutcome := startPrepare(secondCtx, clientProxy, request)
		awaitSignal(t, ctx, secondWaiting, "the identical request waits for the tunnel name")
		gate.releaseFirst()

		first := <-firstOutcome
		second := <-secondOutcome
		require.NoError(t, first.err)
		require.NoError(t, second.err)
		require.Equal(t, first.spec.GetTunnelRef().GetTunnelId(), second.spec.GetTunnelRef().GetTunnelId())
		require.Equal(t, first.spec.GetClientProxyPort(), second.spec.GetClientProxyPort())
		require.Equal(t, int32(1), gate.calls.Load(), "A request waiting for its tunnel name must reuse the prepared tunnel instead of binding another listener")

		clientProxy.lock.Lock()
		require.Len(t, clientProxy.tunnels, 1)
		clientProxy.lock.Unlock()
		require.False(t, gate.boundListener(0).closed.Load())
	})

	t.Run("canceled-preparation-is-not-orphaned-by-name-based-deletion", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
		defer cancel()
		clientProxy, gate := newGatedClientProxy(t, ctx)

		prepareCtx, cancelPrepare := context.WithCancel(ctx)
		defer cancelPrepare()
		prepareOutcome := startPrepare(prepareCtx, clientProxy, &proto.TunnelReq{
			TunnelName:         stdproto.String("abandoned-preparation"),
			ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
			ServerPort:         stdproto.Int32(32003),
			ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		})
		awaitSignal(t, ctx, gate.entered, "the preparation binds its listener")
		// The caller gives up (for example, its deadline expires) while the listener is being bound.
		cancelPrepare()

		deleteCtx, deleteWaiting := waitingOnLease(ctx)
		deleteResult := make(chan error, 1)
		go func() {
			_, deleteErr := clientProxy.DeleteTunnel(deleteCtx, &proto.TunnelRef{TunnelName: stdproto.String("abandoned-preparation")})
			deleteResult <- deleteErr
		}()
		awaitSignal(t, ctx, deleteWaiting, "the name-based deletion waits for the preparation")
		gate.releaseFirst()

		require.Equal(t, codes.Canceled, status.Code((<-prepareOutcome).err))
		require.NoError(t, <-deleteResult)
		clientProxy.lock.Lock()
		require.Empty(t, clientProxy.tunnels)
		require.Empty(t, clientProxy.tunnelsByName)
		clientProxy.lock.Unlock()
		require.True(t, gate.boundListener(0).closed.Load(), "The listener bound for a canceled request must be closed")
	})

	t.Run("retry-after-canceled-preparation-binds-preferred-port", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
		defer cancel()
		clientProxy, gate := newGatedClientProxy(t, ctx)
		portProbe, preferredPort := setupListener(t, autoAllocatePort)
		require.NoError(t, portProbe.Close())

		request := &proto.TunnelReq{
			TunnelName:                   stdproto.String("preferred-port-retry"),
			ServerAddress:                stdproto.String(networking.IPv4LocalhostDefaultAddress),
			ServerPort:                   stdproto.Int32(32004),
			ClientProxyAddress:           stdproto.String(networking.IPv4LocalhostDefaultAddress),
			ClientProxyPort:              stdproto.Int32(int32(preferredPort)),
			AllowClientProxyPortFallback: stdproto.Bool(true),
		}
		prepareCtx, cancelPrepare := context.WithCancel(ctx)
		defer cancelPrepare()
		canceledOutcome := startPrepare(prepareCtx, clientProxy, request)
		awaitSignal(t, ctx, gate.entered, "the first attempt binds the preferred port")
		cancelPrepare()

		retryCtx, retryWaiting := waitingOnLease(ctx)
		retryOutcome := startPrepare(retryCtx, clientProxy, request)
		awaitSignal(t, ctx, retryWaiting, "the retry waits for the canceled attempt")
		gate.releaseFirst()

		require.Equal(t, codes.Canceled, status.Code((<-canceledOutcome).err))
		retry := <-retryOutcome
		require.NoError(t, retry.err)
		require.Equal(t, int32(preferredPort), retry.spec.GetClientProxyPort(),
			"A retry must not fall back to a random port while the canceled attempt still holds the preferred port")
	})

	t.Run("disposed-during-listen", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
		defer cancel()

		dataListener, _ := setupListener(t, autoAllocatePort)
		defer dataListener.Close()
		clientCtx, cancelClient := context.WithCancel(ctx)
		defer cancelClient()

		listenEntered := make(chan struct{})
		releaseListen := make(chan struct{})
		var boundListener *closingTrackedListener

		var listenConfig net.ListenConfig
		clientProxy := NewClientProxy(clientCtx, dataListener, cancelClient, ClientProxyConfig{
			Listen: func(callCtx context.Context, networkName, address string) (net.Listener, error) {
				rawListener, listenErr := listenConfig.Listen(callCtx, networkName, address)
				if listenErr != nil {
					return nil, listenErr
				}
				boundListener = &closingTrackedListener{Listener: rawListener}
				close(listenEntered)
				<-releaseListen
				return boundListener, nil
			},
		}, testutil.NewLogForTesting(t.Name()))

		prepareDone := make(chan error, 1)
		go func() {
			_, prepareErr := clientProxy.PrepareTunnel(ctx, &proto.TunnelReq{
				TunnelName:         stdproto.String("disposed-race"),
				ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
				ServerPort:         stdproto.Int32(32002),
				ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
			})
			prepareDone <- prepareErr
		}()

		<-listenEntered
		_, shutdownErr := clientProxy.Shutdown(ctx, &emptypb.Empty{})
		require.NoError(t, shutdownErr)
		close(releaseListen)

		prepareErr := <-prepareDone
		require.Equal(t, codes.FailedPrecondition, status.Code(prepareErr))
		require.NotNil(t, boundListener)
		require.True(t, boundListener.closed.Load(), "Listener bound during concurrent disposal must be closed")
		clientProxy.lock.Lock()
		require.Empty(t, clientProxy.tunnels)
		clientProxy.lock.Unlock()
	})
}

type closingTrackedListener struct {
	net.Listener
	closed atomic.Bool
}

func (l *closingTrackedListener) Close() error {
	l.closed.Store(true)
	return l.Listener.Close()
}

// gatedTunnelListen binds tunnel listeners and holds the first bind until released, regardless of the
// caller's context, modeling a bind that completes after its request has been canceled.
type gatedTunnelListen struct {
	entered      chan struct{}
	release      chan struct{}
	releaseFirst func()
	calls        atomic.Int32
	lock         sync.Mutex
	bound        []*closingTrackedListener
}

func newGatedTunnelListen() *gatedTunnelListen {
	gate := &gatedTunnelListen{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	gate.releaseFirst = sync.OnceFunc(func() { close(gate.release) })
	return gate
}

func (gate *gatedTunnelListen) listen(ctx context.Context, networkName string, address string) (net.Listener, error) {
	var listenConfig net.ListenConfig
	rawListener, listenErr := listenConfig.Listen(ctx, networkName, address)
	if listenErr != nil {
		return nil, listenErr
	}
	tracked := &closingTrackedListener{Listener: rawListener}
	gate.lock.Lock()
	gate.bound = append(gate.bound, tracked)
	gate.lock.Unlock()
	if gate.calls.Add(1) == 1 {
		close(gate.entered)
		<-gate.release
	}
	return tracked, nil
}

func (gate *gatedTunnelListen) boundListener(index int) *closingTrackedListener {
	gate.lock.Lock()
	defer gate.lock.Unlock()
	return gate.bound[index]
}

// Verifies that PrepareTunnel scopes request deduplication to TunnelName so distinct
// logical tunnels targeting the same server endpoint receive distinct TunnelIDs.
func TestPrepareTunnelScopesIdempotencyByTunnelName(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{})
	defer pair.cleanup()
	echoListener, echoPort := setupEchoServer(t)
	defer echoListener.Close()

	reqA := &proto.TunnelReq{
		TunnelName:                   stdproto.String("tunnel-a"),
		ServerAddress:                stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:                   stdproto.Int32(int32(echoPort)),
		ClientProxyAddress:           stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ClientProxyPort:              stdproto.Int32(int32(echoPort)),
		AllowClientProxyPortFallback: stdproto.Bool(true),
	}
	reqB := stdproto.CloneOf(reqA)
	reqB.TunnelName = stdproto.String("tunnel-b")

	specA, errA := pair.control.PrepareTunnel(ctx, reqA)
	require.NoError(t, errA)
	specB, errB := pair.control.PrepareTunnel(ctx, reqB)
	require.NoError(t, errB)
	require.NotEqual(t, specA.GetTunnelRef().GetTunnelId(), specB.GetTunnelRef().GetTunnelId())
	require.NotEqual(t, specA.GetClientProxyPort(), specB.GetClientProxyPort())

	retryA, retryErrA := pair.control.PrepareTunnel(ctx, reqA)
	require.NoError(t, retryErrA)
	require.Equal(t, specA.GetTunnelRef().GetTunnelId(), retryA.GetTunnelRef().GetTunnelId())

	_, deleteBErr := pair.control.DeleteTunnel(ctx, specB.GetTunnelRef())
	require.NoError(t, deleteBErr)
	require.NoError(t, doRoundTrip(ctx, networking.IPv4LocalhostDefaultAddress, specA.GetClientProxyPort(), 10*time.Second, func() []byte {
		return []byte("tunnel-a remains active\n")
	}))
}

// Verifies that ClientProxy closes idle unauthenticated data connections after the
// preamble deadline and rejects mismatched data-connection bearer tokens.
func TestClientProxyDataConnectionPreambleDeadlineAndTokenRejections(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	dataListener, dataPort := setupListener(t, autoAllocatePort)
	defer dataListener.Close()
	clientCtx, cancelClient := context.WithCancel(ctx)
	defer cancelClient()
	clientProxy := NewClientProxy(clientCtx, dataListener, cancelClient, ClientProxyConfig{
		DataConnectionPreambleTimeout: 100 * time.Millisecond,
	}, testutil.NewLogForTesting(t.Name()))

	var dialer net.Dialer
	idleConn, idleDialErr := dialer.DialContext(ctx, "tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, int32(dataPort)))
	require.NoError(t, idleDialErr)
	defer idleConn.Close()
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.NoError(t, idleConn.SetReadDeadline(deadline))
	_, idleReadErr := idleConn.Read(make([]byte, 1))
	require.Error(t, idleReadErr, "Idle data connection without a preamble must be closed after preambleTimeout")

	spec, prepareErr := clientProxy.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:         stdproto.String("preamble-token-check"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(32010),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	})
	require.NoError(t, prepareErr)

	clientConn, clientDialErr := dialer.DialContext(ctx, "tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, spec.GetClientProxyPort()))
	require.NoError(t, clientDialErr)
	defer clientConn.Close()

	var pendingStreamID StreamID
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		clientProxy.lock.Lock()
		defer clientProxy.lock.Unlock()
		td := clientProxy.tunnels[TunnelID(spec.GetTunnelRef().GetTunnelId())]
		for sid := range td.streams {
			pendingStreamID = sid
			return true, nil
		}
		return false, nil
	}))

	forgedConn, forgedDialErr := dialer.DialContext(ctx, "tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, int32(dataPort)))
	require.NoError(t, forgedDialErr)
	defer forgedConn.Close()
	require.NoError(t, forgedConn.SetDeadline(deadline))

	var preamble bytes.Buffer
	require.NoError(t, binary.Write(&preamble, binary.BigEndian, spec.GetTunnelRef().GetTunnelId()))
	require.NoError(t, binary.Write(&preamble, binary.BigEndian, uint64(pendingStreamID)))
	wrongToken := bytes.Repeat([]byte{0xAA}, len(spec.GetDataConnectionToken()))
	if bytes.Equal(wrongToken, spec.GetDataConnectionToken()) {
		wrongToken[0] ^= 0xFF
	}
	_, _ = preamble.Write(wrongToken)
	_, writeErr := forgedConn.Write(preamble.Bytes())
	require.NoError(t, writeErr)
	_, forgedReadErr := forgedConn.Read(make([]byte, 1))
	require.Error(t, forgedReadErr, "Data connection with wrong token must be closed")

	clientProxy.lock.Lock()
	streamInfo, _ := clientProxy.getStreamInfo(TunnelID(spec.GetTunnelRef().GetTunnelId()), pendingStreamID)
	require.NotNil(t, streamInfo, "Forged data connection must not dispose the waiting client stream")
	require.Nil(t, streamInfo.proxyConn)
	clientProxy.lock.Unlock()
}

// Verifies that new stream establishment is pipelined and isolated per tunnel so an
// unreachable backend saturating one tunnel's dial workers never stalls healthy tunnels.
func TestTunnelStreamEstablishmentDoesNotBlockAcrossTunnels(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		server: ServerProxyConfig{
			MaxConcurrentStreamCreationsPerTunnel: 1,
		},
	})
	defer pair.cleanup()

	// Grab a free port and close its listener immediately so dials to it fail and enter RetryGetExponential.
	closedListener, unreachablePort := setupListener(t, autoAllocatePort)
	require.NoError(t, closedListener.Close())

	echoListener, echoPort := setupEchoServer(t)
	defer echoListener.Close()

	stalledSpec, stalledPrepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:         stdproto.String("stalled-tunnel"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(unreachablePort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	})
	require.NoError(t, stalledPrepareErr)

	healthySpec, healthyPrepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:         stdproto.String("healthy-tunnel"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(echoPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	})
	require.NoError(t, healthyPrepareErr)

	// Open multiple client connections to the unreachable tunnel (exceeding MaxConcurrentStreamCreationsPerTunnel).
	var dialer net.Dialer
	var stalledConns []net.Conn
	defer func() {
		for _, c := range stalledConns {
			_ = c.Close()
		}
	}()
	for range 3 {
		stalledConn, stalledDialErr := dialer.DialContext(
			ctx,
			"tcp",
			networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, stalledSpec.GetClientProxyPort()),
		)
		require.NoError(t, stalledDialErr)
		stalledConns = append(stalledConns, stalledConn)
	}

	// Wait until all 3 stalled streams have been accepted on ClientProxy so they are queued/in-flight on NewStreamsConnection.
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		pair.client.lock.Lock()
		defer pair.client.lock.Unlock()
		td := pair.client.tunnels[TunnelID(stalledSpec.GetTunnelRef().GetTunnelId())]
		return td != nil && len(td.streams) == 3, nil
	}))

	// Verify that multiple connections on the healthy tunnel still complete well within 2s (far below the 60s stalled dial retry).
	for i := range 3 {
		require.NoError(t, doRoundTrip(
			ctx,
			networking.IPv4LocalhostDefaultAddress,
			healthySpec.GetClientProxyPort(),
			2*time.Second,
			func() []byte { return []byte(fmt.Sprintf("fast-healthy-%d\n", i)) },
		))
	}
}

// Verifies that disposing ClientProxy unblocks an open idle NewStreamsConnection
// immediately even while the gRPC server remains running and the client keeps its stream open.
func TestClientProxyShutdownUnblocksIdleNewStreamsConnection(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	securityConfig, securityConfigErr := NewTunnelProxySecurityConfig()
	require.NoError(t, securityConfigErr)
	controlListener, controlPort := setupListener(t, autoAllocatePort)
	dataListener, _ := setupListener(t, autoAllocatePort)
	clientCtx, cancelClient := context.WithCancel(ctx)
	defer cancelClient()

	clientProxy := NewClientProxy(clientCtx, dataListener, cancelClient, ClientProxyConfig{}, testutil.NewLogForTesting(t.Name()))
	controlServer, serverErr := NewControlServer(securityConfig.ClientProxyConfig(), clientProxy)
	require.NoError(t, serverErr)
	go func() {
		serveErr := controlServer.Serve(controlListener)
		if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
			t.Errorf("Client control server failed: %v", serveErr)
		}
	}()
	defer func() {
		controlServer.Stop()
		_ = controlListener.Close()
		_ = dataListener.Close()
	}()

	dialOpts, dialOptsErr := securityConfig.ServerProxyConfig().ClientDialOptions()
	require.NoError(t, dialOptsErr)
	conn, connErr := grpc.NewClient(
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, int32(controlPort)),
		dialOpts...,
	)
	require.NoError(t, connErr)
	defer conn.Close()

	stream, streamErr := proto.NewTunnelControlClient(conn).NewStreamsConnection(ctx)
	require.NoError(t, streamErr)
	headers, headerErr := stream.Header()
	require.NoError(t, headerErr)
	require.Equal(t, []string{"true"}, headers.Get(controlStreamReadyHeader))

	_, shutdownErr := clientProxy.Shutdown(ctx, &emptypb.Empty{})
	require.NoError(t, shutdownErr)

	recvDone := make(chan error, 1)
	go func() {
		_, recvErr := stream.Recv()
		recvDone <- recvErr
	}()
	select {
	case recvErr := <-recvDone:
		require.Equal(t, codes.Unavailable, status.Code(recvErr))
	case <-time.After(3 * time.Second):
		t.Fatal("ClientProxy.Shutdown did not end the idle NewStreamsConnection promptly")
	}

	require.True(t, clientProxy.controlStreamLock.TryLock(), "NewStreamsConnection must release controlStreamLock on shutdown")
	clientProxy.controlStreamLock.Unlock()
}

type doneObserverContext struct {
	context.Context
	onDone func()
}

func (c *doneObserverContext) Done() <-chan struct{} {
	if c.onDone != nil {
		c.onDone()
	}
	return c.Context.Done()
}

type gatedPrepareTunnelClient struct {
	proto.TunnelControlClient
	entered        chan struct{}
	release        chan struct{}
	spec           *proto.TunnelSpec
	failNextDelete atomic.Bool
}

func (c *gatedPrepareTunnelClient) PrepareTunnel(
	ctx context.Context,
	_ *proto.TunnelReq,
	_ ...grpc.CallOption,
) (*proto.TunnelSpec, error) {
	if c.entered != nil {
		close(c.entered)
	}
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	return stdproto.CloneOf(c.spec), nil
}

func (c *gatedPrepareTunnelClient) DeleteTunnel(
	_ context.Context,
	_ *proto.TunnelRef,
	_ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	if c.failNextDelete.CompareAndSwap(true, false) {
		return nil, status.Error(codes.Unavailable, "transient client delete failure")
	}
	return &emptypb.Empty{}, nil
}

func (c *gatedPrepareTunnelClient) Shutdown(
	_ context.Context,
	_ *emptypb.Empty,
	_ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// Verifies that ServerProxy.PrepareTunnel strips DataConnectionToken from the TunnelSpec returned
// to the controller while retaining it in server-side tunnel state, and rejects completions that
// race with proxy disposal or concurrent tunnel deletion.
func TestServerProxyPrepareTunnelStripsTokenAndChecksDisposalAndDeletion(t *testing.T) {
	t.Parallel()

	secretToken := []byte("0123456789abcdef0123456789abcdef")
	clientSpec := &proto.TunnelSpec{
		TunnelRef:            &proto.TunnelRef{TunnelId: stdproto.Uint32(7)},
		ServerAddress:        stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:           stdproto.Int32(34001),
		ClientProxyAddresses: []string{networking.IPv4LocalhostDefaultAddress},
		ClientProxyPort:      stdproto.Int32(44001),
		DataConnectionToken:  append([]byte(nil), secretToken...),
	}

	newUnitServer := func(lifetimeCtx context.Context, requestShutdown func(), client proto.TunnelControlClient) *ServerProxy {
		return &ServerProxy{
			lifetimeCtx:     lifetimeCtx,
			log:             logr.Discard(),
			clientProxy:     client,
			requestShutdown: requestShutdown,
			tunnels:         make(map[TunnelID]*tunnelData[stopStreamFn]),
			tunnelsByName:   make(map[string]TunnelID),
			nameLeases:      concurrency.NewLeaseMap[string](),
			lock:            &sync.Mutex{},
			dispose:         concurrency.NewOneTimeJob[struct{}](),
			failure:         concurrency.NewValuePromise[error](),
			ready:           concurrency.NewValuePromise[struct{}](),
		}
	}

	t.Run("strips-data-connection-token", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
		defer cancel()
		server := newUnitServer(ctx, func() {}, &gatedPrepareTunnelClient{spec: clientSpec})

		returnedSpec, prepareErr := server.PrepareTunnel(ctx, &proto.TunnelReq{
			TunnelName:    stdproto.String("token-strip"),
			ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
			ServerPort:    stdproto.Int32(34001),
		})
		require.NoError(t, prepareErr)
		require.Empty(t, returnedSpec.GetDataConnectionToken(), "Controller-facing TunnelSpec must not carry DataConnectionToken")

		server.lock.Lock()
		stored := server.tunnels[7]
		server.lock.Unlock()
		require.NotNil(t, stored)
		require.Equal(t, secretToken, stored.spec.GetDataConnectionToken())
	})

	t.Run("rejects-disposal-during-client-rpc", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
		defer cancel()
		entered := make(chan struct{})
		release := make(chan struct{})
		serverCtx, stopServer := context.WithCancel(ctx)
		defer stopServer()
		server := newUnitServer(serverCtx, stopServer, &gatedPrepareTunnelClient{entered: entered, release: release, spec: clientSpec})

		outcome := make(chan error, 1)
		go func() {
			_, prepareErr := server.PrepareTunnel(ctx, &proto.TunnelReq{
				TunnelName:    stdproto.String("dispose-race"),
				ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
				ServerPort:    stdproto.Int32(34001),
			})
			outcome <- prepareErr
		}()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("PrepareTunnel did not reach client RPC")
		}

		_, shutdownErr := server.Shutdown(ctx, &emptypb.Empty{})
		require.NoError(t, shutdownErr)
		close(release)

		prepareErr := <-outcome
		require.Equal(t, codes.FailedPrecondition, status.Code(prepareErr))
		server.lock.Lock()
		require.Empty(t, server.tunnels)
		server.lock.Unlock()
	})

	t.Run("rejects-deleted-tunnel-with-draining-stream", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
		defer cancel()
		server := newUnitServer(ctx, func() {}, &gatedPrepareTunnelClient{spec: clientSpec})

		existing := newTunnelData[stopStreamFn](clientSpec)
		existing.tunnelName = "draining-tunnel"
		existing.streams[1] = func() {}
		existing.accepting.Store(false)
		existing.deleted.Store(true)
		server.lock.Lock()
		server.tunnels[7] = existing
		server.lock.Unlock()

		_, prepareErr := server.PrepareTunnel(ctx, &proto.TunnelReq{
			TunnelName:    stdproto.String("draining-tunnel"),
			ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
			ServerPort:    stdproto.Int32(34001),
		})
		require.Equal(t, codes.Aborted, status.Code(prepareErr))
	})

	t.Run("retains-name-mapping-across-failed-delete-retry", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
		defer cancel()
		mockClient := &gatedPrepareTunnelClient{spec: clientSpec}
		mockClient.failNextDelete.Store(true)
		server := newUnitServer(ctx, func() {}, mockClient)

		_, prepareErr := server.PrepareTunnel(ctx, &proto.TunnelReq{
			TunnelName:    stdproto.String("retry-delete-by-name"),
			ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
			ServerPort:    stdproto.Int32(34001),
		})
		require.NoError(t, prepareErr)

		_, firstDeleteErr := server.DeleteTunnel(ctx, &proto.TunnelRef{TunnelName: stdproto.String("retry-delete-by-name")})
		require.Error(t, firstDeleteErr)

		_, retryDeleteErr := server.DeleteTunnel(ctx, &proto.TunnelRef{TunnelName: stdproto.String("retry-delete-by-name")})
		require.NoError(t, retryDeleteErr)

		server.lock.Lock()
		require.Empty(t, server.tunnels, "Retried delete-by-name must evict idle server tunnel")
		require.Empty(t, server.tunnelsByName)
		server.lock.Unlock()
	})
}

// Verifies that ServerProxy.DeleteTunnel evicts idle tunnel entries immediately instead of leaking
// TunnelSpec credentials, and supports deleting tunnels by tunnel_name when tunnel_id is unknown.
func TestServerProxyDeleteTunnelEvictsIdleTunnelAndSupportsDeleteByName(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{})
	defer pair.cleanup()

	echoListener, echoPort := setupEchoServer(t)
	defer func() { _ = echoListener.Close() }()

	preparedByID, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:    stdproto.String("delete-by-id"),
		ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:    stdproto.Int32(int32(echoPort)),
	})
	require.NoError(t, prepareErr)
	require.Empty(t, preparedByID.GetDataConnectionToken())
	tid := TunnelID(preparedByID.GetTunnelRef().GetTunnelId())

	_, deleteErr := pair.control.DeleteTunnel(ctx, &proto.TunnelRef{TunnelId: stdproto.Uint32(uint32(tid))})
	require.NoError(t, deleteErr)

	pair.server.lock.Lock()
	_, existsOnServer := pair.server.tunnels[tid]
	_, nameExistsOnServer := pair.server.tunnelsByName["delete-by-id"]
	pair.server.lock.Unlock()
	require.False(t, existsOnServer, "Idle deleted tunnel must be evicted from sp.tunnels")
	require.False(t, nameExistsOnServer, "Deleted tunnel name must be evicted from sp.tunnelsByName")

	preparedByName, prepareByNameErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:    stdproto.String("delete-by-name"),
		ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:    stdproto.Int32(int32(echoPort)),
	})
	require.NoError(t, prepareByNameErr)
	namedPort := preparedByName.GetClientProxyPort()
	namedID := TunnelID(preparedByName.GetTunnelRef().GetTunnelId())

	_, deleteByNameErr := pair.control.DeleteTunnel(ctx, &proto.TunnelRef{TunnelName: stdproto.String("delete-by-name")})
	require.NoError(t, deleteByNameErr)

	// Repeat delete-by-name to verify idempotency when the tunnel no longer exists on either proxy.
	_, repeatDeleteErr := pair.control.DeleteTunnel(ctx, &proto.TunnelRef{TunnelName: stdproto.String("delete-by-name")})
	require.NoError(t, repeatDeleteErr)

	pair.server.lock.Lock()
	_, namedOnServer := pair.server.tunnels[namedID]
	pair.server.lock.Unlock()
	require.False(t, namedOnServer)

	pair.client.lock.Lock()
	_, namedOnClient := pair.client.tunnels[namedID]
	_, nameMappedOnClient := pair.client.tunnelsByName["delete-by-name"]
	pair.client.lock.Unlock()
	require.False(t, namedOnClient)
	require.False(t, nameMappedOnClient)

	// Re-creating a tunnel with the same name and a different backend port must succeed instead of returning AlreadyExists.
	secondEchoListener, secondEchoPort := setupEchoServer(t)
	defer func() { _ = secondEchoListener.Close() }()
	recreatedSpec, recreateErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:      stdproto.String("delete-by-name"),
		ServerAddress:   stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:      stdproto.Int32(int32(secondEchoPort)),
		ClientProxyPort: stdproto.Int32(namedPort),
	})
	require.NoError(t, recreateErr)
	require.NotEqual(t, uint32(namedID), recreatedSpec.GetTunnelRef().GetTunnelId())
}

// Verifies that ServerProxy.DeleteTunnel waits for a transiently failed control connection to the
// client proxy to recover, instead of failing the deletion immediately.
func TestServerProxyDeleteTunnelWaitsForControlConnectionRecovery(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	transport := &controlledTunnelTransport{}
	deleteInvoked := make(chan struct{})
	var deleteInvokedOnce sync.Once
	pair := createProxyPair(t, ctx, testutil.NewLogForTesting(t.Name()), proxyPairConfig{
		controlDialer: transport.dial,
		controlUnaryInterceptor: func(
			callCtx context.Context,
			method string,
			request any,
			reply any,
			connection *grpc.ClientConn,
			invoker grpc.UnaryInvoker,
			options ...grpc.CallOption,
		) error {
			if method == proto.TunnelControl_DeleteTunnel_FullMethodName {
				deleteInvokedOnce.Do(func() { close(deleteInvoked) })
			}
			return invoker(callCtx, method, request, reply, connection, options...)
		},
	})
	defer pair.cleanup()
	echoListener, echoPort := setupEchoServer(t)
	defer func() { _ = echoListener.Close() }()

	tunnelSpec, prepareErr := pair.control.PrepareTunnel(ctx, &proto.TunnelReq{
		TunnelName:         stdproto.String("delete-during-control-outage"),
		ServerAddress:      stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:         stdproto.Int32(int32(echoPort)),
		ClientProxyAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
	})
	require.NoError(t, prepareErr)

	// Calls that do not wait for readiness fail immediately while the connection is in TRANSIENT_FAILURE.
	require.NoError(t, transport.interrupt(true))
	for state := pair.clientProxyConn.GetState(); state != connectivity.TransientFailure; state = pair.clientProxyConn.GetState() {
		if !pair.clientProxyConn.WaitForStateChange(ctx, state) {
			t.Fatal("The control connection to the client proxy did not enter TRANSIENT_FAILURE")
		}
	}

	deleteResult := make(chan error, 1)
	go func() {
		_, deleteErr := pair.control.DeleteTunnel(ctx, tunnelSpec.GetTunnelRef())
		deleteResult <- deleteErr
	}()
	select {
	case <-deleteInvoked:
	case <-ctx.Done():
		t.Fatal("DeleteTunnel did not call the client proxy")
	}

	transport.lock.Lock()
	transport.blocked = false
	transport.lock.Unlock()
	pair.clientProxyConn.ResetConnectBackoff()

	select {
	case deleteErr := <-deleteResult:
		require.NoError(t, deleteErr)
	case <-ctx.Done():
		t.Fatal("DeleteTunnel did not complete after the control connection recovered")
	}
	pair.client.lock.Lock()
	require.Empty(t, pair.client.tunnels)
	pair.client.lock.Unlock()
}
