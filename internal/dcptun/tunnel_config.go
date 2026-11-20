/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"

	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/security"
)

// TunnelControlSecurityConfig holds the TLS certificates, private key, and bearer-token material
// used to secure gRPC control connections for the tunnel proxy controller, server proxy, or client proxy.
// Because it contains secret key and token material, pass it via protected files or stdin rather than
// command-line arguments or container images.
type TunnelControlSecurityConfig struct {
	// Base64-encoded PEM CA certificate used to verify the TLS server certificate of the control endpoint being dialed:
	// the server proxy's control endpoint when used by the tunnel proxy controller, or the client proxy's control endpoint
	// when used by the server proxy. Empty for the client proxy, which does not dial any control endpoint.
	PeerCACertBase64 string `json:"peer_ca_cert_base64,omitempty"`

	// Base64-encoded PEM certificate chain presented by the gRPC control server of the server proxy or client proxy.
	// Empty for the tunnel proxy controller, which does not run a control server.
	ServerCertBase64 string `json:"server_cert_base64,omitempty"`

	// Base64-encoded PEM private key for ServerCertBase64 on the server proxy or client proxy.
	// Empty for the tunnel proxy controller, which does not run a control server.
	ServerKeyBase64 string `json:"server_key_base64,omitempty"`

	// Bearer token sent on outgoing gRPC control calls from the tunnel proxy controller to the server proxy,
	// or from the server proxy to the client proxy. Empty for the client proxy.
	OutgoingToken string `json:"outgoing_token,omitempty"`

	// Base64-encoded SHA-256 hash of the bearer token required on incoming gRPC control calls
	// to the server proxy (from the tunnel proxy controller) or to the client proxy (from the server proxy).
	// Empty for the tunnel proxy controller.
	IncomingTokenHashBase64 string `json:"incoming_token_hash_base64,omitempty"`
}

// TunnelProxySecurityConfig holds the generated TLS certificates, private keys, and bearer tokens
// for both control hops of a single server-proxy/client-proxy pair (tunnel proxy controller -> server proxy
// and server proxy -> client proxy). Replacement proxy pairs must generate a new TunnelProxySecurityConfig.
type TunnelProxySecurityConfig struct {
	serverCACertBase64 string // Verifies the server proxy's control endpoint when dialed by the tunnel proxy controller.
	serverCertBase64   string
	serverKeyBase64    string
	clientCACertBase64 string // Verifies the client proxy's control endpoint when dialed by the server proxy.
	clientCertBase64   string
	clientKeyBase64    string
	controllerToken    string // Bearer token sent by the tunnel proxy controller to the server proxy.
	serverToken        string // Bearer token sent by the server proxy to the client proxy.
}

type TunnelProxyConfig struct {
	// The address for the control endpoint of the server-side tunnel proxy.
	ServerControlAddress string `json:"server_control_address"`

	// The port for the control endpoint of the server-side tunnel proxy.
	ServerControlPort int32 `json:"server_control_port"`

	// The address for the control endpoint of the client-side tunnel proxy.
	// In the context of the server-side proxy, this is the address that the server-side proxy will be using
	// to connect to the control endpoint of the client-side proxy.
	// In the context of the client-side proxy, this is the address that the client-side proxy will be listening on.
	ClientControlAddress string `json:"client_control_address"`

	// The port for the control endpoint of the client-side tunnel proxy.
	// In the context of the server-side proxy, this is the port that the server-side proxy will be using
	// to connect to the control endpoint of the client-side proxy.
	// In the context of the client-side proxy, this is the port that the client-side proxy will be listening on.
	ClientControlPort int32 `json:"client_control_port"`

	// The address for the data endpoint of the client-side tunnel proxy.
	// In the context of the server-side proxy, this is the address that the server-side proxy will be using
	// to connect to the data endpoint of the client-side proxy.
	// In the context of the client-side proxy, this is the address that the client-side proxy will be listening on.
	ClientDataAddress string `json:"client_data_address"`

	// The port for the data endpoint of the client-side tunnel proxy.
	// In the context of the server-side proxy, this is the port that the server-side proxy will be using
	// to connect to the data endpoint of the client-side proxy.
	// In the context of the client-side proxy, this is the port that the client-side proxy will be listening on.
	ClientDataPort int32 `json:"client_data_port"`
}

// TunnelProxyReadiness reports that the server proxy owns an authenticated control stream.
type TunnelProxyReadiness struct {
	ControlReady bool `json:"control_ready"`
}

func (tc TunnelControlSecurityConfig) HasServerCertificateData() bool {
	return tc.ServerCertBase64 != "" && tc.ServerKeyBase64 != ""
}

// ValidateClient verifies that PeerCACertBase64 and OutgoingToken are present and valid for dialing a control server
// (used by the tunnel proxy controller and the server proxy).
func (tc TunnelControlSecurityConfig) ValidateClient() error {
	if tc.PeerCACertBase64 == "" {
		return fmt.Errorf("peer CA certificate data is required for control client")
	}
	if _, poolErr := tc.GetClientPool(); poolErr != nil {
		return fmt.Errorf("configure peer CA certificate pool: %w", poolErr)
	}
	if tc.OutgoingToken == "" {
		return fmt.Errorf("outgoing control token is required")
	}
	return nil
}

