/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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
	tunnelSpec        *proto.TunnelSpec
	serverConn        net.Conn    // The connection to the server endpoint (if successful).
	clientDataConn    net.Conn    // The connection to the client proxy data endpoint (if successful).
	tunnelNotFoundErr error       // The error indicating that the referenced tunnel (part of stream reference) was not found.
	serverDialErr     error       // The error that occurred while dialing the server endpoint.
	clientDataDialErr error       // The error that occurred while dialing the client proxy data endpoint.
	streamLog         logr.Logger // The logger to be used for the stream.
	stopCleanup       func() bool // Transfers pending sockets out of the control attempt's lifetime.
}

func (res streamConnectionResult) close() {
	if res.stopCleanup != nil {
		res.stopCleanup()
	}
	if res.serverConn != nil {
		_ = res.serverConn.Close()
	}
	if res.clientDataConn != nil {
		_ = res.clientDataConn.Close()
	}
}

// ServerProxyConfig controls recovery of the connection used to establish new streams.
type ServerProxyConfig struct {
	// Defaults to the standard DCP retry budget.
	ReconnectTimeout time.Duration
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

	reconnectTimeout time.Duration
	failure          *concurrency.ValuePromise[error]
}

func NewServerProxy(
	ctx context.Context,
	clientProxy proto.TunnelControlClient,
	clientDataEndpointAddress string,
	clientDataEndpointPort int32,
	requestShutdown func(),
	config ServerProxyConfig,
	log logr.Logger,
) *ServerProxy {
	if config.ReconnectTimeout < 0 {
		panic("ServerProxyConfig.ReconnectTimeout must not be negative")
	}
	if config.ReconnectTimeout == 0 {
		config.ReconnectTimeout = resiliency.DefaultRetryTimeout
	}
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
		reconnectTimeout:          config.ReconnectTimeout,
		failure:                   concurrency.NewValuePromise[error](),
	}

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
	var tunnelSpec *proto.TunnelSpec
	if found {
		tunnelSpec = td.spec
	}
	sp.lock.Unlock()
	if found {
		sp.log.V(1).Info("Deleting tunnel..", "tunnel_spec", tunnelSpec.LogString())
	} else {
		sp.log.V(1).Info("Deleting tunnel missing from server-side bookkeeping", "TunnelID", tid)
	}

	// Always call the client proxy to facilitate retries, including after the
	// server-side entry has drained its last established stream.
	_, clientDeleteErr := sp.clientProxy.DeleteTunnel(ctx, tr)

	// Delete the tunnel from the server-side proxy even if the client proxy deletion fails.
	sp.lock.Lock()
	td, found = sp.tunnels[tid]
	if !found || td.deleted.Load() {
		sp.lock.Unlock()
		sp.log.V(1).Info("Tunnel already deleted", "tunnel_id", tid)
	} else {
		deletedSpec := td.spec
		td.accepting.Store(false)
		td.deleted.Store(true)
		sp.lock.Unlock()
		sp.log.V(1).Info("Tunnel deleted", "tunnel_spec", deletedSpec.LogString())
	}

	if clientDeleteErr != nil {
		sp.log.Error(clientDeleteErr, "Failed to delete tunnel with client proxy", "TunnelID", tid)
		return nil, status.Errorf(codes.Internal, "failed to delete tunnel with client proxy: %s", clientDeleteErr.Error())
	}
	return &emptypb.Empty{}, nil
}

