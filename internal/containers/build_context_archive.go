/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"

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
			return nil, fmt.Errorf("decode build context archive %q raw contents: %w", archive.Digest, decodeErr)
		}
		return io.NopCloser(bytes.NewReader(contents)), nil
	}

	archiveFile, openErr := usvc_io.OpenFile(archive.Source, os.O_RDONLY, 0)
	if openErr != nil {
		return nil, fmt.Errorf("open build context archive %q: %w", archive.Source, openErr)
	}

	if verifyErr := verifySHA256(archiveFile, archive.SHA256); verifyErr != nil {
		_ = archiveFile.Close()
		return nil, fmt.Errorf("verifying build context archive %q: %w", archive.Source, verifyErr)
	}

	if _, seekErr := archiveFile.Seek(0, io.SeekStart); seekErr != nil {
		_ = archiveFile.Close()
		return nil, fmt.Errorf("rewind build context archive %q: %w", archive.Source, seekErr)
	}

	return archiveFile, nil
}
