/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package security

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
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
	modCrypt32                = windows.NewLazyDLL("crypt32.dll")
	procPFXExportCertStoreEx = modCrypt32.NewProc("PFXExportCertStoreEx")
)

func lookupCertificate(thumbprint string) (*ServerCertificateData, error) {
	thumbprint = normalizeThumbprint(thumbprint)

	hashBytes, decodeErr := hex.DecodeString(thumbprint)
	if decodeErr != nil {
		return nil, fmt.Errorf("invalid certificate thumbprint %q: %w", thumbprint, decodeErr)
	}
	if len(hashBytes) != 20 {
		return nil, fmt.Errorf("invalid certificate thumbprint %q: expected 40 hex characters (SHA-1), got %d", thumbprint, len(thumbprint))
	}

	storeName, storeNameErr := windows.UTF16PtrFromString("MY")
	if storeNameErr != nil {
		return nil, fmt.Errorf("could not encode store name: %w", storeNameErr)
	}

	store, openErr := windows.CertOpenSystemStore(0, storeName)
	if openErr != nil {
		return nil, fmt.Errorf("could not open CurrentUser\\My certificate store: %w", openErr)
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
		return nil, fmt.Errorf("certificate with thumbprint %q not found in CurrentUser\\My store: %w", thumbprint, findErr)
	}
	defer windows.CertFreeCertificateContext(certCtx)

	leafCert, leafErr := contextToX509(certCtx)
	if leafErr != nil {
		return nil, fmt.Errorf("could not parse certificate with thumbprint %q: %w", thumbprint, leafErr)
	}

	privateKey, keyErr := exportPrivateKeyViaPFX(certCtx, thumbprint)
	if keyErr != nil {
		return nil, keyErr
	}

	rootCA, chainErr := extractRootCA(certCtx, leafCert)
	if chainErr != nil {
		return nil, chainErr
	}

	return &ServerCertificateData{
		CACertificate: rootCA,
		ServerCert:    leafCert.Raw,
		ServerKey:     privateKey,
	}, nil
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

// contextToX509 converts a Windows CertContext to a Go x509.Certificate.
// The certificate bytes are copied so the result remains valid after the
// CertContext is freed.
func contextToX509(ctx *windows.CertContext) (*x509.Certificate, error) {
	certBytes := make([]byte, ctx.Length)
	copy(certBytes, unsafe.Slice(ctx.EncodedCert, ctx.Length))
	return x509.ParseCertificate(certBytes)
}

// exportPrivateKeyViaPFX exports the private key of the given certificate by
// performing a PFX (PKCS#12) export of a temporary in-memory store, then
// parsing the PFX to extract the RSA private key. This approach works with
// both CNG and legacy CSP key storage providers.
func exportPrivateKeyViaPFX(certCtx *windows.CertContext, thumbprint string) (*rsa.PrivateKey, error) {
	// Create a temporary in-memory certificate store.
	memStore, openErr := windows.CertOpenStore(
		windows.CERT_STORE_PROV_MEMORY,
		0, 0,
		windows.CERT_STORE_CREATE_NEW_FLAG,
		0,
	)
	if openErr != nil {
		return nil, fmt.Errorf("could not create in-memory certificate store: %w", openErr)
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
		return nil, fmt.Errorf("could not add certificate to temporary store: %w", addErr)
	}

	// Use an empty password for the temporary PFX.
	password, passwordErr := windows.UTF16PtrFromString("")
	if passwordErr != nil {
		return nil, fmt.Errorf("could not encode PFX password: %w", passwordErr)
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
		return nil, fmt.Errorf("PFXExportCertStoreEx size query failed: %w", err)
	}
	if pfxBlob.Size == 0 {
		return nil, fmt.Errorf("PFXExportCertStoreEx returned zero size")
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
		return nil, fmt.Errorf("PFXExportCertStoreEx failed: %w (the key may be marked as non-exportable)", err)
	}

	// Parse the PFX to extract the private key.
	privateKey, _, pfxErr := pkcs12.Decode(pfxData[:pfxBlob.Size], "")
	if pfxErr != nil {
		return nil, fmt.Errorf("could not decode PFX data: %w", pfxErr)
	}

	rsaKey, ok := privateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key for certificate %q is not RSA (got %T); only RSA keys are currently supported", thumbprint, privateKey)
	}

	return rsaKey, nil
}

// extractRootCA builds the certificate chain for the given leaf and returns the
// root CA certificate in raw DER form. If the leaf certificate is self-signed,
// it is returned as its own root.
func extractRootCA(leafCtx *windows.CertContext, leafCert *x509.Certificate) ([]byte, error) {
	// Self-signed: the cert is its own trust anchor.
	if isSelfSigned(leafCert) {
		return leafCert.Raw, nil
	}

	chainPara := windows.CertChainPara{
		Size: uint32(unsafe.Sizeof(windows.CertChainPara{})),
	}

	var chainCtx *windows.CertChainContext
	chainErr := windows.CertGetCertificateChain(
		0, // default chain engine
		leafCtx,
		nil, // current time
		0,   // no additional store
		&chainPara,
		0, // default flags
		0, // reserved
		&chainCtx,
	)
	if chainErr != nil {
		return nil, fmt.Errorf("could not build certificate chain: %w", chainErr)
	}
	defer windows.CertFreeCertificateChain(chainCtx)

	if chainCtx.ChainCount == 0 {
		return nil, fmt.Errorf("certificate chain is empty")
	}

	// The first simple chain contains the path from leaf to root.
	chains := unsafe.Slice(chainCtx.Chains, chainCtx.ChainCount)
	chain := chains[0]

	if chain.NumElements == 0 {
		return nil, fmt.Errorf("certificate chain has no elements")
	}

	elements := unsafe.Slice(chain.Elements, chain.NumElements)
	// The last element in the chain is the root CA.
	rootElement := elements[chain.NumElements-1]
	rootCert, rootErr := contextToX509(rootElement.CertContext)
	if rootErr != nil {
		return nil, fmt.Errorf("could not parse root CA certificate from chain: %w", rootErr)
	}

	return rootCert.Raw, nil
}

// isSelfSigned returns true if the certificate is self-signed: its issuer
// matches its subject and the signature is self-verifiable. Self-signed
// non-CA certificates (e.g., ASP.NET dev certs) are valid trust anchors
// and are treated as their own root.
func isSelfSigned(cert *x509.Certificate) bool {
	return bytes.Equal(cert.RawIssuer, cert.RawSubject) &&
		cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) == nil
}
