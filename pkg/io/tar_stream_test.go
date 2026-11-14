/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that OpenDirectoryTar streams relative entries without changing the destination root,
// and closes successfully after the complete archive has been consumed.
func TestDirectoryTarStreamContents(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	source := t.TempDir()
	require.NoError(t, usvc_io.WriteFile(filepath.Join(source, "dependency"), []byte("data"), 0644))
	stream := usvc_io.OpenDirectoryTar(ctx, source, "", 0, 0, nil)
	contents, readErr := io.ReadAll(stream)
	require.NoError(t, readErr)
	require.NoError(t, stream.Close())
	reader := tar.NewReader(bytes.NewReader(contents))
	header, headerErr := reader.Next()
	require.NoError(t, headerErr)
	require.Equal(t, "dependency", header.Name)
	data, payloadErr := io.ReadAll(reader)
	require.NoError(t, payloadErr)
	require.Equal(t, "data", string(data))
	_, endErr := reader.Next()
	require.ErrorIs(t, endErr, io.EOF)
}

// Verifies that closing an unconsumed directory tar stream joins its producer, and cancellation
// releases a pending read without requiring the archive to be fully consumed.
func TestDirectoryTarStreamCancellation(t *testing.T) {
	t.Parallel()
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "early-close", true: "cancel"}[canceled], func(t *testing.T) {
			lifetime, cancelLifetime := testutil.GetTestContext(t, 0)
			defer cancelLifetime()
			ctx, cancel := context.WithCancel(lifetime)
			defer cancel()
			source := t.TempDir()
			require.NoError(t, usvc_io.WriteFile(filepath.Join(source, "data"), make([]byte, 128*1024), 0644))
			stream := usvc_io.OpenDirectoryTar(ctx, source, "adapter", 0, 0, nil)
			if canceled {
				cancel()
				_, readErr := io.ReadAll(stream)
				require.ErrorIs(t, readErr, context.Canceled)
			}
			closed := make(chan error, 1)
			go func() { closed <- stream.Close() }()
			select {
			case closeErr := <-closed:
				require.Error(t, closeErr)
			case <-lifetime.Done():
				t.Fatal("stream closure did not release the tar producer")
			}
			require.Error(t, stream.Close())
		})
	}
}
