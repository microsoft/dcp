/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	stdproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/proxy"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/security"
	"github.com/microsoft/dcp/pkg/slices"
)

type streamInfo struct {
	// The client connection (the original connection from the client to the client-side proxy).
	clientConn net.Conn

	// The connection that the server-side proxy made to facilitate the tunnel.
	proxyConn net.Conn

	// The context for the lifetime of the stream.
	streamCtx context.Context

	// The cancellation function for the stream context.
	streamCtxCancel context.CancelFunc

	// The timestamp when the stream creation was started. Used by the scavenger goroutine
	// to detect and clean up half-open streams.
	created time.Time
}

func (si *streamInfo) dispose() {
	if si.streamCtxCancel != nil {
		si.streamCtxCancel()
		si.streamCtxCancel = nil
	}
	if si.clientConn != nil {
		_ = si.clientConn.Close()
		si.clientConn = nil
	}
	if si.proxyConn != nil {
		_ = si.proxyConn.Close()
		si.proxyConn = nil
	}
}

// clientTunnelData holds data about a tunnel that is managed by client-side proxy.
type clientTunnelData struct {
	tunnelData[*streamInfo]

	// Requested client-proxy listener parameters retained for idempotency checks.
	clientProxyAddress           string
	requestedClientProxyPort     int32
	allowClientProxyPortFallback bool

	// The listener for clients to create new tunnel connections.
	listener net.Listener

	// The lifetime context for the tunnel.
	tunnelCtx context.Context

	// The cancellation function for the context for the lifetime of the tunnel.
	tunnelCtxCancel context.CancelFunc
}

// ClientProxyConfig controls listener binding and incoming data-connection deadlines.
type ClientProxyConfig struct {
	// Bounds how long an incoming data connection may stall before sending its preamble.
	// Defaults to defaultDataConnectionPreambleTimeout.
	DataConnectionPreambleTimeout time.Duration

	// Optional listener factory used to open tunnel listeners; defaults to net.ListenConfig.Listen.
	Listen func(context.Context, string, string) (net.Listener, error)
}

// The client-side proxy of the DCP reverse network tunnel.
type ClientProxy struct {
	// Need to embed the following to ensure gRPC forward compatibility.
	proto.UnimplementedTunnelControlServer

	// The lifetime context of the client-side proxy.
	lifetimeCtx context.Context

	// The logger for the client-side proxy.
	log logr.Logger

	// The network listener for the data endpoint of the client-side proxy.
	// This listener is used to accept incoming connections from the server-side proxy
	// that complete tunnel streams.
	dataListener net.Listener

	// Function to request shutdown of the client-side proxy.
	requestShutdown func()

	// Opens tunnel listeners. Tests replace this at the network-listener boundary.
	listen func(context.Context, string, string) (net.Listener, error)

	// Bounds how long an incoming data connection may stall before sending its authentication preamble.
	preambleTimeout time.Duration

	// Data about tunnels managed by this proxy, indexed by tunnel ID.
	tunnels map[TunnelID]*clientTunnelData

	// Maps each active logical tunnel name to its assigned tunnel ID for idempotency.
	tunnelsByName map[string]TunnelID

	// Serializes PrepareTunnel and name-based DeleteTunnel for the same logical tunnel name.
	nameLeases *concurrency.LeaseMap[string]

	// A channel that carries new stream requests. It is used for communication between
	// goroutines accepting new client connections and the goroutine that interacts
	// with the server-side proxy to start a new tunnel stream.
	newStreamsReqs *concurrency.UnboundedChan[*proto.StreamRef]

	// Mutex to protect access to internal proxy state (primarily the tunnels map).
	lock *sync.Mutex

	// One RPC at a time may consume the pending stream queue.
	controlStreamLock *concurrency.ContextAwareLock

	// True if the proxy has been shut down.
	disposed *atomic.Bool

	// A channel that is closed when the proxy is disposed.
	done chan struct{}
}

