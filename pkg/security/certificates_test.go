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
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCertificateFiles_ValidPair(t *testing.T) {
	certFile, keyFile := generateTestCertAndKey(t)
	err := ValidateCertificateFiles(certFile, keyFile)
	assert.NoError(t, err)
}

func TestValidateCertificateFiles_CertFileNotFound(t *testing.T) {
	_, keyFile := generateTestCertAndKey(t)
	err := ValidateCertificateFiles("/nonexistent/cert.pem", keyFile)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unable to read certificate file")
}

func TestValidateCertificateFiles_KeyFileNotFound(t *testing.T) {
	certFile, _ := generateTestCertAndKey(t)
	err := ValidateCertificateFiles(certFile, "/nonexistent/key.pem")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unable to read private key file")
}

func TestValidateCertificateFiles_MismatchedPair(t *testing.T) {
	certFile, _ := generateTestCertAndKey(t)
	_, keyFile2 := generateTestCertAndKey(t)

	err := ValidateCertificateFiles(certFile, keyFile2)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid pair")
}

func TestValidateCertificateFiles_InvalidPEM(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "bad-cert.pem")
	keyFile := filepath.Join(dir, "bad-key.pem")

	require.NoError(t, os.WriteFile(certFile, []byte("not a cert"), 0600))
	require.NoError(t, os.WriteFile(keyFile, []byte("not a key"), 0600))

	err := ValidateCertificateFiles(certFile, keyFile)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid pair")
}

func TestExtractRootCACertificate_SingleSelfSignedCert(t *testing.T) {
	certPEM := generateSelfSignedCertPEM(t)

	rootCA, err := ExtractRootCACertificate(certPEM)
	require.NoError(t, err)

	// For a single self-signed cert, the root CA should be the cert itself.
	block, _ := pem.Decode(rootCA)
	require.NotNil(t, block)
	assert.Equal(t, "CERTIFICATE", block.Type)

	origBlock, _ := pem.Decode(certPEM)
	assert.Equal(t, origBlock.Bytes, block.Bytes)
}

func TestExtractRootCACertificate_CertChain(t *testing.T) {
	serverPEM, _, rootPEM := generateCertChainPEM(t)

	// Build a chain file: server + root
	chainPEM := append(serverPEM, rootPEM...)

	rootCA, err := ExtractRootCACertificate(chainPEM)
	require.NoError(t, err)

	// Should return the last cert (root CA), not the server cert.
	block, _ := pem.Decode(rootCA)
	require.NotNil(t, block)

	rootBlock, _ := pem.Decode(rootPEM)
	assert.Equal(t, rootBlock.Bytes, block.Bytes)
}

func TestExtractRootCACertificate_ThreeCertChain(t *testing.T) {
	serverPEM, intermediatePEM, rootPEM := generateCertChainPEM(t)

	// Build a chain file: server + intermediate + root
	chainPEM := append(serverPEM, intermediatePEM...)
	chainPEM = append(chainPEM, rootPEM...)

	rootCA, err := ExtractRootCACertificate(chainPEM)
	require.NoError(t, err)

	// Should return the last cert (root CA).
	block, _ := pem.Decode(rootCA)
	require.NotNil(t, block)

	rootBlock, _ := pem.Decode(rootPEM)
	assert.Equal(t, rootBlock.Bytes, block.Bytes)
}

func TestExtractRootCACertificate_NoCertificates(t *testing.T) {
	_, err := ExtractRootCACertificate([]byte("no certs here"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no certificates found")
}

func TestExtractRootCACertificate_EmptyInput(t *testing.T) {
	_, err := ExtractRootCACertificate([]byte{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no certificates found")
}

func TestExtractRootCACertificate_SkipsNonCertBlocks(t *testing.T) {
	certPEM := generateSelfSignedCertPEM(t)

	// Prepend a non-certificate PEM block
	keyBlock := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: []byte("fake key data"),
	}
	mixed := pem.EncodeToMemory(keyBlock)
	mixed = append(mixed, certPEM...)

	rootCA, err := ExtractRootCACertificate(mixed)
	require.NoError(t, err)

	block, _ := pem.Decode(rootCA)
	require.NotNil(t, block)
	assert.Equal(t, "CERTIFICATE", block.Type)
}

// --- Test helpers ---

func generateTestCertAndKey(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()

	key, keyErr := rsa.GenerateKey(cryptorand.Reader, 2048)
	require.NoError(t, keyErr)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}

	certBytes, certErr := x509.CreateCertificate(cryptorand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, certErr)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	cf := filepath.Join(dir, "cert.pem")
	kf := filepath.Join(dir, "key.pem")

	require.NoError(t, os.WriteFile(cf, certPEM, 0600))
	require.NoError(t, os.WriteFile(kf, keyPEM, 0600))

	return cf, kf
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
