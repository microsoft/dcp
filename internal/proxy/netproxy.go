// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/queue"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

const (
	// Timeouts used for the read operation. When the read request times out, it gives us the opportunity
	// to check for pending write requests and whether the proxy connection should be shut down.
	// Reads are interruptible by writes (meaning arriving write will cancel the read operation),
	// so the read timeout can be relatively long.
	DefaultReadTimeout = 3 * time.Second

	// Timeout used for the write operation.
	DefaultWriteTimeout = 5 * time.Second

	// The default connection timeout for establishing a TCP connections.
	DefaultConnectionTimeout = 5 * time.Second

	// The maximum number of UDP packets that will be cached for a single client.
	MaxCachedUdpPackets = 20

	// Even though the maximum UDP packet size is 64 kB, most networks have a maximum transmission unit (MTU)
	// that is much lower. E.g. Ethernet MTU is only 1500 bytes. And packet fragmentation is something
	// that every UDP client must be prepared to deal with.
	// That is why we use a buffer of 4kB for UDP packet data.
	UdpPacketBufferSize = 4 * 1024

	// The size of the TCP data buffer, used for single read between two TCP connections (32 kB).
	TcpDataBufferSize = 32 * 1024

	// The time after which a UDP stream will be shut down if it has not been used.
	UdpStreamInactivityTimeout = 2 * time.Minute
)

// A "UDP stream" binds the client with the Endpoint that is serving it.
// Packets sent by the client will be forwarded to the Endpoint using dedicated PacketConn connection.
// Packets received from that PacketConn will be sent back to the client.
type udpStream struct {
	clientAddr net.Addr                              // Address of the client that is being served by this stream
	packets    *queue.ConcurrentBoundedQueue[[]byte] // Queue of packets from the client
	lastUsed   *concurrency.AtomicTime               // Time when this stream was last used
	ctx        context.Context                       // Context that will be cancelled when the stream is to be retired.
	cancel     context.CancelFunc                    // The function to cancel the stream context.
}

// netProxy is an in-process implementation of the Proxy interface that uses
// the standard library's net package to proxy TCP and UDP connections.
type netProxy struct {
	mode          apiv1.PortProtocol
	listenAddress string
	listenPort    int32

	effectiveAddress string
	effectivePort    int32

	endpointConfigLoadedChannel *concurrency.UnboundedChan[ProxyConfig]
	configurationApplied        *concurrency.AutoResetEvent
	readTimeout                 time.Duration
	writeTimeout                time.Duration
	connectionTimeout           time.Duration
	streamSeqNo                 uint32

	udpStreams *syncmap.Map[string, udpStream]

	lifetimeCtx context.Context
	log         logr.Logger
	state       ProxyState
	lock        sync.Locker
}

func NewRuntimeProxy(mode apiv1.PortProtocol, listenAddress string, listenPort int32, lifetimeCtx context.Context, log logr.Logger) Proxy {
	return newNetProxy(mode, listenAddress, listenPort, lifetimeCtx, log)
}

func newNetProxy(mode apiv1.PortProtocol, listenAddress string, listenPort int32, lifetimeCtx context.Context, log logr.Logger) *netProxy {
	if mode != apiv1.TCP && mode != apiv1.UDP {
		panic(fmt.Errorf("unsupported proxy mode: %s", mode))
	}

	p := netProxy{
		mode:          mode,
		listenAddress: listenAddress,
		listenPort:    listenPort,

		endpointConfigLoadedChannel: concurrency.NewUnboundedChan[ProxyConfig](lifetimeCtx),
		configurationApplied:        concurrency.NewAutoResetEvent(false),
		readTimeout:                 DefaultReadTimeout,
		writeTimeout:                DefaultWriteTimeout,
		connectionTimeout:           DefaultConnectionTimeout,

		udpStreams: &syncmap.Map[string, udpStream]{},

		lifetimeCtx: lifetimeCtx,
		log:         log,
		state:       ProxyStateInitial,
		lock:        &sync.Mutex{},
	}

	return &p
}

func (p *netProxy) ListenAddress() string {
	return p.listenAddress
}

func (p *netProxy) ListenPort() int32 {
	return p.listenPort
}

