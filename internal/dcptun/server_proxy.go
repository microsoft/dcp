/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	stdproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/proxy"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/grpcutil"
	"github.com/microsoft/dcp/pkg/resiliency"
)

// We only need a cancellation function to stop a tunnel stream;
// no other data is needed for a tunnel stream on the server proxy side.
type stopStreamFn context.CancelFunc

// The result of the tunnel stream connection creator.
type streamConnectionResult struct {
	streamRef         *proto.StreamRef // The new stream reference (the one we are creation connections for).
	serverConn        net.Conn         // The connection to the server endpoint (if successful).
	clientDataConn    net.Conn         // The connection to the client proxy data endpoint (if successful).
	tunnelNotFoundErr error            // The error indicating that the referenced tunnel (part of stream reference) was not found.
	serverDialErr     error            // The error that occurred while dialing the server endpoint.
	clientDataDialErr error            // The error that occurred while dialing the client proxy data endpoint.
	streamLog         logr.Logger      // The logger to be used for the stream.
}

// The server-side proxy of the DCP reverse network tunnel.
type ServerProxy struct {
	// Need to embed the following to ensure gRPC forward compatibility.
	proto.UnimplementedTunnelControlServer

	// The lifetime context of the server-side proxy.
	lifetimeCtx context.Context

	// The logger for the server-side proxy.
	log logr.Logger

	// The client data endpoint address and port.
	clientDataEndpointAddress string
	clientDataEndpointPort    int32

	// The client-side counterpart of the tunnel.
	clientProxy proto.TunnelControlClient

	// Function to request shutdown of the server-side proxy.
	requestShutdown func()

	// Data about tunnels managed by this proxy, indexed by tunnel ID.
	tunnels map[TunnelID]*tunnelData[stopStreamFn]

	// Mutex to protect access to internal proxy state (primarily the tunnels map).
	lock *sync.Mutex

	// A OneTimeJob representing disposal of the server-side proxy.
	dispose *concurrency.OneTimeJob[struct{}]

	// Asynchronous worker that creates new stream connections
	// (the server connection and the client data connection to the client proxy).
	streamConnectionCreator *resiliency.AsyncWorker[*proto.StreamRef, streamConnectionResult]
}

func NewServerProxy(
	ctx context.Context,
	clientProxy proto.TunnelControlClient,
	clientDataEndpointAddress string,
	clientDataEndpointPort int32,
	requestShutdown func(),
	log logr.Logger,
) *ServerProxy {
	sp := &ServerProxy{
		lifetimeCtx:               ctx,
		clientProxy:               clientProxy,
		clientDataEndpointAddress: clientDataEndpointAddress,
		clientDataEndpointPort:    clientDataEndpointPort,
		requestShutdown:           requestShutdown,
		log:                       log,
		tunnels:                   make(map[TunnelID]*tunnelData[stopStreamFn]),
		lock:                      &sync.Mutex{},
		dispose:                   concurrency.NewOneTimeJob[struct{}](),
	}
	sp.streamConnectionCreator = resiliency.NewAsyncWorker(ctx, sp.createStreamConnections, resiliency.DefaultConcurrency)

	go sp.streamsCreationWorker()
	context.AfterFunc(ctx, sp.disposeOnce)
	return sp
}

