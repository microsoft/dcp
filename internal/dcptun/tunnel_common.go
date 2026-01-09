/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"runtime"
	"sync/atomic"

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

	// Binary name for the client proxy (Linux executable). Compare with DCPTUN_CLIENT_BINARY in Makefile.
	ClientBinaryName = "dcptun_c"
)

var (
	latestTunnelID TunnelID = invalidTunnelID
	latestStreamID StreamID = invalidStreamID

	// Binary name for the server proxy
	ServerBinaryName string
)

// Holds tunnel data needed by a server-side or client-side proxy.
type tunnelData[StreamInfo any] struct {
	// The spec of the tunnel. Once the tunnelData instance is created, the spec is immutable.
	spec *proto.TunnelSpec

	// Active streams and associated information.
	streams map[StreamID]StreamInfo

	// True if the tunnel is "deleted" and no further connections should be accepted.
	deleted *atomic.Bool
}

func newTunnelData[StreamInfo any](spec *proto.TunnelSpec) *tunnelData[StreamInfo] {
	return &tunnelData[StreamInfo]{
		spec:    spec,
		streams: make(map[StreamID]StreamInfo),
		deleted: &atomic.Bool{},
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

	return nil
}

func init() {
	bn := "dcptun"
	if runtime.GOOS == "windows" {
		bn += ".exe"
	}
	ServerBinaryName = bn
}
