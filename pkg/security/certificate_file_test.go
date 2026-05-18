/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

func TestLoadCertificateFiles_SelfSignedCertificate(t *testing.T) {
	certPEM, keyPEM, cert := generateCertificateFileTestSelfSignedCert(t, net.IPv4(127, 0, 0, 1))
	certFile, keyFile := writeCertificateFileTestPair(t, certPEM, keyPEM)
	thumbprint := certificateFileTestColonThumbprint(cert)

	certData, serverAddress, loadErr := LoadCertificateFiles(certFile, keyFile, "", thumbprint)

	require.NoError(t, loadErr)
	assert.Equal(t, "127.0.0.1", serverAddress)
	assert.Equal(t, certPEM, certData.CACertPEM)
	assert.Equal(t, certPEM, certData.CertChainPEM)
	assert.Equal(t, keyPEM, certData.ServerKeyPEM)
	_, tlsErr := tls.X509KeyPair(certData.CertChainPEM, certData.ServerKeyPEM)
	assert.NoError(t, tlsErr)
}

func TestLoadCertificateFiles_UsesCAFileForIssuedCertificate(t *testing.T) {
	certPEM, keyPEM, caPEM, _ := generateCertificateFileTestIssuedCert(t, net.IPv4(127, 0, 0, 1))
	certFile, keyFile := writeCertificateFileTestPair(t, certPEM, keyPEM)
	caFile := writeCertificateFileTestFile(t, "ca.pem", caPEM)

	certData, _, loadErr := LoadCertificateFiles(certFile, keyFile, caFile, "")

	require.NoError(t, loadErr)
	assert.Equal(t, caPEM, certData.CACertPEM)
	assert.Equal(t, certPEM, certData.CertChainPEM)
}

func TestLoadCertificateFiles_RequiresTrustAnchor(t *testing.T) {
	certPEM, keyPEM, _, _ := generateCertificateFileTestIssuedCert(t, net.IPv4(127, 0, 0, 1))
	certFile, keyFile := writeCertificateFileTestPair(t, certPEM, keyPEM)

	_, _, loadErr := LoadCertificateFiles(certFile, keyFile, "", "")

	assert.Error(t, loadErr)
	assert.Contains(t, loadErr.Error(), "provide a CA certificate file")
}

func TestLoadCertificateFiles_RejectsMismatchedKey(t *testing.T) {
	certPEM, _, _ := generateCertificateFileTestSelfSignedCert(t, net.IPv4(127, 0, 0, 1))
	_, keyPEM, _ := generateCertificateFileTestSelfSignedCert(t, net.IPv4(127, 0, 0, 1))
	certFile, keyFile := writeCertificateFileTestPair(t, certPEM, keyPEM)

	_, _, loadErr := LoadCertificateFiles(certFile, keyFile, "", "")

	assert.Error(t, loadErr)
	assert.Contains(t, loadErr.Error(), "valid certificate/key pair")
}

func TestLoadCertificateFiles_RejectsThumbprintMismatch(t *testing.T) {
	certPEM, keyPEM, _ := generateCertificateFileTestSelfSignedCert(t, net.IPv4(127, 0, 0, 1))
	certFile, keyFile := writeCertificateFileTestPair(t, certPEM, keyPEM)

	_, _, loadErr := LoadCertificateFiles(certFile, keyFile, "", "00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00")

	assert.Error(t, loadErr)
	assert.Contains(t, loadErr.Error(), "thumbprint mismatch")
}

func TestLoadCertificateFiles_RejectsNonLocalhostCertificate(t *testing.T) {
	certPEM, keyPEM, _ := generateCertificateFileTestSelfSignedCert(t, net.IPv4(10, 0, 0, 1))
	certFile, keyFile := writeCertificateFileTestPair(t, certPEM, keyPEM)

	_, _, loadErr := LoadCertificateFiles(certFile, keyFile, "", "")

	assert.Error(t, loadErr)
	assert.Contains(t, loadErr.Error(), "not valid for localhost")
}

func generateCertificateFileTestSelfSignedCert(t *testing.T, ip net.IP) ([]byte, []byte, *x509.Certificate) {
	t.Helper()

	key := generateCertificateFileTestKey(t)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{ip},
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	certBytes, certErr := x509.CreateCertificate(cryptorand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, certErr)
	cert, parseErr := x509.ParseCertificate(certBytes)
	require.NoError(t, parseErr)

	return PEMEncodeCertificates(certBytes), certificateFileTestKeyPEM(t, key), cert
}

func generateCertificateFileTestIssuedCert(t *testing.T, ip net.IP) ([]byte, []byte, []byte, *x509.Certificate) {
	t.Helper()

	caKey := generateCertificateFileTestKey(t)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caBytes, caErr := x509.CreateCertificate(cryptorand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, caErr)
	caCert, caParseErr := x509.ParseCertificate(caBytes)
	require.NoError(t, caParseErr)

	serverKey := generateCertificateFileTestKey(t)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano() + 1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{ip},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	serverBytes, serverErr := x509.CreateCertificate(cryptorand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	require.NoError(t, serverErr)
	serverCert, serverParseErr := x509.ParseCertificate(serverBytes)
	require.NoError(t, serverParseErr)

	return PEMEncodeCertificates(serverBytes), certificateFileTestKeyPEM(t, serverKey), PEMEncodeCertificates(caBytes), serverCert
}

func generateCertificateFileTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()

	key, keyErr := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	require.NoError(t, keyErr)
	return key
}

func certificateFileTestKeyPEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()

	keyBytes, marshalErr := x509.MarshalECPrivateKey(key)
	require.NoError(t, marshalErr)
	return PEMEncodeBlock("EC PRIVATE KEY", keyBytes)
}

func certificateFileTestColonThumbprint(cert *x509.Certificate) string {
	thumbprint := sha1.Sum(cert.Raw)
	hexThumbprint := hex.EncodeToString(thumbprint[:])
	colonThumbprint := ""
	for i := 0; i < len(hexThumbprint); i += 2 {
		if i > 0 {
			colonThumbprint += ":"
		}
		colonThumbprint += hexThumbprint[i : i+2]
	}
	return colonThumbprint
}

func writeCertificateFileTestPair(t *testing.T, certPEM []byte, keyPEM []byte) (string, string) {
	t.Helper()

	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	require.NoError(t, usvc_io.WriteFile(certFile, certPEM, osutil.PermissionOnlyOwnerReadWrite))
	require.NoError(t, usvc_io.WriteFile(keyFile, keyPEM, osutil.PermissionOnlyOwnerReadWrite))

	return certFile, keyFile
}

func writeCertificateFileTestFile(t *testing.T, name string, contents []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, usvc_io.WriteFile(path, contents, osutil.PermissionOnlyOwnerReadWrite))
	return path
}