func NewClientProxy(
	ctx context.Context,
	dataListener net.Listener,
	requestShutdown func(),
	config ClientProxyConfig,
	log logr.Logger,
) *ClientProxy {
	if config.DataConnectionPreambleTimeout < 0 {
		panic("ClientProxyConfig.DataConnectionPreambleTimeout must not be negative")
	}
	if config.DataConnectionPreambleTimeout == 0 {
		config.DataConnectionPreambleTimeout = defaultDataConnectionPreambleTimeout
	}
	listenFn := config.Listen
	if listenFn == nil {
		listenConfig := &net.ListenConfig{}
		listenFn = listenConfig.Listen
	}
	cp := &ClientProxy{
		lifetimeCtx:       ctx,
		dataListener:      dataListener,
		requestShutdown:   requestShutdown,
		listen:            listenFn,
		preambleTimeout:   config.DataConnectionPreambleTimeout,
		log:               log,
		tunnels:           make(map[TunnelID]*clientTunnelData),
		tunnelsByName:     make(map[string]TunnelID),
		nameLeases:        concurrency.NewLeaseMap[string](),
		newStreamsReqs:    concurrency.NewUnboundedChan[*proto.StreamRef](ctx),
		lock:              &sync.Mutex{},
		controlStreamLock: concurrency.NewContextAwareLock(),
		disposed:          &atomic.Bool{},
		done:              make(chan struct{}),
	}

	context.AfterFunc(ctx, func() { _ = cp.disposeOnce() })
	go cp.handleProxyConnections()
	go cp.scavengeHalfOpenStreams()

	return cp
}

// Prepares the proxy pair for tunneling the traffic.
// Upon success, the client-side proxy is listening to client connections and ready to tunnel traffic.
func (cp *ClientProxy) PrepareTunnel(ctx context.Context, tr *proto.TunnelReq) (*proto.TunnelSpec, error) {
	if cp.disposed.Load() {
		return nil, status.Error(codes.FailedPrecondition, errMsgProxyDisposed)
	}

	validationErr := ensureValidTunnelRequest(tr)
	if validationErr != nil {
		return nil, validationErr
	}

	// Holding the lease until the tunnel is registered (or its listener is closed) lets a retry or a
	// name-based deletion observe the outcome of a request whose caller has already given up on it.
	nameLease, nameLeaseErr := cp.nameLeases.Acquire(ctx, tr.GetTunnelName())
	if nameLeaseErr != nil {
		return nil, status.FromContextError(nameLeaseErr).Err()
	}
	defer nameLease.Release()

	// Check if we already have an active tunnel for this logical tunnel name (idempotency).
	cp.lock.Lock()
	existingSpec, conflictErr := cp.findExistingTunnelByName(tr)
	cp.lock.Unlock()
	if conflictErr != nil {
		return nil, conflictErr
	}
	if existingSpec != nil {
		cp.log.V(1).Info("Returning existing tunnel for request", "TunnelID", existingSpec.GetTunnelRef().GetTunnelId(), "TunnelSpec", existingSpec.LogString())
		return existingSpec, nil
	}

	effectiveAddresses, lookupErr := networking.LookupIP(tr.GetClientProxyAddress())
	if lookupErr != nil {
		return nil, status.Errorf(codes.Internal, "failed to resolve client proxy address %s: %s", tr.GetClientProxyAddress(), lookupErr.Error())
	}

	tunnelListener, tlErr := cp.listenForTunnel(ctx, tr)
	if tlErr != nil {
		cp.log.Error(tlErr, "Failed to listen on client proxy address",
			"Address", tr.GetClientProxyAddress(),
			"Port", tr.GetClientProxyPort(),
		)
		return nil, status.Errorf(codes.Internal, "failed to listen on client proxy address %s:%d: %s", tr.GetClientProxyAddress(), tr.GetClientProxyPort(), tlErr.Error())
	}

	authToken, tokenErr := security.MakeBearerToken()
	if tokenErr != nil {
		_ = tunnelListener.Close()
		cp.log.Error(tokenErr, "Failed to generate authentication token for tunnel")
		return nil, status.Errorf(codes.Internal, "failed to generate authentication token for tunnel: %s", tokenErr.Error())
	}

	// This is looking good, let's allocate a new tunnel ID and prepare the tunnel spec.

	listenerAddr := tunnelListener.Addr().(*net.TCPAddr)
	tid := TunnelID(atomic.AddUint32((*uint32)(&latestTunnelID), 1))
	tunnelCtx, tunnelCtxCancel := context.WithCancel(cp.lifetimeCtx)
	ts := &proto.TunnelSpec{
		TunnelRef: &proto.TunnelRef{
			TunnelId: stdproto.Uint32(uint32(tid)),
		},
		ServerAddress:        stdproto.String(tr.GetServerAddress()),
		ServerPort:           stdproto.Int32(int32(tr.GetServerPort())),
		ClientProxyAddresses: slices.Map[string](effectiveAddresses, func(ip net.IP) string { return networking.IpToString(ip) }),
		ClientProxyPort:      stdproto.Int32(int32(listenerAddr.Port)),
		DataConnectionToken:  authToken,
	}
	td := newTunnelData[*streamInfo](ts)
	td.tunnelName = tr.GetTunnelName()
	ctd := &clientTunnelData{
		tunnelData:                   *td,
		clientProxyAddress:           tr.GetClientProxyAddress(),
		requestedClientProxyPort:     tr.GetClientProxyPort(),
		allowClientProxyPortFallback: tr.GetAllowClientProxyPortFallback(),
		listener:                     tunnelListener,
		tunnelCtx:                    tunnelCtx,
		tunnelCtxCancel:              tunnelCtxCancel,
	}
	cp.lock.Lock()
	if cp.disposed.Load() || cp.lifetimeCtx.Err() != nil {
		cp.lock.Unlock()
		tunnelCtxCancel()
		_ = tunnelListener.Close()
		return nil, status.Error(codes.FailedPrecondition, errMsgProxyDisposed)
	}
	if requestErr := ctx.Err(); requestErr != nil {
		// Nobody will learn about a tunnel registered for an abandoned request; release its port instead.
		cp.lock.Unlock()
		tunnelCtxCancel()
		_ = tunnelListener.Close()
		return nil, status.FromContextError(requestErr).Err()
	}
	cp.tunnels[tid] = ctd
	cp.tunnelsByName[tr.GetTunnelName()] = tid
	cp.lock.Unlock()

	go cp.handleClientConnections(tunnelCtx, tid, tunnelListener)
	cp.log.V(1).Info("Tunnel prepared", "TunnelSpec", ts.LogString())
	return ts, nil
}

