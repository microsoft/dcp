/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/networking"
)

func TestExtractRootCertificate_SingleSelfSignedCert(t *testing.T) {
	certPEM := generateSelfSignedCertPEM(t)

	rootCA, err := ExtractRootCertificate(certPEM)
	require.NoError(t, err)

	// For a single self-signed cert, the root CA should be the cert itself.
	block, _ := pem.Decode(rootCA)
	require.NotNil(t, block)
	assert.Equal(t, "CERTIFICATE", block.Type)

	origBlock, _ := pem.Decode(certPEM)
	assert.Equal(t, origBlock.Bytes, block.Bytes)
}

func TestExtractRootCertificate_TwoLevelChain(t *testing.T) {
	leafPEM, rootPEM := generateTwoLevelChainPEM(t)

	// Build a chain file: leaf + root
	chainPEM := append(leafPEM, rootPEM...)

	rootCA, rootCaErr := ExtractRootCertificate(chainPEM)
	require.NoError(t, rootCaErr)

	// Should return the root CA, not the leaf cert.
	block, _ := pem.Decode(rootCA)
	require.NotNil(t, block)

	rootBlock, _ := pem.Decode(rootPEM)
	assert.Equal(t, rootBlock.Bytes, block.Bytes)
}

func TestExtractRootCertificate_ThreeCertChain(t *testing.T) {
	serverPEM, intermediatePEM, rootPEM := generateCertChainPEM(t)

	// Build a chain file: server + intermediate + root
	chainPEM := append(serverPEM, intermediatePEM...)
	chainPEM = append(chainPEM, rootPEM...)

	rootCA, rootCaErr := ExtractRootCertificate(chainPEM)
	require.NoError(t, rootCaErr)

	// Should return the root CA.
	block, _ := pem.Decode(rootCA)
	require.NotNil(t, block)

	rootBlock, _ := pem.Decode(rootPEM)
	assert.Equal(t, rootBlock.Bytes, block.Bytes)
}

func TestExtractRootCertificate_RootFirstOrder(t *testing.T) {
	serverPEM, intermediatePEM, rootPEM := generateCertChainPEM(t)

	// Reversed order: root + intermediate + server
	chainPEM := append(rootPEM, intermediatePEM...)
	chainPEM = append(chainPEM, serverPEM...)

	rootCA, rootCaErr := ExtractRootCertificate(chainPEM)
	require.NoError(t, rootCaErr)

	// Should still return the root CA regardless of ordering.
	block, _ := pem.Decode(rootCA)
	require.NotNil(t, block)

	rootBlock, _ := pem.Decode(rootPEM)
	assert.Equal(t, rootBlock.Bytes, block.Bytes)
}

func TestExtractRootCertificate_UnrelatedCerts(t *testing.T) {
	// Create a self-signed cert and a chain where the leaf is signed by a different root.
	selfSignedPEM := generateSelfSignedCertPEM(t)
	serverPEM, _, _ := generateCertChainPEM(t)

	// Bundle the unrelated chain leaf with the self-signed cert.
	chainPEM := append(serverPEM, selfSignedPEM...)

	_, extractErr := ExtractRootCertificate(chainPEM)
	assert.Error(t, extractErr)
	assert.Contains(t, extractErr.Error(), "certificate chain verification failed")
}

func TestExtractRootCertificate_NoCertificates(t *testing.T) {
	_, err := ExtractRootCertificate([]byte("no certs here"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no certificates found")
}

func TestExtractRootCertificate_EmptyInput(t *testing.T) {
	_, err := ExtractRootCertificate([]byte{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no certificates found")
}

func TestExtractRootCertificate_SkipsNonCertBlocks(t *testing.T) {
	certPEM := generateSelfSignedCertPEM(t)

	// Prepend a non-certificate PEM block
	keyBlock := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: []byte("fake key data"),
	}
	mixed := pem.EncodeToMemory(keyBlock)
	mixed = append(mixed, certPEM...)

	rootCA, err := ExtractRootCertificate(mixed)
	require.NoError(t, err)

	block, _ := pem.Decode(rootCA)
	require.NotNil(t, block)
	assert.Equal(t, "CERTIFICATE", block.Type)
}

// --- ValidateCertificate tests ---

func TestValidateCertificate_ValidIPv4(t *testing.T) {
	cert := generateTestX509Cert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	})
	addr, err := ValidateCertificate(cert)
	assert.NoError(t, err)
	assert.Equal(t, networking.IPv4LocalhostDefaultAddress, addr)
}

func TestValidateCertificate_ValidIPv6(t *testing.T) {
	cert := generateTestX509Cert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv6loopback},
	})
	addr, err := ValidateCertificate(cert)
	assert.NoError(t, err)
	assert.Equal(t, networking.IPv6LocalhostDefaultAddress, addr)
}

func TestValidateCertificate_ValidLocalhostDNS(t *testing.T) {
	cert := generateTestX509Cert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	})
	addr, err := ValidateCertificate(cert)
	assert.NoError(t, err)
	assert.Equal(t, networking.Localhost, addr)
}

