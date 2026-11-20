/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

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
	if s == nil {
		return "<nil>"
	}
	logTS := stdproto.CloneOf(s)
	logTS.DataConnectionToken = []byte("<redacted>")
	return logTS.String()
}

// ExcludedClientProxyPortsApply reports whether excluded_client_proxy_ports constrains the listener port.
// Exclusions are ignored when the request asks for an exact, non-zero client proxy port.
func (tr *TunnelReq) ExcludedClientProxyPortsApply() bool {
	return tr.GetClientProxyPort() == 0 || tr.GetAllowClientProxyPortFallback()
}

// TunnelRequestFingerprint is a set of fields that uniquely identify a tunnel request.
// It is used to determine if two tunnel requests are effectively the same, and to de-duplicate tunnel requests.
type TunnelRequestFingerprint struct {
	TunnelName                   string
	ServerAddress                string
	ServerPort                   int32
	ClientProxyAddress           string
	ClientProxyPort              int32
	AllowClientProxyPortFallback bool
}

func (f TunnelRequestFingerprint) String() string {
	return fmt.Sprintf(
		"{Tunnel %q: Server %s:%d <-> Client %s:%d, port fallback allowed: %t}",
		f.TunnelName, f.ServerAddress, f.ServerPort, f.ClientProxyAddress, f.ClientProxyPort, f.AllowClientProxyPortFallback,
	)
}

// Creates a TunnelRequestFingerprint from a proto.TunnelReq
func (tr *TunnelReq) Fingerprint() TunnelRequestFingerprint {
	return TunnelRequestFingerprint{
		TunnelName:                   tr.GetTunnelName(),
		ServerAddress:                tr.GetServerAddress(),
		ServerPort:                   tr.GetServerPort(),
		ClientProxyAddress:           tr.GetClientProxyAddress(),
		ClientProxyPort:              tr.GetClientProxyPort(),
		AllowClientProxyPortFallback: tr.GetAllowClientProxyPortFallback(),
	}
}