// Prepares the proxy pair for tunneling the traffic.
// Upon success, the client-side proxy is listening to client connections and ready to tunnel traffic.
func (sp *ServerProxy) PrepareTunnel(ctx context.Context, tr *proto.TunnelReq) (*proto.TunnelSpec, error) {
	if sp.dispose.IsDone() {
		return nil, status.Error(codes.FailedPrecondition, errMsgProxyDisposed)
	}

	validationErr := ensureValidTunnelRequest(tr)
	if validationErr != nil {
		return nil, validationErr
	}

	ts, prepareErr := sp.clientProxy.PrepareTunnel(ctx, tr, grpc.WaitForReady(true))
	if prepareErr != nil {
		sp.log.Error(prepareErr, "Failed to prepare tunnel with client proxy", "TunnelReq", tr.String())
		return nil, status.Errorf(codes.Internal, "failed to prepare tunnel with client proxy: %s", prepareErr.Error())
	}

	sp.lock.Lock()
	defer sp.lock.Unlock()

	tid := TunnelID(ts.GetTunnelRef().GetTunnelId())
	if tid == invalidTunnelID {
		return nil, status.Errorf(codes.Internal, "client proxy returned an invalid tunnel ID %d", tid) // Should never happen
	}

	existing, hasTd := sp.tunnels[tid]
	if hasTd && !existing.spec.Same(ts) {
		return nil, status.Errorf(codes.AlreadyExists, "tunnel with ID %d already exists with different configuration", tid) // Should never happen
	}

	if !hasTd {
		sp.tunnels[tid] = newTunnelData[stopStreamFn](ts)
	}

	sp.log.V(1).Info("Tunnel prepared", "TunnelSpec", ts.LogString())

	return ts, nil
}

// Gracefully deletes the tunnel identified by tunnel ID.
// Existing streams will be preserved, but no new streams will be allowed.
func (sp *ServerProxy) DeleteTunnel(ctx context.Context, tr *proto.TunnelRef) (*emptypb.Empty, error) {
	tid := TunnelID(tr.GetTunnelId())
	if tid == invalidTunnelID {
		return nil, status.Error(codes.InvalidArgument, "bad request: no tunnel ID specified")
	}

	sp.lock.Lock()
	td, found := sp.tunnels[tid]
	sp.lock.Unlock()
	if !found {
		return nil, status.Errorf(codes.NotFound, "tunnel with ID %d does not exist", tid)
	}

	sp.log.V(1).Info("Deleting tunnel..", "tunnel_spec", td.spec.LogString())

	// Always call the client proxy to facilitate retries.
	_, clientDeleteErr := sp.clientProxy.DeleteTunnel(ctx, tr)

	// Delete the tunnel from the server-side proxy even if the client proxy deletion fails.
	sp.lock.Lock()
	td, found = sp.tunnels[tid]
	if !found || td.deleted.Load() {
		sp.lock.Unlock()
		// We found it above so the tunnel ID was valid. It is possible that it was deleted concurrently.
		sp.log.V(1).Info("Tunnel already deleted", "tunnel_id", tid)
	} else {
		td.deleted.Store(true)
		sp.lock.Unlock()
		sp.log.V(1).Info("Tunnel deleted", "tunnel_spec", td.spec.LogString())
	}

	if clientDeleteErr != nil {
		sp.log.Error(clientDeleteErr, "Failed to delete tunnel with client proxy", "tunnel_spec", td.spec.LogString())
		return nil, status.Errorf(codes.Internal, "failed to delete tunnel with client proxy: %s", clientDeleteErr.Error())
	} else {
		return &emptypb.Empty{}, nil
	}
}

// Shuts down both sides of the tunneling proxy. Existing streams will be aborted.
func (sp *ServerProxy) Shutdown(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	// Always try to call the client proxy, even if we are already shut down, in case the operation is retried.
	_, clientProxyErr := sp.clientProxy.Shutdown(ctx, &emptypb.Empty{}, grpc.WaitForReady(true))

	// Initiate the shutdown regardless whether the client proxy shutdown was successful or not.
	sp.disposeOnce()

	if clientProxyErr != nil {
		sp.log.Error(clientProxyErr, "Failed to shutdown client proxy")
		return nil, status.Errorf(codes.Internal, "failed to shutdown client proxy: %s", clientProxyErr.Error())
	} else {
		return &emptypb.Empty{}, nil
	}
}

func (sp *ServerProxy) NewStreamsConnection(_ grpc.BidiStreamingServer[proto.NewStreamResult, proto.StreamRef]) error {
	err := status.Error(codes.Unimplemented, "NewStreamsConnection() is only implemented by the client-side proxy of the tunnel")
	return err
}