// ValidateServer verifies that ServerCertBase64, ServerKeyBase64, and IncomingTokenHashBase64 are present and valid
// for running a control server (used by the server proxy and the client proxy).
func (tc TunnelControlSecurityConfig) ValidateServer() error {
	if !tc.HasServerCertificateData() {
		return fmt.Errorf("insufficient server certificate and key data to create TLS configuration")
	}
	if _, tlsErr := tc.GetServerTLSConfig(); tlsErr != nil {
		return fmt.Errorf("configure server TLS credentials: %w", tlsErr)
	}
	if tc.IncomingTokenHashBase64 == "" {
		return fmt.Errorf("incoming control token hash is required")
	}
	decodedHash, hashDecodeErr := base64.StdEncoding.DecodeString(tc.IncomingTokenHashBase64)
	if hashDecodeErr != nil {
		return fmt.Errorf("decode incoming control token hash: %w", hashDecodeErr)
	}
	if len(decodedHash) != sha256.Size {
		return fmt.Errorf("incoming control token hash has invalid length %d", len(decodedHash))
	}
	return nil
}

func (tc TunnelControlSecurityConfig) GetServerTLSConfig() (*tls.Config, error) {
	if !tc.HasServerCertificateData() {
		return nil, fmt.Errorf("insufficient server certificate and key data to create TLS configuration")
	}

	serverCertPEM, certDecodeErr := base64.StdEncoding.DecodeString(tc.ServerCertBase64)
	if certDecodeErr != nil {
		return nil, certDecodeErr
	}

	serverKeyPEM, keyDecodeErr := base64.StdEncoding.DecodeString(tc.ServerKeyBase64)
	if keyDecodeErr != nil {
		return nil, keyDecodeErr
	}

	serverCert, certLoadErr := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if certLoadErr != nil {
		return nil, certLoadErr
	}

	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
	}, nil
}

func (tc TunnelControlSecurityConfig) GetClientPool() (*x509.CertPool, error) {
	if tc.PeerCACertBase64 == "" {
		return nil, fmt.Errorf("peer CA certificate data is missing")
	}

	peerCACertPEM, caCertDecodeErr := base64.StdEncoding.DecodeString(tc.PeerCACertBase64)
	if caCertDecodeErr != nil {
		return nil, caCertDecodeErr
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(peerCACertPEM) {
		return nil, fmt.Errorf("failed to add peer CA certificate to pool")
	}

	return caCertPool, nil
}

// ClientProxyConfig permits incoming server-proxy RPCs without exposing host TLS keys, CA roots, or raw tokens to the container.
func (tc TunnelProxySecurityConfig) ClientProxyConfig() TunnelControlSecurityConfig {
	return TunnelControlSecurityConfig{
		ServerCertBase64:        tc.clientCertBase64,
		ServerKeyBase64:         tc.clientKeyBase64,
		IncomingTokenHashBase64: controlTokenHash(tc.serverToken),
	}
}

// ServerProxyConfig authenticates incoming controller RPCs and outgoing RPCs to the client proxy.
func (tc TunnelProxySecurityConfig) ServerProxyConfig() TunnelControlSecurityConfig {
	return TunnelControlSecurityConfig{
		PeerCACertBase64:        tc.clientCACertBase64,
		ServerCertBase64:        tc.serverCertBase64,
		ServerKeyBase64:         tc.serverKeyBase64,
		OutgoingToken:           tc.serverToken,
		IncomingTokenHashBase64: controlTokenHash(tc.controllerToken),
	}
}

// ControllerConfig authenticates the controller to its generation's server proxy without carrying server keys.
func (tc TunnelProxySecurityConfig) ControllerConfig() TunnelControlSecurityConfig {
	return TunnelControlSecurityConfig{
		PeerCACertBase64: tc.serverCACertBase64,
		OutgoingToken:    tc.controllerToken,
	}
}

// NewTunnelProxySecurityConfig creates disjoint per-hop TLS identities and directional tokens for one proxy pair.
func NewTunnelProxySecurityConfig() (TunnelProxySecurityConfig, error) {
	localhostIP := net.ParseIP(networking.IPv4LocalhostDefaultAddress)
	serverCert, serverCertsErr := security.GenerateECDSAServerCertificate(localhostIP)
	if serverCertsErr != nil {
		return TunnelProxySecurityConfig{}, fmt.Errorf("generate server proxy TLS certificate: %w", serverCertsErr)
	}
	clientCert, clientCertsErr := security.GenerateECDSAServerCertificate(localhostIP)
	if clientCertsErr != nil {
		return TunnelProxySecurityConfig{}, fmt.Errorf("generate client proxy TLS certificate: %w", clientCertsErr)
	}
	controllerToken, controllerTokenErr := security.MakeBearerToken()
	if controllerTokenErr != nil {
		return TunnelProxySecurityConfig{}, fmt.Errorf("generate controller control token: %w", controllerTokenErr)
	}
	serverToken, serverTokenErr := security.MakeBearerToken()
	if serverTokenErr != nil {
		return TunnelProxySecurityConfig{}, fmt.Errorf("generate server control token: %w", serverTokenErr)
	}

	config := TunnelProxySecurityConfig{
		serverCACertBase64: base64.StdEncoding.EncodeToString(serverCert.CACertPEM),
		serverCertBase64:   base64.StdEncoding.EncodeToString(serverCert.CertChainPEM),
		serverKeyBase64:    base64.StdEncoding.EncodeToString(serverCert.ServerKeyPEM),
		clientCACertBase64: base64.StdEncoding.EncodeToString(clientCert.CACertPEM),
		clientCertBase64:   base64.StdEncoding.EncodeToString(clientCert.CertChainPEM),
		clientKeyBase64:    base64.StdEncoding.EncodeToString(clientCert.ServerKeyPEM),
		controllerToken:    base64.RawURLEncoding.EncodeToString(controllerToken),
		serverToken:        base64.RawURLEncoding.EncodeToString(serverToken),
	}
	return config, nil
}

func controlTokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return base64.StdEncoding.EncodeToString(hash[:])
}
