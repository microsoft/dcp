/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package security

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	stdio "io"
	"os"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

// LoadCertificateFiles loads a PEM certificate chain and matching private key
// from files. The returned CA data comes from caFile when provided, otherwise it
// is extracted from the certificate chain when that chain includes a self-signed
// root.
func LoadCertificateFiles(certFile string, keyFile string, caFile string, expectedThumbprint string) (*ServerCertificateData, string, error) {
	if certFile == "" {
		return nil, "", fmt.Errorf("certificate file path is required")
	}
	if keyFile == "" {
		return nil, "", fmt.Errorf("private key file path is required")
	}

	certPEM, certReadErr := readPEMFile(certFile, "certificate file")
	if certReadErr != nil {
		return nil, "", certReadErr
	}
	keyPEM, keyReadErr := readPEMFile(keyFile, "private key file")
	if keyReadErr != nil {
		return nil, "", keyReadErr
	}

	keyPair, keyPairErr := tls.X509KeyPair(certPEM, keyPEM)
	if keyPairErr != nil {
		return nil, "", fmt.Errorf("certificate file %q and private key file %q do not contain a valid certificate/key pair: %w", certFile, keyFile, keyPairErr)
	}
	if len(keyPair.Certificate) == 0 {
		return nil, "", fmt.Errorf("certificate file %q does not contain any certificates", certFile)
	}

	leafCert, leafParseErr := x509.ParseCertificate(keyPair.Certificate[0])
	if leafParseErr != nil {
		return nil, "", fmt.Errorf("certificate file %q contains an invalid leaf certificate: %w", certFile, leafParseErr)
	}

	serverAddress, validateErr := ValidateCertificate(leafCert)
	if validateErr != nil {
		return nil, "", fmt.Errorf("certificate file %q is not valid: %w", certFile, validateErr)
	}

	if expectedThumbprint != "" {
		thumbprintErr := VerifyCertificateThumbprint(leafCert, expectedThumbprint)
		if thumbprintErr != nil {
			return nil, "", fmt.Errorf("certificate file %q failed thumbprint verification: %w", certFile, thumbprintErr)
		}
	}

	var caPEM []byte
	if caFile != "" {
		loadedCA, caReadErr := LoadCertificateAuthorityFile(caFile)
		if caReadErr != nil {
			return nil, "", caReadErr
		}
		caPEM = loadedCA
	} else {
		extractedCA, extractErr := ExtractRootCertificate(certPEM)
		if extractErr != nil {
			return nil, "", fmt.Errorf("unable to determine CA certificate from certificate file %q; provide a CA certificate file: %w", certFile, extractErr)
		}
		caPEM = extractedCA
	}

	return &ServerCertificateData{
		CACertPEM:    caPEM,
		CertChainPEM: certPEM,
		ServerKeyPEM: keyPEM,
	}, serverAddress, nil
}

// LoadCertificateAuthorityFile loads and validates a PEM-encoded CA bundle file.
func LoadCertificateAuthorityFile(caFile string) ([]byte, error) {
	caPEM, caReadErr := readPEMFile(caFile, "CA certificate file")
	if caReadErr != nil {
		return nil, caReadErr
	}
	validateErr := validateCertificatePEM(caPEM, fmt.Sprintf("CA certificate file %q", caFile))
	if validateErr != nil {
		return nil, validateErr
	}

	return caPEM, nil
}

func readPEMFile(path string, description string) ([]byte, error) {
	file, openErr := usvc_io.OpenFile(path, os.O_RDONLY, 0)
	if openErr != nil {
		return nil, fmt.Errorf("unable to open %s %q: %w", description, path, openErr)
	}

	contents, readErr := stdio.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("unable to read %s %q: %w", description, path, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("unable to close %s %q: %w", description, path, closeErr)
	}
	if len(bytes.TrimSpace(contents)) == 0 {
		return nil, fmt.Errorf("%s %q is empty", description, path)
	}

	return contents, nil
}

func validateCertificatePEM(certPEM []byte, description string) error {
	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(certPEM) {
		return fmt.Errorf("%s does not contain any PEM-encoded certificates", description)
	}

	return nil
}
