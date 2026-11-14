/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"context"
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
	archiveFile, openErr := usvc_io.OpenTarArchive(ctx, archive.Source, archive.SHA256, archive.RawContents)
	if openErr != nil {
		if archive.Source == "" && archive.RawContents != "" {
			return nil, fmt.Errorf("decode build context archive: %w", openErr)
		}
		return nil, fmt.Errorf("open build context archive: %w", openErr)
	}

	return archiveFile, nil
}