// UpdateTunnel changes an existing tunnel without replacing its client-facing listener or ID.
func (sp *ServerProxy) UpdateTunnel(ctx context.Context, update *proto.TunnelUpdate) (*emptypb.Empty, error) {
	validationErr := ensureValidTunnelUpdate(update)
	if validationErr != nil {
		return nil, validationErr
	}
	if sp.dispose.IsDone() {
		return nil, status.Error(codes.FailedPrecondition, errMsgProxyDisposed)
	}

	tid := TunnelID(update.GetTunnelRef().GetTunnelId())
	sp.lock.Lock()
	td, found := sp.tunnels[tid]
	if !found || td.deleted.Load() {
		sp.lock.Unlock()
		return nil, status.Errorf(codes.NotFound, "tunnel with ID %d does not exist", tid)
	}

	if update.GetAcceptNewStreams() {
		// Fail closed while changing the backend. The client may still be accepting
		// from an earlier Ready state, so suspend both sides before enabling either
		// side with the replacement target.
		td.accepting.Store(false)
		sp.lock.Unlock()
		suspendUpdate := &proto.TunnelUpdate{
			TunnelRef:        stdproto.CloneOf(update.GetTunnelRef()),
			AcceptNewStreams: stdproto.Bool(false),
		}
		_, clientSuspendErr := sp.clientProxy.UpdateTunnel(ctx, suspendUpdate, grpc.WaitForReady(true))
		if clientSuspendErr != nil {
			sp.log.Error(clientSuspendErr, "Failed to suspend tunnel with client proxy before updating its backend", "TunnelID", tid)
			return nil, status.Errorf(codes.Internal, "failed to suspend tunnel with client proxy before updating its backend: %s", clientSuspendErr.Error())
		}

		sp.lock.Lock()
		td, found = sp.tunnels[tid]
		if !found || td.deleted.Load() {
			sp.lock.Unlock()
			return nil, status.Errorf(codes.NotFound, "tunnel with ID %d does not exist", tid)
		}
		updatedSpec := stdproto.CloneOf(td.spec)
		updatedSpec.ServerAddress = stdproto.String(update.GetServerAddress())
		updatedSpec.ServerPort = stdproto.Int32(update.GetServerPort())
		td.spec = updatedSpec
		td.accepting.Store(true)
	} else {
		// Disable the server side first. If forwarding fails, any connection still
		// accepted by the client side is rejected here instead of reaching a stale backend.
		td.accepting.Store(false)
	}
	sp.lock.Unlock()

	_, clientUpdateErr := sp.clientProxy.UpdateTunnel(ctx, update, grpc.WaitForReady(true))
	if clientUpdateErr != nil {
		if update.GetAcceptNewStreams() {
			// A unary error is ambiguous: the client may have applied the resume
			// before its response was lost. Keep the server side suspended so any
			// accepted client connection still fails closed until an idempotent retry.
			sp.lock.Lock()
			current := sp.tunnels[tid]
			if current == td && !current.deleted.Load() {
				current.accepting.Store(false)
			}
			sp.lock.Unlock()
		}
		sp.log.Error(
			clientUpdateErr,
			"Failed to update tunnel with client proxy",
			"TunnelID", tid,
			"AcceptNewStreams", update.GetAcceptNewStreams(),
		)
		return nil, status.Errorf(codes.Internal, "failed to update tunnel with client proxy: %s", clientUpdateErr.Error())
	}

	sp.log.V(1).Info(
		"Tunnel updated",
		"TunnelID", tid,
		"AcceptNewStreams", update.GetAcceptNewStreams(),
		"ServerAddress", update.GetServerAddress(),
		"ServerPort", update.GetServerPort(),
	)
	return &emptypb.Empty{}, nil
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

// Err reports a failure requiring the controller to recreate the proxy pair.
func (sp *ServerProxy) Err() error {
	if sp.failure.IsSet() {
		return sp.failure.Get()
	}
	return nil
}

// Calls into the client proxy to establish bi-directional stream of messages
// that facilitate creation of new tunnel streams.
// Once the stream creation flow is established, the server proxy will listen for "StreamRef" messages
// from the client proxy (indicating the need for a new tunnel stream),
// and reply with "NewStreamResult" messages indicating stream creation success or failure.
func (sp *ServerProxy) streamsCreationWorker() {
	retryPolicy := resiliency.NewExponentialBackoff()
	// The establishment watchdog also bounds blocking gRPC calls. Backoff alone
	// only limits time between attempts, not WaitForReady or Header.
	retryPolicy.MaxElapsedTime = 0
	deadline := time.Now().Add(sp.reconnectTimeout)
	for {
		if sp.lifetimeCtx.Err() != nil || sp.dispose.IsDone() {
			return
		}
		ready, connectionErr := sp.runStreamsConnection(deadline)
		if sp.lifetimeCtx.Err() != nil || sp.dispose.IsDone() {
			return
		}
		if ready {
			deadline = time.Now().Add(sp.reconnectTimeout)
			retryPolicy.Reset()
		}
		expired := !time.Now().Before(deadline)
		if expired {
			connectionErr = errors.Join(context.DeadlineExceeded, connectionErr)
		}
		if expired || !retryableControlConnectionError(connectionErr) {
			failure := fmt.Errorf("tunnel control stream recovery failed: %w", connectionErr)
			sp.failure.Set(failure)
			sp.log.Error(failure, "Stopping server proxy so the controller can recreate the proxy pair")
			sp.disposeOnce()
			return
		}
		code := status.Code(connectionErr)
		if errors.Is(connectionErr, context.Canceled) {
			code = codes.Canceled
		} else if errors.Is(connectionErr, context.DeadlineExceeded) {
			code = codes.DeadlineExceeded
		}
		sp.log.Info("Tunnel control stream interrupted; reconnecting",
			"Error", connectionErr, "Code", code.String())
		delay := min(retryPolicy.NextBackOff(), time.Until(deadline))
		select {
		case <-sp.lifetimeCtx.Done():
			return
		case <-sp.dispose.Done():
			return
		case <-time.After(delay):
		}
	}
}

func retryableControlConnectionError(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.Unauthenticated, codes.PermissionDenied, codes.Unimplemented, codes.FailedPrecondition:
		return false
	default:
		return true
	}
}

