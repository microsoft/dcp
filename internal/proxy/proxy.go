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
	"github.com/smallnest/chanx"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/queue"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

const (
	DefaultReadWriteTimeout  = 5 * time.Second
	DefaultConnectionTimeout = 5 * time.Second
	MaxCachedUdpPackets      = 20

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

var errInvalidWrite = errors.New("invalid write operation")

type ProxyConfig struct {
	Endpoints []Endpoint
}

func (pc *ProxyConfig) Clone() ProxyConfig {
	endpoints := make([]Endpoint, len(pc.Endpoints))
	copy(endpoints, pc.Endpoints)
	return ProxyConfig{Endpoints: endpoints}
}

func (pc *ProxyConfig) String() string {
	var b bytes.Buffer
	b.WriteString("[")
	for i, endpoint := range pc.Endpoints {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(networking.AddressAndPort(endpoint.Address, endpoint.Port))
	}
	b.WriteString("]")
	return b.String()
}

type Endpoint struct {
	Address string `yaml:"address"`
	Port    int32  `yaml:"port"`
}

type ProxyState uint32

const (
	ProxyStateInitial  ProxyState = 0x1
	ProxyStateRunning  ProxyState = 0x2
	ProxyStateFailed   ProxyState = 0x4
	ProxyStateFinished ProxyState = 0x8
	ProxyStateAny      ProxyState = 0xFFFFFFFF
)

func (s ProxyState) String() string {
	switch s {
	case ProxyStateInitial:
		return "Initial"
	case ProxyStateRunning:
		return "Running"
	case ProxyStateFailed:
		return "Failed"
	case ProxyStateFinished:
		return "Finished"
	default:
		return "Unknown"
	}
}

// A "UDP stream" binds the client with the Endpoint that is serving it.
// Packets sent by the client will be forwarded to the Endpoint using dedicated PacketConn conection.
// Packets received from that PacketConn will be sent back to the client.
type udpStream struct {
	clientAddr net.Addr                              // Address of the client that is being served by this stream
	packets    *queue.ConcurrentBoundedQueue[[]byte] // Queue of packets from the client
	lastUsed   *concurrency.AtomicTime               // Time when this stream was last used
	ctx        context.Context                       // Context that will be cancelled when the stream is to be retired.
	cancel     context.CancelFunc                    // The function to cancel the stream context.
}

type Proxy struct {
	mode          apiv1.PortProtocol
	ListenAddress string
	ListenPort    int32

	EffectiveAddress string
	EffectivePort    int32

	endpointConfigLoadedChannel *chanx.UnboundedChan[ProxyConfig]
	configurationApplied        *concurrency.AutoResetEvent
	readWriteTimeout            time.Duration
	connectionTimeout           time.Duration
	streamSeqNo                 uint32

	udpStreams *syncmap.Map[string, udpStream]

	lifetimeCtx context.Context
	log         logr.Logger
	state       ProxyState
	lock        sync.Locker
}

// Creates a reverse proxy instance.
//
// After Start() method is called, the proxy will listen on the specified address and port
// (which cannot be changed after the proxy is created), and forward incoming connections
// to the endpoints specified by the configuration (supplied via Configure() method).
// The proxy will stop when the lifetime context is cancelled.
//
// If the address is empty, the proxy will listen on localhost. The effectiveAddress field will contain the actual listened-on IPv4 or IPv6 address.
// If the port is 0, the proxy will listen on a random port. The effectivePort field will contain the actual listened-on port.
func NewProxy(mode apiv1.PortProtocol, listenAddress string, listenPort int32, lifetimeCtx context.Context, log logr.Logger) *Proxy {
	if mode != apiv1.TCP && mode != apiv1.UDP {
		panic(fmt.Errorf("unsupported proxy mode: %s", mode))
	}

	p := Proxy{
		mode:          mode,
		ListenAddress: listenAddress,
		ListenPort:    listenPort,

		endpointConfigLoadedChannel: chanx.NewUnboundedChan[ProxyConfig](lifetimeCtx, 1),
		configurationApplied:        concurrency.NewAutoResetEvent(false),
		readWriteTimeout:            DefaultReadWriteTimeout,
		connectionTimeout:           DefaultConnectionTimeout,

		udpStreams: &syncmap.Map[string, udpStream]{},

		lifetimeCtx: lifetimeCtx,
		log:         log,
		state:       ProxyStateInitial,
		lock:        &sync.Mutex{},
	}

	return &p
}

func (p *Proxy) Start() error {
	if err := p.setState(ProxyStateInitial, ProxyStateRunning); err != nil {
		return fmt.Errorf("proxy cannot be started: %w", err)
	}

	if p.ListenAddress == "" {
		p.ListenAddress = "localhost"
	}

	lc := net.ListenConfig{}
	if p.mode == apiv1.TCP {
		tcpListener, err := lc.Listen(p.lifetimeCtx, "tcp", networking.AddressAndPort(p.ListenAddress, p.ListenPort))
		if err != nil {
			_ = p.setState(ProxyStateAny, ProxyStateFailed)
			return err
		}

		p.EffectiveAddress = networking.IpToString(tcpListener.Addr().(*net.TCPAddr).IP)
		p.EffectivePort = int32(tcpListener.Addr().(*net.TCPAddr).Port)

		go p.runTCP(tcpListener)
	} else if p.mode == apiv1.UDP {
		udpListener, err := lc.ListenPacket(p.lifetimeCtx, "udp", networking.AddressAndPort(p.ListenAddress, p.ListenPort))
		if err != nil {
			_ = p.setState(ProxyStateAny, ProxyStateFailed)
			return err
		}

		p.EffectiveAddress = networking.IpToString(udpListener.LocalAddr().(*net.UDPAddr).IP)
		p.EffectivePort = int32(udpListener.LocalAddr().(*net.UDPAddr).Port)

		go p.runUDP(udpListener)
	}

	return nil
}

func (p *Proxy) Configure(newConfig ProxyConfig) error {
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

func (p *Proxy) State() ProxyState {
	p.lock.Lock()
	defer p.lock.Unlock()
	return p.state
}

func (p *Proxy) setState(expectedState, newState ProxyState) error {
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

func (p *Proxy) stop(listener io.Closer) {
	const errMsg = "Error stopping proxy"
	// This Close call will stop TCP Accept / UDP ReadFrom calls, which will ultimately cause the runTCP / runUDP functions to exit
	if err := listener.Close(); err != nil {
		p.log.Error(err, errMsg)
	}

	_ = p.setState(ProxyStateAny, ProxyStateFinished)
}

func (p *Proxy) runTCP(tcpListener net.Listener) {
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
	connectionChannel := chanx.NewUnboundedChan[net.Conn](p.lifetimeCtx, 1)
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

func (p *Proxy) handleTCPConnection(currentConfig ProxyConfig, incoming net.Conn) {
	err := resiliency.RetryExponential(p.lifetimeCtx, func() error {
		streamErr := p.startTCPStream(incoming, &currentConfig)
		if errors.Is(streamErr, errTcpDialFailed) {
			// Retryable error, incoming connection still alive
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

func (p *Proxy) startTCPStream(incoming net.Conn, config *ProxyConfig) error {
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
	if outgoing, dialErr := d.DialContext(dialContext, "tcp", ap); dialErr != nil {
		p.log.V(1).Info(fmt.Sprintf("Error establishing TCP connection to %s, will try another endpoint", ap))
		// Do not close incoming connection
		return fmt.Errorf("%w: %w", errTcpDialFailed, dialErr)
	} else {
		streamCtx, streamCtxCancel := context.WithCancel(p.lifetimeCtx)
		copyingWg := &sync.WaitGroup{}
		copyingWg.Add(2)
		streamID := strconv.FormatUint(uint64(atomic.AddUint32(&p.streamSeqNo, 1)), 10)

		_ = context.AfterFunc(streamCtx, func() {
			copyingWg.Wait() // Do not close either connection until both streams have finished copying.

			if p.log.V(1).Enabled() {
				p.log.V(1).Info(fmt.Sprintf("Closing connections associated with TCP stream %s from %s to %s",
					streamID,
					incoming.RemoteAddr().String(),
					outgoing.RemoteAddr().String()),
				)
			}

			_ = incoming.Close()
			_ = outgoing.Close()
		})

		if p.log.V(1).Enabled() {
			p.log.V(1).Info("Started TCP stream", "Stream", getStreamDescription(streamID, incoming, outgoing))
		}

		go p.copyStream(streamID+" (in)", streamCtx, streamCtxCancel, copyingWg, incoming, outgoing)
		go p.copyStream(streamID+" (out)", streamCtx, streamCtxCancel, copyingWg, outgoing, incoming)
		return nil
	}
}

// Copies data from incoming to outgoing connection until there is no more data to copy,
// or the passed context is cancelled. The passed context corresponds to the lifetime of the (bi-directional) stream,
// so if either half of the stream is done/encounters an error, the other half will be stopped.
// When copying is done, the passed WaitGroup is decremented.
func (p *Proxy) copyStream(
	streamID string,
	streamCtx context.Context,
	streamCtxCancel context.CancelFunc,
	copyingWg *sync.WaitGroup,
	incoming net.Conn,
	outgoing net.Conn,
) {
	defer func() {
		streamCtxCancel()
		copyingWg.Done()
	}()

	// Buffered network stream immediately starts copying data between the two connections and returns when the stream reports
	// an error (either io.EOF for normal termination or another error for abnormal termination).
	stream := StreamNetworkData(streamCtx, TcpDataBufferSize, incoming, outgoing, DefaultReadWriteTimeout)

	if errors.Join(stream.WriteError, stream.ReadError) == nil {
		p.log.Error(
			fmt.Errorf("TCP proxy stream ended prematurely"),
			"bufferedNetworkStream returned with no given reason",
			"Stream", getStreamDescription(streamID, incoming, outgoing),
			"StreamOutcome", stream.LogProperties(),
		)
	} else {
		if p.log.V(1).Enabled() {
			var msg string
			if errors.Is(stream.ReadError, io.EOF) {
				msg = "Client closed incoming connection"
			} else if errors.Is(stream.WriteError, io.EOF) {
				msg = "The outgoing connection was terminated; we will terminate client connection accordingly"
			} else {
				// There are many reasons why a network I/O operation may fail,
				// and the failures are reported differently by different APIs and on different platforms.
				// That is why we generally do not log errors from those operations exept in specific circumstances,
				// such as DNS name resolution or initial connection establishment.
				// In all other cases we just shut down the communication stream and let the client(s) retry.

				msg = "An error copying TCP stream"
			}
			p.log.V(1).Info(
				msg,
				"Stream", getStreamDescription(streamID, incoming, outgoing),
				"StreamOutcome", stream.LogProperties(),
			)
		}
	}
}

type DeadlineReader interface {
	io.Reader
	SetReadDeadline(t time.Time) error
}

type DeadlineWriter interface {
	io.Writer
	SetWriteDeadline(t time.Time) error
}

func (p *Proxy) runUDP(udpListener net.PacketConn) {
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

		if err := udpListener.SetReadDeadline(time.Now().Add(p.readWriteTimeout)); err != nil {
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

func (p *Proxy) shutdownAllUDPStreams() {
	p.udpStreams.Range(func(clientAddr string, stream udpStream) bool {
		p.shutdownUDPStream(stream.clientAddr)
		return true
	})
}

func (p *Proxy) shutdownInactiveUDPStreams() {
	p.udpStreams.Range(func(clientAddr string, stream udpStream) bool {
		lastUsed := stream.lastUsed.Load()
		if time.Since(lastUsed) > UdpStreamInactivityTimeout {
			p.shutdownUDPStream(stream.clientAddr)
		}
		return true
	})
}

func (p *Proxy) shutdownUDPStream(clientAddr net.Addr) {
	stream, loaded := p.udpStreams.LoadAndDelete(clientAddr.String())
	if !loaded {
		return
	}

	if stream.ctx != nil {
		stream.cancel()
		// Endpoint streaming goroutine will close the stream listener.
	}
}

func (p *Proxy) tryStartingUDPStream(stream udpStream, proxyConn net.PacketConn, config ProxyConfig) bool {
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
	streamListener, err := lc.ListenPacket(ctx, "udp", fmt.Sprintf("%s:", p.ListenAddress))
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

func (p *Proxy) tryStartExistingUDPStreams(config ProxyConfig, proxyConn net.PacketConn) {
	p.udpStreams.Range(func(clientAddr string, stream udpStream) bool {
		if stream.ctx == nil {
			// Stream is not running, try starting it
			_ = p.tryStartingUDPStream(stream, proxyConn, config)
		}
		return true
	})
}

func (p *Proxy) getPacketQueue(clientAddr net.Addr, proxyConn net.PacketConn, config ProxyConfig) *queue.ConcurrentBoundedQueue[[]byte] {
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

func (p *Proxy) streamClientPackets(
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

				if err := streamListener.SetWriteDeadline(time.Now().Add(p.readWriteTimeout)); err != nil {
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

func (p *Proxy) streamEndpointPackets(
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

		if err := streamListener.SetReadDeadline(time.Now().Add(p.readWriteTimeout)); err != nil {
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

		if err := proxyConn.SetWriteDeadline(time.Now().Add(p.readWriteTimeout)); err != nil {
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
