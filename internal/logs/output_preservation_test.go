/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

// Verifies that runtime-log descriptors and path-based output cleanup honor the same preservation
// switch while descriptor disposal still cancels capture lifetime and completes normally.
func TestOutputCleanupHonorsPreservation(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		name, setting := "remove", ""
		if preserve {
			name, setting = "preserve", "true"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS, setting)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			descriptor := NewLogDescriptor(ctx, cancel, types.NamespacedName{Name: "runtime-output"}, "runtime-uid")
			stdout, stderr, created, captureErr := descriptor.EnableLogCapturing(t.TempDir())
			require.NoError(t, captureErr)
			require.True(t, created)
			_, stdoutErr := stdout.Write([]byte("container stdout\n"))
			require.NoError(t, stdoutErr)
			_, stderrErr := stderr.Write([]byte("container stderr\n"))
			require.NoError(t, stderrErr)
			stdoutPath, stderrPath, consumerErr := descriptor.LogConsumerStarting()
			require.NoError(t, consumerErr)
			descriptor.LogConsumerStopped()
			require.NoError(t, stdout.Close())
			require.NoError(t, stderr.Close())
			require.NoError(t, descriptor.Dispose(t.Context(), 0))
			require.True(t, descriptor.IsDisposed())
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.NoError(t, descriptor.Dispose(t.Context(), 0))
			adoptedPath := filepath.Join(t.TempDir(), "adopted-output")
			require.NoError(t, usvc_io.WriteFile(adoptedPath, []byte("adopted stdout\n"), osutil.PermissionOnlyOwnerReadWrite))
			require.NoError(t, RemoveOutputFiles(t.Context(), adoptedPath, "", adoptedPath))
			for _, path := range []string{stdoutPath, stderrPath, adoptedPath} {
				output, readErr := os.ReadFile(path)
				if preserve {
					require.NoError(t, readErr)
					require.NotEmpty(t, output)
				} else {
					require.True(t, errors.Is(readErr, os.ErrNotExist), "%v", readErr)
				}
			}
		})
	}
}
