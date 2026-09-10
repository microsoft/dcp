/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun_test

import (
	"archive/tar"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcptun"
)

func TestPrepareClientProxyImageBuild(t *testing.T) {
	t.Parallel()

	dcppaths.EnableTestPathProbing()

	plan, prepareErr := dcptun.PrepareClientProxyImageBuild()
	require.NoError(t, prepareErr)

	const expectedTagPrefix = "dcptun_developer_ms:"
	require.True(t, strings.HasPrefix(plan.Image, expectedTagPrefix))
	require.Greater(t, len(plan.Image), len(expectedTagPrefix), "Image tag should have a version suffix")
	require.NotNil(t, plan.BuildContextArchive)
	require.NotEmpty(t, plan.BuildContextArchive.Digest)
	require.NotEmpty(t, plan.BuildContextArchive.Source)
	require.NotEmpty(t, plan.BuildContextArchive.SHA256)
	require.Empty(t, plan.BuildContextArchive.RawContents)
	require.Equal(t, "Dockerfile", plan.Dockerfile)
	t.Cleanup(func() {
		require.NoError(t, os.Remove(plan.BuildContextArchive.Source))
	})

	archiveReader, openErr := containers.OpenBuildContextArchive(plan.BuildContextArchive)
	require.NoError(t, openErr)
	defer archiveReader.Close()

	entries := map[string][]byte{}
	tarReader := tar.NewReader(archiveReader)
	for {
		header, nextErr := tarReader.Next()
		if nextErr == io.EOF {
			break
		}
		require.NoError(t, nextErr)
		contents, readErr := io.ReadAll(tarReader)
		require.NoError(t, readErr)
		entries[header.Name] = contents
	}

	require.Contains(t, string(entries["Dockerfile"]), "FROM "+dcptun.DefaultBaseImage)
	require.NotEmpty(t, entries[dcptun.ClientBinaryName])
}
