// Copyright (c) Microsoft Corporation. All rights reserved.

package dcptun

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"

	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/security"
)

type TunnelProxySecurityConfig struct {
	// Base64-encoded CA certificate for securing the control connection.
	// If empty, an insecure connection will be used.
	CACertBase64 string `json:"ca_cert_base64,omitempty"`

	// Base64-encoded server certificate for securing the control connection.
	// If empty, an insecure connection will be used.
	ServerCertBase64 string `json:"server_cert_base64,omitempty"`

	// Base64-encoded server private key for securing the control connection.
	// If empty, an insecure connection will be used.
	ServerKeyBase64 string `json:"server_key_base64,omitempty"`
}

type TunnelProxyConfig struct {
	TunnelProxySecurityConfig

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

func (tc TunnelProxySecurityConfig) HasCompleteCertificateData() bool {
	return tc.CACertBase64 != "" && tc.ServerCertBase64 != "" && tc.ServerKeyBase64 != ""
}

func (tc TunnelProxySecurityConfig) GetTlsConfig() (*tls.Config, error) {
	if !tc.HasCompleteCertificateData() {
		return nil, fmt.Errorf("insufficient certificate and server key data to create TLS configuration")
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

	caCertPEM, caCertDecodeErr := base64.StdEncoding.DecodeString(tc.CACertBase64)
	if caCertDecodeErr != nil {
		return nil, caCertDecodeErr
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("failed to add CA certificate to pool")
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    caCertPool,
	}
	return tlsConfig, nil
}

func (tc TunnelProxySecurityConfig) GetClientPool() (*x509.CertPool, error) {
	caCertPEM, caCertDecodeErr := base64.StdEncoding.DecodeString(tc.CACertBase64)
	if caCertDecodeErr != nil {
		return nil, caCertDecodeErr
	}

	serverCertPEM, certDecodeErr := base64.StdEncoding.DecodeString(tc.ServerCertBase64)
	if certDecodeErr != nil {
		return nil, certDecodeErr
	}

	caCertPool := x509.NewCertPool()
	certs := []byte{}
	certs = append(certs, serverCertPEM...)
	certs = append(certs, caCertPEM...)
	if !caCertPool.AppendCertsFromPEM(certs) {
		return nil, fmt.Errorf("failed to add CA certificate to pool")
	}

	return caCertPool, nil
}

func NewTunnelProxySecurityConfig() (TunnelProxySecurityConfig, error) {
	srvCert, certsErr := security.GenerateServerCertificate(net.ParseIP(networking.IPv4LocalhostDefaultAddress))
	if certsErr != nil {
		return TunnelProxySecurityConfig{}, certsErr
	}
	// Base64-encode certificates for passing as command-line arguments
	caCertPEM, caCertErr := srvCert.CA()
	if caCertErr != nil {
		return TunnelProxySecurityConfig{}, caCertErr
	}

	serverCertPEM, serverCertErr := srvCert.Certificate()
	if serverCertErr != nil {
		return TunnelProxySecurityConfig{}, serverCertErr
	}

	serverKeyPEM, serverKeyErr := srvCert.ServerPrivateKey()
	if serverKeyErr != nil {
		return TunnelProxySecurityConfig{}, serverKeyErr
	}

	config := TunnelProxySecurityConfig{
		CACertBase64:     base64.StdEncoding.EncodeToString(caCertPEM),
		ServerCertBase64: base64.StdEncoding.EncodeToString(serverCertPEM),
		ServerKeyBase64:  base64.StdEncoding.EncodeToString(serverKeyPEM),
	}
	return config, nil
}
