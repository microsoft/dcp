/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"

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
	if archive.Source == "" && archive.RawContents == "" {
		return nil, fmt.Errorf("build context archive source or raw contents is required")
	}
	if archive.Source != "" {
		archiveFile, openErr := usvc_io.OpenSHA256VerifiedFile(ctx, archive.Source, archive.SHA256)
		if openErr != nil {
			return nil, fmt.Errorf("open build context archive: %w", openErr)
		}
		return archiveFile, nil
	}
	contents, decodeErr := base64.StdEncoding.DecodeString(archive.RawContents)
	if decodeErr != nil {
		return nil, fmt.Errorf("decode build context archive: %w", decodeErr)
	}
	return io.NopCloser(bytes.NewReader(contents)), nil
}
