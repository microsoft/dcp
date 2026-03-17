/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package security

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"math"
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

// normalizeThumbprint strips common formatting and copy/paste artifacts from
// certificate thumbprints and returns a lowercase hex string. Handles formats
// from various sources:
//   - Windows certmgr/PowerShell Get-ChildItem Cert:\: "AA BB CC DD ..." (space-separated uppercase hex)
//   - OpenSSL x509 -fingerprint: "AA:BB:CC:DD:..." (colon-separated uppercase hex)
//   - .NET X509Certificate2.Thumbprint: "AABBCCDD..." (contiguous uppercase hex)
//   - Programmatic hex with prefix: "0xaabbccdd..." or "0XAABBCCDD..."
//
// Also strips leading/trailing whitespace (\t, \r, \n) and the invisible
// left-to-right mark (U+200E) that Windows certificate UI sometimes embeds.
func normalizeThumbprint(thumbprint string) string {
	thumbprint = strings.TrimSpace(thumbprint)
	thumbprint = strings.ReplaceAll(thumbprint, "\u200e", "") // left-to-right mark
	thumbprint = strings.ReplaceAll(thumbprint, " ", "")
	thumbprint = strings.ReplaceAll(thumbprint, ":", "")
	thumbprint = strings.TrimPrefix(thumbprint, "0x")
	thumbprint = strings.TrimPrefix(thumbprint, "0X")
	return strings.ToLower(thumbprint)
}

// exportViaPFX exports the certificate and its private key by performing a
// PFX (PKCS#12) export of a temporary in-memory store, then parsing the PFX.
// This approach works with both CNG and legacy CSP key storage providers.
// The in-memory store contains a single certificate, so the PFX will contain
// exactly one certificate and one private key.
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
	if pfxBlob.Size > math.MaxInt32 {
		return nil, "", fmt.Errorf("PFXExportCertStoreEx returned unexpectedly large size: %d", pfxBlob.Size)
	}

	pfxSize := int(pfxBlob.Size)

	// Second call: export the PFX into the buffer.
	pfxData := make([]byte, pfxSize)
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

	// Parse the PFX to extract the certificate and private key.
	privateKey, cert, pfxErr := pkcs12.Decode(pfxData[:pfxSize], "")
	if pfxErr != nil {
		return nil, "", fmt.Errorf("could not decode PFX data: %w", pfxErr)
	}

	rsaKey, ok := privateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, "", fmt.Errorf("private key for certificate %q is not RSA (got %T); only RSA keys are currently supported", thumbprint, privateKey)
	}

	serverAddress, validateErr := ValidateCertificate(cert)
	if validateErr != nil {
		return nil, "", fmt.Errorf("certificate with thumbprint %q is not valid: %w", thumbprint, validateErr)
	}

	// For a single certificate, it serves as both the chain and the CA trust anchor.
	certPEM := PEMEncodeCertificates(cert.Raw)

	return &ServerCertificateData{
		CACertPEM:    certPEM,
		CertChainPEM: certPEM,
		ServerKeyPEM: PEMEncodePrivateKey(x509.MarshalPKCS1PrivateKey(rsaKey)),
	}, serverAddress, nil
}
