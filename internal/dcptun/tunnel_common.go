/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"runtime"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	stdproto "google.golang.org/protobuf/proto"

	"github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/concurrency"
)

type StreamID uint64
type TunnelID uint32

type TunnelStream struct {
	TunnelID
	StreamID
}

const (
	invalidTunnelID TunnelID = 0
	invalidStreamID StreamID = 0

	errMsgProxyDisposed = "tunnel proxy has been disposed, no further operations are allowed"

	controlStreamReadyHeader = "dcp-tunnel-stream-ready"

	// Binary name for the client proxy (Linux executable). Compare with DCPTUN_CLIENT_BINARY in Makefile.
	ClientBinaryName = "dcptun_c"

	// ControlKeepaliveTime is the idle interval before either end probes the
	// long-lived tunnel control connection.
	ControlKeepaliveTime = time.Minute

	// ControlKeepaliveMinTime is the minimum interval enforced by the client proxy
	// between incoming keepalive pings, leaving margin for host/VM clock skew and jitter.
	ControlKeepaliveMinTime = 20 * time.Second

	// ControlKeepaliveTimeout bounds how long either end of the tunnel control
	// connection waits for a keepalive acknowledgement.
	ControlKeepaliveTimeout = 20 * time.Second

	// defaultDataConnectionPreambleTimeout bounds how long the client proxy waits
	// for an incoming data connection to send its fixed-length authentication preamble.
	defaultDataConnectionPreambleTimeout = 10 * time.Second

	// defaultMaxConcurrentStreamCreationsPerTunnel bounds simultaneous backend dial
	// workers per tunnel so one slow backend cannot starve other tunnels.
	defaultMaxConcurrentStreamCreationsPerTunnel uint8 = 8
)

var (
	latestTunnelID TunnelID = invalidTunnelID
	latestStreamID StreamID = invalidStreamID

	// Binary name for the server proxy
	ServerBinaryName string
)

// Holds tunnel data needed by a server-side or client-side proxy.
type tunnelData[StreamInfo any] struct {
	// Optional caller-assigned logical tunnel name.
	tunnelName string

	// The current tunnel spec. Owners replace the pointer under their state lock;
	// each pointed-to spec remains immutable.
	spec *proto.TunnelSpec

	// Active streams and associated information.
	streams map[StreamID]StreamInfo

	// Whether the tunnel may accept and establish new streams.
	accepting *atomic.Bool

	// True if the tunnel is "deleted" and no further connections should be accepted.
	deleted *atomic.Bool

	// Serializes mutable control operations that span local state and peer RPCs.
	operationLock *concurrency.ContextAwareLock
}

func newTunnelData[StreamInfo any](spec *proto.TunnelSpec) *tunnelData[StreamInfo] {
	accepting := &atomic.Bool{}
	accepting.Store(true)
	return &tunnelData[StreamInfo]{
		spec:          spec,
		streams:       make(map[StreamID]StreamInfo),
		accepting:     accepting,
		deleted:       &atomic.Bool{},
		operationLock: concurrency.NewContextAwareLock(),
	}
}

func ensureValidTunnelRequest(tr *proto.TunnelReq) error {
	if tr == nil {
		return status.Error(codes.InvalidArgument, "tunnel request data must be provided")
	}

	if tr.GetTunnelName() == "" {
		return status.Error(codes.InvalidArgument, "tunnel name must be provided")
	}

	if !networking.IsValidPort(int(tr.GetServerPort())) {
		return status.Errorf(codes.InvalidArgument, "server port must be a valid port number (1-65535), got %d", tr.GetServerPort())
	}

	if tr.GetServerAddress() == "" {
		tr.ServerAddress = stdproto.String(networking.Localhost)
	}

	if tr.GetClientProxyAddress() == "" {
		tr.ClientProxyAddress = stdproto.String(networking.IPv4AllInterfaceAddress)
	}

	if !networking.IsBindablePort(int(tr.GetClientProxyPort())) {
		return status.Errorf(codes.InvalidArgument, "client port must be a valid port number (1-65535), or zero, got %d", tr.GetClientProxyPort())
	}

	for _, excludedPort := range tr.GetExcludedClientProxyPorts() {
		if !networking.IsValidPort(int(excludedPort)) {
			return status.Errorf(codes.InvalidArgument, "excluded client port must be a valid port number (1-65535), got %d", excludedPort)
		}
	}

	return nil
}

func ensureValidTunnelUpdate(update *proto.TunnelUpdate) error {
	if update == nil || update.GetTunnelRef().GetTunnelId() == 0 {
		return status.Error(codes.InvalidArgument, "tunnel ID must be provided")
	}
	if update.AcceptNewStreams == nil {
		return status.Error(codes.InvalidArgument, "accept new streams state must be provided")
	}
	if !update.GetAcceptNewStreams() {
		return nil
	}
	if !networking.IsValidPort(int(update.GetServerPort())) {
		return status.Errorf(codes.InvalidArgument, "server port must be a valid port number (1-65535), got %d", update.GetServerPort())
	}
	if update.GetServerAddress() == "" {
		update.ServerAddress = stdproto.String(networking.Localhost)
	}
	return nil
}

func init() {
	bn := "dcptun"
	if runtime.GOOS == "windows" {
		bn += ".exe"
	}
	ServerBinaryName = bn
}
