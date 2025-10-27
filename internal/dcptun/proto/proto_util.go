// Copyright (c) Microsoft Corporation. All rights reserved.

package proto

import (
	"fmt"
	"slices"

	stdproto "google.golang.org/protobuf/proto"
)

func (s *TunnelSpec) Same(other *TunnelSpec) bool {
	if s == nil || other == nil {
		return s == other
	}
	return s.GetTunnelRef().GetTunnelId() == other.GetTunnelRef().GetTunnelId() &&
		s.GetServerPort() == other.GetServerPort() &&
		s.GetServerAddress() == other.GetServerAddress() &&
		s.GetClientProxyPort() == other.GetClientProxyPort() &&
		slices.Equal(s.GetClientProxyAddresses(), other.GetClientProxyAddresses())
}

func (s *TunnelSpec) LogString() string {
	logTS := stdproto.CloneOf(s)
	logTS.DataConnectionToken = []byte("<redacted>")
	return logTS.String()
}

// TunnelRequestFingerprint is a set of fields that uniquely identify a tunnel request.
// It is used to determine if two tunnel requests are effectively the same, and to de-duplicate tunnel requests.
type TunnelRequestFingerprint struct {
	ServerAddress      string
	ServerPort         int32
	ClientProxyAddress string
	ClientProxyPort    int32
}

func (f TunnelRequestFingerprint) String() string {
	return fmt.Sprintf("{Server %s:%d <-> Client %s:%d}", f.ServerAddress, f.ServerPort, f.ClientProxyAddress, f.ClientProxyPort)
}

// Creates a TunnelRequestFingerprint from a proto.TunnelReq
func (tr *TunnelReq) Fingerprint() TunnelRequestFingerprint {
	return TunnelRequestFingerprint{
		ServerAddress:      tr.GetServerAddress(),
		ServerPort:         tr.GetServerPort(),
		ClientProxyAddress: tr.GetClientProxyAddress(),
		ClientProxyPort:    tr.GetClientProxyPort(),
	}
}