func (sp *ServerProxy) Done() <-chan struct{} {
	return sp.dispose.Done()
}

// Calls into the client proxy to establish bi-directional stream of messages
// that facilitate creation of new tunnel streams.
// Once the stream creation flow is established, the server proxy will listen for "StreamRef" messages
// from the client proxy (indicating the need for a new tunnel stream),
// and reply with "NewStreamResult" messages indicating stream creation success or failure.
func (sp *ServerProxy) streamsCreationWorker() {
	newStreamsConn, newStreamsConnErr := sp.clientProxy.NewStreamsConnection(sp.lifetimeCtx, grpc.WaitForReady(true))
	if newStreamsConnErr != nil {
		sp.log.Error(newStreamsConnErr, "Failed to establsh new streams connection with client proxy")
		sp.disposeOnce()
		return
	}

	// Stream completion worker goroutine.
	go func() {
		defer func() { _ = newStreamsConn.CloseSend() }()
		streamConnResults := sp.streamConnectionCreator.Results()

		for {
			select {

			case <-sp.lifetimeCtx.Done():
				sp.log.V(1).Info("Lifetime context is done; stopping streams completion worker")
				return

			case <-sp.dispose.Done():
				sp.log.V(1).Info("Server proxy is disposed; stopping streams completion worker")
				return

			case res, isOpen := <-streamConnResults:
				if !isOpen {
					sp.log.V(1).Info("Stream connection results channel is closed; stopping streams completion worker")
					return
				}

				sp.completeStream(res, newStreamsConn)
			}

		}
	}()

	b := grpcutil.StreamRetryBackoff()

	for {
		if sp.lifetimeCtx.Err() != nil || sp.dispose.IsDone() {
			sp.log.Info("Server proxy lifetime context is done or proxy is disposed; stopping streams creation worker")
			return
		}

		sr, recvErr := newStreamsConn.Recv()
		if grpcutil.IsStreamDoneErr(recvErr) {
			sp.log.Info("New streams connection is done, exiting...")
			sp.disposeOnce()
			return
		}

		if recvErr != nil {
			sp.log.Error(recvErr, "Failed to receive stream reference from client proxy")
			time.Sleep(b.NextBackOff())
			continue
		}

		b.Reset()
		// Only fails if the lifetime context is done, and we do not want to start new stream in that case.
		_ = sp.streamConnectionCreator.Enqueue(sr)
	}
}

