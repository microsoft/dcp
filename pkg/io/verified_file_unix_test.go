//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that OpenSHA256VerifiedFile rejects a FIFO without blocking on an open that waits for a writer.
func TestVerifiedFileRejectsFIFO(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	source := filepath.Join(t.TempDir(), "archive.fifo")
	require.NoError(t, syscall.Mkfifo(source, 0600))
	_, openErr := usvc_io.OpenSHA256VerifiedFile(ctx, source, "")
	require.ErrorContains(t, openErr, "is not a regular file")
}