func (p *netProxy) EffectiveAddress() string {
	return p.effectiveAddress
}

func (p *netProxy) EffectivePort() int32 {
	return p.effectivePort
}

func (p *netProxy) Start() error {
	if p.lifetimeCtx.Err() != nil {
		_ = p.setState(ProxyStateAny, ProxyStateFinished)
		return fmt.Errorf("proxy cannot be started: lifetime context expired: %w", p.lifetimeCtx.Err())
	}

	if err := p.setState(ProxyStateInitial, ProxyStateRunning); err != nil {
		return fmt.Errorf("proxy cannot be started: %w", err)
	}

	if p.listenAddress == "" {
		p.listenAddress = networking.Localhost
	}

	lc := net.ListenConfig{}
	if p.mode == apiv1.TCP {
		tcpListener, err := lc.Listen(p.lifetimeCtx, "tcp", networking.AddressAndPort(p.listenAddress, p.listenPort))
		if err != nil {
			_ = p.setState(ProxyStateAny, ProxyStateFailed)
			return err
		}

		tcpAddr := tcpListener.Addr().(*net.TCPAddr)
		p.effectiveAddress = networking.IpToString(tcpAddr.IP)
		p.effectivePort = int32(tcpAddr.Port)

		go p.runTCP(tcpListener)
	} else if p.mode == apiv1.UDP {
		udpListener, err := lc.ListenPacket(p.lifetimeCtx, "udp", networking.AddressAndPort(p.listenAddress, p.listenPort))
		if err != nil {
			_ = p.setState(ProxyStateAny, ProxyStateFailed)
			return err
		}

		udpAddr := udpListener.LocalAddr().(*net.UDPAddr)
		p.effectiveAddress = networking.IpToString(udpAddr.IP)
		p.effectivePort = int32(udpAddr.Port)

		go p.runUDP(udpListener)
	}

	return nil
}

func (p *netProxy) Configure(newConfig ProxyConfig) error {
	state := p.State()
	if state == ProxyStateFailed {
		return fmt.Errorf("proxy cannot be configured in Failed state")
	}

	// Configuration applied after the proxy has finished will not be effective,
	// but the call to Configure might come during shutdown, so we do not return an error in that case.
	if state != ProxyStateFinished {
		p.endpointConfigLoadedChannel.In <- newConfig.Clone()
		if p.mode == apiv1.UDP {
			p.shutdownAllUDPStreams()
		}
		if state == ProxyStateRunning {
			<-p.configurationApplied.Wait()
		}
	}

	return nil
}

func (p *netProxy) State() ProxyState {
	p.lock.Lock()
	defer p.lock.Unlock()
	return p.state
}

func (p *netProxy) setState(expectedState, newState ProxyState) error {
	p.lock.Lock()
	defer p.lock.Unlock()
	if p.state == newState {
		return nil
	}
	if p.state&expectedState != 0 {
		p.state = newState
		return nil
	}

	return fmt.Errorf("proxy cannot be set to state %s (current state %s)", newState.String(), p.state.String())
}

func (p *netProxy) stop(listener io.Closer) {
	const errMsg = "Error stopping proxy"
	// This Close call will stop TCP Accept / UDP ReadFrom calls, which will ultimately cause the runTCP / runUDP functions to exit
	if err := listener.Close(); err != nil {
		p.log.Error(err, errMsg)
	}

	_ = p.setState(ProxyStateAny, ProxyStateFinished)
}