func TestValidateCertificate_ExpiredCertificate(t *testing.T) {
	cert := generateTestX509Cert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-2 * time.Hour),
		NotAfter:     time.Now().Add(-1 * time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	})
	_, err := ValidateCertificate(cert)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "certificate has expired")
}

func TestValidateCertificate_NotYetValid(t *testing.T) {
	cert := generateTestX509Cert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(1 * time.Hour),
		NotAfter:     time.Now().Add(2 * time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	})
	_, err := ValidateCertificate(cert)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not yet valid")
}

func TestValidateCertificate_WrongExtKeyUsage(t *testing.T) {
	cert := generateTestX509Cert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	_, err := ValidateCertificate(cert)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not valid for server authentication")
}

func TestValidateCertificate_RejectsNonLocalhostCert(t *testing.T) {
	cert := generateTestX509Cert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("192.168.1.1")},
	})
	_, err := ValidateCertificate(cert)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not valid for localhost")
}

func TestValidateCertificate_BothIPsSelectsBasedOnPreference(t *testing.T) {
	cert := generateTestX509Cert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	})
	addr, addrErr := ValidateCertificate(cert)
	assert.NoError(t, addrErr)

	preference := networking.GetIpVersionPreference()
	switch preference {
	case networking.IpVersionPreference4:
		assert.Equal(t, networking.IPv4LocalhostDefaultAddress, addr)
	case networking.IpVersionPreference6:
		assert.Equal(t, networking.IPv6LocalhostDefaultAddress, addr)
	default:
		// No preference: either IP is acceptable.
		assert.Contains(t, []string{networking.IPv4LocalhostDefaultAddress, networking.IPv6LocalhostDefaultAddress}, addr)
	}
}

func TestValidateCertificate_PrefersIPv4OverLocalhost(t *testing.T) {
	cert := generateTestX509Cert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	})
	addr, addrErr := ValidateCertificate(cert)
	assert.NoError(t, addrErr)
	assert.Equal(t, networking.IPv4LocalhostDefaultAddress, addr)
}

func TestValidateCertificate_PrefersIPv6OverLocalhost(t *testing.T) {
	cert := generateTestX509Cert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv6loopback},
		DNSNames:     []string{"localhost"},
	})
	addr, addrErr := ValidateCertificate(cert)
	assert.NoError(t, addrErr)
	assert.Equal(t, networking.IPv6LocalhostDefaultAddress, addr)
}

func TestGenerateServerCertificate_UsesECDSAP256(t *testing.T) {
	ip := net.IPv4(127, 0, 0, 1)
	data, err := GenerateServerCertificate(ip)
	require.NoError(t, err)

	// CA cert should be self-signed ECDSA on P-256.
	caBlock, _ := pem.Decode(data.CACertPEM)
	require.NotNil(t, caBlock, "CA cert PEM block must decode")
	require.Equal(t, "CERTIFICATE", caBlock.Type)
	caCert, parseCaErr := x509.ParseCertificate(caBlock.Bytes)
	require.NoError(t, parseCaErr)
	require.True(t, caCert.IsCA, "CA certificate must have IsCA=true")
	caPub, ok := caCert.PublicKey.(*ecdsa.PublicKey)
	require.True(t, ok, "CA public key must be ECDSA, got %T", caCert.PublicKey)
	assert.Equal(t, elliptic.P256(), caPub.Curve, "CA key must use P-256")
	assert.Equal(t, x509.ECDSAWithSHA256, caCert.SignatureAlgorithm)

	// Server cert should also use ECDSA P-256 and be signed by the CA.
	serverBlock, _ := pem.Decode(data.CertChainPEM)
	require.NotNil(t, serverBlock, "server cert PEM block must decode")
	serverCert, parseSrvErr := x509.ParseCertificate(serverBlock.Bytes)
	require.NoError(t, parseSrvErr)
	serverPub, ok := serverCert.PublicKey.(*ecdsa.PublicKey)
	require.True(t, ok, "server public key must be ECDSA, got %T", serverCert.PublicKey)
	assert.Equal(t, elliptic.P256(), serverPub.Curve, "server key must use P-256")

	// Server cert must verify against the CA.
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	_, verifyErr := serverCert.Verify(x509.VerifyOptions{
		Roots:       roots,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		CurrentTime: serverCert.NotBefore.Add(time.Second),
	})
	assert.NoError(t, verifyErr, "server cert must verify against generated CA")

	// Server cert must cover the requested IP.
	assert.NoError(t, serverCert.VerifyHostname(ip.String()))

	// Per RFC 5480 §3, certificates with EC public keys MUST NOT assert the
	// keyEncipherment or dataEncipherment KeyUsage bits.
	assert.Equal(t, x509.KeyUsageDigitalSignature, serverCert.KeyUsage,
		"server cert with ECDSA key must only assert digitalSignature (RFC 5480 §3)")
	assert.Zero(t, serverCert.KeyUsage&x509.KeyUsageKeyEncipherment,
		"keyEncipherment is forbidden on EC certs (RFC 5480 §3)")
	assert.Zero(t, serverCert.KeyUsage&x509.KeyUsageDataEncipherment,
		"dataEncipherment is forbidden on EC certs (RFC 5480 §3)")

	// Private key block must be PKCS#8 "PRIVATE KEY" and pair with the server cert.
	keyBlock, _ := pem.Decode(data.ServerKeyPEM)
	require.NotNil(t, keyBlock, "server key PEM block must decode")
	assert.Equal(t, "PRIVATE KEY", keyBlock.Type)
	parsedKey, parseKeyErr := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	require.NoError(t, parseKeyErr)
	ecKey, ok := parsedKey.(*ecdsa.PrivateKey)
	require.True(t, ok, "server key must be ECDSA, got %T", parsedKey)
	assert.Equal(t, elliptic.P256(), ecKey.Curve)

	// tls.X509KeyPair (used by k8s apiserver and net/tls) must accept the pair.
	pair, pairErr := tls.X509KeyPair(data.CertChainPEM, data.ServerKeyPEM)
	require.NoError(t, pairErr, "tls.X509KeyPair must accept generated cert/key")
	require.NotEmpty(t, pair.Certificate)
}

