/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

// OpenBuildContextArchive verifies and opens an archive for streaming to an image builder.
func OpenBuildContextArchive(ctx context.Context, archive *ContainerBuildContextArchive) (io.ReadCloser, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
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
		var contents bytes.Buffer
		decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(archive.RawContents))
		_, decodeErr := copyBuildContextContents(ctx, &contents, decoder)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode build context archive raw contents: %w", decodeErr)
		}
		return io.NopCloser(bytes.NewReader(contents.Bytes())), nil
	}

	archiveFile, openErr := usvc_io.OpenFileReadOnly(archive.Source)
	if openErr != nil {
		return nil, fmt.Errorf("open build context archive %q: %w", archive.Source, openErr)
	}

	archiveInfo, statErr := archiveFile.Stat()
	if statErr != nil {
		return nil, errors.Join(fmt.Errorf("inspect build context archive %q: %w", archive.Source, statErr), archiveFile.Close())
	}
	if !archiveInfo.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("build context archive %q is not a regular file", archive.Source), archiveFile.Close())
	}

	hasher := sha256.New()
	if _, hashErr := copyBuildContextContents(ctx, hasher, archiveFile); hashErr != nil {
		return nil, errors.Join(fmt.Errorf("hash build context archive %q: %w", archive.Source, hashErr), archiveFile.Close())
	}
	if verifyErr := verifySHA256Hash(hex.EncodeToString(hasher.Sum(nil)), archive.SHA256); verifyErr != nil {
		return nil, errors.Join(fmt.Errorf("verifying build context archive %q: %w", archive.Source, verifyErr), archiveFile.Close())
	}

	if _, seekErr := archiveFile.Seek(0, io.SeekStart); seekErr != nil {
		return nil, errors.Join(fmt.Errorf("rewind build context archive %q: %w", archive.Source, seekErr), archiveFile.Close())
	}

	return archiveFile, nil
}