func (p *netProxy) runTCP(tcpListener net.Listener) {
	var parkedConnections []net.Conn

	defer func() {
		for _, conn := range parkedConnections {
			_ = conn.Close()
		}
	}()
	defer p.configurationApplied.SetAndFreeze() // Make sure that Configure() calls return after the proxy has stopped
	defer p.stop(tcpListener)

	// Wait until the config has been loaded the first time before accepting any connections
	config := <-p.endpointConfigLoadedChannel.Out
	p.configurationApplied.Set()
	p.log.V(1).Info("initial endpoint configuration loaded", "Config", config.String())

	// Make a channel that will receive a connection when one is accepted
	connectionChannel := concurrency.NewUnboundedChan[net.Conn](p.lifetimeCtx)
	go func() {
		for {
			if p.lifetimeCtx.Err() != nil {
				return
			}

			// Accept will block until a connection is received or the listener is closed via p.stop()
			incoming, err := tcpListener.Accept()
			if errors.Is(err, net.ErrClosed) {
				// Normal shutdown pathway, don't log
			} else if err != nil {
				p.log.Info("Error accepting TCP connection: %s", err)
			} else {
				connectionChannel.In <- incoming
			}
		}
	}()

	for {
		select {
		case <-p.lifetimeCtx.Done():
			return

		case config = <-p.endpointConfigLoadedChannel.Out:
			if p.lifetimeCtx.Err() != nil {
				return
			}
			p.configurationApplied.Set()
			p.log.V(1).Info("endpoint configuration changed; new configuration will be applied to future connections...", "Config", config.String())

			if len(config.Endpoints) >= 0 {
				for _, conn := range parkedConnections {
					go p.handleTCPConnection(config, conn)
				}
				parkedConnections = nil
			}

		case incoming := <-connectionChannel.Out:
			if p.lifetimeCtx.Err() != nil {
				return
			}

			if len(config.Endpoints) == 0 {
				p.log.V(1).Info("No endpoints configured, parking connection...")
				parkedConnections = append(parkedConnections, incoming)
			} else {
				// Pass the config (copy value) to the goroutine to avoid data races.
				go p.handleTCPConnection(config, incoming)
			}
		}
	}
}

var errTcpDialFailed = errors.New("Could not establish TCP connection to endpoint")

func (p *netProxy) handleTCPConnection(currentConfig ProxyConfig, incoming net.Conn) {
	err := resiliency.RetryExponential(p.lifetimeCtx, func() error {
		streamErr := p.startTCPStream(incoming, &currentConfig)
		if errors.Is(streamErr, errTcpDialFailed) {
			// Retry-able error, incoming connection still alive
			return streamErr
		} else {
			// Fatal error, incoming connection is closed
			return resiliency.Permanent(streamErr)
		}
	})

	if err != nil {
		p.log.Error(err, "Error handling TCP connection")
	}
}

func (p *netProxy) startTCPStream(incoming net.Conn, config *ProxyConfig) error {
	if p.lifetimeCtx.Err() != nil {
		_ = incoming.Close()
		return nil
	}

	endpoint, err := chooseEndpoint(config)
	if err != nil {
		return errors.Join(err, incoming.Close())
	}

	if p.log.V(1).Enabled() {
		p.log.V(1).Info(fmt.Sprintf("Accepted TCP connection from %s, forwarding to %s ...",
			incoming.RemoteAddr().String(),
			networking.AddressAndPort(endpoint.Address, endpoint.Port),
		))
	}

	var d net.Dialer
	// We use relatively short deadline for dialing because we want to try another endpoint fairly quickly if connection establishment fails.
	dialContext, dialContextCancel := context.WithTimeout(p.lifetimeCtx, p.connectionTimeout)
	defer dialContextCancel()

	ap := networking.AddressAndPort(endpoint.Address, endpoint.Port)
	outgoing, dialErr := d.DialContext(dialContext, "tcp", ap)
	if dialErr != nil {
		p.log.V(1).Info(fmt.Sprintf("Error establishing TCP connection to %s, will try another endpoint", ap))
		// Do not close incoming connection
		return fmt.Errorf("%w: %w", errTcpDialFailed, dialErr)
	}

	streamCtx, streamCtxCancel := context.WithCancel(p.lifetimeCtx)
	streamID := strconv.FormatUint(uint64(atomic.AddUint32(&p.streamSeqNo, 1)), 10)

	if p.log.V(1).Enabled() {
		p.log.V(1).Info("Started TCP stream", "Stream", getStreamDescription(streamID, incoming, outgoing))
	}

	go func() {
		p.runTcpStream(streamID, streamCtx, incoming, outgoing)
		streamCtxCancel()

		if p.log.V(1).Enabled() {
			p.log.V(1).Info(fmt.Sprintf("Closing connections associated with TCP stream %s from %s to %s",
				streamID,
				incoming.RemoteAddr().String(),
				outgoing.RemoteAddr().String()),
			)
		}

		_ = incoming.Close()
		_ = outgoing.Close()
	}()

	return nil
}