func TestGenerateServerCertificate_IsFast(t *testing.T) {
	// Sanity check that ephemeral cert generation is well under the previous RSA cost.
	// RSA 4096 + 2048 took ~850ms on developer hardware; ECDSA P-256 takes ~1ms.
	// Use a generous bound to avoid flakes on shared CI: 250ms is still ~3x faster than
	// the old RSA path and ~250x slower than expected steady-state ECDSA, so it can
	// only fail if we regress to an RSA-class algorithm.
	start := time.Now()
	_, err := GenerateServerCertificate(net.IPv4(127, 0, 0, 1))
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Less(t, elapsed, 250*time.Millisecond, "ephemeral cert generation should be fast; took %s", elapsed)
}


// --- Test helpers ---

func generateTwoLevelChainPEM(t *testing.T) (leafPEM, rootPEM []byte) {
	t.Helper()

	// Generate root CA
	rootKey, rootKeyErr := rsa.GenerateKey(cryptorand.Reader, 2048)
	require.NoError(t, rootKeyErr)

	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Root CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	rootCertBytes, rootCertErr := x509.CreateCertificate(cryptorand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	require.NoError(t, rootCertErr)

	rootCert, rootParseErr := x509.ParseCertificate(rootCertBytes)
	require.NoError(t, rootParseErr)

	// Generate leaf cert signed directly by root
	leafKey, leafKeyErr := rsa.GenerateKey(cryptorand.Reader, 2048)
	require.NoError(t, leafKeyErr)

	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	leafCertBytes, leafCertErr := x509.CreateCertificate(cryptorand.Reader, leafTemplate, rootCert, &leafKey.PublicKey, rootKey)
	require.NoError(t, leafCertErr)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafCertBytes}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootCertBytes})
}

func generateSelfSignedCertPEM(t *testing.T) []byte {
	t.Helper()

	key, keyErr := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	require.NoError(t, keyErr)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "self-signed"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}

	certBytes, certErr := x509.CreateCertificate(cryptorand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, certErr)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certBytes})
}

func generateCertChainPEM(t *testing.T) (serverPEM, intermediatePEM, rootPEM []byte) {
	t.Helper()

	// Generate root CA
	rootKey, rootKeyErr := rsa.GenerateKey(cryptorand.Reader, 2048)
	require.NoError(t, rootKeyErr)

	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Root CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	rootCertBytes, rootCertErr := x509.CreateCertificate(cryptorand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	require.NoError(t, rootCertErr)

	rootCert, parseErr := x509.ParseCertificate(rootCertBytes)
	require.NoError(t, parseErr)

	// Generate intermediate CA
	intermediateKey, intermediateKeyErr := rsa.GenerateKey(cryptorand.Reader, 2048)
	require.NoError(t, intermediateKeyErr)

	intermediateTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "Intermediate CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	intermediateCertBytes, intermediateCertErr := x509.CreateCertificate(cryptorand.Reader, intermediateTemplate, rootCert, &intermediateKey.PublicKey, rootKey)
	require.NoError(t, intermediateCertErr)

	intermediateCert, intermediateParseErr := x509.ParseCertificate(intermediateCertBytes)
	require.NoError(t, intermediateParseErr)

	// Generate server cert
	serverKey, serverKeyErr := rsa.GenerateKey(cryptorand.Reader, 2048)
	require.NoError(t, serverKeyErr)

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "server"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	serverCertBytes, serverCertErr := x509.CreateCertificate(cryptorand.Reader, serverTemplate, intermediateCert, &serverKey.PublicKey, intermediateKey)
	require.NoError(t, serverCertErr)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertBytes}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: intermediateCertBytes}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootCertBytes})
}

func generateTestX509Cert(t *testing.T, template *x509.Certificate) *x509.Certificate {
	t.Helper()

	key, keyErr := rsa.GenerateKey(cryptorand.Reader, 2048)
	require.NoError(t, keyErr)

	certBytes, certErr := x509.CreateCertificate(cryptorand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, certErr)

	cert, parseErr := x509.ParseCertificate(certBytes)
	require.NoError(t, parseErr)

	return cert
}
