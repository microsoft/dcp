/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

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

type preparedTestTunnel struct {
	spec                         *dcptunproto.TunnelSpec
	templateFingerprint          dcptunproto.TunnelRequestFingerprint
	requestedClientProxyPort     int32
	allowClientProxyPortFallback bool
}

type TestTunnelControlClient struct {
	lock *sync.Mutex

	// Enabled tunnel templates keyed by endpoint fingerprint (TunnelName == "", AllowClientProxyPortFallback == false).
	tunnels map[dcptunproto.TunnelRequestFingerprint]*dcptunproto.TunnelSpec

	// Active prepared tunnels keyed by mandatory TunnelName.
	prepared map[string]*preparedTestTunnel

	// Maps each prepared tunnel ID back to its TunnelName.
	preparedNames map[uint32]string

	// Whether each prepared tunnel accepts new streams, keyed by tunnel ID.
	accepting map[uint32]bool

	// One-shot errors returned after preparing a tunnel, keyed by request or template fingerprint.
	prepareResponseErrors map[dcptunproto.TunnelRequestFingerprint]error

	// The next tunnel ID to use (monotonically increasing).
	nextTunnelId uint32
}

func NewTestTunnelControlClient() *TestTunnelControlClient {
	return &TestTunnelControlClient{
		lock:                  &sync.Mutex{},
		tunnels:               make(map[dcptunproto.TunnelRequestFingerprint]*dcptunproto.TunnelSpec),
		prepared:              make(map[string]*preparedTestTunnel),
		preparedNames:         make(map[uint32]string),
		accepting:             make(map[uint32]bool),
		prepareResponseErrors: make(map[dcptunproto.TunnelRequestFingerprint]error),
		nextTunnelId:          InvalidTunnelID + 1,
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
	if tunnelSpec.GetTunnelRef().GetTunnelId() == InvalidTunnelID {
		t.purgeTemplatePreparedLocked(rf)
	}
	t.tunnels[rf] = tunnelSpec
}

// Disables a tunnel, making the subsequent PrepareTunnel calls for the tunnel's fingerprint fail.
// Returns true if the tunnel was found and disabled, false otherwise (i.e. it was not previously enabled, or known).
func (t *TestTunnelControlClient) DisableTunnel(rf dcptunproto.TunnelRequestFingerprint) bool {
	t.lock.Lock()
	defer t.lock.Unlock()

	_, found := t.tunnels[rf]
	delete(t.tunnels, rf)
	t.purgeTemplatePreparedLocked(rf)
	return found
}

func (t *TestTunnelControlClient) purgeTemplatePreparedLocked(templateFingerprint dcptunproto.TunnelRequestFingerprint) {
	for tunnelName, entry := range t.prepared {
		if entry.templateFingerprint != templateFingerprint {
			continue
		}
		tid := entry.spec.GetTunnelRef().GetTunnelId()
		delete(t.prepared, tunnelName)
		delete(t.preparedNames, tid)
		delete(t.accepting, tid)
	}
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

// TunnelAccepting reports whether the prepared tunnel currently accepts new streams.
func (t *TestTunnelControlClient) TunnelAccepting(tunnelID uint32) (bool, bool) {
	t.lock.Lock()
	defer t.lock.Unlock()

	accepting, found := t.accepting[tunnelID]
	return accepting, found
}

// HasPreparedTunnel reports whether a tunnel with tunnelName is currently prepared.
func (t *TestTunnelControlClient) HasPreparedTunnel(tunnelName string) bool {
	t.lock.Lock()
	defer t.lock.Unlock()

	_, found := t.prepared[tunnelName]
	return found
}

// FailNextPrepareResponse returns err after the matching request has been applied.
func (t *TestTunnelControlClient) FailNextPrepareResponse(rf dcptunproto.TunnelRequestFingerprint, err error) {
	t.lock.Lock()
	defer t.lock.Unlock()
	t.prepareResponseErrors[rf] = err
}

//
// TunnelControlClient interface methods
//

func (t *TestTunnelControlClient) isClientPortBoundByOtherTunnelLocked(port int32, requestingTunnelName string) bool {
	if !networking.IsValidPort(int(port)) {
		return false
	}
	for tunnelName, entry := range t.prepared {
		if tunnelName != requestingTunnelName && entry.spec.GetClientProxyPort() == port {
			return true
		}
	}
	return false
}

func (t *TestTunnelControlClient) consumePrepareResponseErrorLocked(
	requestFingerprint dcptunproto.TunnelRequestFingerprint,
	templateFingerprint dcptunproto.TunnelRequestFingerprint,
) error {
	if responseErr, failRequest := t.prepareResponseErrors[requestFingerprint]; failRequest {
		delete(t.prepareResponseErrors, requestFingerprint)
		return responseErr
	}
	if responseErr, failTemplate := t.prepareResponseErrors[templateFingerprint]; failTemplate {
		delete(t.prepareResponseErrors, templateFingerprint)
		return responseErr
	}
	return nil
}

func (t *TestTunnelControlClient) PrepareTunnel(ctx context.Context, req *dcptunproto.TunnelReq, _ ...grpc.CallOption) (*dcptunproto.TunnelSpec, error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	tunnelName := req.GetTunnelName()
	if tunnelName == "" {
		return nil, status.Error(codes.InvalidArgument, "tunnel name must be provided")
	}

	requestFingerprint := req.Fingerprint()
	if existing, exists := t.prepared[tunnelName]; exists {
		sameRequest := existing.spec.GetServerAddress() == req.GetServerAddress() &&
			existing.spec.GetServerPort() == req.GetServerPort() &&
			existing.requestedClientProxyPort == req.GetClientProxyPort() &&
			existing.allowClientProxyPortFallback == req.GetAllowClientProxyPortFallback() &&
			!slices.Contains(req.GetExcludedClientProxyPorts(), existing.spec.GetClientProxyPort())
		if sameRequest {
			result := stdproto.CloneOf(existing.spec)
			if responseErr := t.consumePrepareResponseErrorLocked(requestFingerprint, existing.templateFingerprint); responseErr != nil {
				return nil, responseErr
			}
			return result, nil
		}
	}

	// Enabled tunnels are keyed by endpoint (without TunnelName) and the port they are prepared on.
	// A request that allows port fallback emulates a failed bind of its preferred port when that port
	// is excluded, already bound by another tunnel, or not enabled, resolving to the random-port template.
	f := requestFingerprint
	f.TunnelName = ""
	f.AllowClientProxyPortFallback = false
	tunnelSpec, found := t.tunnels[f]
	if f.ClientProxyPort != 0 {
		portUnavailable := t.isClientPortBoundByOtherTunnelLocked(f.ClientProxyPort, tunnelName)
		if req.GetAllowClientProxyPortFallback() {
			if portUnavailable || slices.Contains(req.GetExcludedClientProxyPorts(), f.ClientProxyPort) {
				found = false
			}
			if !found {
				f.ClientProxyPort = 0
				tunnelSpec, found = t.tunnels[f]
			}
		} else if portUnavailable {
			return nil, status.Errorf(codes.Internal, "client proxy port %d is already bound", f.ClientProxyPort)
		}
	}
	if !found {
		return nil, status.Errorf(codes.Internal, "unexpected PrepareTunnel request for unknown fingerprint: %v", requestFingerprint)
	}

	// If the same named tunnel is rebinding its published port after a simulated proxy restart,
	// retire the previous generation's entry on this shared test client.
	if previous, hadPrevious := t.prepared[tunnelName]; hadPrevious {
		previousID := previous.spec.GetTunnelRef().GetTunnelId()
		delete(t.preparedNames, previousID)
		delete(t.accepting, previousID)
		delete(t.prepared, tunnelName)
	}

	preparedSpec := stdproto.CloneOf(tunnelSpec)
	preparedSpec.TunnelRef = &dcptunproto.TunnelRef{
		TunnelId: stdproto.Uint32(t.nextTunnelId),
	}
	t.accepting[t.nextTunnelId] = true
	t.nextTunnelId++

	if req.GetClientProxyAddress() != "" && req.GetClientProxyAddress() != networking.IPv4AllInterfaceAddress {
		preparedSpec.ClientProxyAddresses = []string{req.GetClientProxyAddress()}
	} else {
		preparedSpec.ClientProxyAddresses = []string{networking.IPv4LocalhostDefaultAddress}
	}

	var port int32
	if networking.IsValidPort(int(f.ClientProxyPort)) {
		port = f.ClientProxyPort
	} else {
		excludedPorts := make(map[int32]struct{}, len(req.GetExcludedClientProxyPorts()))
		for _, excludedPort := range req.GetExcludedClientProxyPorts() {
			excludedPorts[excludedPort] = struct{}{}
		}
		for {
			// Assign a random port in the ephemeral port range.
			port = int32(networking.DefaultEphemeralPortRangeStart) + mathrand.Int31n(networking.DefaultEphemeralPortRangeEnd-networking.DefaultEphemeralPortRangeStart)
			if _, excluded := excludedPorts[port]; !excluded && !t.isClientPortBoundByOtherTunnelLocked(port, tunnelName) {
				break
			}
		}
	}
	preparedSpec.ClientProxyPort = &port

	preparedID := preparedSpec.GetTunnelRef().GetTunnelId()
	t.prepared[tunnelName] = &preparedTestTunnel{
		spec:                         preparedSpec,
		templateFingerprint:          f,
		requestedClientProxyPort:     req.GetClientProxyPort(),
		allowClientProxyPortFallback: req.GetAllowClientProxyPortFallback(),
	}
	t.preparedNames[preparedID] = tunnelName

	// Mirror onto the enabled template so GetTunnelSpec callers using TestFingerprint observe preparation.
	tunnelSpec.TunnelRef = stdproto.CloneOf(preparedSpec.TunnelRef)
	tunnelSpec.ClientProxyAddresses = slices.Map[string](preparedSpec.ClientProxyAddresses, func(addr string) string { return addr })
	tunnelSpec.ClientProxyPort = stdproto.Int32(port)
	t.tunnels[f] = tunnelSpec

	result := stdproto.CloneOf(preparedSpec)
	if responseErr := t.consumePrepareResponseErrorLocked(requestFingerprint, f); responseErr != nil {
		return nil, responseErr
	}
	return result, nil
}

func (t *TestTunnelControlClient) DeleteTunnel(ctx context.Context, tr *dcptunproto.TunnelRef, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	if tr == nil || (tr.GetTunnelId() == InvalidTunnelID && tr.GetTunnelName() == "") {
		return nil, status.Errorf(codes.InvalidArgument, "DeleteTunnel request has invalid tunnel reference")
	}

	targetTunnelID := tr.GetTunnelId()
	var tunnelName string
	var found bool
	if targetTunnelID == InvalidTunnelID {
		entry, foundByName := t.prepared[tr.GetTunnelName()]
		if !foundByName {
			return &emptypb.Empty{}, nil
		}
		targetTunnelID = entry.spec.GetTunnelRef().GetTunnelId()
		tunnelName = tr.GetTunnelName()
		found = true
	} else {
		tunnelName, found = t.preparedNames[targetTunnelID]
	}
	if !found {
		return nil, status.Errorf(codes.NotFound, "DeleteTunnel request for unknown tunnel ID: %d", targetTunnelID)
	}

	removedEntry := t.prepared[tunnelName]
	delete(t.prepared, tunnelName)
	delete(t.preparedNames, targetTunnelID)
	delete(t.accepting, targetTunnelID)

	if removedEntry != nil {
		templateFingerprint := removedEntry.templateFingerprint
		if templateSpec, templateExists := t.tunnels[templateFingerprint]; templateExists &&
			templateSpec.GetTunnelRef().GetTunnelId() == targetTunnelID {
			var replacementSpec *dcptunproto.TunnelSpec
			for _, candidate := range t.prepared {
				if candidate.templateFingerprint == templateFingerprint {
					replacementSpec = candidate.spec
					break
				}
			}
			if replacementSpec != nil {
				templateSpec.TunnelRef = stdproto.CloneOf(replacementSpec.TunnelRef)
				templateSpec.ClientProxyAddresses = slices.Map[string](replacementSpec.ClientProxyAddresses, func(addr string) string { return addr })
				templateSpec.ClientProxyPort = stdproto.Int32(replacementSpec.GetClientProxyPort())
			} else {
				// Reset the tunnel spec to unprepared state, but keep it enabled.
				templateSpec.TunnelRef.TunnelId = nil
				templateSpec.ClientProxyAddresses = nil
				templateSpec.ClientProxyPort = nil
			}
			t.tunnels[templateFingerprint] = templateSpec
		}
	}

	return &emptypb.Empty{}, nil
}

func (t *TestTunnelControlClient) UpdateTunnel(_ context.Context, update *dcptunproto.TunnelUpdate, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	tunnelID := update.GetTunnelRef().GetTunnelId()
	if tunnelID == InvalidTunnelID {
		return nil, status.Error(codes.InvalidArgument, "UpdateTunnel request has invalid tunnel reference")
	}

	tunnelName, found := t.preparedNames[tunnelID]
	if !found {
		return nil, status.Errorf(codes.NotFound, "UpdateTunnel request for unknown tunnel ID: %d", tunnelID)
	}
	entry := t.prepared[tunnelName]
	if entry == nil {
		return nil, status.Errorf(codes.NotFound, "UpdateTunnel request for unknown tunnel ID: %d", tunnelID)
	}

	if update.GetAcceptNewStreams() {
		entry.spec.ServerAddress = stdproto.String(update.GetServerAddress())
		entry.spec.ServerPort = stdproto.Int32(update.GetServerPort())

		templateFingerprint := entry.templateFingerprint
		if templateSpec, templateExists := t.tunnels[templateFingerprint]; templateExists {
			delete(t.tunnels, templateFingerprint)
			templateFingerprint.ServerAddress = update.GetServerAddress()
			templateFingerprint.ServerPort = update.GetServerPort()
			templateSpec.ServerAddress = stdproto.String(update.GetServerAddress())
			templateSpec.ServerPort = stdproto.Int32(update.GetServerPort())
			t.tunnels[templateFingerprint] = templateSpec
			entry.templateFingerprint = templateFingerprint
		}
	}
	t.accepting[tunnelID] = update.GetAcceptNewStreams()
	return &emptypb.Empty{}, nil
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