// Copies data from incoming to outgoing connection (and vice versa) until there is no more data to copy,
// or the passed context is cancelled.
// The passed context corresponds to the lifetime of the (bi-directional) stream,
// so if either half of the stream is done/encounters an error, the other half will be stopped.
func (p *netProxy) runTcpStream(
	streamID string,
	streamCtx context.Context,
	incoming net.Conn,
	outgoing net.Conn,
) {
	// Network stream immediately starts copying data between the two connections.
	// It returns when one of the connections is closed, or when an error occurs.
	// There are many reasons why a network I/O operation may fail,
	// and the failures are reported differently by different APIs and on different platforms.
	// That is why we generally do not log errors from those operations except in specific circumstances,
	// such as DNS name resolution or initial connection establishment.
	// In all other cases we just shut down the communication stream and let the client(s) retry.

	ir, or := StreamNetworkData(streamCtx, incoming, outgoing)

	if p.log.V(1).Enabled() {
		silenceErrors := SilenceTcpStreamCompletionErrors.Load()

		if ir.ReadError != nil && !silenceErrors {
			p.log.V(1).Error(
				ir.ReadError,
				"The incoming TCP connection encountered a read error",
				"Stream", getStreamDescription(streamID, incoming, outgoing),
				"Stats", ir.LogProperties(),
			)
		} else if ir.WriteError != nil && !silenceErrors {
			p.log.V(1).Error(
				ir.WriteError,
				"The incoming TCP connection encountered a write error",
				"Stream", getStreamDescription(streamID, incoming, outgoing),
				"Stats", ir.LogProperties(),
			)
		} else {
			p.log.V(1).Info(
				"Incoming TCP connection is done",
				"Stream", getStreamDescription(streamID, incoming, outgoing),
				"Stats", ir.LogProperties(),
			)
		}

		if or.WriteError != nil && !silenceErrors {
			p.log.V(1).Error(
				or.WriteError,
				"The outgoing TCP connection encountered a write error",
				"Stream", getStreamDescription(streamID, incoming, outgoing),
				"Stats", or.LogProperties(),
			)
		} else if or.ReadError != nil && !silenceErrors {
			p.log.V(1).Error(
				or.ReadError,
				"The outgoing TCP connection encountered a read error",
				"Stream", getStreamDescription(streamID, incoming, outgoing),
				"Stats", or.LogProperties(),
			)
		} else {
			p.log.V(1).Info(
				"Outgoing TCP connection is done",
				"Stream", getStreamDescription(streamID, incoming, outgoing),
				"Stats", or.LogProperties(),
			)
		}
	}
}

func (p *netProxy) runUDP(udpListener net.PacketConn) {
	defer p.configurationApplied.SetAndFreeze() // Make sure that Configure() calls return after the proxy has stopped
	defer p.stop(udpListener)
	defer p.shutdownAllUDPStreams()

	// Wait until the config file has been loaded the first time before accepting any packets
	config := <-p.endpointConfigLoadedChannel.Out
	p.configurationApplied.Set()
	p.log.V(1).Info("initial endpoint configuration loaded", "Config", config.String())

	buffer := make([]byte, UdpPacketBufferSize)

	for {
		select {
		case config = <-p.endpointConfigLoadedChannel.Out:
			if p.lifetimeCtx.Err() != nil {
				return
			}
			p.tryStartExistingUDPStreams(config, udpListener)
			p.configurationApplied.Set()
			p.log.V(1).Info("Configuration changed; new configuration will be applied to future packets...", "Config", config.String())

		default:
			// No config change, continue
		}

		if p.lifetimeCtx.Err() != nil {
			return
		}

		if err := udpListener.SetReadDeadline(time.Now().Add(p.readTimeout)); err != nil {
			p.log.Error(err, "Error setting read deadline for proxy UDP connection", err)
			return
		}

		// ReadFrom will block for at most 3 seconds before hitting the deadline, at which point it will
		// return an error. This is expected so we do not bail out, and as long as the connection context
		// is still active, the deadline will be refreshed.

		bytesRead, addr, err := udpListener.ReadFrom(buffer)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		} else if err != nil {
			p.log.Info("Error reading UDP packet from proxy UDP connection", err)
		} else {
			q := p.getPacketQueue(addr, udpListener, config)
			q.Enqueue(bytes.Clone(buffer[:bytesRead]))
		}
	}
}

