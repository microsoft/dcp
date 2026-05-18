/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package kubeconfig

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/security"
)

func TestEnsureKubeconfigData_UsesCertificateFiles(t *testing.T) {
	certPEM, keyPEM, cert := generateKubeconfigTestSelfSignedCert(t)
	certFile, keyFile := writeKubeconfigTestPair(t, certPEM, keyPEM)
	flags := newKubeconfigTestFlags(t)
	require.NoError(t, flags.Set(TLSCertFileFlagName, certFile))
	require.NoError(t, flags.Set(TLSKeyFileFlagName, keyFile))
	require.NoError(t, flags.Set(TLSCertThumbprintFlagName, kubeconfigTestThumbprint(cert)))

	kconfig, ensureErr := EnsureKubeconfigData(context.Background(), flags, logr.Discard())

	require.NoError(t, ensureErr)
	assert.Equal(t, certPEM, kconfig.certificateData.CACertPEM)
	assert.Equal(t, certPEM, kconfig.certificateData.CertChainPEM)
	assert.Equal(t, keyPEM, kconfig.certificateData.ServerKeyPEM)
	assert.Equal(t, certPEM, kconfig.Config.Clusters["apiserver_cluster"].CertificateAuthorityData)
	assert.Equal(t, "https://127.0.0.1:6443", kconfig.Config.Clusters["apiserver_cluster"].Server)
}

func TestEnsureKubeconfigData_AllowsCAFileWithCertificateFiles(t *testing.T) {
	certPEM, keyPEM, caPEM := generateKubeconfigTestIssuedCert(t)
	certFile, keyFile := writeKubeconfigTestPair(t, certPEM, keyPEM)
	caFile := writeKubeconfigTestFile(t, "ca.pem", caPEM)
	flags := newKubeconfigTestFlags(t)
	require.NoError(t, flags.Set(TLSCertFileFlagName, certFile))
	require.NoError(t, flags.Set(TLSKeyFileFlagName, keyFile))
	require.NoError(t, flags.Set(TLSCAFileFlagName, caFile))

	kconfig, ensureErr := EnsureKubeconfigData(context.Background(), flags, logr.Discard())

	require.NoError(t, ensureErr)
	assert.Equal(t, caPEM, kconfig.certificateData.CACertPEM)
	assert.Equal(t, caPEM, kconfig.Config.Clusters["apiserver_cluster"].CertificateAuthorityData)
}

func TestEnsureKubeconfigData_VerifiesCertificateFileThumbprint(t *testing.T) {
	certPEM, keyPEM, _ := generateKubeconfigTestSelfSignedCert(t)
	certFile, keyFile := writeKubeconfigTestPair(t, certPEM, keyPEM)
	flags := newKubeconfigTestFlags(t)
	require.NoError(t, flags.Set(TLSCertFileFlagName, certFile))
	require.NoError(t, flags.Set(TLSKeyFileFlagName, keyFile))
	require.NoError(t, flags.Set(TLSCertThumbprintFlagName, "00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00"))

	_, ensureErr := EnsureKubeconfigData(context.Background(), flags, logr.Discard())

	assert.Error(t, ensureErr)
	assert.Contains(t, ensureErr.Error(), "thumbprint mismatch")
}

func TestEnsureKubeconfigData_RequiresCertificateFileAndKeyTogether(t *testing.T) {
	certPEM, _, _ := generateKubeconfigTestSelfSignedCert(t)
	certFile := writeKubeconfigTestFile(t, "cert.pem", certPEM)
	flags := newKubeconfigTestFlags(t)
	require.NoError(t, flags.Set(TLSCertFileFlagName, certFile))

	_, ensureErr := EnsureKubeconfigData(context.Background(), flags, logr.Discard())

	assert.Error(t, ensureErr)
	assert.Contains(t, ensureErr.Error(), "must be specified together")
}

func TestEnsureKubeconfigData_CAFileRequiresCertificateSource(t *testing.T) {
	flags := newKubeconfigTestFlags(t)
	require.NoError(t, flags.Set(TLSCAFileFlagName, filepath.Join(t.TempDir(), "missing-ca.pem")))

	_, ensureErr := EnsureKubeconfigData(context.Background(), flags, logr.Discard())

	require.EqualError(t, ensureErr, "--tls-ca-file requires --tls-cert-file/--tls-key-file or --tls-cert-thumbprint to also be specified")
}

func newKubeconfigTestFlags(t *testing.T) *pflag.FlagSet {
	t.Helper()

	resetKubeconfigTestFlagValues()
	t.Cleanup(resetKubeconfigTestFlagValues)

	flags := pflag.NewFlagSet(t.Name(), pflag.ContinueOnError)
	EnsureKubeconfigFlag(flags)
	EnsureKubeconfigPortFlag(flags)
	EnsureTLSCertFileFlag(flags)
	EnsureTLSKeyFileFlag(flags)
	EnsureTLSCertThumbprintFlag(flags)
	EnsureTLSCAFileFlag(flags)

	require.NoError(t, flags.Set(ctrl_config.KubeconfigFlagName, filepath.Join(t.TempDir(), "kubeconfig")))
	require.NoError(t, flags.Set(PortFlagName, "6443"))
	return flags
}

func resetKubeconfigTestFlagValues() {
	port = 0
	tlsCertFile = ""
	tlsKeyFile = ""
	tlsCertThumbprint = ""
	tlsCAFile = ""
}

func generateKubeconfigTestSelfSignedCert(t *testing.T) ([]byte, []byte, *x509.Certificate) {
	t.Helper()

	key := generateKubeconfigTestKey(t)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	certBytes, certErr := x509.CreateCertificate(cryptorand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, certErr)
	cert, parseErr := x509.ParseCertificate(certBytes)
	require.NoError(t, parseErr)

	return security.PEMEncodeCertificates(certBytes), kubeconfigTestKeyPEM(t, key), cert
}

func generateKubeconfigTestIssuedCert(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()

	caKey := generateKubeconfigTestKey(t)
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

	serverKey := generateKubeconfigTestKey(t)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano() + 1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	serverBytes, serverErr := x509.CreateCertificate(cryptorand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	require.NoError(t, serverErr)

	return security.PEMEncodeCertificates(serverBytes), kubeconfigTestKeyPEM(t, serverKey), security.PEMEncodeCertificates(caBytes)
}

func generateKubeconfigTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()

	key, keyErr := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	require.NoError(t, keyErr)
	return key
}

func kubeconfigTestKeyPEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()

	keyBytes, marshalErr := x509.MarshalECPrivateKey(key)
	require.NoError(t, marshalErr)
	return security.PEMEncodeBlock("EC PRIVATE KEY", keyBytes)
}

func kubeconfigTestThumbprint(cert *x509.Certificate) string {
	thumbprint := sha1.Sum(cert.Raw)
	return hex.EncodeToString(thumbprint[:])
}

func writeKubeconfigTestPair(t *testing.T, certPEM []byte, keyPEM []byte) (string, string) {
	t.Helper()

	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	require.NoError(t, usvc_io.WriteFile(certFile, certPEM, osutil.PermissionOnlyOwnerReadWrite))
	require.NoError(t, usvc_io.WriteFile(keyFile, keyPEM, osutil.PermissionOnlyOwnerReadWrite))
	return certFile, keyFile
}

func writeKubeconfigTestFile(t *testing.T, name string, contents []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, usvc_io.WriteFile(path, contents, osutil.PermissionOnlyOwnerReadWrite))
	return path
}
