/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

func TestOpenBuildContextArchive(t *testing.T) {
	t.Parallel()

	contents := []byte("build context")
	archivePath := filepath.Join(t.TempDir(), "context.tar")
	require.NoError(t, usvc_io.WriteFile(archivePath, contents, osutil.PermissionOnlyOwnerReadWrite))
	hash := sha256.Sum256(contents)

	archiveFile, openErr := OpenBuildContextArchive(&ContainerBuildContextArchive{
		Digest: "archive-v1",
		Source: archivePath,
		SHA256: fmt.Sprintf("sha256:%x", hash),
	})
	require.NoError(t, openErr)
	defer archiveFile.Close()

	actualContents, readErr := io.ReadAll(archiveFile)
	require.NoError(t, readErr)
	require.Equal(t, contents, actualContents)
}

func TestOpenBuildContextArchiveRejectsHashMismatch(t *testing.T) {
	t.Parallel()

	archivePath := filepath.Join(t.TempDir(), "context.tar")
	require.NoError(t, usvc_io.WriteFile(archivePath, []byte("build context"), osutil.PermissionOnlyOwnerReadWrite))

	_, openErr := OpenBuildContextArchive(&ContainerBuildContextArchive{
		Digest: "archive-v1",
		Source: archivePath,
		SHA256: "deadbeef",
	})
	require.ErrorContains(t, openErr, "SHA256 mismatch")
}

func TestOpenBuildContextArchiveRawContents(t *testing.T) {
	t.Parallel()

	contents := []byte("build context")
	archiveReader, openErr := OpenBuildContextArchive(&ContainerBuildContextArchive{
		Digest:      "archive-v1",
		RawContents: base64.StdEncoding.EncodeToString(contents),
	})
	require.NoError(t, openErr)
	defer archiveReader.Close()

	actualContents, readErr := io.ReadAll(archiveReader)
	require.NoError(t, readErr)
	require.Equal(t, contents, actualContents)
}

func TestOpenBuildContextArchiveRejectsMissingContents(t *testing.T) {
	t.Parallel()

	_, openErr := OpenBuildContextArchive(&ContainerBuildContextArchive{Digest: "archive-v1"})
	require.ErrorContains(t, openErr, "source or raw contents is required")
}

func TestOpenBuildContextArchiveRejectsConflictingContents(t *testing.T) {
	t.Parallel()

	_, openErr := OpenBuildContextArchive(&ContainerBuildContextArchive{
		Digest:      "archive-v1",
		Source:      "context.tar",
		RawContents: "dGVzdA==",
	})
	require.ErrorContains(t, openErr, "mutually exclusive")
}