func (p *netProxy) shutdownAllUDPStreams() {
	p.udpStreams.Range(func(clientAddr string, stream udpStream) bool {
		p.shutdownUDPStream(stream.clientAddr)
		return true
	})
}

func (p *netProxy) shutdownInactiveUDPStreams() {
	p.udpStreams.Range(func(clientAddr string, stream udpStream) bool {
		lastUsed := stream.lastUsed.Load()
		if time.Since(lastUsed) > UdpStreamInactivityTimeout {
			p.shutdownUDPStream(stream.clientAddr)
		}
		return true
	})
}

func (p *netProxy) shutdownUDPStream(clientAddr net.Addr) {
	stream, loaded := p.udpStreams.LoadAndDelete(clientAddr.String())
	if !loaded {
		return
	}

	if stream.ctx != nil {
		stream.cancel()
		// Endpoint streaming goroutine will close the stream listener.
	}
}

func (p *netProxy) tryStartingUDPStream(stream udpStream, proxyConn net.PacketConn, config ProxyConfig) bool {
	endpoint, err := chooseEndpoint(&config)
	if err != nil {
		// No endpoints yet
		return false
	}
	endpointAddr, err := net.ResolveUDPAddr("udp", networking.AddressAndPort(endpoint.Address, endpoint.Port))
	if err != nil {
		p.log.Error(err, "Could not resolve endpoint address", "EndpointAddress", endpoint.Address, "EndpointPort", endpoint.Port)
		return false
	}

	lc := net.ListenConfig{}
	ctx, cancel := context.WithCancel(p.lifetimeCtx)
	streamListener, err := lc.ListenPacket(ctx, "udp", fmt.Sprintf("%s:", p.listenAddress))
	if err != nil {
		p.log.Error(err, "Could not create an endpoint listener for client", "ClientAddr", stream.clientAddr.String())
		cancel()
		return false
	}

	stream.ctx = ctx
	stream.cancel = cancel
	p.udpStreams.Store(stream.clientAddr.String(), stream)

	// TODO: log the UDP stream shape: client addr -> proxy conn addr (proxy) stream listener addr -> endpoint addr
	go p.streamClientPackets(stream, streamListener, endpointAddr)
	go p.streamEndpointPackets(stream, streamListener, proxyConn)
	return true
}

func (p *netProxy) tryStartExistingUDPStreams(config ProxyConfig, proxyConn net.PacketConn) {
	p.udpStreams.Range(func(clientAddr string, stream udpStream) bool {
		if stream.ctx == nil {
			// Stream is not running, try starting it
			_ = p.tryStartingUDPStream(stream, proxyConn, config)
		}
		return true
	})
}

func (p *netProxy) getPacketQueue(clientAddr net.Addr, proxyConn net.PacketConn, config ProxyConfig) *queue.ConcurrentBoundedQueue[[]byte] {
	stream, loaded := p.udpStreams.LoadOrStoreNew(clientAddr.String(), func() udpStream {
		return udpStream{
			clientAddr: clientAddr,
			packets:    queue.NewConcurrentBoundedQueue[[]byte](MaxCachedUdpPackets),
			lastUsed:   concurrency.AtomicTimeNow(),
			ctx:        nil,
			cancel:     nil,
		}
	})

	if !loaded || stream.ctx == nil {
		_ = p.tryStartingUDPStream(stream, proxyConn, config)
	}

	if !loaded {
		// Attempt to clean up inactive streams when a new client shows up.
		go p.shutdownInactiveUDPStreams()
	}

	return stream.packets
}

