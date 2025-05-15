// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/networking"
)

type Endpoint struct {
	Address string `yaml:"address"`
	Port    int32  `yaml:"port"`
}

type DeadlineReader interface {
	io.Reader
	SetReadDeadline(t time.Time) error
}

type DeadlineWriter interface {
	io.Writer
	SetWriteDeadline(t time.Time) error
}

type DeadlineReaderWriter interface {
	DeadlineReader
	DeadlineWriter
	SetDeadline(t time.Time) error
	Close() error
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

// Represents a reverse proxy.
//
// After Start() method is called, the proxy will listen on the specified address and port
// (which cannot be changed after the proxy is created), and forward incoming connections
// to the endpoints specified by the configuration (supplied via Configure() method).
// The proxy will stop when the lifetime context is cancelled (passed via ProxyFactory.CreateProxy()).
type Proxy interface {
	Start() error
	Configure(ProxyConfig) error
	State() ProxyState
	ListenAddress() string
	ListenPort() int32
	EffectiveAddress() string
	EffectivePort() int32
}

// Creates a reverse proxy.
//
// If the listenAddress is empty, the proxy will listen on localhost.
// The Proxy.EffectiveAddress() returns the actual IPv4 or IPv6 address used for listening.
// If the port is 0, the proxy will listen on a random port. Proxy.EffectivePort() will return the actual listened-on port.
type ProxyFactory func(
	mode apiv1.PortProtocol,
	listenAddress string,
	listenPort int32,
	lifetimeCtx context.Context,
	log logr.Logger,
) Proxy

var ErrWriteQueueFull = errors.New("write queue is full")

// ProxyConn defines an interface a network connection wrapper that helps the proxy
// use a network connection in a goroutine-safe, asynchronous way. All operations are goroutine-safe, except for Run().
//
// ProxyConn does not do any network connection management (e.g. dialing, closing),
// It only reads and writes data to the connection, and sets read/write deadlines.
type ProxyConn interface {
	// Queues a write of the given data to the connection.
	// The error returned (if any) indicates the failure to queue the write.
	// The actual write operation may fail asynchronously, and the error will be reported via Result().
	// If the returned error is ErrWriteQueueFull, the client should wait a bit and retry.
	QueueWrite(data []byte) error

	// Returns the channel that lets the client check whether the run loop has finished.
	Done() <-chan struct{}

	// Returns the connection-terminating result, if available.
	// Typically it is either an I/O error (e.g. the other side closed the connection,
	// represented by io.EOF), or the context associated with the connection was cancelled.
	Result() *NetworkStreamResult

	// Runs the connection's main loop (for reading and writing data).
	// The goroutine that calls Run() is the only goroutine that interacts with the underlying net.TCPConn connection.
	// The connections always work in pairs, and the main loop will stop when either connection fails or is closed by the client.
	Run(ProxyConn, logr.Logger)

	// Stops reading data from the connection, but writes any pending data to the connection,
	// then stops the main loop.
	// DrainAndStop() is a blocking operation that is goroutine-safe and idempotent.
	DrainAndStop()
}

// If set, the proxy will not log TCP stream execution errors upon stream completion.
// This is useful for handling application shutdown,
// when TCP stream abrupt terminations are expected.
var SilenceTcpStreamCompletionErrors = &atomic.Bool{}
