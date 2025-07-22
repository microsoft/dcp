package dcptun

import (
	"context"
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
	"google.golang.org/grpc/status"
	stdproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/microsoft/usvc-apiserver/internal/dcptun/proto"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/proxy"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/grpcutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
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

	// The listener for clients to create new tunnel connections.
	listener net.Listener

	// The lifetime context for the tunnel.
	tunnelCtx context.Context

	// The cancellation function for the context for the lifetime of the tunnel.
	tunnelCtxCancel context.CancelFunc
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

	// Data about tunnels managed by this proxy, indexed by tunnel ID.
	tunnels map[TunnelID]*clientTunnelData

	// A channel that carries new stream requests. It is used for communication between
	// goroutines accepting new client connections and the goroutine that interacts
	// with the server-side proxy to start a new tunnel stream.
	newStreamsReqs *concurrency.UnboundedChan[*proto.StreamRef]

	// Mutex to protect access to internal proxy state (primarily the tunnels map).
	lock *sync.Mutex

	// True if the proxy has been shut down.
	disposed *atomic.Bool

	// A channel that is closed when the proxy is disposed.
	done chan struct{}
}

func NewClientProxy(ctx context.Context, dataListener net.Listener, requestShutdown func(), log logr.Logger) *ClientProxy {
	cp := &ClientProxy{
		lifetimeCtx:     ctx,
		dataListener:    dataListener,
		requestShutdown: requestShutdown,
		log:             log,
		tunnels:         make(map[TunnelID]*clientTunnelData),
		newStreamsReqs:  concurrency.NewUnboundedChan[*proto.StreamRef](ctx),
		lock:            &sync.Mutex{},
		disposed:        &atomic.Bool{},
		done:            make(chan struct{}),
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

	effectiveAddresses, lookupErr := networking.LookupIP(tr.GetClientProxyAddress())
	if lookupErr != nil {
		return nil, status.Errorf(codes.Internal, "failed to resolve client proxy address %s: %s", tr.GetClientProxyAddress(), lookupErr.Error())
	}

	lc := net.ListenConfig{}
	tunnelListener, tlErr := lc.Listen(ctx, "tcp", networking.AddressAndPort(tr.GetClientProxyAddress(), tr.GetClientProxyPort()))
	if tlErr != nil {
		cp.log.Error(tlErr, "Failed to listen on client proxy address",
			"Address", tr.GetClientProxyAddress(),
			"Port", tr.GetClientProxyPort(),
		)
		return nil, status.Errorf(codes.Internal, "failed to listen on client proxy address %s:%d: %s", tr.GetClientProxyAddress(), tr.GetClientProxyPort(), tlErr.Error())
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
		ClientProxyAddresses: slices.Map[net.IP, string](effectiveAddresses, func(ip net.IP) string { return networking.IpToString(ip) }),
		ClientProxyPort:      stdproto.Int32(int32(listenerAddr.Port)),
	}
	ctd := &clientTunnelData{
		tunnelData:      *newTunnelData[*streamInfo](ts),
		listener:        tunnelListener,
		tunnelCtx:       tunnelCtx,
		tunnelCtxCancel: tunnelCtxCancel,
	}
	cp.lock.Lock()
	cp.tunnels[tid] = ctd
	cp.lock.Unlock()

	go cp.handleClientConnections(tunnelCtx, tid, tunnelListener)
	cp.log.V(1).Info("Tunnel prepared", "TunnelSpec", ts.String())
	return ts, nil
}

// Gracefully deletes the tunnel identified by tunnel ID.
// Existing streams will be preserved, but no new streams will be allowed.
func (cp *ClientProxy) DeleteTunnel(ctx context.Context, tr *proto.TunnelRef) (*emptypb.Empty, error) {
	if tr == nil || tr.TunnelId == nil {
		return nil, status.Errorf(codes.InvalidArgument, "bad request: no tunnel ID specified")
	}

	tid := TunnelID(tr.GetTunnelId())
	cp.lock.Lock()
	defer cp.lock.Unlock()
	td, found := cp.tunnels[tid]
	if !found {
		return nil, status.Errorf(codes.NotFound, "tunnel with ID %d does not exist", tid)
	} else {
		td.deleted.Store(true)
		cp.log.V(1).Info("Tunnel deleted", "TunnelID", tid)
		return &emptypb.Empty{}, nil
	}
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

// Creates a long-running, bi-directional streaming connection to the server-side proxy
// to facilitate creation of new tunnel streams.
func (cp *ClientProxy) NewStreamsConnection(newStreamConn grpc.BidiStreamingServer[proto.NewStreamResult, proto.StreamRef]) error {
	for {
		select {
		case <-cp.lifetimeCtx.Done():
			return nil // Client-side proxy is shutting down

		case streamRef := <-cp.newStreamsReqs.Out:

			tid := TunnelID(streamRef.GetTunnelId())
			streamID := StreamID(streamRef.GetStreamId())

			sendErr := newStreamConn.Send(streamRef)

			if grpcutil.IsStreamDoneErr(sendErr) {
				cp.log.V(1).Info("New streams connection is closing...")
				_ = cp.disposeOnce()
				return nil
			}

			if sendErr != nil {
				cp.log.Error(sendErr, "Failed to send new stream request to server proxy", "TunnelID", tid, "StreamID", streamID)
				cp.forgetStream(tid, streamID)
				continue
			}

			res, rcvErr := newStreamConn.Recv()
			if rcvErr == io.EOF || status.Code(rcvErr) == codes.Canceled {
				cp.log.V(1).Info("New streams connection closed by server proxy")
				_ = cp.disposeOnce()
				return nil
			}

			if errors.Is(rcvErr, context.Canceled) {
				cp.log.V(1).Info("New streams connection context is canceled (proxy shutdown)")
				_ = cp.disposeOnce()
				return nil
			}

			if rcvErr != nil {
				cp.log.Error(rcvErr, "Failed to receive new stream result from server proxy", "TunnelID", tid, "StreamID", streamID)
				cp.forgetStream(tid, streamID)
				continue
			}

			streamFailure := res.GetFailure()
			if streamFailure != nil {
				cp.log.Error(nil, "Failed to create new stream on server proxy",
					"TunnelID", tid,
					"StreamID", streamID,
					"ErrorCode", streamFailure.GetError(),
					"ErrorMessage", streamFailure.GetMessage(),
				)
				cp.forgetStream(tid, streamID)
				continue
			}

			// handleProxyConnections() will complete the tunnel stream setup,
			// we can pretty much ignore a successful result here.
		}
	}
}

func (cp *ClientProxy) Done() <-chan struct{} {
	return cp.done
}

func (cp *ClientProxy) handleProxyConnections() {
	connCh := concurrency.NewUnboundedChan[net.Conn](cp.lifetimeCtx)

	go func() {
		for {
			if cp.lifetimeCtx.Err() != nil {
				return
			}

			if cp.disposed.Load() {
				return
			}

			incoming, err := cp.dataListener.Accept()
			if errors.Is(err, net.ErrClosed) {
				// We might get into this case if the proxy is being shut down and the timing is just right.
				return
			} else if err != nil {
				cp.log.Error(err, "Failed to accept incoming proxy connection")
			} else {
				connCh.In <- incoming
			}
		}
	}()

	for {
		select {
		case <-cp.lifetimeCtx.Done():
			return

		case incoming := <-connCh.Out:
			if cp.lifetimeCtx.Err() != nil {
				cp.log.V(1).Info("Lifetime context is done, closing incoming proxy connection")
				_ = incoming.Close()
				return
			}

			if cp.disposed.Load() {
				cp.log.V(1).Info("Client-side proxy is disposed, closing incoming proxy connection")
				_ = incoming.Close()
				return
			}

			go cp.runTunnelStream(incoming)
		}
	}
}

func (cp *ClientProxy) runTunnelStream(proxyConn net.Conn) {
	const preambleSize = 12 // 4 bytes for tunnel ID, 8 bytes for stream ID.
	buf := make([]byte, preambleSize)
	_, preambleErr := io.ReadFull(proxyConn, buf)
	if preambleErr != nil {
		cp.log.Error(preambleErr, "Failed to read stream preamble from incoming data connection")
		_ = proxyConn.Close()
		return
	}

	tid := TunnelID(binary.BigEndian.Uint32(buf[0:4]))
	streamID := StreamID(binary.BigEndian.Uint64(buf[4:12]))
	if tid == invalidTunnelID {
		cp.log.Error(fmt.Errorf("Invalid tunnel ID in a stream preamble: %d", tid), "Stream data connection failed")
		_ = proxyConn.Close()
		return
	}
	if streamID == invalidStreamID {
		cp.log.Error(fmt.Errorf("Invalid stream ID in a stream preamble: %d", streamID), "Stream data connection failed")
		_ = proxyConn.Close()
		return
	}

	cp.lock.Lock()
	si := cp.getStreamInfo(tid, streamID)
	if si == nil {
		cp.lock.Unlock()
		cp.log.Error(fmt.Errorf("Stream not found: TunnelID=%d, StreamID=%d", tid, streamID), "Stream data connection failed")
		_ = proxyConn.Close()
		return
	}
	if si.proxyConn != nil {
		cp.lock.Unlock()
		cp.log.Error(fmt.Errorf("Stream already has a proxy connection: TunnelID=%d, StreamID=%d", tid, streamID), "Stream data connection failed")
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
		cp.forgetStream(tid, streamID)
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
	connCh := concurrency.NewUnboundedChan[net.Conn](tunnelCtx)

	go func() {
		for {
			if tunnelCtx.Err() != nil {
				return
			}

			if cp.disposed.Load() {
				return
			}

			incoming, err := listener.Accept()
			if errors.Is(err, net.ErrClosed) {
				// Shutting down the tunnel listener, no need to log anything.
				return
			} else if err != nil {
				cp.log.Error(err, "Failed to accept incoming client connection", "TunnelID", tid)
			} else {
				connCh.In <- incoming
			}
		}
	}()

	for {
		select {

		case <-tunnelCtx.Done():
			return

		case incoming := <-connCh.Out:
			if tunnelCtx.Err() != nil {
				cp.log.V(1).Info("Tunnel context is done, closing incoming connection",
					"TunnelID", tid, "ClientAddress", incoming.RemoteAddr().String())
				_ = incoming.Close()
				return
			}

			if cp.disposed.Load() {
				cp.log.V(1).Info("Client-side proxy is disposed, closing incoming connection",
					"TunnelID", tid, "ClientAddress", incoming.RemoteAddr().String())
				_ = incoming.Close()
				return
			}

			cp.lock.Lock()

			td, found := cp.tunnels[tid]
			if !found {
				cp.lock.Unlock()
				_ = incoming.Close()
				cp.log.V(1).Info("Tunnel not found, rejecting incoming connection", "TunnelID", tid)
				continue
			}
			if td.deleted.Load() {
				cp.lock.Unlock()
				_ = incoming.Close()
				cp.log.V(1).Info("Tunnel is deleted, rejecting incoming connection", "TunnelID", tid)
				continue
			}

			streamCtx, streamCtxCancel := context.WithCancel(tunnelCtx)
			streamInfo := &streamInfo{
				clientConn:      incoming,
				streamCtx:       streamCtx,
				streamCtxCancel: streamCtxCancel,
				created:         time.Now(),
			}
			streamID := StreamID(atomic.AddUint64((*uint64)(&latestStreamID), 1))
			td.streams[streamID] = streamInfo

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
			cp.newStreamsReqs.In <- streamRef
		}
	}
}

func (cp *ClientProxy) shutDownAllTunnels() {
	cp.lock.Lock()
	tunnels := cp.tunnels
	cp.tunnels = make(map[TunnelID]*clientTunnelData)
	cp.lock.Unlock()

	for tid, td := range tunnels {
		cp.log.V(1).Info("Shutting down tunnel", "TunnelID", tid, "TunnelSpec", td.spec.String())
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

func (cp *ClientProxy) getStreamInfo(tid TunnelID, streamID StreamID) *streamInfo {
	// Assumes cp.lock is held by the caller.

	td, found := cp.tunnels[tid]
	if !found {
		return nil
	}

	streamInfo, infoFound := td.streams[streamID]
	if !infoFound {
		return nil
	}

	return streamInfo
}

func (cp *ClientProxy) forgetStream(tid TunnelID, streamID StreamID) {
	cp.lock.Lock()
	si := cp.getStreamInfo(tid, streamID)
	if si != nil {
		si.dispose()
		delete(cp.tunnels[tid].streams, streamID)
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
				cp.forgetStream(ts.TunnelID, ts.StreamID)
			}

			timer.Reset(scanInterval)
		}
	}
}

var _ proto.TunnelControlServer = &ClientProxy{}