func (sp *ServerProxy) createStreamConnections(_ context.Context, sr *proto.StreamRef) streamConnectionResult {
	res := streamConnectionResult{
		streamRef: sr,
	}
	tid := TunnelID(sr.GetTunnelId())
	streamID := sr.GetStreamId()

	sp.lock.Lock()
	td, found := sp.tunnels[tid]
	sp.lock.Unlock()
	if !found {
		tunnelNotFoundErr := fmt.Errorf("tunnel with ID %d does not exist", tid)
		sp.log.V(1).Error(tunnelNotFoundErr, "Ignoring tunnel reference from client proxy")
		res.tunnelNotFoundErr = tunnelNotFoundErr
		return res
	}

	res.streamLog = sp.log.WithValues(
		"TunnelID", tid, "StreamID", streamID,
		"ServerAddress", td.spec.GetServerAddress(),
		"ServerPort", td.spec.GetServerPort(),
		"ClientDataEndpointAddress", sp.clientDataEndpointAddress,
		"ClientDataEndpointPort", sp.clientDataEndpointPort,
	)

	var d net.Dialer
	streamCreationCtx, streamCreationCtxCancel := context.WithTimeout(sp.lifetimeCtx, 1*time.Minute)
	defer streamCreationCtxCancel()

	ap := networking.AddressAndPort(td.spec.GetServerAddress(), td.spec.GetServerPort())
	serverConn, serverDialErr := resiliency.RetryGetExponential(streamCreationCtx, func() (net.Conn, error) {
		serverDialCtx, serverDialCtxCancel := context.WithTimeout(streamCreationCtx, proxy.DefaultConnectionTimeout)
		defer serverDialCtxCancel()
		return d.DialContext(serverDialCtx, "tcp", ap)
	})
	if serverDialErr != nil {
		if sp.lifetimeCtx.Err() == nil {
			res.streamLog.Error(serverDialErr, "Error establishing connection to server")
		}
		res.serverDialErr = serverDialErr
		return res
	}
	res.streamLog.V(1).Info("Established connection to server")

	ap = networking.AddressAndPort(sp.clientDataEndpointAddress, sp.clientDataEndpointPort)
	clientDataConn, clientDataDialErr := resiliency.RetryGetExponential(streamCreationCtx, func() (net.Conn, error) {
		clientDataDialCtx, clientDataDialCtxCancel := context.WithTimeout(streamCreationCtx, proxy.DefaultConnectionTimeout)
		defer clientDataDialCtxCancel()
		return d.DialContext(clientDataDialCtx, "tcp", ap)
	})

	if clientDataDialErr != nil {
		if sp.lifetimeCtx.Err() == nil {
			res.streamLog.Error(clientDataDialErr, "Error establishing data connection to client proxy")
		}
		res.clientDataDialErr = clientDataDialErr
		_ = serverConn.Close()
		return res
	}
	res.streamLog.V(1).Info("Established data connection to client proxy")

	res.serverConn = serverConn
	res.clientDataConn = clientDataConn
	return res
}

func (sp *ServerProxy) completeStream(
	connRes streamConnectionResult,
	newStreamsConn grpc.BidiStreamingClient[proto.NewStreamResult, proto.StreamRef],
) {
	if connRes.tunnelNotFoundErr != nil {
		sp.reportConnectionError(connRes.streamRef, proto.StreamErr_STREAM_ERR_NONEXISTENT_TUNNEL, connRes.tunnelNotFoundErr, newStreamsConn)
		return
	}
	if connRes.serverDialErr != nil {
		sp.reportConnectionError(connRes.streamRef, proto.StreamErr_STREAM_ERR_SERVER_UNAVAILABLE, connRes.serverDialErr, newStreamsConn)
		return
	}
	if connRes.clientDataDialErr != nil {
		sp.reportConnectionError(connRes.streamRef, proto.StreamErr_STREAM_ERR_DATA_CONNECTION_FAILED, connRes.clientDataDialErr, newStreamsConn)
		return
	}

	// All connections are established successfully, so we can send the result to the client proxy.

	streamLog := connRes.streamLog
	res := &proto.NewStreamResult{
		StreamRef: connRes.streamRef,
	}
	resErr := newStreamsConn.Send(res)
	if resErr != nil {
		if !grpcutil.IsStreamDoneErr(resErr) {
			streamLog.Error(resErr, "Failed to send new stream result to client proxy; stream will not be established")
		} else {
			streamLog.V(1).Info("New streams connection is closing; stream will not be established")
		}
		_ = connRes.serverConn.Close()
		_ = connRes.clientDataConn.Close()
		return
	}

	tid := TunnelID(connRes.streamRef.GetTunnelId())
	streamID := connRes.streamRef.GetStreamId()

	streamCtx, streamCtxCancel := context.WithCancel(sp.lifetimeCtx)
	stopStream := func() {
		streamCtxCancel()
		_ = connRes.clientDataConn.Close()
		_ = connRes.serverConn.Close()
	}

	sp.lock.Lock()
	td, found := sp.tunnels[tid]
	if !found {
		sp.lock.Unlock()
		streamLog.Info("Tunnel deleted while establishing a new stream")
		stopStream()
		return
	}
	td.streams[StreamID(streamID)] = stopStream
	sp.lock.Unlock()

	connRes.streamLog.V(1).Info("New tunnel stream started")

	go func() {
		defer func() {
			stopStream()

			sp.lock.Lock()
			if td, found = sp.tunnels[tid]; found {
				delete(td.streams, StreamID(streamID))

				if len(td.streams) == 0 && td.deleted.Load() {
					streamLog.V(1).Info("Forgetting deleted tunnel with no connections..")
					delete(sp.tunnels, tid)
				}
			}

			sp.lock.Unlock()
		}()

		var buf bytes.Buffer
		_ = binary.Write(&buf, binary.BigEndian, tid)
		_ = binary.Write(&buf, binary.BigEndian, streamID)
		_, _ = buf.Write(td.spec.GetDataConnectionToken())
		sp.runTunnelStream(streamCtx, buf.Bytes(), connRes.serverConn, connRes.clientDataConn, streamLog)
	}()
}

