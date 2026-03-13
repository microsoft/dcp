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
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"

	"github.com/microsoft/dcp/internal/networking"
)

const (
	caKeyLength           = 4096
	keyLength             = 2048
	defaultExpirationDays = 7
	serialNumberBits      = 128
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
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), serialNumberBits)

	// Serial numbers must be positive (RFC 5280 §4.1.2.2), so generate in [0, limit) and add 1.
	caSerialNumber, caSerialNumberErr := cryptorand.Int(cryptorand.Reader, serialNumberLimit)
	if caSerialNumberErr != nil {
		return ServerCertificateData{}, fmt.Errorf("failed to generate CA serial number: %w", caSerialNumberErr)
	}
	caSerialNumber.Add(caSerialNumber, big.NewInt(1))

	serverSerialNumber, serverSerialNumberErr := cryptorand.Int(cryptorand.Reader, serialNumberLimit)
	if serverSerialNumberErr != nil {
		return ServerCertificateData{}, fmt.Errorf("failed to generate server serial number: %w", serverSerialNumberErr)
	}
	serverSerialNumber.Add(serverSerialNumber, big.NewInt(1))

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

	// Generate the subject key ID for the CA certificate as a SHA-256 hash of the CA public key
	caPublicKeyBytes, caPublicKeyBytesErr := asn1.Marshal(*caKey.Public().(*rsa.PublicKey))
	if caPublicKeyBytesErr != nil {
		return ServerCertificateData{}, fmt.Errorf("failed to marshal CA public key: %w", caPublicKeyBytesErr)
	}
	caSubjectKeyId := sha256.Sum256(caPublicKeyBytes)

	now := time.Now()

	// Template for the CA certificate
	ca := &x509.Certificate{
		SerialNumber: caSerialNumber,
		Subject: pkix.Name{
			CommonName: ip.String(),
		},
		NotBefore:             now,
		NotAfter:              now.AddDate(0, 0, defaultExpirationDays),
		SubjectKeyId:          caSubjectKeyId[:],
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	caBytes, caErr := x509.CreateCertificate(cryptorand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if caErr != nil {
		return ServerCertificateData{}, fmt.Errorf("failed to create CA certificate: %w", caErr)
	}

	// Generate the subject key ID for the server certificate as a SHA-256 hash of the server public key
	serverPublicKeyBytes, serverPublicKeyBytesErr := asn1.Marshal(*serverKey.Public().(*rsa.PublicKey))
	if serverPublicKeyBytesErr != nil {
		return ServerCertificateData{}, fmt.Errorf("failed to marshal server public key: %w", serverPublicKeyBytesErr)
	}
	serverSubjectKeyId := sha256.Sum256(serverPublicKeyBytes)

	// Template for the server certificate
	server := &x509.Certificate{
		SerialNumber:   serverSerialNumber,
		Subject:        pkix.Name{},
		IPAddresses:    []net.IP{ip},
		NotBefore:      now,
		NotAfter:       now.AddDate(0, 0, defaultExpirationDays),
		SubjectKeyId:   serverSubjectKeyId[:],
		AuthorityKeyId: caSubjectKeyId[:],
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		KeyUsage:       x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
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

// ValidateCertificateFiles validates that the given certificate and key files are readable,
// contain valid PEM data, that the certificate and key form a valid pair, and that the server
// certificate is valid for localhost.
// Returns the server address to use based on what the certificate covers and the system's
// IP version preference. IP addresses are preferred over DNS names. If the preferred IP
// version is covered, that address is used. Otherwise, it falls back to whichever localhost
// address the cert is valid for.
func ValidateCertificateFiles(certFile, keyFile string) (string, error) {
	certPEM, certReadErr := os.ReadFile(certFile)
	if certReadErr != nil {
		return "", fmt.Errorf("unable to read certificate file '%s': %w", certFile, certReadErr)
	}

	keyPEM, keyReadErr := os.ReadFile(keyFile)
	if keyReadErr != nil {
		return "", fmt.Errorf("unable to read private key file '%s': %w", keyFile, keyReadErr)
	}

	_, tlsErr := tls.X509KeyPair(certPEM, keyPEM)
	if tlsErr != nil {
		return "", fmt.Errorf("certificate and private key are not a valid pair: %w", tlsErr)
	}

	// Parse the server certificate (first in the chain) and determine which localhost address it covers.
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("no certificate found in PEM data")
	}

	cert, parseErr := x509.ParseCertificate(block.Bytes)
	if parseErr != nil {
		return "", fmt.Errorf("failed to parse certificate: %w", parseErr)
	}

	// Check each localhost address using the standard library's VerifyHostname,
	// which checks both SANs and the Subject CN per RFC 6125.
	// VerifyHostname expects raw host/IP strings, so strip brackets from IPv6 addresses.
	hasIPv4 := cert.VerifyHostname(networking.IPv4LocalhostDefaultAddress) == nil
	hasIPv6 := cert.VerifyHostname(networking.ToStandaloneAddress(networking.IPv6LocalhostDefaultAddress)) == nil
	hasLocalhost := cert.VerifyHostname(networking.Localhost) == nil

	if !hasIPv4 && !hasIPv6 && !hasLocalhost {
		return "", fmt.Errorf("certificate is not valid for localhost; '%s', %s, or %s must be a valid certificate subject",
			networking.Localhost, networking.IPv4LocalhostDefaultAddress, networking.IPv6LocalhostDefaultAddress)
	}

	// Prefer IP addresses, respecting the system's IP version preference.
	preference := networking.GetIpVersionPreference()

	if preference == networking.IpVersionPreference4 && hasIPv4 {
		return networking.IPv4LocalhostDefaultAddress, nil
	}
	if preference == networking.IpVersionPreference6 && hasIPv6 {
		return networking.IPv6LocalhostDefaultAddress, nil
	}

	// No preference or preferred version not covered; prefer any IP over "localhost".
	if hasIPv4 {
		return networking.IPv4LocalhostDefaultAddress, nil
	}
	if hasIPv6 {
		return networking.IPv6LocalhostDefaultAddress, nil
	}
	return networking.Localhost, nil
}

// ExtractRootCertificate extracts the trust anchor from PEM-encoded certificate data.
// It identifies the self-signed certificate (where Issuer equals Subject) regardless of
// PEM ordering. For a single self-signed cert it returns that cert. For a chain it returns
// the root CA after verifying that the leaf cert chains to it through any intermediates present.
// Returns an error if no self-signed certificate is found or if the chain is invalid.
func ExtractRootCertificate(certPEM []byte) ([]byte, error) {
	var certs []*x509.Certificate
	var certBlocks []*pem.Block
	rest := certPEM

	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, parseErr := x509.ParseCertificate(block.Bytes)
		if parseErr != nil {
			continue
		}
		certs = append(certs, cert)
		certBlocks = append(certBlocks, block)
	}

	if len(certs) == 0 {
		return nil, fmt.Errorf("no certificates found in PEM data")
	}

	// Find the self-signed certificate (trust anchor). A true self-signed cert has
	// matching Issuer/Subject AND has signed itself. The RawIssuer/RawSubject check
	// alone only identifies "self-issued" certs, which can include intermediates.
	// We use CheckSignature (not CheckSignatureFrom) to verify the cryptographic
	// signature without CA constraint checks, since self-signed non-CA certs
	// (e.g. ASP.NET dev certs) are valid trust anchors.
	rootIdx := -1
	for i, cert := range certs {
		if bytes.Equal(cert.RawIssuer, cert.RawSubject) &&
			cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) == nil {
			rootIdx = i
			break
		}
	}
	if rootIdx < 0 {
		return nil, fmt.Errorf("no self-signed root certificate found in PEM data")
	}

	// For chains with multiple certs, verify the leaf actually chains to the root.
	if len(certs) > 1 {
		leafIdx := findLeafCert(certs, rootIdx)
		if leafIdx < 0 {
			return nil, fmt.Errorf("unable to identify leaf certificate in chain")
		}

		rootPool := x509.NewCertPool()
		rootPool.AddCert(certs[rootIdx])

		intermediatePool := x509.NewCertPool()
		for i, cert := range certs {
			if i == leafIdx || i == rootIdx {
				continue
			}
			intermediatePool.AddCert(cert)
		}

		verifyOpts := x509.VerifyOptions{
			Roots:         rootPool,
			Intermediates: intermediatePool,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		}
		if _, verifyErr := certs[leafIdx].Verify(verifyOpts); verifyErr != nil {
			return nil, fmt.Errorf("certificate chain verification failed: %w", verifyErr)
		}
	}

	var buffer bytes.Buffer
	rootPemBlock := &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certBlocks[rootIdx].Bytes,
	}
	if encodeErr := pem.Encode(&buffer, rootPemBlock); encodeErr != nil {
		return nil, fmt.Errorf("failed to PEM encode root certificate: %w", encodeErr)
	}

	return buffer.Bytes(), nil
}

// findLeafCert returns the index of the leaf certificate in the slice. The leaf is
// the certificate whose Subject does not appear as the Issuer of any other certificate
// in the bundle (i.e. it does not sign any other cert).
func findLeafCert(certs []*x509.Certificate, rootIdx int) int {
	for i, cert := range certs {
		if i == rootIdx {
			continue
		}
		isIssuer := false
		for j, other := range certs {
			if j == i {
				continue
			}
			if bytes.Equal(other.RawIssuer, cert.RawSubject) {
				isIssuer = true
				break
			}
		}
		if !isIssuer {
			return i
		}
	}
	return -1
}