// listenForTunnel binds the listener for a new tunnel: on the exact requested client proxy port,
// or, when the request allows fallback or asks for no specific port, on the preferred port if it
// is usable and otherwise on a random port that the request does not exclude.
func (cp *ClientProxy) listenForTunnel(ctx context.Context, tr *proto.TunnelReq) (net.Listener, error) {
	preferredPort := tr.GetClientProxyPort()
	if !tr.ExcludedClientProxyPortsApply() {
		return cp.listen(ctx, "tcp", networking.AddressAndPort(tr.GetClientProxyAddress(), preferredPort))
	}

	excludedPorts := make(map[int32]struct{}, len(tr.GetExcludedClientProxyPorts()))
	for _, excludedPort := range tr.GetExcludedClientProxyPorts() {
		excludedPorts[excludedPort] = struct{}{}
	}

	if preferredPort != 0 {
		if _, excluded := excludedPorts[preferredPort]; excluded {
			cp.log.V(1).Info("Preferred tunnel port is reserved; selecting a random port", "Port", preferredPort)
		} else {
			preferredListener, preferredListenErr := cp.listen(
				ctx,
				"tcp",
				networking.AddressAndPort(tr.GetClientProxyAddress(), preferredPort),
			)
			if preferredListenErr == nil {
				return preferredListener, nil
			}
			cp.log.V(1).Info("Preferred tunnel port is unavailable; selecting a random port",
				"Port", preferredPort,
				"Error", preferredListenErr,
			)
		}
	}

	// Keep rejected listeners bound until a usable port is selected. This prevents
	// the OS from repeatedly returning the same excluded port and bounds the
	// number of attempts by the number of unique exclusions plus one.
	rejectedListeners := make([]net.Listener, 0, len(excludedPorts))
	defer func() {
		for _, listener := range rejectedListeners {
			_ = listener.Close()
		}
	}()

	for {
		tunnelListener, listenErr := cp.listen(
			ctx,
			"tcp",
			networking.AddressAndPort(tr.GetClientProxyAddress(), 0),
		)
		if listenErr != nil {
			return nil, listenErr
		}

		selectedPort := int32(tunnelListener.Addr().(*net.TCPAddr).Port)
		if _, excluded := excludedPorts[selectedPort]; !excluded {
			return tunnelListener, nil
		}

		cp.log.V(1).Info("Random tunnel port is reserved; selecting another port", "Port", selectedPort)
		rejectedListeners = append(rejectedListeners, tunnelListener)
	}
}