func (sp *ServerProxy) runStreamsConnection(deadline time.Time) (bool, error) {
	if !time.Now().Before(deadline) {
		return false, context.DeadlineExceeded
	}
	attemptCtx, cancelAttempt := context.WithCancelCause(sp.lifetimeCtx)
	defer cancelAttempt(context.Canceled)
	establishmentWatchdog := time.AfterFunc(time.Until(deadline), func() {
		cancelAttempt(context.DeadlineExceeded)
	})
	defer establishmentWatchdog.Stop()

	newStreamsConn, connectErr := sp.clientProxy.NewStreamsConnection(attemptCtx, grpc.WaitForReady(true))
	if connectErr != nil {
		if attemptCtx.Err() != nil {
			return false, context.Cause(attemptCtx)
		}
		return false, connectErr
	}
	// Headers acknowledge that the client owns this RPC, even when no new data
	// streams are requested. Never attach an establishment deadline to the
	// healthy long-lived RPC: https://grpc.io/docs/guides/deadlines/.
	headers, headerErr := newStreamsConn.Header()
	if headerErr != nil {
		if attemptCtx.Err() != nil {
			return false, context.Cause(attemptCtx)
		}
		return false, headerErr
	}
	if headers == nil {
		_, recvErr := newStreamsConn.Recv()
		if attemptCtx.Err() != nil {
			return false, context.Cause(attemptCtx)
		}
		if recvErr == nil {
			return false, status.Error(codes.DataLoss, "tunnel control stream ended before readiness without reporting a status")
		}
		return false, recvErr
	}
	if !establishmentWatchdog.Stop() {
		return false, context.DeadlineExceeded
	}
	if attemptCtx.Err() != nil {
		return false, context.Cause(attemptCtx)
	}
	if values := headers.Get(controlStreamReadyHeader); len(values) != 1 || values[0] != "true" {
		return false, status.Error(codes.FailedPrecondition, "client did not acknowledge ownership of the tunnel control stream")
	}
	sp.log.V(1).Info("Tunnel control stream is ready")

	// A control attempt owns its preparation work. Cancellation closes only
	// pending sockets; completeStream transfers established sockets to the
	// proxy lifetime so a subsequent control reconnect cannot abort them.
	results := make(chan streamConnectionResult, 1)
	work := resiliency.NewWorkQueue(attemptCtx, resiliency.DefaultConcurrency)
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		for {
			select {
			case <-attemptCtx.Done():
				return
			case result, open := <-results:
				if !open {
					cancelAttempt(errors.New("stream-creation results channel closed unexpectedly"))
					return
				}
				if attemptCtx.Err() != nil {
					result.close()
					return
				}
				if completeErr := sp.completeStream(result, newStreamsConn); completeErr != nil {
					cancelAttempt(completeErr)
					return
				}
			}
		}
	}()
	defer func() {
		cancelAttempt(context.Canceled)
		<-senderDone
		// gRPC forbids CloseSend concurrently with Send.
		_ = newStreamsConn.CloseSend()
	}()

	for {
		streamRef, recvErr := newStreamsConn.Recv()
		if recvErr != nil {
			if attemptCtx.Err() != nil {
				return true, context.Cause(attemptCtx)
			}
			return true, recvErr
		}
		enqueueErr := work.Enqueue(func(ctx context.Context) {
			result := sp.createStreamConnections(ctx, streamRef)
			select {
			case results <- result:
			case <-ctx.Done():
				result.close()
			}
		})
		if enqueueErr != nil {
			return true, enqueueErr
		}
	}
}

