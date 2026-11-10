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

	// ControlKeepaliveTime is the idle interval before probing the long-lived
	// tunnel control connection.
	ControlKeepaliveTime = time.Minute

	// ControlKeepaliveTimeout bounds how long the control client waits for a
	// keepalive acknowledgement.
	ControlKeepaliveTimeout = 20 * time.Second
)

var (
	latestTunnelID TunnelID = invalidTunnelID
	latestStreamID StreamID = invalidStreamID

	// Binary name for the server proxy
	ServerBinaryName string
)

// Holds tunnel data needed by a server-side or client-side proxy.
type tunnelData[StreamInfo any] struct {
	// The current tunnel spec. Owners replace the pointer under their state lock;
	// each pointed-to spec remains immutable.
	spec *proto.TunnelSpec

	// Active streams and associated information.
	streams map[StreamID]StreamInfo

	// Whether the tunnel may accept and establish new streams.
	accepting *atomic.Bool

	// True if the tunnel is "deleted" and no further connections should be accepted.
	deleted *atomic.Bool
}

func newTunnelData[StreamInfo any](spec *proto.TunnelSpec) *tunnelData[StreamInfo] {
	accepting := &atomic.Bool{}
	accepting.Store(true)
	return &tunnelData[StreamInfo]{
		spec:      spec,
		streams:   make(map[StreamID]StreamInfo),
		accepting: accepting,
		deleted:   &atomic.Bool{},
	}
}

func ensureValidTunnelRequest(tr *proto.TunnelReq) error {
	if tr == nil {
		return status.Error(codes.InvalidArgument, "tunnel request data must be provided")
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