// Gracefully deletes the tunnel identified by tunnel ID, or by tunnel name when tunnel_id is zero.
// Existing streams will be preserved, but no new streams will be allowed.
func (cp *ClientProxy) DeleteTunnel(ctx context.Context, tr *proto.TunnelRef) (*emptypb.Empty, error) {
	if tr == nil || (tr.GetTunnelId() == uint32(invalidTunnelID) && tr.GetTunnelName() == "") {
		return nil, status.Errorf(codes.InvalidArgument, "bad request: tunnel ID or tunnel name must be specified")
	}

	if tunnelName := tr.GetTunnelName(); tunnelName != "" {
		nameLease, nameLeaseErr := cp.nameLeases.Acquire(ctx, tunnelName)
		if nameLeaseErr != nil {
			return nil, status.FromContextError(nameLeaseErr).Err()
		}
		defer nameLease.Release()
	}

	cp.lock.Lock()
	defer cp.lock.Unlock()

	tid := TunnelID(tr.GetTunnelId())
	if tid == invalidTunnelID {
		var foundByName bool
		tid, foundByName = cp.tunnelsByName[tr.GetTunnelName()]
		if !foundByName {
			return &emptypb.Empty{}, nil
		}
	}

	td, found := cp.tunnels[tid]
	if !found {
		// Do not error (idempotency)
		return &emptypb.Empty{}, nil
	}

	td.accepting.Store(false)
	td.deleted.Store(true)

	if cp.tunnelsByName[td.tunnelName] == tid {
		delete(cp.tunnelsByName, td.tunnelName)
	}
	_ = td.listener.Close()
	if len(td.streams) == 0 {
		td.tunnelCtxCancel()
		delete(cp.tunnels, tid)
	}

	cp.log.V(1).Info("Tunnel deleted", "TunnelID", tid)
	return &emptypb.Empty{}, nil
}

// UpdateTunnel changes whether an existing tunnel accepts new streams and, when
// resuming, records the current server endpoint without replacing its listener.
func (cp *ClientProxy) UpdateTunnel(_ context.Context, update *proto.TunnelUpdate) (*emptypb.Empty, error) {
	validationErr := ensureValidTunnelUpdate(update)
	if validationErr != nil {
		return nil, validationErr
	}
	if cp.disposed.Load() {
		return nil, status.Error(codes.FailedPrecondition, errMsgProxyDisposed)
	}

	tid := TunnelID(update.GetTunnelRef().GetTunnelId())
	cp.lock.Lock()
	defer cp.lock.Unlock()

	td, found := cp.tunnels[tid]
	if !found || td.deleted.Load() {
		return nil, status.Errorf(codes.NotFound, "tunnel with ID %d does not exist", tid)
	}

	if update.GetAcceptNewStreams() {
		updatedSpec := stdproto.CloneOf(td.spec)
		updatedSpec.ServerAddress = stdproto.String(update.GetServerAddress())
		updatedSpec.ServerPort = stdproto.Int32(update.GetServerPort())
		td.spec = updatedSpec
	}
	td.accepting.Store(update.GetAcceptNewStreams())
	cp.log.V(1).Info(
		"Tunnel updated",
		"TunnelID", tid,
		"AcceptNewStreams", update.GetAcceptNewStreams(),
		"ServerAddress", update.GetServerAddress(),
		"ServerPort", update.GetServerPort(),
	)
	return &emptypb.Empty{}, nil
}

// Shuts down the client-side proxy. This will close all tunnels and their streams,
// and stop accepting new client connections.
func (cp *ClientProxy) Shutdown(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	disposed := cp.disposeOnce()
	if disposed {
		cp.log.V(1).Info("Client-side tunnel proxy shutdown complete")
	}
	return &emptypb.Empty{}, nil
}

// inflightStreams tracks StreamRef handshakes sent on a single NewStreamsConnection RPC
// so out-of-order server replies can be matched and incomplete handshakes can be cleaned up on disconnect.
type inflightStreams struct {
	lock    sync.Mutex
	streams map[TunnelStream]struct{}
}

func (is *inflightStreams) add(ts TunnelStream) {
	is.lock.Lock()
	is.streams[ts] = struct{}{}
	is.lock.Unlock()
}

func (is *inflightStreams) complete(ts TunnelStream) bool {
	is.lock.Lock()
	_, wasInflight := is.streams[ts]
	if wasInflight {
		delete(is.streams, ts)
	}
	is.lock.Unlock()
	return wasInflight
}

func (is *inflightStreams) drain() []TunnelStream {
	is.lock.Lock()
	remaining := make([]TunnelStream, 0, len(is.streams))
	for ts := range is.streams {
		remaining = append(remaining, ts)
	}
	clear(is.streams)
	is.lock.Unlock()
	return remaining
}

type controlStreamRecv struct {
	result *proto.NewStreamResult
	err    error
}