func (sp *ServerProxy) createStreamConnections(ctx context.Context, sr *proto.StreamRef) streamConnectionResult {
	res := streamConnectionResult{
		streamRef: sr,
	}
	tid := TunnelID(sr.GetTunnelId())
	streamID := sr.GetStreamId()

	sp.lock.Lock()
	td, found := sp.tunnels[tid]
	var tunnelSpec *proto.TunnelSpec
	if found {
		tunnelSpec = td.spec
	}
	sp.lock.Unlock()
	if !found || td.deleted.Load() {
		tunnelNotFoundErr := fmt.Errorf("tunnel with ID %d does not exist", tid)
		sp.log.V(1).Error(tunnelNotFoundErr, "Ignoring tunnel reference from client proxy")
		res.tunnelNotFoundErr = tunnelNotFoundErr
		return res
	}
	if !td.accepting.Load() {
		tunnelSuspendedErr := fmt.Errorf("tunnel with ID %d is not accepting new streams", tid)
		res.streamLog = sp.log.WithValues("TunnelID", tid, "StreamID", streamID)
		res.serverDialErr = tunnelSuspendedErr
		return res
	}
	res.tunnelSpec = tunnelSpec

	res.streamLog = sp.log.WithValues(
		"TunnelID", tid, "StreamID", streamID,
		"ServerAddress", tunnelSpec.GetServerAddress(),
		"ServerPort", tunnelSpec.GetServerPort(),
		"ClientDataEndpointAddress", sp.clientDataEndpointAddress,
		"ClientDataEndpointPort", sp.clientDataEndpointPort,
	)

	var d net.Dialer
	streamCreationCtx, streamCreationCtxCancel := context.WithTimeout(ctx, 1*time.Minute)
	defer streamCreationCtxCancel()

	ap := networking.AddressAndPort(tunnelSpec.GetServerAddress(), tunnelSpec.GetServerPort())
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
	res.stopCleanup = context.AfterFunc(ctx, func() {
		_ = serverConn.Close()
		_ = clientDataConn.Close()
	})
	return res
}

