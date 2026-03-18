/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package security

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeThumbprint_DotNetFormat(t *testing.T) {
	// .NET X509Certificate2.Thumbprint: contiguous uppercase hex
	result := normalizeThumbprint("A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2")
	assert.Equal(t, "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", result)
}

func TestNormalizeThumbprint_WindowsCertmgrFormat(t *testing.T) {
	// Windows certmgr / PowerShell Get-ChildItem Cert:\: space-separated uppercase hex
	result := normalizeThumbprint("A1 B2 C3 D4 E5 F6 A1 B2 C3 D4 E5 F6 A1 B2 C3 D4 E5 F6 A1 B2")
	assert.Equal(t, "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", result)
}

func TestNormalizeThumbprint_OpenSSLFormat(t *testing.T) {
	// OpenSSL x509 -fingerprint: colon-separated uppercase hex
	result := normalizeThumbprint("A1:B2:C3:D4:E5:F6:A1:B2:C3:D4:E5:F6:A1:B2:C3:D4:E5:F6:A1:B2")
	assert.Equal(t, "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", result)
}

func TestNormalizeThumbprint_HexPrefixLowercase(t *testing.T) {
	// Programmatic hex with 0x prefix
	result := normalizeThumbprint("0xa1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2")
	assert.Equal(t, "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", result)
}

func TestNormalizeThumbprint_HexPrefixUppercase(t *testing.T) {
	// Programmatic hex with 0X prefix
	result := normalizeThumbprint("0XA1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2")
	assert.Equal(t, "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", result)
}

func TestNormalizeThumbprint_AlreadyNormalized(t *testing.T) {
	// Already in normalized form: lowercase contiguous hex
	result := normalizeThumbprint("a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2")
	assert.Equal(t, "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", result)
}

func TestNormalizeThumbprint_MixedFormatting(t *testing.T) {
	// Mixed spaces, colons, and case
	result := normalizeThumbprint("A1:b2 C3:d4 E5:f6 A1:b2 C3:d4 E5:f6 A1:b2 C3:d4 E5:f6 A1:b2")
	assert.Equal(t, "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", result)
}

func TestNormalizeThumbprint_LeadingTrailingWhitespace(t *testing.T) {
	// Copy/paste from shell output with trailing newline and tabs
	result := normalizeThumbprint("\t A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2 \r\n")
	assert.Equal(t, "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", result)
}

func TestNormalizeThumbprint_WindowsLeftToRightMark(t *testing.T) {
	// Windows certificate UI sometimes embeds invisible U+200E (LRM) characters
	result := normalizeThumbprint("\u200eA1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2\u200e")
	assert.Equal(t, "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", result)
}