// Creates a long-running, bi-directional streaming connection to the server-side proxy
// to facilitate creation of new tunnel streams.
func (cp *ClientProxy) NewStreamsConnection(newStreamConn grpc.BidiStreamingServer[proto.NewStreamResult, proto.StreamRef]) error {
	callCtx, cancelCall := context.WithCancel(newStreamConn.Context())
	stopLifetimeCancellation := context.AfterFunc(cp.lifetimeCtx, cancelCall)
	defer stopLifetimeCancellation()
	defer cancelCall()
	if lockErr := cp.controlStreamLock.Lock(callCtx); lockErr != nil {
		return lockErr
	}
	defer cp.controlStreamLock.Unlock()
	if cp.disposed.Load() || cp.lifetimeCtx.Err() != nil {
		return status.Error(codes.Unavailable, errMsgProxyDisposed)
	}
	// A replacement must not receive readiness until the old RPC has released
	// ownership, otherwise the two handlers could consume each other's work.
	if headerErr := newStreamConn.SendHeader(metadata.Pairs(controlStreamReadyHeader, "true")); headerErr != nil {
		return headerErr
	}

	// Pipeline outgoing StreamRef messages and incoming NewStreamResult replies so a slow
	// or unreachable backend dial on one tunnel cannot block new streams on other tunnels.
	// Because the server proxy establishes streams concurrently, replies may arrive out of
	// order; inflight tracks every StreamRef sent during this RPC attempt and cleans up any
	// handshakes left incomplete when this RPC ends without disturbing established streams.
	inflight := &inflightStreams{
		streams: make(map[TunnelStream]struct{}),
	}
	sendErrChan := make(chan error, 1)
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		cp.sendStreamRequests(callCtx, cancelCall, newStreamConn, inflight, sendErrChan)
	}()
	defer func() {
		cancelCall()
		<-senderDone
		// The data connection can arrive before its control reply. Losing
		// that reply must not dispose an already-established data stream.
		for _, ts := range inflight.drain() {
			cp.forgetStream(ts.TunnelID, ts.StreamID, true)
		}
	}()

	recvChan := receiveStreamResults(callCtx, newStreamConn)
	for {
		select {
		case <-callCtx.Done():
			select {
			case sendErr := <-sendErrChan:
				return sendErr
			default:
			}
			if cp.disposed.Load() || cp.lifetimeCtx.Err() != nil {
				return status.Error(codes.Unavailable, errMsgProxyDisposed)
			}
			return callCtx.Err()
		case sendErr := <-sendErrChan:
			return sendErr
		case received := <-recvChan:
			if received.err != nil {
				select {
				case sendErr := <-sendErrChan:
					return sendErr
				default:
				}
				cp.log.Info("Tunnel control stream receive failed; awaiting reconnect", "Error", received.err)
				return received.err
			}
			if handleErr := cp.handleStreamResult(received.result, inflight); handleErr != nil {
				return handleErr
			}
		}
	}
}

func (cp *ClientProxy) sendStreamRequests(
	callCtx context.Context,
	cancelCall context.CancelFunc,
	newStreamConn grpc.BidiStreamingServer[proto.NewStreamResult, proto.StreamRef],
	inflight *inflightStreams,
	sendErrChan chan<- error,
) {
	for {
		select {
		case <-callCtx.Done():
			return
		case streamRef, isOpen := <-cp.newStreamsReqs.Out:
			if !isOpen {
				return
			}

			ts := TunnelStream{
				TunnelID: TunnelID(streamRef.GetTunnelId()),
				StreamID: StreamID(streamRef.GetStreamId()),
			}
			cp.lock.Lock()
			pending, _ := cp.getStreamInfo(ts.TunnelID, ts.StreamID)
			cp.lock.Unlock()
			if pending == nil {
				continue
			}

			// Track the stream before Send so an immediate reply on the receive loop
			// always observes the stream as in flight.
			inflight.add(ts)

			sendErr := newStreamConn.Send(streamRef)
			if sendErr != nil {
				inflight.complete(ts)
				cp.log.Info("Tunnel control stream send failed; awaiting reconnect", "Error", sendErr)
				cp.forgetStream(ts.TunnelID, ts.StreamID, true)
				select {
				case sendErrChan <- sendErr:
				default:
				}
				cancelCall()
				return
			}
		}
	}
}

// receiveStreamResults reads server replies on a dedicated goroutine because cancelling callCtx
// (for example on proxy disposal or a Send failure) does not directly unblock grpc.ServerStream.Recv();
// selecting on callCtx.Done() in NewStreamsConnection lets the RPC handler return promptly so gRPC
// closes the server stream and unblocks Recv().
func receiveStreamResults(
	callCtx context.Context,
	newStreamConn grpc.BidiStreamingServer[proto.NewStreamResult, proto.StreamRef],
) <-chan controlStreamRecv {
	recvChan := make(chan controlStreamRecv, 1)
	go func() {
		for {
			res, rcvErr := newStreamConn.Recv()
			select {
			case recvChan <- controlStreamRecv{result: res, err: rcvErr}:
				if rcvErr != nil {
					return
				}
			case <-callCtx.Done():
				return
			}
		}
	}()
	return recvChan
}

