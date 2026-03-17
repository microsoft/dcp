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

	// Legacy CSP export blob type.
	privatekeyBlob = 0x7 // PRIVATEKEYBLOB

	// PRIVATEKEYBLOB header: BLOBHEADER (8 bytes) + RSAPUBKEY (12 bytes) = 20 bytes.
	privatekeyBlobHeaderSize = 20

	// RSA2 magic in RSAPUBKEY identifies a private key blob.
	rsaPrivateKeyMagic = 0x32415352 // "RSA2" in little-endian
)

var (
	modNCrypt            = windows.NewLazyDLL("ncrypt.dll")
	procNCryptExportKey  = modNCrypt.NewProc("NCryptExportKey")
	procNCryptFreeObject = modNCrypt.NewProc("NCryptFreeObject")

	modAdvapi32         = windows.NewLazyDLL("advapi32.dll")
	procCryptExportKey  = modAdvapi32.NewProc("CryptExportKey")
	procCryptGetUserKey = modAdvapi32.NewProc("CryptGetUserKey")
	procCryptDestroyKey = modAdvapi32.NewProc("CryptDestroyKey")
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
// Supports both CNG and legacy CSP key storage providers.
func acquirePrivateKey(ctx *windows.CertContext, thumbprint string) (*rsa.PrivateKey, error) {
	var keyHandle windows.Handle
	var keySpec uint32
	var callerFree bool

	acquireErr := windows.CryptAcquireCertificatePrivateKey(
		ctx,
		windows.CRYPT_ACQUIRE_ALLOW_NCRYPT_KEY_FLAG|windows.CRYPT_ACQUIRE_SILENT_FLAG,
		nil,
		&keyHandle,
		&keySpec,
		&callerFree,
	)
	if acquireErr != nil {
		return nil, fmt.Errorf("could not acquire private key for certificate %q: %w (the key may require user interaction or may not be accessible)", thumbprint, acquireErr)
	}

	var blobBytes []byte
	var exportErr error

	if keySpec == windows.CERT_NCRYPT_KEY_SPEC {
		// CNG key: export via NCrypt.
		if callerFree {
			defer ncryptFreeObject(keyHandle)
		}
		blobBytes, exportErr = ncryptExportKey(keyHandle)
		if exportErr != nil {
			return nil, fmt.Errorf("could not export CNG private key for certificate %q: %w (the key may be marked as non-exportable)", thumbprint, exportErr)
		}
	} else {
		// Legacy CSP key: export via CryptoAPI.
		if callerFree {
			defer windows.CryptReleaseContext(keyHandle, 0)
		}
		blobBytes, exportErr = cspExportKey(keyHandle, keySpec)
		if exportErr != nil {
			return nil, fmt.Errorf("could not export CSP private key for certificate %q: %w (the key may be marked as non-exportable)", thumbprint, exportErr)
		}
	}

	rsaKey, parseErr := parseRSAPrivateKeyBlob(blobBytes)
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

// cspExportKey exports a legacy CSP key handle as a PRIVATEKEYBLOB.
func cspExportKey(provHandle windows.Handle, keySpec uint32) ([]byte, error) {
	// Get a handle to the key within the CSP.
	var hKey uintptr
	r, _, err := procCryptGetUserKey.Call(
		uintptr(provHandle),
		uintptr(keySpec),
		uintptr(unsafe.Pointer(&hKey)),
	)
	if r == 0 {
		return nil, fmt.Errorf("CryptGetUserKey failed: %w", err)
	}
	defer procCryptDestroyKey.Call(hKey) //nolint:errcheck

	// First call: determine the required buffer size.
	var size uint32
	r, _, err = procCryptExportKey.Call(
		hKey,
		0, // hExpKey (not used for plaintext export)
		uintptr(privatekeyBlob),
		0, // dwFlags
		0, // pbData (nil to get size)
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return nil, fmt.Errorf("CryptExportKey size query failed: %w", err)
	}
	if size == 0 {
		return nil, fmt.Errorf("CryptExportKey returned zero size for key")
	}

	// Second call: export the key into the buffer.
	buf := make([]byte, size)
	r, _, err = procCryptExportKey.Call(
		hKey,
		0,
		uintptr(privatekeyBlob),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return nil, fmt.Errorf("CryptExportKey failed: %w", err)
	}

	return buf[:size], nil
}

// parseRSAPrivateKeyBlob parses either a CNG BCRYPT_RSAFULLPRIVATE_BLOB or a
// legacy CSP PRIVATEKEYBLOB into an rsa.PrivateKey. The format is detected
// by examining the magic value in the blob header.
func parseRSAPrivateKeyBlob(blob []byte) (*rsa.PrivateKey, error) {
	if len(blob) < 4 {
		return nil, fmt.Errorf("RSA key blob too short: %d bytes", len(blob))
	}

	// The first byte distinguishes the two formats:
	// - CSP PRIVATEKEYBLOB starts with BLOBHEADER.bType = 0x07 (PRIVATEKEYBLOB)
	// - CNG BCRYPT_RSAKEY_BLOB starts with Magic (little-endian uint32)
	if blob[0] == privatekeyBlob {
		return parseCSPPrivateKeyBlob(blob)
	}
	return parseCNGFullPrivateBlob(blob)
}

// parseCSPPrivateKeyBlob parses a legacy CSP PRIVATEKEYBLOB.
//
// Layout:
//
//	BLOBHEADER (8 bytes): bType(1) bVersion(1) reserved(2) aiKeyAlg(4)
//	RSAPUBKEY  (12 bytes): magic(4) bitlen(4) pubexp(4)
//	Followed by little-endian byte fields:
//	  modulus     [bitlen/8]
//	  prime1      [bitlen/16]
//	  prime2      [bitlen/16]
//	  exponent1   [bitlen/16]  (d mod (p-1))
//	  exponent2   [bitlen/16]  (d mod (q-1))
//	  coefficient [bitlen/16]  (q^-1 mod p)
//	  privateExponent [bitlen/8]
//
// All multi-byte integers are in little-endian byte order.
// See https://learn.microsoft.com/en-us/windows/win32/seccrypto/base-provider-key-blobs
func parseCSPPrivateKeyBlob(blob []byte) (*rsa.PrivateKey, error) {
	if len(blob) < privatekeyBlobHeaderSize {
		return nil, fmt.Errorf("CSP private key blob too short: %d bytes", len(blob))
	}

	magic := binary.LittleEndian.Uint32(blob[8:12])
	if magic != rsaPrivateKeyMagic {
		return nil, fmt.Errorf("unexpected CSP key blob magic: 0x%08X (expected 0x%08X)", magic, rsaPrivateKeyMagic)
	}

	bitLen := int(binary.LittleEndian.Uint32(blob[12:16]))
	pubExp := int(binary.LittleEndian.Uint32(blob[16:20]))

	byteLen := bitLen / 8
	halfLen := bitLen / 16

	// Total expected size: header + modulus + p + q + dp + dq + qInv + d
	expectedSize := privatekeyBlobHeaderSize + byteLen + halfLen*5 + byteLen
	if len(blob) < expectedSize {
		return nil, fmt.Errorf("CSP private key blob too short: %d bytes (expected at least %d)", len(blob), expectedSize)
	}

	offset := privatekeyBlobHeaderSize
	readFieldLE := func(size int) *big.Int {
		// CSP blobs store integers in little-endian; reverse for big.Int.
		field := make([]byte, size)
		copy(field, blob[offset:offset+size])
		reverseBytes(field)
		offset += size
		return new(big.Int).SetBytes(field)
	}

	modulus := readFieldLE(byteLen)
	prime1 := readFieldLE(halfLen)
	prime2 := readFieldLE(halfLen)
	offset += halfLen * 3 // skip dP, dQ, qInv — Go recomputes them
	privateExp := readFieldLE(byteLen)

	key := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{
			N: modulus,
			E: pubExp,
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

// parseCNGFullPrivateBlob parses a BCRYPT_RSAFULLPRIVATE_BLOB into an rsa.PrivateKey.
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
func parseCNGFullPrivateBlob(blob []byte) (*rsa.PrivateKey, error) {
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

// reverseBytes reverses a byte slice in place.
func reverseBytes(b []byte) {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
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