func (sp *ServerProxy) reportConnectionError(
	sr *proto.StreamRef,
	code proto.StreamErr,
	err error,
	stream grpc.BidiStreamingClient[proto.NewStreamResult, proto.StreamRef],
) {
	res := &proto.NewStreamResult{
		StreamRef: sr,
		Failure: &proto.StreamFailure{
			Error:   grpcutil.EnumVal(code),
			Message: stdproto.String(err.Error()),
		},
	}

	responseErr := stream.Send(res)
	if responseErr != nil && !grpcutil.IsStreamDoneErr(responseErr) {
		sp.log.Error(responseErr, "Failed to send connection failure response to client proxy",
			"TunnelID", sr.GetTunnelId(),
			"StreamID", sr.GetStreamId(),
			"Code", code,
			"Error", err.Error(),
		)
	}
}

func (sp *ServerProxy) runTunnelStream(
	ctx context.Context,
	preamble []byte,
	serverConn net.Conn,
	clientDataConn net.Conn,
	streamLog logr.Logger,
) {
	// Write the tunnel ID, stream ID, and the authentication token as the first data to the data connection.
	_, writeErr := clientDataConn.Write(preamble)
	if writeErr != nil {
		streamLog.Error(writeErr, "Failed to write tunnel ID and connection ID to client data connection")
		return
	}

	sr, cdr := proxy.StreamNetworkData(ctx, serverConn, clientDataConn)

	if sr.ReadError != nil {
		streamLog.Error(sr.ReadError, "The server connection encountered a read error")
	} else if sr.WriteError != nil {
		streamLog.V(1).Error(sr.WriteError, "The server connection encountered a write error")
	} else {
		streamLog.V(1).Info("Server connection is done")
	}

	if cdr.WriteError != nil {
		streamLog.V(1).Error(cdr.WriteError, "The client data connection encountered a write error")
	} else if cdr.ReadError != nil {
		streamLog.V(1).Error(cdr.ReadError, "The client data connection encountered a read error")
	} else {
		streamLog.V(1).Info("Client data connection is done")
	}
}

func (sp *ServerProxy) shutDownAllTunnels() {
	sp.lock.Lock()
	tunnels := sp.tunnels
	sp.tunnels = make(map[TunnelID]*tunnelData[stopStreamFn])
	sp.lock.Unlock()

	for tid, td := range tunnels {
		sp.log.V(1).Info("Shutting down tunnel", "TunnelID", tid, "TunnelSpec", td.spec.LogString())

		for streamID, stopStream := range td.streams {
			if stopStream != nil {
				sp.log.V(1).Info("Stopping tunnel stream", "TunnelID", tid, "StreamID", streamID)
				stopStream()
			}
		}
	}
}

func (sp *ServerProxy) disposeOnce() {
	if !sp.dispose.TryTake() {
		return
	}
	defer sp.dispose.Complete(struct{}{})

	sp.log.V(1).Info("Disposing server-side proxy...")
	sp.requestShutdown()
	sp.shutDownAllTunnels()
}

var _ proto.TunnelControlServer = &ServerProxy{}