func (cp *ClientProxy) handleStreamResult(res *proto.NewStreamResult, inflight *inflightStreams) error {
	ts := TunnelStream{
		TunnelID: TunnelID(res.GetStreamRef().GetTunnelId()),
		StreamID: StreamID(res.GetStreamRef().GetStreamId()),
	}
	if !inflight.complete(ts) {
		return status.Error(codes.DataLoss, "control reply does not match any pending tunnel stream")
	}

	streamFailure := res.GetFailure()
	if streamFailure != nil {
		cp.log.Error(nil, "Failed to create new stream on server proxy",
			"TunnelID", ts.TunnelID,
			"StreamID", ts.StreamID,
			"ErrorCode", streamFailure.GetError(),
			"ErrorMessage", streamFailure.GetMessage(),
		)
		cp.forgetStream(ts.TunnelID, ts.StreamID, true)
	}

	// handleProxyConnections() completes setup for successful streams when the data connection arrives.
	return nil
}

func (cp *ClientProxy) Done() <-chan struct{} {
	return cp.done
}

func (cp *ClientProxy) handleProxyConnections() {
	for {
		incoming, acceptErr := cp.dataListener.Accept()
		if errors.Is(acceptErr, net.ErrClosed) {
			return
		}
		if acceptErr != nil {
			cp.log.Error(acceptErr, "Failed to accept incoming proxy connection")
			continue
		}
		if cp.lifetimeCtx.Err() != nil || cp.disposed.Load() {
			_ = incoming.Close()
			return
		}
		go cp.runTunnelStream(incoming)
	}
}

func (cp *ClientProxy) runTunnelStream(proxyConn net.Conn) {
	stopCloseOnShutdown := context.AfterFunc(cp.lifetimeCtx, func() { _ = proxyConn.Close() })
	defer stopCloseOnShutdown()
	if cp.preambleTimeout > 0 {
		if setDeadlineErr := proxyConn.SetReadDeadline(time.Now().Add(cp.preambleTimeout)); setDeadlineErr != nil {
			cp.log.Error(setDeadlineErr, "Failed to set preamble read deadline on incoming data connection")
			_ = proxyConn.Close()
			return
		}
	}
	const preambleSize = 12 + security.BearerTokenLength // 4 bytes for tunnel ID, 8 bytes for stream ID, and the size of the auth token.
	buf := make([]byte, preambleSize)
	_, preambleErr := io.ReadFull(proxyConn, buf)
	if preambleErr != nil {
		cp.log.Error(preambleErr, "Failed to read stream preamble from incoming data connection")
		_ = proxyConn.Close()
		return
	}
	if cp.preambleTimeout > 0 {
		if clearDeadlineErr := proxyConn.SetReadDeadline(time.Time{}); clearDeadlineErr != nil {
			cp.log.Error(clearDeadlineErr, "Failed to clear preamble read deadline on incoming data connection")
			_ = proxyConn.Close()
			return
		}
	}

	tid := TunnelID(binary.BigEndian.Uint32(buf[0:4]))
	streamID := StreamID(binary.BigEndian.Uint64(buf[4:12]))
	authToken := buf[12 : 12+security.BearerTokenLength]
	if tid == invalidTunnelID {
		cp.log.Error(fmt.Errorf("invalid tunnel ID in a stream preamble: %d", tid), "Stream data connection failed")
		_ = proxyConn.Close()
		return
	}
	if streamID == invalidStreamID {
		cp.log.Error(fmt.Errorf("invalid stream ID in a stream preamble: %d", streamID), "Stream data connection failed")
		_ = proxyConn.Close()
		return
	}

	cp.lock.Lock()
	si, td := cp.getStreamInfo(tid, streamID)
	if si == nil {
		cp.lock.Unlock()
		cp.log.Error(fmt.Errorf("stream not found: TunnelID=%d, StreamID=%d", tid, streamID), "Stream data connection failed")
		_ = proxyConn.Close()
		return
	}
	if si.proxyConn != nil {
		cp.lock.Unlock()
		cp.log.Error(fmt.Errorf("stream already has a proxy connection: TunnelID=%d, StreamID=%d", tid, streamID), "Stream data connection failed")
		_ = proxyConn.Close()
		return
	}
	expectedToken := td.spec.GetDataConnectionToken()
	if len(expectedToken) == 0 || subtle.ConstantTimeCompare(authToken, expectedToken) != 1 {
		cp.lock.Unlock()
		cp.log.Error(fmt.Errorf("invalid authentication token for stream: TunnelID=%d, StreamID=%d", tid, streamID), "Stream data connection failed")
		_ = proxyConn.Close()
		return
	}
	clientConn := si.clientConn
	si.proxyConn = proxyConn
	streamCtx := si.streamCtx
	cp.lock.Unlock()

	streamLog := cp.log.WithValues(
		"TunnelID", tid,
		"StreamID", streamID,
		"ClientAddress", clientConn.RemoteAddr().String(),
		"ProxyConnAddress", proxyConn.RemoteAddr().String(),
	)
	streamLog.V(1).Info("New tunnel stream started")

	defer func() {
		cp.forgetStream(tid, streamID, false)
	}()

	pcr, cr := proxy.StreamNetworkData(streamCtx, proxyConn, clientConn)

	if pcr.ReadError != nil {
		streamLog.Error(pcr.ReadError, "The proxy data connection encountered a read error")
	} else if pcr.WriteError != nil {
		streamLog.V(1).Error(pcr.WriteError, "The proxy data connection encountered a write error")
	} else {
		streamLog.V(1).Info("Proxy data connection is done")
	}

	if cr.WriteError != nil {
		streamLog.V(1).Error(cr.WriteError, "The client connection encountered a write error")
	} else if cr.ReadError != nil {
		streamLog.V(1).Error(cr.ReadError, "The client cconnection encountered a read error")
	} else {
		streamLog.V(1).Info("Client connection is done")
	}
}

