/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

// OpenBuildContextArchive verifies and opens an archive for streaming to an image builder.
func OpenBuildContextArchive(archive *ContainerBuildContextArchive) (io.ReadCloser, error) {
	if archive == nil {
		return nil, fmt.Errorf("build context archive is required")
	}
	if archive.Source != "" && archive.RawContents != "" {
		return nil, fmt.Errorf("build context archive source and raw contents are mutually exclusive")
	}
	if archive.Source == "" {
		if archive.RawContents == "" {
			return nil, fmt.Errorf("build context archive source or raw contents is required")
		}
		contents, decodeErr := base64.StdEncoding.DecodeString(archive.RawContents)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode build context archive raw contents: %w", decodeErr)
		}
		return io.NopCloser(bytes.NewReader(contents)), nil
	}

	archiveFile, openErr := usvc_io.OpenFile(archive.Source, os.O_RDONLY, 0)
	if openErr != nil {
		return nil, fmt.Errorf("open build context archive %q: %w", archive.Source, openErr)
	}

	hash := sha256.New()
	if _, copyErr := io.Copy(hash, archiveFile); copyErr != nil {
		_ = archiveFile.Close()
		return nil, fmt.Errorf("hash build context archive %q: %w", archive.Source, copyErr)
	}

	expectedHash := strings.TrimPrefix(strings.ToLower(archive.SHA256), "sha256:")
	actualHash := hex.EncodeToString(hash.Sum(nil))
	if actualHash != expectedHash {
		_ = archiveFile.Close()
		return nil, fmt.Errorf("build context archive %q SHA256 mismatch: expected %s, got %s", archive.Source, expectedHash, actualHash)
	}

	if _, seekErr := archiveFile.Seek(0, io.SeekStart); seekErr != nil {
		_ = archiveFile.Close()
		return nil, fmt.Errorf("rewind build context archive %q: %w", archive.Source, seekErr)
	}

	return archiveFile, nil
}
