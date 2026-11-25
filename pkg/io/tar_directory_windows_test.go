//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

func createJunction(t *testing.T, link, target string) {
	t.Helper()
	output, junctionErr := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	require.NoError(t, junctionErr, "%s", output)
}

// Verifies that TarWriter.CopyDirectory follows a junction in the source path itself,
// but rejects a junction inside the copied directory.
func TestTarDirectoryJunctions(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"source-ancestor", "within-source"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			base := t.TempDir()
			actual := filepath.Join(base, "actual")
			require.NoError(t, os.MkdirAll(filepath.Join(actual, "adapter"), 0755))
			require.NoError(t, usvc_io.WriteFile(filepath.Join(actual, "adapter", "binary"), []byte("binary"), 0644))
			source := filepath.Join(actual, "adapter")
			if scenario == "source-ancestor" {
				createJunction(t, filepath.Join(base, "junction"), actual)
				source = filepath.Join(base, "junction", "adapter")
			} else {
				outside := filepath.Join(base, "outside")
				require.NoError(t, os.Mkdir(outside, 0755))
				createJunction(t, filepath.Join(source, "dependency"), outside)
			}
			writer := usvc_io.NewTarWriter()
			copyErr := writer.CopyDirectory(ctx, source, "adapter", 0, 0, nil)
			if scenario == "within-source" {
				require.ErrorContains(t, copyErr, "unsupported directory entry")
				require.NoError(t, writer.Close())
				return
			}
			require.NoError(t, copyErr)
			buffer, finalizeErr := writer.Buffer()
			require.NoError(t, finalizeErr)
			reader := tar.NewReader(buffer)
			found := false
			for {
				header, nextErr := reader.Next()
				if errors.Is(nextErr, io.EOF) {
					break
				}
				require.NoError(t, nextErr)
				if header.Name == "adapter/binary" {
					payload, readErr := io.ReadAll(reader)
					require.NoError(t, readErr)
					require.Equal(t, "binary", string(payload))
					found = true
				}
			}
			require.True(t, found)
		})
	}
}