// Handles incoming connections from clients.
//
// The control flow is as follows:
//
//  1. The connection is accepted by the passed listener.
//
//  2. We generate a new stream ID and store it, together with the parked connection, in the tunnel data.
//
//  3. We sent a request to the new streams goroutine to start a new tunnel stream.
//
//  4. The new streams goroutine sends a proto.StreamsRef message to the server proxy.
//
//  5. The server proxy sets up the server portion of the stream and sends a proto.NewStreamResult message back to us.
//     There are two possible outcomes:
//
//     a. The server portion of the stream is created successfully. In that case we might receive a data connection
//     (see handleDataConnections), and we receive a proto.ConnectionResult message indicating success.
//
//     These two things will happen asynchronously. Stream setup will be completed by the handleDataConnection() method.
//
//     b. The server portion of the stream cannot be created. In that case we receive a proto.NewStreamResult message
//     with an error code and message. We then close the parked connection and remove it from the tunnel data (streams map).
func (cp *ClientProxy) handleClientConnections(tunnelCtx context.Context, tid TunnelID, listener net.Listener) {
	for {
		incoming, acceptErr := listener.Accept()
		if errors.Is(acceptErr, net.ErrClosed) {
			return
		}
		if acceptErr != nil {
			cp.log.Error(acceptErr, "Failed to accept incoming client connection", "TunnelID", tid)
			continue
		}
		if tunnelCtx.Err() != nil || cp.disposed.Load() {
			_ = incoming.Close()
			return
		}
		cp.lock.Lock()
		td, found := cp.tunnels[tid]
		if !found || td.deleted.Load() || !td.accepting.Load() {
			cp.lock.Unlock()
			_ = incoming.Close()
			cp.log.V(1).Info("Tunnel is not accepting new streams, rejecting incoming connection", "TunnelID", tid)
			continue
		}

		streamCtx, streamCtxCancel := context.WithCancel(tunnelCtx)
		si := &streamInfo{
			clientConn:      incoming,
			streamCtx:       streamCtx,
			streamCtxCancel: streamCtxCancel,
			created:         time.Now(),
		}
		streamID := StreamID(atomic.AddUint64((*uint64)(&latestStreamID), 1))
		td.streams[streamID] = si
		cp.lock.Unlock()

		cp.log.V(1).Info("New client connection accepted",
			"TunnelID", tid,
			"StreamID", streamID,
			"ClientAddress", incoming.RemoteAddr().String(),
			"ClientProxyAddress", incoming.LocalAddr().String(),
		)
		streamRef := &proto.StreamRef{
			TunnelId: stdproto.Uint32(uint32(tid)),
			StreamId: stdproto.Uint64(uint64(streamID)),
		}
		select {
		case cp.newStreamsReqs.In <- streamRef:
		case <-tunnelCtx.Done():
			cp.forgetStream(tid, streamID, true)
			return
		}
	}
}

func (cp *ClientProxy) shutDownAllTunnels() {
	cp.lock.Lock()
	tunnels := cp.tunnels
	cp.tunnels = make(map[TunnelID]*clientTunnelData)
	cp.tunnelsByName = make(map[string]TunnelID)
	cp.lock.Unlock()

	for tid, td := range tunnels {
		cp.log.V(1).Info("Shutting down tunnel", "TunnelID", tid, "TunnelSpec", td.spec.LogString())
		td.tunnelCtxCancel()
		_ = td.listener.Close()

		for streamID, streamInfo := range td.streams {
			if streamInfo == nil {
				continue
			}

			cp.log.V(1).Info("Stopping tunnel stream", "TunnelID", tid, "StreamID", streamID)
			streamInfo.dispose()
		}
	}
}

