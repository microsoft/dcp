/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

// Verifies that IID files accept a whitespace-padded SHA256 identifier and reject oversized, short, or nonhexadecimal contents.
func TestReadImageIDFile(t *testing.T) {
	t.Parallel()

	validImageID := "SHA256:" + strings.Repeat("A", sha256.Size*2)
	testCases := []struct {
		name          string
		contents      string
		expected      string
		errorContains string
	}{
		{
			name:     "valid",
			contents: " \r\n" + validImageID + "\r\n ",
			expected: validImageID,
		},
		{
			name:          "oversized",
			contents:      strings.Repeat("a", maxImageIDFileSize+1),
			errorContains: "exceeds 1024 bytes",
		},
		{
			name:          "short",
			contents:      "sha256:" + strings.Repeat("a", sha256.Size*2-1),
			errorContains: "expected sha256: followed by 64 hexadecimal characters",
		},
		{
			name:          "nonhex",
			contents:      "sha256:" + strings.Repeat("a", sha256.Size*2-1) + "g",
			errorContains: "decoding SHA256 value",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "image.iid")
			require.NoError(t, usvc_io.WriteFile(path, []byte(testCase.contents), osutil.PermissionOnlyOwnerReadWrite))

			imageID, readErr := ReadImageIDFile(path)

			if testCase.errorContains == "" {
				require.NoError(t, readErr)
				assert.Equal(t, testCase.expected, imageID)
			} else {
				require.Error(t, readErr)
				assert.Contains(t, readErr.Error(), testCase.errorContains)
				assert.Empty(t, imageID)
			}
		})
	}
}

// Verifies that a directory cannot be read as an IID file and yields no image identifier.
func TestReadImageIDFileRejectsNonRegularFile(t *testing.T) {
	t.Parallel()

	imageID, readErr := ReadImageIDFile(t.TempDir())

	require.Error(t, readErr)
	assert.Contains(t, readErr.Error(), "is not a regular file")
	assert.Empty(t, imageID)
}

// Verifies that an IID file created by an external runtime can be read without DCP restricted-file ownership or ACL validation.
func TestReadImageIDFileAllowsExternallyCreatedFile(t *testing.T) {
	t.Parallel()

	expectedImageID := "sha256:" + strings.Repeat("a", sha256.Size*2)
	path := filepath.Join(t.TempDir(), "external-image.iid")
	require.NoError(t, os.WriteFile(path, []byte(expectedImageID+"\n"), 0o600))

	imageID, readErr := ReadImageIDFile(path)

	require.NoError(t, readErr)
	require.Equal(t, expectedImageID, imageID)
}
