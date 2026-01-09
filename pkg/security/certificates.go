/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package security

import (
	"bytes"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"
	mathrand "math/rand"
	"net"
	"time"
)

const (
	caKeyLength           = 4096
	keyLength             = 2048
	defaultExpirationDays = 7
)

type ServerCertificateData struct {
	CACertificate []byte          // Self-signed CA certificate, not encoded
	ServerCert    []byte          // Server certificate, not encoded
	ServerKey     *rsa.PrivateKey // Server private key
}

// Returns PEM-encoded server and certificate authority certificates.
func (scd ServerCertificateData) Certificate() ([]byte, error) {
	return PEMEncodeCertificates(scd.ServerCert, scd.CACertificate)
}

// Returns PEM-encoded server private key.
func (scd ServerCertificateData) ServerPrivateKey() ([]byte, error) {
	return PEMEncodePrivateKey(scd.ServerKey)
}

// Returns PEM-encoded CA certificate.
func (scd ServerCertificateData) CA() ([]byte, error) {
	return PEMEncodeCertificates(scd.CACertificate)
}

// Generates a self-signed certificate authority, server certificate, and a server private key
// for securing network connections. Returned certificates are raw (not PEM-encoded).
func GenerateServerCertificate(ip net.IP) (ServerCertificateData, error) {
	// Generate keys for the CA certificate
	caKey, caKeyErr := rsa.GenerateKey(cryptorand.Reader, caKeyLength)
	if caKeyErr != nil {
		return ServerCertificateData{}, fmt.Errorf("failed to generate CA key: %w", caKeyErr)
	}

	// Generate keys for the server certificate
	serverKey, serverKeyErr := rsa.GenerateKey(cryptorand.Reader, keyLength)
	if serverKeyErr != nil {
		return ServerCertificateData{}, fmt.Errorf("failed to generate server key: %w", serverKeyErr)
	}

	// Template for the CA certificate
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(mathrand.Int63()),
		Subject: pkix.Name{
			CommonName: ip.String(),
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(0, 0, defaultExpirationDays),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	caBytes, caErr := x509.CreateCertificate(cryptorand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if caErr != nil {
		return ServerCertificateData{}, fmt.Errorf("failed to create CA certificate: %w", caErr)
	}

	// Generate the subject ID for the server certificate as a SHA256 hash of the server public key
	serverPublicKeyBytes, serverPublicKeyBytesErr := asn1.Marshal(*serverKey.Public().(*rsa.PublicKey))
	if serverPublicKeyBytesErr != nil {
		return ServerCertificateData{}, fmt.Errorf("failed to marshal server public key: %w", serverPublicKeyBytesErr)
	}
	serverPublicKeySubjectId := sha256.Sum256(serverPublicKeyBytes)

	// Template for the server certificate
	server := &x509.Certificate{
		SerialNumber: big.NewInt(mathrand.Int63()),
		Subject:      pkix.Name{},
		IPAddresses:  []net.IP{ip},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(0, 0, defaultExpirationDays),
		SubjectKeyId: serverPublicKeySubjectId[:],
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}

	serverBytes, serverErr := x509.CreateCertificate(cryptorand.Reader, server, ca, &serverKey.PublicKey, caKey)
	if serverErr != nil {
		return ServerCertificateData{}, fmt.Errorf("failed to create server certificate: %w", serverErr)
	}

	return ServerCertificateData{
		CACertificate: caBytes,
		ServerCert:    serverBytes,
		ServerKey:     serverKey,
	}, nil
}

// PEM-encodes a set of certificates into a common buffer
func PEMEncodeCertificates(certs ...[]byte) ([]byte, error) {
	if len(certs) == 0 {
		return nil, fmt.Errorf("no certificates provided for PEM encoding")
	}

	var buffer bytes.Buffer

	for _, cert := range certs {
		pemBlock := &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: cert,
		}
		if err := pem.Encode(&buffer, pemBlock); err != nil {
			return nil, fmt.Errorf("failed to PEM encode certificate: %w", err)
		}
	}

	return buffer.Bytes(), nil
}

// PEM-encodes a private key
func PEMEncodePrivateKey(key *rsa.PrivateKey) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("private key is nil")
	}

	var buffer bytes.Buffer

	pemBlock := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	if err := pem.Encode(&buffer, pemBlock); err != nil {
		return nil, fmt.Errorf("failed to PEM encode private key: %w", err)
	}

	return buffer.Bytes(), nil
}