// findExistingTunnelByName assumes cp.lock is held and validates request compatibility when an active tunnel shares tr.TunnelName.
func (cp *ClientProxy) findExistingTunnelByName(tr *proto.TunnelReq) (*proto.TunnelSpec, error) {
	tid, found := cp.tunnelsByName[tr.GetTunnelName()]
	if !found {
		return nil, nil
	}
	tunnel, tunnelExists := cp.tunnels[tid]
	if !tunnelExists || tunnel.deleted.Load() {
		delete(cp.tunnelsByName, tr.GetTunnelName())
		return nil, nil
	}
	if tr.GetServerAddress() != tunnel.spec.GetServerAddress() ||
		tr.GetServerPort() != tunnel.spec.GetServerPort() ||
		tr.GetClientProxyAddress() != tunnel.clientProxyAddress ||
		tr.GetClientProxyPort() != tunnel.requestedClientProxyPort ||
		tr.GetAllowClientProxyPortFallback() != tunnel.allowClientProxyPortFallback ||
		(tr.ExcludedClientProxyPortsApply() && slices.Contains(tr.GetExcludedClientProxyPorts(), tunnel.spec.GetClientProxyPort())) {
		return nil, status.Errorf(
			codes.AlreadyExists,
			"tunnel %q already exists with different configuration",
			tr.GetTunnelName(),
		)
	}
	return tunnel.spec, nil
}

func (cp *ClientProxy) disposeOnce() bool {
	if cp.disposed.Swap(true) {
		return false
	}

	defer close(cp.done)
	cp.log.V(1).Info("Disposing client-side proxy")
	cp.requestShutdown()
	cp.shutDownAllTunnels()
	_ = cp.dataListener.Close()
	return true
}

func (cp *ClientProxy) getStreamInfo(tid TunnelID, streamID StreamID) (*streamInfo, *clientTunnelData) {
	// Assumes cp.lock is held by the caller.

	td, found := cp.tunnels[tid]
	if !found {
		return nil, nil
	}

	streamInfo, infoFound := td.streams[streamID]
	if !infoFound {
		return nil, nil
	}

	return streamInfo, td
}

func (cp *ClientProxy) forgetStream(tid TunnelID, streamID StreamID, pendingOnly bool) {
	cp.lock.Lock()
	si, ctd := cp.getStreamInfo(tid, streamID)
	if si != nil && (!pendingOnly || si.proxyConn == nil) {
		si.dispose()
		delete(ctd.streams, streamID)

		if len(ctd.streams) == 0 && ctd.deleted.Load() {
			cp.log.V(1).Info("All streams for deleted tunnel have been closed, cleaning up the tunnel", "TunnelID", tid)
			ctd.tunnelCtxCancel()
			_ = ctd.listener.Close()
			delete(cp.tunnels, tid)
			// cp.tunnelRequests entry was already removed when ctd.deleted was set to true
		}
	}

	cp.lock.Unlock()
}

// scavengeHalfOpenStreams periodically scans for streams that have been half-open
// (no proxyConn) for longer than two minutes and cleans them up.
func (cp *ClientProxy) scavengeHalfOpenStreams() {
	const scanInterval = 30 * time.Second
	const halfOpenTimeout = 2 * time.Minute

	timer := time.NewTimer(scanInterval)
	defer timer.Stop()

	for {
		select {
		case <-cp.lifetimeCtx.Done():
			return

		case <-timer.C:
			if cp.disposed.Load() {
				return
			}

			now := time.Now()
			var streamsToCleanup []TunnelStream

			cp.lock.Lock()
			for tid, td := range cp.tunnels {
				if td == nil {
					continue
				}
				for streamID, si := range td.streams {
					if si == nil {
						continue
					}

					if si.clientConn != nil && si.proxyConn == nil && now.Sub(si.created) > halfOpenTimeout {
						streamsToCleanup = append(streamsToCleanup, TunnelStream{
							TunnelID: tid,
							StreamID: streamID,
						})
					}
				}
			}
			cp.lock.Unlock()

			for _, ts := range streamsToCleanup {
				cp.log.V(1).Info("Scavenging half-open stream", "TunnelID", ts.TunnelID, "StreamID", ts.StreamID)
				cp.forgetStream(ts.TunnelID, ts.StreamID, true)
			}

			timer.Reset(scanInterval)
		}
	}
}

var _ proto.TunnelControlServer = &ClientProxy{}
