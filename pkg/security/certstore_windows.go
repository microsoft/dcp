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
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// NCrypt blob type for full RSA private key export.
	ncryptRSAFullPrivateBlob = "RSAFULLPRIVATEBLOB"

	// BCRYPT_RSAFULLPRIVATE_MAGIC identifies an RSA full private key blob.
	bcryptRSAFullPrivateMagic = 0x33415352 // "RSA3" in little-endian

	// Size of the BCRYPT_RSAKEY_BLOB header (6 x uint32).
	bcryptRSAKeyBlobHeaderSize = 6 * 4
)

var (
	modNCrypt            = windows.NewLazyDLL("ncrypt.dll")
	procNCryptExportKey  = modNCrypt.NewProc("NCryptExportKey")
	procNCryptFreeObject = modNCrypt.NewProc("NCryptFreeObject")
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

	privateKey, keyErr := acquirePrivateKey(certCtx, thumbprint)
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
func contextToX509(ctx *windows.CertContext) (*x509.Certificate, error) {
	certBytes := unsafe.Slice(ctx.EncodedCert, ctx.Length)
	return x509.ParseCertificate(certBytes)
}

// acquirePrivateKey gets the RSA private key associated with a certificate context.
func acquirePrivateKey(ctx *windows.CertContext, thumbprint string) (*rsa.PrivateKey, error) {
	var keyHandle windows.Handle
	var keySpec uint32
	var callerFree bool

	acquireErr := windows.CryptAcquireCertificatePrivateKey(
		ctx,
		windows.CRYPT_ACQUIRE_ONLY_NCRYPT_KEY_FLAG|windows.CRYPT_ACQUIRE_SILENT_FLAG,
		nil,
		&keyHandle,
		&keySpec,
		&callerFree,
	)
	if acquireErr != nil {
		return nil, fmt.Errorf("could not acquire private key for certificate %q: %w (the key may require user interaction or may not be accessible)", thumbprint, acquireErr)
	}
	if callerFree {
		defer ncryptFreeObject(keyHandle)
	}

	blobBytes, exportErr := ncryptExportKey(keyHandle)
	if exportErr != nil {
		return nil, fmt.Errorf("could not export private key for certificate %q: %w (the key may be marked as non-exportable)", thumbprint, exportErr)
	}

	rsaKey, parseErr := parseRSAFullPrivateBlob(blobBytes)
	if parseErr != nil {
		return nil, fmt.Errorf("could not parse exported private key for certificate %q: %w", thumbprint, parseErr)
	}

	return rsaKey, nil
}

// ncryptExportKey exports a CNG key handle as an RSA full private key blob.
func ncryptExportKey(keyHandle windows.Handle) ([]byte, error) {
	blobType, blobTypeErr := windows.UTF16PtrFromString(ncryptRSAFullPrivateBlob)
	if blobTypeErr != nil {
		return nil, fmt.Errorf("could not encode blob type: %w", blobTypeErr)
	}

	// First call: determine the required buffer size.
	var size uint32
	r, _, err := procNCryptExportKey.Call(
		uintptr(keyHandle),
		0, // hExportKey (not used)
		uintptr(unsafe.Pointer(blobType)),
		0, // pParameterList (not used)
		0, // pbOutput (nil to get size)
		0, // cbOutput
		uintptr(unsafe.Pointer(&size)),
		0, // dwFlags
	)
	if r != 0 {
		return nil, fmt.Errorf("NCryptExportKey size query failed: %w (NTSTATUS 0x%08X)", err, r)
	}
	if size == 0 {
		return nil, fmt.Errorf("NCryptExportKey returned zero size for key")
	}

	// Second call: export the key into the buffer.
	buf := make([]byte, size)
	r, _, err = procNCryptExportKey.Call(
		uintptr(keyHandle),
		0,
		uintptr(unsafe.Pointer(blobType)),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(size),
		uintptr(unsafe.Pointer(&size)),
		0,
	)
	if r != 0 {
		return nil, fmt.Errorf("NCryptExportKey failed: %w (NTSTATUS 0x%08X)", err, r)
	}

	return buf[:size], nil
}

// parseRSAFullPrivateBlob parses a BCRYPT_RSAFULLPRIVATE_BLOB into an rsa.PrivateKey.
//
// Blob layout (all integers are unsigned, little-endian):
//
//	BCRYPT_RSAKEY_BLOB header (6 x uint32):
//	  Magic, BitLength, cbPublicExp, cbModulus, cbPrime1, cbPrime2
//	Followed by contiguous byte fields:
//	  PublicExponent  [cbPublicExp]
//	  Modulus         [cbModulus]
//	  Prime1          [cbPrime1]
//	  Prime2          [cbPrime2]
//	  Exponent1       [cbPrime1]   (d mod (p-1))
//	  Exponent2       [cbPrime2]   (d mod (q-1))
//	  Coefficient     [cbPrime1]   (q^-1 mod p)
//	  PrivateExponent [cbModulus]
//
// See https://learn.microsoft.com/en-us/windows/win32/api/bcrypt/ns-bcrypt-bcrypt_rsakey_blob
func parseRSAFullPrivateBlob(blob []byte) (*rsa.PrivateKey, error) {
	if len(blob) < bcryptRSAKeyBlobHeaderSize {
		return nil, fmt.Errorf("RSA key blob too short: %d bytes", len(blob))
	}

	magic := binary.LittleEndian.Uint32(blob[0:4])
	if magic != bcryptRSAFullPrivateMagic {
		return nil, fmt.Errorf("unexpected RSA key blob magic: 0x%08X (expected 0x%08X)", magic, bcryptRSAFullPrivateMagic)
	}

	cbPublicExp := int(binary.LittleEndian.Uint32(blob[8:12]))
	cbModulus := int(binary.LittleEndian.Uint32(blob[12:16]))
	cbPrime1 := int(binary.LittleEndian.Uint32(blob[16:20]))
	cbPrime2 := int(binary.LittleEndian.Uint32(blob[20:24]))

	// Total expected size: header + PublicExp + Modulus + P + Q + dP + dQ + qInv + D
	expectedSize := bcryptRSAKeyBlobHeaderSize + cbPublicExp + cbModulus +
		cbPrime1 + cbPrime2 + // P, Q
		cbPrime1 + cbPrime2 + cbPrime1 + // dP, dQ, qInv
		cbModulus // D
	if len(blob) < expectedSize {
		return nil, fmt.Errorf("RSA key blob too short: %d bytes (expected at least %d)", len(blob), expectedSize)
	}

	offset := bcryptRSAKeyBlobHeaderSize
	readField := func(size int) *big.Int {
		field := new(big.Int).SetBytes(blob[offset : offset+size])
		offset += size
		return field
	}

	publicExp := readField(cbPublicExp)
	modulus := readField(cbModulus)
	prime1 := readField(cbPrime1)
	prime2 := readField(cbPrime2)
	offset += cbPrime1 + cbPrime2 + cbPrime1 // skip dP, dQ, qInv — Go recomputes them
	privateExp := readField(cbModulus)

	key := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{
			N: modulus,
			E: int(publicExp.Int64()),
		},
		D:      privateExp,
		Primes: []*big.Int{prime1, prime2},
	}
	key.Precompute()

	if validateErr := key.Validate(); validateErr != nil {
		return nil, fmt.Errorf("exported RSA key failed validation: %w", validateErr)
	}

	return key, nil
}

// ncryptFreeObject releases an NCrypt handle.
func ncryptFreeObject(handle windows.Handle) {
	procNCryptFreeObject.Call(uintptr(handle)) //nolint:errcheck
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
