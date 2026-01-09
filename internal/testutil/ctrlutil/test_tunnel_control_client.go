// Copyright (c) Microsoft Corporation. All rights reserved.

package ctrlutil

// An implementation of TunnelControlClient for testing purposes.

import (
	"context"
	mathrand "math/rand"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	stdproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/slices"
)

const InvalidTunnelID uint32 = 0

type TestTunnelControlClient struct {
	lock *sync.Mutex

	// Tunnels that this test client is aware of/can handle, keyed by their fingerprint.
	tunnels map[dcptunproto.TunnelRequestFingerprint]*dcptunproto.TunnelSpec

	// The next tunnel ID to use (monotonically increasing).
	nextTunnelId uint32
}

func NewTestTunnelControlClient() *TestTunnelControlClient {
	return &TestTunnelControlClient{
		lock:         &sync.Mutex{},
		tunnels:      make(map[dcptunproto.TunnelRequestFingerprint]*dcptunproto.TunnelSpec),
		nextTunnelId: InvalidTunnelID + 1,
	}
}

// Enables a tunnel, making the subsequent PrepareTunnel calls return the provided tunnel spec.
func (t *TestTunnelControlClient) EnableTunnel(tunnelSpec *dcptunproto.TunnelSpec) {
	t.lock.Lock()
	defer t.lock.Unlock()

	if tunnelSpec == nil {
		panic("EnableTunnel called with nil tunnelSpec")
	}

	rf := TestFingerprint(tunnelSpec)
	t.tunnels[rf] = tunnelSpec
}

// Disables a tunnel, making the subsequent PrepareTunnel calls for the tunnel's fingerprint fail.
// Returns true if the tunnel was found and disabled, false otherwise (i.e. it was not previously enabled, or known).
func (t *TestTunnelControlClient) DisableTunnel(rf dcptunproto.TunnelRequestFingerprint) bool {
	t.lock.Lock()
	defer t.lock.Unlock()

	_, found := t.tunnels[rf]
	delete(t.tunnels, rf)
	return found
}

// Gets the tunnel spec for the specified fingerprint.
// Can be used to verify that a tunnel has been prepared by the ContainerNetworkTunnelProxy controller
// (by checking if the tunnel ID has been assigned to the tunnel spec).
func (t *TestTunnelControlClient) GetTunnelSpec(rf dcptunproto.TunnelRequestFingerprint) *dcptunproto.TunnelSpec {
	t.lock.Lock()
	defer t.lock.Unlock()

	tunnelSpec, found := t.tunnels[rf]
	if !found {
		return nil
	}

	return stdproto.CloneOf(tunnelSpec)
}

//
// TunnelControlClient interface methods
//

func (t *TestTunnelControlClient) PrepareTunnel(ctx context.Context, req *dcptunproto.TunnelReq, _ ...grpc.CallOption) (*dcptunproto.TunnelSpec, error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	f := req.Fingerprint()
	tunnelSpec, found := t.tunnels[f]
	if !found {
		return nil, status.Errorf(codes.Internal, "unexpected PrepareTunnel request for unknown fingerprint: %v", f)
	}

	if tunnelSpec.TunnelRef == nil {
		tunnelSpec.TunnelRef = &dcptunproto.TunnelRef{}
	}
	if tunnelSpec.TunnelRef.TunnelId != nil {
		// Tunnel already prepared.
		return stdproto.CloneOf(tunnelSpec), nil
	}

	tunnelSpec.TunnelRef.TunnelId = stdproto.Uint32(t.nextTunnelId)
	t.nextTunnelId++

	if req.GetClientProxyAddress() != "" && req.GetClientProxyAddress() != networking.IPv4AllInterfaceAddress {
		tunnelSpec.ClientProxyAddresses = []string{req.GetClientProxyAddress()}
	} else {
		tunnelSpec.ClientProxyAddresses = []string{networking.IPv4LocalhostDefaultAddress}
	}

	var port int32
	if networking.IsValidPort(int(req.GetClientProxyPort())) {
		port = req.GetClientProxyPort()
	} else {
		// Assign a random port in the ephemeral port range.
		port = int32(networking.DefaultEphemeralPortRangeStart) + mathrand.Int31n(networking.DefaultEphemeralPortRangeEnd-networking.DefaultEphemeralPortRangeStart)
	}
	tunnelSpec.ClientProxyPort = &port

	t.tunnels[f] = tunnelSpec
	return stdproto.CloneOf(tunnelSpec), nil
}

func (t *TestTunnelControlClient) DeleteTunnel(ctx context.Context, tr *dcptunproto.TunnelRef, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	if tr == nil || tr.TunnelId == nil || *tr.TunnelId == InvalidTunnelID {
		return nil, status.Errorf(codes.InvalidArgument, "DeleteTunnel request has invalid tunnel reference")
	}

	found := false

	for rf, tunnelSpec := range t.tunnels {
		if tunnelSpec.GetTunnelRef().GetTunnelId() == *tr.TunnelId {
			found = true
			// Reset the tunnel spec to unprepared state, but keep it enabled.
			tunnelSpec.TunnelRef.TunnelId = nil
			tunnelSpec.ClientProxyAddresses = nil
			tunnelSpec.ClientProxyPort = nil
			t.tunnels[rf] = tunnelSpec
			break
		}
	}

	if !found {
		return nil, status.Errorf(codes.NotFound, "DeleteTunnel request for unknown tunnel ID: %d", *tr.TunnelId)
	} else {
		return &emptypb.Empty{}, nil
	}
}

func (t *TestTunnelControlClient) Shutdown(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (t *TestTunnelControlClient) NewStreamsConnection(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[dcptunproto.NewStreamResult, dcptunproto.StreamRef], error) {
	// TODO: Implement as necessary
	return nil, nil
}

// Creates a TunnelRequestFingerprint from a proto.TunnelSpec, compatible with ones created from TunnelReq.
// Used for testing only.
func TestFingerprint(ts *dcptunproto.TunnelSpec) dcptunproto.TunnelRequestFingerprint {
	allAddresses := slices.Accumulate[string, string](ts.GetClientProxyAddresses(), func(acc, addr string) string { return acc + addr })
	return dcptunproto.TunnelRequestFingerprint{
		ServerAddress:      ts.GetServerAddress(),
		ServerPort:         ts.GetServerPort(),
		ClientProxyAddress: allAddresses,
		ClientProxyPort:    ts.GetClientProxyPort(),
	}
}

var _ dcptunproto.TunnelControlClient = (*TestTunnelControlClient)(nil)
