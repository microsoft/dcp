/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package security

import (
	"bytes"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/crypto/pkcs12"
	"golang.org/x/sys/windows"
)

const (
	// EXPORT_PRIVATE_KEYS causes PFXExportCertStoreEx to include private keys.
	exportPrivateKeys = 0x4

	// REPORT_NOT_ABLE_TO_EXPORT_PRIVATE_KEY makes PFXExportCertStoreEx fail
	// immediately (instead of silently omitting keys) when a key can't be exported.
	reportNotAbleToExportPrivateKey = 0x2
)

var (
	modCrypt32               = windows.NewLazyDLL("crypt32.dll")
	procPFXExportCertStoreEx = modCrypt32.NewProc("PFXExportCertStoreEx")
)

type certWithPEM struct {
	cert *x509.Certificate
	pem  []byte
}

func lookupCertificate(thumbprint string) (*ServerCertificateData, string, error) {
	thumbprint = normalizeThumbprint(thumbprint)

	hashBytes, decodeErr := hex.DecodeString(thumbprint)
	if decodeErr != nil {
		return nil, "", fmt.Errorf("invalid certificate thumbprint %q: %w", thumbprint, decodeErr)
	}
	if len(hashBytes) != 20 {
		return nil, "", fmt.Errorf("invalid certificate thumbprint %q: expected 40 hex characters (SHA-1), got %d", thumbprint, len(thumbprint))
	}

	storeName, storeNameErr := windows.UTF16PtrFromString("MY")
	if storeNameErr != nil {
		return nil, "", fmt.Errorf("could not encode store name: %w", storeNameErr)
	}

	store, openErr := windows.CertOpenSystemStore(0, storeName)
	if openErr != nil {
		return nil, "", fmt.Errorf("could not open CurrentUser\\My certificate store: %w", openErr)
	}
	defer windows.CertCloseStore(store, 0)

	hashBlob := windows.CryptHashBlob{
		Size: uint32(len(hashBytes)),
		Data: &hashBytes[0],
	}

	certCtx, findErr := windows.CertFindCertificateInStore(
		store,
		windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING,
		0,
		windows.CERT_FIND_HASH,
		unsafe.Pointer(&hashBlob),
		nil,
	)
	if findErr != nil {
		return nil, "", fmt.Errorf("certificate with thumbprint %q not found in CurrentUser\\My store: %w", thumbprint, findErr)
	}
	defer windows.CertFreeCertificateContext(certCtx)

	return exportViaPFX(certCtx, thumbprint)
}

// normalizeThumbprint strips common formatting from certificate thumbprints:
// spaces, colons, and the leading "0x" prefix.
func normalizeThumbprint(thumbprint string) string {
	thumbprint = strings.ReplaceAll(thumbprint, " ", "")
	thumbprint = strings.ReplaceAll(thumbprint, ":", "")
	thumbprint = strings.TrimPrefix(thumbprint, "0x")
	thumbprint = strings.TrimPrefix(thumbprint, "0X")
	return strings.ToLower(thumbprint)
}

