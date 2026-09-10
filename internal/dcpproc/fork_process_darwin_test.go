//go:build darwin

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestForkProcessExecShimRestoresTargetGoDebug(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	t.Cleanup(testCancel)

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	const originalGoDebug = "asyncpreemptoff=0,tracebackancestors=7"
	outputPath := filepath.Join(t.TempDir(), "target-godebug")
	cmdArgs := forkProcessArgsForCurrentProcess(
		t,
		"/bin/sh",
		"-c",
		`printf '%s' "$GODEBUG" > "$1"`,
		"sh",
		outputPath,
	)
	dcpProcCmd := exec.CommandContext(testCtx, dcpProc, cmdArgs...)
	dcpProcCmd.Env = environmentWithValue(os.Environ(), "GODEBUG", originalGoDebug)
	var stdout, stderr bytes.Buffer
	dcpProcCmd.Stdout = &stdout
	dcpProcCmd.Stderr = &stderr

	runErr := dcpProcCmd.Run()
	require.NoError(t, runErr, "dcp fork-process should exit cleanly; stderr: %s", stderr.String())
	_ = parseForkedPid(t, stdout.String())

	outputFile, openErr := usvc_io.OpenFileReadOnly(outputPath)
	require.NoError(t, openErr)
	output, readErr := io.ReadAll(outputFile)
	closeErr := outputFile.Close()
	require.NoError(t, readErr)
	require.NoError(t, closeErr)
	require.Equal(t, originalGoDebug, string(output), "the requested program should observe the caller's GODEBUG")
}

func environmentWithValue(env []string, name string, value string) []string {
	prefix := name + "="
	updatedEnv := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			updatedEnv = append(updatedEnv, entry)
		}
	}
	return append(updatedEnv, prefix+value)
}