func (p *netProxy) streamClientPackets(
	stream udpStream,
	streamListener net.PacketConn,
	endpointAddr *net.UDPAddr,
) {
	newPackets := stream.packets.NewData()

	for {
		select {
		// No need to check the proxy lifetime context because stream context is a child of it.
		case <-stream.ctx.Done():
			return

		case <-newPackets:
			for {
				if stream.ctx.Err() != nil {
					return
				}

				buf, ok := stream.packets.Dequeue()
				if !ok {
					break
				}

				if err := streamListener.SetWriteDeadline(time.Now().Add(p.writeTimeout)); err != nil {
					p.log.V(1).Info("Error setting write deadline for UDP endpoint connection", "Error", err.Error(), "EndpointAddress", endpointAddr.String())
					p.shutdownUDPStream(stream.clientAddr)
					return
				}

				written, err := streamListener.WriteTo(buf, endpointAddr)
				if err != nil {
					// Handle write deadline exceeded like any other (unexpected) error for UDP connections
					p.log.V(1).Info("Error writing to UDP endpoint connection", "Error", err.Error(), "EndpointAddress", endpointAddr.String())
					p.shutdownUDPStream(stream.clientAddr)
					return
				}
				if written != len(buf) {
					p.log.V(1).Info("UDP endpoint connection write did not transmit the whole packet", "BytesSubmitted", len(buf), "BytesWritten", written, "EndpointAddress", endpointAddr.String())
					p.shutdownUDPStream(stream.clientAddr)
					return
				}

				stream.lastUsed.TryAdvancingTo(time.Now())
			}
		}
	}
}

func (p *netProxy) streamEndpointPackets(
	stream udpStream,
	streamListener net.PacketConn,
	proxyConn net.PacketConn,
) {
	buffer := make([]byte, UdpPacketBufferSize)
	defer streamListener.Close()

	for {
		if stream.ctx.Err() != nil {
			return
		}

		if err := streamListener.SetReadDeadline(time.Now().Add(p.readTimeout)); err != nil {
			p.log.V(1).Info("Error setting read deadline for UDP endpoint connection", "Error", err.Error(), "EndpointConnectionAddress", streamListener.LocalAddr().String())
			p.shutdownUDPStream(stream.clientAddr)
			return
		}

		bytesRead, _, readErr := streamListener.ReadFrom(buffer)
		if errors.Is(readErr, os.ErrDeadlineExceeded) {
			continue
		} else if readErr != nil {
			p.log.Info("Error reading UDP packet from proxy UDP connection", "Error", readErr.Error(), "EndpointConnectionAddress", streamListener.LocalAddr().String())
			p.shutdownUDPStream(stream.clientAddr)
			return
		}

		if err := proxyConn.SetWriteDeadline(time.Now().Add(p.writeTimeout)); err != nil {
			p.log.V(1).Info("Error setting write deadline for UDP proxy connection", "Error", err.Error(), "ClientAddress", stream.clientAddr.String())
			p.shutdownUDPStream(stream.clientAddr)
			return
		}

		written, writeErr := proxyConn.WriteTo(buffer[:bytesRead], stream.clientAddr)
		if writeErr != nil {
			// Handle write deadline exceeded like any other (unexpected) error for UDP connections
			p.log.V(1).Info("Error writing to UDP proxy connection", "Error", writeErr.Error(), "ClientAddress", stream.clientAddr.String())
			p.shutdownUDPStream(stream.clientAddr)
			return
		}
		if written != len(buffer[:bytesRead]) {
			p.log.V(1).Info("UDP proxy connection write did not transmit the whole packet", "BytesSubmitted", len(buffer[:bytesRead]), "BytesWritten", written, "ClientAddress", stream.clientAddr.String())
			p.shutdownUDPStream(stream.clientAddr)
			return
		}

		stream.lastUsed.TryAdvancingTo(time.Now())

	}
}

func chooseEndpoint(config *ProxyConfig) (*Endpoint, error) {
	// Select a random endpoint from the configured list
	if len(config.Endpoints) == 0 {
		return nil, errors.New("no endpoints configured")
	}

	return &config.Endpoints[rand.Intn(len(config.Endpoints))], nil
}

func getStreamDescription(streamID string, incoming, outgoing net.Conn) string {
	return fmt.Sprintf(
		"%s: %s -> %s (proxy) %s -> %s",
		streamID,
		incoming.RemoteAddr().String(),
		incoming.LocalAddr().String(),
		outgoing.LocalAddr().String(),
		outgoing.RemoteAddr().String(),
	)
}

var _ Proxy = &netProxy{}