// exportViaPFX exports the certificate, its chain, and its private key by
// performing a PFX (PKCS#12) export of a temporary in-memory store, then
// parsing the PFX to extract all components. This approach works with both
// CNG and legacy CSP key storage providers.
func exportViaPFX(certCtx *windows.CertContext, thumbprint string) (*ServerCertificateData, string, error) {
	// Create a temporary in-memory certificate store.
	memStore, openErr := windows.CertOpenStore(
		windows.CERT_STORE_PROV_MEMORY,
		0, 0,
		windows.CERT_STORE_CREATE_NEW_FLAG,
		0,
	)
	if openErr != nil {
		return nil, "", fmt.Errorf("could not create in-memory certificate store: %w", openErr)
	}
	defer windows.CertCloseStore(memStore, 0)

	// Add the certificate (with its private key link) to the temporary store.
	addErr := windows.CertAddCertificateContextToStore(
		memStore,
		certCtx,
		windows.CERT_STORE_ADD_ALWAYS,
		nil,
	)
	if addErr != nil {
		return nil, "", fmt.Errorf("could not add certificate to temporary store: %w", addErr)
	}

	// Use an empty password for the temporary PFX.
	password, passwordErr := windows.UTF16PtrFromString("")
	if passwordErr != nil {
		return nil, "", fmt.Errorf("could not encode PFX password: %w", passwordErr)
	}

	flags := uint32(exportPrivateKeys | reportNotAbleToExportPrivateKey)

	// First call: determine the required buffer size.
	pfxBlob := windows.CryptDataBlob{}
	r, _, err := procPFXExportCertStoreEx.Call(
		uintptr(memStore),
		uintptr(unsafe.Pointer(&pfxBlob)),
		uintptr(unsafe.Pointer(password)),
		0, // pvPara (reserved)
		uintptr(flags),
	)
	if r == 0 {
		return nil, "", fmt.Errorf("PFXExportCertStoreEx size query failed: %w", err)
	}
	if pfxBlob.Size == 0 {
		return nil, "", fmt.Errorf("PFXExportCertStoreEx returned zero size")
	}

	// Second call: export the PFX into the buffer.
	pfxData := make([]byte, pfxBlob.Size)
	pfxBlob.Data = &pfxData[0]
	r, _, err = procPFXExportCertStoreEx.Call(
		uintptr(memStore),
		uintptr(unsafe.Pointer(&pfxBlob)),
		uintptr(unsafe.Pointer(password)),
		0,
		uintptr(flags),
	)
	if r == 0 {
		return nil, "", fmt.Errorf("PFXExportCertStoreEx failed: %w (the key may be marked as non-exportable)", err)
	}

	// Parse the PFX into PEM blocks and extract the leaf cert, root CA, and key.
	pemBlocks, pfxErr := pkcs12.ToPEM(pfxData[:pfxBlob.Size], "")
	if pfxErr != nil {
		return nil, "", fmt.Errorf("could not decode PFX data: %w", pfxErr)
	}

	var certs []certWithPEM
	var keyPEM []byte

	for _, block := range pemBlocks {
		switch block.Type {
		case "CERTIFICATE":
			cert, certErr := x509.ParseCertificate(block.Bytes)
			if certErr != nil {
				return nil, "", fmt.Errorf("could not parse certificate from PFX: %w", certErr)
			}
			certs = append(certs, certWithPEM{cert: cert, pem: pem.EncodeToMemory(block)})
		case "PRIVATE KEY":
			if keyPEM == nil {
				keyPEM = pem.EncodeToMemory(block)
			}
		}
	}

	if len(certs) == 0 {
		return nil, "", fmt.Errorf("PFX for certificate %q did not contain a certificate", thumbprint)
	}
	if keyPEM == nil {
		return nil, "", fmt.Errorf("PFX for certificate %q did not contain a private key", thumbprint)
	}

	// Validate the leaf certificate (first in the list).
	leafCert := certs[0].cert
	serverAddress, validateErr := ValidateCertificate(leafCert)
	if validateErr != nil {
		return nil, "", fmt.Errorf("certificate with thumbprint %q is not valid: %w", thumbprint, validateErr)
	}

	// The chain includes all certificates; the root CA is the trust anchor for the kubeconfig.
	rootIdx, rootFound := findRootCertIndex(certs)
	if !rootFound {
		return nil, "", fmt.Errorf("certificate chain for %q does not contain a self-signed root CA", thumbprint)
	}

	var chainPEM []byte
	for _, c := range certs {
		chainPEM = append(chainPEM, c.pem...)
	}

	return &ServerCertificateData{
		CACertPEM:    certs[rootIdx].pem,
		CertChainPEM: chainPEM,
		ServerKeyPEM: keyPEM,
	}, serverAddress, nil
}

// findRootCertIndex returns the index of the self-signed root certificate.
// If none is found, returns the index of the last certificate.
func findRootCertIndex(certs []certWithPEM) (int, bool) {
	for i, c := range certs {
		if bytes.Equal(c.cert.RawIssuer, c.cert.RawSubject) &&
			c.cert.CheckSignature(c.cert.SignatureAlgorithm, c.cert.RawTBSCertificate, c.cert.Signature) == nil {
			return i, true
		}
	}
	return -1, false
}