func (sp *ServerProxy) completeStream(
	connRes streamConnectionResult,
	newStreamsConn grpc.BidiStreamingClient[proto.NewStreamResult, proto.StreamRef],
) error {
	if connRes.tunnelNotFoundErr != nil {
		return sp.reportConnectionError(connRes.streamRef, proto.StreamErr_STREAM_ERR_NONEXISTENT_TUNNEL, connRes.tunnelNotFoundErr, newStreamsConn)
	}
	if connRes.serverDialErr != nil {
		return sp.reportConnectionError(connRes.streamRef, proto.StreamErr_STREAM_ERR_SERVER_UNAVAILABLE, connRes.serverDialErr, newStreamsConn)
	}
	if connRes.clientDataDialErr != nil {
		return sp.reportConnectionError(connRes.streamRef, proto.StreamErr_STREAM_ERR_DATA_CONNECTION_FAILED, connRes.clientDataDialErr, newStreamsConn)
	}

	tid := TunnelID(connRes.streamRef.GetTunnelId())
	streamID := connRes.streamRef.GetStreamId()
	streamLog := connRes.streamLog
	streamCtx, streamCtxCancel := context.WithCancel(sp.lifetimeCtx)
	stopStream := func() {
		streamCtxCancel()
		_ = connRes.clientDataConn.Close()
		_ = connRes.serverConn.Close()
	}

	tunnelSpec, registrationErr := sp.registerTunnelStream(
		tid,
		StreamID(streamID),
		connRes.tunnelSpec,
		stopStream,
	)
	if registrationErr != nil {
		streamCtxCancel()
		connRes.close()
		return sp.reportConnectionError(connRes.streamRef, proto.StreamErr_STREAM_ERR_SERVER_UNAVAILABLE, registrationErr, newStreamsConn)
	}

	removeRegisteredStream := func() {
		sp.lock.Lock()
		current, currentFound := sp.tunnels[tid]
		if currentFound {
			delete(current.streams, StreamID(streamID))
			if len(current.streams) == 0 && current.deleted.Load() {
				delete(sp.tunnels, tid)
			}
		}
		sp.lock.Unlock()
		stopStream()
	}

	if !connRes.stopCleanup() {
		removeRegisteredStream()
		return context.Canceled
	}

	// Registration is linearized before success is reported. Suspension preserves
	// registered streams, so a successful response always has an owner that will
	// either start the stream or clean it up if the send fails.
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
		removeRegisteredStream()
		return resErr
	}

	connRes.streamLog.V(1).Info("New tunnel stream started")

	go func() {
		defer func() {
			removeRegisteredStream()
		}()

		var buf bytes.Buffer
		_ = binary.Write(&buf, binary.BigEndian, tid)
		_ = binary.Write(&buf, binary.BigEndian, streamID)
		_, _ = buf.Write(tunnelSpec.GetDataConnectionToken())
		sp.runTunnelStream(streamCtx, buf.Bytes(), connRes.serverConn, connRes.clientDataConn, streamLog)
	}()
	return nil
}

func (sp *ServerProxy) registerTunnelStream(
	tunnelID TunnelID,
	streamID StreamID,
	expectedSpec *proto.TunnelSpec,
	stopStream stopStreamFn,
) (*proto.TunnelSpec, error) {
	sp.lock.Lock()
	defer sp.lock.Unlock()

	tunnel, found := sp.tunnels[tunnelID]
	if !found || tunnel.deleted.Load() || !tunnel.accepting.Load() {
		return nil, fmt.Errorf("tunnel with ID %d is not accepting new streams", tunnelID)
	}
	if tunnel.spec != expectedSpec {
		return nil, fmt.Errorf("tunnel with ID %d changed its server endpoint while the stream was being established", tunnelID)
	}

	tunnel.streams[streamID] = stopStream
	return tunnel.spec, nil
}

func (sp *ServerProxy) reportConnectionError(
	sr *proto.StreamRef,
	code proto.StreamErr,
	err error,
	stream grpc.BidiStreamingClient[proto.NewStreamResult, proto.StreamRef],
) error {
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
	return responseErr
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
